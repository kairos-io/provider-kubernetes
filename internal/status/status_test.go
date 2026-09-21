package status

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	sigsyaml "sigs.k8s.io/yaml"

	"github.com/kairos-io/provider-kubernetes/internal/reconcile"
	"github.com/kairos-io/provider-kubernetes/internal/reconcile/actualstate"
)

const testNow = "2026-06-03T12:00:00Z"
const testBootID = "abc-123"
const testVersion = "v0.2.0"

func baseParams() BuildParams {
	return BuildParams{
		Role:       actualstate.RoleWorker,
		Membership: actualstate.Uninitialized,
		LastAction: reconcile.ActionNone,
		Now:        testNow,
		BootID:     testBootID,
		Version:    testVersion,
	}
}

// TestBuildStatusSuccess verifies the converged-success path.
func TestBuildStatusSuccess(t *testing.T) {
	p := baseParams()
	p.Role = actualstate.RoleInit
	p.Membership = actualstate.Initialized
	p.LastAction = reconcile.ActionRunInit
	p.Err = nil

	s := BuildStatus(p)

	if s.APIVersion != APIVersion {
		t.Errorf("apiVersion = %q, want %q", s.APIVersion, APIVersion)
	}
	if s.Phase != PhaseConverged {
		t.Errorf("phase = %q, want Converged", s.Phase)
	}
	if s.Outcome != OutcomeSuccess {
		t.Errorf("outcome = %q, want success", s.Outcome)
	}
	if s.Reason != ReasonNone {
		t.Errorf("reason = %q, want empty on success", s.Reason)
	}
	if s.Terminal {
		t.Error("terminal must be false on success")
	}
	if s.Budget.Attempts != 0 || s.Budget.MaxAttempts != 0 {
		t.Errorf("budget = %+v, want {0,0} on success", s.Budget)
	}
	if s.UpdatedAt != testNow {
		t.Errorf("updatedAt = %q, want %q", s.UpdatedAt, testNow)
	}
	if s.BootID != testBootID {
		t.Errorf("bootID = %q, want %q", s.BootID, testBootID)
	}
	if s.Version != testVersion {
		t.Errorf("version = %q, want %q", s.Version, testVersion)
	}
	if s.Role != string(actualstate.RoleInit) {
		t.Errorf("role = %q, want init", s.Role)
	}
	if s.Membership != string(actualstate.Initialized) {
		t.Errorf("membership = %q, want initialized", s.Membership)
	}
}

// TestBuildStatusTerminalCases covers all terminal reason derivations.
func TestBuildStatusTerminalCases(t *testing.T) {
	termErr := fmt.Errorf("%w: refused", reconcile.ErrTerminal)

	tests := []struct {
		name       string
		action     reconcile.Action
		err        error
		wantReason Reason
		wantPhase  Phase
		wantTerm   bool
	}{
		{
			name:       "RefuseInit terminal",
			action:     reconcile.ActionRefuseInit,
			err:        termErr,
			wantReason: ReasonInitRefused,
			wantPhase:  PhaseFailed,
			wantTerm:   true,
		},
		{
			name:       "RefuseUpgrade terminal",
			action:     reconcile.ActionRefuseUpgrade,
			err:        termErr,
			wantReason: ReasonUpgradeRefused,
			wantPhase:  PhaseFailed,
			wantTerm:   true,
		},
		{
			name:       "RunJoin terminal",
			action:     reconcile.ActionRunJoin,
			err:        termErr,
			wantReason: ReasonJoinTimeout,
			wantPhase:  PhaseFailed,
			wantTerm:   true,
		},
		{
			name:       "other action terminal -> KubeadmError",
			action:     reconcile.ActionRunInit,
			err:        termErr,
			wantReason: ReasonKubeadmError,
			wantPhase:  PhaseFailed,
			wantTerm:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := baseParams()
			p.LastAction = tc.action
			p.Err = tc.err
			s := BuildStatus(p)
			if s.Phase != tc.wantPhase {
				t.Errorf("phase = %q, want %q", s.Phase, tc.wantPhase)
			}
			if s.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", s.Reason, tc.wantReason)
			}
			if s.Terminal != tc.wantTerm {
				t.Errorf("terminal = %v, want %v", s.Terminal, tc.wantTerm)
			}
			if s.Outcome != OutcomeFailure {
				t.Errorf("outcome = %q, want failure", s.Outcome)
			}
		})
	}
}

