//go:build linux

package imageimport

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// treePaths names every path in a valid test tree (ADR-16-A2 O-3).
type treePaths struct {
	root         string
	systemDir    string
	providersDir string
	anchorFile   string
	providerDir  string
	imagesDir    string
	lockFile     string
	tarball      string
}

// newValidTree builds a complete, valid bundle tree under a fresh temp
// directory and points the package's rootPath/expectedOwnerUID seams at it
// (restored via t.Cleanup). Every directory is 0755 and every file 0644,
// owned by the test's own uid (expectedOwnerUID is set to match, since an
// unprivileged test cannot create root-owned fixtures).
func newValidTree(t *testing.T) treePaths {
	t.Helper()
	root := t.TempDir()
	// t.TempDir()'s own mode depends on the process umask (observed 0775 in
	// some environments); this tree's root stands in for "/" in the walk, so
	// pin it to a known-safe mode regardless of umask.
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatalf("chmod root: %v", err)
	}
	tp := treePaths{
		root:         root,
		systemDir:    filepath.Join(root, "system"),
		providersDir: filepath.Join(root, "system", "providers"),
		anchorFile:   filepath.Join(root, "system", "providers", providerBinaryName),
		providerDir:  filepath.Join(root, "system", providerDirName),
		imagesDir:    filepath.Join(root, "system", providerDirName, "images"),
		lockFile:     filepath.Join(root, "system", providerDirName, "images", "images.lock"),
		tarball:      filepath.Join(root, "system", providerDirName, "images", "example.tar"),
	}

	for _, d := range []string{tp.systemDir, tp.providersDir, tp.providerDir, tp.imagesDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(tp.anchorFile, []byte("#!/bin/fake-binary\n"), 0o644); err != nil {
		t.Fatalf("write anchor: %v", err)
	}
	if err := os.WriteFile(tp.lockFile, []byte(`{"lock":"placeholder"}`), 0o644); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	if err := os.WriteFile(tp.tarball, []byte(strings.Repeat("x", minTarballSize)), 0o644); err != nil {
		t.Fatalf("write tarball: %v", err)
	}

	setSeams(t, root, os.Getuid())
	return tp
}

// setSeams points the package-private walk seams at root/uid and restores
// the previous values on cleanup. These tests never run in parallel with
// each other (no t.Parallel()): the seams are package-global.
func setSeams(t *testing.T, root string, uid int) {
	t.Helper()
	oldRoot, oldUID, oldFstat := rootPath, expectedOwnerUID, fstatFn
	rootPath, expectedOwnerUID = root, uid
	t.Cleanup(func() {
		rootPath, expectedOwnerUID, fstatFn = oldRoot, oldUID, oldFstat
	})
}

// withDeviceOverride makes fstatFn report devOverride for any fd whose
// /proc/self/fd readlink target ends with pathSuffix, simulating a
// device mismatch without a second real filesystem (hardware-free, O-3).
func withDeviceOverride(t *testing.T, pathSuffix string, devOverride uint64) {
	t.Helper()
	real := fstatFn
	fstatFn = func(fd int, st *unix.Stat_t) error {
		if err := real(fd, st); err != nil {
			return err
		}
		if link, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd)); err == nil && strings.HasSuffix(link, pathSuffix) {
			st.Dev = devOverride
		}
		return nil
	}
	t.Cleanup(func() { fstatFn = real })
}

