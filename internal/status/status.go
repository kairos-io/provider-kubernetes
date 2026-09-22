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
	// PhaseDegraded: the node is an already-established member (Initialized or
	// Joined) but its kubelet health signal is not healthy (D-2, the 2026-09-17
	// U2 security sign-off). The reconcile action is still ActionNone --
	// recovering an established member is a deliberate, explicit reset flow,
	// never an automatic re-bootstrap -- so this is NOT a Failed/terminal
	// phase: the next boot (or an operator reset) may still converge cleanly.
	// It exists so that fact is never silently reported as Converged, which
	// would hide a real outage (e.g. every control-plane container exited).
	PhaseDegraded Phase = "Degraded"
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
	// ReasonKubeletUnhealthy names the kubelet health signal behind
	// PhaseDegraded (D-2): the node is already Initialized or Joined but
	// actualstate.State.KubeletHealthy is false.
	ReasonKubeletUnhealthy Reason = "KubeletUnhealthy"

	// D-3 / F-UKIBOOT (security review 2026-09-18, S-D3-9): the closed set of
	// reasons internal/clusterconfigdir reports when it establishes (or finds
	// unsafe) /usr/local/cloud-config before the kairos-sdk clusterplugin
	// writes cluster.kairos.yaml there. Written well before any reconcile pass
	// runs, so a later Converged/Failed status overwriting one of these is
	// expected, not a bug.
	//
	// ReasonClusterConfigAncestorUnsafe: an ancestor of the target directory
	// (/usr or /usr/local) is a symlink, not a directory, or not owned by
	// uid 0 (S-D3-2). Withholds cluster_token (S-D3-5a, security review
	// amendment 2026-09-21): the walk verified nothing about where that
	// component leads, and the SDK's own open resolves the full path by name
	// straight through it.
	ReasonClusterConfigAncestorUnsafe Reason = "ClusterConfigAncestorUnsafe"
	// ReasonClusterConfigAncestorMissing: an ancestor of the target directory
	// (/usr or /usr/local) is simply absent (S-D3-2). Reported, never
	// withheld (S-D3-5a): this is not a safety refusal, the SDK's own open
	// then fails the identical ENOENT, and no token goes anywhere either way.
	ReasonClusterConfigAncestorMissing Reason = "ClusterConfigAncestorMissing"
	// ReasonClusterConfigDirCreateFailed: mkdirat failed for a reason other
	// than EEXIST (e.g. a read-only filesystem).
	ReasonClusterConfigDirCreateFailed Reason = "ClusterConfigDirCreateFailed"
	// ReasonClusterConfigDirUnsafe: /usr/local/cloud-config already exists and
	// is not a root-owned directory (a symlink, FIFO, device or regular file)
	// (S-D3-3). One of three reasons that withholds cluster_token (with
	// ReasonClusterConfigAncestorUnsafe and ReasonClusterConfigTokenFileUnsafe).
	ReasonClusterConfigDirUnsafe Reason = "ClusterConfigDirUnsafe"
	// ReasonClusterConfigDirWritable: /usr/local/cloud-config exists, is a
	// root-owned directory, but is OTHER-writable (narrowed from
	// group-or-other by the 2026-09-21 VM run). Reported, never refused
	// (S-D3-3): the platform's own 10_accounting.yaml widens it to 0770
	// root:admin ~130ms after we create it, on every boot from the second
	// onward -- that expected, non-attacker state is group-writable, not
	// other-writable, so a group-or-other check would have reported it on
	// every single boot.
	ReasonClusterConfigDirWritable Reason = "ClusterConfigDirWritable"
	// ReasonClusterConfigTokenFileUnsafe: the token-file target (normally
	// cluster.kairos.yaml) is anything other than absent (ENOENT) or an
	// intact, root-owned, mode-0600-or-tighter, nlink-1 regular file -- an
	// allowlist, not an enumerated unsafe set (S-D3-4, amended S-D3-4a,
	// security review 2026-09-21). One of three reasons that withholds
	// cluster_token.
	ReasonClusterConfigTokenFileUnsafe Reason = "ClusterConfigTokenFileUnsafe"
	// ReasonClusterConfigOverrideRejected: cluster_config_path was set but its
	// directory is not exactly /usr/local/cloud-config (or the path is
	// relative or contains ".." after Clean). Nothing is created; the operator
	// owns pre-creating their own override directory (S-D3-7).
	ReasonClusterConfigOverrideRejected Reason = "ClusterConfigOverrideRejected"
	// ReasonClusterConfigNotPersistent: /usr/local's device is the same as
	// /'s, i.e. COS_PERSISTENT is not actually mounted there. Reported, never
	// refused: the token is about to be written to ephemeral storage and the
	// node will not survive a reboot converged (S-D3-8).
	ReasonClusterConfigNotPersistent Reason = "ClusterConfigNotPersistent"
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
	// Degraded is D-2's signal: reconcile.Plan returned VerdictDegraded (the
	// node is already Initialized or Joined but its kubelet is not healthy).
	// Only consulted when Err is nil (a real failure already has its own
	// phase/reason via the failure path below); forces PhaseDegraded instead
	// of PhaseConverged so this is never silently reported as success.
	Degraded bool
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
		// D-2: an established member (Initialized/Joined) whose kubelet is not
		// healthy must not report Converged/success, even though the reconcile
		// action was (deliberately) ActionNone. Membership is left as the
		// probed value (s.Membership, set above) -- no action ran, so there is
		// no post-action membership to derive as the switch below does.
		if p.Degraded {
			s.Phase = PhaseDegraded
			s.Outcome = OutcomeFailure
			s.Reason = ReasonKubeletUnhealthy
			s.Terminal = false
			s.Budget = Budget{Attempts: 0, MaxAttempts: 0}
			s.Message = sanitize("kubelet health check is failing on an already-" + s.Membership +
				" node; recovery is an explicit reset, not automatic")
			return s
		}
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

