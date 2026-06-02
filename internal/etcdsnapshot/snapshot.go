// Package etcdsnapshot takes a best-effort etcd snapshot before a destructive
// kubeadm upgrade apply (ADR-12 U5).
//
// SECURITY: an etcd snapshot is a full plaintext dump of every cluster Secret and
// the cluster PKI. It is therefore written ONLY to the encrypted persistent
// partition, 0600 root:root, OFF the reset/artifact paths (so a later reset never
// wipes or follows it), and single-retained. If the persistent partition's
// encryption cannot be confirmed the snapshot is REFUSED -- the provider never
// writes a plaintext full-cluster dump (this is the one place ADR-12 escalates
// from warn to refuse). The provider never copies the snapshot off the node;
// durability/custody is the operator's responsibility.
//
// It is best-effort and MUST NOT block the upgrade: every failure mode (including
// the encryption refusal and non-stacked etcd) returns an error that the caller
// logs and then proceeds (#4099-1). Bounded by ctx.
package etcdsnapshot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultDir is the snapshot directory. It lives under /var/lib (persisted by
// Kairos onto COS_PERSISTENT) and OFF the reset artifact paths (/etc/kubernetes,
// /var/lib/kubelet, /var/lib/etcd) so reset never wipes or traverses it.
const DefaultDir = "/var/lib/provider-kubernetes/etcd-backup"

// Options configures a snapshot.
type Options struct {
	// RootPath is the cluster root (cluster_root_path); locates the etcd manifest,
	// member dir, and PKI.
	RootPath string
	// Dir is the snapshot directory; empty -> DefaultDir. Must be absolute and off
	// the reset artifact paths.
	Dir string
	// EncryptionConfirmed reports whether Dir's backing storage is encrypted. nil
	// or false -> the snapshot is refused (no plaintext is written).
	EncryptionConfirmed func(ctx context.Context) bool
	// Save performs the actual `etcdctl snapshot save <dest>`; nil -> DefaultSave.
	Save func(ctx context.Context, rootPath, dest string) error
	// Now is the timestamp used in the filename; zero -> time.Now().
	Now time.Time
}

// Run performs a best-effort, bounded etcd snapshot. See the package doc for the
// security posture. It returns nil only when a snapshot was actually written.
func Run(ctx context.Context, o Options) error {
	if !IsStackedEtcd(o.RootPath) {
		return fmt.Errorf("etcd is external/non-stacked on this node; the operator must snapshot etcd before upgrading")
	}
	if o.EncryptionConfirmed == nil || !o.EncryptionConfirmed(ctx) {
		return fmt.Errorf("persistent-partition encryption is unconfirmed; refusing to write a plaintext etcd snapshot (take one manually onto encrypted storage)")
	}

	dir := o.Dir
	if dir == "" {
		dir = DefaultDir
	}
	if err := validateDir(o.RootPath, dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create snapshot dir: %w", err)
	}

	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	dest := filepath.Join(dir, fmt.Sprintf("etcd-snapshot-%s.db", now.UTC().Format("20060102T150405Z")))

	save := o.Save
	if save == nil {
		save = DefaultSave
	}
	if err := save(ctx, o.RootPath, dest); err != nil {
		return fmt.Errorf("etcd snapshot save: %w", err)
	}
	if err := os.Chmod(dest, 0o600); err != nil {
		return fmt.Errorf("chmod snapshot 0600: %w", err)
	}
	// Single-retain: remove any older snapshots so a full-cluster dump never
	// accumulates on disk.
	pruneOld(dir, dest)
	return nil
}

// IsStackedEtcd reports whether this node runs a stacked etcd, by the presence of
// the etcd static-pod manifest or the etcd member dir under root. Pure file check.
func IsStackedEtcd(root string) bool {
	for _, p := range []string{
		filepath.Join(root, "etc", "kubernetes", "manifests", "etcd.yaml"),
		filepath.Join(root, "var", "lib", "etcd", "member"),
	} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// validateDir rejects a snapshot dir that is not absolute or that lives under a
// reset artifact path (where a later reset's os.RemoveAll would wipe it, or where
// it would sit inside the etcd data dir it is backing up).
func validateDir(root, dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("snapshot dir must be absolute, got %q", dir)
	}
	clean := filepath.Clean(dir)
	forbidden := []string{
		filepath.Clean(filepath.Join(root, "etc", "kubernetes")),
		filepath.Clean(filepath.Join(root, "var", "lib", "kubelet")),
		filepath.Clean(filepath.Join(root, "var", "lib", "etcd")),
	}
	for _, f := range forbidden {
		if clean == f || strings.HasPrefix(clean, f+string(os.PathSeparator)) {
			return fmt.Errorf("snapshot dir %q must not be under the reset artifact path %q", dir, f)
		}
	}
	return nil
}

// pruneOld removes etcd-snapshot-*.db files in dir other than keep (single-retain).
func pruneOld(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "etcd-snapshot-") || !strings.HasSuffix(name, ".db") {
			continue
		}
		p := filepath.Join(dir, name)
		if p == keep {
			continue
		}
		_ = os.Remove(p)
	}
}

// DefaultSave runs `etcdctl snapshot save <dest>` against the local stacked etcd
// member using the kubeadm-managed etcd client certificates. Bounded by ctx.
func DefaultSave(ctx context.Context, rootPath, dest string) error {
	pki := filepath.Join(rootPath, "etc", "kubernetes", "pki", "etcd")
	cmd := exec.CommandContext(ctx, "etcdctl",
		"--endpoints=https://127.0.0.1:2379",
		"--cacert="+filepath.Join(pki, "ca.crt"),
		"--cert="+filepath.Join(pki, "healthcheck-client.crt"),
		"--key="+filepath.Join(pki, "healthcheck-client.key"),
		"snapshot", "save", dest,
	)
	cmd.Env = append(os.Environ(), "ETCDCTL_API=3")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("etcdctl: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
