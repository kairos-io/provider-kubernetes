//go:build linux

package clusterconfigdir

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/kairos-io/kairos-sdk/clusterplugin"

	"github.com/kairos-io/provider-kubernetes/internal/status"
)

// setSeams points rootPath/expectedOwnerUID at root/uid and restores the
// previous values on cleanup (mirrors internal/imageimport/bundle_linux_test.go
// and internal/unitmigrate/unitmigrate_linux_test.go's seam pattern exactly).
// These tests never run in parallel with each other (no t.Parallel()): the
// seams are package-global.
func setSeams(t *testing.T, root string, uid int) {
	t.Helper()
	oldRoot, oldUID := rootPath, expectedOwnerUID
	rootPath, expectedOwnerUID = root, uid
	t.Cleanup(func() { rootPath, expectedOwnerUID = oldRoot, oldUID })
}

// newValidRoot builds a fresh "/usr/local" tree (without "cloud-config") under
// a temp directory, owned by the test's own uid, and points the walk seams at
// it. Mirrors internal/imageimport/bundle_linux_test.go's newValidTree.
func newValidRoot(t *testing.T) (root, localDir string) {
	t.Helper()
	root = t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatalf("chmod root: %v", err)
	}
	usrDir := filepath.Join(root, "usr")
	localDir = filepath.Join(usrDir, "local")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("mkdir usr/local: %v", err)
	}
	setSeams(t, root, os.Getuid())
	return root, localDir
}

func TestEnsureCreatesDirectoryFresh(t *testing.T) {
	_, localDir := newValidRoot(t)
	// t.TempDir()'s tree lives entirely on one real filesystem, so without
	// this override checkNotPersistent would (correctly, for this fixture)
	// report true -- a concern this test isn't about (see
	// TestEnsureNotPersistentReportedNotRefused for that).
	old := notPersistentOverride
	notPersistentOverride = func(rootFd, localFd int) bool { return false }
	t.Cleanup(func() { notPersistentOverride = old })

	rep := ensureDir(DefaultFileName)
	if rep.Reason != ReasonNone || rep.Withhold {
		t.Fatalf("unexpected report: %+v", rep)
	}

	info, err := os.Lstat(filepath.Join(localDir, "cloud-config"))
	if err != nil {
		t.Fatalf("cloud-config was not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("cloud-config must be a directory")
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("cloud-config mode = %o, want 0700", info.Mode().Perm())
	}
}

// TestEnsureSymlinkedLocalRefused is S-D3-2's "symlinked local" case: every
// ancestor up to and including "usr" is valid and owned by the test's own
// uid (matching expectedOwnerUID); "local" itself is a symlink. The walk
// must refuse (openDirNoFollow's O_NOFOLLOW returns ELOOP), WITHHOLD
// (S-D3-5a: our walk verified nothing about where the symlink leads, and the
// SDK's own open resolves the full path by name straight through it), and
// create nothing anywhere, including through the symlink's target.
//
// Mutation (a) from the task: replacing this openat walk with os.MkdirAll
// would follow the symlink and MkdirAll("cloud-config") under the symlink
// target, making this test fail (no ELOOP, a directory silently appears
// under target/cloud-config instead of being refused).
//
// Mutation (i) from the coordinator's S-D3-5a follow-up: dropping the new
// ancestor withhold (i.e. reverting ancestorErrReport's ELOOP/ENOTDIR branch
// to Withhold: false) must make this test fail -- see the mutation run
// recorded in the implementation report.
func TestEnsureSymlinkedLocalRefused(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatalf("chmod root: %v", err)
	}
	usrDir := filepath.Join(root, "usr")
	if err := os.MkdirAll(usrDir, 0o755); err != nil {
		t.Fatalf("mkdir usr: %v", err)
	}
	target := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir elsewhere: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(usrDir, "local")); err != nil {
		t.Fatalf("symlink usr/local: %v", err)
	}
	setSeams(t, root, os.Getuid())

	rep := ensureDir(DefaultFileName)
	if rep.Reason != ReasonAncestorUnsafe {
		t.Fatalf("reason = %q, want %q", rep.Reason, ReasonAncestorUnsafe)
	}
	if !rep.Withhold {
		t.Fatal("ancestor-unsafe (a symlinked ancestor) must withhold (S-D3-5a)")
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("read symlink target: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected nothing created through the symlink target, got %v", entries)
	}
}

