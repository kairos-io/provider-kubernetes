package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadmconfig"
)

// writeKubeconfig creates <root>/etc/kubernetes/<name>, so kubeconfigFor(root)
// resolves it, and returns its path.
func writeKubeconfig(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, "etc", "kubernetes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", dir, err)
	}
	kc := filepath.Join(dir, name)
	if err := os.WriteFile(kc, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatalf("write %q: %v", kc, err)
	}
	return kc
}

func TestClusterVersionViaKubectl_ExactArgvAndStdoutOnly(t *testing.T) {
	root := t.TempDir()
	kc := writeKubeconfig(t, root, "admin.conf")
	wantArgs := []string{
		"--kubeconfig", kc,
		"-n", "kube-system", "get", "configmap", "kubeadm-config",
		"-o", "jsonpath={.data.ClusterConfiguration}",
	}

	fr := &fakeRunner{respond: func(args []string) (kubeadm.Result, error) {
		return kubeadm.Result{
			Stdout: "kind: ClusterConfiguration\nkubernetesVersion: v1.37.0\n",
			Stderr: "Warning: this is noise and must never be parsed",
		}, nil
	}}
	got := clusterVersionViaKubectl(root, fr)(context.Background())

	if got != "v1.37.0" {
		t.Fatalf("clusterVersionViaKubectl = %q, want v1.37.0", got)
	}
	if len(fr.calls) != 1 || !slices.Equal(fr.calls[0], wantArgs) {
		t.Fatalf("argv = %v, want %v", fr.calls, wantArgs)
	}
}

func TestClusterVersionViaKubectl_RunnerErrorReturnsEmpty(t *testing.T) {
	root := t.TempDir()
	writeKubeconfig(t, root, "admin.conf")

	fr := &fakeRunner{respond: func([]string) (kubeadm.Result, error) {
		return kubeadm.Result{}, fmt.Errorf("kubectl: boom")
	}}
	if got := clusterVersionViaKubectl(root, fr)(context.Background()); got != "" {
		t.Fatalf("clusterVersionViaKubectl = %q, want empty on runner error", got)
	}
}

// TestClusterVersionViaKubectl_NeverParsesStderr catches a regression to
// CombinedOutput-style parsing: stdout carries no version at all, while stderr
// carries a decoy that matches the version regex. Only Stdout may ever be
// consulted (ADR-1-A1), so the result must be empty.
func TestClusterVersionViaKubectl_NeverParsesStderr(t *testing.T) {
	root := t.TempDir()
	writeKubeconfig(t, root, "admin.conf")

	fr := &fakeRunner{respond: func([]string) (kubeadm.Result, error) {
		return kubeadm.Result{
			Stdout: "kind: ClusterConfiguration\n",
			Stderr: "Warning: kubernetesVersion: v9.9.9 (decoy, must never be parsed)",
		}, nil
	}}
	if got := clusterVersionViaKubectl(root, fr)(context.Background()); got != "" {
		t.Fatalf("clusterVersionViaKubectl = %q, want empty: a decoy version in Stderr must never be parsed", got)
	}
}

func TestClusterVersionViaKubectl_NoKubeconfigNeverInvokesRunner(t *testing.T) {
	root := t.TempDir() // no admin.conf / kubelet.conf under root
	fr := &fakeRunner{}
	if got := clusterVersionViaKubectl(root, fr)(context.Background()); got != "" {
		t.Fatalf("clusterVersionViaKubectl = %q, want empty with no kubeconfig", got)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("runner invoked with no kubeconfig available: %v", fr.calls)
	}
}

func TestRunningKubeletVersionViaKubectl_ExactArgvAndStdoutOnly(t *testing.T) {
	root := t.TempDir()
	kc := writeKubeconfig(t, root, "admin.conf")
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skip("cannot resolve hostname in this environment")
	}
	wantArgs := []string{
		"--kubeconfig", kc,
		"get", "node", strings.ToLower(host),
		"-o", "jsonpath={.status.nodeInfo.kubeletVersion}",
	}

	fr := &fakeRunner{respond: func(args []string) (kubeadm.Result, error) {
		return kubeadm.Result{Stdout: "v1.37.0", Stderr: "Warning: this is noise and must never be parsed"}, nil
	}}
	got := runningKubeletVersionViaKubectl(root, "", fr)(context.Background())

	if got != "v1.37.0" {
		t.Fatalf("runningKubeletVersionViaKubectl = %q, want v1.37.0", got)
	}
	if len(fr.calls) != 1 || !slices.Equal(fr.calls[0], wantArgs) {
		t.Fatalf("argv = %v, want %v", fr.calls, wantArgs)
	}
}

