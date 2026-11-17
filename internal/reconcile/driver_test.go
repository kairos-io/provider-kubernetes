package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeExec records calls and fails a configurable number of times per action.
type fakeExec struct {
	calls       map[Action]int
	failUntil   int // succeed once calls for the action exceed this
	alwaysError bool
}

func newFakeExec() *fakeExec { return &fakeExec{calls: map[Action]int{}} }

func (f *fakeExec) Execute(_ context.Context, a Action) error {
	f.calls[a]++
	if f.alwaysError {
		return errors.New("permanent failure")
	}
	if f.calls[a] <= f.failUntil {
		return errors.New("transient failure")
	}
	return nil
}

func testReconciler(exec Executor) Reconciler {
	return Reconciler{
		Budget: Budget{PerAttempt: time.Second, MaxAttempts: 3, Total: time.Minute},
		Exec:   exec,
		Sleep:  func(time.Duration) {}, // do not actually wait in tests
	}
}

func TestReconcilerSucceedsAfterTransientFailures(t *testing.T) {
	exec := newFakeExec()
	exec.failUntil = 2 // fail twice, succeed on the third attempt
	err := testReconciler(exec).Run(context.Background(), []Action{ActionRunInit})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if exec.calls[ActionRunInit] != 3 {
		t.Fatalf("expected 3 attempts, got %d", exec.calls[ActionRunInit])
	}
}

func TestReconcilerFailsLoudAfterMaxAttempts(t *testing.T) {
	exec := newFakeExec()
	exec.alwaysError = true
	err := testReconciler(exec).Run(context.Background(), []Action{ActionRunJoin})
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if exec.calls[ActionRunJoin] != 3 {
		t.Fatalf("expected exactly 3 bounded attempts, got %d", exec.calls[ActionRunJoin])
	}
}

// perAttemptExec fails every call with a different error.
type perAttemptExec struct{ calls int }

func (e *perAttemptExec) Execute(_ context.Context, _ Action) error {
	e.calls++
	if e.calls == 1 {
		return errors.New("wait-control-plane timed out")
	}
	return errors.New("preflight: manifests already exist")
}

func TestReconcilerKeepsFirstAttemptError(t *testing.T) {
	exec := &perAttemptExec{}
	err := testReconciler(exec).Run(context.Background(), []Action{ActionRunInit})
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	msg := err.Error()
	first := strings.Index(msg, "wait-control-plane timed out")
	last := strings.Index(msg, "preflight: manifests already exist")
	if first < 0 || last < 0 {
		t.Fatalf("error must mention both attempts, got %q", msg)
	}
	// Order matters, not just presence: the status Message is cut at the first
	// newline and then to 256 characters, so a note appended after a multi-line
	// kubeadm error is exactly the part the operator never sees.
	if first > last {
		t.Fatalf("the first-attempt note must precede the final error, got %q", msg)
	}
}

// errLastAttempt is the sentinel sentinelExec wraps on every attempt after the
// first, so a test can assert what errors.Is targets.
var errLastAttempt = errors.New("last-attempt sentinel")

type sentinelExec struct{ calls int }

func (e *sentinelExec) Execute(_ context.Context, _ Action) error {
	e.calls++
	if e.calls == 1 {
		return errors.New("first boom")
	}
	return fmt.Errorf("later boom: %w", errLastAttempt)
}

// Only the error the action actually ended on is %w-wrapped. The first-attempt
// note is informational and must not hijack errors.Is/As.
func TestReconcilerWrapsOnlyTheFinalError(t *testing.T) {
	err := testReconciler(&sentinelExec{}).Run(context.Background(), []Action{ActionRunInit})
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if !errors.Is(err, errLastAttempt) {
		t.Fatalf("errors.Is must target the error the action ended on, got %v", err)
	}
	if !strings.Contains(err.Error(), "first boom") {
		t.Fatalf("the first attempt must still be reported, got %v", err)
	}
}

// noisyFirstExec fails the first attempt with a long multi-line error, the way
// kubeadm does, and later attempts with a short one.
type noisyFirstExec struct{ calls int }

func (e *noisyFirstExec) Execute(_ context.Context, _ Action) error {
	e.calls++
	if e.calls == 1 {
		return errors.New("line one is the real cause " + strings.Repeat("padding ", 40) +
			"\nline two\nline three")
	}
	return errors.New("manifests already exist")
}

