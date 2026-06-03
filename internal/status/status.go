// Package status defines the machine-readable provider status document (ADR-4-S,
// S1). Every reconcile pass -- success or failure -- writes one Status value to
// the local-status channel so operators learn the outcome without SSH+journald.
//
// Design constraints (load-bearing):
//   - The schema is CLOSED: by construction it cannot carry a secret. The only
//     free-text field (Message) is sanitized before write.
//   - BuildStatus is a PURE function (no I/O, deterministic given a clock); only
//     the StatusSink implementations do I/O (S2).
//   - Reason is a CLOSED enumeration; unknown/unexpected errors map to a generic
//     code, never to raw error text.
package status

import (
	"errors"
	"regexp"
	"strings"

	"github.com/kairos-io/provider-kubernetes/internal/reconcile"
	"github.com/kairos-io/provider-kubernetes/internal/reconcile/actualstate"
)

// APIVersion is the stable schema version tag written to every status document.
// Readers may gate on this field before parsing.
const APIVersion = "provider-kubernetes.kairos.io/v1"

// Phase is the coarse lifecycle state of the node's reconcile pass.
type Phase string

const (
	// PhaseReconciling: the reconcile is in progress (written at start, replaced
	// on completion; only visible if the provider is killed mid-run).
	PhaseReconciling Phase = "Reconciling"
	// PhaseConverged: the reconcile completed successfully.
	PhaseConverged Phase = "Converged"
	// PhaseFailed: the reconcile failed (terminal or budget-exhausted).
	PhaseFailed Phase = "Failed"
	// PhaseReset: the node was reset via EventClusterReset.
	PhaseReset Phase = "Reset"
)

// Outcome is the explicit success/failure signal, redundant with Phase but
// useful for scrapers that do not want a three-way Phase comparison.
type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	OutcomeFailure Outcome = "failure"
)

// Reason is a CLOSED enumeration of machine-stable failure codes. It is empty
// on success. Unknown / unexpected errors map to ReasonKubeadmError rather than
// carrying raw error text as a reason code (closed-schema contract).
type Reason string

const (
	ReasonNone                    Reason = ""
	ReasonControlPlaneUnreachable Reason = "ControlPlaneUnreachable"
	ReasonJoinTimeout             Reason = "JoinTimeout"
	ReasonInitRefused             Reason = "InitRefused"
	ReasonUpgradeRefused          Reason = "UpgradeRefused"
	ReasonBudgetExhausted         Reason = "BudgetExhausted"
	ReasonKubeadmError            Reason = "KubeadmError"
	ReasonConfigInvalid           Reason = "ConfigInvalid"
	ReasonResetFailed             Reason = "ResetFailed"
	ReasonResetOK                 Reason = "ResetOK"
)

// Budget captures how many attempts were consumed for the last/failing action.
// Both fields are 0 on success (the action completed on the first attempt and
// we do not track success-path attempt counts in the status).
//
// NOTE: sigs.k8s.io/yaml marshals via encoding/json, so json tags drive the
// YAML key names. The yaml tags are kept for documentation only.
type Budget struct {
	Attempts    int `json:"attempts"    yaml:"attempts"`
	MaxAttempts int `json:"maxAttempts" yaml:"maxAttempts"`
}

// Status is the frozen, closed status document (ADR-4-S). It is marshaled as
// YAML with sigs.k8s.io/yaml (which uses encoding/json internally), so json
// tags drive the on-disk YAML key names. Fields are ordered for human
// readability; the schema is additive-only (never repurpose or remove a field
// without an ADR).
//
// Key mapping (json tag -> YAML key):
//
//	APIVersion -> apiVersion
//	Phase      -> phase
//	Role       -> role
//	Membership -> membership
//	Outcome    -> outcome
//	Reason     -> reason
//	Terminal   -> terminal
//	LastAction -> lastAction
//	Message    -> message
//	Budget     -> budget
//	UpdatedAt  -> updatedAt
//	BootID     -> bootID
//	Version    -> version
type Status struct {
	APIVersion string  `json:"apiVersion" yaml:"apiVersion"`
	Phase      Phase   `json:"phase"      yaml:"phase"`
	Role       string  `json:"role"       yaml:"role"`
	Membership string  `json:"membership" yaml:"membership"`
	Outcome    Outcome `json:"outcome"    yaml:"outcome"`
	// Reason is a CLOSED machine token; empty on success.
	Reason Reason `json:"reason" yaml:"reason"`
	// Terminal is true when the failure will not self-resolve on next boot.
	Terminal bool `json:"terminal" yaml:"terminal"`
	// LastAction is the reconcile.Action that completed or failed.
	LastAction string `json:"lastAction" yaml:"lastAction"`
	// Message is a one-line operator summary. ALWAYS sanitized (see sanitize).
	Message   string `json:"message"   yaml:"message"`
	Budget    Budget `json:"budget"    yaml:"budget"`
	UpdatedAt string `json:"updatedAt" yaml:"updatedAt"`
	BootID    string `json:"bootID"    yaml:"bootID"`
	Version   string `json:"version"   yaml:"version"`
}

