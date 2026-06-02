package credential

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
)

type respondRunner struct {
	respond func(args []string) (kubeadm.Result, error)
}

func (r respondRunner) Run(_ context.Context, args ...string) (kubeadm.Result, error) {
	return r.respond(args)
}

// fakeRunner records args and returns canned output.
type fakeRunner struct {
	out      string
	err      error
	lastArgs []string
}

func (f *fakeRunner) Run(_ context.Context, args ...string) (kubeadm.Result, error) {
	f.lastArgs = args
	return kubeadm.Result{Stdout: f.out}, f.err
}

func selfSignedPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestSPKIHashFromPEM(t *testing.T) {
	pemData := selfSignedPEM(t)
	h, err := SPKIHashFromPEM(pemData)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(h) {
		t.Fatalf("unexpected hash format: %q", h)
	}
	// Deterministic for the same cert.
	h2, _ := SPKIHashFromPEM(pemData)
	if h != h2 {
		t.Fatalf("hash not deterministic: %q vs %q", h, h2)
	}
}

func TestSPKIHashFromPEMErrors(t *testing.T) {
	if _, err := SPKIHashFromPEM([]byte("not pem")); err == nil {
		t.Fatal("expected error for non-PEM input")
	}
	bad := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("garbage")})
	if _, err := SPKIHashFromPEM(bad); err == nil {
		t.Fatal("expected error for invalid certificate bytes")
	}
}

func TestGenerateToken(t *testing.T) {
	fr := &fakeRunner{out: "abcdef.0123456789abcdef\n"}
	m := Minter{Runner: fr, RootPath: "/"}
	tok, err := m.GenerateToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "abcdef.0123456789abcdef" {
		t.Fatalf("token not trimmed/parsed: %q", tok)
	}
	if strings.Join(fr.lastArgs, " ") != "token generate" {
		t.Fatalf("unexpected argv: %v", fr.lastArgs)
	}
}

func TestCreateToken(t *testing.T) {
	fr := &fakeRunner{out: "abcdef.0123456789abcdef\n"}
	m := Minter{Runner: fr, RootPath: "/"}
	tok, ttl, err := m.CreateToken(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok == "" || ttl != "1h0m0s" {
		t.Fatalf("unexpected token/ttl: %q / %q", tok, ttl)
	}
	argv := strings.Join(fr.lastArgs, " ")
	if !strings.Contains(argv, "token create --ttl 1h0m0s --kubeconfig /etc/kubernetes/admin.conf") {
		t.Fatalf("unexpected argv: %v", fr.lastArgs)
	}
	// The token value must never be passed on argv (it comes from stdout).
	if strings.Contains(argv, tok) {
		t.Fatalf("token value must not appear in argv: %v", fr.lastArgs)
	}
}

func TestCreateTokenRejectsZeroTTL(t *testing.T) {
	m := Minter{Runner: &fakeRunner{}, RootPath: "/"}
	if _, _, err := m.CreateToken(context.Background(), 0); err == nil {
		t.Fatal("expected error for zero TTL (never non-expiring)")
	}
}

func TestMintJoinMaterial(t *testing.T) {
	root := t.TempDir()
	runDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc/kubernetes/pki"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/kubernetes/pki/ca.crt"), selfSignedPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}

	var uploadCertsCalls [][]string
	rr := respondRunner{respond: func(args []string) (kubeadm.Result, error) {
		switch {
		case args[0] == "token" && args[1] == "create":
			return kubeadm.Result{Stdout: "abcdef.0123456789abcdef\n"}, nil
		case args[0] == "certs" && args[1] == "certificate-key":
			return kubeadm.Result{Stdout: strings.Repeat("a", 64) + "\n"}, nil
		case args[0] == "init" && len(args) > 1 && args[1] == "phase":
			// upload-certs call: record args for assertions, return success.
			uploadCertsCalls = append(uploadCertsCalls, args)
			return kubeadm.Result{}, nil
		}
		return kubeadm.Result{}, nil
	}}
	m := Minter{Runner: rr, RootPath: root, RunDir: runDir}

	// Worker: token + CA hash, no cert key, no upload-certs call.
	jm, err := m.MintJoinMaterial(context.Background(), false, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if jm.Token == "" || len(jm.CACertHashes) != 1 || !strings.HasPrefix(jm.CACertHashes[0], "sha256:") {
		t.Fatalf("unexpected worker join material: %+v", jm)
	}
	if jm.CertificateKey != "" {
		t.Fatalf("worker join material must not include a certificate key")
	}
	if len(uploadCertsCalls) != 0 {
		t.Fatalf("worker mint must not call upload-certs, got %v", uploadCertsCalls)
	}

	// Control plane: also a cert key + upload-certs called atomically.
	jmCP, err := m.MintJoinMaterial(context.Background(), true, time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if jmCP.CertificateKey == "" {
		t.Fatalf("control-plane join material must include a certificate key")
	}
	if len(uploadCertsCalls) != 1 {
		t.Fatalf("expected exactly one upload-certs call, got %d", len(uploadCertsCalls))
	}
	// B1: --certificate-key must NEVER appear on argv.
	for _, call := range uploadCertsCalls {
		for _, arg := range call {
			if arg == "--certificate-key" {
				t.Fatalf("--certificate-key must not appear on argv (B1): %v", call)
			}
		}
	}
	// B1: cert-key value must not appear on argv either.
	for _, call := range uploadCertsCalls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, jmCP.CertificateKey) {
			t.Fatalf("certificate key value must not appear on argv (B1): %v", call)
		}
	}
	// Transient config files must be shredded after upload-certs exec.
	entries, _ := os.ReadDir(runDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "kubeadm-") && strings.HasSuffix(e.Name(), ".yaml") {
			t.Fatalf("transient config was not shredded after upload-certs: %s", e.Name())
		}
	}
}

