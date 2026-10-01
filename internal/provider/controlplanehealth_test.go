package provider

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kairos-io/kairos-sdk/clusterplugin"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
	"github.com/kairos-io/provider-kubernetes/internal/status"
)

// scriptedProbe answers from answers in order and repeats the last one, and
// counts its calls.
type scriptedProbe struct {
	mu      sync.Mutex
	answers []bool
	calls   int
}

func (p *scriptedProbe) probe(context.Context) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := p.calls
	p.calls++
	if i >= len(p.answers) {
		i = len(p.answers) - 1
	}
	return p.answers[i]
}

// answer adapts the probe to Options.InitFinishedProbe: the cluster always
// answered.
func (p *scriptedProbe) answer(ctx context.Context) (bool, error) {
	return p.probe(ctx), nil
}

// captureLogs installs a fresh logrus test hook on the standard logger and
// restores the previous hooks when the test ends.
func captureLogs(t *testing.T) *logrustest.Hook {
	t.Helper()
	logger := logrus.StandardLogger()
	saved := logger.ReplaceHooks(make(logrus.LevelHooks))
	t.Cleanup(func() { logger.ReplaceHooks(saved) })
	return logrustest.NewLocal(logger)
}

func (p *scriptedProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// initializedControlPlane returns a cluster root holding admin.conf, which
// the prober reads as an Initialized node, and an init-role cluster for it.
func initializedControlPlane(t *testing.T, options string) (string, clusterplugin.Cluster) {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "etc", "kubernetes", "admin.conf"), "apiVersion: v1\n")
	return root, clusterplugin.Cluster{
		Role:             clusterplugin.RoleInit,
		ClusterToken:     validToken(),
		ControlPlaneHost: "10.0.0.1",
		ProviderOptions:  map[string]string{"cluster_root_path": root},
		Options:          options,
	}
}

func versionOnlyRunner() *fakeRunner {
	return &fakeRunner{respond: func(args []string) (kubeadm.Result, error) {
		if args[0] == "version" {
			return kubeadm.Result{Stdout: "v1.35.0\n"}, nil
		}
		return kubeadm.Result{}, nil
	}}
}

func TestControlPlaneGraceProductionValues(t *testing.T) {
	if productionControlPlaneGrace != 3*time.Minute {
		t.Errorf("controlPlaneGrace = %s, want 3m (the documented bound on how long a pass waits)", productionControlPlaneGrace)
	}
	if productionControlPlanePoll != 3*time.Second {
		t.Errorf("controlPlanePoll = %s, want 3s", productionControlPlanePoll)
	}
}

