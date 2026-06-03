package status

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/kairos-io/provider-kubernetes/internal/reconcile"
	"github.com/kairos-io/provider-kubernetes/internal/reconcile/actualstate"
)

// testStatus returns a minimal Status suitable for write tests.
func testStatus() Status {
	return BuildStatus(BuildParams{
		Role:       actualstate.RoleWorker,
		Membership: actualstate.Uninitialized,
		LastAction: reconcile.ActionRunJoin,
		Err:        nil,
		Now:        "2026-06-03T12:00:00Z",
		BootID:     "test-boot-id",
		Version:    "v0.2.0-test",
	})
}

// tempFileSink creates a FileSink writing to two temp files under t.TempDir().
// Returns the sink and both paths so callers can inspect the written files.
func tempFileSink(t *testing.T) (*FileSink, string, string) {
	t.Helper()
	dir := t.TempDir()
	p1 := filepath.Join(dir, "run", "status.yaml")
	p2 := filepath.Join(dir, "log", "status.yaml")
	return &FileSink{Paths: []string{p1, p2}}, p1, p2
}

// TestFileSinkWritesBothPaths verifies that Record writes to every configured path.
func TestFileSinkWritesBothPaths(t *testing.T) {
	sink, p1, p2 := tempFileSink(t)
	sink.Record(context.Background(), testStatus())

	for _, p := range []string{p1, p2} {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("path %s not written: %v", p, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("path %s is empty", p)
		}
	}
}

// TestFileSinkYAMLIsValid verifies the written bytes are valid YAML that round-
// trips back to a Status with the expected fields.
func TestFileSinkYAMLIsValid(t *testing.T) {
	sink, p1, _ := tempFileSink(t)
	orig := testStatus()
	sink.Record(context.Background(), orig)

	data, err := os.ReadFile(p1)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}

	var got Status
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if got.APIVersion != APIVersion {
		t.Errorf("apiVersion = %q, want %q", got.APIVersion, APIVersion)
	}
	if got.Phase != orig.Phase {
		t.Errorf("phase = %q, want %q", got.Phase, orig.Phase)
	}
	if got.Version != orig.Version {
		t.Errorf("version = %q, want %q", got.Version, orig.Version)
	}
}

// TestFileSinkFileMode verifies the 0640 permission on the written file.
func TestFileSinkFileMode(t *testing.T) {
	sink, p1, _ := tempFileSink(t)
	sink.Record(context.Background(), testStatus())

	info, err := os.Stat(p1)
	if err != nil {
		t.Fatalf("stat %s: %v", p1, err)
	}
	// Mode must be exactly 0640 (we set it explicitly via Chmod).
	if info.Mode().Perm() != 0o640 {
		t.Errorf("file mode = %04o, want 0640", info.Mode().Perm())
	}
}

// TestFileSinkAtomicity verifies no partial/empty file is ever visible. We
// test this indirectly by checking that the target file either does not exist
// or has valid YAML content, never truncated bytes. We also check the temp
// file is cleaned up after a successful write.
func TestFileSinkAtomicity(t *testing.T) {
	sink, p1, _ := tempFileSink(t)
	sink.Record(context.Background(), testStatus())

	// The temp file (.status-*.yaml.tmp) must be gone after a successful write.
	dir := filepath.Dir(p1)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("temp file %s was not cleaned up after successful write", e.Name())
		}
	}

	// The target file must be valid YAML (not truncated).
	data, _ := os.ReadFile(p1)
	var s Status
	if err := yaml.Unmarshal(data, &s); err != nil {
		t.Errorf("written file is not valid YAML: %v", err)
	}
}

// TestFileSinkSwallowsWriteError verifies that a write error (e.g. path in a
// non-writable dir) is swallowed and does not cause Record to panic or return.
func TestFileSinkSwallowsWriteError(t *testing.T) {
	// Use a path that cannot be created (parent is a file, not a dir).
	dir := t.TempDir()
	blocker := filepath.Join(dir, "notadir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	badPath := filepath.Join(blocker, "status.yaml")

	sink := &FileSink{Paths: []string{badPath}}
	// Must not panic.
	sink.Record(context.Background(), testStatus())
}

// TestFileSinkParentDirCreated verifies that Record creates missing parent dirs.
func TestFileSinkParentDirCreated(t *testing.T) {
	base := t.TempDir()
	deep := filepath.Join(base, "a", "b", "c", "status.yaml")
	sink := &FileSink{Paths: []string{deep}}
	sink.Record(context.Background(), testStatus())

	if _, err := os.Stat(deep); err != nil {
		t.Errorf("deep path not created: %v", err)
	}
}

// TestMultiSinkFansOut verifies all sinks in a MultiSink receive the call.
type countSink struct{ count int }

func (c *countSink) Record(_ context.Context, _ Status) { c.count++ }

func TestMultiSinkFansOut(t *testing.T) {
	a, b := &countSink{}, &countSink{}
	m := MultiSink{a, b}
	m.Record(context.Background(), testStatus())
	if a.count != 1 {
		t.Errorf("sink a count = %d, want 1", a.count)
	}
	if b.count != 1 {
		t.Errorf("sink b count = %d, want 1", b.count)
	}
}

// TestMultiSinkIsolatesPanics verifies a panic in one sink does not prevent others.
type panicSink struct{}

func (panicSink) Record(_ context.Context, _ Status) { panic("injected panic") }

func TestMultiSinkIsolatesPanics(t *testing.T) {
	good := &countSink{}
	m := MultiSink{panicSink{}, good}
	// Must not propagate the panic.
	m.Record(context.Background(), testStatus())
	if good.count != 1 {
		t.Errorf("good sink count = %d after panic in first sink, want 1", good.count)
	}
}

// TestMultiSinkNilSinkSkipped verifies nil entries in MultiSink are silently skipped.
func TestMultiSinkNilSinkSkipped(t *testing.T) {
	good := &countSink{}
	m := MultiSink{nil, good, nil}
	m.Record(context.Background(), testStatus())
	if good.count != 1 {
		t.Errorf("good sink count = %d, want 1", good.count)
	}
}

// TestFileSinkCanceledContextSwallowed verifies that a canceled context
// causes the write to be swallowed, not panicked.
func TestFileSinkCanceledContextSwallowed(t *testing.T) {
	sink, _, _ := tempFileSink(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately canceled
	// Must not panic, may or may not write (race between deadline and write).
	sink.Record(ctx, testStatus())
}