// MergeReportOnly builds a Status for a report-only finding that MUST NOT
// move an existing reconcile verdict backwards (S-D3-9a, security review
// 2026-09-21: the D-3/F-UKIBOOT VM run found the ensure path driving a
// converged GRUB node's status from Converged to Failed while the boot
// converged fine -- a diagnostic that downgrades a healthy node is a
// security-relevant defect in its own right, since it trains operators to
// ignore Failed).
//
// record is false when the finding must not reach the status channel at all.
// Reason is the machine-stable verdict code; a report-only diagnostic may
// fill an empty one, but it must never displace one that is already on
// record. Passing Reason (and Message) through on top of a recorded FAILURE
// relabels that failure: prev's Phase, Outcome, Terminal, Budget and
// LastAction are preserved by design, so the document ends up asserting a
// terminal failure of prev's action, with prev's attempt counts, under this
// finding's reason -- and the real reason is gone from both status files and
// from the Node annotations. That is reached on any boot following a failed
// one: statusReader consults /run (tmpfs, empty this early) before the
// persistent /var/log mirror, so the previous boot's failure is what a
// report-only finding is handed. It is also self-contradictory for the
// report-only reasons the schema documents as "Reported, never refused",
// which cannot be terminal. The finding is still logged at error level by
// the caller, which is where a diagnostic belongs once the verdict slot is
// taken.
//
// Phase, Outcome, Membership, Role, LastAction, Budget and Terminal are
// copied through from prev UNCHANGED when existing is true (the caller
// already read the most recent Status, e.g. via ReadLatest); only Reason,
// Message, UpdatedAt, BootID and Version are set fresh. This is a pure
// function (design principle 6): the read is the caller's job, so this
// stays hardware-free testable, exactly like BuildStatus.
//
// When existing is false (ReadLatest found nothing parseable at any
// configured path -- a truly fresh install, or a fresh boot before this
// boot's own reconcile has run and no persistent mirror survives from
// before), the returned Status starts at PhaseReconciling with an empty
// Outcome: the reconcile has not run yet this boot, which is an honest
// "in progress", never a false "failed". PhaseReconciling already existed
// for exactly this "written at start, replaced on completion" case.
func MergeReportOnly(prev Status, existing bool, reason Reason, message, bootID, version, now string) (s Status, record bool) {
	if existing && prev.Reason != ReasonNone {
		return prev, false
	}
	s = prev
	if !existing {
		s = Status{Phase: PhaseReconciling}
	}
	s.APIVersion = APIVersion
	s.Reason = reason
	s.Message = message
	s.UpdatedAt = now
	s.BootID = bootID
	s.Version = version
	return s, true
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
