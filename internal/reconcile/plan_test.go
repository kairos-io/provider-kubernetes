package reconcile

import (
	"reflect"
	"testing"

	"github.com/kairos-io/provider-kubernetes/internal/reconcile/actualstate"
)

func TestPlan(t *testing.T) {
	tests := []struct {
		name    string
		desired actualstate.Role
		state   actualstate.State
		want    []Action
	}{
		{
			name:    "init on uninitialized node",
			desired: actualstate.RoleInit,
			state:   actualstate.State{Membership: actualstate.Uninitialized},
			want:    []Action{ActionRunInit},
		},
		{
			name:    "init already converged is no-op",
			desired: actualstate.RoleInit,
			state:   actualstate.State{Membership: actualstate.Initialized, KubeletHealthy: true},
			want:    []Action{ActionNone},
		},
		{
			name:    "worker join when CP reachable",
			desired: actualstate.RoleWorker,
			state:   actualstate.State{Membership: actualstate.Uninitialized, ControlPlaneReachable: true},
			want:    []Action{ActionRunJoin},
		},
		{
			name:    "worker waits for CP when unreachable",
			desired: actualstate.RoleWorker,
			state:   actualstate.State{Membership: actualstate.Uninitialized, ControlPlaneReachable: false},
			want:    []Action{ActionWaitForControlPlane, ActionRunJoin},
		},
		{
			name:    "joined healthy worker is no-op",
			desired: actualstate.RoleWorker,
			state:   actualstate.State{Membership: actualstate.Joined, KubeletHealthy: true},
			want:    []Action{ActionNone},
		},
		{
			name:    "controlplane join when CP reachable",
			desired: actualstate.RoleControlPlane,
			state:   actualstate.State{Membership: actualstate.Uninitialized, ControlPlaneReachable: true},
			want:    []Action{ActionRunJoin},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Plan(tt.desired, "", tt.state)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Plan(%v, %+v) = %v, want %v", tt.desired, tt.state, got, tt.want)
			}
		})
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
	got := Plan(actualstate.RoleInit, "", actualstate.State{
		Membership:            actualstate.Uninitialized,
		ControlPlaneReachable: true,
	})
	if len(got) != 1 || got[0] != ActionRefuseInit {
		t.Fatalf("expected [%s] when init+uninitialized+reachable, got %v", ActionRefuseInit, got)
	}
}

func TestPlanInitUninitialized_CPUnreachable_RunsInit(t *testing.T) {
	// When role=init and ControlPlaneReachable=false, Plan must proceed with init
	// (normal single-CP bootstrap, no existing CP).
	got := Plan(actualstate.RoleInit, "", actualstate.State{
		Membership:            actualstate.Uninitialized,
		ControlPlaneReachable: false,
	})
	if len(got) != 1 || got[0] != ActionRunInit {
		t.Fatalf("expected [%s] when init+uninitialized+unreachable, got %v", ActionRunInit, got)
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
			name:    "CP apply-authority (cluster not flipped, this CP old)",
			desired: actualstate.RoleControlPlane, target: t135,
			state: actualstate.State{Membership: actualstate.Initialized, ClusterVersion: "v1.34.8", NodeComponentVersion: "v1.34.8"},
			want:  []Action{ActionUpgradeApply},
		},
		{
			name:    "CP follower (cluster flipped, this CP old)",
			desired: actualstate.RoleControlPlane, target: t135,
			state: actualstate.State{Membership: actualstate.Initialized, ClusterVersion: t135, NodeComponentVersion: "v1.34.8"},
			want:  []Action{ActionWaitForClusterUpgrade, ActionUpgradeNode},
		},
		{
			name:    "CP converged -> no-op",
			desired: actualstate.RoleControlPlane, target: t135,
			state: actualstate.State{Membership: actualstate.Initialized, KubeletHealthy: true, ClusterVersion: t135, NodeComponentVersion: t135},
			want:  []Action{ActionNone},
		},
		{
			name:    "worker upgrade-node (cluster flipped, kubelet old)",
			desired: actualstate.RoleWorker, target: t135,
			state: actualstate.State{Membership: actualstate.Joined, ClusterVersion: t135, RunningKubeletVersion: "v1.34.8"},
			want:  []Action{ActionUpgradeNode},
		},
		{
			name:    "worker waits (cluster not flipped yet)",
			desired: actualstate.RoleWorker, target: t135,
			state: actualstate.State{Membership: actualstate.Joined, ClusterVersion: "v1.34.8", RunningKubeletVersion: "v1.34.8"},
			want:  []Action{ActionWaitForClusterUpgrade, ActionUpgradeNode},
		},
		{
			name:    "worker converged -> no-op",
			desired: actualstate.RoleWorker, target: t135,
			state: actualstate.State{Membership: actualstate.Joined, KubeletHealthy: true, ClusterVersion: t135, RunningKubeletVersion: t135},
			want:  []Action{ActionNone},
		},
		{
			name:    "refuse skip-level",
			desired: actualstate.RoleControlPlane, target: "v1.36.0",
			state: actualstate.State{Membership: actualstate.Initialized, ClusterVersion: "v1.34.8", NodeComponentVersion: "v1.34.8"},
			want:  []Action{ActionRefuseUpgrade},
		},
		{
			name:    "refuse downgrade",
			desired: actualstate.RoleControlPlane, target: "v1.34.0",
			state: actualstate.State{Membership: actualstate.Initialized, ClusterVersion: t135, NodeComponentVersion: t135},
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
			name:    "cluster version unknown -> no-op this pass",
			desired: actualstate.RoleControlPlane, target: t135,
			state: actualstate.State{Membership: actualstate.Initialized, ClusterVersion: "", NodeComponentVersion: "v1.34.8"},
			want:  []Action{ActionNone},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Plan(c.desired, c.target, c.state)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Plan(%v, %q, %+v) = %v, want %v", c.desired, c.target, c.state, got, c.want)
			}
		})
	}
}
