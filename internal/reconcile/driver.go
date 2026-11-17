package reconcile

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrTerminal marks an executor error as a deliberate, non-retryable decision
// (e.g. refusing to init because the cluster already exists). The Reconciler
// returns immediately on such an error instead of burning the retry budget on a
// verdict that will not change. Wrap with fmt.Errorf("%w: ...", ErrTerminal, ...).
var ErrTerminal = errors.New("terminal")

// RunResult carries the post-execution metadata the status layer needs (S4).
// It is populated by Reconciler.RunWithResult; a zero value is safe (all-zero
// = success / no-op). Plan and Reconciler.Run are unchanged.
type RunResult struct {
	// LastAction is the action that was executing when the reconcile finished
	// (the final action on success; the failing action on error).
	LastAction Action
	// Attempts is the number of attempts consumed for LastAction.
	Attempts int
	// MaxAttempts is the budget cap that was in effect for LastAction.
	MaxAttempts int
	// BudgetExhausted is true when the failure reason is "retried N times and
	// still failing" (as opposed to a terminal fast-fail or a hard error on the
	// first attempt).
	BudgetExhausted bool
}

// Executor performs a single Action. Implementations MUST honor the context
// deadline and MUST NOT run unbounded work.
type Executor interface {
	Execute(ctx context.Context, a Action) error
}

// Reconciler drives a Plan to completion under a Budget with bounded retries, so
// the provider can never hang and block later Kairos boot stages (issue #4099-1).
type Reconciler struct {
	Budget Budget
	Exec   Executor
	// Sleep is the inter-attempt backoff; nil defaults to time.Sleep. Injectable
	// so tests do not actually wait.
	Sleep func(time.Duration)
}

// Run executes the actions in order. ActionNone is skipped. Each action is retried
// up to Budget.MaxAttempts, each attempt bounded by Budget.PerAttempt, all under a
// hard Budget.Total ceiling. On exhaustion it returns a non-nil error (fail loud);
// it never loops forever.
func (r Reconciler) Run(ctx context.Context, actions []Action) error {
	_, err := r.RunWithResult(ctx, actions)
	return err
}

// RunWithResult is identical to Run but also returns a RunResult so callers
// (the status layer, S4) can learn which action failed and how many attempts
// were consumed without any change to Plan or Run's semantics.
func (r Reconciler) RunWithResult(ctx context.Context, actions []Action) (RunResult, error) {
	if !r.Budget.Valid() {
		return RunResult{}, fmt.Errorf("invalid reconcile budget: %+v", r.Budget)
	}
	if r.Exec == nil {
		return RunResult{}, fmt.Errorf("reconciler has no executor")
	}
	sleep := r.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}

	totalCtx, cancel := context.WithTimeout(ctx, r.Budget.Total)
	defer cancel()

	var lastAction Action
	for _, a := range actions {
		if a == ActionNone {
			continue
		}
		lastAction = a
		result, err := r.runAction(totalCtx, a, sleep)
		if err != nil {
			result.LastAction = a
			result.MaxAttempts = r.Budget.MaxAttempts
			return result, fmt.Errorf("action %q: %w", a, err)
		}
	}
	return RunResult{LastAction: lastAction, MaxAttempts: r.Budget.MaxAttempts}, nil
}

func (r Reconciler) runAction(ctx context.Context, a Action, sleep func(time.Duration)) (RunResult, error) {
	var firstErr, lastErr error
	for attempt := 1; attempt <= r.Budget.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return RunResult{
				Attempts:        attempt,
				BudgetExhausted: true,
			}, fmt.Errorf("budget exhausted before attempt %d: %w", attempt, err)
		}

		attemptCtx, cancel := context.WithTimeout(ctx, r.Budget.PerAttempt)
		err := r.Exec.Execute(attemptCtx, a)
		cancel()
		if err == nil {
			return RunResult{Attempts: attempt}, nil
		}
		if firstErr == nil {
			firstErr = err
		}
		lastErr = err

		// A terminal decision will not change on retry; fail fast (still loud).
		if errors.Is(err, ErrTerminal) {
			return RunResult{Attempts: attempt}, fmt.Errorf("terminal on attempt %d: %w", attempt, err)
		}

		if attempt < r.Budget.MaxAttempts {
			sleep(backoff(attempt))
		}
	}
	res := RunResult{
		Attempts:        r.Budget.MaxAttempts,
		BudgetExhausted: true,
	}
	// A retry often fails only on what the first attempt left behind, so the
	// first attempt is usually the real cause and must reach the operator.
	//
	// The operator reads this through the status document, whose Message is cut
	// at the first newline and then to 256 bytes (internal/status.sanitize). The
	// first line is therefore a summary naming both causes, each reduced to the
	// part of the error that says what went wrong; the full errors follow on the
	// next lines for the log.
	//
	// Only lastErr is wrapped, deliberately: errors.Is/As must target the error
	// the action actually ended on.
	if r.Budget.MaxAttempts > 1 && firstErr != nil && lastErr != nil &&
		firstErr.Error() != lastErr.Error() {
		return res, &exhaustedError{
			msg: summarizeAttempts(r.Budget.MaxAttempts, firstErr.Error(), lastErr.Error()) +
				"\nfirst attempt: " + firstErr.Error() +
				"\nlast attempt: " + lastErr.Error(),
			last: lastErr,
		}
	}
	return res, fmt.Errorf("failed after %d attempts: %w", r.Budget.MaxAttempts, lastErr)
}

