package reconcile

import (
	"reflect"
	"testing"

	"github.com/kairos-io/provider-kubernetes/internal/reconcile/actualstate"
)

func TestPlan(t *testing.T) {
	tests := []struct {
		name        string
		desired     actualstate.Role
		state       actualstate.State
		want        []Action
		wantVerdict Verdict
	}{
		{
			name:        "init on uninitialized node",
			desired:     actualstate.RoleInit,
			state:       actualstate.State{Membership: actualstate.Uninitialized},
			want:        []Action{ActionRunInit},
			wantVerdict: VerdictOK,
		},
		{
			name:        "init already converged is no-op",
			desired:     actualstate.RoleInit,
			state:       actualstate.State{Membership: actualstate.Initialized, KubeletHealthy: true, APIServerReachable: true},
			want:        []Action{ActionNone},
			wantVerdict: VerdictOK,
		},
		{
			name:        "worker join when CP reachable",
			desired:     actualstate.RoleWorker,
			state:       actualstate.State{Membership: actualstate.Uninitialized, ControlPlaneReachable: true},
			want:        []Action{ActionRunJoin},
			wantVerdict: VerdictOK,
		},
		{
			name:        "worker waits for CP when unreachable",
			desired:     actualstate.RoleWorker,
			state:       actualstate.State{Membership: actualstate.Uninitialized, ControlPlaneReachable: false},
			want:        []Action{ActionWaitForControlPlane, ActionRunJoin},
			wantVerdict: VerdictOK,
		},
		{
			name:        "joined healthy worker is no-op",
			desired:     actualstate.RoleWorker,
			state:       actualstate.State{Membership: actualstate.Joined, KubeletHealthy: true},
			want:        []Action{ActionNone},
			wantVerdict: VerdictOK,
		},
		{
			name:        "controlplane join when CP reachable",
			desired:     actualstate.RoleControlPlane,
			state:       actualstate.State{Membership: actualstate.Uninitialized, ControlPlaneReachable: true},
			want:        []Action{ActionRunJoin},
			wantVerdict: VerdictOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, verdict := Plan(tt.desired, "", tt.state)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Plan(%v, %+v) = %v, want %v", tt.desired, tt.state, got, tt.want)
			}
			if verdict != tt.wantVerdict {
				t.Fatalf("Plan(%v, %+v) verdict = %q, want %q", tt.desired, tt.state, verdict, tt.wantVerdict)
			}
		})
	}
}

// TestPlanDegradedVerdict is D-2: an already-established member (Initialized
// or Joined) whose kubelet is not healthy must return the SAME ActionNone as
// the healthy case (no automatic re-bootstrap: recovery is an explicit,
// separate reset flow) but a DIFFERENT Verdict, so the status layer can tell
// the two apart. Covers both the RoleInit/Initialized shape and the
// RoleControlPlane+RoleWorker/Joined shape the security sign-off named. The
// kubelet verdict wins even when the control-plane signals are also bad: a
// stopped kubelet explains a down apiserver, not the other way round.
func TestPlanDegradedVerdict(t *testing.T) {
	cases := []struct {
		name    string
		desired actualstate.Role
		state   actualstate.State
	}{
		{
			name:    "init role, initialized, unhealthy kubelet",
			desired: actualstate.RoleInit,
			state:   actualstate.State{Membership: actualstate.Initialized, KubeletHealthy: false},
		},
		{
			name:    "controlplane role, joined, unhealthy kubelet",
			desired: actualstate.RoleControlPlane,
			state:   actualstate.State{Membership: actualstate.Joined, KubeletHealthy: false},
		},
		{
			name:    "worker role, joined, unhealthy kubelet",
			desired: actualstate.RoleWorker,
			state:   actualstate.State{Membership: actualstate.Joined, KubeletHealthy: false},
		},
		{
			name:    "controlplane role, initialized, unhealthy kubelet, control plane also down",
			desired: actualstate.RoleControlPlane,
			state:   actualstate.State{Membership: actualstate.Initialized, KubeletHealthy: false, InitIncomplete: true, APIServerReachable: false},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			actions, verdict := Plan(c.desired, "", c.state)
			if !reflect.DeepEqual(actions, []Action{ActionNone}) {
				t.Fatalf("actions = %v, want [none] (the action must not change)", actions)
			}
			if verdict != VerdictKubeletUnhealthy {
				t.Fatalf("verdict = %q, want %q", verdict, VerdictKubeletUnhealthy)
			}
		})
	}
}

