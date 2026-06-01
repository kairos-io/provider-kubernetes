package reset

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
)

type fakeRunner struct {
	calls [][]string
	err   error
}

func (f *fakeRunner) Run(_ context.Context, args ...string) (kubeadm.Result, error) {
	f.calls = append(f.calls, args)
	return kubeadm.Result{}, f.err
}

func seedArtifacts(t *testing.T, root string) {
	t.Helper()
	for _, d := range []string{"etc/kubernetes/pki", "var/lib/kubelet", "var/lib/etcd"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A stand-in for the CA key that must be shredded.
	if err := os.WriteFile(filepath.Join(root, "etc/kubernetes/pki/ca.key"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResetRunsKubeadmAndRemovesArtifacts(t *testing.T) {
	root := t.TempDir()
	seedArtifacts(t, root)
	fr := &fakeRunner{}

	if err := Run(context.Background(), Options{Runner: fr, RootPath: root, CRISocket: "unix:///run/containerd/containerd.sock"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// kubeadm reset invoked with the expected bounded argv.
	if len(fr.calls) != 1 {
		t.Fatalf("expected one kubeadm call, got %v", fr.calls)
	}
	got := strings.Join(fr.calls[0], " ")
	if got != "reset -f --cleanup-tmp-dir --cri-socket unix:///run/containerd/containerd.sock" {
		t.Fatalf("unexpected reset argv: %q", got)
	}
	// Authoritative artifacts (incl. the CA key) are gone.
	for _, d := range []string{"etc/kubernetes", "var/lib/kubelet", "var/lib/etcd"} {
		if _, err := os.Stat(filepath.Join(root, d)); !os.IsNotExist(err) {
			t.Fatalf("expected %s removed, stat err=%v", d, err)
		}
	}
}

func TestResetIsIdempotentOnKubeadmError(t *testing.T) {
	root := t.TempDir()
	seedArtifacts(t, root)
	fr := &fakeRunner{err: context.DeadlineExceeded} // kubeadm reset "fails"

	// Cleanup still happens and Run succeeds (idempotent).
	if err := Run(context.Background(), Options{Runner: fr, RootPath: root}); err != nil {
		t.Fatalf("expected reset to continue cleanup despite kubeadm error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/kubernetes")); !os.IsNotExist(err) {
		t.Fatal("expected artifacts removed even when kubeadm reset errored")
	}
}

func TestResetNoCRISocketOmitsFlag(t *testing.T) {
	fr := &fakeRunner{}
	if err := Run(context.Background(), Options{Runner: fr, RootPath: t.TempDir()}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(strings.Join(fr.calls[0], " "), "--cri-socket") {
		t.Fatalf("did not expect --cri-socket: %v", fr.calls[0])
	}
}
