package reconcile

import (
	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
	"github.com/kairos-io/provider-kubernetes/internal/reconcile/actualstate"
)

// Action is a single planned step toward the desired state.
type Action string

const (
	// ActionNone: already converged; nothing to do (the reboot-safe fast path).
	ActionNone Action = "none"
	// ActionRunInit: run `kubeadm init` (first control-plane node).
	ActionRunInit Action = "run-init"
	// ActionWaitForControlPlane: block (bounded) until the control-plane endpoint
	// is reachable before attempting a join.
	ActionWaitForControlPlane Action = "wait-for-control-plane"
	// ActionRunJoin: run `kubeadm join` (control-plane or worker).
	ActionRunJoin Action = "run-join"
	// ActionRefuseInit: refuse to init because a control plane already serves at
	// the target endpoint (HA-3 init-clobber guard, ADR-11 #2). The executor turns
	// this into a loud actionable error directing the operator to use role=controlplane.
	// Directly supports #4099-5 (never clobber an existing/externally-managed CP).
	ActionRefuseInit Action = "refuse-init"

	// ActionUpgradeApply: run `kubeadm upgrade apply <target>` on the FIRST control
	// plane (the apply-authority). Flips the cluster version to the target (ADR-12).
	ActionUpgradeApply Action = "upgrade-apply"
	// ActionWaitForClusterUpgrade: block (bounded) until the control plane has been
	// upgraded to the target (cluster version flipped and CP healthy) before this
	// node runs `kubeadm upgrade node` (ADR-12 follower path).
	ActionWaitForClusterUpgrade Action = "wait-for-cluster-upgrade"
	// ActionUpgradeNode: run `kubeadm upgrade node` on a follower control plane or a
	// worker to converge this node's components to the target (ADR-12).
	ActionUpgradeNode Action = "upgrade-node"
	// ActionRefuseUpgrade: refuse an unsafe upgrade (downgrade, skip-level, or
	// out-of-window target). The executor turns this into a loud terminal error
	// (ADR-12 skew enforcement); it is never retried.
	ActionRefuseUpgrade Action = "refuse-upgrade"
	// ActionRepairKubeletConfig: regenerate /var/lib/kubelet/kubeadm-flags.env +
	// config.yaml with the NEW kubeadm (API-free), so a kubelet that crashlooped on
	// a stale flag after an A/B image swap can start and bring the (old-version)
	// control plane back up before `kubeadm upgrade apply` runs (ADR-12-R1).
	ActionRepairKubeletConfig Action = "repair-kubelet-config"
)

// Verdict augments Plan's action list with the convergence signal the status
// layer needs, WITHOUT changing what the provider does. Security sign-off
// finding D-2 (PROJECT_CONTEXT.md ADR-16-A2/ADR-19 U2 security sign-off,
// 2026-09-17): an already-established member (Initialized or Joined) whose
// kubelet is not healthy took the exact same ActionNone as a fully healthy
// member, so a masked/crashed kubelet was silently reported as a converged
// success. The fix is deliberately NOT a new action -- recovery of an
// established member stays an explicit, separate reset flow, never an
// automatic re-bootstrap -- it is a second return value the status layer
// (internal/status) uses to tell a converged member from a degraded one. A
// live kubelet later turned out not to be enough for a control plane: a
// failed `kubeadm init` or control-plane join leaves admin.conf behind with
// the kubelet running, so an Initialized node is also checked for a finished
// init and a serving local apiserver.
type Verdict string

