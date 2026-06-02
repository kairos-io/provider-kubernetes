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
func Plan(desired actualstate.Role, target string, s actualstate.State) []Action {
	if acts, handled := planUpgrade(desired, target, s); handled {
		return acts
	}

	switch desired {
	case actualstate.RoleInit:
		if s.Membership == actualstate.Initialized && s.KubeletHealthy {
			return []Action{ActionNone}
		}
		if s.Membership == actualstate.Uninitialized {
			// HA-3: if the endpoint is already serving, refuse loudly (ADR-11 #2).
			if s.ControlPlaneReachable {
				return []Action{ActionRefuseInit}
			}
			return []Action{ActionRunInit}
		}
		// Initialized-but-unhealthy, or already joined: do not re-init. Recovery is
		// an explicit, separate flow (reset), not an automatic re-bootstrap.
		return []Action{ActionNone}

	case actualstate.RoleControlPlane, actualstate.RoleWorker:
		if s.Membership == actualstate.Joined && s.KubeletHealthy {
			return []Action{ActionNone}
		}
		if s.Membership == actualstate.Uninitialized {
			if !s.ControlPlaneReachable {
				return []Action{ActionWaitForControlPlane, ActionRunJoin}
			}
			return []Action{ActionRunJoin}
		}
		// Initialized (this node is itself a CP) or joined-but-unhealthy: no-op.
		return []Action{ActionNone}

	default:
		return []Action{ActionNone}
	}
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
