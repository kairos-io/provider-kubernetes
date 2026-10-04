//go:build linux

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kairos-io/kairos-sdk/bus"
	"github.com/kairos-io/kairos-sdk/clusterplugin"
	"github.com/mudler/go-pluggable"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
	"github.com/kairos-io/provider-kubernetes/internal/status"
)

type resetFakeRunner struct{ calls [][]string }

func (f *resetFakeRunner) Run(_ context.Context, args ...string) (kubeadm.Result, error) {
	f.calls = append(f.calls, args)
	return kubeadm.Result{}, nil
}

func resetEvent(t *testing.T, clusterYAML string) *pluggable.Event {
	t.Helper()
	payload := bus.EventPayload{Config: clusterYAML}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return &pluggable.Event{Data: string(data)}
}

func TestHandleClusterResetRunsReset(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "etc/kubernetes/admin.conf"), "apiVersion: v1\n")
	clusterYAML := "cluster:\n  providerConfig:\n    cluster_root_path: " + root + "\n  role: init\n"
	fr := &resetFakeRunner{}

	resp := handleClusterReset(resetEvent(t, clusterYAML), hermeticResetOptions(fr))
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	if len(fr.calls) != 1 || fr.calls[0][0] != "reset" {
		t.Fatalf("expected kubeadm reset to be invoked, got %v", fr.calls)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/kubernetes/admin.conf")); !os.IsNotExist(err) {
		t.Fatal("expected /etc/kubernetes emptied under the configured root")
	}
}

// TestResetClusterWritesStatus: the reset subcommand and the cluster.reset
// event share ResetCluster, so both record Phase=Reset, ResetOK on success
// and ResetFailed with the reason on failure.
func TestResetClusterWritesStatus(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		root := t.TempDir()
		mustWrite(t, filepath.Join(root, "etc/kubernetes/admin.conf"), "apiVersion: v1\n")
		sink := &recordingSink{}
		opts := hermeticResetOptions(&resetFakeRunner{})
		opts.Sink = sink
		cluster := clusterplugin.Cluster{Role: clusterplugin.RoleInit, ProviderOptions: map[string]string{"cluster_root_path": root}}

		if err := ResetCluster(context.Background(), cluster, opts); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := sink.only(t); got.Phase != status.PhaseReset || got.Reason != status.ReasonResetOK {
			t.Fatalf("recorded phase=%q reason=%q, want %q/%q", got.Phase, got.Reason, status.PhaseReset, status.ReasonResetOK)
		}
	})
	t.Run("failure", func(t *testing.T) {
		sink := &recordingSink{}
		opts := hermeticResetOptions(&resetFakeRunner{})
		opts.Sink = sink
		cluster := clusterplugin.Cluster{Role: clusterplugin.RoleInit, ProviderOptions: map[string]string{"cluster_root_path": "/tmp/../etc"}}

		if err := ResetCluster(context.Background(), cluster, opts); err == nil {
			t.Fatal("want an error for a traversal root path")
		}
		if got := sink.only(t); got.Phase != status.PhaseReset || got.Reason != status.ReasonResetFailed {
			t.Fatalf("recorded phase=%q reason=%q, want %q/%q", got.Phase, got.Reason, status.PhaseReset, status.ReasonResetFailed)
		}
	})
}

// TestResetClusterEtcdAdvisoryFollowsTheProbe: on a stacked-etcd control plane
// the full "member may stay registered" warning is for a node whose apiserver
// does not answer; when it answers, a "verify the member is gone" line takes
// its place, never silence. The full warning used to fire on every reset
// because no probe was wired.
func TestResetClusterEtcdAdvisoryFollowsTheProbe(t *testing.T) {
	for _, up := range []bool{true, false} {
		t.Run(fmt.Sprintf("apiserver up=%t", up), func(t *testing.T) {
			hook := captureLogs(t)
			root := t.TempDir()
			mustWrite(t, filepath.Join(root, "etc/kubernetes/manifests/etcd.yaml"), "kind: Pod\n")
			opts := hermeticResetOptions(&resetFakeRunner{})
			opts.ControlPlaneReachable = func(context.Context) bool { return up }
			cluster := clusterplugin.Cluster{Role: clusterplugin.RoleInit, ProviderOptions: map[string]string{"cluster_root_path": root}}

			if err := ResetCluster(context.Background(), cluster, opts); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			warned, verify := false, false
			for _, e := range hook.AllEntries() {
				warned = warned || strings.Contains(e.Message, "stacked-etcd control-plane node and its etcd member may not have been removed")
				verify = verify || strings.Contains(e.Message, "verify with `etcdctl member list`")
			}
			if warned == up || verify != up {
				t.Fatalf("full warning=%t, verify line=%t with the local apiserver up=%t", warned, verify, up)
			}
		})
	}
}

func TestHandleClusterResetNilEventIsSafe(t *testing.T) {
	if resp := handleClusterReset(nil, hermeticResetOptions(&resetFakeRunner{})); resp.Error != "" {
		t.Fatalf("nil event must be a safe no-op, got %q", resp.Error)
	}
}

func TestHandleClusterResetNoClusterIsNoop(t *testing.T) {
	fr := &resetFakeRunner{}
	resp := handleClusterReset(resetEvent(t, "other: value\n"), hermeticResetOptions(fr))
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("expected no reset when no cluster present, got %v", fr.calls)
	}
}

func TestHandleClusterResetRejectsOversizedPayload(t *testing.T) {
	// Construct an oversized event.Data (>1 MiB) without unmarshaling.
	big := make([]byte, (1<<20)+1)
	for i := range big {
		big[i] = 'a'
	}
	resp := handleClusterReset(&pluggable.Event{Data: string(big)}, hermeticResetOptions(&resetFakeRunner{}))
	if resp.Error == "" {
		t.Fatal("expected reset to reject oversized payload, got nil error")
	}
}

func TestHandleClusterResetIgnoresTokenValidation(t *testing.T) {
	// Reset must work even with an empty/invalid cluster_token (no token gate).
	root := t.TempDir()
	clusterYAML := "cluster:\n  cluster_token: \"\"\n  providerConfig:\n    cluster_root_path: " + root + "\n"
	resp := handleClusterReset(resetEvent(t, clusterYAML), hermeticResetOptions(&resetFakeRunner{}))
	if resp.Error != "" {
		t.Fatalf("reset must not fail on empty token, got %q", resp.Error)
	}
}

// hermeticResetOptions keeps a reset test off the host: the given runner, no
// status sink, and an apiserver probe that never dials.
func hermeticResetOptions(r kubeadm.Runner) ResetOptions {
	return ResetOptions{Runner: r, ControlPlaneReachable: func(context.Context) bool { return true }}
}