// BuildParams collects all inputs to BuildStatus so the call site is readable
// and the function signature stays stable as fields are added.
type BuildParams struct {
	// Role is the desired node role echoed from the reconcile context.
	Role actualstate.Role
	// Membership is the observed membership at the time of the status write.
	Membership actualstate.Membership
	// LastAction is the action that completed (success) or failed.
	LastAction reconcile.Action
	// Err is nil on success, non-nil on failure. ErrTerminal-wrapped errors
	// set Terminal=true and suppress retry-budget-exhaustion reasoning.
	Err error
	// Result carries attempt counts from the driver (S4).
	Result reconcile.RunResult
	// Now is an RFC3339 timestamp for UpdatedAt. Inject in tests for
	// determinism; in production callers pass time.Now().UTC().Format(time.RFC3339).
	Now string
	// BootID is the contents of /proc/sys/kernel/random/boot_id (best-effort;
	// empty is acceptable).
	BootID string
	// Version is the provider build version.
	Version string
	// IsReset is true when building a post-reset status (phase=Reset).
	IsReset bool
	// ResetErr is non-nil when a reset itself failed.
	ResetErr error
	// ConfigInvalid marks an early-exit failure that occurred before the node
	// role/state could be probed because the provider configuration itself was
	// rejected (e.g. an empty/too-short cluster_token or unparseable user
	// config). It forces reason=ConfigInvalid (terminal) rather than letting the
	// (action, err) heuristic mislabel a config error as a generic KubeadmError.
	// Requires Err to be non-nil.
	ConfigInvalid bool
}

// BuildStatus is a PURE function: it maps reconcile outcomes to a Status value.
// It has no I/O and is fully deterministic for a fixed clock value, so every
// code path is table-testable without hardware (design principle 6).
func BuildStatus(p BuildParams) Status {
	s := Status{
		APIVersion: APIVersion,
		Role:       string(p.Role),
		Membership: string(p.Membership),
		UpdatedAt:  p.Now,
		BootID:     p.BootID,
		Version:    p.Version,
		LastAction: string(p.LastAction),
	}

	// Reset path: a distinct terminal status written after EventClusterReset.
	if p.IsReset {
		s.Phase = PhaseReset
		if p.ResetErr != nil {
			s.Outcome = OutcomeFailure
			s.Reason = ReasonResetFailed
			s.Terminal = true
			s.Message = sanitize("reset failed: " + p.ResetErr.Error())
		} else {
			s.Outcome = OutcomeSuccess
			s.Reason = ReasonResetOK
			s.Terminal = true
			s.Message = "cluster reset completed"
		}
		return s
	}

	// Config-invalid early-exit: the provider config was rejected before any
	// state probe, so (lastAction, err) carry no useful signal. Map explicitly
	// to the closed ConfigInvalid reason (terminal: bad config will not converge
	// on retry without operator action).
	if p.ConfigInvalid && p.Err != nil {
		s.Phase = PhaseFailed
		s.Outcome = OutcomeFailure
		s.Reason = ReasonConfigInvalid
		s.Terminal = true
		s.Message = sanitize("invalid configuration: " + p.Err.Error())
		return s
	}

	// Normal reconcile path.
	if p.Err == nil {
		s.Phase = PhaseConverged
		s.Outcome = OutcomeSuccess
		s.Reason = ReasonNone
		s.Terminal = false
		s.Budget = Budget{Attempts: 0, MaxAttempts: 0}
		s.Message = "converged"
		// Bug fix (ADR-4-S accuracy): when a membership-establishing action just
		// completed, the pre-action probe membership is stale (it was probed BEFORE
		// the action ran). Derive the post-reconcile membership from the action so
		// a Converged status is self-consistent rather than contradictory.
		//   ActionRunInit -> Initialized (this node now owns the control plane)
		//   ActionRunJoin -> Joined      (worker or extra-CP, now a member)
		//   anything else  -> keep the probed membership (already accurate)
		switch p.LastAction {
		case reconcile.ActionRunInit:
			s.Membership = string(actualstate.Initialized)
		case reconcile.ActionRunJoin:
			s.Membership = string(actualstate.Joined)
		}
		return s
	}

	// Failure path: map (lastAction, err) -> reason/terminal.
	s.Phase = PhaseFailed
	s.Outcome = OutcomeFailure
	s.Terminal = errors.Is(p.Err, reconcile.ErrTerminal)
	s.Budget = Budget{
		Attempts:    p.Result.Attempts,
		MaxAttempts: p.Result.MaxAttempts,
	}
	s.Reason = deriveReason(p.LastAction, p.Err, p.Result)
	s.Message = sanitize(actionMessage(p.LastAction, p.Err))
	return s
}