// TestBuildStatusBudgetExhausted covers budget-exhaustion reason derivations.
func TestBuildStatusBudgetExhausted(t *testing.T) {
	baseErr := errors.New("timed out")

	tests := []struct {
		name       string
		action     reconcile.Action
		wantReason Reason
	}{
		{
			name:       "WaitForControlPlane exhausted",
			action:     reconcile.ActionWaitForControlPlane,
			wantReason: ReasonControlPlaneUnreachable,
		},
		{
			name:       "RunJoin exhausted",
			action:     reconcile.ActionRunJoin,
			wantReason: ReasonControlPlaneUnreachable,
		},
		{
			name:       "WaitForClusterUpgrade exhausted",
			action:     reconcile.ActionWaitForClusterUpgrade,
			wantReason: ReasonJoinTimeout,
		},
		{
			name:       "UpgradeApply exhausted -> BudgetExhausted",
			action:     reconcile.ActionUpgradeApply,
			wantReason: ReasonBudgetExhausted,
		},
		{
			name:       "RunInit exhausted -> BudgetExhausted",
			action:     reconcile.ActionRunInit,
			wantReason: ReasonBudgetExhausted,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := baseParams()
			p.LastAction = tc.action
			p.Err = baseErr
			p.Result = reconcile.RunResult{
				Attempts:        3,
				MaxAttempts:     3,
				BudgetExhausted: true,
			}
			s := BuildStatus(p)
			if s.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", s.Reason, tc.wantReason)
			}
			if s.Phase != PhaseFailed {
				t.Errorf("phase = %q, want Failed", s.Phase)
			}
			if s.Terminal {
				t.Error("budget-exhausted must not be terminal (retryable on next boot)")
			}
			if s.Budget.Attempts != 3 || s.Budget.MaxAttempts != 3 {
				t.Errorf("budget = %+v, want {3,3}", s.Budget)
			}
		})
	}
}

// TestBuildStatusNonExhaustedFailure covers plain (non-terminal, non-exhausted) errors.
func TestBuildStatusNonExhaustedFailure(t *testing.T) {
	tests := []struct {
		name       string
		action     reconcile.Action
		wantReason Reason
	}{
		{
			name:       "WaitForControlPlane plain error",
			action:     reconcile.ActionWaitForControlPlane,
			wantReason: ReasonControlPlaneUnreachable,
		},
		{
			name:       "RunJoin plain error",
			action:     reconcile.ActionRunJoin,
			wantReason: ReasonJoinTimeout,
		},
		{
			name:       "RunInit plain error -> KubeadmError",
			action:     reconcile.ActionRunInit,
			wantReason: ReasonKubeadmError,
		},
		{
			name:       "UpgradeApply plain error -> KubeadmError",
			action:     reconcile.ActionUpgradeApply,
			wantReason: ReasonKubeadmError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := baseParams()
			p.LastAction = tc.action
			p.Err = errors.New("something failed")
			s := BuildStatus(p)
			if s.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", s.Reason, tc.wantReason)
			}
			if s.Phase != PhaseFailed {
				t.Errorf("phase = %q, want Failed", s.Phase)
			}
		})
	}
}

// TestBuildStatusReset verifies the reset success and failure paths.
func TestBuildStatusReset(t *testing.T) {
	t.Run("reset success", func(t *testing.T) {
		p := baseParams()
		p.IsReset = true
		s := BuildStatus(p)
		if s.Phase != PhaseReset {
			t.Errorf("phase = %q, want Reset", s.Phase)
		}
		if s.Outcome != OutcomeSuccess {
			t.Errorf("outcome = %q, want success", s.Outcome)
		}
		if s.Reason != ReasonResetOK {
			t.Errorf("reason = %q, want ResetOK", s.Reason)
		}
		if !s.Terminal {
			t.Error("reset must be terminal (node is being torn down)")
		}
	})

	t.Run("reset failure", func(t *testing.T) {
		p := baseParams()
		p.IsReset = true
		p.ResetErr = errors.New("kubeadm reset exited 1")
		s := BuildStatus(p)
		if s.Phase != PhaseReset {
			t.Errorf("phase = %q, want Reset", s.Phase)
		}
		if s.Outcome != OutcomeFailure {
			t.Errorf("outcome = %q, want failure", s.Outcome)
		}
		if s.Reason != ReasonResetFailed {
			t.Errorf("reason = %q, want ResetFailed", s.Reason)
		}
		if !s.Terminal {
			t.Error("reset failure must be terminal")
		}
	})
}

