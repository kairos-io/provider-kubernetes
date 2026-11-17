package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
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
	// It therefore goes BEFORE the wrapped error, flattened to one line and
	// capped: the operator reads this through the status document, whose Message
	// is cut at the first newline and then to 256 characters
	// (internal/status.sanitize). Appended after a multi-line kubeadm error, the
	// note is exactly the part that gets cut.
	//
	// Only lastErr is %w-wrapped, deliberately: errors.Is/As must target the
	// error the action actually ended on. The note is rendered with %s so it
	// contributes no wrapping of its own.
	if r.Budget.MaxAttempts > 1 && firstErr != nil && lastErr != nil &&
		firstErr.Error() != lastErr.Error() {
		return res, fmt.Errorf("failed after %d attempts (first attempt: %s): %w",
			r.Budget.MaxAttempts, oneLine(firstErr.Error(), firstAttemptNoteMax), lastErr)
	}
	return res, fmt.Errorf("failed after %d attempts: %w", r.Budget.MaxAttempts, lastErr)
}

// firstAttemptNoteMax caps the first-attempt note. The status Message is cut to
// 256 characters, so an unbounded note would push the error the action actually
// ended on out of the operator's view entirely.
const firstAttemptNoteMax = 120

// oneLine flattens s to a single line and caps it at max runes, so it survives
// the status document's first-newline cut without crowding out the rest of the
// message. Blank lines are dropped; the rest are joined with "; ".
func oneLine(s string, max int) string {
	// Only the first max runes can ever survive, so never materialize a
	// pathological error: the kubeadm runner captures stderr into an unbounded
	// buffer, and splitting tens of MiB into lines before capping allocates
	// orders of magnitude more than it keeps. Being OOM-killed here would cost
	// the status write in internal/provider.Run's defer -- losing exactly the
	// failure report this note exists to deliver. Input past the bound is
	// deliberately not consulted.
	const maxInput = 64 << 10
	if len(s) > maxInput {
		s = strings.ToValidUTF8(s[:maxInput], "")
	}
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(strings.TrimSuffix(line, "\r")); line != "" {
			kept = append(kept, line)
		}
	}
	out := strings.Join(kept, "; ")
	if rs := []rune(out); len(rs) > max {
		out = strings.TrimSpace(string(rs[:max])) + "..."
	}
	return out
}

// backoff is a simple linear backoff between attempts.
func backoff(attempt int) time.Duration {
	return time.Duration(attempt) * time.Second
}
