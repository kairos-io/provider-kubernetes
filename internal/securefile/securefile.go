// Package securefile provides helpers for writing and shredding transient
// secret-bearing files on ephemeral storage (ADR-2 / OQ-7). Files are created
// 0600 root:root under a caller-supplied directory (which MUST be tmpfs, e.g.
// /run); they are shredded immediately after the kubeadm exec returns so no
// secret rests on disk beyond the exec lifetime.
package securefile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sirupsen/logrus"
)

const (
	transientPrefix = "kubeadm-"
	transientSuffix = ".yaml"
)

// writeContent writes the transient file content. It is a variable so tests can
// simulate a failing write (a full tmpfs) without filling a real filesystem.
var writeContent = func(f *os.File, content string) error {
	_, err := f.WriteString(content)
	return err
}

// WriteTransient writes content to a fresh 0600 file under dir (ephemeral
// storage) and returns the file path. The caller is responsible for calling
// Shred on the returned path after use. If anything fails after the file is
// created, the file is removed before returning, so a failed write (for example
// ENOSPC on a full tmpfs) never leaves a partial secret behind.
func WriteTransient(dir, content string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create run dir: %w", err)
	}
	f, err := os.CreateTemp(dir, "kubeadm-*.yaml")
	if err != nil {
		return "", fmt.Errorf("create transient config: %w", err)
	}
	name := f.Name()
	fail := func(err error) (string, error) {
		_ = f.Close()
		Shred(name)
		return "", err
	}
	if err := f.Chmod(0o600); err != nil {
		return fail(fmt.Errorf("chmod transient config: %w", err))
	}
	if err := writeContent(f, content); err != nil {
		return fail(fmt.Errorf("write transient config: %w", err))
	}
	if err := f.Close(); err != nil {
		Shred(name)
		return "", fmt.Errorf("close transient config: %w", err)
	}
	return name, nil
}

// Shred best-effort overwrites then removes a transient secret file. On tmpfs
// (/run) removal already prevents persistence; the overwrite is defense-in-depth.
// A lingering secret file is a real risk so failures are logged rather than silently
// swallowed (Design Principle 5).
//
// Shred never follows symlinks: a symlink at path is removed without touching
// its target, and other non-regular files are left alone.
func Shred(path string) {
	info, err := os.Lstat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logrus.Debugf("provider-kubernetes: failed to stat transient secret file %s: %v", path, err)
		}
		return
	}
	switch {
	case info.Mode().IsRegular():
		zero(path, info)
	case info.Mode()&os.ModeSymlink != 0:
	default:
		logrus.Warnf("provider-kubernetes: refusing to shred non-regular file %s (%s)", path, info.Mode().Type())
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		logrus.Debugf("provider-kubernetes: failed to remove transient secret file %s: %v", path, err)
	}
}

// zero overwrites the regular file at path with zeros. The file is opened with
// O_NOFOLLOW and checked against the Lstat result so a path swapped for a
// symlink or another file in between is not written through.
func zero(path string, info os.FileInfo) {
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		logrus.Debugf("provider-kubernetes: failed to open transient secret file %s for overwrite: %v", path, err)
		return
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		logrus.Warnf("provider-kubernetes: transient secret file %s changed before overwrite, not zeroing", path)
		return
	}
	if _, err := f.Write(make([]byte, opened.Size())); err != nil {
		logrus.Debugf("provider-kubernetes: failed to overwrite transient secret file %s: %v", path, err)
	}
}

// SweepRunDir removes all kubeadm-*.yaml transient config files from dir that
// may have been left by an interrupted join. It is called during reset to ensure
// no stale credential-bearing files persist. Non-fatal: failures are logged.
// Only names WriteTransient can produce are matched; each match goes through
// Shred, so symlinks are removed without touching their targets.
func SweepRunDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			logrus.Warnf("provider-kubernetes: sweep rundir %s: %v", dir, err)
		}
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if isTransientName(name) {
			Shred(filepath.Join(dir, name))
		}
	}
}

// isTransientName reports whether name matches the "kubeadm-*.yaml" pattern
// WriteTransient passes to os.CreateTemp, with a non-empty random part.
func isTransientName(name string) bool {
	return len(name) > len(transientPrefix)+len(transientSuffix) &&
		strings.HasPrefix(name, transientPrefix) &&
		strings.HasSuffix(name, transientSuffix)
}
