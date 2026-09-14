package provider

import (
	"context"
	"os"
	"testing"

	"github.com/kairos-io/kairos-sdk/clusterplugin"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
	"github.com/kairos-io/provider-kubernetes/internal/status"
)

// productionStatusSinkGuard is the panic value of the guard TestMain installs
// over newDefaultStatusSink.
const productionStatusSinkGuard = "provider tests: Run built the production status sink " +
	"(host FileSink + real-kubectl NodeAnnotationSink); inject Options.StatusSink via hermeticRunOptions"

// TestMain arms a fail-closed guard for every test in this package. When
// Options.StatusSink is nil, Run builds the production sinks: a FileSink that
// writes /run and /var/log on the host, and a NodeAnnotationSink that execs the
// host's kubectl against whatever API server it can reach. The guard panics
// before either is built, failing the offending test by name.
func TestMain(m *testing.M) {
	newDefaultStatusSink = func(string) (status.StatusSink, *status.NodeAnnotationSink) {
		panic(productionStatusSinkGuard)
	}
	os.Exit(m.Run())
}

// recordingSink is a hermetic StatusSink that keeps every recorded Status in
// memory instead of writing to the host or the API server.
type recordingSink struct {
	records []status.Status
}

func (r *recordingSink) Record(_ context.Context, s status.Status) {
	r.records = append(r.records, s)
}

// only returns the status Run recorded, failing the test unless exactly one was
// recorded (ADR-4-S: every Run exit path records once).
func (r *recordingSink) only(t *testing.T) status.Status {
	t.Helper()
	if len(r.records) != 1 {
		t.Fatalf("recorded %d statuses, want exactly 1: %+v", len(r.records), r.records)
	}
	return r.records[0]
}

// hermeticRunOptions returns Run options that keep a test off the host: the
// given kubeadm runner, a temp RunDir, an in-memory status sink, and an
// unreachable control plane (no TCP dial to the cluster endpoint). The upgrade
// hooks, whose nil defaults exec kubectl/systemctl (via kubeadm.KubectlRunner /
// kubeadm.SystemctlRunner, ADR-1-A1), inspect the host's snapshot dir and block
// devices, or probe the local apiserver, fail the test if consulted. Run always
// builds a kubeadm.KubectlRunner() when an upgrade target is set (constructing
// it is inert -- no exec happens), but only invokes it through these nil
// defaults, so setting them below keeps a test hermetic. Tests override the
// fields their path needs.
func hermeticRunOptions(t *testing.T, runner kubeadm.Runner) (Options, *recordingSink) {
	t.Helper()
	sink := &recordingSink{}
	return Options{
		Runner:                     runner,
		RunDir:                     t.TempDir(),
		StatusSink:                 sink,
		CPReachableProbe:           func(context.Context) bool { return false },
		ClusterVersionProbe:        failIfCalled[string](t, "ClusterVersionProbe"),
		RunningKubeletVersionProbe: failIfCalled[string](t, "RunningKubeletVersionProbe"),
		EncryptionConfirmed: func(ctx context.Context, _ string) bool {
			return failIfCalled[bool](t, "EncryptionConfirmed")(ctx)
		},
		KubeletRestart:          failIfCalled[error](t, "KubeletRestart"),
		APIServerReachableProbe: failIfCalled[bool](t, "APIServerReachableProbe"),
	}, sink
}

// failIfCalled returns a hook that fails the test when Run consults it.
func failIfCalled[T any](t *testing.T, field string) func(context.Context) T {
	return func(context.Context) T {
		t.Errorf("Run consulted Options.%s, which the test did not set for this path", field)
		var zero T
		return zero
	}
}

// stubDefaultStatusSink replaces the guarded production status sink for one
// test, restoring the guard afterwards. Not for parallel tests.
func stubDefaultStatusSink(t *testing.T, build func(rootPath string) (status.StatusSink, *status.NodeAnnotationSink)) {
	t.Helper()
	guard := newDefaultStatusSink
	newDefaultStatusSink = build
	t.Cleanup(func() { newDefaultStatusSink = guard })
}

// TestRunWithoutStatusSinkTripsHermeticGuard proves the TestMain guard is armed:
// a Run that omits Options.StatusSink is stopped before it does any work, so no
// test in this package can write host status files or exec the host's kubectl.
func TestRunWithoutStatusSinkTripsHermeticGuard(t *testing.T) {
	fr := &fakeRunner{}
	opts, _ := hermeticRunOptions(t, fr)
	opts.StatusSink = nil
	cluster := clusterplugin.Cluster{
		Role:             clusterplugin.RoleInit,
		ClusterToken:     validToken(),
		ControlPlaneHost: "10.0.0.1",
		ProviderOptions:  map[string]string{"cluster_root_path": t.TempDir()},
	}

	got := recoverPanic(func() { _ = Run(context.Background(), cluster, opts) })
	if got != productionStatusSinkGuard {
		t.Fatalf("Run without a StatusSink: recovered %v, want the production status sink guard", got)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("Run invoked kubeadm before the guard tripped; calls=%v", fr.calls)
	}
}

func recoverPanic(f func()) (r any) {
	defer func() { r = recover() }()
	f()
	return nil
}