func TestReconcilerSummaryLineIsBounded(t *testing.T) {
	err := testReconciler(&noisyFirstExec{}).Run(context.Background(), []Action{ActionRunInit})
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	summary, _, _ := strings.Cut(err.Error(), "\n")
	const actionWrap = `action "run-init": `
	summary = strings.TrimPrefix(summary, actionWrap)
	if len(summary) > summaryMax {
		t.Fatalf("the summary line must fit in %d bytes, got %d: %q", summaryMax, len(summary), summary)
	}
	if !strings.Contains(summary, "line one is the real cause") {
		t.Fatalf("the summary must keep the head of the first error, got %q", summary)
	}
	// The short last cause hands its unused room to the long first one, and is
	// itself never cut.
	if !strings.Contains(summary, "last attempt: manifests already exist") {
		t.Fatalf("the final error must survive in the summary, got %q", summary)
	}
	// The full errors still reach the log, below the summary.
	if !strings.Contains(err.Error(), "line three") {
		t.Fatalf("the full first error must follow the summary, got %q", err)
	}
}

// kubeadmExec fails the way `kubeadm init` does through the kubeadm runner: the
// command line ahead of stderr, progress output, and the error last. The first
// attempt times out in wait-control-plane; the retries trip over what it left.
type kubeadmExec struct{ calls int }

const kubeadmRunnerPrefix = "kubeadm init: kubeadm [init --config /run/kubeadm-1234567.yaml --upload-certs] failed (exit 1): exit status 1: "

const kubeadmWaitControlPlane = kubeadmRunnerPrefix +
	"[init] Using Kubernetes version: v1.36.2\n" +
	"[preflight] Running pre-flight checks\n" +
	"[wait-control-plane] Waiting for the kubelet to boot up the control plane as static Pods from directory \"/etc/kubernetes/manifests\"\n" +
	"[kubelet-check] Waiting for a healthy kubelet at http://127.0.0.1:10248/healthz. This can take up to 4m0s\n" +
	"error: error execution phase wait-control-plane: failed while waiting for the kubelet to start: context deadline exceeded\n" +
	"To see the stack trace of this error execute with --v=5 or higher\n"

const kubeadmPreflight = kubeadmRunnerPrefix +
	"[init] Using Kubernetes version: v1.36.2\n" +
	"[preflight] Running pre-flight checks\n" +
	"error: error execution phase preflight: [preflight] Some fatal errors occurred:\n" +
	"\t[ERROR Port-6443]: Port 6443 is in use\n" +
	"\t[ERROR FileAvailable--etc-kubernetes-manifests-kube-apiserver.yaml]: /etc/kubernetes/manifests/kube-apiserver.yaml already exists\n" +
	"To see the stack trace of this error execute with --v=5 or higher\n"

func (e *kubeadmExec) Execute(_ context.Context, _ Action) error {
	e.calls++
	if e.calls == 1 {
		return errors.New(kubeadmWaitControlPlane)
	}
	return errors.New(kubeadmPreflight)
}

func TestReconcilerSummaryDropsKubeadmCommandLine(t *testing.T) {
	err := testReconciler(&kubeadmExec{}).Run(context.Background(), []Action{ActionRunInit})
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	summary, _, _ := strings.Cut(err.Error(), "\n")
	for _, want := range []string{
		"first attempt: wait-control-plane: failed while waiting for the kubelet",
		"last attempt: preflight:",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary must contain %q, got %q", want, summary)
		}
	}
	if strings.Contains(summary, "--upload-certs") {
		t.Fatalf("the command line must not spend the summary's room, got %q", summary)
	}
}

func TestErrorCause(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "wait-control-plane timed out", "wait-control-plane timed out"},
		{"multi-line without error line", "a\n\n b \r\nc", "a; b; c"},
		{"runner prefix", kubeadmRunnerPrefix + "something broke", "something broke"},
		{"kubeadm error line", kubeadmWaitControlPlane,
			"wait-control-plane: failed while waiting for the kubelet to start: context deadline exceeded"},
		{"preflight keeps the ERROR lines", kubeadmPreflight,
			"preflight: [preflight] Some fatal errors occurred:; [ERROR Port-6443]: Port 6443 is in use; " +
				"[ERROR FileAvailable--etc-kubernetes-manifests-kube-apiserver.yaml]: /etc/kubernetes/manifests/kube-apiserver.yaml already exists"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorCause(tc.in); got != tc.want {
				t.Fatalf("errorCause() = %q, want %q", got, tc.want)
			}
		})
	}
}

