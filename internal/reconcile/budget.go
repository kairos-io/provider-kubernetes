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

// DefaultBudget returns the bounded default applied to every non-upgrade action
// (run-init, run-join, wait-for-control-plane). Sizing reflects two facts:
//
//   - PerAttempt=6m: the production image bundles the kubeadm/kubelet binaries but
//     does NOT pre-pull the control-plane container images (etcd, apiserver,
//     controller-manager, scheduler), so a real first-boot `kubeadm init` PULLS
//     those images and then waits for them to become healthy. On a slow node this
//     can run 3-6 minutes; the old 2m deadline SIGKILL-ed kubeadm via the expired
//     context. 6m gives a cold init genuine headroom.
//   - Total=8m (NOT PerAttempt*MaxAttempts): Total is the hard wall-clock ceiling
//     and is deliberately the lever that bounds FAILURE-detection latency. The
//     worst case is a node pointed at an unreachable control-plane endpoint
//     (the [wait-for-control-plane, run-join] plan): wait-for-control-plane polls
//     until its context expires, so its surfacing time is driven by the budget.
//     With Total=8m the first 6m attempt runs in full, the second attempt starts
//     but is capped by the remaining 2m, and the failure surfaces at ~8m -- the
//     same ceiling the old 3x2m budget produced, NOT the ~12m a 2x6m budget would.
//     This preserves fail-fast responsiveness (design principle 4 / #4099-1) while
//     still tolerating a transient blip via the partial second attempt.
//
// MaxAttempts=2 (down from 3): with a 6m PerAttempt window each attempt already
// absorbs short transient failures, so the second attempt is a safety net rather
// than a retry loop. Note Total < PerAttempt*MaxAttempts by design: Total wins, so
// the second attempt may be truncated. This is intentional -- the ceiling on how
// long a failing node may block later Kairos boot stages is the priority.
//
// KNOWN LIMITATION (follow-up: per-action budgets): a single global PerAttempt
// conflates a slow legitimate waiter (cold init) with a pure reachability probe
// (wait-for-control-plane against a bad endpoint), which ideally fails in ~60-90s.
// Splitting these requires the Reconciler to select a budget per Action; that
// wiring is a separate slice. Until then Total=8m caps the bad-endpoint case.
func DefaultBudget() Budget {
	return Budget{
		PerAttempt:  6 * time.Minute,
		MaxAttempts: 2,
		Total:       8 * time.Minute,
	}
}

// UpgradeBudget returns the bounded budget for upgrade actions (ADR-12). Upgrades
// are slower (etcd + static-pod restarts) and `kubeadm upgrade apply` is a
// destructive control-plane mutation that must not be blindly retried, so the
// per-attempt deadline is larger and attempts are few. Still hard-bounded by Total
// so an upgrade can never hang (#4099-1).
func UpgradeBudget() Budget {
	return Budget{
		PerAttempt:  10 * time.Minute,
		MaxAttempts: 2,
		Total:       20 * time.Minute,
	}
}

// Valid reports whether the budget is internally consistent and bounded.
func (b Budget) Valid() bool {
	return b.PerAttempt > 0 && b.MaxAttempts > 0 && b.Total >= b.PerAttempt
}
