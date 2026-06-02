// Package action implements reconcile.Executor by turning planned actions into
// bounded kubeadm invocations (ADR-1 argv, ADR-4 bounded, ADR-10 operator-driven
// join). Credential-bearing config is materialized to a 0600 file on ephemeral
// storage and shredded immediately after the kubeadm process returns (OQ-7); secret
// values are never logged.
package action

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadm/credential"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadmconfig"
	"github.com/kairos-io/provider-kubernetes/internal/reconcile"
	"github.com/kairos-io/provider-kubernetes/internal/reconcile/actualstate"
	"github.com/kairos-io/provider-kubernetes/internal/securefile"
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
	// RunDir is the directory for transient secret-bearing config. It MUST be an
	// ephemeral (tmpfs) directory; empty defaults to /run. Pointing it at a
	// persistent filesystem would defeat the shred guarantee (OQ-7).
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
	case reconcile.ActionRefuseInit:
		// Terminal: a verdict that will not change on retry (fail fast, still loud).
		return fmt.Errorf("%w: a control plane already answers at %q; this node is role=init but the cluster exists. Use role=controlplane to join", reconcile.ErrTerminal, e.Input.ControlPlaneEndpoint)
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
	path, err := securefile.WriteTransient(e.runDir(), content)
	if err != nil {
		return err
	}
	defer securefile.Shred(path)

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
	in.DiscoveryFilePath = e.Join.DiscoveryFilePath
	in.JoinAsControlPlane = e.Role == actualstate.RoleControlPlane

	joinCfg, err := kubeadmconfig.BuildJoinConfiguration(in) // enforces CA pinning
	if err != nil {
		return err
	}
	content, err := kubeadmconfig.Marshal(joinCfg)
	if err != nil {
		return err
	}
	path, err := securefile.WriteTransient(e.runDir(), content)
	if err != nil {
		return err
	}
	defer securefile.Shred(path)

	if _, err := e.Runner.Run(ctx, "join", "--config", path); err != nil {
		return fmt.Errorf("kubeadm join: %w", err)
	}
	return nil
}

// waitForControlPlane waits until the CP endpoint is reachable (TCP-level) and,
// for control-plane joins, until the CP /readyz endpoint reports healthy (HA-4).
// The overall wait is bounded by ctx (which the reconcile.Reconciler sets from
// Budget.PerAttempt); it never hangs (design principle 4 / #4099-1).
func (e *KubeadmExecutor) waitForControlPlane(ctx context.Context) error {
	if e.CPReachable == nil {
		return nil // no checker; the bounded join attempt will surface unreachability
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for !e.CPReachable(ctx) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("control plane not reachable within budget: %w", ctx.Err())
		case <-ticker.C:
		}
	}
	// HA-4: for control-plane joins, add a bounded /readyz health gate so we do
	// not join a quorum that is TCP-reachable but not yet healthy. Worker joins
	// keep the existing behavior (kubeadm join handles the API availability wait).
	if e.Role == actualstate.RoleControlPlane {
		return e.waitForCPHealthy(ctx)
	}
	return nil
}

// waitForCPHealthy polls https://<endpoint>/readyz with a 5s per-probe timeout
// until the CP reports healthy or ctx expires. This is the HA-4 bounded health
// gate before a CP join. No distributed lock: the operator delivers one CP-join
// cloud-config at a time (ADR-11 #4).
func (e *KubeadmExecutor) waitForCPHealthy(ctx context.Context) error {
	endpoint := e.Input.ControlPlaneEndpoint
	if endpoint == "" {
		// No endpoint configured; skip and let kubeadm report the error.
		return nil
	}
	url := "https://" + endpoint + "/readyz"
	// Use an HTTP client that skips TLS verification only for the readyz probe:
	// we are checking liveness, not authenticating; the real CA-pinned join
	// follows immediately after. This avoids needing the CA at probe time.
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsInsecure(),
		},
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	logrus.Infof("provider-kubernetes: waiting for control-plane health at %s (CP join health gate)", url)
	for {
		resp, err := client.Get(url) //nolint:noctx // bounded by ticker + ctx.Done below
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				logrus.Infof("provider-kubernetes: control-plane healthy (%s)", url)
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("control plane not healthy within budget (%s): %w", url, ctx.Err())
		case <-ticker.C:
		}
	}
}
