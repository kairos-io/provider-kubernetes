package kubeadm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kairos-io/provider-kubernetes/internal/hostexec"
)

// Helper-process mode sentinels. Production's ExecRunner always execs a fixed
// absolute path with a CLOSED environment (ADR-1-A1), so a helper process run
// through it cannot be steered by environment variables -- only by the argv
// Run() was given. TestMain re-execs this same test binary and dispatches on
// os.Args[1] to stand in for kubeadm/systemctl/etc. in the tests below (the
// same pattern as internal/etcdsnapshot/exec_test.go's TestMain).
const (
	helperDumpEnviron             = "__dump_environ__"
	helperMarker                  = "__marker__"
	helperFail                    = "__fail__"
	helperSpawnGrandchild         = "__spawn_grandchild__"
	helperSpawnGrandchildThenHang = "__spawn_grandchild_then_hang__"
	helperGrandchildSleep         = "__grandchild_sleep__"
	helperStdinSHA256             = "__stdin_sha256__"
)

func TestMain(m *testing.M) {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case helperDumpEnviron:
			helperDumpEnvironMain(os.Args[2])
		case helperMarker:
			helperMarkerMain(os.Args[2])
		case helperFail:
			helperFailMain()
		case helperSpawnGrandchild:
			helperSpawnGrandchildMain(os.Args[2], false)
		case helperSpawnGrandchildThenHang:
			helperSpawnGrandchildMain(os.Args[2], true)
		case helperGrandchildSleep:
			helperGrandchildSleepMain()
		case helperStdinSHA256:
			helperStdinSHA256Main()
		}
	}
	os.Exit(m.Run())
}

// helperDumpEnvironMain records exactly the environment this process observed,
// so a test can assert the child's os.Environ() equals the configured Env.
func helperDumpEnvironMain(outFile string) {
	_ = os.WriteFile(outFile, []byte(strings.Join(os.Environ(), "\n")), 0o600)
	os.Exit(0)
}

// helperMarkerMain writes a marker file. Used to prove a hostile PATH entry
// masquerading as "kubeadm" is never executed by Run for a relative/empty path.
func helperMarkerMain(markerFile string) {
	_ = os.WriteFile(markerFile, []byte("marker"), 0o600)
	os.Exit(0)
}

// helperFailMain exits non-zero with a secret-free stderr line, to drive Run's
// error path without the stderr itself ever containing "secret".
func helperFailMain() {
	fmt.Fprintln(os.Stderr, "boom")
	os.Exit(1)
}

// helperStdinSHA256Main reads all of fd 0 (its stdin) and prints the hex
// sha256 of exactly what it read -- proving RunStdin wired the real file
// descriptor (not a copy, not a truncated/altered stream) as the child's fd 0.
func helperStdinSHA256Main() {
	h := sha256.New()
	if _, err := io.Copy(h, os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(hex.EncodeToString(h.Sum(nil)))
	os.Exit(0)
}

// helperGrandchildSleepMain is the long-lived grandchild that inherits the
// immediate child's stdout pipe and never closes it on its own.
func helperGrandchildSleepMain() {
	time.Sleep(time.Hour)
	os.Exit(0)
}

// helperSpawnGrandchildMain starts a grandchild (this same binary, in
// helperGrandchildSleep mode) with its Stdout set to this process's own
// os.Stdout -- an *os.File, so exec dup's the underlying fd directly into the
// grandchild rather than piping through Go. That keeps the pipe's write end
// open (via the grandchild) even after this process exits, which is exactly
// the "a grandchild kept stdout open" scenario WaitDelay must bound. The
// grandchild's pid is recorded to pidFile so the test can kill it afterwards.
func helperSpawnGrandchildMain(pidFile string, hang bool) {
	self, err := os.Executable()
	if err != nil {
		os.Exit(9)
	}
	gc := exec.Command(self, helperGrandchildSleep) //nolint:gosec // test helper, fixed argv
	gc.Stdout = os.Stdout
	if err := gc.Start(); err != nil {
		os.Exit(9)
	}
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(gc.Process.Pid)), 0o600)
	if hang {
		time.Sleep(time.Hour)
	}
	os.Exit(0)
}

// testBinary returns the path of the running test binary, which acts as the
// helper process (kubeadm/systemctl stand-in) when re-exec'd per TestMain.
func testBinary(t *testing.T) string {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	return bin
}

