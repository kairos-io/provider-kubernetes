package reconcile

import (
	"context"
	"errors"
	"fmt"
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
	var lastErr error
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
		lastErr = err

		// A terminal decision will not change on retry; fail fast (still loud).
		if errors.Is(err, ErrTerminal) {
			return RunResult{Attempts: attempt}, fmt.Errorf("terminal on attempt %d: %w", attempt, err)
		}

		if attempt < r.Budget.MaxAttempts {
			sleep(backoff(attempt))
		}
	}
	return RunResult{
		Attempts:        r.Budget.MaxAttempts,
		BudgetExhausted: true,
	}, fmt.Errorf("failed after %d attempts: %w", r.Budget.MaxAttempts, lastErr)
}

// backoff is a simple linear backoff between attempts.
func backoff(attempt int) time.Duration {
	return time.Duration(attempt) * time.Second
}
