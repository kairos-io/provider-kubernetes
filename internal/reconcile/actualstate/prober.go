package actualstate

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
)

// FileProber detects cluster membership from on-disk kubeadm artifacts under
// RootPath and delegates liveness checks to injected functions, so it is testable
// without a real node or network. It performs no mutation (ADR-4).
//
// Membership heuristic (refined in later slices as needed):
//   - admin.conf present     -> Initialized (this node runs a control plane)
//   - else kubelet.conf only -> Joined
//   - else                   -> Uninitialized
type FileProber struct {
	// RootPath is the cluster root (ProviderOptions["cluster_root_path"], default "/").
	RootPath string
	// KubeletHealthy reports kubelet liveness; nil is treated as not healthy.
	KubeletHealthy func(ctx context.Context) bool
	// ControlPlaneReachable reports CP endpoint reachability; nil is treated as not reachable.
	ControlPlaneReachable func(ctx context.Context) bool
	// ClusterVersion reports the cluster's current Kubernetes version (from the
	// kubeadm-config ConfigMap); nil yields "" (ADR-12). Injected so probing stays
	// hardware-free.
	ClusterVersion func(ctx context.Context) string
	// RunningKubeletVersion reports THIS node's running kubelet version; nil yields
	// "" (ADR-12 worker convergence signal). Injected for testability.
	RunningKubeletVersion func(ctx context.Context) string
	// APIServerReachable reports whether the LOCAL apiserver answers /healthz; nil
	// yields false (ADR-12-R1: drives the kubelet-config repair decision). Injected.
	APIServerReachable func(ctx context.Context) bool
}

// Probe reads the node's actual state. It never mutates the node.
func (p FileProber) Probe(ctx context.Context) (State, error) {
	s := State{Membership: p.membership()}
	if p.KubeletHealthy != nil {
		s.KubeletHealthy = p.KubeletHealthy(ctx)
	}
	if p.ControlPlaneReachable != nil {
		s.ControlPlaneReachable = p.ControlPlaneReachable(ctx)
	}
	// ADR-12 upgrade signals.
	s.NodeComponentVersion = p.nodeComponentVersion()
	if p.ClusterVersion != nil {
		s.ClusterVersion = p.ClusterVersion(ctx)
	}
	if p.RunningKubeletVersion != nil {
		s.RunningKubeletVersion = p.RunningKubeletVersion(ctx)
	}
	if p.APIServerReachable != nil {
		s.APIServerReachable = p.APIServerReachable(ctx)
	}
	return s, nil
}

// apiserverImageRe extracts the kube-apiserver image tag from a static-pod
// manifest, e.g. "v1.34.0" from "image: registry.k8s.io/kube-apiserver:v1.34.0".
var apiserverImageRe = regexp.MustCompile(`kube-apiserver:(v[0-9]+\.[0-9]+\.[0-9]+[^\s"']*)`)

// nodeComponentVersion reads this control plane's kube-apiserver static-pod
// manifest image tag under RootPath (ADR-12 per-node CP convergence signal).
// Empty on workers / pre-init / unparseable.
func (p FileProber) nodeComponentVersion() string {
	path := filepath.Join(p.RootPath, "etc", "kubernetes", "manifests", "kube-apiserver.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if m := apiserverImageRe.FindStringSubmatch(string(data)); len(m) == 2 {
		return m[1]
	}
	return ""
}

func (p FileProber) membership() Membership {
	k8sDir := filepath.Join(p.RootPath, "etc", "kubernetes")
	if fileExists(filepath.Join(k8sDir, "admin.conf")) {
		return Initialized
	}
	if fileExists(filepath.Join(k8sDir, "kubelet.conf")) {
		return Joined
	}
	return Uninitialized
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