// TestPlanHealthyVerdictNotDegraded is the converse of
// TestPlanDegradedVerdict: the SAME already-established-member states, but
// with a healthy kubelet (and, on an Initialized node, a finished init and a
// serving local apiserver), must report VerdictOK -- proving the distinction
// is keyed on those signals and not, say, always degraded or always ok.
func TestPlanHealthyVerdictNotDegraded(t *testing.T) {
	cases := []struct {
		name    string
		desired actualstate.Role
		state   actualstate.State
	}{
		{
			name:    "init role, initialized, healthy kubelet and control plane",
			desired: actualstate.RoleInit,
			state:   actualstate.State{Membership: actualstate.Initialized, KubeletHealthy: true, APIServerReachable: true},
		},
		{
			name:    "controlplane role, initialized (a joined control plane), healthy",
			desired: actualstate.RoleControlPlane,
			state:   actualstate.State{Membership: actualstate.Initialized, KubeletHealthy: true, APIServerReachable: true},
		},
		{
			name:    "controlplane role, joined, healthy kubelet",
			desired: actualstate.RoleControlPlane,
			state:   actualstate.State{Membership: actualstate.Joined, KubeletHealthy: true},
		},
		{
			name:    "worker role, joined, healthy kubelet",
			desired: actualstate.RoleWorker,
			state:   actualstate.State{Membership: actualstate.Joined, KubeletHealthy: true},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			actions, verdict := Plan(c.desired, "", c.state)
			if !reflect.DeepEqual(actions, []Action{ActionNone}) {
				t.Fatalf("actions = %v, want [none]", actions)
			}
			if verdict != VerdictOK {
				t.Fatalf("verdict = %q, want ok", verdict)
			}
		})
	}
}

// TestPlanControlPlaneVerdicts covers what a live kubelet cannot show on a
// control plane. kubeadm writes admin.conf in its kubeconfig phase, long
// before it waits for the control plane, so a failed init or control-plane
// join leaves an Initialized node with a running kubelet; before these
// verdicts that was reported as converged on every later boot.
func TestPlanControlPlaneVerdicts(t *testing.T) {
	initialized := func(initIncomplete, apiUp bool) actualstate.State {
		return actualstate.State{
			Membership:         actualstate.Initialized,
			KubeletHealthy:     true,
			InitIncomplete:     initIncomplete,
			APIServerReachable: apiUp,
		}
	}
	cases := []struct {
		name  string
		state actualstate.State
		want  Verdict
	}{
		{name: "init finished, apiserver serving", state: initialized(false, true), want: VerdictOK},
		{name: "init unfinished, apiserver serving", state: initialized(true, true), want: VerdictInitIncomplete},
		// An unfinished init needs a reset whatever the apiserver does, so it
		// is the verdict reported when both are true.
		{name: "init unfinished, apiserver down", state: initialized(true, false), want: VerdictInitIncomplete},
		{name: "init finished, apiserver down", state: initialized(false, false), want: VerdictControlPlaneUnhealthy},
		// A Joined node has no control plane of its own; neither signal is
		// consulted for it (the prober never sets InitIncomplete for one).
		{
			name:  "joined node is judged on its kubelet only",
			state: actualstate.State{Membership: actualstate.Joined, KubeletHealthy: true, InitIncomplete: true, APIServerReachable: false},
			want:  VerdictOK,
		},
	}
	for _, desired := range []actualstate.Role{actualstate.RoleInit, actualstate.RoleControlPlane, actualstate.RoleWorker} {
		for _, c := range cases {
			t.Run(string(desired)+"/"+c.name, func(t *testing.T) {
				actions, verdict := Plan(desired, "", c.state)
				if !reflect.DeepEqual(actions, []Action{ActionNone}) {
					t.Fatalf("actions = %v, want [none] (no automatic recovery)", actions)
				}
				if verdict != c.want {
					t.Fatalf("verdict = %q, want %q", verdict, c.want)
				}
				if verdict.Degraded() != (c.want != VerdictOK) {
					t.Fatalf("Degraded() = %v for %q", verdict.Degraded(), verdict)
				}
			})
		}
	}
}

