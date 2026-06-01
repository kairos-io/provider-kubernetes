// Package actualstate models the observed, real state of a node and the interface
// for probing it (ADR-4: desired-vs-actual reconciliation, never sentinel files).
// Probing reads authoritative artifacts (kubelet health, /etc/kubernetes/*.conf,
// etcd membership, control-plane reachability) and performs no mutation.
package actualstate

import "context"

// Role is the desired Kubernetes role for the node, from the Cluster definition.
type Role string

const (
	RoleInit         Role = "init"
	RoleControlPlane Role = "controlplane"
	RoleWorker       Role = "worker"
)

// Membership describes whether and how the node currently participates in a cluster.
type Membership string

const (
	// Uninitialized: no usable kubeadm membership artifacts present.
	Uninitialized Membership = "uninitialized"
	// Initialized: this node runs a control plane it initialized (kubeadm init done).
	Initialized Membership = "initialized"
	// Joined: this node joined an existing cluster (kubelet.conf + valid client cert).
	Joined Membership = "joined"
)

// State is the observed actual state of the node.
type State struct {
	// Membership is the node's current cluster-membership status.
	Membership Membership
	// KubeletHealthy reports whether the local kubelet is up and healthy.
	KubeletHealthy bool
	// ControlPlaneReachable reports whether the target control-plane endpoint is
	// reachable from this node (relevant before a join).
	ControlPlaneReachable bool
}

// Prober observes the node's actual state without mutating it. Implementations
// MUST honor the context deadline so probing is bounded (issue #4099-1).
type Prober interface {
	Probe(ctx context.Context) (State, error)
}
