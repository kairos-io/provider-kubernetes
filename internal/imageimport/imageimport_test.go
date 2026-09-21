package imageimport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
)

// --- fakeBundle: a preparedBundle backed by real files on disk (so offset-0
// and fstat identity checks on the *os.File RunStdin receives are
// meaningful), with no filesystem-walk machinery at all. ---

type fakeBundle struct {
	lockData []byte
	dir      string
	readOnly bool

	mu     sync.Mutex
	opened []string // every name OpenTarball was ever called with
	closed bool
}

func (b *fakeBundle) LockData() []byte { return b.lockData }
func (b *fakeBundle) ReadOnly() bool   { return b.readOnly }
func (b *fakeBundle) Close()           { b.closed = true }

func (b *fakeBundle) OpenTarball(name string, minSize, maxSize int64, missingReason Reason) (*os.File, Reason, string) {
	b.mu.Lock()
	b.opened = append(b.opened, name)
	b.mu.Unlock()

	f, err := os.Open(filepath.Join(b.dir, name))
	if err != nil {
		return nil, missingReason, err.Error()
	}
	info, statErr := f.Stat()
	if statErr != nil {
		f.Close()
		return nil, ReasonNotRegular, statErr.Error()
	}
	if info.Size() < minSize || info.Size() > maxSize {
		f.Close()
		return nil, ReasonSize, fmt.Sprintf("%d bytes, want %d..%d", info.Size(), minSize, maxSize)
	}
	return f, "", ""
}

