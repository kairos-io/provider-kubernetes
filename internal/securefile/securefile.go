// Package securefile provides helpers for writing and shredding transient
// secret-bearing files on ephemeral storage (ADR-2 / OQ-7). Files are created
// 0600 root:root under a caller-supplied directory (which MUST be tmpfs, e.g.
// /run); they are shredded immediately after the kubeadm exec returns so no
// secret rests on disk beyond the exec lifetime.
package securefile

import (
	"fmt"
	"os"

	"github.com/sirupsen/logrus"
)

// WriteTransient writes content to a fresh 0600 file under dir (ephemeral
// storage) and returns the file path. The caller is responsible for calling
// Shred on the returned path after use.
func WriteTransient(dir, content string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create run dir: %w", err)
	}
	f, err := os.CreateTemp(dir, "kubeadm-*.yaml")
	if err != nil {
		return "", fmt.Errorf("create transient config: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Chmod(0o600); err != nil {
		return "", fmt.Errorf("chmod transient config: %w", err)
	}
	if _, err := f.WriteString(content); err != nil {
		return "", fmt.Errorf("write transient config: %w", err)
	}
	return f.Name(), nil
}

// Shred best-effort overwrites then removes a transient secret file. On tmpfs
// (/run) removal already prevents persistence; the overwrite is defense-in-depth.
// A lingering secret file is a real risk so failures are logged rather than silently
// swallowed (Design Principle 5).
func Shred(path string) {
	if info, err := os.Stat(path); err == nil {
		_ = os.WriteFile(path, make([]byte, info.Size()), 0o600)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		logrus.Debugf("provider-kubernetes: failed to remove transient secret file %s: %v", path, err)
	}
}

// SweepRunDir removes all kubeadm-*.yaml transient config files from dir that
// may have been left by an interrupted join. It is called during reset to ensure
// no stale credential-bearing files persist. Non-fatal: failures are logged.
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
		// Match the pattern used by WriteTransient: "kubeadm-*.yaml"
		if len(name) > 12 && name[:7] == "kubeadm" && name[len(name)-5:] == ".yaml" {
			p := dir + "/" + name
			Shred(p)
		}
	}
}