// TestPlanControlPlaneVerdictWithPinnedVersion covers the steady state of a
// cluster that pins kubernetesVersion, as every sample does: a target is set
// but this control plane already runs it, so planUpgrade hands the decision
// back and the member verdicts must still apply.
func TestPlanControlPlaneVerdictWithPinnedVersion(t *testing.T) {
	s := actualstate.State{
		Membership:           actualstate.Initialized,
		KubeletHealthy:       true,
		NodeComponentVersion: "v1.37.0",
		APIServerReachable:   false,
	}
	actions, verdict := Plan(actualstate.RoleInit, "v1.37.0", s)
	if !reflect.DeepEqual(actions, []Action{ActionNone}) {
		t.Fatalf("actions = %v, want [none]", actions)
	}
	if verdict != VerdictControlPlaneUnhealthy {
		t.Fatalf("verdict = %q, want %q", verdict, VerdictControlPlaneUnhealthy)
	}
}

func TestDefaultBudgetIsBounded(t *testing.T) {
	if !DefaultBudget().Valid() {
		t.Fatal("DefaultBudget must be valid/bounded")
	}
}

// HA-3: init-clobber guard (ADR-11 #2).

func TestPlanInitUninitialized_CPReachable_RefusesInit(t *testing.T) {
	// When role=init and ControlPlaneReachable=true, Plan must refuse to init
	// (a CP already serves at the endpoint; operator must use role=controlplane).
	got, verdict := Plan(actualstate.RoleInit, "", actualstate.State{
		Membership:            actualstate.Uninitialized,
		ControlPlaneReachable: true,
	})
	if len(got) != 1 || got[0] != ActionRefuseInit {
		t.Fatalf("expected [%s] when init+uninitialized+reachable, got %v", ActionRefuseInit, got)
	}
	if verdict != VerdictOK {
		t.Fatalf("verdict = %q, want ok", verdict)
	}
}

func TestPlanInitUninitialized_CPUnreachable_RunsInit(t *testing.T) {
	// When role=init and ControlPlaneReachable=false, Plan must proceed with init
	// (normal single-CP bootstrap, no existing CP).
	got, verdict := Plan(actualstate.RoleInit, "", actualstate.State{
		Membership:            actualstate.Uninitialized,
		ControlPlaneReachable: false,
	})
	if len(got) != 1 || got[0] != ActionRunInit {
		t.Fatalf("expected [%s] when init+uninitialized+unreachable, got %v", ActionRunInit, got)
	}
	if verdict != VerdictOK {
		t.Fatalf("verdict = %q, want ok", verdict)
	}
}

