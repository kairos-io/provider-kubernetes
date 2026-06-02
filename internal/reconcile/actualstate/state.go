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
	// BinaryVersion is the bundled kubeadm binary version on this node (e.g.
	// "v1.35.0"); empty if not probed. The desired/target version for an upgrade
	// (ADR-12: the pinned version equals the bundled binary version).
	BinaryVersion string
	// ClusterVersion is the cluster's current Kubernetes version as recorded in the
	// kubeadm-config ConfigMap (e.g. "v1.34.0"); empty if not probed or not yet a
	// member. It flips to the target as soon as the FIRST control plane runs
	// `kubeadm upgrade apply`, so it answers a cluster-wide question, not a per-node
	// one (ADR-12).
	ClusterVersion string
	// NodeComponentVersion is THIS control-plane node's component version, read from
	// the kube-apiserver static-pod manifest image tag under
	// /etc/kubernetes/manifests (e.g. "v1.34.0"); empty on workers or pre-init. It
	// is the per-node convergence signal for a control plane: it reaches the target
	// only after this node runs `kubeadm upgrade apply` (first CP) or `upgrade node`
	// (other CPs) (ADR-12).
	NodeComponentVersion string
	// RunningKubeletVersion is THIS node's RUNNING kubelet version (e.g. "v1.34.0");
	// empty if not probed. It is the per-node convergence signal for a worker: post
	// image-swap the kubelet binary on disk is already the target, but the running
	// kubelet stays the old version until `kubeadm upgrade node` + kubelet restart,
	// so this distinguishes "cluster flipped" from "this node converged" (ADR-12).
	RunningKubeletVersion string
}

// Prober observes the node's actual state without mutating it. Implementations
// MUST honor the context deadline so probing is bounded (issue #4099-1).
type Prober interface {
	Probe(ctx context.Context) (State, error)
}
