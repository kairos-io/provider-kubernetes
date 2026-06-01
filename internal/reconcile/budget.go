// Package reconcile holds the desired-vs-actual planning logic (ADR-4) and the
// execution budget that guarantees every external action is bounded so the
// provider can never hang and block later Kairos boot stages (issue #4099-1).
package reconcile

import "time"

// Budget bounds external actions. It is the single place that guarantees
// "nothing runs unbounded": callers derive a per-attempt context deadline from
// PerAttempt, cap retries at MaxAttempts, and enforce Total as the hard ceiling
// across all attempts of one operation.
type Budget struct {
	// PerAttempt is the deadline for a single external action.
	PerAttempt time.Duration
	// MaxAttempts is the maximum number of attempts before failing loud.
	MaxAttempts int
	// Total is the hard ceiling across all attempts of one operation.
	Total time.Duration
}

// DefaultBudget returns conservative bounded defaults. These exist so that an
// unbounded operation is impossible by construction; tune per-action as needed.
func DefaultBudget() Budget {
	return Budget{
		PerAttempt:  2 * time.Minute,
		MaxAttempts: 3,
		Total:       8 * time.Minute,
	}
}

// Valid reports whether the budget is internally consistent and bounded.
func (b Budget) Valid() bool {
	return b.PerAttempt > 0 && b.MaxAttempts > 0 && b.Total >= b.PerAttempt
}