func TestRunningKubeletVersionViaKubectl_RunnerErrorReturnsEmpty(t *testing.T) {
	root := t.TempDir()
	writeKubeconfig(t, root, "admin.conf")

	fr := &fakeRunner{respond: func([]string) (kubeadm.Result, error) {
		return kubeadm.Result{}, fmt.Errorf("kubectl: boom")
	}}
	if got := runningKubeletVersionViaKubectl(root, "", fr)(context.Background()); got != "" {
		t.Fatalf("runningKubeletVersionViaKubectl = %q, want empty on runner error", got)
	}
}

// TestRunningKubeletVersionViaKubectl_NeverParsesStderr catches a regression to
// CombinedOutput-style parsing: an empty Stdout with a decoy version in Stderr
// must yield an empty result, never the Stderr content (ADR-1-A1).
func TestRunningKubeletVersionViaKubectl_NeverParsesStderr(t *testing.T) {
	root := t.TempDir()
	writeKubeconfig(t, root, "admin.conf")

	fr := &fakeRunner{respond: func([]string) (kubeadm.Result, error) {
		return kubeadm.Result{Stdout: "", Stderr: "v9.9.9"}, nil
	}}
	if got := runningKubeletVersionViaKubectl(root, "", fr)(context.Background()); got != "" {
		t.Fatalf("runningKubeletVersionViaKubectl = %q, want empty: Stderr must never be parsed", got)
	}
}

func TestRunningKubeletVersionViaKubectl_NoKubeconfigNeverInvokesRunner(t *testing.T) {
	root := t.TempDir()
	fr := &fakeRunner{}
	if got := runningKubeletVersionViaKubectl(root, "", fr)(context.Background()); got != "" {
		t.Fatalf("runningKubeletVersionViaKubectl = %q, want empty with no kubeconfig", got)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("runner invoked with no kubeconfig available: %v", fr.calls)
	}
}

// healthzServer starts a TLS /healthz server on loopback and returns its port.
func healthzServer(t *testing.T) int32 {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse %q: %v", srv.URL, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port of %q: %v", srv.URL, err)
	}
	return int32(p)
}

// A cluster that pins localAPIEndpoint.bindPort runs its apiserver on that
// port, so the local liveness probe has to follow it. Probing the kubeadm
// default instead reports a healthy control plane as down.
func TestLocalAPIHealthyProbe_FollowsBindPort(t *testing.T) {
	port := healthzServer(t)
	if port == 6443 {
		t.Skip("httptest picked the default port; nothing to distinguish")
	}
	if !localAPIHealthyProbe(port)(context.Background()) {
		t.Fatalf("probe with bindPort %d did not reach the apiserver on that port", port)
	}
	if localAPIHealthyProbe(0)(context.Background()) {
		t.Fatalf("probe with an unset bindPort answered true without a server on 6443")
	}
}

// bindPort is optional in the user config; unset must keep meaning 6443.
func TestLocalAPIHealthyProbe_UnsetBindPortUsesDefault(t *testing.T) {
	if got := kubeadmconfig.LocalAPIHealthzURL(0); got != "https://127.0.0.1:6443/healthz" {
		t.Fatalf("unset bindPort probes %q", got)
	}
}