// Every verdict other than VerdictOK accompanies []Action{ActionNone} on an
// already-established member (Initialized or Joined). Plan takes no recovery
// action for any of them -- recovering an established member stays an
// explicit reset -- but the caller MUST NOT report them as a converged
// success. They are checked in the order listed: a stopped kubelet explains
// the other two, and an unfinished init needs a reset whatever the apiserver
// is doing.
const (
	// VerdictOK: either the node is fully converged (a healthy established
	// member) or Plan returned a real, forward-moving action (init/join/
	// upgrade/refuse/wait); the caller's normal success/failure handling
	// applies unchanged.
	VerdictOK Verdict = "ok"
	// VerdictKubeletUnhealthy: Membership is Initialized or Joined but
	// KubeletHealthy is false (D-2).
	VerdictKubeletUnhealthy Verdict = "kubelet-unhealthy"
	// VerdictInitIncomplete: Membership is Initialized, the kubelet is up, but
	// InitIncomplete shows this node's `kubeadm init` never finished. kubeadm
	// writes admin.conf long before the control plane is up, so without this
	// a failed init looked converged from the next boot on.
	VerdictInitIncomplete Verdict = "init-incomplete"
	// VerdictControlPlaneUnhealthy: Membership is Initialized, the kubelet is
	// up and init finished, but the local apiserver does not answer /healthz
	// (a crashlooping apiserver or etcd, or a control-plane join that failed
	// after admin.conf was written). The caller may re-probe after a bounded
	// grace period before reporting it, since after a reboot the static pods
	// start after the kubelet.
	VerdictControlPlaneUnhealthy Verdict = "control-plane-unhealthy"
)

// Degraded reports whether the verdict is one the caller must surface as a
// degraded member rather than as success.
func (v Verdict) Degraded() bool {
	return v != VerdictOK
}

// Plan is a pure function: given the desired role, the operator-pinned upgrade
// target (empty when no upgrade is intended), and the observed actual state, it
// returns the ordered actions required to converge. It performs NO I/O and is
// fully unit-testable (design principle 6).
//
// Upgrade (ADR-12) is evaluated first for members when a target is set: the first
// control plane to observe the cluster still at the old version self-elects to
// `upgrade apply`; followers (other control planes, workers) wait for the cluster
// version to flip then run `upgrade node`; unsafe transitions refuse terminally.
// When no upgrade applies, the normal bootstrap/join logic runs: detecting an
// already-healthy CP/join yields ActionNone (reboot-safe no-op); an existing
// control plane while desired==worker/controlplane drives toward join, never init
// (#4099-5); HA-3 refuses to init when the endpoint already serves (ADR-11 #2).
func Plan(desired actualstate.Role, target string, s actualstate.State) ([]Action, Verdict) {
	if acts, handled := planUpgrade(desired, target, s); handled {
		// Every planUpgrade-handled branch returns a real, forward-moving action
		// (refuse/apply/repair/wait/node) -- never a hidden ActionNone -- so the
		// member verdicts do not apply here.
		return acts, VerdictOK
	}

	switch desired {
	case actualstate.RoleInit:
		if s.Membership == actualstate.Uninitialized {
			// HA-3: if the endpoint is already serving, refuse loudly (ADR-11 #2).
			if s.ControlPlaneReachable {
				return []Action{ActionRefuseInit}, VerdictOK
			}
			return []Action{ActionRunInit}, VerdictOK
		}
		// Already Initialized or Joined: never re-init. Recovery is an explicit,
		// separate flow (reset), not an automatic re-bootstrap; whether this
		// member is healthy is reported through the Verdict, not the action.
		return []Action{ActionNone}, memberVerdict(s)

	case actualstate.RoleControlPlane, actualstate.RoleWorker:
		if s.Membership == actualstate.Uninitialized {
			if !s.ControlPlaneReachable {
				return []Action{ActionWaitForControlPlane, ActionRunJoin}, VerdictOK
			}
			return []Action{ActionRunJoin}, VerdictOK
		}
		// Initialized (this node is itself a CP) or Joined: no-op, with health
		// reported through the Verdict.
		return []Action{ActionNone}, memberVerdict(s)

	default:
		return []Action{ActionNone}, VerdictOK
	}
}

// memberVerdict classifies the ActionNone taken when the node is already an
// established member (Initialized or Joined -- the only two Memberships that
// reach this call, since Uninitialized always returns earlier), so the status
// layer never has to re-derive it from raw state.
//
// A Joined node is a worker as far as membership goes: its health is its
// kubelet (D-2). An Initialized node runs a control plane, and a live kubelet
// says nothing about it -- the kubelet stays healthy while every static pod
// it runs crashloops -- so it must also have finished init and serve its
// local apiserver before it counts as converged.
func memberVerdict(s actualstate.State) Verdict {
	if !s.KubeletHealthy {
		return VerdictKubeletUnhealthy
	}
	if s.Membership != actualstate.Initialized {
		return VerdictOK
	}
	if s.InitIncomplete {
		return VerdictInitIncomplete
	}
	if !s.APIServerReachable {
		return VerdictControlPlaneUnhealthy
	}
	return VerdictOK
}

