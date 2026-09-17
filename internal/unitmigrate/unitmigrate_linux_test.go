//go:build linux

package unitmigrate

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
)

// --- test infra: seams, tree builder, fake runner, log capture ---
// (mirrors internal/imageimport/bundle_linux_test.go's seam pattern and
// internal/imageimport/imageimport_test.go's messageHook/captureLogs, which
// are unexported in that package and so cannot be imported directly.)

// setSeams points rootPath/expectedOwnerUID at root/uid and restores the
// previous values on cleanup. These tests never run in parallel with each
// other (no t.Parallel()): the seams are package-global.
func setSeams(t *testing.T, root string, uid int) {
	t.Helper()
	oldRoot, oldUID := rootPath, expectedOwnerUID
	rootPath, expectedOwnerUID = root, uid
	t.Cleanup(func() { rootPath, expectedOwnerUID = oldRoot, oldUID })
}

// tree builds a scope tree under a fresh temp directory and points rootPath/
// expectedOwnerUID at it.
type tree struct {
	root      string
	systemDir string
}

func newTree(t *testing.T) *tree {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatalf("chmod root: %v", err)
	}
	tr := &tree{root: root, systemDir: filepath.Join(root, "etc", "systemd", "system")}
	setSeams(t, root, os.Getuid())
	return tr
}

func (tr *tree) mkSystemDir(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(tr.systemDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", tr.systemDir, err)
	}
}

func (tr *tree) writeFragment(t *testing.T, name string, data []byte, mode os.FileMode) {
	t.Helper()
	tr.mkSystemDir(t)
	if err := os.WriteFile(filepath.Join(tr.systemDir, name), data, mode); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func (tr *tree) dropinDir() string { return filepath.Join(tr.systemDir, nameDropinDir) }
func (tr *tree) wantsDir() string  { return filepath.Join(tr.systemDir, nameWantsDir) }

func (tr *tree) writeDropin(t *testing.T, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(tr.dropinDir(), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", tr.dropinDir(), err)
	}
	if err := os.WriteFile(filepath.Join(tr.dropinDir(), nameDropin), data, mode); err != nil {
		t.Fatalf("write drop-in: %v", err)
	}
}

func (tr *tree) writeLink(t *testing.T, name, target string) {
	t.Helper()
	if err := os.MkdirAll(tr.wantsDir(), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", tr.wantsDir(), err)
	}
	if err := os.Symlink(target, filepath.Join(tr.wantsDir(), name)); err != nil {
		t.Fatalf("symlink %s: %v", name, err)
	}
}

// readFixture reads one of testdata/'s historical byte-for-byte blobs.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// buildFull writes every scope entry as a byte-identical, correctly linked
// copy, deliberately varying each file's mode (S19-7: mode is never checked).
func (tr *tree) buildFull(t *testing.T) {
	t.Helper()
	tr.writeFragment(t, nameContainerd, readFixture(t, "containerd.service"), 0o644)
	tr.writeFragment(t, nameKubelet, readFixture(t, "kubelet.service"), 0o664)
	tr.writeFragment(t, nameImport, readFixture(t, "provider-kubernetes-image-import.service.1630"), 0o600)
	tr.writeDropin(t, readFixture(t, "10-kubeadm.conf"), 0o666)
	for _, name := range fragmentNames {
		tr.writeLink(t, name, wantsLinkTarget(name))
	}
}

// replaceWithSymlink removes whatever is at path (file or directory) and
// replaces it with a symlink to a fresh, unrelated directory elsewhere.
func replaceWithSymlink(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Mkdir(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, path); err != nil {
		t.Fatalf("symlink %s: %v", path, err)
	}
}

// fakeRunner is a hardware-free kubeadm.Runner: it records every call's argv
// and, if failNext is set, returns an error instead of running anything real.
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	fail  bool
}

func (r *fakeRunner) Run(_ context.Context, args ...string) (kubeadm.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := append([]string(nil), args...)
	r.calls = append(r.calls, cp)
	if r.fail {
		return kubeadm.Result{}, errors.New("simulated systemctl failure")
	}
	return kubeadm.Result{}, nil
}