// TestEnsureMissingLocalReportedNotWithheld is S-D3-2's "missing local" case,
// and the S-D3-5a amendment's other required test: a missing ancestor is
// reported (ReasonAncestorMissing) but must NOT withhold -- the SDK's own
// open will fail the identical ENOENT either way, so withholding would only
// replace one legible error with a second one.
func TestEnsureMissingLocalReportedNotWithheld(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatalf("chmod root: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "usr"), 0o755); err != nil {
		t.Fatalf("mkdir usr: %v", err)
	}
	setSeams(t, root, os.Getuid())

	rep := ensureDir(DefaultFileName)
	if rep.Reason != ReasonAncestorMissing {
		t.Fatalf("reason = %q, want %q", rep.Reason, ReasonAncestorMissing)
	}
	if rep.Withhold {
		t.Fatal("a missing ancestor must not withhold (S-D3-5a)")
	}
}

// TestEnsureNonRootOwnedLocalRefused is S-D3-2's "non-root-owned local" case.
// Mirrors internal/imageimport/bundle_linux_test.go's TestWalkBundleWrongOwner:
// build a fully valid tree at the test's own uid, then point expectedOwnerUID
// at a uid nothing in the tree has, so the "owned by uid 0 (expectedOwnerUID
// in production)" check fails. This is an ancestor that EXISTS but fails the
// safety check, not a missing one, so it withholds (S-D3-5a).
func TestEnsureNonRootOwnedLocalRefused(t *testing.T) {
	root, _ := newValidRoot(t)
	setSeams(t, root, os.Getuid()+12345) // no real file matches this uid

	rep := ensureDir(DefaultFileName)
	if rep.Reason != ReasonAncestorUnsafe {
		t.Fatalf("reason = %q, want %q", rep.Reason, ReasonAncestorUnsafe)
	}
	if !rep.Withhold {
		t.Fatal("a non-root-owned ancestor must withhold (S-D3-5a)")
	}
}

// TestEnsureMissingLocalRecordsStatusReportOnly is the Ensure-level (not
// just ensureDir) analog of TestEnsureMissingLocalReportedNotWithheld: it
// confirms the missing-ancestor case is genuinely "reported" (S-D3-9: a
// status record is written) even though it is not withheld, using the same
// fake-sink seam style as clusterconfigdir_test.go's withFakeSink.
func TestEnsureMissingLocalRecordsStatusReportOnly(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatalf("chmod root: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "usr"), 0o755); err != nil {
		t.Fatalf("mkdir usr: %v", err)
	}
	setSeams(t, root, os.Getuid())

	sink := &fakeSink{}
	oldSink := statusSink
	statusSink = sink
	t.Cleanup(func() { statusSink = oldSink })

	rep := Ensure(clusterplugin.Cluster{})
	if rep.Reason != ReasonAncestorMissing || rep.Withhold {
		t.Fatalf("report = %+v, want {Reason: %q, Withhold: false}", rep, ReasonAncestorMissing)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("expected exactly one status record, got %d", len(sink.calls))
	}
	if sink.calls[0].Reason != status.ReasonClusterConfigAncestorMissing {
		t.Fatalf("status reason = %q, want %q", sink.calls[0].Reason, status.ReasonClusterConfigAncestorMissing)
	}
	if sink.calls[0].Terminal {
		t.Fatal("a report-only status record must not be Terminal")
	}
}

