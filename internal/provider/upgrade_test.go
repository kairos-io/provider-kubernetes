package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
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
	got := runningKubeletVersionViaKubectl(root, fr)(context.Background())

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
	if got := runningKubeletVersionViaKubectl(root, fr)(context.Background()); got != "" {
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
	if got := runningKubeletVersionViaKubectl(root, fr)(context.Background()); got != "" {
		t.Fatalf("runningKubeletVersionViaKubectl = %q, want empty: Stderr must never be parsed", got)
	}
}

func TestRunningKubeletVersionViaKubectl_NoKubeconfigNeverInvokesRunner(t *testing.T) {
	root := t.TempDir()
	fr := &fakeRunner{}
	if got := runningKubeletVersionViaKubectl(root, fr)(context.Background()); got != "" {
		t.Fatalf("runningKubeletVersionViaKubectl = %q, want empty with no kubeconfig", got)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("runner invoked with no kubeconfig available: %v", fr.calls)
	}
}
