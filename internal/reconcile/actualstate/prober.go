package actualstate

import (
	"context"
	"os"
	"path/filepath"
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
	return s, nil
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