// TestCreateOrCheckCloudConfigDir covers S-D3-2's mkdirat and S-D3-3's
// EEXIST rules directly against real fds, isolated from the ancestor walk.
func TestCreateOrCheckCloudConfigDir(t *testing.T) {
	old := expectedOwnerUID
	expectedOwnerUID = os.Getuid()
	t.Cleanup(func() { expectedOwnerUID = old })

	cases := []struct {
		name         string
		setup        func(t *testing.T, localDir string)
		wantReason   Reason
		wantWithhold bool
	}{
		{
			name:  "fresh create",
			setup: func(t *testing.T, localDir string) {},
		},
		{
			name: "existing safe directory",
			setup: func(t *testing.T, localDir string) {
				if err := os.Mkdir(filepath.Join(localDir, "cloud-config"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// mode&0o002: 0o777 sets the other-write bit, so this must
			// still be reported under the S-D3-3 VM-run narrowing.
			name: "existing other-writable (0777) directory is reported not refused",
			setup: func(t *testing.T, localDir string) {
				dir := filepath.Join(localDir, "cloud-config")
				if err := os.Mkdir(dir, 0o777); err != nil {
					t.Fatal(err)
				}
				// Mkdir's mode is masked by the process umask, so under the
				// usual 022 the directory would come out 0755 and the very
				// condition under test -- other-writable -- would not
				// exist. Chmod is not masked; without it this passes only on
				// a umask that happens to keep the bits (it passed locally
				// under 002 and failed in CI under 022).
				if err := os.Chmod(dir, 0o777); err != nil {
					t.Fatal(err)
				}
			},
			wantReason: ReasonDirWritable,
		},
		{
			// S-D3-3 VM-run finding (2026-09-21): kairos-init's
			// 10_accounting.yaml chmods the directory 0770 root:admin about
			// 130ms after we create it, on every boot from the second
			// onward. That is the platform's own EXPECTED state, not an
			// attacker's, and it is group-writable, not other-writable, so
			// it must NOT be reported. Mutation (i): widening the check back
			// to mode&0o022 makes this subtest fail (it would report
			// ReasonDirWritable on the platform's normal state, on every
			// boot from the second onward).
			name: "existing 0770 root:admin directory (platform's normal state) is NOT reported",
			setup: func(t *testing.T, localDir string) {
				dir := filepath.Join(localDir, "cloud-config")
				if err := os.Mkdir(dir, 0o770); err != nil {
					t.Fatal(err)
				}
				// Same umask concern as above: chmod explicitly so the mode
				// under test is exact regardless of the process umask.
				if err := os.Chmod(dir, 0o770); err != nil {
					t.Fatal(err)
				}
			},
			wantReason: ReasonNone,
		},
		{
			name: "symlink refused",
			setup: func(t *testing.T, localDir string) {
				if err := os.Symlink("/etc", filepath.Join(localDir, "cloud-config")); err != nil {
					t.Fatal(err)
				}
			},
			wantReason:   ReasonDirUnsafe,
			wantWithhold: true,
		},
		{
			name: "regular file refused",
			setup: func(t *testing.T, localDir string) {
				if err := os.WriteFile(filepath.Join(localDir, "cloud-config"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantReason:   ReasonDirUnsafe,
			wantWithhold: true,
		},
		{
			name: "fifo refused (bounded, never blocking)",
			setup: func(t *testing.T, localDir string) {
				if err := unix.Mkfifo(filepath.Join(localDir, "cloud-config"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantReason:   ReasonDirUnsafe,
			wantWithhold: true,
		},
		{
			name: "existing directory owned by someone else refused",
			setup: func(t *testing.T, localDir string) {
				if err := os.Mkdir(filepath.Join(localDir, "cloud-config"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			wantReason:   ReasonDirUnsafe,
			wantWithhold: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			localDir := t.TempDir()
			c.setup(t, localDir)

			owner := expectedOwnerUID
			if c.name == "existing directory owned by someone else refused" {
				// Simulate a mismatched owner the same way
				// TestEnsureNonRootOwnedLocalRefused does: nothing in the
				// fixture matches this uid.
				owner = os.Getuid() + 12345
			}
			oldOwner := expectedOwnerUID
			expectedOwnerUID = owner
			defer func() { expectedOwnerUID = oldOwner }()

			localFd, err := unix.Open(localDir, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				t.Fatalf("open localDir: %v", err)
			}
			defer func() { _ = unix.Close(localFd) }()

			reason, withhold := createOrCheckCloudConfigDir(localFd)
			if reason != c.wantReason {
				t.Fatalf("reason = %q, want %q", reason, c.wantReason)
			}
			if withhold != c.wantWithhold {
				t.Fatalf("withhold = %t, want %t", withhold, c.wantWithhold)
			}
		})
	}
}

// TestCheckTokenFile covers S-D3-4's allowlist (amended S-D3-4a): the two
// cases the original condition set named explicitly (a pre-created 0644
// file owned by a non-root writer, and a FIFO, which must be refused in
// bounded time, never opened blocking), plus the two type-confusion cases
// the security review's S-D3-4a amendment added (a directory and a socket
// planted at the token path) that an enumerated unsafe set would have
// missed. Mutation (b) from the task removes this preflight's call site
// entirely, which would make the 0644 case below pass through as safe (see
// the end-to-end tests below for the mutation that actually catches that).
// Mutation (ii) from the S-D3-5a/S-D3-4a follow-up restores the old
// enumerated unsafe set, which makes the "directory at token path" subtest
// fail.
func TestCheckTokenFile(t *testing.T) {
	old := expectedOwnerUID
	expectedOwnerUID = os.Getuid()
	t.Cleanup(func() { expectedOwnerUID = old })

	cases := []struct {
		name       string
		setup      func(t *testing.T, dir string)
		wantReason Reason
	}{
		{
			name:  "missing is safe",
			setup: func(t *testing.T, dir string) {},
		},
		{
			name: "clean pre-existing 0600 file is safe",
			setup: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, DefaultFileName), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "pre-created 0644 file is unsafe (mode loose)",
			setup: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, DefaultFileName), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantReason: ReasonTokenFileUnsafe,
		},
		{
			name: "symlink is unsafe",
			setup: func(t *testing.T, dir string) {
				if err := os.Symlink("/etc/passwd", filepath.Join(dir, DefaultFileName)); err != nil {
					t.Fatal(err)
				}
			},
			wantReason: ReasonTokenFileUnsafe,
		},
		{
			name: "fifo is unsafe and must not be opened blocking",
			setup: func(t *testing.T, dir string) {
				if err := unix.Mkfifo(filepath.Join(dir, DefaultFileName), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantReason: ReasonTokenFileUnsafe,
		},
		{
			name: "hard-linked file is unsafe (nlink > 1)",
			setup: func(t *testing.T, dir string) {
				other := filepath.Join(dir, "other")
				if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(other, filepath.Join(dir, DefaultFileName)); err != nil {
					t.Fatal(err)
				}
			},
			wantReason: ReasonTokenFileUnsafe,
		},
		{
			name: "owned by someone else is unsafe",
			setup: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, DefaultFileName), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantReason: ReasonTokenFileUnsafe,
		},
		{
			// S-D3-4a: the allowlist rejects anything that is not ENOENT or
			// an intact regular file -- a directory is exactly the kind of
			// "type nobody enumerated" the amendment is about. Mutation (ii)
			// (restoring the old enumerated unsafe set, whose switch's
			// `default:` branch returned ReasonNone for S_ISDIR) makes this
			// subtest fail.
			name: "directory at token path is unsafe",
			setup: func(t *testing.T, dir string) {
				if err := os.Mkdir(filepath.Join(dir, DefaultFileName), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			wantReason: ReasonTokenFileUnsafe,
		},
		{
			// S-D3-4a: a UNIX domain socket special file is likewise not in
			// the allowlist.
			name: "socket at token path is unsafe",
			setup: func(t *testing.T, dir string) {
				fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = unix.Close(fd) }()
				addr := &unix.SockaddrUnix{Name: filepath.Join(dir, DefaultFileName)}
				if err := unix.Bind(fd, addr); err != nil {
					t.Fatal(err)
				}
			},
			wantReason: ReasonTokenFileUnsafe,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			c.setup(t, dir)

			if c.name == "owned by someone else is unsafe" {
				oldOwner := expectedOwnerUID
				expectedOwnerUID = os.Getuid() + 12345
				defer func() { expectedOwnerUID = oldOwner }()
			}

			dirFd, err := unix.Open(dir, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				t.Fatalf("open dir: %v", err)
			}
			defer func() { _ = unix.Close(dirFd) }()

			// Bounded: checkTokenFile must never block (#4099-1). A FIFO
			// opened without O_NONBLOCK would hang this goroutine forever;
			// the select below turns that into a test failure instead of a
			// wedged test run.
			done := make(chan Reason, 1)
			go func() { done <- checkTokenFile(dirFd, DefaultFileName) }()
			select {
			case got := <-done:
				if got != c.wantReason {
					t.Fatalf("reason = %q, want %q", got, c.wantReason)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("checkTokenFile blocked (must never hang, #4099-1)")
			}
		})
	}
}

func TestCheckNotPersistentOverrideSeam(t *testing.T) {
	old := notPersistentOverride
	t.Cleanup(func() { notPersistentOverride = old })

	notPersistentOverride = func(rootFd, localFd int) bool { return true }
	if !checkNotPersistent(-1, -1) {
		t.Fatal("expected override to report not-persistent")
	}

	notPersistentOverride = func(rootFd, localFd int) bool { return false }
	if checkNotPersistent(-1, -1) {
		t.Fatal("expected override to report persistent")
	}
}

// TestEnsureNotPersistentReportedNotRefused is S-D3-8: the override seam
// simulates "/usr/local" NOT being a separate mount; Ensure must still
// create the directory and use the real config (report only, never refuse).
func TestEnsureNotPersistentReportedNotRefused(t *testing.T) {
	_, localDir := newValidRoot(t)

	old := notPersistentOverride
	notPersistentOverride = func(rootFd, localFd int) bool { return true }
	t.Cleanup(func() { notPersistentOverride = old })

	rep := ensureDir(DefaultFileName)
	if rep.Withhold {
		t.Fatal("not-persistent must never withhold")
	}
	if rep.Reason != ReasonNotPersistent {
		t.Fatalf("reason = %q, want %q", rep.Reason, ReasonNotPersistent)
	}
	if _, err := os.Lstat(filepath.Join(localDir, "cloud-config")); err != nil {
		t.Fatalf("directory must still be created even when not persistent: %v", err)
	}
}

// TestEnsureNotPersistentPreservesExistingConvergedPhase is S-D3-9a's
// end-to-end test (through the real Ensure/ensureDir walk, not a synthetic
// Report): a converged node whose "/usr/local" happens to not be a separate
// mount this boot must keep reporting Converged, not be told it Failed. This
// is the exact VM-run scenario: the real walk drives ReasonNotPersistent,
// and the phase-preservation must hold end to end, not just through the
// pure MergeReportOnly/report() unit tests in clusterconfigdir_test.go.
func TestEnsureNotPersistentPreservesExistingConvergedPhase(t *testing.T) {
	_, localDir := newValidRoot(t)

	old := notPersistentOverride
	notPersistentOverride = func(rootFd, localFd int) bool { return true }
	t.Cleanup(func() { notPersistentOverride = old })

	sink := &fakeSink{}
	oldSink := statusSink
	statusSink = sink
	t.Cleanup(func() { statusSink = oldSink })

	oldReader := statusReader
	statusReader = func() (status.Status, bool) {
		return status.Status{Phase: status.PhaseConverged, Outcome: status.OutcomeSuccess}, true
	}
	t.Cleanup(func() { statusReader = oldReader })

	rep := Ensure(clusterplugin.Cluster{Role: "controlplane"})
	if rep.Reason != ReasonNotPersistent || rep.Withhold {
		t.Fatalf("report = %+v, want {Reason: %q, Withhold: false}", rep, ReasonNotPersistent)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("expected exactly one status record, got %d", len(sink.calls))
	}
	if sink.calls[0].Phase != status.PhaseConverged {
		t.Fatalf("Phase = %q, want the preserved %q (S-D3-9a)", sink.calls[0].Phase, status.PhaseConverged)
	}
	if _, err := os.Lstat(filepath.Join(localDir, "cloud-config")); err != nil {
		t.Fatalf("directory must still be created: %v", err)
	}
}

// TestEnsurePlantedSymlinkWithholdsAndNeverWritesThroughIt is the unit-test
// analog of S-D3-11(iii): plant /usr/local/cloud-config -> /etc and confirm
// Ensure withholds, and that nothing was created under the symlink's target.
func TestEnsurePlantedSymlinkWithholdsAndNeverWritesThroughIt(t *testing.T) {
	root, localDir := newValidRoot(t)
	fakeEtc := filepath.Join(root, "etc")
	if err := os.MkdirAll(fakeEtc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fakeEtc, filepath.Join(localDir, "cloud-config")); err != nil {
		t.Fatal(err)
	}

	rep := ensureDir(DefaultFileName)
	if !rep.Withhold || rep.Reason != ReasonDirUnsafe {
		t.Fatalf("report = %+v, want withhold ReasonDirUnsafe", rep)
	}

	entries, err := os.ReadDir(fakeEtc)
	if err != nil {
		t.Fatalf("read fakeEtc: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected nothing written under the symlink target, got %v", entries)
	}
}

// TestEnsurePlantedFIFOAtTokenFileRefusedBounded is the unit-test analog of
// S-D3-11(iv): a root-owned FIFO at cluster.kairos.yaml must be refused in
// bounded time.
func TestEnsurePlantedFIFOAtTokenFileRefusedBounded(t *testing.T) {
	_, localDir := newValidRoot(t)
	cloudConfigDir := filepath.Join(localDir, "cloud-config")
	if err := os.Mkdir(cloudConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(cloudConfigDir, DefaultFileName), 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan Report, 1)
	go func() { done <- ensureDir(DefaultFileName) }()
	select {
	case rep := <-done:
		if !rep.Withhold || rep.Reason != ReasonTokenFileUnsafe {
			t.Fatalf("report = %+v, want withhold ReasonTokenFileUnsafe", rep)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ensureDir blocked on a planted FIFO (must never hang, #4099-1)")
	}
}

// TestEnsurePlantedWorldReadableTokenFileWithholds is the end-to-end
// (through ensureDir, not the isolated checkTokenFile unit) version of the
// task's mutation (b): "drop the token-file preflight -> the
// pre-created-0644-file test must fail". TestCheckTokenFile's
// "pre-created_0644" subtest alone does not catch a mutation that removes
// checkTokenFile's CALL SITE in ensureDir (it calls checkTokenFile directly);
// this test exercises the real wiring end to end and is what actually fails
// under that mutation.
func TestEnsurePlantedWorldReadableTokenFileWithholds(t *testing.T) {
	_, localDir := newValidRoot(t)
	cloudConfigDir := filepath.Join(localDir, "cloud-config")
	if err := os.Mkdir(cloudConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cloudConfigDir, DefaultFileName), []byte("planted"), 0o644); err != nil {
		t.Fatal(err)
	}

	rep := ensureDir(DefaultFileName)
	if !rep.Withhold || rep.Reason != ReasonTokenFileUnsafe {
		t.Fatalf("report = %+v, want withhold ReasonTokenFileUnsafe", rep)
	}
}

// TestEnsurePlantedDirectoryAtTokenFileWithholds is the end-to-end (through
// ensureDir) version of "a directory planted at the token path -> withhold"
// (S-D3-4a follow-up). This is the test mutation (ii) -- restoring the old
// enumerated unsafe set -- must fail.
func TestEnsurePlantedDirectoryAtTokenFileWithholds(t *testing.T) {
	_, localDir := newValidRoot(t)
	cloudConfigDir := filepath.Join(localDir, "cloud-config")
	if err := os.Mkdir(cloudConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cloudConfigDir, DefaultFileName), 0o700); err != nil {
		t.Fatal(err)
	}

	rep := ensureDir(DefaultFileName)
	if !rep.Withhold || rep.Reason != ReasonTokenFileUnsafe {
		t.Fatalf("report = %+v, want withhold ReasonTokenFileUnsafe", rep)
	}
}