func (b *fakeBundle) Unlisted(lockedNames map[string]bool) []string {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.Name() == "images.lock" || lockedNames[e.Name()] {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

// --- fakeRunner: a StdinRunner that records argv and the identity (dev/ino)
// of the fd it received, and can be told to fail for specific tarball
// (base)names. ---

type fakeRunner struct {
	mu      sync.Mutex
	calls   []fakeRunCall
	failFor map[string]string // tarball basename -> stderr to fail with
}

type fakeRunCall struct {
	args   []string
	devIno [2]uint64
	name   string
}

func (r *fakeRunner) RunStdin(_ context.Context, stdin *os.File, args ...string) (kubeadm.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	dev, ino := fdIdentity(stdin)
	base := filepath.Base(stdin.Name())
	r.calls = append(r.calls, fakeRunCall{args: append([]string{}, args...), devIno: [2]uint64{dev, ino}, name: base})

	if stderr, fail := r.failFor[base]; fail {
		return kubeadm.Result{Stderr: stderr}, errors.New("ctr images import failed")
	}
	return kubeadm.Result{}, nil
}

// --- test infra: build a valid tar for a ref, capture logrus output ---

// tarBytesForRef builds a minimal, valid three-member docker-save tarball
// (config, one layer, manifest.json) naming ref, reusing tarcheck_test.go's
// buildTar/tarMember/sha256Hex helpers (same package).
func tarBytesForRef(t testing.TB, ref, layerSeed string) []byte {
	configContent := []byte("config-for-" + ref)
	configName := "sha256:" + sha256Hex(configContent)
	layerName := sha256Hex([]byte("layer-"+layerSeed)) + strings.Repeat("0", 0) // 64 hex chars already
	layerName = layerName[:64] + ".tar.gz"
	manifest := []byte(`[{"Config":"` + configName + `","RepoTags":["` + ref + `"],"Layers":["` + layerName + `"]}]`)
	return buildTar(t, []tarMember{
		{name: configName, content: configContent},
		{name: layerName, content: []byte("layer-blob-" + layerSeed)},
		{name: manifestName, content: manifest},
	})
}

func fdIdentity(f *os.File) (dev, ino uint64) {
	info, err := f.Stat()
	if err != nil {
		return 0, 0
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return uint64(st.Dev), st.Ino
}

// messageHook captures every logged entry's RAW Message (the string built by
// the logrus.Infof/Errorf/Warnf call, before any formatter's own field
// quoting/escaping) so tests assert on exactly what imageimport.go emits,
// independent of the configured formatter's on-disk presentation.
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

// captureLogs silences the standard logger's normal output (so test runs
// stay quiet) and returns a hook recording every raw message.
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

// removeHook drops h from the standard logger's hook set (logrus has no
// RemoveHook API, so tests must clean up the underlying map themselves to
// avoid leaking a hook -- and its captured messages -- into later tests).
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

// buildLockAndDir writes n entries' tarballs to a fresh temp dir and returns
// the canonical lock bytes plus the dir. If missingIdx >= 0, that entry's
// tarball file is NOT written (simulating a missing lock entry).
func buildLockAndDir(t *testing.T, refs []string, missingIdx int) (lockBytes []byte, dir string) {
	t.Helper()
	dir = t.TempDir()
	l := lock{
		KubernetesVersion: "v1.37.0",
		ImageRepository:   "registry.k8s.io",
		VerifiedBy:        lockVerifiedBy{Identity: "id", Issuer: "iss"},
	}
	for i, ref := range refs {
		tb := tarballForRef(ref)
		l.Images = append(l.Images, lockImage{
			Ref: ref, Digest: "sha256:" + sha256Hex([]byte(ref)), Tarball: tb, Verified: true, VerifyReason: "",
		})
		if i == missingIdx {
			continue
		}
		data := tarBytesForRef(t, ref, ref)
		if err := os.WriteFile(filepath.Join(dir, tb), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return renderLock(l), dir
}

func TestDoImportSuccessAllEntriesImported(t *testing.T) {
	refs := []string{"registry.k8s.io/pause:3.10.2", "registry.k8s.io/etcd:3.7.0-0"}
	lockBytes, dir := buildLockAndDir(t, refs, -1)
	fb := &fakeBundle{lockData: lockBytes, dir: dir}
	fr := &fakeRunner{}
	logs := captureLogs(t)

	res := doImport(context.Background(), fr, false, func() (preparedBundle, error) { return fb, nil })

	if res.Outcome != OutcomeSuccess || res.Entries != 2 || res.Imported != 2 || res.Refused != 0 || res.Failed != 0 {
		t.Fatalf("res = %+v", res)
	}
	if !fb.closed {
		t.Error("bundle was never closed")
	}
	if len(fr.calls) != 2 {
		t.Fatalf("want 2 RunStdin calls, got %d", len(fr.calls))
	}
	for _, c := range fr.calls {
		want := []string{"-n", "k8s.io", "images", "import", "-"}
		if len(c.args) != len(want) {
			t.Fatalf("argv = %v, want %v", c.args, want)
		}
		for i := range want {
			if c.args[i] != want[i] {
				t.Fatalf("argv = %v, want %v", c.args, want)
			}
		}
		// The fd identity must match the real on-disk tarball (offset-0,
		// same file -- never a path, never a copy).
		wantDev, wantIno := fileIdentity(t, filepath.Join(dir, c.name))
		if c.devIno[0] != wantDev || c.devIno[1] != wantIno {
			t.Fatalf("RunStdin fd identity (dev=%d ino=%d) != on-disk file (dev=%d ino=%d)",
				c.devIno[0], c.devIno[1], wantDev, wantIno)
		}
	}

	out := logs.joined()
	for _, want := range []string{
		"image-import: imported registry.k8s.io_pause_3.10.2.tar ref=registry.k8s.io/pause:3.10.2",
		"image-import: imported registry.k8s.io_etcd_3.7.0-0.tar ref=registry.k8s.io/etcd:3.7.0-0",
		"image-import: imported 2 tarball(s) from " + "/system/provider-kubernetes/images",
		"image-import: summary outcome=success entries=2 imported=2 refused=0 failed=0 unlisted=0 readonly=false dir=/system/provider-kubernetes/images",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q\n--- full log ---\n%s", want, out)
		}
	}
	assertSummaryIsLastLine(t, logs)
}

func TestDoImportMissingEntryFailsOnlyItself(t *testing.T) {
	refs := []string{"registry.k8s.io/pause:3.10.2", "registry.k8s.io/etcd:3.7.0-0", "registry.k8s.io/coredns/coredns:v1.14.6"}
	lockBytes, dir := buildLockAndDir(t, refs, 1) // etcd's tarball file is never written
	fb := &fakeBundle{lockData: lockBytes, dir: dir}
	fr := &fakeRunner{}
	logs := captureLogs(t)

	res := doImport(context.Background(), fr, false, func() (preparedBundle, error) { return fb, nil })

	if res.Entries != 3 || res.Imported != 2 || res.Refused != 1 || res.Failed != 0 {
		t.Fatalf("res = %+v", res)
	}
	if res.Outcome != OutcomePartial {
		t.Fatalf("outcome = %s, want partial", res.Outcome)
	}
	if len(fr.calls) != 2 {
		t.Fatalf("want 2 RunStdin calls (the missing entry must not reach the runner), got %d", len(fr.calls))
	}

	out := logs.joined()
	want := "image-import: refused registry.k8s.io_etcd_3.7.0-0.tar ref=registry.k8s.io/etcd:3.7.0-0 reason=missing"
	if !strings.Contains(out, want) {
		t.Errorf("log output missing %q\n--- full log ---\n%s", want, out)
	}
}

func TestDoImportInvalidLockImportsNothing(t *testing.T) {
	fb := &fakeBundle{lockData: []byte(`{"not":"a valid lock"}`)}
	fr := &fakeRunner{}
	logs := captureLogs(t)

	res := doImport(context.Background(), fr, false, func() (preparedBundle, error) { return fb, nil })

	if res.Outcome != OutcomeRefused || res.Entries != 0 || res.Imported != 0 {
		t.Fatalf("res = %+v", res)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("want 0 RunStdin calls for an invalid lock, got %d", len(fr.calls))
	}
	if !strings.Contains(logs.joined(), "reason=lock-invalid") {
		t.Errorf("log output missing lock-invalid reason:\n%s", logs.joined())
	}
}

func TestDoImportUnlistedFileNeverOpenedAndWarned(t *testing.T) {
	refs := []string{"registry.k8s.io/pause:3.10.2"}
	lockBytes, dir := buildLockAndDir(t, refs, -1)
	if err := os.WriteFile(filepath.Join(dir, "evil.tar"), []byte("not-checked"), 0o644); err != nil {
		t.Fatal(err)
	}
	fb := &fakeBundle{lockData: lockBytes, dir: dir}
	fr := &fakeRunner{}
	logs := captureLogs(t)

	res := doImport(context.Background(), fr, false, func() (preparedBundle, error) { return fb, nil })

	if res.Unlisted != 1 {
		t.Fatalf("Unlisted = %d, want 1", res.Unlisted)
	}
	for _, name := range fb.opened {
		if name == "evil.tar" {
			t.Fatal("evil.tar was opened -- unlisted files must never be opened")
		}
	}
	if !strings.Contains(logs.joined(), `image-import: unlisted "evil.tar" not imported`) {
		t.Errorf("log output missing the unlisted warning:\n%s", logs.joined())
	}
}

func TestDoImportVerifyOnlyNeverCallsRunner(t *testing.T) {
	refs := []string{"registry.k8s.io/pause:3.10.2", "registry.k8s.io/etcd:3.7.0-0"}
	lockBytes, dir := buildLockAndDir(t, refs, -1)
	fb := &fakeBundle{lockData: lockBytes, dir: dir}
	fr := &fakeRunner{}
	logs := captureLogs(t)

	res := doImport(context.Background(), fr, true, func() (preparedBundle, error) { return fb, nil })

	if len(fr.calls) != 0 {
		t.Fatalf("want 0 RunStdin calls in --verify-only mode, got %d", len(fr.calls))
	}
	if res.Outcome != OutcomeVerified || res.Imported != 0 {
		t.Fatalf("res = %+v", res)
	}
	if strings.Contains(logs.joined(), "image-import: imported") {
		t.Errorf("verify-only must never log an imported line:\n%s", logs.joined())
	}
}

func TestDoImportVerifyOnlyRefusedOnAnyRefusal(t *testing.T) {
	refs := []string{"registry.k8s.io/pause:3.10.2", "registry.k8s.io/etcd:3.7.0-0"}
	lockBytes, dir := buildLockAndDir(t, refs, 1)
	fb := &fakeBundle{lockData: lockBytes, dir: dir}
	fr := &fakeRunner{}
	captureLogs(t)

	res := doImport(context.Background(), fr, true, func() (preparedBundle, error) { return fb, nil })

	if res.Outcome != OutcomeRefused {
		t.Fatalf("outcome = %s, want refused", res.Outcome)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("want 0 RunStdin calls, got %d", len(fr.calls))
	}
}

// TestDoImportStructurallyInvalidTarballIsRefusedNotImported proves the import
// path itself runs the per-boot structural check (ADR-16-A2 O-6): a tarball that
// passes every file check but names another ref, or is not an image archive at
// all, is refused with its reason and never reaches ctr, while the other entries
// still import. Without this, dropping the CheckTar call in doImport would go
// unnoticed by the CheckTar unit tests.
func TestDoImportStructurallyInvalidTarballIsRefusedNotImported(t *testing.T) {
	for _, tc := range []struct {
		name       string
		content    func(t *testing.T) []byte
		wantReason Reason
	}{
		{
			name:       "manifest names another ref",
			content:    func(t *testing.T) []byte { return tarBytesForRef(t, "registry.k8s.io/pause:e2e-other", "pause") },
			wantReason: ReasonManifest,
		},
		{
			name:       "not an image archive",
			content:    func(*testing.T) []byte { return []byte(strings.Repeat("x", 4096)) },
			wantReason: ReasonTarStructure,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refs := []string{"registry.k8s.io/pause:3.10.2", "registry.k8s.io/etcd:3.7.0-0"}
			lockBytes, dir := buildLockAndDir(t, refs, -1)
			bad := tarballForRef(refs[0])
			if err := os.WriteFile(filepath.Join(dir, bad), tc.content(t), 0o644); err != nil {
				t.Fatal(err)
			}
			fb := &fakeBundle{lockData: lockBytes, dir: dir}
			fr := &fakeRunner{}
			logs := captureLogs(t)

			res := doImport(context.Background(), fr, false, func() (preparedBundle, error) { return fb, nil })

			if res.Outcome != OutcomePartial || res.Imported != 1 || res.Refused != 1 || res.Failed != 0 {
				t.Fatalf("res = %+v, want partial with 1 imported and 1 refused", res)
			}
			for _, c := range fr.calls {
				if c.name == bad {
					t.Fatalf("the invalid tarball %s was passed to ctr", bad)
				}
			}
			want := "image-import: refused " + bad + " ref=" + refs[0] + " reason=" + string(tc.wantReason) + " "
			if !strings.Contains(logs.joined(), want) {
				t.Errorf("log output missing %q\n--- full log ---\n%s", want, logs.joined())
			}
			assertSummaryIsLastLine(t, logs)
		})
	}
}

func TestDoImportCtrFailureIsSanitizedAndCapped(t *testing.T) {
	refs := []string{"registry.k8s.io/pause:3.10.2"}
	lockBytes, dir := buildLockAndDir(t, refs, -1)
	fb := &fakeBundle{lockData: lockBytes, dir: dir}
	fr := &fakeRunner{failFor: map[string]string{
		tarballForRef(refs[0]): "boom near token abcdef.0123456789abcdef",
	}}
	logs := captureLogs(t)

	res := doImport(context.Background(), fr, false, func() (preparedBundle, error) { return fb, nil })

	if res.Failed != 1 || res.Imported != 0 || res.Outcome != OutcomeFailed {
		t.Fatalf("res = %+v", res)
	}
	out := logs.joined()
	if strings.Contains(out, "abcdef.0123456789abcdef") {
		t.Fatalf("ctr stderr leaked a secret-shaped token:\n%s", out)
	}
	if !strings.Contains(out, "reason=ctr-failed") || !strings.Contains(out, "[REDACTED-TOKEN]") {
		t.Errorf("log output missing sanitized ctr-failed line:\n%s", out)
	}
}

func TestDoImportDeadlineMarksEntriesFailed(t *testing.T) {
	refs := []string{"registry.k8s.io/pause:3.10.2", "registry.k8s.io/etcd:3.7.0-0"}
	lockBytes, dir := buildLockAndDir(t, refs, -1)
	fb := &fakeBundle{lockData: lockBytes, dir: dir}
	fr := &fakeRunner{}
	logs := captureLogs(t)

	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	time.Sleep(time.Millisecond) // ensure the deadline has actually passed

	res := doImport(ctx, fr, false, func() (preparedBundle, error) { return fb, nil })

	if res.Failed != 2 || res.Imported != 0 || res.Refused != 0 {
		t.Fatalf("res = %+v", res)
	}
	if res.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %s, want failed", res.Outcome)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("want 0 RunStdin calls once the deadline has passed, got %d", len(fr.calls))
	}
	if !strings.Contains(logs.joined(), "reason=deadline") {
		t.Errorf("log output missing deadline reason:\n%s", logs.joined())
	}
}

func TestDoImportNotBundled(t *testing.T) {
	logs := captureLogs(t)
	res := doImport(context.Background(), &fakeRunner{}, false, func() (preparedBundle, error) { return nil, errNotBundled })
	if res.Outcome != OutcomeNotBundled {
		t.Fatalf("outcome = %s, want not-bundled", res.Outcome)
	}
	if !strings.Contains(logs.joined(), "image-import: summary outcome=not-bundled entries=0 imported=0 refused=0 failed=0 unlisted=0") {
		t.Errorf("log output missing the not-bundled summary:\n%s", logs.joined())
	}
}

func TestDoImportWholeBundleRefusal(t *testing.T) {
	logs := captureLogs(t)
	res := doImport(context.Background(), &fakeRunner{}, false, func() (preparedBundle, error) {
		return nil, &bundleError{ReasonDirUnsafe, "images dir is group-writable"}
	})
	if res.Outcome != OutcomeRefused || res.Entries != 0 {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(logs.joined(), "reason=dir-unsafe") {
		t.Errorf("log output missing the dir-unsafe reason:\n%s", logs.joined())
	}
}

// TestExitCode locks in decision 8/O-9's exit-code mapping.
func TestExitCode(t *testing.T) {
	cases := map[Outcome]int{
		OutcomeSuccess:    0,
		OutcomeVerified:   0,
		OutcomeNotBundled: 0,
		OutcomePartial:    1,
		OutcomeRefused:    1,
		OutcomeFailed:     1,
	}
	for outcome, want := range cases {
		if got := ExitCode(outcome); got != want {
			t.Errorf("ExitCode(%s) = %d, want %d", outcome, got, want)
		}
	}
}

// --- helpers ---

var summaryLineRE = regexp.MustCompile(`image-import: summary outcome=\S+ entries=\d+ imported=\d+ refused=\d+ failed=\d+ unlisted=\d+ readonly=(?:true|false) dir=\S+`)

// assertSummaryIsLastLine requires exactly one summary line, and that it is
// the last logged line (decision 8: "exactly ONE summary line, emitted
// LAST").
func assertSummaryIsLastLine(t *testing.T, logs *messageHook) {
	t.Helper()
	out := logs.joined()
	matches := summaryLineRE.FindAllString(out, -1)
	if len(matches) != 1 {
		t.Fatalf("want exactly 1 summary line, found %d:\n%s", len(matches), out)
	}
	if last := logs.last(); !strings.Contains(last, "image-import: summary") {
		t.Fatalf("last log line is not the summary:\n%s", last)
	}
}

func fileIdentity(t *testing.T, path string) (dev, ino uint64) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return fdIdentity(f)
}
