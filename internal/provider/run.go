package provider

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/kairos-io/kairos-sdk/clusterplugin"
	"github.com/sirupsen/logrus"

	"github.com/kairos-io/provider-kubernetes/internal/etcdsnapshot"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadm/action"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadm/credential"
	"github.com/kairos-io/provider-kubernetes/internal/reconcile"
	"github.com/kairos-io/provider-kubernetes/internal/reconcile/actualstate"
)

// Options configures a reconcile pass.
type Options struct {
	// Runner invokes the kubeadm binary (ExecRunner in production).
	Runner kubeadm.Runner
	// RunDir is the ephemeral directory for transient secret-bearing config; empty
	// defaults to /run.
	RunDir string
	// CPReachableProbe overrides the default TCP-dial probe for control-plane
	// reachability. When nil, a bounded TCP-dial to the controlPlaneEndpoint is
	// used. Inject a custom probe in tests to avoid real network calls.
	CPReachableProbe func(ctx context.Context) bool

	// --- Upgrade probes (ADR-12); nil -> production exec defaults. Injectable for
	// hardware-free tests. Only consulted when an upgrade target is pinned. ---
	// ClusterVersionProbe reads the cluster's current version (kubeadm-config CM).
	ClusterVersionProbe func(ctx context.Context) string
	// RunningKubeletVersionProbe reads this node's running kubelet version.
	RunningKubeletVersionProbe func(ctx context.Context) string
	// EncryptionConfirmed reports whether the etcd-snapshot dir is encrypted.
	EncryptionConfirmed func(ctx context.Context) bool
	// KubeletRestart restarts the kubelet after an upgrade; nil -> systemctl.
	KubeletRestart func(ctx context.Context) error
}

