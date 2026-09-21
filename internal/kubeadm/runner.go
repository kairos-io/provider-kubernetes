// Package kubeadm provides a thin, typed wrapper for invoking the kubeadm binary
// (ADR-1: orchestration runs through the binary via argv, never a shell) and for
// version detection / supported-window enforcement (ADR-3).
package kubeadm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"golang.org/x/sys/unix"

	"github.com/kairos-io/provider-kubernetes/internal/hostexec"
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

// StdinRunner executes the binary with an *os.File wired directly as its
// stdin (ADR-16-A2 decision 5, amending ADR-1-A1). It is how the bundled
// control-plane image tarballs reach `ctr images import -`: the caller passes
// the already fd-checked tarball itself, at its current offset, so there is
// no window between validating the file and importing it in which it could be
// swapped for a path lookup. Implementations MUST apply the same hygiene as
// Run (absolute-path refusal, closed environment, WaitDelay, error hygiene)
// and MUST refuse a nil, non-regular, or non-read-only descriptor.
type StdinRunner interface {
	RunStdin(ctx context.Context, stdin *os.File, args ...string) (Result, error)
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

// waitDelay bounds how long Run waits, after the child process itself has
// exited or ctx has ended, for any stray descendant that inherited stdout/
// stderr to release them (os/exec WaitDelay). Without a bound, a grandchild
// that keeps a pipe open can hang Run forever even though the direct child is
// long gone (#4099-1). Production always uses waitDelay; it is an unexported
// var only so tests can shorten it to exercise the kill path without a real
// wait.
var waitDelay = 5 * time.Second

// ExecRunner runs a real host binary via os/exec at a fixed absolute path with
// a complete, closed environment (ADR-1-A1). Its fields are unexported: the
// constructors below are the only way to obtain one, so nothing outside this
// package can hand it a PATH-resolved name or an inherited environment.
type ExecRunner struct {
	path string
	env  []string
}

// newExecRunner builds an ExecRunner from a hostexec.Command, normalizing a
// nil Env to an empty (never-inherited) slice.
func newExecRunner(c hostexec.Command) ExecRunner {
	env := c.Env
	if env == nil {
		env = []string{}
	}
	return ExecRunner{path: c.Path, env: env}
}

// DefaultRunner returns the runner for the bundled kubeadm binary (ADR-1-A1).
func DefaultRunner() ExecRunner {
	return newExecRunner(hostexec.Kubeadm(os.LookupEnv))
}

// KubectlRunner returns the runner for the bundled kubectl binary (ADR-1-A1).
func KubectlRunner() ExecRunner {
	return newExecRunner(hostexec.Kubectl(os.LookupEnv))
}

// CtrRunner returns the runner for the bundled ctr binary (ADR-16 image import).
func CtrRunner() ExecRunner {
	return newExecRunner(hostexec.Ctr())
}

// SystemctlRunner returns the runner for the base image's systemctl.
func SystemctlRunner() ExecRunner {
	return newExecRunner(hostexec.Systemctl())
}

// Run executes the configured binary with the given context as the hard
// deadline. It refuses to run anything if Path is empty or not absolute
// (ADR-1-A1: never resolve a bare name via PATH) -- so the zero value
// ExecRunner{} always errors without exec'ing anything. A nil Env becomes an
// empty, non-nil slice so the child never inherits the provider's own
// environment.
func (r ExecRunner) Run(ctx context.Context, args ...string) (Result, error) {
	if r.path == "" || !filepath.IsAbs(r.path) {
		return Result{}, fmt.Errorf("refusing to run %q: not an absolute path (ADR-1-A1)", r.path)
	}
	env := r.env
	if env == nil {
		env = []string{}
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, r.path, args...) //nolint:gosec // r.path is a fixed absolute constant, never PATH-resolved
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = env
	cmd.WaitDelay = waitDelay

	runErr := cmd.Run()

	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if runErr != nil {
		// Name the binary, args, exit code and sanitized stderr only -- NEVER the
		// environment (proxy URLs in Env can carry credentials, ADR-1-A1).
		name := filepath.Base(r.path)
		return res, fmt.Errorf("%s %v failed (exit %d): %w: %s", name, args, res.ExitCode, runErr, Sanitize(res.Stderr))
	}
	return res, nil
}

// RunStdin is Run's stdin-wired counterpart (ADR-16-A2 decision 5): stdin is
// assigned directly to cmd.Stdin (never wrapped in an io.Reader, and never
// passed via ExtraFiles), so os/exec dup2's the real descriptor into the
// child's fd 0. It applies every Run hygiene rule (absolute-path refusal,
// closed environment, WaitDelay, error hygiene naming only the binary/args/
// exit code/sanitized stderr) and additionally refuses to run at all if stdin
// is nil, not a regular file (fstat), or not opened O_RDONLY (fcntl
// F_GETFL) -- so a directory, FIFO, socket, or a file opened for writing is
// never wired to a child's stdin.
func (r ExecRunner) RunStdin(ctx context.Context, stdin *os.File, args ...string) (Result, error) {
	if stdin == nil {
		return Result{}, errors.New("refusing to run with a nil stdin file (ADR-16-A2)")
	}
	if err := checkReadOnlyRegularFile(stdin); err != nil {
		return Result{}, fmt.Errorf("refusing to run with stdin %s: %w", stdin.Name(), err)
	}
	if r.path == "" || !filepath.IsAbs(r.path) {
		return Result{}, fmt.Errorf("refusing to run %q: not an absolute path (ADR-1-A1)", r.path)
	}
	env := r.env
	if env == nil {
		env = []string{}
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, r.path, args...) //nolint:gosec // r.path is a fixed absolute constant, never PATH-resolved
	cmd.Stdin = stdin
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = env
	cmd.WaitDelay = waitDelay

	runErr := cmd.Run()

	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if runErr != nil {
		name := filepath.Base(r.path)
		return res, fmt.Errorf("%s %v failed (exit %d): %w: %s", name, args, res.ExitCode, runErr, Sanitize(res.Stderr))
	}
	return res, nil
}

// checkReadOnlyRegularFile requires f to fstat as a regular file that was
// opened with an O_RDONLY access mode. It never follows a symlink (fstat
// operates on the already-open descriptor) and never reads file content.
func checkReadOnlyRegularFile(f *os.File) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return fmt.Errorf("fstat: %w", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("not a regular file (mode %#o)", st.Mode)
	}
	flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFL, 0)
	if err != nil {
		return fmt.Errorf("fcntl F_GETFL: %w", err)
	}
	if flags&unix.O_ACCMODE != unix.O_RDONLY {
		return errors.New("not opened O_RDONLY")
	}
	return nil
}
