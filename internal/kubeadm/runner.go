// Package kubeadm provides a thin, typed wrapper for invoking the kubeadm binary
// (ADR-1: orchestration runs through the binary via argv, never a shell) and for
// version detection / supported-window enforcement (ADR-3).
package kubeadm

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// Result captures the outcome of a kubeadm invocation.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner executes the kubeadm binary. Implementations MUST pass arguments as argv
// (never via a shell, to avoid injection and quoting hazards) and MUST honor the
// context's deadline/cancellation so callers can bound every invocation (ADR-4 /
// issue #4099-1: nothing may hang).
type Runner interface {
	Run(ctx context.Context, args ...string) (Result, error)
}

// ExecRunner runs the real kubeadm binary via os/exec.
type ExecRunner struct {
	// Path to the kubeadm binary. Empty means resolve "kubeadm" from PATH.
	Path string
}

// Run executes `kubeadm <args...>` with the given context as the hard deadline.
func (r ExecRunner) Run(ctx context.Context, args ...string) (Result, error) {
	path := r.Path
	if path == "" {
		path = "kubeadm"
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if runErr != nil {
		return res, fmt.Errorf("kubeadm %v failed (exit %d): %w: %s", args, res.ExitCode, runErr, res.Stderr)
	}
	return res, nil
}
