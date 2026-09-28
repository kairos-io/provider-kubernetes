//go:build linux

package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kairos-io/kairos-sdk/clusterplugin"

	"github.com/kairos-io/provider-kubernetes/internal/status"
)

// unfinishedInitRoot lays out what `kubeadm init` leaves when it stops
// before kubelet-finalize: admin.conf, a kubelet.conf that still embeds its
// certificate, and the kubelet's rotated certificate. kubeadm's documented
// recovery for a failed kubelet client-certificate rotation leaves the same
// files on a healthy node, which is what the cluster check tells apart.
func unfinishedInitRoot(t *testing.T) clusterplugin.Cluster {
	t.Helper()
	root, cluster := initializedControlPlane(t, "")
	mustWrite(t, filepath.Join(root, "etc", "kubernetes", "kubelet.conf"),
		"users:\n- name: system:node:cp1\n  user:\n    client-certificate-data: ZmFrZQ==\n    client-key-data: ZmFrZQ==\n")
	pki := filepath.Join(root, "var", "lib", "kubelet", "pki")
	mustWrite(t, filepath.Join(pki, "kubelet-client-2026-09-24-10-00-00.pem"), "placeholder\n")
	if err := os.Symlink("kubelet-client-2026-09-24-10-00-00.pem", filepath.Join(pki, "kubelet-client-current.pem")); err != nil {
		t.Fatal(err)
	}
	return cluster
}

// TestRunUnfinishedInitReportsInitIncomplete is the failure this verdict
// exists for: the files show an init that stopped before kubelet-finalize and
// the cluster confirms it (no addon objects). That node used to be reported
// Converged on every boot after the failed one. Nothing is repaired.
func TestRunUnfinishedInitReportsInitIncomplete(t *testing.T) {
	t.Run("apiserver serving: the cluster is asked once, without waiting", func(t *testing.T) {
		cluster := unfinishedInitRoot(t)
		fr := versionOnlyRunner()
		opts, sink := hermeticRunOptions(t, fr)
		api := &scriptedProbe{answers: []bool{true}}
		opts.APIServerReachableProbe = api.probe
		asked := &scriptedProbe{answers: []bool{false}}
		opts.InitFinishedProbe = asked.answer

		if err := Run(context.Background(), cluster, opts); err != nil {
			t.Fatalf("a degraded member is not a failed pass: %v", err)
		}
		if fr.called("init") || fr.called("reset") {
			t.Fatalf("an unfinished init is reported, never repaired automatically; calls=%v", fr.calls)
		}
		if got := sink.only(t); got.Phase != status.PhaseDegraded || got.Reason != status.ReasonInitIncomplete {
			t.Fatalf("recorded phase=%q reason=%q, want %q/%q", got.Phase, got.Reason, status.PhaseDegraded, status.ReasonInitIncomplete)
		}
		if api.count() != 1 || asked.count() != 1 {
			t.Fatalf("apiserver probed %d times and cluster asked %d times, want 1 and 1", api.count(), asked.count())
		}
	})
	t.Run("apiserver down: the grace is spent and the cluster is never asked", func(t *testing.T) {
		cluster := unfinishedInitRoot(t)
		opts, sink := hermeticRunOptions(t, versionOnlyRunner())
		api := &scriptedProbe{answers: []bool{false}}
		opts.APIServerReachableProbe = api.probe
		// opts.InitFinishedProbe stays failIfCalled: with no apiserver there is
		// nothing to ask, and the finding stands.

		if err := Run(context.Background(), cluster, opts); err != nil {
			t.Fatalf("a degraded member is not a failed pass: %v", err)
		}
		if got := sink.only(t); got.Reason != status.ReasonInitIncomplete {
			t.Fatalf("recorded reason=%q, want %q", got.Reason, status.ReasonInitIncomplete)
		}
		if api.count() < 2 {
			t.Fatalf("apiserver probed %d times, want polling through the grace period", api.count())
		}
	})
}

// TestRunClusterCheckFailureIsNotEvidence: when the cluster cannot be asked
// (here admin.conf's certificate has expired, so kubectl fails although
// /healthz answers), the finding stands unconfirmed, and the log says the
// question went unanswered. It must never claim init did not finish: on a
// healthy node that would point the operator at a reset that deletes etcd.
func TestRunClusterCheckFailureIsNotEvidence(t *testing.T) {
	hook := captureLogs(t)
	cluster := unfinishedInitRoot(t)
	opts, sink := hermeticRunOptions(t, versionOnlyRunner())
	opts.APIServerReachableProbe = func(context.Context) bool { return true }
	opts.InitFinishedProbe = func(context.Context) (bool, error) { return false, errors.New("kubectl exit 1") }

	if err := Run(context.Background(), cluster, opts); err != nil {
		t.Fatalf("a degraded member is not a failed pass: %v", err)
	}
	if got := sink.only(t); got.Reason != status.ReasonInitIncomplete {
		t.Fatalf("recorded reason=%q, want %q", got.Reason, status.ReasonInitIncomplete)
	}
	var asked bool
	for _, e := range hook.AllEntries() {
		if strings.Contains(e.Message, "did not finish") {
			t.Fatalf("logged %q although the cluster was never asked", e.Message)
		}
		if strings.Contains(e.Message, "could not be asked") && strings.Contains(e.Message, "kubectl exit 1") {
			asked = true
		}
	}
	if !asked {
		t.Fatal("no log line says the cluster could not be asked")
	}
}

// TestRunReissuedKubeletConfIsNotInitIncomplete is the false positive the
// cluster check removes: the files look like an unfinished init, but the
// cluster has the addon phase's objects, so init finished long ago and
// kubelet.conf was re-issued by hand (kubeadm's documented recovery for a
// failed kubelet client-certificate rotation, last step skipped).
func TestRunReissuedKubeletConfIsNotInitIncomplete(t *testing.T) {
	for name, answers := range map[string][]bool{
		"apiserver already serving":           {true},
		"apiserver comes up during the grace": {false, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			cluster := unfinishedInitRoot(t)
			opts, sink := hermeticRunOptions(t, versionOnlyRunner())
			api := &scriptedProbe{answers: answers}
			opts.APIServerReachableProbe = api.probe
			asked := &scriptedProbe{answers: []bool{true}}
			opts.InitFinishedProbe = asked.answer

			if err := Run(context.Background(), cluster, opts); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := sink.only(t); got.Phase != status.PhaseConverged {
				t.Fatalf("recorded phase=%q reason=%q, want %q", got.Phase, got.Reason, status.PhaseConverged)
			}
			if asked.count() != 1 {
				t.Fatalf("cluster asked %d times, want exactly 1", asked.count())
			}
		})
	}
}