// copyExecutable copies src to dst and marks dst executable, so a hostile PATH
// entry can be a real, runnable copy of the test binary rather than an inert
// placeholder -- the test must prove Run never invokes it, not merely that
// nothing happens to be there.
func copyExecutable(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %q: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o700); err != nil {
		t.Fatalf("write %q: %v", dst, err)
	}
}

// killPidFile best-effort kills the pid recorded in pidFile by
// helperSpawnGrandchildMain. Safe to call even if the file was never written.
func killPidFile(t *testing.T, pidFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Kill()
	}
}

// hostileEnviron sets, on the TEST process itself, every variable a closed
// child environment must drop (ADR-1-A1's threat model: a compromised or
// merely noisy provider environment), plus a hostile PATH whose FIRST entry is
// a real, executable copy of the test binary named "kubeadm" that would write
// a marker file if it were ever exec'd. It returns that hostile directory.
func hostileEnviron(t *testing.T) (hostileDir string) {
	t.Helper()
	hostileDir = t.TempDir()
	copyExecutable(t, testBinary(t), filepath.Join(hostileDir, "kubeadm"))

	t.Setenv("PATH", hostileDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SYSTEMD_OFFLINE", "1")
	t.Setenv("KUBERC", "/tmp/x")
	t.Setenv("CONTAINERD_ADDRESS", "/run/evil.sock")
	t.Setenv("GODEBUG", "x=1")
	t.Setenv("SSL_CERT_FILE", "/tmp/evil.pem")
	t.Setenv("HTTPS_PROXY", "http://u:secret@proxy:3128")
	return hostileDir
}

// TestRunClosedEnvironmentAgainstHostileProvider is the E-B2 helper-process
// test: with the calling (provider) process made hostile per hostileEnviron,
// Run must (a) send the child EXACTLY the configured Env, never anything
// inherited; (b) never resolve a relative/empty path via PATH, so the planted
// hostile "kubeadm" never runs; and (c) never let an environment value (e.g. a
// proxied credential) leak into a returned error.
func TestRunClosedEnvironmentAgainstHostileProvider(t *testing.T) {
	hostileEnviron(t)
	bin := testBinary(t)

	t.Run("exact configured env, nothing inherited", func(t *testing.T) {
		knownEnv := []string{"FOO=bar", "PATH=" + hostexec.ChildPATH}
		r := newExecRunner(hostexec.Command{Path: bin, Env: knownEnv})
		out := filepath.Join(t.TempDir(), "environ.out")
		if _, err := r.Run(context.Background(), helperDumpEnviron, out); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("read helper output: %v", err)
		}
		got := strings.Split(string(data), "\n")
		if !slices.Equal(got, knownEnv) {
			t.Fatalf("child observed env %v, want exactly %v", got, knownEnv)
		}
	})

	t.Run("nil env yields zero variables", func(t *testing.T) {
		r := newExecRunner(hostexec.Command{Path: bin, Env: nil})
		out := filepath.Join(t.TempDir(), "environ.out")
		if _, err := r.Run(context.Background(), helperDumpEnviron, out); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("read helper output: %v", err)
		}
		if len(data) != 0 {
			t.Fatalf("nil env child observed %d byte(s) of environment, want 0: %q", len(data), data)
		}
	})

	t.Run("relative and empty path refused without exec", func(t *testing.T) {
		for _, path := range []string{"kubeadm", ""} {
			r := newExecRunner(hostexec.Command{Path: path, Env: []string{}})
			marker := filepath.Join(t.TempDir(), "MARKER")
			if _, err := r.Run(context.Background(), helperMarker, marker); err == nil {
				t.Errorf("path %q: expected Run to refuse, got nil error", path)
			}
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Errorf("path %q: marker file exists -- the hostile PATH kubeadm was executed", path)
			}
		}
	})

	t.Run("error never carries an environment value", func(t *testing.T) {
		r := newExecRunner(hostexec.Command{Path: bin, Env: []string{"HTTPS_PROXY=http://u:secret@proxy:3128"}})
		_, err := r.Run(context.Background(), helperFail)
		if err == nil {
			t.Fatal("expected an error")
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatalf("error leaked an environment value: %v", err)
		}
		if base := filepath.Base(bin); !strings.Contains(err.Error(), base) {
			t.Fatalf("error does not name the binary %q: %v", base, err)
		}
	})
}

