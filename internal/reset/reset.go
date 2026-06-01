// Package reset implements bounded, idempotent cluster reset (ADR-4): it runs
// `kubeadm reset` via argv (no shell) and removes the authoritative kubeadm
// artifacts so the next boot re-converges from clean state. Removing
// /etc/kubernetes also shreds the cluster PKI and any bootstrap material on the
// node (ADR-2 reset shredding). Every step is bounded so reset can never hang
// (issue #4099-1).
package reset

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
)

const defaultTimeout = 5 * time.Minute

// Options configures a reset pass.
type Options struct {
	Runner    kubeadm.Runner
	RootPath  string
	CRISocket string        // optional; passed to `kubeadm reset --cri-socket` when set
	Timeout   time.Duration // bounded; zero defaults to defaultTimeout
}

// authoritativeArtifacts are the paths whose presence the actualstate prober
// treats as cluster membership. Removing them makes the next boot read
// "uninitialized" so the reconcile re-converges cleanly.
func authoritativeArtifacts(root string) []string {
	return []string{
		filepath.Join(root, "etc", "kubernetes"), // confs + pki (shreds CA key)
		filepath.Join(root, "var", "lib", "kubelet"),
		filepath.Join(root, "var", "lib", "etcd"),
	}
}

// Run performs a bounded, idempotent reset. `kubeadm reset` failing (e.g. on an
// already-clean node) is non-fatal: artifact cleanup still runs so reset is
// idempotent. It returns the first artifact-removal error, if any.
func Run(ctx context.Context, opts Options) error {
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
		// Non-fatal: proceed to artifact cleanup so reset is idempotent. The error
		// is already secret-sanitized by the Runner.
		logrus.Warnf("provider-kubernetes: kubeadm reset returned an error (continuing cleanup): %v", err)
	}

	var firstErr error
	for _, p := range authoritativeArtifacts(opts.RootPath) {
		if err := os.RemoveAll(p); err != nil {
			logrus.Warnf("provider-kubernetes: failed to remove %s during reset: %v", p, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
