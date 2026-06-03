package status

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"
	"sigs.k8s.io/yaml"
)

// Path constants for the Layer-1 local status channel (ADR-4-S). These are
// the documented production paths; tests override them via FileSink.Paths.
const (
	// StatusRunPath is the primary status file on tmpfs: current-boot truth,
	// wiped on reboot. Permissions 0640 root:adm (maintainer decision 2026-06-03).
	StatusRunPath = "/run/provider-kubernetes/status.yaml"
	// StatusLogPath is the persistent post-mortem mirror. Permissions 0640 root:adm.
	StatusLogPath = "/var/log/provider-kubernetes/status.yaml"
)

// StatusSink records a Status value. Record is best-effort: it MUST NOT
// propagate write failures that would mask the real reconcile outcome. The
// contract is: call it, move on. Implementations log swallowed errors.
type StatusSink interface {
	Record(ctx context.Context, s Status)
}

// MultiSink fans out a Record call to N sinks in order, each isolated: a
// failure in one sink does not prevent subsequent sinks from running.
// This is the seam for S3's NodeAnnotationSink to plug into without any
// changes to the calling code.
type MultiSink []StatusSink

// Record calls every sink. Each sink runs independently; a panic in one sink
// is recovered so the others always run.
func (m MultiSink) Record(ctx context.Context, s Status) {
	for _, sink := range m {
		if sink == nil {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					logrus.Warnf("provider-kubernetes: status sink panic (swallowed): %v", r)
				}
			}()
			sink.Record(ctx, s)
		}()
	}
}

// FileSink writes the Status document atomically to one or more paths on the
// local filesystem. It targets:
//   - /run/provider-kubernetes/status.yaml  (tmpfs source-of-truth, wiped on reboot)
//   - /var/log/provider-kubernetes/status.yaml  (persistent post-mortem mirror)
//
// Permissions: 0640, owner root, group adm (ADR-4-S maintainer decision
// 2026-06-03). Parent directories are created 0750. If the adm group does not
// exist (containers, tests) the chown is skipped and logged -- not fatal.
//
// Each write runs under its own short deadline (WriteDeadline, default 2s).
// A failure on either path is logged and swallowed; Record never returns an
// error and never blocks (design principle 4 / #4099-1).
type FileSink struct {
	// Paths is the set of file paths to write. In production this is
	// {StatusRunPath, StatusLogPath}. Tests supply t.TempDir() paths.
	Paths []string
	// WriteDeadline caps each individual file write (including directory
	// creation and the atomic rename). Defaults to 2s when zero.
	WriteDeadline time.Duration
}

// NewFileSink returns a FileSink targeting the production paths.
func NewFileSink() *FileSink {
	return &FileSink{
		Paths: []string{
			StatusRunPath,
			StatusLogPath,
		},
	}
}

// deadline returns the effective write deadline.
func (f *FileSink) deadline() time.Duration {
	if f.WriteDeadline > 0 {
		return f.WriteDeadline
	}
	return 2 * time.Second
}

// Record marshals s to YAML and writes it atomically to every configured path.
// Each write is independent; a failure on one path does not prevent writes to
// the others.
func (f *FileSink) Record(ctx context.Context, s Status) {
	data, err := yaml.Marshal(s)
	if err != nil {
		logrus.Warnf("provider-kubernetes: status marshal: %v (swallowed)", err)
		return
	}

	admGID := lookupAdmGID()

	for _, path := range f.Paths {
		wctx, cancel := context.WithTimeout(ctx, f.deadline())
		writeErr := writeAtomic(wctx, path, data, admGID)
		cancel()
		if writeErr != nil {
			logrus.Warnf("provider-kubernetes: status write %s: %v (swallowed)", path, writeErr)
		}
	}
}

// writeAtomic writes data to path atomically by writing to a temp file in the
// same directory then renaming over the target. The file gets mode 0640; if
// admGID >= 0 the file's group is set to that GID.
func writeAtomic(ctx context.Context, path string, data []byte, admGID int) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context done before write: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".status-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()

	// Clean up the temp file on any error after this point.
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(0o640); err != nil {
		return fmt.Errorf("chmod temp: %w", err)
	}

	// Best-effort group chown. If admGID < 0 the lookup failed; skip silently
	// (already logged by lookupAdmGID).
	if admGID >= 0 {
		if err := tmp.Chown(-1, admGID); err != nil {
			// Not fatal: the file is still 0640 root:root which is a safe default.
			logrus.Debugf("provider-kubernetes: status chown to adm gid %d: %v (continuing)", admGID, err)
		}
	}

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context done before rename: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename to %s: %w", path, err)
	}
	ok = true
	return nil
}

// lookupAdmGID looks up the numeric GID of the "adm" group. Returns -1 when
// the group does not exist (containers, test environments) -- already logged
// so callers treat negative as "skip chown".
func lookupAdmGID() int {
	g, err := user.LookupGroup("adm")
	if err != nil {
		// Normal in containers and test environments; log once at debug level.
		logrus.Debugf("provider-kubernetes: adm group not found (%v); status file will be root:root 0640", err)
		return -1
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		logrus.Debugf("provider-kubernetes: adm group GID parse error (%v); status file will be root:root 0640", err)
		return -1
	}
	return gid
}