func (r *fakeRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *fakeRunner) lastCall() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return nil
	}
	return r.calls[len(r.calls)-1]
}

// messageHook captures every logged entry's raw Message (mirrors
// internal/imageimport/imageimport_test.go's helper of the same name).
type messageHook struct {
	mu       sync.Mutex
	messages []string
}

func (h *messageHook) Levels() []logrus.Level { return logrus.AllLevels }
func (h *messageHook) Fire(e *logrus.Entry) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, e.Message)
	return nil
}

func (h *messageHook) joined() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.messages, "\n")
}

func (h *messageHook) last() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.messages) == 0 {
		return ""
	}
	return h.messages[len(h.messages)-1]
}

func captureLogs(t *testing.T) *messageHook {
	t.Helper()
	old := logrus.StandardLogger().Out
	logrus.SetOutput(io.Discard)
	t.Cleanup(func() { logrus.SetOutput(old) })

	h := &messageHook{}
	logrus.AddHook(h)
	t.Cleanup(func() { removeHook(h) })
	return h
}

func removeHook(h *messageHook) {
	hooks := logrus.StandardLogger().Hooks
	for level, hs := range hooks {
		filtered := hs[:0]
		for _, existing := range hs {
			if existing != logrus.Hook(h) {
				filtered = append(filtered, existing)
			}
		}
		hooks[level] = filtered
	}
}

// runBounded runs Migrate on a background goroutine and fails the test if it
// does not return within 5s -- proving a planted FIFO or an expired deadline
// never hangs the boot path (#4099-1).
func runBounded(t *testing.T, ctx context.Context, runner kubeadm.Runner) Result {
	t.Helper()
	done := make(chan Result, 1)
	go func() { done <- Migrate(ctx, runner) }()
	select {
	case res := <-done:
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("Migrate did not return within the test deadline")
		return Result{}
	}
}

// --- tests ---

func TestMigrateAbsentTreeIsClean(t *testing.T) {
	newTree(t) // seams point at an empty temp root; nothing under etc/
	r := &fakeRunner{}
	logs := captureLogs(t)

	res := runBounded(t, context.Background(), r)

	if res.Outcome != OutcomeClean || res.Removed != 0 || res.Kept != 0 || res.Reloaded {
		t.Fatalf("res = %+v, want clean/0/0/false", res)
	}
	if r.callCount() != 0 {
		t.Errorf("systemctl called %d times, want 0", r.callCount())
	}
	if !strings.HasPrefix(logs.last(), "unit-migrate: summary outcome=clean") {
		t.Errorf("last log line = %q", logs.last())
	}
}

func TestMigrateRemovesFrozenBlobsRegardlessOfMode(t *testing.T) {
	tr := newTree(t)
	tr.buildFull(t)
	r := &fakeRunner{}

	res := runBounded(t, context.Background(), r)

	if res.Outcome != OutcomeMigrated {
		t.Fatalf("Outcome = %s, want migrated", res.Outcome)
	}
	if res.Removed != 7 || res.Kept != 0 {
		t.Fatalf("Removed=%d Kept=%d, want 7/0", res.Removed, res.Kept)
	}
	if !res.Reloaded {
		t.Error("Reloaded = false, want true")
	}
	if ExitCode(res.Outcome) != 0 {
		t.Errorf("ExitCode = %d, want 0", ExitCode(res.Outcome))
	}

	for _, name := range fragmentNames {
		assertAbsent(t, filepath.Join(tr.systemDir, name))
	}
	assertAbsent(t, filepath.Join(tr.dropinDir(), nameDropin))
	assertAbsent(t, tr.dropinDir()) // now-empty dir must be rmdir'd
	for _, name := range fragmentNames {
		assertAbsent(t, filepath.Join(tr.wantsDir(), name))
	}
}

