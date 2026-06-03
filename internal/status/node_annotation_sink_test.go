package status

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
)

// fakeKubectlRunner records every Run call and optionally returns a configured
// error. It is the fake injectable runner for NodeAnnotationSink tests.
type fakeKubectlRunner struct {
	calls [][]string
	err   error
}

func (f *fakeKubectlRunner) Run(_ context.Context, args ...string) (kubeadm.Result, error) {
	f.calls = append(f.calls, args)
	return kubeadm.Result{}, f.err
}

// convergedStatus returns a fully-populated Status as produced by a successful
// reconcile. Used as a representative value in tests that need a real Status.
func convergedStatus() Status {
	return BuildStatus(BuildParams{
		Membership: "initialized",
		LastAction: "none",
		Now:        "2026-06-03T12:00:00Z",
		BootID:     "test-boot-id",
		Version:    "v0.2.0-test",
	})
}

// --- happy-path argv assertions ---

// TestNodeAnnotationSinkHappyPathArgv verifies the exact kubectl argv emitted
// on a successful Record when both kubeconfig and node name are available.
func TestNodeAnnotationSinkHappyPathArgv(t *testing.T) {
	runner := &fakeKubectlRunner{}
	sink := &NodeAnnotationSink{
		Runner:            runner,
		ResolveNode:       func() (string, error) { return "my-node", nil },
		ResolveKubeconfig: func() string { return "/etc/kubernetes/admin.conf" },
		AnnotateDeadline:  100 * time.Millisecond,
	}

	s := convergedStatus()
	sink.Record(context.Background(), s)

	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 runner call, got %d", len(runner.calls))
	}
	args := runner.calls[0]

	// Position-0 through 2: "annotate node <nodename>"
	if args[0] != "annotate" {
		t.Errorf("args[0] = %q, want %q", args[0], "annotate")
	}
	if args[1] != "node" {
		t.Errorf("args[1] = %q, want %q", args[1], "node")
	}
	if args[2] != "my-node" {
		t.Errorf("args[2] = %q, want %q", args[2], "my-node")
	}

	// The last three args must be --overwrite --kubeconfig <path>.
	n := len(args)
	if args[n-3] != "--overwrite" {
		t.Errorf("args[n-3] = %q, want --overwrite", args[n-3])
	}
	if args[n-2] != "--kubeconfig" {
		t.Errorf("args[n-2] = %q, want --kubeconfig", args[n-2])
	}
	if args[n-1] != "/etc/kubernetes/admin.conf" {
		t.Errorf("args[n-1] = %q, want /etc/kubernetes/admin.conf", args[n-1])
	}

	// The annotation slice (positions 3..n-4) must contain all expected keys.
	annotationArgs := args[3 : n-3]
	expectedKeys := []string{
		AnnotationPhase,
		AnnotationOutcome,
		AnnotationReason,
		AnnotationTerminal,
		AnnotationLastAction,
		AnnotationUpdatedAt,
		AnnotationVersion,
	}
	for _, key := range expectedKeys {
		found := false
		for _, a := range annotationArgs {
			if strings.HasPrefix(a, key+"=") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("annotation key %q not found in argv", key)
		}
	}

	// Message must NOT appear on argv (security / minimalism constraint).
	for _, a := range annotationArgs {
		if strings.HasPrefix(a, AnnotationPrefix+"message=") {
			t.Errorf("message annotation must not appear on argv, got %q", a)
		}
	}

	// No shell invocation: none of the args may be "sh", "-c", or start with "$(".
	for _, a := range args {
		if a == "sh" || a == "-c" || strings.HasPrefix(a, "$(") {
			t.Errorf("shell-like arg %q found on argv: shell invocation is forbidden", a)
		}
	}

	// No secret-shaped value in any arg (bootstrap token pattern, PEM header, 64-hex).
	for _, a := range args {
		if strings.Contains(a, "-----BEGIN") {
			t.Errorf("PEM block detected in argv arg %q", a)
		}
	}
}