func TestAwaitLocalAPIServer(t *testing.T) {
	t.Run("healthy on a later attempt", func(t *testing.T) {
		p := &scriptedProbe{answers: []bool{false, false, true}}
		if !awaitLocalAPIServer(context.Background(), p.probe, time.Second, time.Millisecond) {
			t.Fatal("want true once the probe answers healthy")
		}
		if p.count() != 3 {
			t.Fatalf("probe called %d times, want 3 (stop at the first healthy answer)", p.count())
		}
	})
	t.Run("never healthy gives up at the grace period", func(t *testing.T) {
		p := &scriptedProbe{answers: []bool{false}}
		grace := 100 * time.Millisecond
		start := time.Now()
		if awaitLocalAPIServer(context.Background(), p.probe, grace, 5*time.Millisecond) {
			t.Fatal("want false when the probe never answers healthy")
		}
		if elapsed := time.Since(start); elapsed < grace || elapsed > grace+2*time.Second {
			t.Fatalf("returned after %s, want about the %s grace period", elapsed, grace)
		}
		if p.count() < 2 {
			t.Fatalf("probe called %d times, want it polled through the grace period", p.count())
		}
	})
	t.Run("nil probe", func(t *testing.T) {
		if awaitLocalAPIServer(context.Background(), nil, time.Second, time.Millisecond) {
			t.Fatal("want false for a nil probe")
		}
	})
	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		p := &scriptedProbe{answers: []bool{true}}
		if awaitLocalAPIServer(ctx, p.probe, time.Minute, time.Millisecond) {
			t.Fatal("want false once the caller's context is done")
		}
	})
	t.Run("no attempt once the context is done, even when a tick is ready too", func(t *testing.T) {
		// With a 1ns tick and an already-canceled context, both select cases
		// are ready on most iterations and Go picks one at random; only the
		// check after the tick keeps the probe from running.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		p := &scriptedProbe{answers: []bool{true}}
		for range 200 {
			if awaitLocalAPIServer(ctx, p.probe, time.Minute, time.Nanosecond) {
				t.Fatal("want false once the caller's context is done")
			}
		}
		if p.count() != 0 {
			t.Fatalf("probe ran %d times on a context that was already done", p.count())
		}
	})
	t.Run("each attempt carries the grace deadline", func(t *testing.T) {
		grace := time.Minute
		var deadline time.Time
		var hasDeadline bool
		probe := func(ctx context.Context) bool {
			deadline, hasDeadline = ctx.Deadline()
			return true
		}
		start := time.Now()
		awaitLocalAPIServer(context.Background(), probe, grace, time.Millisecond)
		end := time.Now()
		if !hasDeadline {
			t.Fatal("the probe's context has no deadline: a stalled attempt could outlive the grace period")
		}
		if deadline.Before(start.Add(grace)) || deadline.After(end.Add(grace)) {
			t.Fatalf("probe deadline %s is not the grace period from the call (between %s and %s)", deadline, start.Add(grace), end.Add(grace))
		}
	})
}

// TestRunControlPlaneNotServingReportsControlPlaneUnhealthy: an Initialized
// node whose kubelet is up but whose apiserver never answers is Degraded,
// not Converged, and only after the grace period was spent polling.
func TestRunControlPlaneNotServingReportsControlPlaneUnhealthy(t *testing.T) {
	_, cluster := initializedControlPlane(t, "")
	fr := versionOnlyRunner()
	opts, sink := hermeticRunOptions(t, fr)
	api := &scriptedProbe{answers: []bool{false}}
	opts.APIServerReachableProbe = api.probe

	if err := Run(context.Background(), cluster, opts); err != nil {
		t.Fatalf("a degraded member is not a failed pass: %v", err)
	}
	if fr.called("init") {
		t.Fatalf("a degraded control plane must not be re-initialized; calls=%v", fr.calls)
	}
	got := sink.only(t)
	if got.Phase != status.PhaseDegraded || got.Reason != status.ReasonControlPlaneUnhealthy {
		t.Fatalf("recorded phase=%q reason=%q, want %q/%q", got.Phase, got.Reason, status.PhaseDegraded, status.ReasonControlPlaneUnhealthy)
	}
	if got.Terminal || got.Outcome != status.OutcomeFailure {
		t.Fatalf("recorded terminal=%t outcome=%q, want non-terminal failure", got.Terminal, got.Outcome)
	}
	if api.count() < 2 {
		t.Fatalf("apiserver probed %d times, want the prober's probe plus polling through the grace period", api.count())
	}
}

