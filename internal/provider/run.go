package provider

import (
	"context"
	"fmt"

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
	in, err := BuildInput(pctx, uc, resolved)
	if err != nil {
		return err
	}

	role := actualstate.Role(pctx.Role)

	state, err := actualstate.FileProber{RootPath: pctx.RootPath}.Probe(ctx)
	if err != nil {
		return err
	}
	actions := reconcile.Plan(role, state)

	var join *credential.JoinMaterial
	if role == actualstate.RoleWorker || role == actualstate.RoleControlPlane {
		if join, err = BuildJoinMaterial(pctx, uc); err != nil {
			return err
		}
	}

	exec := &action.KubeadmExecutor{
		Runner:   opts.Runner,
		Minter:   credential.Minter{Runner: opts.Runner, RootPath: pctx.RootPath},
		RootPath: pctx.RootPath,
		RunDir:   opts.RunDir,
		Role:     role,
		Input:    in,
		Join:     join,
	}

	logrus.Infof("provider-kubernetes: reconciling role=%q membership=%q actions=%v", role, state.Membership, actions)
	if err := (reconcile.Reconciler{Budget: reconcile.DefaultBudget(), Exec: exec}).Run(ctx, actions); err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	return nil
}