// exhaustedError reports an action that failed every attempt with differing
// errors. It unwraps to the final attempt's error only.
type exhaustedError struct {
	msg  string
	last error
}

func (e *exhaustedError) Error() string { return e.msg }
func (e *exhaustedError) Unwrap() error { return e.last }

// summaryMax bounds the one-line summary of an exhausted action. The status
// Message keeps 256 bytes, and ahead of this summary it carries the action name
// twice plus the "reconcile: " and "action %q: " wrapping; with the longest
// action name that prefix is 72 bytes.
const summaryMax = 176

// summarizeAttempts renders the single-line summary of an action whose first
// and last attempts failed differently. The two causes share the space left by
// the fixed text; a short cause hands what it does not use to the other. When
// both do not fit, kubeadm's preflight check failures are shortened to their
// names before anything is cut.
func summarizeAttempts(attempts int, first, last string) string {
	head := fmt.Sprintf("failed after %d attempts: first attempt: ", attempts)
	const mid = "; last attempt: "
	room := summaryMax - len(head) - len(mid)
	fullF, fullL := errorCause(first), errorCause(last)
	var f, l string
	for level := 0; level <= maxCompact; level++ {
		f, l = compactCause(fullF, level), compactCause(fullL, level)
		if len(f)+len(l) <= room {
			break
		}
	}
	fMax, lMax := room/2, room-room/2
	if len(f) < fMax {
		lMax += fMax - len(f)
	} else if len(l) < lMax {
		fMax += lMax - len(l)
	}
	return head + capBytes(f, fMax) + mid + capBytes(l, lMax)
}

// runnerPrefix matches the command description internal/kubeadm's runner puts
// ahead of a failed command's stderr: "<name> [<args>] failed (exit N): <exec
// error>: ", plus anything wrapped around it.
var runnerPrefix = regexp.MustCompile(`^.*? failed \(exit -?\d+\): (?:signal: )?[^:]*: `)

// underlyingCause matches the lines kubeadm prints ahead of its "error ..." line
// that say why a wait gave up.
var underlyingCause = regexp.MustCompile(`(?i)timed out|deadline exceeded|not healthy`)

// genericFailure matches kubeadm "error ..." lines that only say a phase failed,
// capturing the phase when there is one.
var genericFailure = regexp.MustCompile(`^(?:([\w-]+): )?(?:(?:couldn't|could not) initialize a Kubernetes cluster|preflight checks failed)$`)

// errorCause reduces an error message to the single line that says what went
// wrong. The command line kubeadm ran, its progress output and its
// troubleshooting advice are dropped. What stays is kubeadm's own "error ..."
// line, the [ERROR ...] lines and the line naming why a wait gave up, wherever
// kubeadm printed them; an "error ..." line that only says the phase failed is
// cut to the phase name when a more precise cause is there. Without an "error"
// line the whole message is kept, flattened. The parts are joined with "; ".
func errorCause(s string) string {
	// Only a few dozen bytes survive, so never split a pathological message:
	// the kubeadm runner captures stderr into an unbounded buffer, and being
	// OOM-killed here would cost the status write in internal/provider.Run's
	// defer. kubeadm prints its error last, so the tail is searched for it and
	// the head is the fallback.
	const maxInput = 64 << 10
	head, tail := s, s
	if len(s) > maxInput {
		head = strings.ToValidUTF8(s[:maxInput], "")
		tail = strings.ToValidUTF8(s[len(s)-maxInput:], "")
	}
	lines := causeLines(tail)
	at := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.ToLower(line), "error") {
			at = i
			break
		}
	}
	if at < 0 {
		return strings.Join(causeLines(head), "; ")
	}
	msg := strings.TrimPrefix(lines[at], "error: ")
	msg = strings.TrimPrefix(msg, "error execution phase ")

	var causes, progress []string
	for _, line := range lines[:at] {
		switch {
		case strings.HasPrefix(line, "[ERROR "):
			causes = append(causes, line)
		case underlyingCause.MatchString(line):
			// "[kubelet-check] ..." progress lines repeat themselves; they only
			// stand in when nothing better is there.
			if strings.HasPrefix(line, "[") {
				progress = append(progress, line)
			} else {
				causes = append(causes, line)
			}
		}
	}
	causes = append(causes, lines[at+1:]...)
	if len(causes) == 0 && len(progress) > 0 {
		causes = progress[len(progress)-1:]
	}
	causes = dedupe(causes)

	if m := genericFailure.FindStringSubmatch(msg); m != nil && len(causes) > 0 {
		if m[1] == "" {
			return strings.Join(causes, "; ")
		}
		return m[1] + ": " + strings.Join(causes, "; ")
	}
	return strings.Join(append([]string{msg}, causes...), "; ")
}