func TestJoinMaterialIsSelfRedacting(t *testing.T) {
	jm := JoinMaterial{
		Token:          "abcdef.0123456789abcdef",
		CertificateKey: strings.Repeat("k", 64),
	}
	for _, s := range []string{jm.String(), jm.GoString(), "as Stringer: " + jm.String()} {
		if strings.Contains(s, jm.Token) || strings.Contains(s, jm.CertificateKey) {
			t.Fatalf("JoinMaterial must not expose secrets via String/GoString, got: %q", s)
		}
	}
	// Verbose Printf-style formatting via %v / %s also routes through String().
	if v := fmt.Sprintf("%v", jm); strings.Contains(v, jm.Token) || strings.Contains(v, jm.CertificateKey) {
		t.Fatalf("%%v formatting leaked secrets: %q", v)
	}
}

// KEYSTONE: UploadCerts must write cert-key into the --config file (0600 tmpfs),
// NEVER pass it as --certificate-key on argv (B1), and shred the transient file.
func TestUploadCerts_CertKeyNeverOnArgv(t *testing.T) {
	runDir := t.TempDir()
	root := t.TempDir()
	// Ensure admin.conf path is constructable (does not need to exist for the
	// fake runner, but the path must be formed from RootPath).
	const certKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	var capturedArgs [][]string
	rr := respondRunner{respond: func(args []string) (kubeadm.Result, error) {
		capturedArgs = append(capturedArgs, args)
		return kubeadm.Result{}, nil
	}}
	m := Minter{Runner: rr, RootPath: root, RunDir: runDir}
	if err := m.UploadCerts(context.Background(), certKey); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, call := range capturedArgs {
		for _, arg := range call {
			if arg == "--certificate-key" {
				t.Fatalf("B1 violation: --certificate-key must not appear on argv: %v", call)
			}
			if arg == certKey {
				t.Fatalf("B1 violation: cert-key value must not appear on argv: %v", call)
			}
		}
	}
	// --upload-certs must be present (required for the actual upload).
	found := false
	for _, call := range capturedArgs {
		for _, arg := range call {
			if arg == "--upload-certs" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected --upload-certs flag in argv, calls: %v", capturedArgs)
	}
	// Transient config must be shredded after exec.
	entries, _ := os.ReadDir(runDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "kubeadm-") && strings.HasSuffix(e.Name(), ".yaml") {
			t.Fatalf("transient config was not shredded after UploadCerts: %s", e.Name())
		}
	}
}

func TestUploadCerts_EmptyKeyReturnsError(t *testing.T) {
	m := Minter{Runner: respondRunner{respond: func(args []string) (kubeadm.Result, error) {
		return kubeadm.Result{}, nil
	}}}
	if err := m.UploadCerts(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty cert key")
	}
}

func TestGenerateCertificateKey(t *testing.T) {
	key := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	fr := &fakeRunner{out: key + "\n"}
	m := Minter{Runner: fr, RootPath: "/"}
	got, err := m.GenerateCertificateKey(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != key {
		t.Fatalf("unexpected key: %q", got)
	}
	if strings.Join(fr.lastArgs, " ") != "certs certificate-key" {
		t.Fatalf("unexpected argv: %v", fr.lastArgs)
	}
	// The key must never be passed on argv.
	if strings.Contains(strings.Join(fr.lastArgs, " "), key) {
		t.Fatalf("key value must not appear in argv: %v", fr.lastArgs)
	}
}
