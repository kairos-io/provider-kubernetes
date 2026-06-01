package reconcile

import (
	"context"
	"fmt"
	"time"
)

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
	if !r.Budget.Valid() {
		return fmt.Errorf("invalid reconcile budget: %+v", r.Budget)
	}
	if r.Exec == nil {
		return fmt.Errorf("reconciler has no executor")
	}
	sleep := r.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}

	totalCtx, cancel := context.WithTimeout(ctx, r.Budget.Total)
	defer cancel()

	for _, a := range actions {
		if a == ActionNone {
			continue
		}
		if err := r.runAction(totalCtx, a, sleep); err != nil {
			return fmt.Errorf("action %q: %w", a, err)
		}
	}
	return nil
}

func (r Reconciler) runAction(ctx context.Context, a Action, sleep func(time.Duration)) error {
	var lastErr error
	for attempt := 1; attempt <= r.Budget.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("budget exhausted before attempt %d: %w", attempt, err)
		}

		attemptCtx, cancel := context.WithTimeout(ctx, r.Budget.PerAttempt)
		err := r.Exec.Execute(attemptCtx, a)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err

		if attempt < r.Budget.MaxAttempts {
			sleep(backoff(attempt))
		}
	}
	return fmt.Errorf("failed after %d attempts: %w", r.Budget.MaxAttempts, lastErr)
}

// backoff is a simple linear backoff between attempts.
func backoff(attempt int) time.Duration {
	return time.Duration(attempt) * time.Second
}
