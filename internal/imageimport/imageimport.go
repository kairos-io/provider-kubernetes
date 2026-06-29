// Package imageimport imports the control-plane container-image tarballs that the
// image build pre-bundles (ADR-16) into containerd's "k8s.io" namespace, so that
// kubeadm init finds the control-plane images locally and never pulls from a
// registry -- enabling a first boot to converge fully air-gapped.
//
// It is invoked at boot by a systemd oneshot (ordered After=containerd.service,
// Before=kubelet.service) via the `agent-provider-kubernetes import-images`
// subcommand. The operation is idempotent (re-importing already-present images is
// harmless), bounded by the caller's context, and a no-op when no tarballs are
// present.
package imageimport

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/sirupsen/logrus"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
)

// DefaultDir is the read-only (immutable) path the image build embeds the
// control-plane image tarballs at. Being in the OS image layer, they survive
// reboots and are available with no network (air-gap).
const DefaultDir = "/opt/provider-kubernetes/images"

// Runner executes `ctr` via argv (no shell). It is satisfied by
// kubeadm.ExecRunner{Path: "ctr"}; the interface keeps Import unit-testable.
type Runner interface {
	Run(ctx context.Context, args ...string) (kubeadm.Result, error)
}

// Import loads every *.tar under dir into containerd's k8s.io namespace via
// `ctr -n k8s.io images import <tar>`. It returns nil (a logged no-op) when dir
// has no tarballs -- so an image with nothing bundled still boots. It attempts
// every tarball and returns the first error encountered (fail loud without
// silently skipping the rest); a partial failure leaves the missing image to be
// pulled by kubeadm under its own bounded budget rather than blocking boot.
func Import(ctx context.Context, dir string, r Runner) error {
	tars, err := filepath.Glob(filepath.Join(dir, "*.tar"))
	if err != nil {
		return fmt.Errorf("glob %q: %w", dir, err)
	}
	if len(tars) == 0 {
		logrus.Infof("image-import: no tarballs under %s; nothing to import", dir)
		return nil
	}
	sort.Strings(tars)

	var firstErr error
	for _, tar := range tars {
		out, runErr := r.Run(ctx, "-n", "k8s.io", "images", "import", tar)
		if runErr != nil {
			logrus.Errorf("image-import: failed to import %s: %v\n%s",
				filepath.Base(tar), runErr, kubeadm.Sanitize(out.Stderr))
			if firstErr == nil {
				firstErr = fmt.Errorf("import %s: %w", filepath.Base(tar), runErr)
			}
			continue
		}
		logrus.Infof("image-import: imported %s", filepath.Base(tar))
	}
	if firstErr != nil {
		return fmt.Errorf("one or more image imports failed: %w", firstErr)
	}
	logrus.Infof("image-import: imported %d tarball(s) from %s", len(tars), dir)
	return nil
}
