package securefile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteTransientCreates0600File(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteTransient(dir, "secret: value\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path == "" {
		t.Fatal("expected a non-empty path")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600, got %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(data) != "secret: value\n" {
		t.Fatalf("unexpected content: %q", string(data))
	}
}

func TestWriteTransientNameMatchesPattern(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteTransient(dir, "x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "kubeadm-") || !strings.HasSuffix(base, ".yaml") {
		t.Fatalf("file name %q does not match kubeadm-*.yaml pattern", base)
	}
}

func TestShredRemovesFile(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteTransient(dir, "secret")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	Shred(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected file removed after Shred, stat err=%v", err)
	}
}

func TestShredIsIdempotent(t *testing.T) {
	// Shredding a non-existent file must not panic or error.
	Shred("/tmp/does-not-exist-kubeadm-test.yaml")
}

func TestSweepRunDirRemovesTransientFiles(t *testing.T) {
	dir := t.TempDir()

	// Create two transient files.
	p1, err := WriteTransient(dir, "secret1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p2, err := WriteTransient(dir, "secret2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Create a non-transient file that must survive.
	survive := filepath.Join(dir, "other.txt")
	if err := os.WriteFile(survive, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	SweepRunDir(dir)

	for _, p := range []string{p1, p2} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("expected transient file %s removed, stat err=%v", p, err)
		}
	}
	if _, err := os.Stat(survive); err != nil {
		t.Fatalf("non-transient file must survive sweep: %v", err)
	}
}

func TestSweepRunDirNonExistentDir(t *testing.T) {
	// Must not panic or fail.
	SweepRunDir("/tmp/does-not-exist-sweep-test-12345")
}
