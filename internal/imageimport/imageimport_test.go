package imageimport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
)

// fakeRunner records the argv of every Run call and can be made to fail.
type fakeRunner struct {
	calls   [][]string
	failOn  string // basename of a tar to fail on ("" = none)
	failErr error
}

func (f *fakeRunner) Run(_ context.Context, args ...string) (kubeadm.Result, error) {
	f.calls = append(f.calls, args)
	if f.failOn != "" && len(args) > 0 && filepath.Base(args[len(args)-1]) == f.failOn {
		return kubeadm.Result{Stderr: "boom"}, f.failErr
	}
	return kubeadm.Result{}, nil
}

func writeTar(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestImportRunsCtrPerTarball(t *testing.T) {
	dir := t.TempDir()
	writeTar(t, dir, "etcd.tar")
	writeTar(t, dir, "kube-apiserver.tar")
	writeTar(t, dir, "ignored.txt") // non-tar must be skipped

	fr := &fakeRunner{}
	if err := Import(context.Background(), dir, fr); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(fr.calls) != 2 {
		t.Fatalf("want 2 ctr calls (one per .tar), got %d: %v", len(fr.calls), fr.calls)
	}
	// Exact argv: ctr -n k8s.io images import <tar> (sorted, so etcd first).
	want := []string{"-n", "k8s.io", "images", "import", filepath.Join(dir, "etcd.tar")}
	got := fr.calls[0]
	if len(got) != len(want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestImportNoopOnEmptyOrMissingDir(t *testing.T) {
	fr := &fakeRunner{}
	// Empty dir.
	if err := Import(context.Background(), t.TempDir(), fr); err != nil {
		t.Fatalf("empty dir: want nil, got %v", err)
	}
	// Missing dir.
	if err := Import(context.Background(), filepath.Join(t.TempDir(), "nope"), fr); err != nil {
		t.Fatalf("missing dir: want nil, got %v", err)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("want 0 ctr calls on empty/missing dir, got %d", len(fr.calls))
	}
}

func TestImportAttemptsAllAndReturnsError(t *testing.T) {
	dir := t.TempDir()
	writeTar(t, dir, "a.tar")
	writeTar(t, dir, "b.tar") // fails
	writeTar(t, dir, "c.tar")

	fr := &fakeRunner{failOn: "b.tar", failErr: errors.New("import failed")}
	err := Import(context.Background(), dir, fr)
	if err == nil {
		t.Fatal("want error when a tarball fails to import, got nil")
	}
	// All three are still attempted (a bad tarball does not skip the rest).
	if len(fr.calls) != 3 {
		t.Fatalf("want all 3 tarballs attempted, got %d calls", len(fr.calls))
	}
}
