package provider

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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
	// This fixture is "already initialized -> converged no-op" (see the
	// mustWrite comment above): make that literally true by injecting a
	// healthy kubelet, matching the test's intent. Without this, an
	// Initialized node with unknown/unverified kubelet health now (correctly,
	// D-2) reports PhaseDegraded, not PhaseConverged -- this test is about the
	// default status sink's node-name resolution, not kubelet health.
	opts.KubeletHealthyProbe = func(context.Context) bool { return true }
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

// TestRunAlreadyInitializedUnhealthyKubeletReportsDegradedNotConverged is D-2
// end-to-end (Run -> reconcile.Plan -> status.BuildStatus): an already
// Initialized node whose kubelet health probe reports false must NOT exit
// with an error (the reconcile action is still the reboot-safe ActionNone;
// #4099-1 never blocks later boot stages) and must NOT record phase=Converged
// -- it must record phase=Degraded, outcome=failure (a real problem, non-
// terminal), reason=KubeletUnhealthy.
func TestRunAlreadyInitializedUnhealthyKubeletReportsDegradedNotConverged(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "etc", "kubernetes", "admin.conf"), "apiVersion: v1\n")

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
	}
	opts, sink := hermeticRunOptions(t, fr)
	opts.KubeletHealthyProbe = func(context.Context) bool { return false }

	err := Run(context.Background(), cluster, opts)
	if err != nil {
		t.Fatalf("Run must not fail loud for a degraded (not unmet-prerequisite) reboot-safe no-op: %v", err)
	}
	if fr.called("init") {
		t.Fatalf("degraded already-initialized node must NOT auto re-bootstrap; calls=%v", fr.calls)
	}

	got := sink.only(t)
	if got.Phase == status.PhaseConverged {
		t.Fatalf("recorded phase=%q, must not be Converged when the kubelet is unhealthy", got.Phase)
	}
	if got.Phase != status.PhaseDegraded {
		t.Fatalf("recorded phase=%q, want %q", got.Phase, status.PhaseDegraded)
	}
	if got.Outcome != status.OutcomeFailure {
		t.Fatalf("recorded outcome=%q, want %q", got.Outcome, status.OutcomeFailure)
	}
	if got.Reason != status.ReasonKubeletUnhealthy {
		t.Fatalf("recorded reason=%q, want %q", got.Reason, status.ReasonKubeletUnhealthy)
	}
	if got.Terminal {
		t.Fatal("degraded must be non-terminal: a later boot (or an explicit reset) may still converge")
	}
}

// TestRunProductionKubeletHealthyProbeDefaultAgainstRealLoopback proves the
// wiring in run.go itself, not just the probe function in isolation: when
// Options.KubeletHealthyProbe is left at its nil zero value (the ONE field
// this test overrides away from hermeticRunOptions' healthy fake), Run
// consults the REAL production default (kubeletHealthyProbe: a loopback GET
// to 127.0.0.1:10248/healthz) -- the exact endpoint kubeadm itself waits on.
// This is the combination the fix exists for: an already-Initialized node
// with a live healthz reports Converged; the identical node with nothing
// listening reports Degraded with reason=KubeletUnhealthy. Before wiring a
// real default, EVERY already-converged node reported Degraded (nothing ever
// answered); before D-2 existed at all, a masked kubelet reported Converged.
//
// Skips (rather than fails) if 127.0.0.1:10248 cannot be bound in this
// environment, so a shared/restricted sandbox never makes this test flake;
// kubeletHealthyProbeAt's own unit tests already cover the probe logic
// against an arbitrary address.
func TestRunProductionKubeletHealthyProbeDefaultAgainstRealLoopback(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "etc", "kubernetes", "admin.conf"), "apiVersion: v1\n")
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
	}

	t.Run("live healthz on 127.0.0.1:10248 -> Converged", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:10248")
		if err != nil {
			t.Skipf("cannot bind 127.0.0.1:10248 in this environment: %v", err)
		}
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		_ = srv.Listener.Close()
		srv.Listener = ln
		srv.Start()
		defer srv.Close()

		opts, sink := hermeticRunOptions(t, fr)
		opts.KubeletHealthyProbe = nil // exercise the REAL production default
		if err := Run(context.Background(), cluster, opts); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := sink.only(t)
		if got.Phase != status.PhaseConverged {
			t.Fatalf("phase = %q, want Converged (a real loopback healthz answered 200)", got.Phase)
		}
	})

	t.Run("nothing listening on 127.0.0.1:10248 -> Degraded", func(t *testing.T) {
		// Confirm the port is actually free here (not left bound by a stray
		// process); if not, skip rather than risk a false pass/fail.
		probe, err := net.Listen("tcp", "127.0.0.1:10248")
		if err != nil {
			t.Skipf("127.0.0.1:10248 is not free in this environment: %v", err)
		}
		if err := probe.Close(); err != nil {
			t.Fatal(err)
		}

		opts, sink := hermeticRunOptions(t, fr)
		opts.KubeletHealthyProbe = nil // exercise the REAL production default
		if err := Run(context.Background(), cluster, opts); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := sink.only(t)
		if got.Phase != status.PhaseDegraded {
			t.Fatalf("phase = %q, want Degraded (nothing listens on 127.0.0.1:10248)", got.Phase)
		}
		if got.Reason != status.ReasonKubeletUnhealthy {
			t.Fatalf("reason = %q, want KubeletUnhealthy", got.Reason)
		}
	})
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

