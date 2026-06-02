package provider

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/kairos-io/kairos-sdk/clusterplugin"
	"github.com/sirupsen/logrus"

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

	state, err := actualstate.FileProber{
		RootPath:              pctx.RootPath,
		ControlPlaneReachable: cpReachable,
	}.Probe(ctx)
	if err != nil {
		return err
	}
	// Upgrade target wiring (the operator-pinned version) lands in a later slice;
	// passing "" means no upgrade is evaluated and the bootstrap/join logic runs.
	actions := reconcile.Plan(role, "", state)

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
	}

	logrus.Infof("provider-kubernetes: reconciling role=%q membership=%q actions=%v", role, state.Membership, actions)
	if err := (reconcile.Reconciler{Budget: reconcile.DefaultBudget(), Exec: exec}).Run(ctx, actions); err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	return nil
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
