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
	"os/exec"
	"strings"
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

	// --- Upgrade (ADR-12) ---
	// TargetVersion is the operator-pinned upgrade target (e.g. "v1.35.0"); empty
	// when no upgrade is in play. ClusterVersion is the observed cluster version at
	// plan time (used to render the RefuseUpgrade message).
	TargetVersion  string
	ClusterVersion string
	// ClusterVersionProbe re-reads the cluster's current version (kubeadm-config) for
	// the pre-apply lost-race re-check and the follower wait; nil disables them.
	ClusterVersionProbe func(ctx context.Context) string
	// KubeletRestart restarts the local kubelet after an upgrade; nil uses systemctl.
	KubeletRestart func(ctx context.Context) error
	// LocalAPIReachable reports whether the LOCAL apiserver answers; used after a
	// kubelet-config repair (ADR-12-R1). nil uses an HTTPS /healthz probe to
	// 127.0.0.1:6443. Injectable for tests.
	LocalAPIReachable func(ctx context.Context) bool
	// SnapshotEtcd, when set, is invoked best-effort before `upgrade apply` on a
	// control plane (ADR-12 U5). nil skips it. It must never block (bounded by ctx).
	SnapshotEtcd func(ctx context.Context) error
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
	case reconcile.ActionRepairKubeletConfig:
		return e.runRepairKubeletConfig(ctx)
	case reconcile.ActionUpgradeApply:
		return e.runUpgradeApply(ctx)
	case reconcile.ActionUpgradeNode:
		return e.runUpgradeNode(ctx)
	case reconcile.ActionWaitForClusterUpgrade:
		return e.waitForClusterUpgrade(ctx)
	case reconcile.ActionRefuseUpgrade:
		// Terminal: an unsafe transition (downgrade / skip-level / out-of-window).
		_, err := kubeadm.UpgradePath(e.ClusterVersion, e.TargetVersion)
		if err == nil {
			err = fmt.Errorf("unsafe upgrade refused (cluster %q -> target %q)", e.ClusterVersion, e.TargetVersion)
		}
		return fmt.Errorf("%w: %v", reconcile.ErrTerminal, err)
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

// runUpgradeApply runs `kubeadm upgrade apply <target>` on the apply-authority
// control plane (ADR-12). A pre-apply re-check guards the lost race: if the cluster
// is already at the target minor (another CP applied), this node converges via
// `upgrade node` instead. Certificate renewal is enabled explicitly; --upload-certs
// is NEVER passed (the root-equivalent cert-key stays mint-only, ADR-11 #3). No
// secret is on argv, and stdout is not logged (upgrade can print cert hints).
func (e *KubeadmExecutor) runUpgradeApply(ctx context.Context) error {
	if e.TargetVersion == "" {
		return fmt.Errorf("upgrade apply: no target version set")
	}
	if e.ClusterVersionProbe != nil {
		if cur := e.ClusterVersionProbe(ctx); sameMinor(cur, e.TargetVersion) {
			logrus.Infof("provider-kubernetes: cluster already at %s; running upgrade node instead of apply", kubeadm.Minor(cur))
			return e.runUpgradeNode(ctx)
		}
	}
	// U5: best-effort etcd snapshot before the destructive apply (bounded; never
	// blocks the upgrade -- failures are logged below). On a bounded retry of apply
	// this runs again; that is harmless (the snapshot is single-retained and the
	// pre-apply re-check above degrades a retry to upgrade-node once the cluster has
	// actually flipped).
	if e.SnapshotEtcd != nil {
		if err := e.SnapshotEtcd(ctx); err != nil {
			logrus.Warnf("provider-kubernetes: pre-upgrade etcd snapshot did not complete: %v (continuing)", err)
		}
	}
	if _, err := e.Runner.Run(ctx, "upgrade", "apply", e.TargetVersion, "--yes", "--certificate-renewal=true"); err != nil {
		return fmt.Errorf("kubeadm upgrade apply %s: %w", e.TargetVersion, err)
	}
	return e.restartKubelet(ctx)
}

