package provider

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kairos-io/kairos-sdk/clusterplugin"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
	"github.com/kairos-io/provider-kubernetes/internal/reconcile"
	"github.com/kairos-io/provider-kubernetes/internal/status"
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
			return kubeadm.Result{Stdout: "v1.37.0\n"}, nil
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
	opts, sink := hermeticRunOptions(t, fr)
	if err := Run(context.Background(), cluster, opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fr.called("init") {
		t.Fatalf("expected kubeadm init to be invoked; calls=%v", fr.calls)
	}
	if got := sink.only(t); got.Phase != status.PhaseConverged || got.LastAction != string(reconcile.ActionRunInit) {
		t.Fatalf("recorded phase=%q lastAction=%q, want %q after %q", got.Phase, got.LastAction, status.PhaseConverged, reconcile.ActionRunInit)
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
	opts, sink := hermeticRunOptions(t, fr)
	// A reachable control plane makes Plan yield [ActionRunJoin] directly (no wait).
	opts.CPReachableProbe = func(_ context.Context) bool { return true }
	if err := Run(context.Background(), cluster, opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fr.called("join") {
		t.Fatalf("expected kubeadm join to be invoked; calls=%v", fr.calls)
	}
	if got := sink.only(t); got.Phase != status.PhaseConverged || got.LastAction != string(reconcile.ActionRunJoin) {
		t.Fatalf("recorded phase=%q lastAction=%q, want %q after %q", got.Phase, got.LastAction, status.PhaseConverged, reconcile.ActionRunJoin)
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
	opts, sink := hermeticRunOptions(t, fr)
	if err := Run(context.Background(), cluster, opts); err == nil {
		t.Fatal("expected error for unsupported kubeadm version")
	}
	if got := sink.only(t); got.Phase != status.PhaseFailed || got.Outcome != status.OutcomeFailure {
		t.Fatalf("recorded phase=%q outcome=%q, want %q/%q", got.Phase, got.Outcome, status.PhaseFailed, status.OutcomeFailure)
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
	opts, sink := hermeticRunOptions(t, fr)
	opts.ClusterVersionProbe = func(context.Context) string { return "v1.34.8" } // cluster not yet flipped
	opts.RunningKubeletVersionProbe = func(context.Context) string { return "v1.35.0" }
	opts.APIServerReachableProbe = func(context.Context) bool { return true }      // local API up -> no repair, straight to apply
	opts.EncryptionConfirmed = func(context.Context, string) bool { return false } // snapshot skipped (safe)
	opts.KubeletRestart = func(context.Context) error { restarted = true; return nil }
	err := Run(context.Background(), cluster, opts)
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
	if got := sink.only(t); got.Phase != status.PhaseConverged || got.LastAction != string(reconcile.ActionUpgradeApply) {
		t.Fatalf("recorded phase=%q lastAction=%q, want %q after %q", got.Phase, got.LastAction, status.PhaseConverged, reconcile.ActionUpgradeApply)
	}
}

// With no injected StatusSink, Run builds the default sink for the cluster root
// and, once the node is a member, publishes the Layer-2 annotation to the kubeadm
// node name rather than the OS hostname (ADR-4-S S3, Finding D). Both layers are
// stubbed in memory, so the host's kubectl and status paths are never touched.
func TestRunDefaultStatusSinkAnnotatesKubeadmNodeName(t *testing.T) {
	root := t.TempDir()
	adminConf := filepath.Join(root, "etc", "kubernetes", "admin.conf")
	mustWrite(t, adminConf, "apiVersion: v1\n") // already initialized -> converged no-op

	layer1 := &recordingSink{}
	kubectl := &fakeRunner{}
	builtFor := ""
	stubDefaultStatusSink(t, func(rootPath string) (status.StatusSink, *status.NodeAnnotationSink) {
		builtFor = rootPath
		annot := status.NewNodeAnnotationSink(rootPath, "")
		annot.Runner = kubectl
		return status.MultiSink{layer1, annot}, annot
	})

	fr := &fakeRunner{respond: func(args []string) (kubeadm.Result, error) {
		if args[0] == "version" {
			return kubeadm.Result{Stdout: "v1.35.0\n"}, nil
		}
		return kubeadm.Result{}, nil
	}}
	cluster := clusterplugin.Cluster{
		Role:             clusterplugin.RoleInit,
		ClusterToken:     validToken(),
		ControlPlaneHost: "10.0.0.1",
		ProviderOptions:  map[string]string{"cluster_root_path": root},
		Options:          "initConfiguration:\n  nodeRegistration:\n    name: kubeadm-node-1\n",
	}
	opts, _ := hermeticRunOptions(t, fr)
	opts.StatusSink = nil
	if err := Run(context.Background(), cluster, opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if builtFor != root {
		t.Fatalf("default status sink built for root %q, want %q", builtFor, root)
	}
	if got := layer1.only(t); got.Phase != status.PhaseConverged {
		t.Fatalf("recorded phase=%q, want %q", got.Phase, status.PhaseConverged)
	}
	if len(kubectl.calls) != 1 {
		t.Fatalf("kubectl calls=%v, want exactly one annotate", kubectl.calls)
	}
	call := kubectl.calls[0]
	if len(call) < 3 || call[0] != "annotate" || call[1] != "node" || call[2] != "kubeadm-node-1" {
		t.Fatalf("kubectl argv=%v, want annotate node kubeadm-node-1 ...", call)
	}
	if !slices.Contains(call, status.AnnotationPhase+"="+string(status.PhaseConverged)) {
		t.Fatalf("kubectl argv=%v, want the %s annotation", call, status.PhaseConverged)
	}
	if i := slices.Index(call, "--kubeconfig"); i < 0 || i+1 >= len(call) || call[i+1] != adminConf {
		t.Fatalf("kubectl argv=%v, want --kubeconfig %s", call, adminConf)
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