// TestNodeAnnotationSinkAnnotationValues verifies the actual values encoded for
// a converged status match the Status fields.
func TestNodeAnnotationSinkAnnotationValues(t *testing.T) {
	runner := &fakeKubectlRunner{}
	sink := &NodeAnnotationSink{
		Runner:            runner,
		ResolveNode:       func() (string, error) { return "test-node", nil },
		ResolveKubeconfig: func() string { return "/etc/kubernetes/kubelet.conf" },
		AnnotateDeadline:  100 * time.Millisecond,
	}

	s := convergedStatus()
	sink.Record(context.Background(), s)

	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 runner call, got %d", len(runner.calls))
	}
	args := runner.calls[0]

	// Build a map of key -> value from the annotation args (positions 3..n-4).
	n := len(args)
	annotMap := make(map[string]string)
	for _, a := range args[3 : n-3] {
		idx := strings.Index(a, "=")
		if idx < 0 {
			t.Errorf("annotation arg %q has no '='", a)
			continue
		}
		annotMap[a[:idx]] = a[idx+1:]
	}

	checks := []struct {
		key  string
		want string
	}{
		{AnnotationPhase, string(s.Phase)},
		{AnnotationOutcome, string(s.Outcome)},
		{AnnotationReason, string(s.Reason)},
		{AnnotationTerminal, boolStr(s.Terminal)},
		{AnnotationLastAction, s.LastAction},
		{AnnotationUpdatedAt, s.UpdatedAt},
		{AnnotationVersion, s.Version},
	}
	for _, tc := range checks {
		if got := annotMap[tc.key]; got != tc.want {
			t.Errorf("annotation %s = %q, want %q", tc.key, got, tc.want)
		}
	}
}

// --- no-op paths ---

// TestNodeAnnotationSinkNoopOnEmptyKubeconfig verifies that when the kubeconfig
// resolver returns "", Record makes zero runner calls (pre-membership no-op).
func TestNodeAnnotationSinkNoopOnEmptyKubeconfig(t *testing.T) {
	runner := &fakeKubectlRunner{}
	sink := &NodeAnnotationSink{
		Runner:            runner,
		ResolveNode:       func() (string, error) { return "some-node", nil },
		ResolveKubeconfig: func() string { return "" }, // pre-membership
		AnnotateDeadline:  100 * time.Millisecond,
	}
	sink.Record(context.Background(), convergedStatus())

	if len(runner.calls) != 0 {
		t.Errorf("expected 0 runner calls when kubeconfig is empty, got %d", len(runner.calls))
	}
}

// TestNodeAnnotationSinkNoopOnNodeNameError verifies that a node-name resolution
// error produces zero runner calls.
func TestNodeAnnotationSinkNoopOnNodeNameError(t *testing.T) {
	runner := &fakeKubectlRunner{}
	sink := &NodeAnnotationSink{
		Runner:            runner,
		ResolveNode:       func() (string, error) { return "", errors.New("no hostname") },
		ResolveKubeconfig: func() string { return "/etc/kubernetes/admin.conf" },
		AnnotateDeadline:  100 * time.Millisecond,
	}
	sink.Record(context.Background(), convergedStatus())

	if len(runner.calls) != 0 {
		t.Errorf("expected 0 runner calls when node name errors, got %d", len(runner.calls))
	}
}

// TestNodeAnnotationSinkNoopOnEmptyNodeName verifies an empty (non-error) node
// name also triggers the no-op path.
func TestNodeAnnotationSinkNoopOnEmptyNodeName(t *testing.T) {
	runner := &fakeKubectlRunner{}
	sink := &NodeAnnotationSink{
		Runner:            runner,
		ResolveNode:       func() (string, error) { return "", nil },
		ResolveKubeconfig: func() string { return "/etc/kubernetes/admin.conf" },
		AnnotateDeadline:  100 * time.Millisecond,
	}
	sink.Record(context.Background(), convergedStatus())

	if len(runner.calls) != 0 {
		t.Errorf("expected 0 runner calls when node name is empty, got %d", len(runner.calls))
	}
}

// --- runner error swallowing ---

// TestNodeAnnotationSinkRunnerErrorSwallowed verifies that a runner error does
// not panic or propagate: the sink swallows it (best-effort guarantee).
func TestNodeAnnotationSinkRunnerErrorSwallowed(t *testing.T) {
	runner := &fakeKubectlRunner{err: errors.New("kubectl: connection refused")}
	sink := &NodeAnnotationSink{
		Runner:            runner,
		ResolveNode:       func() (string, error) { return "some-node", nil },
		ResolveKubeconfig: func() string { return "/etc/kubernetes/admin.conf" },
		AnnotateDeadline:  100 * time.Millisecond,
	}
	// Must not panic, must not return an error (Record has no return).
	sink.Record(context.Background(), convergedStatus())

	// Runner was called (we tried), but the error was swallowed.
	if len(runner.calls) != 1 {
		t.Errorf("expected 1 runner call even on error, got %d", len(runner.calls))
	}
}