// Run must hand the cluster's localAPIEndpoint.bindPort to the production
// APIServerReachable default, not probe a fixed 6443. This exercises the REAL
// default (opts.APIServerReachableProbe = nil) against a live TLS /healthz on
// a non-default port, the way a cluster that moves the apiserver off 6443 (for
// example to leave 6443 to a VIP or load balancer) boots.
//
// With the port ignored the probe answers false, planUpgrade prepends
// ActionRepairKubeletConfig to a control plane whose apiserver is perfectly
// healthy, and the repair's own wait then polls the wrong port until the
// budget runs out: the upgrade never happens.
func TestRunProductionAPIServerProbeFollowsBindPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind a loopback port in this environment: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.StartTLS()
	defer srv.Close()

	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "etc", "kubernetes", "admin.conf"), "apiVersion: v1\n")
	mustWrite(t, filepath.Join(root, "etc", "kubernetes", "manifests", "kube-apiserver.yaml"),
		"spec:\n  containers:\n  - image: registry.k8s.io/kube-apiserver:v1.34.8\n")

	fr := &fakeRunner{respond: func(args []string) (kubeadm.Result, error) {
		if args[0] == "version" {
			return kubeadm.Result{Stdout: "v1.35.0\n"}, nil
		}
		return kubeadm.Result{}, nil
	}}
	cluster := clusterplugin.Cluster{
		Role:             clusterplugin.RoleControlPlane,
		ClusterToken:     validToken(),
		ControlPlaneHost: "10.0.0.1",
		ProviderOptions:  map[string]string{"cluster_root_path": root},
		Options: "clusterConfiguration:\n  kubernetesVersion: v1.35.0\n" +
			"initConfiguration:\n  localAPIEndpoint:\n    bindPort: " + strconv.Itoa(port) + "\n",
	}
	opts, sink := hermeticRunOptions(t, fr)
	opts.ClusterVersionProbe = func(context.Context) string { return "v1.34.8" }
	opts.RunningKubeletVersionProbe = func(context.Context) string { return "v1.35.0" }
	opts.APIServerReachableProbe = nil // exercise the REAL production default
	opts.EncryptionConfirmed = func(context.Context, string) bool { return false }
	opts.KubeletRestart = func(context.Context) error { return nil }

	// Bounded: if the probe misses the port the repair wait would otherwise sit
	// here for the whole reconcile budget.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Run(ctx, cluster, opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, c := range fr.calls {
		if len(c) >= 3 && c[0] == "init" && c[1] == "phase" && c[2] == "kubelet-start" {
			t.Fatalf("a healthy apiserver on port %d was reported down: Run planned a kubelet-config repair; calls=%v", port, fr.calls)
		}
	}
	if got := sink.only(t); got.Phase != status.PhaseConverged || got.LastAction != string(reconcile.ActionUpgradeApply) {
		t.Fatalf("recorded phase=%q lastAction=%q, want %q after %q", got.Phase, got.LastAction, status.PhaseConverged, reconcile.ActionUpgradeApply)
	}
}
