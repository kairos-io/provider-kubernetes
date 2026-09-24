//go:build linux

package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kairos-io/provider-kubernetes/internal/status"
)

// TestRunUnfinishedInitReportsInitIncomplete is the failure this verdict
// exists for: `kubeadm init` wrote admin.conf and started the kubelet, then
// stopped before kubelet-finalize, so kubelet.conf still embeds its
// certificate while the kubelet's rotated one exists. That node used to be
// reported Converged on every boot after the failed one. It is decided from
// files, so it never waits for the apiserver, whatever the apiserver does.
func TestRunUnfinishedInitReportsInitIncomplete(t *testing.T) {
	for _, apiUp := range []bool{true, false} {
		name := "apiserver down"
		if apiUp {
			name = "apiserver serving"
		}
		t.Run(name, func(t *testing.T) {
			root, cluster := initializedControlPlane(t, "")
			mustWrite(t, filepath.Join(root, "etc", "kubernetes", "kubelet.conf"),
				"users:\n- name: system:node:cp1\n  user:\n    client-certificate-data: ZmFrZQ==\n    client-key-data: ZmFrZQ==\n")
			pki := filepath.Join(root, "var", "lib", "kubelet", "pki")
			mustWrite(t, filepath.Join(pki, "kubelet-client-2026-09-24-10-00-00.pem"), "placeholder\n")
			if err := os.Symlink("kubelet-client-2026-09-24-10-00-00.pem", filepath.Join(pki, "kubelet-client-current.pem")); err != nil {
				t.Fatal(err)
			}

			fr := versionOnlyRunner()
			opts, sink := hermeticRunOptions(t, fr)
			api := &scriptedProbe{answers: []bool{apiUp}}
			opts.APIServerReachableProbe = api.probe

			if err := Run(context.Background(), cluster, opts); err != nil {
				t.Fatalf("a degraded member is not a failed pass: %v", err)
			}
			if fr.called("init") || fr.called("reset") {
				t.Fatalf("an unfinished init is reported, never repaired automatically; calls=%v", fr.calls)
			}
			got := sink.only(t)
			if got.Phase != status.PhaseDegraded || got.Reason != status.ReasonInitIncomplete {
				t.Fatalf("recorded phase=%q reason=%q, want %q/%q", got.Phase, got.Reason, status.PhaseDegraded, status.ReasonInitIncomplete)
			}
			if api.count() != 1 {
				t.Fatalf("apiserver probed %d times, want exactly 1 (no grace wait for a file-based verdict)", api.count())
			}
		})
	}
}
