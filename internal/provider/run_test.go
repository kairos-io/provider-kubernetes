package provider

import (
	"context"
	"os"
	"path/filepath"
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

// ADR-12 U6: end-to-end upgrade wiring (Run -> Plan -> executor) for the
// control-plane apply path, fully hardware-free via injected probes.
func TestRunUpgradeApplyPipeline(t *testing.T) {
	root := t.TempDir()
	// Make this look like an Initialized control plane whose apiserver is still at
	// the old minor (so it needs to upgrade).
	mustWrite(t, filepath.Join(root, "etc", "kubernetes", "admin.conf"), "apiVersion: v1\n")
	mustWrite(t, filepath.Join(root, "etc", "kubernetes", "manifests", "kube-apiserver.yaml"),
		"spec:\n  containers:\n  - image: registry.k8s.io/kube-apiserver:v1.34.8\n")

	fr := &fakeRunner{respond: func(args []string) (kubeadm.Result, error) {
		if args[0] == "version" {
			return kubeadm.Result{Stdout: "v1.35.0\n"}, nil // bundled binary = target
		}
		return kubeadm.Result{}, nil
	}}
	cluster := clusterplugin.Cluster{
		Role:             clusterplugin.RoleControlPlane,
		ClusterToken:     validToken(),
		ControlPlaneHost: "10.0.0.1",
		ProviderOptions:  map[string]string{"cluster_root_path": root},
		// Pin the new version -> explicit upgrade intent (ADR-12).
		Options: "clusterConfiguration:\n  kubernetesVersion: v1.35.0\n",
	}
	restarted := false
	err := Run(context.Background(), cluster, Options{
		Runner:                     fr,
		RunDir:                     t.TempDir(),
		ClusterVersionProbe:        func(context.Context) string { return "v1.34.8" }, // cluster not yet flipped
		RunningKubeletVersionProbe: func(context.Context) string { return "v1.35.0" },
		APIServerReachableProbe:    func(context.Context) bool { return true },  // local API up -> no repair, straight to apply
		EncryptionConfirmed:        func(context.Context) bool { return false }, // snapshot skipped (safe)
		KubeletRestart:             func(context.Context) error { restarted = true; return nil },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The apply-authority CP must run `kubeadm upgrade apply v1.35.0`.
	foundApply := false
	for _, c := range fr.calls {
		if len(c) >= 3 && c[0] == "upgrade" && c[1] == "apply" && c[2] == "v1.35.0" {
			foundApply = true
		}
	}
	if !foundApply {
		t.Fatalf("expected 'kubeadm upgrade apply v1.35.0'; calls=%v", fr.calls)
	}
	if !restarted {
		t.Fatal("expected kubelet restart after upgrade apply")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