func TestMigrateKeepsModifiedFragmentAndNeverLogsItsContent(t *testing.T) {
	tr := newTree(t)
	tr.buildFull(t)
	const marker = "SECRET-PROXY-TOKEN-DO-NOT-LOG"
	modified := append(readFixture(t, "kubelet.service"), []byte("\n# "+marker+"\n")...)
	if err := os.WriteFile(filepath.Join(tr.systemDir, nameKubelet), modified, 0o644); err != nil {
		t.Fatal(err)
	}
	r := &fakeRunner{}
	logs := captureLogs(t)

	res := runBounded(t, context.Background(), r)

	if res.Outcome != OutcomeKeptModified {
		t.Fatalf("Outcome = %s, want kept-modified", res.Outcome)
	}
	if res.Kept != 1 || res.Removed != 6 {
		t.Fatalf("Kept=%d Removed=%d, want 1/6", res.Kept, res.Removed)
	}
	if ExitCode(res.Outcome) != 1 {
		t.Errorf("ExitCode = %d, want 1", ExitCode(res.Outcome))
	}
	assertPresent(t, filepath.Join(tr.systemDir, nameKubelet))

	wantLine := `unit-migrate: kept "/etc/systemd/system/kubelet.service" reason=modified`
	if !strings.Contains(logs.joined(), wantLine) {
		t.Errorf("logs = %q, want to contain %q", logs.joined(), wantLine)
	}
	if strings.Contains(logs.joined(), marker) {
		t.Fatalf("logs leaked file content: %q", logs.joined())
	}
	if strings.Contains(logs.joined(), "size=") || strings.Contains(logs.joined(), "hash=") {
		t.Errorf("logs must never report size or hash of a non-matching file: %q", logs.joined())
	}
}

func TestMigrateSymlinkAtFragmentPathLeftSilentlyAndTargetUntouched(t *testing.T) {
	tr := newTree(t)
	tr.buildFull(t)

	targetFile := filepath.Join(t.TempDir(), "real-content")
	targetBytes := []byte("this file must never be touched")
	if err := os.WriteFile(targetFile, targetBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	kubeletPath := filepath.Join(tr.systemDir, nameKubelet)
	if err := os.Remove(kubeletPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetFile, kubeletPath); err != nil {
		t.Fatal(err)
	}

	r := &fakeRunner{}
	logs := captureLogs(t)
	res := runBounded(t, context.Background(), r)

	// The mask is silent and does not count as kept; the other 6 entries are
	// byte-identical and still removed cleanly.
	if res.Outcome != OutcomeMigrated || res.Kept != 0 || res.Removed != 6 {
		t.Fatalf("res = %+v, want migrated/kept=0/removed=6", res)
	}

	fi, err := os.Lstat(kubeletPath)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("kubelet.service symlink was disturbed: fi=%v err=%v", fi, err)
	}
	gotTarget, err := os.Readlink(kubeletPath)
	if err != nil || gotTarget != targetFile {
		t.Fatalf("symlink target changed: got %q err %v, want %q", gotTarget, err, targetFile)
	}
	gotBytes, err := os.ReadFile(targetFile)
	if err != nil || string(gotBytes) != string(targetBytes) {
		t.Fatalf("symlink target content changed: %q, err %v", gotBytes, err)
	}
	// The deletion walk itself must never log a "kept" line for the mask...
	if strings.Contains(logs.joined(), `kept "`+pathKubeletFragment+`"`) {
		t.Errorf("a symlink at a fragment path must not produce a kept log line, got: %q", logs.joined())
	}
	// ...but the separate, independent override report is expected to name it
	// (it legitimately still shadows the image unit's enablement at that path).
	if res.Overrides != 1 {
		t.Errorf("Overrides = %d, want 1 (the mask itself, reported not acted on)", res.Overrides)
	}
	if !strings.Contains(logs.joined(), `unit-migrate: override "`+pathKubeletFragment+`"`) {
		t.Errorf("logs = %q, want an override line for the mask", logs.joined())
	}
}

