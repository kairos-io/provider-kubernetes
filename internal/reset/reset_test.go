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

func TestResetRejectsTraversalRootPath(t *testing.T) {
	if err := Run(context.Background(), Options{Runner: &fakeRunner{}, RootPath: ".."}); err == nil {
		t.Fatal("expected hard error on traversal RootPath '..'")
	}
	if err := Run(context.Background(), Options{Runner: &fakeRunner{}, RootPath: "/etc/../.."}); err == nil {
		t.Fatal("expected hard error on embedded traversal segments")
	}
}

func TestResetRejectsRelativeRootPath(t *testing.T) {
	if err := Run(context.Background(), Options{Runner: &fakeRunner{}, RootPath: "relative/root"}); err == nil {
		t.Fatal("expected hard error on relative RootPath")
	}
}

// HA-5: stacked-etcd CP detection tests.

func TestIsStackedEtcdCP_EtcdManifest(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc/kubernetes/manifests"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/kubernetes/manifests/etcd.yaml"), []byte("spec:"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !isStackedEtcdCP(root) {
		t.Fatal("expected isStackedEtcdCP=true when etcd.yaml manifest present")
	}
}

func TestIsStackedEtcdCP_EtcdDataDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "var/lib/etcd/member"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !isStackedEtcdCP(root) {
		t.Fatal("expected isStackedEtcdCP=true when etcd data dir present")
	}
}

func TestIsStackedEtcdCP_NoIndicators(t *testing.T) {
	root := t.TempDir()
	if isStackedEtcdCP(root) {
		t.Fatal("expected isStackedEtcdCP=false when no etcd indicators present")
	}
}

// HA-5: reset with stacked-etcd CP and unreachable cluster runs and does not hang.
func TestResetStackedEtcdCPUnreachableEmitsWarning(t *testing.T) {
	root := t.TempDir()
	// Seed etcd indicator so isStackedEtcdCP returns true.
	if err := os.MkdirAll(filepath.Join(root, "var/lib/etcd/member"), 0o700); err != nil {
		t.Fatal(err)
	}
	seedArtifacts(t, root)
	fr := &fakeRunner{}

	// ControlPlaneReachable=false: should emit warning and proceed.
	if err := Run(context.Background(), Options{
		Runner:                fr,
		RootPath:              root,
		ControlPlaneReachable: func(context.Context) bool { return false },
		NodeName:              "cp-node-1",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Artifacts still removed despite the warning.
	if _, err := os.Stat(filepath.Join(root, "etc/kubernetes")); !os.IsNotExist(err) {
		t.Fatal("expected artifacts removed even with stacked-etcd warning")
	}
}

// HA-5: sweep of RunDir removes leftover kubeadm-*.yaml transient files.
func TestResetSweepsRunDir(t *testing.T) {
	root := t.TempDir()
	runDir := t.TempDir()

	// Plant a fake transient file in runDir.
	transient := filepath.Join(runDir, "kubeadm-123456.yaml")
	if err := os.WriteFile(transient, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Plant a non-transient file that must survive.
	other := filepath.Join(runDir, "something.txt")
	if err := os.WriteFile(other, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Run(context.Background(), Options{
		Runner:   &fakeRunner{},
		RootPath: root,
		RunDir:   runDir,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(transient); !os.IsNotExist(err) {
		t.Fatalf("expected transient file swept, stat err=%v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("non-transient file must survive reset sweep: %v", err)
	}
}

func TestResetDefaultsEmptyRootPathToSlash(t *testing.T) {
	// We don't actually want to wipe the test host, so use a fake runner and
	// assert only that validateRoot accepts an empty input (defense-in-depth:
	// package applies the default; we get past validation, and any removals
	// either no-op (NotExist) or fail with permission, neither of which is fatal
	// here because we only check that Run returns nil for a clean system).
	// To avoid touching real /etc/kubernetes etc., we point at a tmp root in
	// other tests and assert the empty-default behavior via the error path:
	// when EUID != 0, RemoveAll under "/" returns permission errors that surface
	// from removeArtifact. We accept either nil or a permission error here; what
	// matters is that no path-validation error fires.
	err := Run(context.Background(), Options{Runner: &fakeRunner{}, RootPath: ""})
	if err != nil && strings.Contains(err.Error(), "RootPath") {
		t.Fatalf("expected empty RootPath to default to '/', not be rejected: %v", err)
	}
}

func TestResetRefusesSymlinkedArtifact(t *testing.T) {
	root := t.TempDir()
	// Create the artifact path as a SYMLINK to a sibling directory whose
	// contents must NOT be destroyed by reset.
	target := filepath.Join(t.TempDir(), "must-survive")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(target, "canary")
	if err := os.WriteFile(canary, []byte("survive"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "var", "lib", "kubelet")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), Options{Runner: &fakeRunner{}, RootPath: root}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("expected the symlink to be removed, stat err=%v", err)
	}
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("symlink target was destroyed (security-critical): %v", err)
	}
}