// An identical first error adds nothing but noise to a 256-character budget.
func TestReconcilerOmitsRedundantFirstAttemptNote(t *testing.T) {
	exec := newFakeExec()
	exec.alwaysError = true // every attempt fails with the same message
	err := testReconciler(exec).Run(context.Background(), []Action{ActionRunJoin})
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if strings.Contains(err.Error(), "first attempt") {
		t.Fatalf("an identical first error must not be repeated, got %q", err)
	}
}

func TestReconcilerSingleAttemptErrorHasNoFirstAttemptNote(t *testing.T) {
	exec := newFakeExec()
	exec.alwaysError = true
	r := testReconciler(exec)
	r.Budget.MaxAttempts = 1
	err := r.Run(context.Background(), []Action{ActionRunInit})
	if err == nil || strings.Contains(err.Error(), "first attempt") {
		t.Fatalf("unexpected error %v", err)
	}
}

// terminalExec always returns an ErrTerminal-wrapped error.
type terminalExec struct{ calls int }

func (e *terminalExec) Execute(_ context.Context, _ Action) error {
	e.calls++
	return fmt.Errorf("%w: a control plane already exists", ErrTerminal)
}

func TestReconcilerFailsFastOnTerminalError(t *testing.T) {
	exec := &terminalExec{}
	err := testReconciler(exec).Run(context.Background(), []Action{ActionRefuseInit})
	if err == nil {
		t.Fatal("expected a terminal error")
	}
	if !errors.Is(err, ErrTerminal) {
		t.Fatalf("expected error to wrap ErrTerminal, got %v", err)
	}
	// Terminal verdicts must NOT be retried: exactly one attempt.
	if exec.calls != 1 {
		t.Fatalf("expected exactly 1 attempt for a terminal error, got %d", exec.calls)
	}
}

func TestReconcilerSkipsActionNone(t *testing.T) {
	exec := newFakeExec()
	if err := testReconciler(exec).Run(context.Background(), []Action{ActionNone}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("ActionNone must not invoke the executor, got %v", exec.calls)
	}
}

func TestReconcilerRejectsInvalidBudget(t *testing.T) {
	r := Reconciler{Budget: Budget{}, Exec: newFakeExec(), Sleep: func(time.Duration) {}}
	if err := r.Run(context.Background(), []Action{ActionRunInit}); err == nil {
		t.Fatal("expected error for invalid budget")
	}
}

func TestReconcilerRequiresExecutor(t *testing.T) {
	r := Reconciler{Budget: DefaultBudget(), Sleep: func(time.Duration) {}}
	if err := r.Run(context.Background(), []Action{ActionRunInit}); err == nil {
		t.Fatal("expected error when executor is nil")
	}
}

// A pathological error must not be split line by line just to keep a few dozen
// bytes: being OOM-killed here would cost the status write and lose the very
// report the summary exists to deliver. kubeadm prints its error last, so the
// tail is where the cause is looked for.
func TestErrorCauseBoundsItsInput(t *testing.T) {
	huge := "head\n" + strings.Repeat("progress\n", 64<<10) + "error execution phase marker: boom\n"
	if got := errorCause(huge); got != "marker: boom" {
		t.Fatalf("the error line at the tail must be found, got %q", got)
	}
	middle := "head\n" + strings.Repeat("x", 64<<10) + "\nerror: lost\n" + strings.Repeat("y", 64<<10)
	if got := errorCause(middle); strings.Contains(got, "lost") {
		t.Fatalf("input past both bounds must not be consulted, got %q", got[:64])
	}
	long := strings.Repeat("abcdefghij", 100<<10)
	if n := len(summarizeAttempts(3, long, long+"!")); n > summaryMax {
		t.Fatalf("the summary must stay capped regardless of input size, got %d bytes", n)
	}
}
