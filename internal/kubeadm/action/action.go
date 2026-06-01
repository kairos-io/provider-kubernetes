// Package action implements reconcile.Executor by turning planned actions into
// bounded kubeadm invocations (ADR-1 argv, ADR-4 bounded, ADR-10 operator-driven
// join). Credential-bearing config is materialized to a 0600 file on ephemeral
// storage and shredded immediately after the kubeadm process returns (OQ-7); secret
// values are never logged.
package action

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadm/credential"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadmconfig"
	"github.com/kairos-io/provider-kubernetes/internal/reconcile"
	"github.com/kairos-io/provider-kubernetes/internal/reconcile/actualstate"
)

// defaultTokenTTL is the bounded bootstrap-token TTL (ADR-10 decision).
const defaultTokenTTL = time.Hour

// KubeadmExecutor implements reconcile.Executor. It is constructed by the provider
// reconcile pipeline with already-merged Input and (for joiners) operator-delivered
// JoinMaterial.
type KubeadmExecutor struct {
	Runner   kubeadm.Runner
	Minter   credential.Minter
	RootPath string
	// RunDir is the ephemeral directory for transient secret-bearing config; empty
	// defaults to /run (tmpfs).
	RunDir string
	Role   actualstate.Role
	Input  kubeadmconfig.Input
	// Join is the operator-delivered join material (controlplane/worker). Nil on the
	// init node. Joiners never mint it themselves (ADR-10).
	Join *credential.JoinMaterial
	// TokenTTL bounds the init bootstrap token; zero defaults to defaultTokenTTL.
	TokenTTL time.Duration
	// CPReachable is a bounded reachability probe used by ActionWaitForControlPlane.
	CPReachable func(ctx context.Context) bool
}

// Execute runs a single planned action. The reconcile.Reconciler bounds ctx.
func (e *KubeadmExecutor) Execute(ctx context.Context, a reconcile.Action) error {
	switch a {
	case reconcile.ActionNone:
		return nil
	case reconcile.ActionWaitForControlPlane:
		return e.waitForControlPlane(ctx)
	case reconcile.ActionRunInit:
		return e.runInit(ctx)
	case reconcile.ActionRunJoin:
		return e.runJoin(ctx)
	default:
		return fmt.Errorf("unknown action %q", a)
	}
}

func (e *KubeadmExecutor) runDir() string {
	if e.RunDir != "" {
		return e.RunDir
	}
	return "/run"
}

func (e *KubeadmExecutor) tokenTTL() time.Duration {
	if e.TokenTTL > 0 {
		return e.TokenTTL
	}
	return defaultTokenTTL
}

func (e *KubeadmExecutor) runInit(ctx context.Context) error {
	// Pre-seed a bounded-TTL bootstrap token and a fresh certificate key (both via
	// kubeadm CSPRNG; never derived from cluster_token) so init never leaves the
	// 24h default token and uploads certs under a fresh key (ADR-2).
	token, err := e.Minter.GenerateToken(ctx)
	if err != nil {
		return err
	}
	certKey, err := e.Minter.GenerateCertificateKey(ctx)
	if err != nil {
		return err
	}

	cluster := kubeadmconfig.BuildClusterConfiguration(e.Input)
	initCfg := kubeadmconfig.BuildInitConfiguration(e.Input)
	initCfg.BootstrapTokens = []kubeadmconfig.BootstrapToken{{
		Token:  token,
		TTL:    e.tokenTTL().String(),
		Usages: []string{"signing", "authentication"},
		Groups: []string{"system:bootstrappers:kubeadm:default-node-token"},
	}}
	initCfg.CertificateKey = certKey

	content, err := kubeadmconfig.Marshal(cluster, initCfg)
	if err != nil {
		return err
	}
	path, err := writeTransient(e.runDir(), content)
	if err != nil {
		return err
	}
	defer shred(path)

	// stdout is intentionally not logged: kubeadm init prints the join command
	// (including the token) on success.
	if _, err := e.Runner.Run(ctx, "init", "--config", path, "--upload-certs"); err != nil {
		return fmt.Errorf("kubeadm init: %w", err)
	}
	return nil
}

func (e *KubeadmExecutor) runJoin(ctx context.Context) error {
	if e.Join == nil {
		return fmt.Errorf("join requires delivered join material, but none was provided")
	}

	in := e.Input
	in.JoinToken = e.Join.Token
	in.CACertHashes = e.Join.CACertHashes
	in.CertificateKey = e.Join.CertificateKey
	in.JoinAsControlPlane = e.Role == actualstate.RoleControlPlane

	joinCfg, err := kubeadmconfig.BuildJoinConfiguration(in) // enforces CA pinning
	if err != nil {
		return err
	}
	content, err := kubeadmconfig.Marshal(joinCfg)
	if err != nil {
		return err
	}
	path, err := writeTransient(e.runDir(), content)
	if err != nil {
		return err
	}
	defer shred(path)

	if _, err := e.Runner.Run(ctx, "join", "--config", path); err != nil {
		return fmt.Errorf("kubeadm join: %w", err)
	}
	return nil
}

func (e *KubeadmExecutor) waitForControlPlane(ctx context.Context) error {
	if e.CPReachable == nil {
		return nil // no checker; the bounded join attempt will surface unreachability
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if e.CPReachable(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("control plane not reachable within budget: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// writeTransient writes content to a fresh 0600 file under dir (ephemeral storage).
func writeTransient(dir, content string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create run dir: %w", err)
	}
	f, err := os.CreateTemp(dir, "kubeadm-*.yaml")
	if err != nil {
		return "", fmt.Errorf("create transient config: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Chmod(0o600); err != nil {
		return "", fmt.Errorf("chmod transient config: %w", err)
	}
	if _, err := f.WriteString(content); err != nil {
		return "", fmt.Errorf("write transient config: %w", err)
	}
	return f.Name(), nil
}

// shred best-effort overwrites then removes a transient secret file. On tmpfs
// (/run) removal already prevents persistence; the overwrite is defense-in-depth.
func shred(path string) {
	if info, err := os.Stat(path); err == nil {
		_ = os.WriteFile(path, make([]byte, info.Size()), 0o600)
	}
	_ = os.Remove(path)
}
