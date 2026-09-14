package action

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"

	"github.com/kairos-io/provider-kubernetes/internal/etcdsnapshot"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadm/credential"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadmconfig"
	"github.com/kairos-io/provider-kubernetes/internal/reconcile"
	"github.com/kairos-io/provider-kubernetes/internal/reconcile/actualstate"
)

// captureLogs installs a fresh logrus test hook on the package-level standard
// logger (which logrus.Infof/Warnf/etc. write through) and restores the prior
// hook set on test cleanup, so log-assertion tests never leak state into other
// tests.
func captureLogs(t *testing.T) *logrustest.Hook {
	t.Helper()
	logger := logrus.StandardLogger()
	saved := logger.ReplaceHooks(make(logrus.LevelHooks))
	t.Cleanup(func() { logger.ReplaceHooks(saved) })
	return logrustest.NewLocal(logger)
}

type fakeRunner struct {
	calls   [][]string
	respond func(args []string) (kubeadm.Result, error)
}

func (f *fakeRunner) Run(_ context.Context, args ...string) (kubeadm.Result, error) {
	f.calls = append(f.calls, args)
	if f.respond != nil {
		return f.respond(args)
	}
	return kubeadm.Result{}, nil
}

// configArg returns the value following "--config" in args, or "".
func configArg(args []string) string {
	for i, a := range args {
		if a == "--config" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestRunInitMaterializesAndShreds(t *testing.T) {
	runDir := t.TempDir()
	var initConfigPath, capturedContent string

	fr := &fakeRunner{respond: func(args []string) (kubeadm.Result, error) {
		switch {
		case args[0] == "token" && args[1] == "generate":
			return kubeadm.Result{Stdout: "abcdef.0123456789abcdef\n"}, nil
		case args[0] == "certs" && args[1] == "certificate-key":
			return kubeadm.Result{Stdout: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"}, nil
		case args[0] == "init":
			initConfigPath = configArg(args)
			b, _ := os.ReadFile(initConfigPath) // file must exist during exec
			capturedContent = string(b)
			return kubeadm.Result{}, nil
		}
		return kubeadm.Result{}, nil
	}}

	e := &KubeadmExecutor{
		Runner:   fr,
		Minter:   credential.Minter{Runner: fr, RootPath: "/"},
		RootPath: "/",
		RunDir:   runDir,
		Role:     actualstate.RoleInit,
		Input:    kubeadmconfig.Input{KubernetesVersion: "v1.34.0", ControlPlaneEndpoint: "10.0.0.1:6443"},
		TokenTTL: time.Hour,
	}
	if err := e.Execute(context.Background(), reconcile.ActionRunInit); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if initConfigPath == "" {
		t.Fatal("kubeadm init was not invoked with --config")
	}
	// During exec the config carried the bounded token + cert key.
	if !strings.Contains(capturedContent, "abcdef.0123456789abcdef") || !strings.Contains(capturedContent, "ttl: 1h0m0s") {
		t.Fatalf("init config missing bounded bootstrap token: %s", capturedContent)
	}
	if !strings.Contains(capturedContent, "certificateKey:") {
		t.Fatalf("init config missing certificate key")
	}
	// --upload-certs must be present.
	last := fr.calls[len(fr.calls)-1]
	if strings.Join(last, " ") != "init --config "+initConfigPath+" --upload-certs" {
		t.Fatalf("unexpected init argv: %v", last)
	}
	// M2: the certificate key must never be passed on argv (it flows through the
	// 0600 config), and no expiry-mutation of kubeadm-certs is invoked.
	for _, c := range fr.calls {
		if strings.Contains(strings.Join(c, " "), "--certificate-key") {
			t.Fatalf("certificate key must not appear on argv: %v", c)
		}
	}
	// The transient config must be shredded after exec.
	if _, err := os.Stat(initConfigPath); !os.IsNotExist(err) {
		t.Fatalf("transient init config was not shredded: stat err=%v", err)
	}
}

func TestRunJoinWorkerPinsCAAndShreds(t *testing.T) {
	runDir := t.TempDir()
	var joinConfigPath, content string
	fr := &fakeRunner{respond: func(args []string) (kubeadm.Result, error) {
		if args[0] == "join" {
			joinConfigPath = configArg(args)
			b, _ := os.ReadFile(joinConfigPath)
			content = string(b)
		}
		return kubeadm.Result{}, nil
	}}

	e := &KubeadmExecutor{
		Runner:   fr,
		RootPath: "/",
		RunDir:   runDir,
		Role:     actualstate.RoleWorker,
		Input:    kubeadmconfig.Input{ControlPlaneEndpoint: "10.0.0.1:6443"},
		Join: &credential.JoinMaterial{
			Token:        "abcdef.0123456789abcdef",
			CACertHashes: []string{"sha256:deadbeef"},
		},
	}
	if err := e.Execute(context.Background(), reconcile.ActionRunJoin); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(content, "sha256:deadbeef") || !strings.Contains(content, "unsafeSkipCAVerification: false") {
		t.Fatalf("join config not CA-pinned: %s", content)
	}
	if _, err := os.Stat(joinConfigPath); !os.IsNotExist(err) {
		t.Fatalf("transient join config was not shredded")
	}
}

func TestRunJoinRequiresMaterial(t *testing.T) {
	e := &KubeadmExecutor{Runner: &fakeRunner{}, RunDir: t.TempDir(), Role: actualstate.RoleWorker}
	if err := e.Execute(context.Background(), reconcile.ActionRunJoin); err == nil {
		t.Fatal("expected error when join material is missing")
	}
}

func TestWaitForControlPlane(t *testing.T) {
	e := &KubeadmExecutor{CPReachable: func(context.Context) bool { return true }}
	if err := e.Execute(context.Background(), reconcile.ActionWaitForControlPlane); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// nil checker is a no-op (the bounded join attempt surfaces unreachability).
	e2 := &KubeadmExecutor{}
	if err := e2.Execute(context.Background(), reconcile.ActionWaitForControlPlane); err != nil {
		t.Fatalf("unexpected error with nil checker: %v", err)
	}
}

func TestExecuteNoneAndUnknown(t *testing.T) {
	e := &KubeadmExecutor{}
	if err := e.Execute(context.Background(), reconcile.ActionNone); err != nil {
		t.Fatalf("ActionNone must be a no-op, got %v", err)
	}
	if err := e.Execute(context.Background(), reconcile.Action("bogus")); err == nil {
		t.Fatal("expected error for unknown action")
	}
}

func TestExecuteRefuseInitIsHardError(t *testing.T) {
	e := &KubeadmExecutor{
		Input: kubeadmconfig.Input{ControlPlaneEndpoint: "10.0.0.1:6443"},
	}
	err := e.Execute(context.Background(), reconcile.ActionRefuseInit)
	if err == nil {
		t.Fatal("ActionRefuseInit must return a hard error")
	}
	// The error message must name the endpoint and guide the operator.
	if !strings.Contains(err.Error(), "10.0.0.1:6443") {
		t.Fatalf("ActionRefuseInit error must name the endpoint, got: %v", err)
	}
	if !strings.Contains(err.Error(), "role=controlplane") {
		t.Fatalf("ActionRefuseInit error must mention role=controlplane, got: %v", err)
	}
}

// HA-4: worker join does NOT run the /readyz health gate; it goes straight through
// after TCP reachability.
func TestWaitForControlPlane_WorkerSkipsHealthGate(t *testing.T) {
	reached := false
	e := &KubeadmExecutor{
		Role:        actualstate.RoleWorker,
		Input:       kubeadmconfig.Input{ControlPlaneEndpoint: "10.0.0.1:6443"},
		CPReachable: func(context.Context) bool { reached = true; return true },
	}
	if err := e.Execute(context.Background(), reconcile.ActionWaitForControlPlane); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reached {
		t.Fatal("CPReachable probe must be called for worker join")
	}
	// For workers, the method returns after TCP reachability (no /readyz) -- the
	// test just verifies it returns nil quickly without hanging.
}

// HA-4: CP join /readyz gate: when endpoint is empty, the health check is skipped.
func TestWaitForCPHealthy_EmptyEndpointSkips(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	e := &KubeadmExecutor{
		Role:        actualstate.RoleControlPlane,
		Input:       kubeadmconfig.Input{ControlPlaneEndpoint: ""},
		CPReachable: func(context.Context) bool { return true },
	}
	// Should return nil immediately (no endpoint, no dial attempt).
	if err := e.Execute(ctx, reconcile.ActionWaitForControlPlane); err != nil {
		t.Fatalf("empty endpoint must skip health gate, got: %v", err)
	}
}

// ADR-12 U4: upgrade executor actions.

func hasFlag(calls [][]string, flag string) bool {
	for _, c := range calls {
		for _, a := range c {
			if strings.Contains(a, flag) {
				return true
			}
		}
	}
	return false
}

func TestUpgradeApply(t *testing.T) {
	r := &fakeRunner{}
	restarted := false
	e := &KubeadmExecutor{
		Runner:         r,
		Role:           actualstate.RoleControlPlane,
		TargetVersion:  "v1.35.0",
		ClusterVersion: "v1.34.8",
		KubeletRestart: func(context.Context) error { restarted = true; return nil },
	}
	if err := e.Execute(context.Background(), reconcile.ActionUpgradeApply); err != nil {
		t.Fatalf("upgrade apply: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected 1 kubeadm call, got %v", r.calls)
	}
	got := strings.Join(r.calls[0], " ")
	if got != "upgrade apply v1.35.0 --yes --certificate-renewal=true" {
		t.Fatalf("unexpected argv: %q", got)
	}
	// B3: must NOT re-upload certs; B1: no cert-key on argv.
	if hasFlag(r.calls, "--upload-certs") || hasFlag(r.calls, "--certificate-key") {
		t.Fatalf("upgrade apply must not carry --upload-certs/--certificate-key: %v", r.calls)
	}
	if !restarted {
		t.Fatal("expected kubelet restart after apply")
	}
}

func TestUpgradeApplyPreApplyRecheckDegradesToNode(t *testing.T) {
	// If the cluster already reached the target (another CP applied first), the
	// pre-apply re-check must run `upgrade node` instead of a second apply, and
	// must NOT invoke SnapshotEtcd (no destructive apply is about to happen).
	r := &fakeRunner{}
	snapshotCalls := 0
	e := &KubeadmExecutor{
		Runner:              r,
		Role:                actualstate.RoleControlPlane,
		TargetVersion:       "v1.35.0",
		ClusterVersionProbe: func(context.Context) string { return "v1.35.4" },
		KubeletRestart:      func(context.Context) error { return nil },
		SnapshotEtcd: func(context.Context) etcdsnapshot.Result {
			snapshotCalls++
			return etcdsnapshot.Result{Outcome: etcdsnapshot.OutcomeTaken}
		},
	}
	if err := e.Execute(context.Background(), reconcile.ActionUpgradeApply); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(r.calls) != 1 || strings.Join(r.calls[0], " ") != "upgrade node" {
		t.Fatalf("expected a single 'upgrade node' call after race re-check, got %v", r.calls)
	}
	if hasFlag(r.calls, "apply") {
		t.Fatalf("must not apply when cluster already at target: %v", r.calls)
	}
	if snapshotCalls != 0 {
		t.Fatalf("SnapshotEtcd must not be called when the pre-apply re-check degrades to upgrade node, called %d times", snapshotCalls)
	}
}

// ADR-12-A1: every SnapshotEtcd outcome must (a) never block `upgrade apply`
// (the argv is unchanged regardless of outcome) and (b) produce exactly one
// "etcd-snapshot outcome=" log line, at Info for taken/already-taken and Warn
// for everything else.
func TestUpgradeApply_SnapshotOutcomeLogging(t *testing.T) {
	cases := []struct {
		outcome   etcdsnapshot.Outcome
		wantLevel logrus.Level
	}{
		{etcdsnapshot.OutcomeTaken, logrus.InfoLevel},
		{etcdsnapshot.OutcomeAlreadyTaken, logrus.InfoLevel},
		{etcdsnapshot.OutcomeExternalEtcd, logrus.WarnLevel},
		{etcdsnapshot.OutcomeEncryptionUnconfirmed, logrus.WarnLevel},
		{etcdsnapshot.OutcomeEtcdctlMissing, logrus.WarnLevel},
		{etcdsnapshot.OutcomeInsufficientSpace, logrus.WarnLevel},
		{etcdsnapshot.OutcomeFailed, logrus.WarnLevel},
	}
	for _, tc := range cases {
		t.Run(string(tc.outcome), func(t *testing.T) {
			hook := captureLogs(t)

			r := &fakeRunner{}
			restarted := false
			e := &KubeadmExecutor{
				Runner:         r,
				Role:           actualstate.RoleControlPlane,
				TargetVersion:  "v1.35.0",
				ClusterVersion: "v1.34.8",
				KubeletRestart: func(context.Context) error { restarted = true; return nil },
				SnapshotEtcd: func(context.Context) etcdsnapshot.Result {
					return etcdsnapshot.Result{Outcome: tc.outcome, Path: "/some/path", Detail: "some detail"}
				},
			}
			if err := e.Execute(context.Background(), reconcile.ActionUpgradeApply); err != nil {
				t.Fatalf("upgrade apply: %v", err)
			}

			// Apply must ALWAYS proceed with the exact existing argv, regardless of
			// the snapshot outcome.
			if len(r.calls) != 1 || strings.Join(r.calls[0], " ") != "upgrade apply v1.35.0 --yes --certificate-renewal=true" {
				t.Fatalf("unexpected argv for outcome %s: %v", tc.outcome, r.calls)
			}
			if !restarted {
				t.Fatalf("expected kubelet restart after apply for outcome %s", tc.outcome)
			}

			var matches []*logrus.Entry
			for _, e := range hook.AllEntries() {
				if strings.Contains(e.Message, "etcd-snapshot outcome=") {
					matches = append(matches, e)
				}
			}
			if len(matches) != 1 {
				t.Fatalf("expected exactly 1 'etcd-snapshot outcome=' log entry, got %d: %v", len(matches), hook.AllEntries())
			}
			if !strings.Contains(matches[0].Message, "outcome="+string(tc.outcome)) {
				t.Fatalf("log entry does not name the outcome: %q", matches[0].Message)
			}
			if matches[0].Level != tc.wantLevel {
				t.Fatalf("outcome %s logged at %s, want %s", tc.outcome, matches[0].Level, tc.wantLevel)
			}
		})
	}
}

// ADR-1-A1/E-B5: defaultKubeletRestart runs through kubeadm.SystemctlRunner(),
// daemon-reload then restart kubelet, in order, stopping at the first error.
func TestRestartKubeletVia_OrderAndArgv(t *testing.T) {
	r := &fakeRunner{}
	if err := restartKubeletVia(context.Background(), r); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := [][]string{{"daemon-reload"}, {"restart", "kubelet"}}
	if len(r.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", r.calls, want)
	}
	for i, args := range want {
		if strings.Join(r.calls[i], " ") != strings.Join(args, " ") {
			t.Fatalf("call %d = %v, want %v", i, r.calls[i], args)
		}
	}
}

func TestRestartKubeletVia_StopsOnFirstError(t *testing.T) {
	r := &fakeRunner{respond: func(args []string) (kubeadm.Result, error) {
		if args[0] == "daemon-reload" {
			return kubeadm.Result{}, errors.New("daemon-reload failed")
		}
		return kubeadm.Result{}, nil
	}}
	err := restartKubeletVia(context.Background(), r)
	if err == nil {
		t.Fatal("expected an error when daemon-reload fails")
	}
	if !strings.Contains(err.Error(), "daemon-reload") {
		t.Fatalf("error does not name the failing command: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected exactly 1 call (stop on first error), got %v", r.calls)
	}
}

func TestUpgradeNode(t *testing.T) {
	r := &fakeRunner{}
	restarted := false
	e := &KubeadmExecutor{
		Runner:         r,
		Role:           actualstate.RoleWorker,
		KubeletRestart: func(context.Context) error { restarted = true; return nil },
	}
	if err := e.Execute(context.Background(), reconcile.ActionUpgradeNode); err != nil {
		t.Fatalf("upgrade node: %v", err)
	}
	if len(r.calls) != 1 || strings.Join(r.calls[0], " ") != "upgrade node" {
		t.Fatalf("unexpected argv: %v", r.calls)
	}
	if !restarted {
		t.Fatal("expected kubelet restart after upgrade node")
	}
}

func TestRefuseUpgradeIsTerminal(t *testing.T) {
	e := &KubeadmExecutor{
		Role:           actualstate.RoleControlPlane,
		ClusterVersion: "v1.34.0",
		TargetVersion:  "v1.36.0", // skip-level
	}
	err := e.Execute(context.Background(), reconcile.ActionRefuseUpgrade)
	if err == nil {
		t.Fatal("expected a terminal refuse-upgrade error")
	}
	if !errors.Is(err, reconcile.ErrTerminal) {
		t.Fatalf("refuse-upgrade must wrap ErrTerminal, got %v", err)
	}
}

func TestWaitForClusterUpgrade(t *testing.T) {
	// Already flipped -> returns immediately.
	e := &KubeadmExecutor{
		TargetVersion:       "v1.35.0",
		ClusterVersionProbe: func(context.Context) string { return "v1.35.4" },
	}
	if err := e.Execute(context.Background(), reconcile.ActionWaitForClusterUpgrade); err != nil {
		t.Fatalf("expected nil when cluster already at target, got %v", err)
	}

	// Not flipped + bounded ctx -> loud timeout, never hangs.
	e2 := &KubeadmExecutor{
		TargetVersion:       "v1.35.0",
		ClusterVersionProbe: func(context.Context) string { return "v1.34.8" },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := e2.Execute(ctx, reconcile.ActionWaitForClusterUpgrade); err == nil {
		t.Fatal("expected a bounded timeout error when the cluster never flips")
	}
}

// ADR-12-R1 U9: kubelet-config repair (init phase kubelet-start, API-free, no secret).
func TestRepairKubeletConfig(t *testing.T) {
	runDir := t.TempDir()
	var cfgPath, content string
	fr := &fakeRunner{respond: func(args []string) (kubeadm.Result, error) {
		if len(args) >= 3 && args[0] == "init" && args[1] == "phase" && args[2] == "kubelet-start" {
			cfgPath = configArg(args)
			b, _ := os.ReadFile(cfgPath)
			content = string(b)
		}
		return kubeadm.Result{}, nil
	}}
	e := &KubeadmExecutor{
		Runner:            fr,
		RootPath:          "/",
		RunDir:            runDir,
		Role:              actualstate.RoleControlPlane,
		Input:             kubeadmconfig.Input{KubernetesVersion: "v1.35.0", ControlPlaneEndpoint: "10.0.0.1:6443", ServiceSubnet: "10.96.0.0/12"},
		LocalAPIReachable: func(context.Context) bool { return true }, // API returns after repair
	}
	if err := e.Execute(context.Background(), reconcile.ActionRepairKubeletConfig); err != nil {
		t.Fatalf("repair: %v", err)
	}
	if cfgPath == "" {
		t.Fatal("kubeadm init phase kubelet-start was not invoked with --config")
	}
	// argv is exactly the kubelet-start phase with --config (no API, no secret flags).
	last := strings.Join(fr.calls[len(fr.calls)-1], " ")
	if last != "init phase kubelet-start --config "+cfgPath {
		t.Fatalf("unexpected repair argv: %q", last)
	}
	// The rendered config must carry NO secret (no bootstrap token / cert key).
	if strings.Contains(content, "bootstrapTokens") || strings.Contains(content, "certificateKey") || strings.Contains(content, "token:") {
		t.Fatalf("repair config must contain no secret material: %s", content)
	}
	// Transient config shredded.
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatalf("repair transient config was not shredded")
	}
}

func TestRepairKubeletConfig_BoundedWait(t *testing.T) {
	// If the local API never returns, the repair fails loud within ctx (never hangs).
	e := &KubeadmExecutor{
		Runner:            &fakeRunner{},
		RunDir:            t.TempDir(),
		Role:              actualstate.RoleControlPlane,
		Input:             kubeadmconfig.Input{ControlPlaneEndpoint: "10.0.0.1:6443"},
		LocalAPIReachable: func(context.Context) bool { return false },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := e.Execute(ctx, reconcile.ActionRepairKubeletConfig); err == nil {
		t.Fatal("expected a bounded error when the local API never returns")
	}
}

// C2: with a custom serviceSubnet, runInit emits a KubeletConfiguration document
// pinning clusterDNS to the derived DNS IP. With the default (empty) subnet it
// emits no KubeletConfiguration (kubeadm derives clusterDNS itself).
func TestRunInitEmitsKubeletClusterDNSForCustomSubnet(t *testing.T) {
	capture := func(in kubeadmconfig.Input) string {
		t.Helper()
		var content string
		fr := &fakeRunner{respond: func(args []string) (kubeadm.Result, error) {
			switch {
			case args[0] == "token" && args[1] == "generate":
				return kubeadm.Result{Stdout: "abcdef.0123456789abcdef\n"}, nil
			case args[0] == "certs" && args[1] == "certificate-key":
				return kubeadm.Result{Stdout: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"}, nil
			case args[0] == "init":
				b, _ := os.ReadFile(configArg(args))
				content = string(b)
			}
			return kubeadm.Result{}, nil
		}}
		e := &KubeadmExecutor{
			Runner: fr, Minter: credential.Minter{Runner: fr, RootPath: "/"},
			RootPath: "/", RunDir: t.TempDir(), Role: actualstate.RoleInit,
			Input: in, TokenTTL: time.Hour,
		}
		if err := e.Execute(context.Background(), reconcile.ActionRunInit); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return content
	}

	custom := capture(kubeadmconfig.Input{
		KubernetesVersion:    "v1.34.0",
		ControlPlaneEndpoint: "10.0.0.1:6443",
		ServiceSubnet:        "172.20.0.0/16",
	})
	if !strings.Contains(custom, "kind: KubeletConfiguration") {
		t.Fatalf("custom subnet init config missing KubeletConfiguration:\n%s", custom)
	}
	if !strings.Contains(custom, "172.20.0.10") {
		t.Fatalf("custom subnet init config missing derived clusterDNS 172.20.0.10:\n%s", custom)
	}

	def := capture(kubeadmconfig.Input{
		KubernetesVersion:    "v1.34.0",
		ControlPlaneEndpoint: "10.0.0.1:6443",
	})
	if strings.Contains(def, "kind: KubeletConfiguration") {
		t.Fatalf("default subnet must NOT emit a KubeletConfiguration:\n%s", def)
	}
}

// C2/ADR-17: the join path must NOT emit a KubeletConfiguration even with a custom
// serviceSubnet -- joining nodes inherit clusterDNS from the kubelet-config
// ConfigMap kubeadm uploaded at init. Locks the init-emits/join-omits split.
func TestRunJoinOmitsKubeletConfiguration(t *testing.T) {
	var content string
	fr := &fakeRunner{respond: func(args []string) (kubeadm.Result, error) {
		if args[0] == "join" {
			b, _ := os.ReadFile(configArg(args))
			content = string(b)
		}
		return kubeadm.Result{}, nil
	}}
	e := &KubeadmExecutor{
		Runner: fr, RootPath: "/", RunDir: t.TempDir(), Role: actualstate.RoleWorker,
		Input: kubeadmconfig.Input{ControlPlaneEndpoint: "10.0.0.1:6443", ServiceSubnet: "172.20.0.0/16"},
		Join: &credential.JoinMaterial{
			Token:        "abcdef.0123456789abcdef",
			CACertHashes: []string{"sha256:deadbeef"},
		},
	}
	if err := e.Execute(context.Background(), reconcile.ActionRunJoin); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(content, "kind: KubeletConfiguration") {
		t.Fatalf("join config must NOT contain a KubeletConfiguration:\n%s", content)
	}
}