// TestRunControlPlaneThatComesUpDuringGraceConverges is the reboot case: the
// apiserver is still starting when the pass probes it, then answers.
func TestRunControlPlaneThatComesUpDuringGraceConverges(t *testing.T) {
	_, cluster := initializedControlPlane(t, "")
	opts, sink := hermeticRunOptions(t, versionOnlyRunner())
	api := &scriptedProbe{answers: []bool{false, false, true}}
	opts.APIServerReachableProbe = api.probe

	if err := Run(context.Background(), cluster, opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := sink.only(t); got.Phase != status.PhaseConverged {
		t.Fatalf("recorded phase=%q reason=%q, want %q", got.Phase, got.Reason, status.PhaseConverged)
	}
	if api.count() != 3 {
		t.Fatalf("apiserver probed %d times, want 3 (stop polling once it answers)", api.count())
	}
}

// TestRunCallerDeadlineShorterThanGrace: production runs the pass without a
// deadline of its own, so only the grace ends the wait. A caller that does set
// a shorter one is told which limit ended it, and the node is still reported
// Degraded, not as a failed pass.
func TestRunCallerDeadlineShorterThanGrace(t *testing.T) {
	hook := captureLogs(t)
	_, cluster := initializedControlPlane(t, "")
	opts, sink := hermeticRunOptions(t, versionOnlyRunner())
	opts.APIServerReachableProbe = func(context.Context) bool { return false }
	ctx, cancel := context.WithTimeout(context.Background(), controlPlaneGrace/6)
	defer cancel()

	err := Run(ctx, cluster, opts)
	got := sink.only(t)
	t.Logf("Run returned %v; recorded phase=%q reason=%q", err, got.Phase, got.Reason)
	if got.Phase != status.PhaseDegraded || got.Reason != status.ReasonControlPlaneUnhealthy {
		t.Fatalf("recorded phase=%q reason=%q, want %q/%q", got.Phase, got.Reason, status.PhaseDegraded, status.ReasonControlPlaneUnhealthy)
	}
	var cutShort, claimedGrace bool
	for _, e := range hook.AllEntries() {
		cutShort = cutShort || strings.Contains(e.Message, "cut short by the pass's own deadline")
		claimedGrace = claimedGrace || strings.Contains(e.Message, "did not answer /healthz within")
	}
	if !cutShort || claimedGrace {
		t.Fatalf("log names the wrong limit: cut short=%t, claimed the full grace=%t", cutShort, claimedGrace)
	}
}

// TestRunHealthyControlPlaneDoesNotWait: a serving apiserver is probed once
// and never polled.
func TestRunHealthyControlPlaneDoesNotWait(t *testing.T) {
	_, cluster := initializedControlPlane(t, "")
	opts, sink := hermeticRunOptions(t, versionOnlyRunner())
	api := &scriptedProbe{answers: []bool{true}}
	opts.APIServerReachableProbe = api.probe

	if err := Run(context.Background(), cluster, opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := sink.only(t); got.Phase != status.PhaseConverged {
		t.Fatalf("recorded phase=%q, want %q", got.Phase, status.PhaseConverged)
	}
	if api.count() != 1 {
		t.Fatalf("apiserver probed %d times, want exactly 1", api.count())
	}
}

// TestRunKubeletDownDoesNotWaitForTheAPIServer: with the kubelet down the
// apiserver cannot be up either, and the verdict is KubeletUnhealthy; the
// grace period would only delay the boot.
func TestRunKubeletDownDoesNotWaitForTheAPIServer(t *testing.T) {
	_, cluster := initializedControlPlane(t, "")
	opts, sink := hermeticRunOptions(t, versionOnlyRunner())
	opts.KubeletHealthyProbe = func(context.Context) bool { return false }
	api := &scriptedProbe{answers: []bool{false}}
	opts.APIServerReachableProbe = api.probe

	if err := Run(context.Background(), cluster, opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := sink.only(t); got.Reason != status.ReasonKubeletUnhealthy {
		t.Fatalf("recorded reason=%q, want %q", got.Reason, status.ReasonKubeletUnhealthy)
	}
	if api.count() != 1 {
		t.Fatalf("apiserver probed %d times, want exactly 1 (no grace wait behind a dead kubelet)", api.count())
	}
}

// TestRunProductionAPIServerProbeDecidesTheVerdict wires the REAL production
// probe (opts.APIServerReachableProbe = nil) on the non-upgrade path, against
// a live TLS /healthz on the cluster's bindPort, and against a port nothing
// listens on.
func TestRunProductionAPIServerProbeDecidesTheVerdict(t *testing.T) {
	t.Run("serving", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Skipf("cannot bind a loopback port in this environment: %v", err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/healthz" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		_ = srv.Listener.Close()
		srv.Listener = ln
		srv.StartTLS()
		defer srv.Close()

		_, cluster := initializedControlPlane(t, "initConfiguration:\n  localAPIEndpoint:\n    bindPort: "+strconv.Itoa(port)+"\n")
		opts, sink := hermeticRunOptions(t, versionOnlyRunner())
		opts.APIServerReachableProbe = nil
		if err := Run(context.Background(), cluster, opts); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := sink.only(t); got.Phase != status.PhaseConverged {
			t.Fatalf("recorded phase=%q reason=%q, want %q", got.Phase, got.Reason, status.PhaseConverged)
		}
	})
	t.Run("nothing listening", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Skipf("cannot bind a loopback port in this environment: %v", err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()

		_, cluster := initializedControlPlane(t, "initConfiguration:\n  localAPIEndpoint:\n    bindPort: "+strconv.Itoa(port)+"\n")
		opts, sink := hermeticRunOptions(t, versionOnlyRunner())
		opts.APIServerReachableProbe = nil
		if err := Run(context.Background(), cluster, opts); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := sink.only(t); got.Reason != status.ReasonControlPlaneUnhealthy {
			t.Fatalf("recorded phase=%q reason=%q, want %q", got.Phase, got.Reason, status.ReasonControlPlaneUnhealthy)
		}
	})
}

// TestInitFinishedViaKubectl pins the production cluster check: it asks this
// node's apiserver through admin.conf, verifying the serving certificate under
// the name kubeadm always includes, names the resources with their API group,
// and only an exact addon object name in stdout counts. A kubectl failure is
// "could not ask", reported with the exit code and nothing else.
func TestInitFinishedViaKubectl(t *testing.T) {
	cases := []struct {
		name    string
		stdout  string
		stderr  string
		err     error
		want    bool
		wantErr string
	}{
		{name: "both addons", stdout: "deployment.apps/coredns\ndaemonset.apps/kube-proxy\n", want: true},
		{name: "kube-proxy replaced, CoreDNS kept", stdout: "deployment.apps/coredns\n", want: true},
		{name: "CoreDNS replaced, kube-proxy kept", stdout: "daemonset.apps/kube-proxy\n", want: true},
		{name: "neither exists", stdout: "", want: false},
		{name: "something else printed", stdout: "deployment.apps/coredns-custom\n", want: false},
		{
			name:    "kubectl failed: could not ask, exit code only",
			stdout:  "deployment.apps/coredns\n",
			stderr:  "error: You must be logged in to the server (Unauthorized) https://secret.example/proxy",
			err:     errors.New("kubectl [--kubeconfig /root-x/etc/kubernetes/admin.conf ...] failed (exit 1): Unauthorized"),
			wantErr: "kubectl exit 1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fr := &fakeRunner{respond: func([]string) (kubeadm.Result, error) {
				res := kubeadm.Result{Stdout: tc.stdout, Stderr: tc.stderr}
				if tc.err != nil {
					res.ExitCode = 1
				}
				return res, tc.err
			}}
			got, err := initFinishedViaKubectl("/root-x", 6444, fr)(context.Background())
			if got != tc.want {
				t.Fatalf("initFinishedViaKubectl = %v, want %v", got, tc.want)
			}
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr) {
				t.Fatalf("error = %v, want exactly %q (no argv, no kubectl output)", err, tc.wantErr)
			}
			want := []string{
				"--kubeconfig", "/root-x/etc/kubernetes/admin.conf",
				"--server", "https://127.0.0.1:6444",
				"--tls-server-name", "kubernetes",
				"--request-timeout", "10s",
				"-n", "kube-system", "get", "deployments.apps/coredns", "daemonsets.apps/kube-proxy",
				"-o", "name", "--ignore-not-found",
			}
			if len(fr.calls) != 1 || !slices.Equal(fr.calls[0], want) {
				t.Fatalf("kubectl argv = %v, want %v", fr.calls, want)
			}
		})
	}
}