// TestExportedConstructorsMatchHostexecBuilders locks each exported runner
// constructor to hostexec's corresponding builder (with os.LookupEnv) and
// confirms DefaultRunner's path is the hostexec constant -- it takes no
// root/path argument, so it can never be root-joined.
func TestExportedConstructorsMatchHostexecBuilders(t *testing.T) {
	cases := []struct {
		name string
		got  ExecRunner
		want hostexec.Command
	}{
		{"DefaultRunner", DefaultRunner(), hostexec.Kubeadm(os.LookupEnv)},
		{"KubectlRunner", KubectlRunner(), hostexec.Kubectl(os.LookupEnv)},
		{"CtrRunner", CtrRunner(), hostexec.Ctr()},
		{"SystemctlRunner", SystemctlRunner(), hostexec.Systemctl()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got.path != c.want.Path {
				t.Errorf("path = %q, want %q", c.got.path, c.want.Path)
			}
			wantEnv := c.want.Env
			if wantEnv == nil {
				wantEnv = []string{}
			}
			if !slices.Equal(c.got.env, wantEnv) {
				t.Errorf("env = %v, want %v", c.got.env, wantEnv)
			}
		})
	}
	if DefaultRunner().path != hostexec.KubeadmPath {
		t.Fatalf("DefaultRunner path = %q, want the hostexec constant %q unconditionally (never root-joined)", DefaultRunner().path, hostexec.KubeadmPath)
	}
}

// setWaitDelay shortens the package's WaitDelay for the duration of one test.
func setWaitDelay(t *testing.T, d time.Duration) {
	t.Helper()
	old := waitDelay
	waitDelay = d
	t.Cleanup(func() { waitDelay = old })
}

// TestRunWaitDelayGrandchildHeldPipe covers E-B4: the direct child exits 0
// almost immediately, but a grandchild it spawned inherited stdout and never
// closes it. Run must not hang waiting for EOF forever -- it must return
// within WaitDelay (+margin) with an error satisfying
// errors.Is(err, exec.ErrWaitDelay), never a false success.
func TestRunWaitDelayGrandchildHeldPipe(t *testing.T) {
	setWaitDelay(t, 200*time.Millisecond)
	bin := testBinary(t)
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	t.Cleanup(func() { killPidFile(t, pidFile) })

	r := newExecRunner(hostexec.Command{Path: bin, Env: []string{}})

	start := time.Now()
	_, err := r.Run(context.Background(), helperSpawnGrandchild, pidFile)
	elapsed := time.Since(start)

	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("err = %v, want errors.Is(err, exec.ErrWaitDelay)", err)
	}
	if elapsed > waitDelay+5*time.Second {
		t.Fatalf("Run took %s, want within WaitDelay(%s)+margin", elapsed, waitDelay)
	}
}

// TestRunWaitDelayCtxDeadlineWithGrandchild covers the other E-B4 case: ctx
// itself expires while a grandchild holds the pipe open. Run must still return
// within ctx+WaitDelay(+margin), with a non-nil error, never hanging past the
// caller's own deadline (#4099-1).
func TestRunWaitDelayCtxDeadlineWithGrandchild(t *testing.T) {
	setWaitDelay(t, 200*time.Millisecond)
	bin := testBinary(t)
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	t.Cleanup(func() { killPidFile(t, pidFile) })

	r := newExecRunner(hostexec.Command{Path: bin, Env: []string{}})

	ctxTimeout := 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), ctxTimeout)
	defer cancel()

	start := time.Now()
	_, err := r.Run(ctx, helperSpawnGrandchildThenHang, pidFile)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error when ctx deadline expires with a grandchild holding the pipe")
	}
	if elapsed > ctxTimeout+waitDelay+5*time.Second {
		t.Fatalf("Run took %s, want within ctx(%s)+WaitDelay(%s)+margin", elapsed, ctxTimeout, waitDelay)
	}
}

