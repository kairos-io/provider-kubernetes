//go:build e2e && nightly

package e2e

// nightly_failure_test.go is the Tier-2 (ADR-13 E4) pre-membership FAILURE-STATUS
// scenario. It is the cheapest nightly scenario by design (no real cluster comes
// up), and proves the #4099-1 never-hang guarantee end to end: a worker pointed at
// an UNREACHABLE control-plane endpoint must FAIL LOUD and SURFACE a structured,
// machine-readable status within the bounded budget ceiling -- it must NOT hang and
// block later Kairos boot stages.
//
// Gating: `//go:build e2e && nightly`. Compiled only under `-tags "e2e nightly"`
// (the nightly job), NOT under `-tags e2e` alone (the per-PR job). See
// nightly_doc_test.go for the gating contract.
//
// What it proves (the pre-membership regime, ADR-4-S Layer 1):
//   - The Plan for an Uninitialized worker against an unreachable endpoint is
//     [wait-for-control-plane, run-join]; wait-for-control-plane polls CPReachable
//     until its context expires, so the surfacing time is driven by the reconcile
//     Budget (DefaultBudget Total=8m), NOT by an unbounded loop.
//   - The reconcile exits non-zero (loud failure surfaced to the exit code).
//   - status.yaml (the ALWAYS-available Layer-1 channel that works pre-membership)
//     records phase=Failed, outcome=failure, membership=uninitialized, and the
//     CLOSED-enum reason the provider derives for an exhausted reachability wait:
//     ReasonControlPlaneUnreachable ("ControlPlaneUnreachable"), terminal=false
//     (a transient/retryable verdict, not an ErrTerminal one).
//   - It surfaces WITHIN the budget ceiling: writeClusterAndReconcile bounds the
//     docker exec at reconcileTimeout (14m), above the provider's DefaultBudget
//     Total (8m), so the exec never masks the provider's own bounded failure. If
//     the provider hung, the test would fail by exceeding the wall-clock assertion
//     below rather than passing.

import (
	"strings"
	"testing"
	"time"

	"github.com/kairos-io/kairos-sdk/clusterplugin"
)

// unreachableEndpoint is an address in the RFC 5735 192.0.0.0/8-adjacent
// 10.255.255.0/24 range that is not routed inside the docker bridge, so a TCP
// dial to it neither connects nor is RST-ed quickly -- exactly the "dead
// control-plane endpoint" the #4099-1 hang guard exists for. Port 6443 is the
// kube-apiserver port the provider probes.
const unreachableEndpoint = "10.255.255.1:6443"

// budgetCeiling mirrors the provider's DefaultBudget().Total (reconcile/budget.go).
// The pre-membership failure must surface at or below this, plus a margin for the
// final run-join attempt's setup and the docker-exec round trip. We assert the
// failure surfaces well under reconcileTimeout (14m) -- i.e. the provider's OWN
// bound fired, not the harness backstop.
const budgetCeiling = 8 * time.Minute

// surfacingCeiling is the wall-clock bound we hold the reconcile to. It is the
// provider Total (8m) plus generous slack for the truncated second attempt's
// teardown and the docker exec overhead, but still strictly below reconcileTimeout
// so a PASS proves the provider self-bounded rather than the 14m exec backstop
// catching a hang.
const surfacingCeiling = budgetCeiling + 3*time.Minute

