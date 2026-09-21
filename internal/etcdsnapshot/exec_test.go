package etcdsnapshot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain lets the test binary itself stand in for etcdctl in the DEFAULT-exec
// tests below (real os/exec, not the `save` test seam): when it is invoked with
// SaveCommand's argv shape (`... snapshot save <dest>`) it runs fakeEtcdctlMain
// instead of the tests. The mode is selected purely from argv because production
// runs the child with an EMPTY environment, so no env-var signaling is possible.
func TestMain(m *testing.M) {
	if n := len(os.Args); n >= 4 && os.Args[n-3] == "snapshot" && os.Args[n-2] == "save" {
		fakeEtcdctlMain(os.Args[n-1])
	}
	os.Exit(m.Run())
}

// fakeEtcdctlMain emulates etcdctl for one destination and exits:
//
//	dest contains "HANG"    -> write "<dest>.part" then block until killed
//	dest contains "FAILBIG" -> print >1KiB of output, including a PEM block
//	                            near the end, to stdout and exit 1
//	otherwise               -> write the destination file and exit 0
//
// It always writes "<dest>.environ" recording the number of environment
// variables it observed, so a test can assert the child saw none.
func fakeEtcdctlMain(dest string) {
	_ = os.WriteFile(dest+".environ", []byte(fmt.Sprintf("%d", len(os.Environ()))), 0o600)
	switch {
	case strings.Contains(dest, "HANG"):
		_ = os.WriteFile(dest+".part", []byte("partial"), 0o600)
		// A sleep, not `select {}`: with no other goroutines the runtime would abort
		// a bare select as a deadlock, and the kill path would never be exercised.
		time.Sleep(time.Hour)
		os.Exit(3)
	case strings.Contains(dest, "FAILBIG"):
		block := "-----BEGIN PRIVATE KEY-----\n" + strings.Repeat("QUJDRA==\n", 100) + "-----END PRIVATE KEY-----\n"
		fmt.Print(strings.Repeat("x", 2000) + block)
		os.Exit(1)
	default:
		if err := os.WriteFile(dest, []byte("snapshot-data"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
}

// fakeEtcdctl returns the path of the running test binary, which acts as etcdctl
// when exec'd with SaveCommand's argv (see TestMain).
func fakeEtcdctl(t *testing.T) string {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	return bin
}

func TestDefaultSave_Success(t *testing.T) {
	bin := fakeEtcdctl(t)
	dest := filepath.Join(t.TempDir(), "etcd-snapshot-ok.db")
	if err := defaultSave(context.Background(), bin, "/root", dest); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("destination not written: %v", err)
	}
}

func TestDefaultSave_ChildObservesNoEnvironment(t *testing.T) {
	// The parent process has ETCDCTL_INSECURE_SKIP_TLS_VERIFY set; production's
	// empty-env child must not inherit it (or anything else).
	t.Setenv("ETCDCTL_INSECURE_SKIP_TLS_VERIFY", "true")
	bin := fakeEtcdctl(t)
	dest := filepath.Join(t.TempDir(), "etcd-snapshot-env.db")
	if err := defaultSave(context.Background(), bin, "/root", dest); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := os.ReadFile(dest + ".environ")
	if err != nil {
		t.Fatalf("child did not record its environment: %v", err)
	}
	if strings.TrimSpace(string(data)) != "0" {
		t.Fatalf("child observed %s environment variable(s), want 0 (ETCDCTL_* must not be inherited)", data)
	}
}

func TestDefaultSave_NonZeroExitProducesBoundedSanitizedDetail(t *testing.T) {
	bin := fakeEtcdctl(t)
	dest := filepath.Join(t.TempDir(), "etcd-snapshot-FAILBIG.db")
	err := defaultSave(context.Background(), bin, "/root", dest)
	if err == nil {
		t.Fatal("expected an error for a non-zero exit")
	}
	msg := err.Error()
	if strings.Contains(msg, "BEGIN PRIVATE KEY") {
		t.Fatalf("detail must be sanitized (no raw PEM block survives): %s", msg)
	}
	if !strings.Contains(msg, "REDACTED-PEM") {
		t.Fatalf("detail must retain the redaction marker within the capped tail: %s", msg)
	}
	if len(msg) > errorDetailCap+256 { // generous allowance for the wrap/quote overhead around the capped body
		t.Fatalf("detail is not bounded: %d bytes: %s", len(msg), msg)
	}
}

func TestRun_DefaultExec_HangingChildKilledWithinBudget(t *testing.T) {
	oldTimeout := snapshotTimeout
	snapshotTimeout = 300 * time.Millisecond
	defer func() { snapshotTimeout = oldTimeout }()

	bin := fakeEtcdctl(t)
	// The etcd image tag becomes the snapshot file name's "from-<tag>" component;
	// steer it to contain the fake etcdctl's HANG sentinel (argv-only signaling,
	// since production runs the child with an empty environment).
	root := newStackedRootWithEtcdTag(t, "cluster-hang-real", 1024, "HANG-0")
	o, dir := testOptions(t, root)
	o.etcdctlPath = bin
	o.save = nil // exercise the REAL default exec path, not the test seam

	start := time.Now()
	res := Run(context.Background(), o)
	elapsed := time.Since(start)

	if res.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %q, want failed (detail=%s)", res.Outcome, res.Detail)
	}
	// The child must have really hung until the sub-deadline (not exited early),
	// and then been killed well before its own hour-long sleep.
	if elapsed < snapshotTimeout {
		t.Fatalf("Run returned after %s, before snapshotTimeout %s: the child did not hang (detail=%s)", elapsed, snapshotTimeout, res.Detail)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Run took %s, expected the hung child to be killed within snapshotTimeout+WaitDelay", elapsed)
	}
	if !strings.Contains(res.Detail, "killed") {
		t.Fatalf("detail should report the child was killed at the deadline: %s", res.Detail)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read snapshot dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Fatalf("a stale .part file was left behind: %s", e.Name())
		}
		if strings.HasPrefix(e.Name(), "etcd-snapshot-") && strings.HasSuffix(e.Name(), ".db") {
			t.Fatalf("an unverified destination was left behind: %s", e.Name())
		}
	}
}