// TestMigrateSymlinkToFrozenContentIsNeverFollowed is the adversarial version
// of the mask test above: the symlink's target is BYTE-IDENTICAL to a frozen
// blob (so a buggy implementation that stats/opens through the symlink -- as
// if it had used stat/os.Remove instead of lstat/unlinkat -- would compute a
// matching hash and delete the target out from under the operator). It proves
// the AT_SYMLINK_NOFOLLOW check on the fragment path itself, not just the
// content mismatch in the test above, is what keeps the target safe.
func TestMigrateSymlinkToFrozenContentIsNeverFollowed(t *testing.T) {
	tr := newTree(t)
	tr.buildFull(t)

	// The target is a real, root-of-its-own file that happens to hold exactly
	// kubelet.service's frozen bytes (the fragment name being checked here) --
	// plausible if an operator keeps a reference copy elsewhere, or simply by
	// coincidence.
	targetFile := filepath.Join(t.TempDir(), "operators-own-file")
	frozenBytes := readFixture(t, "kubelet.service")
	if err := os.WriteFile(targetFile, frozenBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	kubeletPath := filepath.Join(tr.systemDir, nameKubelet)
	if err := os.Remove(kubeletPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetFile, kubeletPath); err != nil {
		t.Fatal(err)
	}

	res := runBounded(t, context.Background(), &fakeRunner{})

	if res.Outcome != OutcomeMigrated || res.Kept != 0 || res.Removed != 6 {
		t.Fatalf("res = %+v, want migrated/kept=0/removed=6 (the symlink itself is never a removal candidate)", res)
	}
	if _, err := os.Lstat(kubeletPath); err != nil {
		t.Fatalf("the symlink at the fragment path was removed: %v", err)
	}
	gotBytes, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("the symlink's target file was deleted: %v", err)
	}
	if string(gotBytes) != string(frozenBytes) {
		t.Fatalf("the symlink's target file content changed: %q", gotBytes)
	}
}

func TestMigrateDirectoryAtFragmentPathKeptWithoutHanging(t *testing.T) {
	tr := newTree(t)
	tr.buildFull(t)
	p := filepath.Join(tr.systemDir, nameContainerd)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}

	res := runBounded(t, context.Background(), &fakeRunner{})

	if res.Outcome != OutcomeKeptModified || res.Kept != 1 {
		t.Fatalf("res = %+v, want kept-modified/kept=1", res)
	}
	assertPresent(t, p)
}

// TestMigrateFIFOAtFragmentPathKeptWithoutHanging swaps a FIFO in AFTER the
// tree is built (S19-7's "FIFO swapped in after lstat" case is really about
// proving O_NONBLOCK: no writer will ever open the other end, and the open
// must still return immediately instead of blocking the boot path).
func TestMigrateFIFOAtFragmentPathKeptWithoutHanging(t *testing.T) {
	tr := newTree(t)
	tr.buildFull(t)
	p := filepath.Join(tr.systemDir, nameContainerd)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Fatal(err)
	}

	res := runBounded(t, context.Background(), &fakeRunner{})

	if res.Outcome != OutcomeKeptModified || res.Kept != 1 {
		t.Fatalf("res = %+v, want kept-modified/kept=1", res)
	}
	assertPresent(t, p)
}

func TestMigrateOversizeFragmentKept(t *testing.T) {
	tr := newTree(t)
	tr.buildFull(t)
	big := make([]byte, maxUnitSize+100)
	if err := os.WriteFile(filepath.Join(tr.systemDir, nameKubelet), big, 0o644); err != nil {
		t.Fatal(err)
	}

	res := runBounded(t, context.Background(), &fakeRunner{})

	if res.Outcome != OutcomeKeptModified || res.Kept != 1 {
		t.Fatalf("res = %+v, want kept-modified/kept=1", res)
	}
	assertPresent(t, filepath.Join(tr.systemDir, nameKubelet))
}