// TestBuildStatusConfigInvalid verifies the early-exit config-rejection path maps
// to reason=ConfigInvalid (terminal), not a generic KubeadmError.
func TestBuildStatusConfigInvalid(t *testing.T) {
	p := baseParams()
	p.LastAction = reconcile.ActionNone
	p.ConfigInvalid = true
	p.Err = errors.New("cluster_token too short")

	s := BuildStatus(p)

	if s.Phase != PhaseFailed {
		t.Errorf("phase = %q, want Failed", s.Phase)
	}
	if s.Reason != ReasonConfigInvalid {
		t.Errorf("reason = %q, want ConfigInvalid", s.Reason)
	}
	if s.Outcome != OutcomeFailure {
		t.Errorf("outcome = %q, want failure", s.Outcome)
	}
	if !s.Terminal {
		t.Error("config-invalid must be terminal (will not converge without operator action)")
	}
	if !contains(s.Message, "invalid configuration") {
		t.Errorf("message = %q, want an invalid-configuration summary", s.Message)
	}
}

// TestBuildStatusConfigInvalidWithoutErrIsIgnored verifies the flag is inert
// when Err is nil (defensive: a stray flag must not turn success into failure).
func TestBuildStatusConfigInvalidWithoutErrIsIgnored(t *testing.T) {
	p := baseParams()
	p.ConfigInvalid = true
	p.Err = nil

	s := BuildStatus(p)
	if s.Phase != PhaseConverged {
		t.Errorf("phase = %q, want Converged (ConfigInvalid must require a non-nil Err)", s.Phase)
	}
}

// TestBuildStatusDegraded is D-2: an established member (Initialized or
// Joined) whose reconcile.Plan verdict was degraded must report
// PhaseDegraded/OutcomeFailure/ReasonKubeletUnhealthy, non-terminal, and must
// NEVER be reported as PhaseConverged.
func TestBuildStatusDegraded(t *testing.T) {
	tests := []struct {
		name       string
		membership actualstate.Membership
	}{
		{name: "initialized member", membership: actualstate.Initialized},
		{name: "joined member", membership: actualstate.Joined},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := baseParams()
			p.Membership = tc.membership
			p.LastAction = reconcile.ActionNone
			p.Err = nil
			p.Degraded = true

			s := BuildStatus(p)

			if s.Phase == PhaseConverged {
				t.Fatalf("phase = %q, must never be Converged when Degraded is set", s.Phase)
			}
			if s.Phase != PhaseDegraded {
				t.Errorf("phase = %q, want Degraded", s.Phase)
			}
			if s.Outcome != OutcomeFailure {
				t.Errorf("outcome = %q, want failure (a real problem, even though non-terminal)", s.Outcome)
			}
			if s.Reason != ReasonKubeletUnhealthy {
				t.Errorf("reason = %q, want KubeletUnhealthy", s.Reason)
			}
			if s.Terminal {
				t.Error("degraded must be non-terminal: a later boot or an explicit reset may still converge")
			}
			if s.Membership != string(tc.membership) {
				t.Errorf("membership = %q, want %q (probed value kept: no action ran)", s.Membership, tc.membership)
			}
			if s.Budget.Attempts != 0 || s.Budget.MaxAttempts != 0 {
				t.Errorf("budget = %+v, want {0,0}", s.Budget)
			}
		})
	}
}

// TestBuildStatusDegradedFalseIsInertWithoutErr is the converse of
// TestBuildStatusDegraded: Degraded defaults to false (its zero value), so
// every existing success-path caller that does not set it is unaffected.
func TestBuildStatusDegradedFalseIsInertWithoutErr(t *testing.T) {
	p := baseParams()
	p.Membership = actualstate.Initialized
	p.LastAction = reconcile.ActionNone
	p.Err = nil
	// p.Degraded left at its zero value (false).

	s := BuildStatus(p)
	if s.Phase != PhaseConverged {
		t.Errorf("phase = %q, want Converged (Degraded defaults to false)", s.Phase)
	}
}

// TestBuildStatusDegradedIgnoredWhenErrSet: Degraded must never override a
// real failure's phase/reason -- it is only consulted on the Err == nil path.
func TestBuildStatusDegradedIgnoredWhenErrSet(t *testing.T) {
	p := baseParams()
	p.LastAction = reconcile.ActionRunJoin
	p.Err = errors.New("connection refused")
	p.Degraded = true // must be inert here

	s := BuildStatus(p)
	if s.Phase != PhaseFailed {
		t.Errorf("phase = %q, want Failed (Degraded must not mask a real error)", s.Phase)
	}
	if s.Reason != ReasonJoinTimeout {
		t.Errorf("reason = %q, want JoinTimeout (deriveReason's normal mapping, untouched by Degraded)", s.Reason)
	}
}