// ADR-12: upgrade decision table.
func TestPlanUpgrade(t *testing.T) {
	const t135 = "v1.35.0"
	cases := []struct {
		name    string
		desired actualstate.Role
		target  string
		state   actualstate.State
		want    []Action
	}{
		{
			name:    "CP apply (API up, manifest old) -> apply",
			desired: actualstate.RoleControlPlane, target: t135,
			state: actualstate.State{Membership: actualstate.Initialized, APIServerReachable: true, NodeComponentVersion: "v1.34.8"},
			want:  []Action{ActionUpgradeApply},
		},
		{
			name:    "CP API down (broken kubelet post image-swap) -> repair then apply",
			desired: actualstate.RoleControlPlane, target: t135,
			state: actualstate.State{Membership: actualstate.Initialized, APIServerReachable: false, NodeComponentVersion: "v1.34.8"},
			want:  []Action{ActionRepairKubeletConfig, ActionUpgradeApply},
		},
		{
			name:    "CP converged (manifest at target) -> no-op",
			desired: actualstate.RoleControlPlane, target: t135,
			state: actualstate.State{Membership: actualstate.Initialized, APIServerReachable: true, NodeComponentVersion: t135},
			want:  []Action{ActionNone},
		},
		{
			name:    "worker upgrade-node (kubelet healthy) -> wait then node",
			desired: actualstate.RoleWorker, target: t135,
			state: actualstate.State{Membership: actualstate.Joined, KubeletHealthy: true, ClusterVersion: t135, RunningKubeletVersion: "v1.34.8"},
			want:  []Action{ActionWaitForClusterUpgrade, ActionUpgradeNode},
		},
		{
			name:    "worker broken kubelet -> repair, wait, node",
			desired: actualstate.RoleWorker, target: t135,
			state: actualstate.State{Membership: actualstate.Joined, KubeletHealthy: false, ClusterVersion: t135, RunningKubeletVersion: "v1.34.8"},
			want:  []Action{ActionRepairKubeletConfig, ActionWaitForClusterUpgrade, ActionUpgradeNode},
		},
		{
			name:    "worker converged -> no-op",
			desired: actualstate.RoleWorker, target: t135,
			state: actualstate.State{Membership: actualstate.Joined, KubeletHealthy: true, RunningKubeletVersion: t135},
			want:  []Action{ActionNone},
		},
		{
			name:    "CP refuse skip-level (manifest 1.34 -> target 1.36)",
			desired: actualstate.RoleControlPlane, target: "v1.36.0",
			state: actualstate.State{Membership: actualstate.Initialized, APIServerReachable: true, NodeComponentVersion: "v1.34.8"},
			want:  []Action{ActionRefuseUpgrade},
		},
		{
			// Both minors are inside the window, so this is refused as a downgrade,
			// not as an out-of-window target.
			name:    "CP refuse downgrade (manifest 1.36 -> target 1.35)",
			desired: actualstate.RoleControlPlane, target: t135,
			state: actualstate.State{Membership: actualstate.Initialized, APIServerReachable: true, NodeComponentVersion: "v1.36.4"},
			want:  []Action{ActionRefuseUpgrade},
		},
		{
			name:    "no target -> normal flow (no-op when healthy member)",
			desired: actualstate.RoleWorker, target: "",
			state: actualstate.State{Membership: actualstate.Joined, KubeletHealthy: true},
			want:  []Action{ActionNone},
		},
		{
			name:    "fresh node joins at new version (not a member -> join, no upgrade)",
			desired: actualstate.RoleWorker, target: t135,
			state: actualstate.State{Membership: actualstate.Uninitialized, ControlPlaneReachable: true},
			want:  []Action{ActionRunJoin},
		},
		{
			name:    "CP manifest tag unknown -> no-op this pass",
			desired: actualstate.RoleControlPlane, target: t135,
			state: actualstate.State{Membership: actualstate.Initialized, APIServerReachable: true, NodeComponentVersion: ""},
			want:  []Action{ActionNone},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := Plan(c.desired, c.target, c.state)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Plan(%v, %q, %+v) = %v, want %v", c.desired, c.target, c.state, got, c.want)
			}
		})
	}
}

// TestPlanUpgradeHandledPathsAreNeverDegraded proves planUpgrade-handled
// branches always report VerdictOK, even when the state's KubeletHealthy is
// false: those branches return a real action (repair/apply/wait/node/refuse),
// never a hidden ActionNone, so D-2's degraded distinction does not apply.
func TestPlanUpgradeHandledPathsAreNeverDegraded(t *testing.T) {
	const t135 = "v1.35.0"
	cases := []struct {
		name    string
		desired actualstate.Role
		state   actualstate.State
	}{
		{
			name:    "CP API down, unhealthy manifest-old -> repair+apply, still ok",
			desired: actualstate.RoleControlPlane,
			state: actualstate.State{
				Membership: actualstate.Initialized, APIServerReachable: false, NodeComponentVersion: "v1.34.8",
			},
		},
		{
			name:    "worker broken kubelet -> repair+wait+node, still ok",
			desired: actualstate.RoleWorker,
			state: actualstate.State{
				Membership: actualstate.Joined, KubeletHealthy: false, ClusterVersion: t135, RunningKubeletVersion: "v1.34.8",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			actions, verdict := Plan(c.desired, t135, c.state)
			if len(actions) == 0 || actions[0] == ActionNone {
				t.Fatalf("actions = %v, want a real forward-moving action, not a hidden no-op", actions)
			}
			if verdict != VerdictOK {
				t.Fatalf("verdict = %q, want ok (planUpgrade never returns a hidden ActionNone)", verdict)
			}
		})
	}
}
