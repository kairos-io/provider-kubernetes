package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/kairos-io/kairos-sdk/clusterplugin"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
)

type fakeRunner struct {
	calls   [][]string
	respond func(args []string) (kubeadm.Result, error)
}

func (f *fakeRunner) Run(_ context.Context, args ...string) (kubeadm.Result, error) {
	f.calls = append(f.calls, args)
	if f.respond != nil {
		return f.respond(args)
	}
	return kubeadm.Result{}, nil
}

func (f *fakeRunner) called(name string) bool {
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == name {
			return true
		}
	}
	return false
}

func TestRunInitPipeline(t *testing.T) {
	fr := &fakeRunner{respond: func(args []string) (kubeadm.Result, error) {
		switch {
		case args[0] == "version":
			return kubeadm.Result{Stdout: "v1.34.0\n"}, nil
		case args[0] == "token" && args[1] == "generate":
			return kubeadm.Result{Stdout: "abcdef.0123456789abcdef\n"}, nil
		case args[0] == "certs":
			return kubeadm.Result{Stdout: strings.Repeat("a", 64) + "\n"}, nil
		}
		return kubeadm.Result{}, nil
	}}
	cluster := clusterplugin.Cluster{
		Role:             clusterplugin.RoleInit,
		ClusterToken:     validToken(),
		ControlPlaneHost: "10.0.0.1",
		ProviderOptions:  map[string]string{"cluster_root_path": t.TempDir()},
		Options:          "clusterConfiguration:\n  networking:\n    podSubnet: 10.244.0.0/16\n",
	}
	if err := Run(context.Background(), cluster, Options{Runner: fr, RunDir: t.TempDir()}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fr.called("init") {
		t.Fatalf("expected kubeadm init to be invoked; calls=%v", fr.calls)
	}
}

func TestRunWorkerJoinPipeline(t *testing.T) {
	fr := &fakeRunner{respond: func(args []string) (kubeadm.Result, error) {
		if args[0] == "version" {
			return kubeadm.Result{Stdout: "v1.35.0\n"}, nil
		}
		return kubeadm.Result{}, nil
	}}
	cluster := clusterplugin.Cluster{
		Role:             clusterplugin.RoleWorker,
		ClusterToken:     validToken(),
		ControlPlaneHost: "10.0.0.1",
		ProviderOptions:  map[string]string{"cluster_root_path": t.TempDir()},
		Options: "joinConfiguration:\n  discovery:\n    bootstrapToken:\n" +
			"      token: abcdef.0123456789abcdef\n      caCertHashes:\n      - sha256:deadbeef\n",
	}
	if err := Run(context.Background(), cluster, Options{
		Runner: fr,
		RunDir: t.TempDir(),
		// Inject a reachable probe so Plan yields [ActionRunJoin] directly (no wait),
		// avoiding real TCP dials to the fake endpoint in this hardware-free test.
		CPReachableProbe: func(_ context.Context) bool { return true },
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fr.called("join") {
		t.Fatalf("expected kubeadm join to be invoked; calls=%v", fr.calls)
	}
}

func TestRunRejectsUnsupportedVersion(t *testing.T) {
	fr := &fakeRunner{respond: func(args []string) (kubeadm.Result, error) {
		if args[0] == "version" {
			return kubeadm.Result{Stdout: "v1.30.0\n"}, nil
		}
		return kubeadm.Result{}, nil
	}}
	cluster := clusterplugin.Cluster{
		Role:             clusterplugin.RoleInit,
		ClusterToken:     validToken(),
		ControlPlaneHost: "10.0.0.1",
		ProviderOptions:  map[string]string{"cluster_root_path": t.TempDir()},
	}
	if err := Run(context.Background(), cluster, Options{Runner: fr, RunDir: t.TempDir()}); err == nil {
		t.Fatal("expected error for unsupported kubeadm version")
	}
}