// Run executes one bounded reconcile pass for the given cluster. It is the runtime
// entrypoint (invoked by the reconcile subcommand that the emitted yip stage runs
// on every boot). It is idempotent: when the node is already converged, Plan yields
// no actions and Run is a fast no-op (ADR-4). It never blocks indefinitely: all
// work is bounded by the reconcile Budget (issue #4099-1).
func Run(ctx context.Context, cluster clusterplugin.Cluster, opts Options) error {
	pctx, err := NewContext(cluster)
	if err != nil {
		return err
	}
	if pctx.TokenWarning != "" {
		logrus.Warnf("provider-kubernetes: %s", pctx.TokenWarning)
	}

	detected, err := kubeadm.DetectVersion(ctx, opts.Runner)
	if err != nil {
		return err
	}
	uc, err := ParseUserConfig(pctx.UserOptions)
	if err != nil {
		return err
	}
	resolved, err := kubeadm.Resolve(detected, uc.ClusterConfiguration.KubernetesVersion)
	if err != nil {
		return err
	}
	in, endpointWarn, err := BuildInput(pctx, uc, resolved)
	if err != nil {
		return err
	}
	// HA-1: log any non-fatal endpoint advisory returned by BuildInput.
	if endpointWarn != "" {
		logrus.Warnf("provider-kubernetes: %s", endpointWarn)
	}

	role := actualstate.Role(pctx.Role)

	// HA-3: inject a bounded CP reachability probe for all roles, including init,
	// so Plan can detect an already-serving CP at role=init and refuse to clobber
	// it (ADR-11 #2). Worker/CP joins already used this probe; now init does too.
	// The probe is injectable via Options.CPReachableProbe for test isolation.
	cpReachable := opts.CPReachableProbe
	if cpReachable == nil {
		cpReachable = makeCPReachableProbe(in.ControlPlaneEndpoint)
	}

	// ADR-12: an upgrade is intended only when the operator PINS kubernetesVersion
	// (explicit-pin trigger). The pin equals the bundled binary by ADR-3 (Resolve),
	// so the target is the resolved version. A newer binary without a pin does NOT
	// auto-upgrade. The version probes are wired only when a target is set, so the
	// no-upgrade path stays free of cluster/kubelet version reads.
	target := ""
	prober := actualstate.FileProber{RootPath: pctx.RootPath, ControlPlaneReachable: cpReachable}
	if uc.ClusterConfiguration.KubernetesVersion != "" {
		target = resolved
		prober.ClusterVersion = opts.ClusterVersionProbe
		if prober.ClusterVersion == nil {
			prober.ClusterVersion = clusterVersionViaKubectl(pctx.RootPath)
		}
		prober.RunningKubeletVersion = opts.RunningKubeletVersionProbe
		if prober.RunningKubeletVersion == nil {
			prober.RunningKubeletVersion = runningKubeletVersionViaKubectl(pctx.RootPath)
		}
	}

	state, err := prober.Probe(ctx)
	if err != nil {
		return err
	}
	actions := reconcile.Plan(role, target, state)

	var join *credential.JoinMaterial
	if role == actualstate.RoleWorker || role == actualstate.RoleControlPlane {
		if join, err = BuildJoinMaterial(pctx, uc); err != nil {
			return err
		}
	}

	runDir := opts.RunDir
	if runDir == "" {
		runDir = "/run"
	}

	exec := &action.KubeadmExecutor{
		Runner:      opts.Runner,
		Minter:      credential.Minter{Runner: opts.Runner, RootPath: pctx.RootPath, RunDir: runDir},
		RootPath:    pctx.RootPath,
		RunDir:      runDir,
		Role:        role,
		Input:       in,
		Join:        join,
		CPReachable: cpReachable,
		// ADR-12 upgrade wiring.
		TargetVersion:       target,
		ClusterVersion:      state.ClusterVersion,
		ClusterVersionProbe: prober.ClusterVersion, // re-check + follower wait (nil when no target)
		KubeletRestart:      opts.KubeletRestart,   // nil -> systemctl (production)
	}
	// Best-effort pre-apply etcd snapshot on a control plane only (ADR-12 U5).
	if target != "" && (role == actualstate.RoleInit || role == actualstate.RoleControlPlane) {
		encConfirmed := opts.EncryptionConfirmed
		if encConfirmed == nil {
			encConfirmed = encryptionConfirmedDefault(etcdsnapshot.DefaultDir)
		}
		exec.SnapshotEtcd = func(c context.Context) error {
			return etcdsnapshot.Run(c, etcdsnapshot.Options{RootPath: pctx.RootPath, EncryptionConfirmed: encConfirmed})
		}
	}

	// Upgrade actions get the larger upgrade budget (slower; apply not freely
	// retried); everything else uses the default bounded budget (ADR-12 / #4099-1).
	budget := reconcile.DefaultBudget()
	if containsUpgradeAction(actions) {
		budget = reconcile.UpgradeBudget()
	}

	logrus.Infof("provider-kubernetes: reconciling role=%q membership=%q actions=%v", role, state.Membership, actions)
	if err := (reconcile.Reconciler{Budget: budget, Exec: exec}).Run(ctx, actions); err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	return nil
}

// containsUpgradeAction reports whether any planned action is an upgrade action.
func containsUpgradeAction(actions []reconcile.Action) bool {
	for _, a := range actions {
		switch a {
		case reconcile.ActionUpgradeApply, reconcile.ActionUpgradeNode,
			reconcile.ActionWaitForClusterUpgrade, reconcile.ActionRefuseUpgrade:
			return true
		}
	}
	return false
}

// makeCPReachableProbe returns a bounded TCP reachability probe for the given
// endpoint ("host:port"). Returns nil if the endpoint is empty (callers treat
// nil as unreachable). The probe attempts a TCP dial with a 5s deadline so it
// never hangs (design principle 4 / #4099-1).
func makeCPReachableProbe(endpoint string) func(ctx context.Context) bool {
	if endpoint == "" {
		return nil
	}
	return func(ctx context.Context) bool {
		dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", endpoint)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}
}