// planUpgrade implements the ADR-12 (+R1) per-node upgrade decision. It returns
// (actions, true) when it owns the decision for this pass, or (nil, false) to fall
// through to the normal bootstrap/join logic (no upgrade intended, not a member,
// signals not yet observable, or this node already converged).
//
// ADR-12-R1: detection is API-FREE. For a control plane the trigger is the local
// kube-apiserver static-pod manifest tag (NodeComponentVersion) vs the bundled
// target -- NOT a kubeadm-config read, which fails exactly when an A/B image swap
// has left the kubelet crashlooping on a stale flag. When the local apiserver is
// down and an upgrade is due, the kubelet config is repaired first (new kubeadm,
// API-free) so the old control plane comes back up before `upgrade apply`.
func planUpgrade(desired actualstate.Role, target string, s actualstate.State) ([]Action, bool) {
	// No upgrade intent: the operator has not pinned a target version.
	if target == "" {
		return nil, false
	}
	// Only existing members upgrade; a non-member falls through to init/join.
	if s.Membership != actualstate.Initialized && s.Membership != actualstate.Joined {
		return nil, false
	}

	switch desired {
	case actualstate.RoleInit, actualstate.RoleControlPlane:
		// Per-node CP convergence signal: the apiserver static-pod manifest tag
		// (file-based; readable with the API down).
		if s.NodeComponentVersion == "" {
			return nil, false // cannot determine CP convergence; no-op this pass
		}
		if sameMinor(s.NodeComponentVersion, target) {
			return nil, false // this control plane is already converged
		}
		// Skew is evaluated against THIS CP's current version (the manifest tag),
		// terminal before any destructive action.
		if _, err := kubeadm.UpgradePath(s.NodeComponentVersion, target); err != nil {
			return []Action{ActionRefuseUpgrade}, true
		}
		// If the local apiserver is down (post image-swap stale kubelet flags),
		// repair the kubelet config first so the old control plane returns and
		// `upgrade apply` can reach it. UpgradeApply self-degrades to `upgrade node`
		// via its pre-apply re-check when the cluster has already flipped (follower).
		if !s.APIServerReachable {
			return []Action{ActionRepairKubeletConfig, ActionUpgradeApply}, true
		}
		return []Action{ActionUpgradeApply}, true

	case actualstate.RoleWorker:
		// Per-node worker signal: the RUNNING kubelet version.
		if s.RunningKubeletVersion == "" {
			return nil, false // cannot determine worker convergence; no-op this pass
		}
		if sameMinor(s.RunningKubeletVersion, target) {
			return nil, false // this worker is already converged
		}
		// Skew check against the cluster version when known (best-effort on workers).
		if s.ClusterVersion != "" {
			if _, err := kubeadm.UpgradePath(s.ClusterVersion, target); err != nil {
				return []Action{ActionRefuseUpgrade}, true
			}
		}
		acts := make([]Action, 0, 3)
		if !s.KubeletHealthy {
			// The worker's new kubelet may also crashloop on a stale flag; repair it.
			acts = append(acts, ActionRepairKubeletConfig)
		}
		// Wait for the control plane to flip the cluster version, then upgrade node.
		acts = append(acts, ActionWaitForClusterUpgrade, ActionUpgradeNode)
		return acts, true

	default:
		return nil, false
	}
}

// sameMinor reports whether two versions share a major.minor (e.g. "v1.34.8" and
// "v1.34.0"). Invalid/empty versions never match a valid one.
func sameMinor(a, b string) bool {
	ma, mb := kubeadm.Minor(a), kubeadm.Minor(b)
	return ma != "" && ma == mb
}
