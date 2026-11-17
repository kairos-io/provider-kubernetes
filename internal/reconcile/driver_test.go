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
		return errors.New("line one is the real cause\n" +
			strings.Repeat("padding ", 40) + "\nline three")
	}
	return errors.New("manifests already exist")
}

func TestReconcilerFirstAttemptNoteIsSingleLineAndCapped(t *testing.T) {
	err := testReconciler(&noisyFirstExec{}).Run(context.Background(), []Action{ActionRunInit})
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	msg := err.Error()
	const marker = "(first attempt: "
	open := strings.Index(msg, marker)
	if open < 0 {
		t.Fatalf("expected a first-attempt note, got %q", msg)
	}
	note := msg[open+len(marker):]
	if end := strings.Index(note, "): "); end >= 0 {
		note = note[:end]
	}
	if strings.ContainsAny(note, "\n\r") {
		t.Fatalf("the note must be single-line or sanitize() cuts it, got %q", note)
	}
	if n := len([]rune(note)); n > firstAttemptNoteMax+3 { // +3 for the "..." marker
		t.Fatalf("the note must be capped at %d runes, got %d: %q", firstAttemptNoteMax, n, note)
	}
	if !strings.Contains(note, "line one is the real cause") {
		t.Fatalf("the note must keep the head of the first error, got %q", note)
	}
	// Capping the note is what leaves room for this.
	if !strings.Contains(msg, "manifests already exist") {
		t.Fatalf("the final error must survive the note, got %q", msg)
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

// A pathological first-attempt error must not be split line by line just to
// keep a 120-rune note: being OOM-killed here would cost the status write and
// lose the very report the note exists to deliver.
func TestOneLineBoundsItsInput(t *testing.T) {
	huge := strings.Repeat("\n", 128<<10) + "marker"
	if out := oneLine(huge, firstAttemptNoteMax); strings.Contains(out, "marker") {
		t.Fatalf("input past the bound must not be consulted, got %q", out)
	}
	long := strings.Repeat("abcdefghij", 100<<10)
	if n := len([]rune(oneLine(long, firstAttemptNoteMax))); n > firstAttemptNoteMax+3 {
		t.Fatalf("the note must stay capped regardless of input size, got %d runes", n)
	}
}
