package securefile

import (
	"errors"
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

func TestWriteTransientRemovesFileWhenWriteFails(t *testing.T) {
	dir := t.TempDir()
	orig := writeContent
	t.Cleanup(func() { writeContent = orig })
	writeContent = func(f *os.File, content string) error {
		// Write part of the content, then fail like a full tmpfs would.
		_, _ = f.WriteString(content[:4])
		return errors.New("no space left on device")
	}

	path, err := WriteTransient(dir, "secret: value\n")
	if err == nil {
		t.Fatal("expected an error when the write fails")
	}
	if path != "" {
		t.Fatalf("expected an empty path on error, got %q", path)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no leftover files, found %d", len(entries))
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != want {
		t.Fatalf("%s content changed: got %q, want %q", path, string(data), want)
	}
}

func TestShredRemovesSymlinkWithoutTouchingTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target.conf")
	writeFile(t, target, "placeholder-target-content")
	link := filepath.Join(dir, "kubeadm-link.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	Shred(link)

	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("expected symlink removed, lstat err=%v", err)
	}
	assertContent(t, target, "placeholder-target-content")
}

func TestShredLeavesDirectoryAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kubeadm-dir.yaml")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(dir, "inner")
	writeFile(t, inner, "placeholder-inner-content")

	Shred(dir)

	assertContent(t, inner, "placeholder-inner-content")
}

func TestSweepRunDirOnlyMatchesTransientNames(t *testing.T) {
	dir := t.TempDir()
	transient := filepath.Join(dir, "kubeadm-x.yaml")
	writeFile(t, transient, "placeholder-transient-content")
	keep := map[string]string{
		"kubeadmcfg.yaml":   "placeholder-kubeadmcfg",
		"kubeadm-.yaml":     "placeholder-empty-random",
		"kubeadm-x.yaml.bk": "placeholder-backup",
		"kubeadm.yaml":      "placeholder-plain",
	}
	for name, content := range keep {
		writeFile(t, filepath.Join(dir, name), content)
	}

	SweepRunDir(dir)

	if _, err := os.Lstat(transient); !os.IsNotExist(err) {
		t.Fatalf("expected %s removed, lstat err=%v", transient, err)
	}
	for name, content := range keep {
		assertContent(t, filepath.Join(dir, name), content)
	}
}

func TestSweepRunDirDoesNotFollowSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "kubeadmcfg.yaml")
	writeFile(t, target, "placeholder-target-content")
	link := filepath.Join(dir, "kubeadm-link.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	SweepRunDir(dir)

	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("expected symlink removed, lstat err=%v", err)
	}
	assertContent(t, target, "placeholder-target-content")
}