// A cluster may set initConfiguration.nodeRegistration.name (samples/cluster.yaml
// documents it as "defaults to the node's hostname"), and the provider renders it
// into both the init and the join config, so kubeadm registers the Node under that
// name. Asking for the hostname instead gets a NotFound, which the probe reports as
// an empty version -- and reconcile.planUpgrade reads an empty version as "worker
// convergence unknown" and takes no action at all, so the worker silently never
// upgrades.
func TestRunningKubeletVersionViaKubectl_AsksForTheConfiguredNodeName(t *testing.T) {
	root := t.TempDir()
	kc := writeKubeconfig(t, root, "kubelet.conf")
	const nodeName = "worker-7"
	host, err := os.Hostname()
	if err == nil && strings.EqualFold(host, nodeName) {
		t.Skip("this host is already named like the fixture; nothing to distinguish")
	}
	wantArgs := []string{
		"--kubeconfig", kc,
		"get", "node", nodeName,
		"-o", "jsonpath={.status.nodeInfo.kubeletVersion}",
	}

	fr := &fakeRunner{respond: func([]string) (kubeadm.Result, error) {
		return kubeadm.Result{Stdout: "v1.36.4\n"}, nil
	}}
	if got := runningKubeletVersionViaKubectl(root, nodeName, fr)(context.Background()); got != "v1.36.4" {
		t.Fatalf("runningKubeletVersionViaKubectl = %q, want v1.36.4", got)
	}
	if len(fr.calls) != 1 || !slices.Equal(fr.calls[0], wantArgs) {
		t.Fatalf("argv = %v, want %v", fr.calls, wantArgs)
	}
}

// The configured name is used verbatim: kubeadm lower-cases only the hostname it
// derives itself, so a name the operator wrote is the name the Node carries.
func TestRunningKubeletVersionViaKubectl_DoesNotRewriteTheConfiguredNodeName(t *testing.T) {
	root := t.TempDir()
	writeKubeconfig(t, root, "kubelet.conf")

	fr := &fakeRunner{respond: func([]string) (kubeadm.Result, error) {
		return kubeadm.Result{Stdout: "v1.36.4"}, nil
	}}
	runningKubeletVersionViaKubectl(root, "Worker-7", fr)(context.Background())
	if len(fr.calls) != 1 || !slices.Contains(fr.calls[0], "Worker-7") {
		t.Fatalf("argv = %v, want it to carry the configured name verbatim", fr.calls)
	}
}

// TestKubeconfigFor_PrefersKubeletConf pins the precedence the two upgrade
// probes resolve a kubeconfig with. Both are read-only, best-effort reads about
// this node (the kubeadm-config ConfigMap and this node's own Node object), and
// kubelet.conf's system:node:<name> identity covers both: upstream kubeadm binds
// the kubeadm:nodes-kubeadm-config Role to the system:nodes group, and the Node
// authorizer lets a node get its own Node. admin.conf is system:masters, an
// over-grant for these two calls, and it is the credential that stops working
// first on a long-lived control plane because the kubelet rotates kubelet.conf
// and nothing rotates admin.conf.
//
// status.resolveKubeconfig makes the same choice for the same reason
// (security Q2) and its doc claims these two agree, so this test is also what
// keeps that claim true.
func TestKubeconfigFor_PrefersKubeletConf(t *testing.T) {
	root := t.TempDir()
	admin := writeKubeconfig(t, root, "admin.conf")
	kubelet := writeKubeconfig(t, root, "kubelet.conf")

	if got := kubeconfigFor(root); got != kubelet {
		t.Fatalf("kubeconfigFor with both files = %q, want kubelet.conf %q", got, kubelet)
	}

	// admin.conf remains the fallback: a control plane mid-bootstrap can have it
	// before the kubelet has been handed its own credential.
	if err := os.Remove(kubelet); err != nil {
		t.Fatal(err)
	}
	if got := kubeconfigFor(root); got != admin {
		t.Fatalf("kubeconfigFor without kubelet.conf = %q, want admin.conf %q", got, admin)
	}
}

// TestKubeconfigFor_EmptyRootIsAbsolute: an empty cluster_root_path means "/",
// never a path relative to the provider's working directory. options.go defaults
// it today, so this guards the function itself rather than a live caller --
// status.resolveKubeconfig already normalizes it the same way.
func TestKubeconfigFor_EmptyRootIsAbsolute(t *testing.T) {
	if got := kubeconfigFor(""); got != "" && !filepath.IsAbs(got) {
		t.Fatalf("kubeconfigFor(\"\") = %q, want \"\" or an absolute path", got)
	}

	// Prove it is not reading relative to the working directory.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "etc", "kubernetes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "etc", "kubernetes", "kubelet.conf"), []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if got := kubeconfigFor(""); got == filepath.Join("etc", "kubernetes", "kubelet.conf") {
		t.Fatalf("kubeconfigFor(\"\") resolved %q against the working directory", got)
	}
}
