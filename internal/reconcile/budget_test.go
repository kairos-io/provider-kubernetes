package reconcile

import (
	"context"
	"testing"
	"time"
)

// TestDefaultBudgetTotalIsTheBindingCeiling locks the #4099-1 never-hang
// invariant: Total is the hard wall-clock ceiling and is deliberately LESS than
// PerAttempt*MaxAttempts, so the worst-case failure-detection time is bounded by
// Total (8m), not the naive product (with 6m/2 that product is 12m). If someone
// bumps PerAttempt/MaxAttempts without keeping Total the binding cap, this fails.
func TestDefaultBudgetTotalIsTheBindingCeiling(t *testing.T) {
	b := DefaultBudget()
	if b.PerAttempt <= 0 || b.MaxAttempts <= 0 || b.Total <= 0 {
		t.Fatalf("DefaultBudget has non-positive fields: %+v", b)
	}
	product := b.PerAttempt * time.Duration(b.MaxAttempts)
	if b.Total >= product {
		t.Fatalf("Total (%s) must be < PerAttempt*MaxAttempts (%s) so Total is the binding never-hang ceiling", b.Total, product)
	}
	// Sanity upper bound: a failing reconcile must surface well within a boot's
	// patience; 10m is a generous ceiling for the current 8m value.
	if b.Total > 10*time.Minute {
		t.Fatalf("Total (%s) exceeds the 10m never-hang sanity bound", b.Total)
	}
}

// blockingExec simulates a perpetually-stuck action (e.g. wait-for-control-plane
// against an unreachable endpoint): it blocks until its context deadline fires.
type blockingExec struct{ calls int }

func (b *blockingExec) Execute(ctx context.Context, _ Action) error {
	b.calls++
	<-ctx.Done()
	return ctx.Err()
}

// TestReconcilerBoundedByTotalNotProduct proves the run is bounded by Budget.Total
// even when every attempt hangs to its deadline -- the failure surfaces at ~Total,
// NOT PerAttempt*MaxAttempts. This is the runtime guarantee behind #4099-1.
func TestReconcilerBoundedByTotalNotProduct(t *testing.T) {
	exec := &blockingExec{}
	r := Reconciler{
		// product = 100ms*5 = 500ms; Total caps at 250ms.
		Budget: Budget{PerAttempt: 100 * time.Millisecond, MaxAttempts: 5, Total: 250 * time.Millisecond},
		Exec:   exec,
		Sleep:  func(time.Duration) {}, // skip backoff; the ctx deadlines do the bounding
	}

	start := time.Now()
	err := r.Run(context.Background(), []Action{ActionWaitForControlPlane})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a loud failure when the action never succeeds")
	}
	// Bounded by Total (250ms) plus slack, and clearly under the 500ms product.
	if elapsed >= 450*time.Millisecond {
		t.Fatalf("run took %s; must be bounded by ~Total (250ms), not PerAttempt*MaxAttempts (500ms)", elapsed)
	}
}
