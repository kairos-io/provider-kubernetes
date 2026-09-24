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
	"github.com/kairos-io/provider-kubernetes/internal/kubeadmconfig"
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
	// (ADR-4-S, S4). Nil defaults to newDefaultStatusSink: a FileSink writing to
	// both the /run and /var/log production paths plus the Node-annotation sink.
	// Inject a fake in tests.
	StatusSink status.StatusSink

	// --- Upgrade probes (ADR-12); nil -> production exec defaults. Injectable for
	// hardware-free tests. Only consulted when an upgrade target is pinned. ---
	// ClusterVersionProbe reads the cluster's current version (kubeadm-config CM).
	ClusterVersionProbe func(ctx context.Context) string
	// RunningKubeletVersionProbe reads this node's running kubelet version.
	RunningKubeletVersionProbe func(ctx context.Context) string
	// EncryptionConfirmed reports whether the given etcd-snapshot dir is confirmed
	// encrypted at rest. nil -> etcdsnapshot.EncryptedAtRest (the fail-closed
	// sysfs dm-crypt gate).
	EncryptionConfirmed func(ctx context.Context, dir string) bool
	// KubeletRestart restarts the kubelet after an upgrade; nil -> systemctl.
	KubeletRestart func(ctx context.Context) error
	// APIServerReachableProbe reports whether the LOCAL apiserver answers /healthz
	// (ADR-12-R1); nil -> a /healthz probe to 127.0.0.1 on the cluster's
	// localAPIEndpoint.bindPort (6443 when unset). Unlike the other probes in
	// this block it is consulted on every pass, pinned or not: on an
	// Initialized node it decides whether the control plane counts as healthy,
	// and it is polled through a bounded grace period (controlplanehealth.go)
	// before a down apiserver is reported.
	APIServerReachableProbe func(ctx context.Context) bool

	// KubeletHealthyProbe reports local kubelet liveness (actualstate.State.
	// KubeletHealthy), which reconcile.Plan uses to distinguish a fully
	// converged established member from one that is already Initialized/
	// Joined but degraded (status.PhaseDegraded, D-2). nil -> kubeletHealthyProbe():
	// a bounded GET of the kubelet's own loopback healthz
	// (http://127.0.0.1:10248/healthz, kubelethealth.go) -- the same check
	// kubeadm itself waits on. Inject a fake in tests that need a healthy or
	// degraded fixture without a real kubelet.
	KubeletHealthyProbe func(ctx context.Context) bool
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
	// S4+S3: construct the status sink once at the top. In production this is
	// newDefaultStatusSink's MultiSink{FileSink, NodeAnnotationSink}: Layer 1
	// (always-written local file) plus Layer 2 (post-membership Node annotation via
	// kubectl-argv, no-op when no kubeconfig exists). rootPath is extracted here
	// (before NewContext) so the NodeAnnotationSink can be wired before the pctx
	// parse error path. It uses the same ProviderOptions key as NewContext
	// (providerOptRootPathKey / cluster_root_path).
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
		sink, annotSink = newDefaultStatusSink(rootPath)
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
		// finalVerdict is reconcile.Plan's member verdict. A degraded one (an
		// already-established member whose kubelet is down, whose init never
		// finished, or whose control plane is not serving) must only ever
		// suppress a would-be Converged status, never mask a real error --
		// BuildStatus only consults it on the p.Err == nil path.
		finalVerdict reconcile.Verdict
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
			Verdict:    finalVerdict,
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
		cpReachable = makeCPReachableProbe(joinDialEndpoint(role, in))
	}

	// ADR-12: an upgrade is intended only when the operator PINS kubernetesVersion
	// (explicit-pin trigger). The pin equals the bundled binary by ADR-3 (Resolve),
	// so the target is the resolved version. A newer binary without a pin does NOT
	// auto-upgrade. The version probes are wired only when a target is set, so the
	// no-upgrade path stays free of cluster/kubelet version reads.
	target := ""
	prober := actualstate.FileProber{
		RootPath:              pctx.RootPath,
		ControlPlaneReachable: cpReachable,
	}
	// D-2: unconditional (unlike the upgrade-only probes below), since Plan's
	// base (non-upgrade) path also needs KubeletHealthy to distinguish a
	// converged member from a degraded one. nil -> the production default,
	// a bounded loopback healthz probe (kubelethealth.go).
	prober.KubeletHealthy = opts.KubeletHealthyProbe
	if prober.KubeletHealthy == nil {
		prober.KubeletHealthy = kubeletHealthyProbe()
	}
	if uc.ClusterConfiguration.KubernetesVersion != "" {
		target = resolved
		// One kubectl runner for both probes (ADR-1-A1): absolute path, closed
		// environment, never PATH-resolved.
		kubectlRunner := kubeadm.KubectlRunner()
		prober.ClusterVersion = opts.ClusterVersionProbe
		if prober.ClusterVersion == nil {
			prober.ClusterVersion = clusterVersionViaKubectl(pctx.RootPath, kubectlRunner)
		}
		prober.RunningKubeletVersion = opts.RunningKubeletVersionProbe
		if prober.RunningKubeletVersion == nil {
			prober.RunningKubeletVersion = runningKubeletVersionViaKubectl(pctx.RootPath, kubectlRunner)
		}
	}
	// Unconditional as well: on an Initialized node Plan's base path needs it to
	// tell a serving control plane from one that is not (a live kubelet says
	// nothing about the static pods it runs), and the upgrade path uses the
	// same answer to decide on a kubelet-config repair (ADR-12-R1).
	prober.APIServerReachable = opts.APIServerReachableProbe
	if prober.APIServerReachable == nil {
		prober.APIServerReachable = localAPIHealthyProbe(in.BindPort)
	}

	state, err := prober.Probe(ctx)
	finalState = state // capture for defer regardless of error
	if err != nil {
		finalErr = err
		return err
	}
	actions, verdict := reconcile.Plan(role, target, state)
	if verdict == reconcile.VerdictControlPlaneUnhealthy {
		// After a reboot the kubelet starts the static pods only once it is up
		// itself, so a healthy control plane's apiserver is often still coming
		// up while this pass runs. Give it a bounded grace period before
		// reporting it, and re-plan on what is then observed.
		logrus.Infof("provider-kubernetes: the local apiserver is not answering /healthz yet; waiting up to %s before reporting it", controlPlaneGrace)
		if awaitLocalAPIServer(ctx, prober.APIServerReachable, controlPlaneGrace, controlPlanePoll) {
			logrus.Info("provider-kubernetes: the local apiserver is answering /healthz")
			state.APIServerReachable = true
			finalState = state
			actions, verdict = reconcile.Plan(role, target, state)
		} else {
			logrus.Warnf("provider-kubernetes: the local apiserver did not answer /healthz within %s", controlPlaneGrace)
		}
	}
	finalVerdict = verdict

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
		LocalAPIReachable:   prober.APIServerReachable, // post-repair local-API wait
	}
	// Best-effort pre-apply etcd snapshot on a control plane only (ADR-12 U5,
	// revised by ADR-12-A1). etcdsnapshot.Run defaults EncryptionConfirmed to its
	// own fail-closed sysfs gate when opts.EncryptionConfirmed is nil.
	if target != "" && (role == actualstate.RoleInit || role == actualstate.RoleControlPlane) {
		exec.SnapshotEtcd = func(c context.Context) etcdsnapshot.Result {
			return etcdsnapshot.Run(c, etcdsnapshot.Options{
				RootPath:            pctx.RootPath,
				TargetMinor:         kubeadm.Minor(target),
				EncryptionConfirmed: opts.EncryptionConfirmed,
			})
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

// newDefaultStatusSink builds the production status sink Run uses when
// Options.StatusSink is nil (ADR-4-S): Layer 1, a FileSink on the /run and
// /var/log paths, fanned out with Layer 2, a NodeAnnotationSink that execs
// kubectl against the kubeconfig under rootPath. The annotation sink is also
// returned so Run can late-bind the kubeadm node name into it (Finding D).
// Both layers act on the host, so this is a variable: the package tests replace
// it with a guard that fails any test reaching it.
var newDefaultStatusSink = func(rootPath string) (status.StatusSink, *status.NodeAnnotationSink) {
	// Construct with empty nodeName; ResolveNode falls back to os.Hostname() until
	// Run updates it with the kubeadm node name after BuildInput.
	annot := status.NewNodeAnnotationSink(rootPath, "")
	return status.MultiSink{status.NewFileSink(), annot}, annot
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

// joinDialEndpoint returns the endpoint the reachability probe should dial, which
// is the one kubeadm itself will dial. For a joining node that is the join-scoped
// apiServerEndpoint when the operator set one; everywhere else it is the
// cluster-wide controlPlaneEndpoint. Probing a different address than the join
// uses would report a reachable control plane and then fail in kubeadm join.
func joinDialEndpoint(role actualstate.Role, in kubeadmconfig.Input) string {
	if role != actualstate.RoleWorker && role != actualstate.RoleControlPlane {
		return in.ControlPlaneEndpoint
	}
	if in.JoinAPIServerEndpoint != "" {
		return in.JoinAPIServerEndpoint
	}
	return in.ControlPlaneEndpoint
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