// runRepairKubeletConfig regenerates /var/lib/kubelet/kubeadm-flags.env and
// config.yaml with the NEW kubeadm from LOCAL config, then restarts the kubelet
// (ADR-12-R1). After a Kairos A/B image swap the new kubelet can crashloop on a
// flag the old kubeadm wrote but the new kubelet removed (e.g.
// --pod-infra-container-image in 1.35), which leaves the control plane down. This
// is `kubeadm init phase kubelet-start`, which is API-free and touches no PKI,
// etcd, or static-pod manifests. The rendered config carries NO secret (no
// bootstrap token, no certificate key) -- it is still 0600 on tmpfs and shredded
// for consistency. It then bounded-waits for the local apiserver so the subsequent
// `upgrade apply` can reach the control plane.
func (e *KubeadmExecutor) runRepairKubeletConfig(ctx context.Context) error {
	cluster := kubeadmconfig.BuildClusterConfiguration(e.Input)
	initCfg := kubeadmconfig.BuildInitConfiguration(e.Input)
	content, err := kubeadmconfig.Marshal(cluster, initCfg)
	if err != nil {
		return err
	}
	path, err := securefile.WriteTransient(e.runDir(), content)
	if err != nil {
		return err
	}
	defer securefile.Shred(path)

	if _, err := e.Runner.Run(ctx, "init", "phase", "kubelet-start", "--config", path); err != nil {
		return fmt.Errorf("kubeadm init phase kubelet-start: %w", err)
	}
	// Wait (bounded) for the local control plane to return before upgrade apply.
	return e.waitForLocalAPIHealthy(ctx)
}

// waitForLocalAPIHealthy polls the LOCAL apiserver /healthz until it answers or
// ctx expires (ADR-12-R1). Liveness only (InsecureSkipVerify), like the HA-4 CP
// join gate; the upgrade that follows is the real, authenticated operation.
func (e *KubeadmExecutor) waitForLocalAPIHealthy(ctx context.Context) error {
	reachable := e.LocalAPIReachable
	if reachable == nil {
		client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsInsecure()}}
		reachable = func(context.Context) bool {
			resp, err := client.Get("https://127.0.0.1:6443/healthz") //nolint:noctx // bounded by the loop below
			if err != nil {
				return false
			}
			_ = resp.Body.Close()
			return resp.StatusCode == http.StatusOK
		}
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	logrus.Info("provider-kubernetes: waiting for local control plane to return after kubelet-config repair")
	for !reachable(ctx) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("local control plane did not return after kubelet-config repair within budget: %w", ctx.Err())
		case <-ticker.C:
		}
	}
	logrus.Info("provider-kubernetes: local control plane healthy after repair")
	return nil
}

// runUpgradeNode runs `kubeadm upgrade node` on a follower control plane or a
// worker to converge this node's components to the cluster target (ADR-12).
func (e *KubeadmExecutor) runUpgradeNode(ctx context.Context) error {
	if _, err := e.Runner.Run(ctx, "upgrade", "node"); err != nil {
		return fmt.Errorf("kubeadm upgrade node: %w", err)
	}
	return e.restartKubelet(ctx)
}

// waitForClusterUpgrade blocks (bounded by ctx) until the cluster version reaches
// the target minor -- i.e. an apply-authority control plane has applied -- before a
// follower runs `upgrade node` (ADR-12). Without a probe it falls back to the CP
// health gate. It never loops unbounded (#4099-1).
func (e *KubeadmExecutor) waitForClusterUpgrade(ctx context.Context) error {
	if e.ClusterVersionProbe == nil {
		return e.waitForCPHealthy(ctx)
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for !sameMinor(e.ClusterVersionProbe(ctx), e.TargetVersion) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("cluster did not reach %s within budget: %w", e.TargetVersion, ctx.Err())
		case <-ticker.C:
		}
	}
	return nil
}

// restartKubelet restarts the local kubelet after an upgrade (its config and, for
// CPs, the static-pod manifests have changed). Injectable for tests.
func (e *KubeadmExecutor) restartKubelet(ctx context.Context) error {
	if e.KubeletRestart != nil {
		return e.KubeletRestart(ctx)
	}
	return defaultKubeletRestart(ctx)
}

// defaultKubeletRestart runs `systemctl daemon-reload` then `systemctl restart
// kubelet` via argv (no shell). Bounded by ctx.
func defaultKubeletRestart(ctx context.Context) error {
	for _, args := range [][]string{{"daemon-reload"}, {"restart", "kubelet"}} {
		out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// sameMinor reports whether two versions share a major.minor (e.g. "v1.34.8" and
// "v1.34.0"). Invalid/empty versions never match a valid one.
func sameMinor(a, b string) bool {
	ma, mb := kubeadm.Minor(a), kubeadm.Minor(b)
	return ma != "" && ma == mb
}