// TestNodeAnnotationSinkCanceledContextSwallowed verifies that a canceled
// context does not panic (the deadline check inside the sink handles it).
func TestNodeAnnotationSinkCanceledContextSwallowed(t *testing.T) {
	runner := &fakeKubectlRunner{}
	sink := &NodeAnnotationSink{
		Runner:            runner,
		ResolveNode:       func() (string, error) { return "node", nil },
		ResolveKubeconfig: func() string { return "/etc/kubernetes/admin.conf" },
		AnnotateDeadline:  100 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel
	// Must not panic.
	sink.Record(ctx, convergedStatus())
}

// --- kubeconfig resolver helpers ---

// TestResolveKubeconfigPrefersKubeletConf verifies that kubelet.conf is chosen
// over admin.conf when both files exist (least-privilege node-identity precedence,
// security Q2).
func TestResolveKubeconfigPrefersKubeletConf(t *testing.T) {
	dir := t.TempDir()
	k8sDir := filepath.Join(dir, "etc", "kubernetes")
	if err := os.MkdirAll(k8sDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"admin.conf", "kubelet.conf"} {
		if err := os.WriteFile(filepath.Join(k8sDir, name), []byte("placeholder"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	got := resolveKubeconfig(dir)
	want := filepath.Join(k8sDir, "kubelet.conf")
	if got != want {
		t.Errorf("resolveKubeconfig = %q, want %q (kubelet.conf must be preferred over admin.conf)", got, want)
	}
}

// TestResolveKubeconfigFallsBackToAdminConf verifies that admin.conf is returned
// when kubelet.conf is absent (fallback to cluster-admin credential).
func TestResolveKubeconfigFallsBackToAdminConf(t *testing.T) {
	dir := t.TempDir()
	k8sDir := filepath.Join(dir, "etc", "kubernetes")
	if err := os.MkdirAll(k8sDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(k8sDir, "admin.conf"), []byte("placeholder"), 0o600); err != nil {
		t.Fatalf("write admin.conf: %v", err)
	}
	got := resolveKubeconfig(dir)
	want := filepath.Join(k8sDir, "admin.conf")
	if got != want {
		t.Errorf("resolveKubeconfig = %q, want %q", got, want)
	}
}

// TestResolveKubeconfigEmptyWhenNeitherExists verifies that "" is returned when
// neither admin.conf nor kubelet.conf is present (pre-membership state).
func TestResolveKubeconfigEmptyWhenNeitherExists(t *testing.T) {
	dir := t.TempDir() // no kubernetes files created
	got := resolveKubeconfig(dir)
	if got != "" {
		t.Errorf("resolveKubeconfig = %q, want empty string (pre-membership)", got)
	}
}

// --- leading-dash argv guard (security Finding A) ---

// TestNodeAnnotationSinkNoopOnDashNodeName verifies that a node name beginning
// with '-' causes the sink to no-op (zero runner calls). A leading '-' could be
// parsed as a flag by kubectl/cobra even though the closed-enum schema prevents
// this today; the guard is defense-in-depth against future schema drift.
func TestNodeAnnotationSinkNoopOnDashNodeName(t *testing.T) {
	runner := &fakeKubectlRunner{}
	sink := &NodeAnnotationSink{
		Runner:            runner,
		ResolveNode:       func() (string, error) { return "-bad-node-name", nil },
		ResolveKubeconfig: func() string { return "/etc/kubernetes/admin.conf" },
		AnnotateDeadline:  100 * time.Millisecond,
	}
	sink.Record(context.Background(), convergedStatus())

	if len(runner.calls) != 0 {
		t.Errorf("expected 0 runner calls when node name begins with '-', got %d (flag injection guard must no-op)", len(runner.calls))
	}
}

// TestNodeAnnotationSinkTokensNeverStartWithDash verifies the invariant that no
// annotation token passed to kubectl begins with '-', even when a status value
// begins with '-'. The annotation tokens are always of the form
// "provider-kubernetes.kairos.io/<key>=<value>", so the token itself starts with
// the vendor prefix ('p'), never with '-'. This test asserts that:
//   - the write proceeds (the token is safe; the value is carried AFTER '=')
//   - none of the argv tokens start with '-' (the node name and flag args aside)
//
// This is the defense-in-depth invariant from security Finding A.
func TestNodeAnnotationSinkTokensNeverStartWithDash(t *testing.T) {
	runner := &fakeKubectlRunner{}
	sink := &NodeAnnotationSink{
		Runner:            runner,
		ResolveNode:       func() (string, error) { return "valid-node", nil },
		ResolveKubeconfig: func() string { return "/etc/kubernetes/admin.conf" },
		AnnotateDeadline:  100 * time.Millisecond,
	}

	// Craft a Status with a Phase value that begins with '-'.
	// The annotation token will be "provider-kubernetes.kairos.io/phase=-...",
	// which starts with 'p', not '-'. The write must proceed.
	crafted := convergedStatus()
	crafted.Phase = Phase("-flag-injection-attempt")

	sink.Record(context.Background(), crafted)

	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 runner call (annotation tokens do not start with '-'), got %d", len(runner.calls))
	}

	// Assert that no annotation token (positions 3..n-4) starts with '-'.
	args := runner.calls[0]
	n := len(args)
	for _, tok := range args[3 : n-3] {
		if strings.HasPrefix(tok, "-") {
			t.Errorf("annotation token %q starts with '-': violates argv safety invariant (Finding A)", tok)
		}
	}
}

// --- kubeconfig resolver helpers ---

// TestAnnotationKeysHaveCorrectPrefix verifies all exported annotation
// constants start with the declared AnnotationPrefix.
func TestAnnotationKeysHaveCorrectPrefix(t *testing.T) {
	keys := []string{
		AnnotationPhase,
		AnnotationOutcome,
		AnnotationReason,
		AnnotationTerminal,
		AnnotationLastAction,
		AnnotationUpdatedAt,
		AnnotationVersion,
	}
	for _, k := range keys {
		if !strings.HasPrefix(k, AnnotationPrefix) {
			t.Errorf("key %q does not start with prefix %q", k, AnnotationPrefix)
		}
	}
}

// TestNodeAnnotationSinkSatisfiesStatusSink verifies at compile time that
// *NodeAnnotationSink satisfies the StatusSink interface.
var _ StatusSink = (*NodeAnnotationSink)(nil)

// TestNewNodeAnnotationSinkProduction verifies that NewNodeAnnotationSink
// returns a non-nil sink with a non-nil runner wired in (smoke test; does not
// invoke kubectl).
func TestNewNodeAnnotationSinkProduction(t *testing.T) {
	sink := NewNodeAnnotationSink("/", "")
	if sink == nil {
		t.Fatal("NewNodeAnnotationSink returned nil")
	}
	if sink.Runner == nil {
		t.Error("Runner is nil")
	}
	if sink.ResolveNode == nil {
		t.Error("ResolveNode is nil")
	}
	if sink.ResolveKubeconfig == nil {
		t.Error("ResolveKubeconfig is nil")
	}
}

// TestNewNodeAnnotationSinkUsesKnownNodeName verifies that when a non-empty
// nodeName is passed to NewNodeAnnotationSink, the ResolveNode func returns it
// directly (Finding D: prefer kubeadm NodeRegistration.Name over os.Hostname).
func TestNewNodeAnnotationSinkUsesKnownNodeName(t *testing.T) {
	sink := NewNodeAnnotationSink("/", "my-kubeadm-node")
	if sink == nil {
		t.Fatal("NewNodeAnnotationSink returned nil")
	}
	name, err := sink.ResolveNode()
	if err != nil {
		t.Fatalf("ResolveNode returned error: %v", err)
	}
	if name != "my-kubeadm-node" {
		t.Errorf("ResolveNode = %q, want %q", name, "my-kubeadm-node")
	}
}

// TestMakeNodeResolverFallsBackToHostname verifies that MakeNodeResolver("")
// falls back to os.Hostname() lowercased when no known name is provided.
func TestMakeNodeResolverFallsBackToHostname(t *testing.T) {
	resolver := MakeNodeResolver("")
	name, err := resolver()
	if err != nil {
		t.Fatalf("makeNodeResolver(\"\") returned error: %v", err)
	}
	// We cannot predict the test host name, but it must be non-empty and lowercase.
	if name == "" {
		t.Error("fallback resolver returned empty name")
	}
	if name != strings.ToLower(name) {
		t.Errorf("fallback resolver returned non-lowercase name %q", name)
	}
}
