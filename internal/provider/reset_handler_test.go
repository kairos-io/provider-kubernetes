package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kairos-io/kairos-sdk/bus"
	"github.com/mudler/go-pluggable"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
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
	if err := os.MkdirAll(filepath.Join(root, "etc/kubernetes"), 0o700); err != nil {
		t.Fatal(err)
	}
	clusterYAML := "cluster:\n  providerConfig:\n    cluster_root_path: " + root + "\n  role: init\n"
	fr := &resetFakeRunner{}

	resp := handleClusterReset(resetEvent(t, clusterYAML), fr, nil)
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	if len(fr.calls) != 1 || fr.calls[0][0] != "reset" {
		t.Fatalf("expected kubeadm reset to be invoked, got %v", fr.calls)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/kubernetes")); !os.IsNotExist(err) {
		t.Fatal("expected /etc/kubernetes removed under the configured root")
	}
}

func TestHandleClusterResetNilEventIsSafe(t *testing.T) {
	if resp := handleClusterReset(nil, &resetFakeRunner{}, nil); resp.Error != "" {
		t.Fatalf("nil event must be a safe no-op, got %q", resp.Error)
	}
}

func TestHandleClusterResetNoClusterIsNoop(t *testing.T) {
	fr := &resetFakeRunner{}
	resp := handleClusterReset(resetEvent(t, "other: value\n"), fr, nil)
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
	resp := handleClusterReset(&pluggable.Event{Data: string(big)}, &resetFakeRunner{}, nil)
	if resp.Error == "" {
		t.Fatal("expected reset to reject oversized payload, got nil error")
	}
}

func TestHandleClusterResetIgnoresTokenValidation(t *testing.T) {
	// Reset must work even with an empty/invalid cluster_token (no token gate).
	root := t.TempDir()
	clusterYAML := "cluster:\n  cluster_token: \"\"\n  providerConfig:\n    cluster_root_path: " + root + "\n"
	resp := handleClusterReset(resetEvent(t, clusterYAML), &resetFakeRunner{}, nil)
	if resp.Error != "" {
		t.Fatalf("reset must not fail on empty token, got %q", resp.Error)
	}
}