// TestBuildStatusIsDeterministic verifies BuildStatus is pure: same inputs ->
// identical outputs regardless of how many times it is called.
func TestBuildStatusIsDeterministic(t *testing.T) {
	p := baseParams()
	p.LastAction = reconcile.ActionRunJoin
	p.Err = errors.New("connection refused")

	s1 := BuildStatus(p)
	s2 := BuildStatus(p)

	if s1 != s2 {
		t.Errorf("BuildStatus is not deterministic:\nfirst:  %+v\nsecond: %+v", s1, s2)
	}
}

// TestSanitizeRemovesBootstrapToken checks that bootstrap token patterns are redacted.
func TestSanitizeRemovesBootstrapToken(t *testing.T) {
	input := "join failed: token abc123.1234567890abcdef was rejected"
	out := sanitize(input)
	if out == input {
		t.Error("sanitize must redact bootstrap tokens")
	}
	if contains(out, "abc123.1234567890abcdef") {
		t.Error("token must not appear in sanitized output")
	}
}

// TestSanitizeRemovesPEM checks that PEM blocks are redacted.
func TestSanitizeRemovesPEM(t *testing.T) {
	input := "cert: -----BEGIN CERTIFICATE-----\nMIIABC\n-----END CERTIFICATE-----"
	out := sanitize(input)
	if contains(out, "MIIABC") {
		t.Errorf("PEM material must be redacted, got: %q", out)
	}
}

// TestSanitizeLongSecretIsRedacted checks that long hex/base64 strings are redacted.
func TestSanitizeLongSecretIsRedacted(t *testing.T) {
	// 64 hex chars -- looks like a SHA-256 hash or raw key material.
	long := "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	input := "hash=" + long
	out := sanitize(input)
	if contains(out, long) {
		t.Errorf("long secret must be redacted, got: %q", out)
	}
}

// TestSanitizeTruncatesLongMessage checks the 256-char hard limit.
func TestSanitizeTruncatesLongMessage(t *testing.T) {
	long := fmt.Sprintf("%-300s", "x") // 300 spaces
	out := sanitize(long)
	if len(out) > 256 {
		t.Errorf("message not truncated: len=%d", len(out))
	}
}

// TestSanitizeKeepsShortSafeMessage checks that safe text is preserved.
func TestSanitizeKeepsShortSafeMessage(t *testing.T) {
	input := "action run-join: connection refused"
	out := sanitize(input)
	if !contains(out, "run-join") {
		t.Errorf("sanitize must not redact safe text, got: %q", out)
	}
}

// TestAllReasonCodesAreCovered asserts that every action that can fail maps to
// one of the defined Reason constants (closed enumeration guard).
func TestAllReasonCodesAreCovered(t *testing.T) {
	// All actions that can legitimately fail (exclude ActionNone which is skipped).
	failableActions := []reconcile.Action{
		reconcile.ActionRunInit,
		reconcile.ActionWaitForControlPlane,
		reconcile.ActionRunJoin,
		reconcile.ActionRefuseInit,
		reconcile.ActionUpgradeApply,
		reconcile.ActionWaitForClusterUpgrade,
		reconcile.ActionUpgradeNode,
		reconcile.ActionRefuseUpgrade,
		reconcile.ActionRepairKubeletConfig,
	}
	validReasons := map[Reason]bool{
		ReasonControlPlaneUnreachable: true,
		ReasonJoinTimeout:             true,
		ReasonInitRefused:             true,
		ReasonUpgradeRefused:          true,
		ReasonBudgetExhausted:         true,
		ReasonKubeadmError:            true,
		ReasonConfigInvalid:           true,
		ReasonResetFailed:             true,
		ReasonResetOK:                 true,
	}

	for _, a := range failableActions {
		p := baseParams()
		p.LastAction = a
		p.Err = errors.New("simulated failure")
		s := BuildStatus(p)
		if s.Reason == ReasonNone {
			t.Errorf("action %q with error must produce a non-empty reason", a)
		}
		if !validReasons[s.Reason] {
			t.Errorf("action %q produced unknown reason %q", a, s.Reason)
		}
	}
}

