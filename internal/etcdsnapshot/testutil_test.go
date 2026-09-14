package etcdsnapshot

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mustMkdirAll is os.MkdirAll with t.Fatal on error, for fixture setup.
func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// mustWriteFile writes content to path (creating parent dirs) with t.Fatal on
// error, for fixture setup.
func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	mustMkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// makeCACertPEM generates a fresh self-signed CA certificate PEM for tests.
// Different cn values yield different SPKI hashes, and therefore different
// derived cluster ids (used to prove per-cluster snapshot isolation).
func makeCACertPEM(t *testing.T, cn string) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// newStackedRoot builds a RootPath fixture that looks like a real stacked-etcd
// control plane: an etcd static-pod manifest with a parseable image tag, a
// self-signed cluster CA, and an etcd db file of dbSize bytes.
func newStackedRoot(t *testing.T, cn string, dbSize int) string {
	t.Helper()
	return newStackedRootWithEtcdTag(t, cn, dbSize, "3.6.8-0")
}

// newStackedRootWithEtcdTag is newStackedRoot with an explicit etcd image tag
// (which ends up as the "from-<tag>" component of the computed snapshot file
// name) -- used by tests that need to steer the DEFAULT-exec fake etcdctl's
// argv-only behavior selection via the destination path.
func newStackedRootWithEtcdTag(t *testing.T, cn string, dbSize int, tag string) string {
	t.Helper()
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "etc", "kubernetes", "manifests", "etcd.yaml"),
		"kind: Pod\nspec:\n  containers:\n  - image: registry.k8s.io/etcd:"+tag+"\n")
	mustWriteFile(t, filepath.Join(root, "etc", "kubernetes", "pki", "ca.crt"), string(makeCACertPEM(t, cn)))

	dbPath := filepath.Join(root, "var", "lib", "etcd", "member", "snap", "db")
	mustMkdirAll(t, filepath.Dir(dbPath))
	if err := os.WriteFile(dbPath, make([]byte, dbSize), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// regularFileEtcdctl creates a plain regular (executable) file to stand in for
// etcdctl in tests that never actually exec it (the `save` test seam is
// injected instead) -- only Run's Lstat-is-regular-file check needs to pass.
func regularFileEtcdctl(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "etcdctl")
	mustWriteFile(t, p, "#!/bin/true\n")
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// hugeFreeBytes is a freeBytes test seam reporting abundant free space, so
// tests unrelated to the free-space gate never depend on real disk state.
func hugeFreeBytes(string) (uint64, error) { return 100 << 30, nil } // 100 GiB

// failIfCalled returns a `save` test seam that fails the test if ever invoked,
// for asserting Run returns before reaching the save step.
func failIfCalled(t *testing.T) func(context.Context, string) error {
	t.Helper()
	return func(context.Context, string) error {
		t.Fatal("save must not be called")
		return nil
	}
}

// testOptions returns a fully-valid Options (stacked, encrypted, plenty of
// free space, matching owner) pointing at a fresh snapshot dir under root's
// sibling temp space, ready for a caller to override individual fields.
func testOptions(t *testing.T, root string) (Options, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "etcd-backup")
	o := Options{
		RootPath:            root,
		TargetMinor:         "1.37",
		EncryptionConfirmed: func(context.Context, string) bool { return true },
		dir:                 dir,
		etcdctlPath:         regularFileEtcdctl(t),
		freeBytes:           hugeFreeBytes,
		ownerUID:            os.Getuid(),
		save:                failIfCalled(t),
	}
	return o, dir
}

// hasArg reports whether argv contains want verbatim.
func hasArg(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

// argWithPrefix returns the suffix of the first argv element with prefix, and
// whether one was found.
func argWithPrefix(argv []string, prefix string) (string, bool) {
	for _, a := range argv {
		if strings.HasPrefix(a, prefix) {
			return strings.TrimPrefix(a, prefix), true
		}
	}
	return "", false
}
