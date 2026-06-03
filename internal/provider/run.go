package provider

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/kairos-io/kairos-sdk/clusterplugin"
	"github.com/sirupsen/logrus"

	"github.com/kairos-io/provider-kubernetes/internal/etcdsnapshot"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadm/action"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadm/credential"
	"github.com/kairos-io/provider-kubernetes/internal/reconcile"
	"github.com/kairos-io/provider-kubernetes/internal/reconcile/actualstate"
	"github.com/kairos-io/provider-kubernetes/internal/status"
	"github.com/kairos-io/provider-kubernetes/version"
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

	// StatusSink is the destination for the structured reconcile status
	// (ADR-4-S, S4). Nil defaults to a FileSink writing to both the
	// /run and /var/log production paths. Inject a fake in tests.
	StatusSink status.StatusSink

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
	// APIServerReachableProbe reports whether the LOCAL apiserver answers /healthz
	// (ADR-12-R1); nil -> a /healthz probe to 127.0.0.1:6443.
	APIServerReachableProbe func(ctx context.Context) bool
}

// Options also carries an injectable StatusSink for testing; nil -> production
// FileSink writing to both StatusRunPath and StatusLogPath.
//
// Run executes one bounded reconcile pass for the given cluster. It is the runtime
// entrypoint (invoked by the reconcile subcommand that the emitted yip stage runs
// on every boot). It is idempotent: when the node is already converged, Plan yields
// no actions and Run is a fast no-op (ADR-4). It never blocks indefinitely: all
// work is bounded by the reconcile Budget (issue #4099-1).
//
// Every return path (success AND failure) records a structured status to the
// Layer-1 local channel (ADR-4-S, S4). The status write is best-effort: it is
// bounded to 2s per path, errors are logged and swallowed, and the real reconcile
// exit code is never masked.
func Run(ctx context.Context, cluster clusterplugin.Cluster, opts Options) error {
	// S4+S3: construct the status sink once at the top. In production this is a
	// MultiSink{FileSink, NodeAnnotationSink}: Layer 1 (always-written local file)
	// plus Layer 2 (post-membership Node annotation via kubectl-argv, no-op when no
	// kubeconfig exists). rootPath is extracted here (before NewContext) so the
	// NodeAnnotationSink can be wired before the pctx parse error path. It uses the
	// same ProviderOptions key as NewContext (providerOptRootPathKey / cluster_root_path).
	// Tests may inject their own sink via opts.StatusSink.
	sink := opts.StatusSink
	// annotSink is the Layer-2 NodeAnnotationSink. It is declared here so it can
	// be updated after BuildInput resolves the kubeadm node name (Finding D).
	// It is nil when opts.StatusSink is provided (injected by tests).
	var annotSink *status.NodeAnnotationSink
	if sink == nil {
		rootPath := defaultRootPath
		if v := cluster.ProviderOptions[providerOptRootPathKey]; v != "" {
			rootPath = v
		}
		// Construct with empty nodeName; ResolveNode will fall back to os.Hostname().
		// After BuildInput we update ResolveNode to prefer the kubeadm node name.
		annotSink = status.NewNodeAnnotationSink(rootPath, "")
		sink = status.MultiSink{
			status.NewFileSink(),
			annotSink,
		}
	}

	bootID := readBootID()
	providerVersion := version.Version

	pctx, err := NewContext(cluster)
	if err != nil {
		// Early-exit before we have role/membership; write a ConfigInvalid status.
		recordConfigInvalid(ctx, sink, cluster, err, bootID, providerVersion)
		return err
	}
	if pctx.TokenWarning != "" {
		logrus.Warnf("provider-kubernetes: %s", pctx.TokenWarning)
	}

	role := actualstate.Role(pctx.Role)

	// S4: status state that the defer captures by pointer so the final defer
	// always sees the last written values regardless of which return fires.
	var (
		finalResult     reconcile.RunResult
		finalErr        error
		finalState      actualstate.State
		finalLastAction reconcile.Action
	)
	// Defer record-then-return: runs exactly once on every exit path.
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		sink.Record(sctx, status.BuildStatus(status.BuildParams{
			Role:       role,
			Membership: finalState.Membership,
			LastAction: finalLastAction,
			Err:        finalErr,
			Result:     finalResult,
			Now:        time.Now().UTC().Format(time.RFC3339),
			BootID:     bootID,
			Version:    providerVersion,
		}))
	}()

	detected, err := kubeadm.DetectVersion(ctx, opts.Runner)
	if err != nil {
		finalErr = err
		return err
	}
	uc, err := ParseUserConfig(pctx.UserOptions)
	if err != nil {
		finalErr = err
		return err
	}
	resolved, err := kubeadm.Resolve(detected, uc.ClusterConfiguration.KubernetesVersion)
	if err != nil {
		finalErr = err
		return err
	}
	in, endpointWarn, err := BuildInput(pctx, uc, resolved)
	if err != nil {
		finalErr = err
		return err
	}
	// Finding D: wire the kubeadm NodeRegistration.Name into the annotation sink
	// now that BuildInput has resolved it. The deferred sink.Record (and any
	// subsequent Record call on annotSink) will use this precise Kubernetes node
	// name rather than the os.Hostname() fallback. annotSink is nil when
	// opts.StatusSink was injected (tests), so guard before updating.
	if annotSink != nil && in.NodeName != "" {
		annotSink.ResolveNode = status.MakeNodeResolver(in.NodeName)
	}
	// HA-1: log any non-fatal endpoint advisory returned by BuildInput.
	if endpointWarn != "" {
		logrus.Warnf("provider-kubernetes: %s", endpointWarn)
	}

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
		prober.APIServerReachable = opts.APIServerReachableProbe
		if prober.APIServerReachable == nil {
			prober.APIServerReachable = localAPIHealthyProbe()
		}
	}

	state, err := prober.Probe(ctx)
	finalState = state // capture for defer regardless of error
	if err != nil {
		finalErr = err
		return err
	}
	actions := reconcile.Plan(role, target, state)

	var join *credential.JoinMaterial
	if role == actualstate.RoleWorker || role == actualstate.RoleControlPlane {
		if join, err = BuildJoinMaterial(pctx, uc); err != nil {
			finalErr = err
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
		ClusterVersionProbe: prober.ClusterVersion,     // re-check + follower wait (nil when no target)
		KubeletRestart:      opts.KubeletRestart,       // nil -> systemctl (production)
		LocalAPIReachable:   prober.APIServerReachable, // post-repair local-API wait (nil when no target)
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
	reconciler := reconcile.Reconciler{Budget: budget, Exec: exec}
	result, reconcileErr := reconciler.RunWithResult(ctx, actions)
	finalResult = result
	finalLastAction = result.LastAction
	if reconcileErr != nil {
		finalErr = fmt.Errorf("reconcile: %w", reconcileErr)
		return finalErr
	}
	return nil
}

// recordConfigInvalid writes a ConfigInvalid status when Run exits before
// probing state (e.g. bad cluster token, unparseable user config). We use
// the cluster's raw role field because pctx may not be valid.
func recordConfigInvalid(ctx context.Context, sink status.StatusSink, cluster clusterplugin.Cluster, err error, bootID, ver string) {
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sink.Record(sctx, status.BuildStatus(status.BuildParams{
		Role:          actualstate.Role(string(cluster.Role)),
		Membership:    actualstate.Uninitialized,
		LastAction:    reconcile.ActionNone,
		Err:           err,
		ConfigInvalid: true,
		Now:           time.Now().UTC().Format(time.RFC3339),
		BootID:        bootID,
		Version:       ver,
	}))
}

// readBootID reads /proc/sys/kernel/random/boot_id best-effort. Returns empty
// string if the file cannot be read (containers, test environments). The value
// lets readers distinguish a fresh-boot status from a stale /var/log mirror.
func readBootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// containsUpgradeAction reports whether any planned action is an upgrade action.
func containsUpgradeAction(actions []reconcile.Action) bool {
	for _, a := range actions {
		switch a {
		case reconcile.ActionUpgradeApply, reconcile.ActionUpgradeNode,
			reconcile.ActionWaitForClusterUpgrade, reconcile.ActionRefuseUpgrade,
			reconcile.ActionRepairKubeletConfig:
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