func TestWalkBundleAcceptsValidTree(t *testing.T) {
	tp := newValidTree(t)

	b, err := walkBundle()
	if err != nil {
		t.Fatalf("walkBundle: %v", err)
	}
	defer b.Close()

	if string(b.LockData()) != `{"lock":"placeholder"}` {
		t.Errorf("LockData() = %q", b.LockData())
	}

	f, reason, detail := b.OpenTarball("example.tar", minTarballSize, maxTarballSize, ReasonMissing)
	if reason != "" {
		t.Fatalf("OpenTarball: reason=%s detail=%s", reason, detail)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() != minTarballSize {
		t.Errorf("OpenTarball returned unexpected file: size=%d err=%v", info.Size(), err)
	}
	off, err := f.Seek(0, io.SeekCurrent)
	if err != nil || off != 0 {
		t.Errorf("OpenTarball file offset = %d, err=%v, want 0", off, err)
	}

	_ = tp
}

func TestWalkBundleMissingProviderDirIsNotBundled(t *testing.T) {
	tp := newValidTree(t)
	if err := os.RemoveAll(tp.providerDir); err != nil {
		t.Fatal(err)
	}

	_, err := walkBundle()
	if !errors.Is(err, errNotBundled) {
		t.Fatalf("err = %v, want errNotBundled", err)
	}
}

func TestWalkBundleMissingImagesDirIsRefused(t *testing.T) {
	tp := newValidTree(t)
	if err := os.RemoveAll(tp.imagesDir); err != nil {
		t.Fatal(err)
	}

	_, err := walkBundle()
	if errors.Is(err, errNotBundled) {
		t.Fatal("missing images dir (parent provider-kubernetes present) must be refused, not not-bundled")
	}
	var be *bundleError
	if !errors.As(err, &be) || be.reason != ReasonDirUnsafe {
		t.Fatalf("err = %v, want a dir-unsafe bundleError", err)
	}
}

func TestWalkBundleSymlinkAtEachDirComponent(t *testing.T) {
	outside := func(t *testing.T) string {
		d := t.TempDir()
		if err := os.Mkdir(filepath.Join(d, "elsewhere"), 0o755); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(d, "elsewhere")
	}

	cases := []struct {
		name       string
		replace    func(tp treePaths) string // returns the path to replace with a symlink
		wantReason Reason
	}{
		{"system", func(tp treePaths) string { return tp.systemDir }, ReasonDirUnsafe},
		{"provider-kubernetes", func(tp treePaths) string { return tp.providerDir }, ReasonDirUnsafe},
		{"images", func(tp treePaths) string { return tp.imagesDir }, ReasonDirUnsafe},
		{"providers", func(tp treePaths) string { return tp.providersDir }, ReasonAnchorUnsafe},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tp := newValidTree(t)
			target := c.replace(tp)
			if err := os.RemoveAll(target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside(t), target); err != nil {
				t.Fatal(err)
			}

			_, err := walkBundle()
			var be *bundleError
			if !errors.As(err, &be) || be.reason != c.wantReason {
				t.Fatalf("err = %v, want a %s bundleError", err, c.wantReason)
			}
		})
	}

	t.Run("the anchor binary itself", func(t *testing.T) {
		tp := newValidTree(t)
		if err := os.Remove(tp.anchorFile); err != nil {
			t.Fatal(err)
		}
		realBin := filepath.Join(t.TempDir(), "real-binary")
		if err := os.WriteFile(realBin, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realBin, tp.anchorFile); err != nil {
			t.Fatal(err)
		}

		_, err := walkBundle()
		var be *bundleError
		if !errors.As(err, &be) || be.reason != ReasonAnchorUnsafe {
			t.Fatalf("err = %v, want an anchor-unsafe bundleError", err)
		}
	})

	t.Run("the lock file itself", func(t *testing.T) {
		tp := newValidTree(t)
		if err := os.Remove(tp.lockFile); err != nil {
			t.Fatal(err)
		}
		realLock := filepath.Join(t.TempDir(), "real-lock")
		if err := os.WriteFile(realLock, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realLock, tp.lockFile); err != nil {
			t.Fatal(err)
		}

		_, err := walkBundle()
		var be *bundleError
		if !errors.As(err, &be) || be.reason != ReasonSymlink {
			t.Fatalf("err = %v, want a symlink bundleError", err)
		}
	})
}

