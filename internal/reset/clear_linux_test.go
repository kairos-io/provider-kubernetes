//go:build linux

package reset

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"golang.org/x/sys/unix"
)

func mkfile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// withHooks installs the test seams for one test and removes them after.
func withHooks(t *testing.T, openat2, unlink func(name string) error) {
	t.Helper()
	testHookOpenat2, testHookUnlinkat = openat2, unlink
	t.Cleanup(func() { testHookOpenat2, testHookUnlinkat = nil, nil })
}

// TestOpenBeneathRefusesARealMountPoint checks the mechanism itself on this
// kernel, without privileges: /proc is always a mount point below /, so
// opening it from a descriptor of / with RESOLVE_NO_XDEV must fail with EXDEV.
func TestOpenBeneathRefusesARealMountPoint(t *testing.T) {
	root, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(root) }()
	fd, err := openBeneath(root, "proc")
	if err == nil {
		_ = unix.Close(fd)
		t.Fatal("openat2 RESOLVE_NO_XDEV opened /proc from /: it would cross into a mounted filesystem")
	}
	if errors.Is(err, unix.ENOSYS) {
		t.Skip("this kernel has no openat2")
	}
	if !errors.Is(err, unix.EXDEV) {
		t.Fatalf("openat2 on /proc from / returned %v, want EXDEV", err)
	}
}

