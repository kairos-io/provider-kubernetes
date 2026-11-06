package provider

import (
	"fmt"

	"github.com/kairos-io/kairos-sdk/clusterplugin"

	"github.com/kairos-io/provider-kubernetes/internal/reconcile/actualstate"
	"github.com/kairos-io/provider-kubernetes/internal/reset"
)

const (
	defaultRootPath        = "/"
	providerOptRootPathKey = "cluster_root_path"

	// ClusterStatePath is the tmpfs path where Provider() serializes the Cluster
	// for the reconcile subcommand to consume at boot. /run is tmpfs on every
	// supported distro, so this file is wiped on reboot (OQ-7: persist no
	// bootstrap secrets). Permissions are 0600 root:root.
	ClusterStatePath = "/run/provider-kubernetes/cluster.json"

	// ReconcileLogPath is where the boot-time reconcile subcommand writes its
	// stdout/stderr; surfacing failures to operators (OQ-4 logs-only for v1).
	ReconcileLogPath = "/var/log/provider-kubernetes-reconcile.log"

	// ImportLogPath is where the boot-time import-images step (ADR-16) writes its
	// stdout/stderr. The import runs as a yip step ordered BEFORE reconcile so the
	// pre-bundled control-plane images are in containerd before kubeadm init/join
	// runs (a systemd oneshot alone races the kairos-agent reconcile stage).
	ImportLogPath = "/var/log/provider-kubernetes-image-import.log"

	// The Layer-1 structured status paths are defined in internal/status:
	//   status.StatusRunPath = "/run/provider-kubernetes/status.yaml"  (tmpfs, 0640)
	//   status.StatusLogPath = "/var/log/provider-kubernetes/status.yaml"  (persistent, 0640)
	// See ADR-4-S for the full design.
)

// Context is the parsed, validated provider input derived from a Cluster. It is a
// plain value with no I/O, so it (and the parsing) is unit-testable without
// hardware (design principle 6).
type Context struct {
	Role              string
	RootPath          string
	ControlPlaneHost  string
	ClusterToken      string
	UserOptions       string
	Env               map[string]string
	CACerts           []string
	ClusterConfigPath string

	// TokenWarning is a non-fatal advisory from cluster_token validation (OQ-8).
	TokenWarning string
}

// rootPathSupported reports whether NewContext accepts a validated, cleaned
// cluster_root_path. kubeadm init, join and upgrade write /etc/kubernetes and
// /var/lib/kubelet on the host whatever the root is, so "/" is the only root
// under which the prober, the join minter and reset find that state. Tests
// swap it to run the pipeline against a temporary root.
var rootPathSupported = func(root string) bool { return root == defaultRootPath }

// NewContext parses and validates a Cluster into a provider Context. It fails
// fast on invalid input (e.g. an empty/too-short cluster_token, a role other
// than init, controlplane or worker, or a cluster_root_path other than "/") so the provider never proceeds on bad
// configuration.
func NewContext(cluster clusterplugin.Cluster) (Context, error) {
	warning, err := ValidateClusterToken(cluster.ClusterToken)
	if err != nil {
		return Context{}, err
	}
	if err := validateRole(cluster.Role); err != nil {
		return Context{}, err
	}

	rootPath := defaultRootPath
	if v := cluster.ProviderOptions[providerOptRootPathKey]; v != "" {
		rootPath = v
	}
	clean, err := reset.ValidateRoot(rootPath)
	if err != nil {
		return Context{}, fmt.Errorf("cluster_root_path: %w", err)
	}
	if !rootPathSupported(clean) {
		return Context{}, fmt.Errorf("cluster_root_path: only %q is supported, got %q: kubeadm keeps its state in the host's /etc/kubernetes and /var/lib/kubelet",
			defaultRootPath, rootPath)
	}

	return Context{
		Role:              string(cluster.Role),
		RootPath:          rootPath,
		ControlPlaneHost:  cluster.ControlPlaneHost,
		ClusterToken:      cluster.ClusterToken,
		UserOptions:       cluster.Options,
		Env:               cluster.Env,
		CACerts:           cluster.CACerts,
		ClusterConfigPath: cluster.ClusterConfigPath,
		TokenWarning:      warning,
	}, nil
}

// validateRole accepts only the exact role names the reconcile planner acts on.
// Anything else (empty, "master", "control-plane", "Worker", ...) would plan no
// action at all and leave the node looking converged.
func validateRole(role clusterplugin.Role) error {
	switch actualstate.Role(role) {
	case actualstate.RoleInit, actualstate.RoleControlPlane, actualstate.RoleWorker:
		return nil
	}
	return fmt.Errorf("unsupported cluster role %q: must be one of %q, %q or %q",
		string(role), actualstate.RoleInit, actualstate.RoleControlPlane, actualstate.RoleWorker)
}