// TestRunStdinPassesExactFileContentAsFD0 covers ADR-16-A2 O-7: the child
// observes fd 0 as EXACTLY the wired *os.File's content (RunStdin assigns it
// directly to cmd.Stdin, no io.Reader relay).
func TestRunStdinPassesExactFileContentAsFD0(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload.bin")
	data := []byte("ADR-16-A2 RunStdin payload\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(data)

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open payload: %v", err)
	}
	defer f.Close()

	r := newExecRunner(hostexec.Command{Path: testBinary(t), Env: []string{}})
	res, err := r.RunStdin(context.Background(), f, helperStdinSHA256)
	if err != nil {
		t.Fatalf("RunStdin: %v", err)
	}
	if got := res.Stdout; got != hex.EncodeToString(want[:]) {
		t.Fatalf("child sha256 = %q, want %q", got, hex.EncodeToString(want[:]))
	}
}

// TestRunStdinRefusesNilNonRegularAndWriteMode is ADR-16-A2 O-7: RunStdin
// must refuse -- WITHOUT exec'ing anything -- a nil file, a non-regular file
// (a directory, a FIFO), and a file opened for writing (O_RDWR).
func TestRunStdinRefusesNilNonRegularAndWriteMode(t *testing.T) {
	r := newExecRunner(hostexec.Command{Path: testBinary(t), Env: []string{}})

	assertRefusedNoExec := func(t *testing.T, name string, f *os.File) {
		t.Helper()
		marker := filepath.Join(t.TempDir(), "MARKER")
		if _, err := r.RunStdin(context.Background(), f, helperMarker, marker); err == nil {
			t.Errorf("%s: expected RunStdin to refuse, got nil error", name)
		}
		if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
			t.Errorf("%s: marker file exists -- helperMarker was executed despite the refusal", name)
		}
	}

	t.Run("nil", func(t *testing.T) {
		assertRefusedNoExec(t, "nil", nil)
	})

	t.Run("directory", func(t *testing.T) {
		dir, err := os.Open(t.TempDir())
		if err != nil {
			t.Fatalf("open dir: %v", err)
		}
		defer dir.Close()
		assertRefusedNoExec(t, "directory", dir)
	})

	t.Run("FIFO", func(t *testing.T) {
		fifoPath := filepath.Join(t.TempDir(), "fifo")
		if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
			t.Fatalf("mkfifo: %v", err)
		}
		// O_NONBLOCK so opening for read does not itself block waiting for a
		// writer (there is none) -- this test must not hang either.
		fd, err := syscall.Open(fifoPath, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Fatalf("open fifo: %v", err)
		}
		fifo := os.NewFile(uintptr(fd), fifoPath)
		defer fifo.Close()
		assertRefusedNoExec(t, "FIFO", fifo)
	})

	t.Run("O_RDWR", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "rw.bin")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		rw, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("open O_RDWR: %v", err)
		}
		defer rw.Close()
		assertRefusedNoExec(t, "O_RDWR", rw)
	})
}

// TestRunStdinClosedEnvironmentAndPathRefusal is RunStdin's counterpart to
// TestRunClosedEnvironmentAgainstHostileProvider: with the calling process
// made hostile, the child observes EXACTLY the configured Env, and a
// relative path is refused before anything is exec'd.
func TestRunStdinClosedEnvironmentAndPathRefusal(t *testing.T) {
	hostileEnviron(t)
	bin := testBinary(t)

	payload := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(payload, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("exact configured env, nothing inherited", func(t *testing.T) {
		f, err := os.Open(payload)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		knownEnv := []string{"FOO=bar", "PATH=" + hostexec.ChildPATH}
		r := newExecRunner(hostexec.Command{Path: bin, Env: knownEnv})
		out := filepath.Join(t.TempDir(), "environ.out")
		if _, err := r.RunStdin(context.Background(), f, helperDumpEnviron, out); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("read helper output: %v", err)
		}
		got := strings.Split(string(data), "\n")
		if !slices.Equal(got, knownEnv) {
			t.Fatalf("child observed env %v, want exactly %v", got, knownEnv)
		}
	})

	t.Run("relative path refused without exec", func(t *testing.T) {
		f, err := os.Open(payload)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		r := newExecRunner(hostexec.Command{Path: "kubeadm", Env: []string{}})
		marker := filepath.Join(t.TempDir(), "MARKER")
		if _, err := r.RunStdin(context.Background(), f, helperMarker, marker); err == nil {
			t.Error("expected RunStdin to refuse a relative path")
		}
		if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
			t.Error("marker file exists -- the hostile PATH kubeadm was executed")
		}
	})
}