// boilerplate lists the starts of the advice lines kubeadm prints around an
// error. They never name the cause.
var boilerplate = []string{
	"- ",
	"If you are on a systemd-powered system",
	"Additionally, a control plane component may have crashed",
	"To troubleshoot, list all containers",
	"Here is one example",
	"Once you have found the failing container",
	"To see the stack trace",
	"[preflight] If you know what you are doing",
}

// boilerplateHeaders are advice headers that may carry the cause on the same
// line, as in "Unfortunately, an error has occurred: timed out ...". Only what
// follows the first colon is kept.
var boilerplateHeaders = []string{
	"Unfortunately, an error has occurred",
	"This error is likely caused by",
}

// causeLines splits s into trimmed, non-empty lines with the runner's command
// prefix and kubeadm's advice removed.
func causeLines(s string) []string {
	var kept []string
next:
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(runnerPrefix.ReplaceAllString(strings.TrimSuffix(line, "\r"), ""))
		for _, p := range boilerplate {
			if strings.HasPrefix(line, p) {
				continue next
			}
		}
		for _, p := range boilerplateHeaders {
			if strings.HasPrefix(line, p) {
				_, line, _ = strings.Cut(line, ":")
				line = strings.TrimSpace(line)
				break
			}
		}
		if line != "" {
			kept = append(kept, line)
		}
	}
	return kept
}

// dedupe drops repeated lines, keeping the first of each.
func dedupe(lines []string) []string {
	seen := make(map[string]bool, len(lines))
	var kept []string
	for _, line := range lines {
		if !seen[line] {
			seen[line] = true
			kept = append(kept, line)
		}
	}
	return kept
}

// maxCompact is the strongest level compactCause knows.
const maxCompact = 2

var (
	preflightError = regexp.MustCompile(`\[ERROR ([^\]]+)\]:[^;]*`)
	preflightName  = regexp.MustCompile(`\[ERROR ([^\]]+)\]`)
)

// compactCause shortens kubeadm's preflight failures in a cause from
// errorCause. Level 1 keeps only each check's "[ERROR Name]"; level 2 folds a
// run of them into one "[ERROR Name xN, Other]" token, counting checks of the
// same kind ("FileAvailable--<path>") together. Other text is left alone.
func compactCause(cause string, level int) string {
	if level < 1 {
		return cause
	}
	cause = strings.ReplaceAll(cause, "[preflight] Some fatal errors occurred:; ", "")
	cause = preflightError.ReplaceAllString(cause, "[ERROR $1]")
	if level < 2 {
		return cause
	}
	locs := preflightName.FindAllStringSubmatchIndex(cause, -1)
	if len(locs) < 2 {
		return cause
	}
	for i := 1; i < len(locs); i++ {
		if cause[locs[i-1][1]:locs[i][0]] != "; " {
			return cause
		}
	}
	var kinds []string
	count := map[string]int{}
	for _, loc := range locs {
		kind, _, _ := strings.Cut(cause[loc[2]:loc[3]], "--")
		if count[kind] == 0 {
			kinds = append(kinds, kind)
		}
		count[kind]++
	}
	for i, kind := range kinds {
		if count[kind] > 1 {
			kinds[i] = fmt.Sprintf("%s x%d", kind, count[kind])
		}
	}
	return cause[:locs[0][0]] + "[ERROR " + strings.Join(kinds, ", ") + "]" + cause[locs[len(locs)-1][1]:]
}

// capBytes cuts s to at most max bytes on a rune boundary, marking the cut.
func capBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max - len("...")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + "..."
}

// backoff is a simple linear backoff between attempts.
func backoff(attempt int) time.Duration {
	return time.Duration(attempt) * time.Second
}
