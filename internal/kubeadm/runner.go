// Package kubeadm provides a thin, typed wrapper for invoking the kubeadm binary
// (ADR-1: orchestration runs through the binary via argv, never a shell) and for
// version detection / supported-window enforcement (ADR-3).
package kubeadm

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
)

// Result captures the outcome of a kubeadm invocation.
//
// SECURITY: Stderr and Stdout are raw kubeadm output and MAY contain secrets
// (e.g. `kubeadm init` prints the join command including a bootstrap token).
// Callers MUST NOT log these fields directly; run them through Sanitize first.
// The error returned by Run already has its embedded stderr sanitized.
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

var (
	// reBootstrapToken matches a kubeadm bootstrap token "abcdef.0123456789abcdef".
	reBootstrapToken = regexp.MustCompile(`[a-z0-9]{6}\.[a-z0-9]{16}`)
	// reHex64 matches a 64-hex value (certificate key / SPKI hash hex).
	reHex64 = regexp.MustCompile(`[a-fA-F0-9]{64}`)
	// rePEM matches a PEM block (private keys, certs).
	rePEM = regexp.MustCompile(`(?s)-----BEGIN [^-]+-----.*?-----END [^-]+-----`)
)

// Sanitize redacts secret-shaped substrings (bootstrap tokens, 64-hex keys/hashes,
// PEM blocks) from kubeadm output so it is safe to log. It is intentionally
// conservative: it may over-redact, never under-redact (ADR-2.7: no secrets in logs).
func Sanitize(s string) string {
	s = rePEM.ReplaceAllString(s, "[REDACTED-PEM]")
	s = reBootstrapToken.ReplaceAllString(s, "[REDACTED-TOKEN]")
	s = reHex64.ReplaceAllString(s, "[REDACTED-HEX]")
	return s
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
		// Sanitize stderr before it enters the (loggable) error chain. Args are
		// safe to include: by design no secret is ever passed on argv.
		return res, fmt.Errorf("kubeadm %v failed (exit %d): %w: %s", args, res.ExitCode, runErr, Sanitize(res.Stderr))
	}
	return res, nil
}