func TestWalkBundleNonDirectoryComponent(t *testing.T) {
	tp := newValidTree(t)
	if err := os.RemoveAll(tp.imagesDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tp.imagesDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := walkBundle()
	var be *bundleError
	if !errors.As(err, &be) || be.reason != ReasonDirUnsafe {
		t.Fatalf("err = %v, want a dir-unsafe bundleError", err)
	}
}

func TestWalkBundleWrongOwner(t *testing.T) {
	tp := newValidTree(t)
	setSeams(t, tp.root, os.Getuid()+12345) // no real file matches this uid

	_, err := walkBundle()
	var be *bundleError
	if !errors.As(err, &be) || be.reason != ReasonDirUnsafe {
		t.Fatalf("err = %v, want a dir-unsafe bundleError (owner)", err)
	}
}

func TestWalkBundleGroupOtherWritableDir(t *testing.T) {
	tp := newValidTree(t)
	if err := os.Chmod(tp.imagesDir, 0o777); err != nil {
		t.Fatal(err)
	}

	_, err := walkBundle()
	var be *bundleError
	if !errors.As(err, &be) || be.reason != ReasonDirUnsafe {
		t.Fatalf("err = %v, want a dir-unsafe bundleError (mode)", err)
	}
}

func TestOpenTarballGroupOtherWritableFile(t *testing.T) {
	tp := newValidTree(t)
	if err := os.Chmod(tp.tarball, 0o666); err != nil {
		t.Fatal(err)
	}

	b, err := walkBundle()
	if err != nil {
		t.Fatalf("walkBundle: %v", err)
	}
	defer b.Close()

	f, reason, _ := b.OpenTarball("example.tar", minTarballSize, maxTarballSize, ReasonMissing)
	if reason != ReasonMode {
		t.Fatalf("reason = %s, want mode (f=%v)", reason, f)
	}
}

func TestOpenTarballSetuidFile(t *testing.T) {
	tp := newValidTree(t)
	// os.FileMode's setuid bit (os.ModeSetuid) is NOT the traditional unix
	// 04000 bit position -- os.Chmod translates it internally, so it must be
	// set via the ModeSetuid constant, not a raw 0o4644 literal.
	if err := os.Chmod(tp.tarball, 0o644|os.ModeSetuid); err != nil {
		t.Fatal(err)
	}

	b, err := walkBundle()
	if err != nil {
		t.Fatalf("walkBundle: %v", err)
	}
	defer b.Close()

	f, reason, _ := b.OpenTarball("example.tar", minTarballSize, maxTarballSize, ReasonMissing)
	if reason != ReasonMode {
		t.Fatalf("reason = %s, want mode (f=%v)", reason, f)
	}
}

// TestOpenTarballFIFONeverHangs is O-3's FIFO case: a planted FIFO in place
// of a tarball must be refused (not-regular), and MUST NOT block boot even
// though no writer will ever open the other end -- proven with a bounded test
// deadline around the call (not just the process-wide test timeout).
func TestOpenTarballFIFONeverHangs(t *testing.T) {
	tp := newValidTree(t)
	if err := os.Remove(tp.tarball); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(tp.tarball, 0o644); err != nil {
		t.Fatal(err)
	}

	b, err := walkBundle()
	if err != nil {
		t.Fatalf("walkBundle: %v", err)
	}
	defer b.Close()

	done := make(chan struct{})
	var reason Reason
	go func() {
		defer close(done)
		_, reason, _ = b.OpenTarball("example.tar", minTarballSize, maxTarballSize, ReasonMissing)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("OpenTarball on a FIFO did not return within the test deadline -- boot would hang")
	}
	if reason != ReasonNotRegular {
		t.Fatalf("reason = %s, want not-regular", reason)
	}
}

func TestOpenTarballDeviceMismatch(t *testing.T) {
	tp := newValidTree(t)
	withDeviceOverride(t, "/example.tar", 0xDEADBEEF)

	b, err := walkBundle()
	if err != nil {
		t.Fatalf("walkBundle: %v", err)
	}
	defer b.Close()

	f, reason, _ := b.OpenTarball("example.tar", minTarballSize, maxTarballSize, ReasonMissing)
	if reason != ReasonDevice {
		t.Fatalf("reason = %s, want device (f=%v)", reason, f)
	}
	_ = tp
}

func TestOpenTarballSizeBounds(t *testing.T) {
	tp := newValidTree(t)
	b, err := walkBundle()
	if err != nil {
		t.Fatalf("walkBundle: %v", err)
	}
	defer b.Close()

	if f, reason, _ := b.OpenTarball("example.tar", minTarballSize+1, maxTarballSize, ReasonMissing); reason != ReasonSize {
		t.Fatalf("reason = %s, want size (f=%v)", reason, f)
	}
	_ = tp
}

func TestOpenTarballMissingIsReported(t *testing.T) {
	newValidTree(t)
	b, err := walkBundle()
	if err != nil {
		t.Fatalf("walkBundle: %v", err)
	}
	defer b.Close()

	if f, reason, _ := b.OpenTarball("does-not-exist.tar", minTarballSize, maxTarballSize, ReasonMissing); reason != ReasonMissing {
		t.Fatalf("reason = %s, want missing (f=%v)", reason, f)
	}
}

func TestUnlistedNamesAreReportedButNeverOpened(t *testing.T) {
	tp := newValidTree(t)
	extra := filepath.Join(tp.imagesDir, "evil.tar")
	if err := os.WriteFile(extra, []byte(strings.Repeat("y", minTarballSize)), 0o644); err != nil {
		t.Fatal(err)
	}

	b, err := walkBundle()
	if err != nil {
		t.Fatalf("walkBundle: %v", err)
	}
	defer b.Close()

	unlisted := b.Unlisted(map[string]bool{"example.tar": true})
	found := false
	for _, n := range unlisted {
		if n == "evil.tar" {
			found = true
		}
		if n == "images.lock" || n == "example.tar" {
			t.Errorf("Unlisted() must exclude images.lock and locked names, got %q", n)
		}
	}
	if !found {
		t.Fatalf("Unlisted() = %v, want to include %q", unlisted, "evil.tar")
	}
}