// TestMigrateFragmentSwappedBetweenHashAndUnlinkIsKeptAsRace closes the first
// proof gap the 2026-09-17 security review found: nothing proved that the
// immediate pre-unlink dev/ino re-check actually keeps a file that changed
// between the hash and the unlink. It uses the testHookBeforeUnlink seam to
// swap kubelet.service's content (and therefore its inode) at exactly that
// window, deterministically, without a second real process.
func TestMigrateFragmentSwappedBetweenHashAndUnlinkIsKeptAsRace(t *testing.T) {
	tr := newTree(t)
	tr.buildFull(t)

	kubeletPath := filepath.Join(tr.systemDir, nameKubelet)
	swapped := []byte("swapped in the race window, after the hash matched")
	t.Cleanup(func() { testHookBeforeUnlink = nil })
	testHookBeforeUnlink = func(name string) {
		if name != nameKubelet {
			return
		}
		if err := os.Remove(kubeletPath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(kubeletPath, swapped, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	logs := captureLogs(t)
	res := runBounded(t, context.Background(), &fakeRunner{})

	if res.Outcome != OutcomeKeptModified || res.Kept != 1 || res.Removed != 6 {
		t.Fatalf("res = %+v, want kept-modified/kept=1/removed=6", res)
	}
	wantLine := `unit-migrate: kept "` + pathKubeletFragment + `" reason=race`
	if !strings.Contains(logs.joined(), wantLine) {
		t.Errorf("logs = %q, want to contain %q", logs.joined(), wantLine)
	}
	got, err := os.ReadFile(kubeletPath)
	if err != nil || string(got) != string(swapped) {
		t.Fatalf("the swapped-in file was disturbed: %q, err %v", got, err)
	}
}

// TestMigrateFragmentGrowsAfterFstatIsCaughtByBoundedRead closes the second
// proof gap: TestMigrateOversizeFragmentKept only trips the st_size gate on
// the FIRST fstatat, before the file is even opened. This test starts from a
// small, byte-identical kubelet.service (so every check up to and including
// fstat(fd) passes) and uses the testHookBeforeRead seam to append data
// past maxUnitSize at exactly the window between that fstat and the bounded
// read, proving the read bound -- not st_size -- is what catches growth.
func TestMigrateFragmentGrowsAfterFstatIsCaughtByBoundedRead(t *testing.T) {
	tr := newTree(t)
	tr.buildFull(t)

	kubeletPath := filepath.Join(tr.systemDir, nameKubelet)
	t.Cleanup(func() { testHookBeforeRead = nil })
	testHookBeforeRead = func(name string) {
		if name != nameKubelet {
			return
		}
		f, err := os.OpenFile(kubeletPath, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		if _, err := f.Write(make([]byte, maxUnitSize)); err != nil {
			t.Fatal(err)
		}
	}

	logs := captureLogs(t)
	res := runBounded(t, context.Background(), &fakeRunner{})

	if res.Outcome != OutcomeKeptModified || res.Kept != 1 || res.Removed != 6 {
		t.Fatalf("res = %+v, want kept-modified/kept=1/removed=6", res)
	}
	wantLine := `unit-migrate: kept "` + pathKubeletFragment + `" reason=size`
	if !strings.Contains(logs.joined(), wantLine) {
		t.Errorf("logs = %q, want to contain %q", logs.joined(), wantLine)
	}
	assertPresent(t, kubeletPath)
}

func TestMigrateSymlinkedParentComponentSkipsOnlyThatSubtree(t *testing.T) {
	t.Run("kubelet.service.d", func(t *testing.T) {
		tr := newTree(t)
		tr.buildFull(t)
		replaceWithSymlink(t, tr.dropinDir())

		logs := captureLogs(t)
		res := runBounded(t, context.Background(), &fakeRunner{})

		if res.Removed != 6 || res.Kept != 1 || res.Outcome != OutcomeFailed {
			t.Fatalf("res = %+v, want removed=6/kept=1/failed", res)
		}
		if !strings.Contains(logs.joined(), `unit-migrate: kept "`+pathKubeadmDropin+`" reason=walk-unsafe`) {
			t.Errorf("logs = %q", logs.joined())
		}
		// The 6 unaffected entries (3 fragments + 3 links) still processed normally.
		for _, name := range fragmentNames {
			assertAbsent(t, filepath.Join(tr.systemDir, name))
		}
	})

	t.Run("multi-user.target.wants", func(t *testing.T) {
		tr := newTree(t)
		tr.buildFull(t)
		replaceWithSymlink(t, tr.wantsDir())

		res := runBounded(t, context.Background(), &fakeRunner{})

		if res.Removed != 4 || res.Kept != 3 || res.Outcome != OutcomeFailed {
			t.Fatalf("res = %+v, want removed=4/kept=3/failed", res)
		}
		for _, name := range fragmentNames {
			assertAbsent(t, filepath.Join(tr.systemDir, name))
		}
	})

	t.Run("etc/systemd/system itself", func(t *testing.T) {
		tr := newTree(t)
		tr.buildFull(t)
		replaceWithSymlink(t, tr.systemDir)

		res := runBounded(t, context.Background(), &fakeRunner{})

		if res.Removed != 0 || res.Kept != 7 || res.Outcome != OutcomeFailed {
			t.Fatalf("res = %+v, want removed=0/kept=7/failed", res)
		}
	})
}

func TestMigrateLinkWrongTargetKeptOthersRemoved(t *testing.T) {
	tr := newTree(t)
	tr.buildFull(t)
	kubeletLinkFile := filepath.Join(tr.wantsDir(), nameKubelet)
	if err := os.Remove(kubeletLinkFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/systemd/system/some-other.service", kubeletLinkFile); err != nil {
		t.Fatal(err)
	}

	logs := captureLogs(t)
	res := runBounded(t, context.Background(), &fakeRunner{})

	if res.Removed != 6 || res.Kept != 1 || res.Outcome != OutcomeKeptModified {
		t.Fatalf("res = %+v, want removed=6/kept=1/kept-modified", res)
	}
	wantLine := `unit-migrate: kept "` + linkPath(nameKubelet) + `" reason=link-target`
	if !strings.Contains(logs.joined(), wantLine) {
		t.Errorf("logs = %q, want to contain %q", logs.joined(), wantLine)
	}
	got, err := os.Readlink(kubeletLinkFile)
	if err != nil || got != "/etc/systemd/system/some-other.service" {
		t.Fatalf("wrong-target link was disturbed: got %q err %v", got, err)
	}
}

func TestMigrateRmdirOnlyWhenDropinDirEmpty(t *testing.T) {
	tr := newTree(t)
	tr.buildFull(t)
	operatorFile := filepath.Join(tr.dropinDir(), "20-operator.conf")
	if err := os.WriteFile(operatorFile, []byte("[Service]\nEnvironment=HTTPS_PROXY=http://op:secret@proxy:3128\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := runBounded(t, context.Background(), &fakeRunner{})

	if res.Removed != 7 {
		t.Fatalf("Removed = %d, want 7 (the drop-in file itself is still removed)", res.Removed)
	}
	assertAbsent(t, filepath.Join(tr.dropinDir(), nameDropin))
	assertPresent(t, tr.dropinDir()) // ENOTEMPTY: the directory must survive
	gotOperator, err := os.ReadFile(operatorFile)
	if err != nil || !strings.Contains(string(gotOperator), "proxy:3128") {
		t.Fatalf("operator drop-in was disturbed: %q, err %v", gotOperator, err)
	}
}

func TestMigrateReloadCalledOnlyWhenFragmentOrDropinRemoved(t *testing.T) {
	t.Run("links only: no reload", func(t *testing.T) {
		tr := newTree(t)
		// Only the three (dangling) links exist; no fragments, no drop-in.
		for _, name := range fragmentNames {
			tr.writeLink(t, name, wantsLinkTarget(name))
		}
		r := &fakeRunner{}

		res := runBounded(t, context.Background(), r)

		if res.Removed != 3 || res.Outcome != OutcomeMigrated {
			t.Fatalf("res = %+v, want removed=3/migrated", res)
		}
		if res.Reloaded {
			t.Error("Reloaded = true, want false (only links were removed)")
		}
		if r.callCount() != 0 {
			t.Errorf("systemctl called %d times, want 0", r.callCount())
		}
	})

	t.Run("fragment removed: reload with exact argv", func(t *testing.T) {
		tr := newTree(t)
		tr.writeFragment(t, nameContainerd, readFixture(t, "containerd.service"), 0o644)
		r := &fakeRunner{}

		res := runBounded(t, context.Background(), r)

		if res.Removed != 1 || !res.Reloaded {
			t.Fatalf("res = %+v, want removed=1/reloaded=true", res)
		}
		if r.callCount() != 1 {
			t.Fatalf("systemctl called %d times, want 1", r.callCount())
		}
		if got := r.lastCall(); len(got) != 1 || got[0] != "daemon-reload" {
			t.Errorf("argv = %v, want [daemon-reload]", got)
		}
	})

	t.Run("drop-in removed: reload", func(t *testing.T) {
		tr := newTree(t)
		tr.writeDropin(t, readFixture(t, "10-kubeadm.conf"), 0o644)
		r := &fakeRunner{}

		res := runBounded(t, context.Background(), r)

		if res.Removed != 1 || !res.Reloaded {
			t.Fatalf("res = %+v, want removed=1/reloaded=true", res)
		}
		if r.callCount() != 1 {
			t.Errorf("systemctl called %d times, want 1", r.callCount())
		}
	})
}

func TestMigrateReloadFailureFailsButKeepsRemovals(t *testing.T) {
	tr := newTree(t)
	tr.buildFull(t)
	r := &fakeRunner{fail: true}

	res := runBounded(t, context.Background(), r)

	if res.Outcome != OutcomeFailed {
		t.Fatalf("Outcome = %s, want failed", res.Outcome)
	}
	if ExitCode(res.Outcome) != 1 {
		t.Errorf("ExitCode = %d, want 1", ExitCode(res.Outcome))
	}
	if res.Removed != 7 {
		t.Errorf("Removed = %d, want 7 (removals already done stay done)", res.Removed)
	}
	for _, name := range fragmentNames {
		assertAbsent(t, filepath.Join(tr.systemDir, name))
	}
}

func TestMigrateSecondRunIsCleanWithNoReload(t *testing.T) {
	newTreeAndMigrateTwice(t)
}

func newTreeAndMigrateTwice(t *testing.T) {
	t.Helper()
	tr := newTree(t)
	tr.buildFull(t)
	r := &fakeRunner{}

	first := runBounded(t, context.Background(), r)
	if first.Outcome != OutcomeMigrated || first.Removed != 7 {
		t.Fatalf("first run = %+v, want migrated/removed=7", first)
	}
	if r.callCount() != 1 {
		t.Fatalf("first run called systemctl %d times, want 1", r.callCount())
	}

	second := runBounded(t, context.Background(), r)
	if second.Outcome != OutcomeClean || second.Removed != 0 || second.Kept != 0 || second.Reloaded {
		t.Fatalf("second run = %+v, want clean/0/0/false", second)
	}
	if r.callCount() != 1 {
		t.Errorf("second run must not reload again: systemctl called %d times total, want 1", r.callCount())
	}
	_ = tr
}

func TestMigrateDeadlineHonored(t *testing.T) {
	tr := newTree(t)
	tr.buildFull(t)

	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-ctx.Done() // guarantee the deadline has already passed

	res := runBounded(t, ctx, &fakeRunner{})

	if res.Outcome != OutcomeFailed {
		t.Fatalf("Outcome = %s, want failed", res.Outcome)
	}
	// Nothing was touched: the deadline is checked before the walk starts.
	for _, name := range fragmentNames {
		assertPresent(t, filepath.Join(tr.systemDir, name))
	}
}

func TestMigrateSummaryIsLastLine(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T, tr *tree)
		run   *fakeRunner
	}{
		{"clean", func(t *testing.T, tr *tree) {}, &fakeRunner{}},
		{"migrated", func(t *testing.T, tr *tree) { tr.buildFull(t) }, &fakeRunner{}},
		{"kept-modified", func(t *testing.T, tr *tree) {
			tr.buildFull(t)
			if err := os.WriteFile(filepath.Join(tr.systemDir, nameKubelet), []byte("edited"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, &fakeRunner{}},
		{"failed (reload error)", func(t *testing.T, tr *tree) { tr.buildFull(t) }, &fakeRunner{fail: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := newTree(t)
			c.build(t, tr)
			logs := captureLogs(t)

			runBounded(t, context.Background(), c.run)

			if !strings.HasPrefix(logs.last(), "unit-migrate: summary outcome=") {
				t.Fatalf("last log line = %q, want it to start with the summary", logs.last())
			}
		})
	}
}

// --- small filesystem assertion helpers ---

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Errorf("%s still exists, want removed", path)
	} else if !os.IsNotExist(err) {
		t.Errorf("lstat %s: unexpected error %v", path, err)
	}
}

func assertPresent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("lstat %s: %v, want it to still exist", path, err)
	}
}