// TestMarshalKeysAreCamelCase is the regression guard for ADR-4-S schema fidelity.
// sigs.k8s.io/yaml marshals via encoding/json, so absent json tags the keys would
// be PascalCase Go field names instead of the documented camelCase. This test
// marshals a representative Status through the SAME path FileSink uses and asserts:
//   - every documented top-level key appears in camelCase
//   - no PascalCase Go field name (e.g. "Phase:", "APIVersion:") appears as a
//     YAML key (checked per-line to avoid false positives from substrings, e.g.
//     "Version:" inside "apiVersion:" or "Attempts:" inside "maxAttempts:")
//   - nested budget keys are camelCase
func TestMarshalKeysAreCamelCase(t *testing.T) {
	s := BuildStatus(BuildParams{
		Role:       actualstate.RoleWorker,
		Membership: actualstate.Uninitialized,
		LastAction: reconcile.ActionWaitForControlPlane,
		Err:        errors.New("timed out"),
		Result: reconcile.RunResult{
			Attempts:        3,
			MaxAttempts:     3,
			BudgetExhausted: true,
		},
		Now:     testNow,
		BootID:  testBootID,
		Version: testVersion,
	})

	data, err := sigsyaml.Marshal(s)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	yamlStr := string(data)

	// Every documented top-level camelCase key must be present. Top-level keys
	// appear at column 0 (no leading whitespace), so we check for "\nKEY:" or
	// the key at the very start of the document.
	wantTopLevelKeys := []string{
		"apiVersion:",
		"phase:",
		"role:",
		"membership:",
		"outcome:",
		"reason:",
		"terminal:",
		"lastAction:",
		"message:",
		"budget:",
		"updatedAt:",
		"bootID:",
		"version:",
	}
	for _, k := range wantTopLevelKeys {
		if !strings.Contains(yamlStr, "\n"+k) && !strings.HasPrefix(yamlStr, k) {
			t.Errorf("marshaled YAML missing expected top-level key %q\nfull output:\n%s", k, yamlStr)
		}
	}

	// Nested budget keys must be camelCase (indented under budget:).
	wantBudgetKeys := []string{"attempts:", "maxAttempts:"}
	for _, k := range wantBudgetKeys {
		if !strings.Contains(yamlStr, k) {
			t.Errorf("marshaled YAML missing budget key %q\nfull output:\n%s", k, yamlStr)
		}
	}

	// No PascalCase Go field names may appear as line-leading YAML keys. We check
	// each line individually: strip leading whitespace (indentation) and test whether
	// the line starts with the forbidden PascalCase token followed by ':'. This avoids
	// false positives where a forbidden name is a substring of a valid camelCase key
	// (e.g. "Version:" is a suffix of "apiVersion:"; "Attempts:" is a suffix of
	// "maxAttempts:").
	forbiddenLineKeys := []string{
		"APIVersion:",
		"Phase:",
		"Role:",
		"Membership:",
		"Outcome:",
		"Reason:",
		"Terminal:",
		"LastAction:",
		"Message:",
		"Budget:",
		"UpdatedAt:",
		"BootID:",
		"Version:",
		"Attempts:",
		"MaxAttempts:",
	}
	for _, line := range strings.Split(yamlStr, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		for _, k := range forbiddenLineKeys {
			if strings.HasPrefix(trimmed, k) {
				t.Errorf("marshaled YAML line %q starts with PascalCase key %q -- json tags missing or wrong\nfull output:\n%s", line, k, yamlStr)
			}
		}
	}
}

// TestBuildStatusConvergedMembershipAccuracy verifies Bug 2 fix: on a Converged
// outcome after a membership-establishing action, the reported membership reflects
// the post-reconcile reality, not the stale pre-action probe.
func TestBuildStatusConvergedMembershipAccuracy(t *testing.T) {
	tests := []struct {
		name             string
		action           reconcile.Action
		probedMembership actualstate.Membership
		wantMembership   string
	}{
		{
			// init completed: node is now initialized, not stale uninitialized.
			name:             "RunInit -> Initialized",
			action:           reconcile.ActionRunInit,
			probedMembership: actualstate.Uninitialized,
			wantMembership:   string(actualstate.Initialized),
		},
		{
			// join completed: node is now joined, not stale uninitialized.
			name:             "RunJoin -> Joined",
			action:           reconcile.ActionRunJoin,
			probedMembership: actualstate.Uninitialized,
			wantMembership:   string(actualstate.Joined),
		},
		{
			// no-op fast path (already converged): keep probed membership as-is.
			name:             "ActionNone -> keep probed membership",
			action:           reconcile.ActionNone,
			probedMembership: actualstate.Initialized,
			wantMembership:   string(actualstate.Initialized),
		},
		{
			// non-establishing action: keep probed membership.
			name:             "UpgradeApply -> keep probed membership",
			action:           reconcile.ActionUpgradeApply,
			probedMembership: actualstate.Joined,
			wantMembership:   string(actualstate.Joined),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := BuildParams{
				Role:       actualstate.RoleInit,
				Membership: tc.probedMembership,
				LastAction: tc.action,
				Err:        nil, // success path
				Now:        testNow,
				BootID:     testBootID,
				Version:    testVersion,
			}
			s := BuildStatus(p)
			if s.Phase != PhaseConverged {
				t.Errorf("phase = %q, want Converged", s.Phase)
			}
			if s.Membership != tc.wantMembership {
				t.Errorf("membership = %q, want %q", s.Membership, tc.wantMembership)
			}
		})
	}
}