// deriveReason maps (action, error, result) to a CLOSED Reason constant. This
// is the single authoritative switch for reason derivation; every caller goes
// through here.
func deriveReason(a reconcile.Action, err error, result reconcile.RunResult) Reason {
	if err == nil {
		return ReasonNone
	}

	// Terminal errors: the executor made a deliberate no-retry decision.
	if errors.Is(err, reconcile.ErrTerminal) {
		switch a {
		case reconcile.ActionRefuseInit:
			return ReasonInitRefused
		case reconcile.ActionRefuseUpgrade:
			return ReasonUpgradeRefused
		case reconcile.ActionRunJoin:
			return ReasonJoinTimeout
		default:
			return ReasonKubeadmError
		}
	}

	// Budget exhaustion: the driver retried MaxAttempts times.
	if result.BudgetExhausted {
		switch a {
		case reconcile.ActionWaitForControlPlane, reconcile.ActionRunJoin:
			return ReasonControlPlaneUnreachable
		case reconcile.ActionWaitForClusterUpgrade:
			return ReasonJoinTimeout
		default:
			return ReasonBudgetExhausted
		}
	}

	// Non-terminal, non-exhausted failure (e.g. internal/config error).
	switch a {
	case reconcile.ActionWaitForControlPlane:
		return ReasonControlPlaneUnreachable
	case reconcile.ActionRunJoin:
		return ReasonJoinTimeout
	default:
		return ReasonKubeadmError
	}
}

// actionMessage produces a short, sanitized operator-facing summary. It uses
// only our own typed action/reason text -- never raw external bytes -- so the
// sanitizer has a small surface to guard.
func actionMessage(a reconcile.Action, err error) string {
	if err == nil {
		return "converged"
	}
	// Use the stable action string as the leading token, then the error.
	// The error may contain kubeadm output; sanitize() scrubs credential patterns.
	return string(a) + ": " + err.Error()
}

// sanitize scrubs free-text that might contain credential material before it
// reaches the status document. The closed schema means Message is the only
// vector; we apply conservative regex scrubbing so a stray log line in a
// wrapped error cannot carry a bootstrap token into the file.
//
// Patterns removed (replaced with "[REDACTED]"):
//   - bootstrap tokens: <token-id>.<token-secret> (6 alnum . 16 alnum)
//   - PEM blocks: -----BEGIN ... -----END ...
//   - long hex/base64 strings that look like keys/hashes (>=32 contiguous chars
//     of [0-9a-fA-F] or base64url alphabet) -- conservative but cheap
func sanitize(s string) string {
	s = reBootstrapToken.ReplaceAllString(s, "[REDACTED]")
	s = rePEM.ReplaceAllString(s, "[REDACTED]")
	s = reLongSecret.ReplaceAllString(s, "[REDACTED]")
	// Trim to a reasonable single-line length.
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	const maxLen = 256
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	return strings.TrimSpace(s)
}

var (
	// Bootstrap token pattern: 6 lowercase-alnum.16 lowercase-alnum
	reBootstrapToken = regexp.MustCompile(`\b[a-z0-9]{6}\.[a-z0-9]{16}\b`)
	// PEM block (multi-line; use (?s) for dot-matches-newline)
	rePEM = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]+-----.*?-----END [A-Z ]+-----`)
	// Long contiguous hex or base64url run (>= 32 chars) -- hashes, base64 secrets
	reLongSecret = regexp.MustCompile(`[A-Za-z0-9+/=_-]{32,}`)
)
