// Package reset implements bounded, idempotent cluster reset (ADR-4): it runs
// `kubeadm reset` via argv (no shell) and removes the authoritative kubeadm
// artifacts so the next boot re-converges from clean state. Removing
// /etc/kubernetes also shreds the cluster PKI and any bootstrap material on the
// node (ADR-2 reset shredding). Every step is bounded so reset can never hang
// (issue #4099-1).
//
// SECURITY: `RootPath` is operator-supplied via cluster_root_path. Because this
// package runs os.RemoveAll on paths derived from it, RootPath is validated
// (absolute, no traversal segments) inside Run; any intermediate symlinked
// artifact is refused (not followed) to avoid destroying a bind-mount target.
// Operators must NOT point RootPath at a directory whose only copy of their
// externally-managed PKI lives under it.
package reset

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
)

const (
	defaultRootPath = "/"
	defaultTimeout  = 5 * time.Minute
)

// Options configures a reset pass.
type Options struct {
	Runner    kubeadm.Runner
	RootPath  string
	CRISocket string        // optional; passed to `kubeadm reset --cri-socket` when set
	Timeout   time.Duration // bounded; zero defaults to defaultTimeout
}

// authoritativeArtifacts are the paths whose presence the actualstate prober
// treats as cluster membership. Removing them makes the next boot read
// "uninitialized" so the reconcile re-converges cleanly. Pre-condition: root is
// already validated (absolute, no traversal) by validateRoot.
func authoritativeArtifacts(root string) []string {
	return []string{
		filepath.Join(root, "etc", "kubernetes"), // confs + pki (shreds CA key)
		filepath.Join(root, "var", "lib", "kubelet"),
		filepath.Join(root, "var", "lib", "etcd"),
	}
}

// validateRoot enforces that RootPath is absolute, free of traversal segments,
// and cleaned. Empty -> "/" default (defense-in-depth: do not rely on callers).
func validateRoot(root string) (string, error) {
	if root == "" {
		root = defaultRootPath
	}
	if strings.Contains(root, "..") {
		return "", fmt.Errorf("reset: RootPath must not contain traversal segments: %q", root)
	}
	clean := filepath.Clean(root)
	if !filepath.IsAbs(clean) {
		return "", fmt.Errorf("reset: RootPath must be absolute, got %q", root)
	}
	return clean, nil
}

// removeArtifact safely removes one authoritative artifact. If the path itself is
// a symlink, the symlink is removed but the target is NOT followed: this avoids
// recursively destroying a bind-mount target the operator did not intend to wipe.
// (os.RemoveAll DOES traverse intermediate symlinks during recursion; this guard
// prevents that on the artifact root.)
func removeArtifact(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // idempotent
		}
		return fmt.Errorf("lstat %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		logrus.Warnf("provider-kubernetes: refusing to follow symlinked artifact %s (removing the link only, not its target)", path)
		return os.Remove(path)
	}
	return os.RemoveAll(path)
}

// Run performs a bounded, idempotent reset. `kubeadm reset` failing (e.g. on an
// already-clean node) is non-fatal: artifact cleanup still runs so reset is
// idempotent. It returns the first artifact-removal error, if any.
func Run(ctx context.Context, opts Options) error {
	root, err := validateRoot(opts.RootPath)
	if err != nil {
		return err
	}
	opts.RootPath = root

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"reset", "-f", "--cleanup-tmp-dir"}
	if opts.CRISocket != "" {
		args = append(args, "--cri-socket", opts.CRISocket)
	}
	if _, err := opts.Runner.Run(ctx, args...); err != nil {
		// Non-fatal: proceed to artifact cleanup so reset is idempotent. The
		// error is already secret-sanitized by the Runner.
		logrus.Warnf("provider-kubernetes: kubeadm reset returned an error (continuing cleanup): %v", err)
	}

	var firstErr error
	for _, p := range authoritativeArtifacts(opts.RootPath) {
		if err := removeArtifact(p); err != nil {
			logrus.Warnf("provider-kubernetes: failed to remove %s during reset: %v", p, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
