package reconcile

import "github.com/kairos-io/provider-kubernetes/internal/reconcile/actualstate"

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
)

// Plan is a pure function: given the desired role and the observed actual state,
// it returns the ordered actions required to converge. It performs NO I/O and is
// fully unit-testable (design principle 6). Detecting an already-healthy CP/join
// yields ActionNone (reboot-safe no-op); detecting an existing control plane while
// desired==worker/controlplane drives toward join, never init (#4099-5).
func Plan(desired actualstate.Role, s actualstate.State) []Action {
	switch desired {
	case actualstate.RoleInit:
		if s.Membership == actualstate.Initialized && s.KubeletHealthy {
			return []Action{ActionNone}
		}
		if s.Membership == actualstate.Uninitialized {
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