// TestNightlyPreMembershipFailureStatus (ADR-13 Tier-2): a worker reconcile against
// an unreachable endpoint fails loud, fast (within budget), and writes a structured
// failure status -- the pre-membership regime of ADR-4-S Layer 1, and the #4099-1
// never-hang proof.
func TestNightlyPreMembershipFailureStatus(t *testing.T) {
	nc := startNode(t, uniqueName("fail"))

	// Build a worker Cluster whose ControlPlaneHost is unreachable, with a
	// well-formed-but-bogus joinConfiguration that mirrors the SHAPE mint-join
	// emits: a syntactically valid bootstrap token (the `[a-z0-9]{6}.[a-z0-9]{16}`
	// kubeadm format) and a sha256: CA hash. This is deliberate: the provider must
	// build a valid join config and actually ATTEMPT the join (so the failure is a
	// runtime reachability failure surfaced via the budget), NOT reject the config
	// up front (which would be a ConfigInvalid terminal verdict, a different path).
	//
	// The token/hash are pure placeholders; no real cluster exists. CA pinning is
	// present (caCertHashes), so UnsafeSkipCAVerification is never engaged.
	cluster := clusterplugin.Cluster{
		Role:             clusterplugin.Role(clusterplugin.RoleWorker),
		ClusterToken:     randomToken(t),
		ControlPlaneHost: unreachableEndpoint,
		ProviderOptions:  map[string]string{"cluster_root_path": "/"},
		Options: strings.Join([]string{
			"joinConfiguration:",
			"  discovery:",
			"    bootstrapToken:",
			"      token: abcdef.0123456789abcdef",
			"      apiServerEndpoint: " + unreachableEndpoint,
			"      caCertHashes:",
			"      - sha256:0000000000000000000000000000000000000000000000000000000000000000",
		}, "\n") + "\n",
	}

	// Run reconcile and TIME it. The provider must surface the failure within its
	// own budget ceiling, never reaching the 14m docker-exec backstop.
	start := time.Now()
	out, err := writeClusterAndReconcile(t, nc, cluster)
	elapsed := time.Since(start)
	t.Logf("reconcile (unreachable endpoint) exited after %s with err=%v\n--- output ---\n%s", elapsed, err, out)

	// 1. The reconcile MUST exit non-zero: a failed pre-membership join is a loud
	//    failure, surfaced to the exit code the yip stage observes.
	if err == nil {
		t.Fatalf("reconcile against unreachable endpoint should have exited non-zero, but succeeded\n--- output ---\n%s", out)
	}

	// 2. It MUST surface within the budget ceiling (never hang). If the provider
	//    looped unbounded, the docker exec would run to reconcileTimeout (14m) and
	//    elapsed would exceed surfacingCeiling. A PASS here proves the provider's
	//    own bounded budget fired.
	if elapsed > surfacingCeiling {
		t.Fatalf("reconcile took %s to surface a dead-endpoint failure; want <= %s (provider DefaultBudget Total=%s). "+
			"This indicates the provider hung rather than failing fast (#4099-1).",
			elapsed, surfacingCeiling, budgetCeiling)
	}

	// 3. status.yaml (Layer-1, the only channel available pre-membership) records a
	//    structured failure. Assert typed behavior, not log strings (principle 6).
	st := readStatus(t, nc)

	if st.Phase != "Failed" {
		t.Errorf("status phase = %q, want Failed (full status: %+v)", st.Phase, st)
	}
	if st.Outcome != "failure" {
		t.Errorf("status outcome = %q, want failure (full status: %+v)", st.Outcome, st)
	}
	// The node never reached the API, so it is still uninitialized (no membership
	// transition occurred). This distinguishes pre-membership failure from a
	// post-join failure.
	if st.Membership != "uninitialized" {
		t.Errorf("status membership = %q, want uninitialized (no join completed; full status: %+v)", st.Membership, st)
	}
	if st.Role != "worker" {
		t.Errorf("status role = %q, want worker", st.Role)
	}

	// 4. Reason is the CLOSED-enum token the provider sets when the bounded
	//    wait-for-control-plane reachability budget is exhausted against a dead
	//    endpoint: ReasonControlPlaneUnreachable (internal/status deriveReason maps
	//    {ActionWaitForControlPlane|ActionRunJoin, BudgetExhausted} ->
	//    "ControlPlaneUnreachable"). The lastAction the plan reaches is
	//    wait-for-control-plane (it never advances to run-join because the endpoint
	//    never becomes reachable).
	const wantReason = "ControlPlaneUnreachable"
	if st.Reason != wantReason {
		t.Errorf("status reason = %q, want %q (the budget-exhausted reachability reason; full status: %+v)",
			st.Reason, wantReason, st)
	}

	// 5. The failure is NON-terminal: an unreachable endpoint is a transient/
	//    retryable condition (the CP may come up later), not an ErrTerminal verdict.
	if st.Terminal {
		t.Errorf("status terminal = true, want false (ControlPlaneUnreachable is a retryable, non-terminal failure; full status: %+v)", st)
	}

	// 6. lastAction reflects the action in flight when the budget exhausted. For the
	//    [wait-for-control-plane, run-join] plan against a dead endpoint, the wait
	//    never completes, so lastAction is the wait.
	if st.LastAction != string(reconcileWaitForControlPlane) {
		t.Errorf("status lastAction = %q, want %q (the bounded reachability wait that exhausted)",
			st.LastAction, reconcileWaitForControlPlane)
	}
}

// reconcileWaitForControlPlane is a local copy of the
// reconcile.ActionWaitForControlPlane string value ("wait-for-control-plane").
// Kept as a local constant so the e2e package depends only on the public binary
// contract (the serialized status.yaml lastAction field), not on internal/* --
// the same coupling discipline provider.go uses for the path constants. If the
// action's string value changes in internal/reconcile, this must change too; that
// coupling is intentional (we assert the real wire contract).
const reconcileWaitForControlPlane = "wait-for-control-plane"
