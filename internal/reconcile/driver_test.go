package reconcile

import (
	"context"
	"errors"
	"fmt"
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
