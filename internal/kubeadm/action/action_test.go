package action

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadm/credential"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadmconfig"
	"github.com/kairos-io/provider-kubernetes/internal/reconcile"
	"github.com/kairos-io/provider-kubernetes/internal/reconcile/actualstate"
)

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