// TestClearArtifactKeepsAMountedDirectory: the volume of a pod whose unmount
// kubeadm reset could not do. Its data is never touched; everything around it
// goes.
func TestClearArtifactKeepsAMountedDirectory(t *testing.T) {
	kubelet := filepath.Join(t.TempDir(), "var", "lib", "kubelet")
	data := filepath.Join(kubelet, "pods", "p1", "volumes", "kubernetes.io~nfs", "data")
	mkfile(t, filepath.Join(data, "precious"), "volume data")
	mkfile(t, filepath.Join(kubelet, "pods", "p1", "etc-hosts"), "x")
	mkfile(t, filepath.Join(kubelet, "pods", "p2", "containers", "c", "log"), "x")
	mkfile(t, filepath.Join(kubelet, "kubeadm-flags.env"), "x")
	withHooks(t, func(name string) error {
		if name == "data" {
			return unix.EXDEV
		}
		return nil
	}, nil)

	kept, err := clearArtifact(kubelet)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(kept) != 1 || kept[0] != data {
		t.Fatalf("kept = %v, want exactly %s", kept, data)
	}
	if b, err := os.ReadFile(filepath.Join(data, "precious")); err != nil || string(b) != "volume data" {
		t.Fatalf("the mounted volume's data was touched: %q, %v", b, err)
	}
	for _, gone := range []string{"kubeadm-flags.env", "pods/p2", "pods/p1/etc-hosts"} {
		if _, err := os.Lstat(filepath.Join(kubelet, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived the reset (err=%v)", gone, err)
		}
	}
}

// TestClearArtifactKeepsAMountedFile: a file bind mount (a subPath of one
// file) cannot be unlinked; it is kept, not reported as a failure.
func TestClearArtifactKeepsAMountedFile(t *testing.T) {
	kubelet := filepath.Join(t.TempDir(), "kubelet")
	file := filepath.Join(kubelet, "pods", "p1", "volume-subpaths", "cfg", "c", "0")
	mkfile(t, file, "bound file")
	withHooks(t, nil, func(name string) error {
		if name == "0" {
			return unix.EBUSY
		}
		return nil
	})

	kept, err := clearArtifact(kubelet)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(kept) != 1 || kept[0] != file {
		t.Fatalf("kept = %v, want exactly %s", kept, file)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("the bound file was removed: %v", err)
	}
}

// TestClearArtifactWithoutOpenat2DescendsNothing: if the kernel cannot say
// whether a directory is a mount point, the walk does not go into it.
func TestClearArtifactWithoutOpenat2DescendsNothing(t *testing.T) {
	kubelet := filepath.Join(t.TempDir(), "kubelet")
	mkfile(t, filepath.Join(kubelet, "pods", "p1", "data"), "x")
	mkfile(t, filepath.Join(kubelet, "top-file"), "x")
	withHooks(t, func(string) error { return unix.ENOSYS }, nil)

	kept, err := clearArtifact(kubelet)
	if err == nil || !strings.Contains(err.Error(), "no openat2") {
		t.Fatalf("error = %v, want the missing openat2 reported", err)
	}
	if len(kept) != 1 || kept[0] != filepath.Join(kubelet, "pods") {
		t.Fatalf("kept = %v, want the unchecked directory", kept)
	}
	if _, err := os.Stat(filepath.Join(kubelet, "pods", "p1", "data")); err != nil {
		t.Fatalf("a directory that could not be checked was entered: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(kubelet, "top-file")); !os.IsNotExist(err) {
		t.Fatalf("plain files are still removed: err=%v", err)
	}
}

// TestClearArtifactNeverFollowsAnInnerSymlink: a link inside the tree that
// points elsewhere is removed as a link; its target survives.
func TestClearArtifactNeverFollowsAnInnerSymlink(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside")
	mkfile(t, filepath.Join(outside, "canary"), "survive")
	kubelet := filepath.Join(t.TempDir(), "kubelet")
	if err := os.MkdirAll(filepath.Join(kubelet, "pods"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(kubelet, "pods", "link")); err != nil {
		t.Fatal(err)
	}

	if _, err := clearArtifact(kubelet); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(kubelet, "pods", "link")); !os.IsNotExist(err) {
		t.Fatalf("the link survived: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "canary")); err != nil {
		t.Fatalf("the link's target was destroyed: %v", err)
	}
}

// TestClearArtifactNeverOpensAFIFO: removing a FIFO is an unlink, never an
// open, so a FIFO without a writer cannot hang the reset.
func TestClearArtifactNeverOpensAFIFO(t *testing.T) {
	kubelet := filepath.Join(t.TempDir(), "kubelet")
	if err := os.MkdirAll(kubelet, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(kubelet, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := clearArtifact(kubelet)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("clearArtifact blocked on a FIFO")
	}
	assertCleared(t, filepath.Dir(kubelet), "kubelet")
}

// TestClearArtifactDepthIsBounded: a tree deeper than the bound is left in
// place and reported, not recursed into without limit.
func TestClearArtifactDepthIsBounded(t *testing.T) {
	kubelet := filepath.Join(t.TempDir(), "kubelet")
	deep := kubelet
	for i := 0; i <= maxClearDepth+1; i++ {
		deep = filepath.Join(deep, "d")
	}
	mkfile(t, filepath.Join(deep, "f"), "x")

	_, err := clearArtifact(kubelet)
	if err == nil || !strings.Contains(err.Error(), "nested deeper") {
		t.Fatalf("error = %v, want the depth bound reported", err)
	}
}

// TestClearArtifactTopLevelIsKept: the artifact directory itself is emptied,
// not removed; on Kairos it is a mount point.
func TestClearArtifactTopLevelIsKept(t *testing.T) {
	kubelet := filepath.Join(t.TempDir(), "kubelet")
	mkfile(t, filepath.Join(kubelet, "config.yaml"), "x")
	if _, err := clearArtifact(kubelet); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fi, err := os.Stat(kubelet); err != nil || !fi.IsDir() {
		t.Fatalf("the artifact directory itself was removed: %v", err)
	}
	assertCleared(t, filepath.Dir(kubelet), "kubelet")
}

// TestResetReportsKeptMounts: Run turns kept mount points into an error that
// wraps ErrMountsKept, names them (at most maxReportedMounts), and tells the
// operator what to do, while still clearing everything else.
func TestResetReportsKeptMounts(t *testing.T) {
	root := t.TempDir()
	seedArtifacts(t, root)
	pods := filepath.Join(root, "var", "lib", "kubelet", "pods")
	var mounts []string
	for i := 0; i < maxReportedMounts+2; i++ {
		m := filepath.Join(pods, fmt.Sprintf("p%d", i), "volumes", "v", fmt.Sprintf("mnt%d", i))
		mkfile(t, filepath.Join(m, "data"), "x")
		mounts = append(mounts, m)
	}
	withHooks(t, func(name string) error {
		if strings.HasPrefix(name, "mnt") {
			return unix.EXDEV
		}
		return nil
	}, nil)

	err := Run(context.Background(), Options{Runner: &fakeRunner{}, RootPath: root})
	if !errors.Is(err, ErrMountsKept) {
		t.Fatalf("error = %v, want one wrapping ErrMountsKept", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, mounts[0]) || !strings.Contains(msg, "and 2 more") || !strings.Contains(msg, "unmount them") {
		t.Fatalf("error does not name the kept mounts and the remedy: %q", msg)
	}
	for _, m := range mounts {
		if _, err := os.Stat(filepath.Join(m, "data")); err != nil {
			t.Fatalf("data under a kept mount was removed: %v", err)
		}
	}
	for _, d := range []string{"etc/kubernetes", "var/lib/etcd"} {
		assertCleared(t, root, d)
	}
}

// TestResetReportsKeptMountsAlongsideAnotherFailure: when a mount is kept and
// something else fails too, both are reported; the kept mounts used to drop
// out of the error.
func TestResetReportsKeptMountsAlongsideAnotherFailure(t *testing.T) {
	root := t.TempDir()
	seedArtifacts(t, root)
	mnt := filepath.Join(root, "var", "lib", "kubelet", "mnt")
	mkfile(t, filepath.Join(mnt, "data"), "x")
	mkfile(t, filepath.Join(root, "var", "lib", "kubelet", "locked"), "x")
	withHooks(t, func(name string) error {
		if name == "mnt" {
			return unix.EXDEV
		}
		return nil
	}, func(name string) error {
		if name == "locked" {
			return unix.EACCES
		}
		return nil
	})

	err := Run(context.Background(), Options{Runner: &fakeRunner{}, RootPath: root})
	if !errors.Is(err, ErrMountsKept) {
		t.Fatalf("error = %v, want the kept mount reported", err)
	}
	if !strings.Contains(err.Error(), "locked") {
		t.Fatalf("error = %v, want the other failure reported too", err)
	}
	var kept *MountsKeptError
	if !errors.As(err, &kept) || len(kept.Paths) != 1 || kept.Paths[0] != mnt {
		t.Fatalf("kept paths = %+v, want exactly %s", kept, mnt)
	}
}

// TestEtcdMemberAdvisoryIsNeverSilent: on a stacked-etcd control plane the
// operator is always told something. kubeadm reset removes the member through
// the controlPlaneEndpoint and reports a failed removal only as a warning, so
// a healthy local apiserver and a clean exit mean "verify", never silence.
func TestEtcdMemberAdvisoryIsNeverSilent(t *testing.T) {
	const fullWarning = "stacked-etcd control-plane node and its etcd member may not have been removed"
	const verifyLine = "verify with `etcdctl member list`"
	cases := []struct {
		name       string
		reachable  bool
		kubeadmErr error
		want       string
		notWant    string
	}{
		{name: "apiserver up, kubeadm reset clean", reachable: true, want: verifyLine, notWant: fullWarning},
		{name: "apiserver up, kubeadm reset failed", reachable: true, kubeadmErr: context.DeadlineExceeded, want: fullWarning + " (kubeadm reset failed)", notWant: verifyLine},
		{name: "apiserver down", reachable: false, want: fullWarning + " (this node's apiserver did not answer)", notWant: verifyLine},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hook := captureLogs(t)
			root := t.TempDir()
			mkfile(t, filepath.Join(root, "etc", "kubernetes", "manifests", "etcd.yaml"), "kind: Pod\n")
			_ = Run(context.Background(), Options{
				Runner:                &fakeRunner{err: tc.kubeadmErr},
				RootPath:              root,
				NodeName:              "cp-1",
				ControlPlaneReachable: func(context.Context) bool { return tc.reachable },
			})
			var got string
			for _, e := range hook.AllEntries() {
				got += e.Message + "\n"
			}
			if !strings.Contains(got, tc.want) || strings.Contains(got, tc.notWant) {
				t.Fatalf("logs do not carry %q (and only that):\n%s", tc.want, got)
			}
		})
	}
}

// captureLogs installs a fresh logrus test hook on the standard logger and
// restores the previous hooks when the test ends.
func captureLogs(t *testing.T) *logrustest.Hook {
	t.Helper()
	logger := logrus.StandardLogger()
	saved := logger.ReplaceHooks(make(logrus.LevelHooks))
	t.Cleanup(func() { logger.ReplaceHooks(saved) })
	return logrustest.NewLocal(logger)
}

// TestClearArtifactReportsInSortedOrder: the walk visits entries in name order,
// so what it keeps, and the error and log that name it, read the same on every
// filesystem. Directory listing order is the filesystem's own: hash order on
// ext4, creation or reverse-creation order on tmpfs depending on the kernel.
func TestClearArtifactReportsInSortedOrder(t *testing.T) {
	kubelet := filepath.Join(t.TempDir(), "kubelet")
	var want []string
	for _, p := range []string{"p6", "p2", "p9", "p0", "p4", "p8", "p1", "p7", "p3", "p5"} {
		m := filepath.Join(kubelet, "pods", p, "mnt")
		mkfile(t, filepath.Join(m, "data"), "x")
		want = append(want, m)
	}
	sort.Strings(want)
	withHooks(t, func(name string) error {
		if name == "mnt" {
			return unix.EXDEV
		}
		return nil
	}, nil)

	kept, err := clearArtifact(kubelet)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(kept, want) {
		t.Fatalf("kept in order %v, want sorted %v", kept, want)
	}
}