// contains is a helper to avoid importing strings in the test.
func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}

// TestMergeReportOnlyPreservesExistingFields is S-D3-9a's pure-function
// contract: Phase, Outcome, Membership, Role, LastAction, Budget and
// Terminal must pass through prev unchanged when existing is true; only
// Reason, Message, UpdatedAt, BootID and Version move.
func TestMergeReportOnlyPreservesExistingFields(t *testing.T) {
	prev := Status{
		APIVersion: APIVersion,
		Phase:      PhaseConverged,
		Role:       "controlplane",
		Membership: "initialized",
		Outcome:    OutcomeSuccess,
		Reason:     ReasonNone,
		Terminal:   false,
		LastAction: "run-init",
		Message:    "converged",
		Budget:     Budget{Attempts: 0, MaxAttempts: 0},
		UpdatedAt:  "2026-09-20T00:00:00Z",
		BootID:     "old-boot-id",
		Version:    "v0.1.0",
	}

	got := MergeReportOnly(prev, true, ReasonClusterConfigNotPersistent, "not persistent", "new-boot-id", "v0.2.0", testNow)

	if got.Phase != PhaseConverged {
		t.Errorf("Phase = %q, want preserved %q", got.Phase, PhaseConverged)
	}
	if got.Outcome != OutcomeSuccess {
		t.Errorf("Outcome = %q, want preserved %q", got.Outcome, OutcomeSuccess)
	}
	if got.Membership != "initialized" {
		t.Errorf("Membership = %q, want preserved", got.Membership)
	}
	if got.Role != "controlplane" {
		t.Errorf("Role = %q, want preserved", got.Role)
	}
	if got.LastAction != "run-init" {
		t.Errorf("LastAction = %q, want preserved", got.LastAction)
	}
	if got.Terminal {
		t.Error("Terminal must be preserved (false), not flipped")
	}
	if got.Reason != ReasonClusterConfigNotPersistent {
		t.Errorf("Reason = %q, want the fresh value", got.Reason)
	}
	if got.Message != "not persistent" {
		t.Errorf("Message = %q, want the fresh value", got.Message)
	}
	if got.UpdatedAt != testNow {
		t.Errorf("UpdatedAt = %q, want the fresh value %q", got.UpdatedAt, testNow)
	}
	if got.BootID != "new-boot-id" || got.Version != "v0.2.0" {
		t.Errorf("BootID/Version were not updated: %+v", got)
	}
}

// TestMergeReportOnlyNoExistingUsesReconciling covers the no-prior-record
// case: MergeReportOnly must start from PhaseReconciling, never PhaseFailed,
// when existing is false.
func TestMergeReportOnlyNoExistingUsesReconciling(t *testing.T) {
	got := MergeReportOnly(Status{}, false, ReasonClusterConfigDirWritable, "writable", testBootID, testVersion, testNow)

	if got.Phase != PhaseReconciling {
		t.Errorf("Phase = %q, want %q", got.Phase, PhaseReconciling)
	}
	if got.Phase == PhaseFailed {
		t.Error("MergeReportOnly must never fabricate PhaseFailed")
	}
	if got.Outcome != "" {
		t.Errorf("Outcome = %q, want empty (nothing decided yet)", got.Outcome)
	}
	if got.Terminal {
		t.Error("Terminal must be false on the fresh-boot fallback")
	}
	if got.Reason != ReasonClusterConfigDirWritable {
		t.Errorf("Reason = %q, want the fresh value", got.Reason)
	}
}
