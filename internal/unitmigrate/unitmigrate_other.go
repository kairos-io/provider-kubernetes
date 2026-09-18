//go:build !linux

package unitmigrate

import (
	"context"

	"github.com/sirupsen/logrus"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
)

// Migrate has no implementation outside linux: the S19-7 walk relies on
// Linux-specific openat/fstatat/unlinkat/readlinkat semantics (O_NOFOLLOW,
// O_NONBLOCK, dev/ino comparison). Kairos images are Linux-only in
// production, so this fails closed (nothing removed, nothing claimed clean)
// rather than silently skipping every check.
func Migrate(_ context.Context, _ kubeadm.Runner) Result {
	logrus.Errorf("unit-migrate: not supported on this platform")
	res := Result{Outcome: OutcomeFailed}
	logSummary(res)
	return res
}

// logSummary emits the same summary line as the linux implementation.
func logSummary(res Result) {
	logrus.Infof("unit-migrate: summary outcome=%s removed=%d kept=%d overrides=%d reloaded=%t",
		res.Outcome, res.Removed, res.Kept, res.Overrides, res.Reloaded)
}
