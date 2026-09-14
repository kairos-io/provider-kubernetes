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

// --- 1. SaveCommand ---------------------------------------------------------

func TestSaveCommand(t *testing.T) {
	argv, env := SaveCommand("/usr/bin/etcdctl", "/rootpath", "/snap/dir/dest.db")

	if argv[0] != "/usr/bin/etcdctl" {
		t.Fatalf("argv[0] = %q, want the given etcdctl path", argv[0])
	}

	pkiDir := "/rootpath/etc/kubernetes/pki/etcd"
	for flag, want := range map[string]string{
		"--cacert=": pkiDir + "/ca.crt",
		"--cert=":   pkiDir + "/healthcheck-client.crt",
		"--key=":    pkiDir + "/healthcheck-client.key",
	} {
		if got, ok := argWithPrefix(argv, flag); !ok || got != want {
			t.Errorf("%s: got %q (present=%v), want %q", flag, got, ok, want)
		}
	}

	if !hasArg(argv, "--endpoints=https://127.0.0.1:2379") {
		t.Errorf("argv missing the loopback endpoint: %v", argv)
	}

	n := len(argv)
	if n < 3 || argv[n-3] != "snapshot" || argv[n-2] != "save" || argv[n-1] != "/snap/dir/dest.db" {
		t.Fatalf("argv must end with 'snapshot save <dest>', got %v", argv)
	}

	ct, ok := argWithPrefix(argv, "--command-timeout=")
	if !ok {
		t.Fatal("argv missing --command-timeout")
	}
	d, err := time.ParseDuration(ct)
	if err != nil {
		t.Fatalf("--command-timeout=%q did not parse as a duration: %v", ct, err)
	}
	if d >= SnapshotTimeout {
		t.Fatalf("--command-timeout %s must be strictly below SnapshotTimeout %s", d, SnapshotTimeout)
	}

	if env == nil {
		t.Fatal("env must be non-nil")
	}
	if len(env) != 0 {
		t.Fatalf("env must be empty, got %v", env)
	}

	for _, a := range argv {
		if strings.Contains(a, "PRIVATE KEY") || strings.Contains(a, "BEGIN CERTIFICATE") {
			t.Fatalf("argv element looks like it carries key material: %q", a)
		}
	}
}

// --- 3. Missing etcdctl / non-stacked ---------------------------------------

func TestRun_NonStackedEtcdRefused(t *testing.T) {
	root := t.TempDir() // no etcd manifest/member dir
	o, dir := testOptions(t, root)
	res := Run(context.Background(), o)
	if res.Outcome != OutcomeExternalEtcd {
		t.Fatalf("outcome = %q, want %q (detail=%s)", res.Outcome, OutcomeExternalEtcd, res.Detail)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("snapshot dir must not be created on non-stacked etcd, stat err=%v", err)
	}
}

func TestRun_MissingEtcdctl(t *testing.T) {
	root := newStackedRoot(t, "cluster-missing-etcdctl", 1024)
	o, dir := testOptions(t, root)
	o.etcdctlPath = filepath.Join(t.TempDir(), "does-not-exist")
	res := Run(context.Background(), o)
	if res.Outcome != OutcomeEtcdctlMissing {
		t.Fatalf("outcome = %q, want %q (detail=%s)", res.Outcome, OutcomeEtcdctlMissing, res.Detail)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("snapshot dir must not be created when etcdctl is missing, stat err=%v", err)
	}
}

func TestRun_EtcdctlSymlinkCountsAsMissing(t *testing.T) {
	root := newStackedRoot(t, "cluster-etcdctl-symlink", 1024)
	real := regularFileEtcdctl(t)
	link := filepath.Join(t.TempDir(), "etcdctl-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	o, _ := testOptions(t, root)
	o.etcdctlPath = link
	res := Run(context.Background(), o)
	if res.Outcome != OutcomeEtcdctlMissing {
		t.Fatalf("a symlinked etcdctl path must count as missing, got %q", res.Outcome)
	}
}

func TestRun_EtcdctlDirectoryCountsAsMissing(t *testing.T) {
	root := newStackedRoot(t, "cluster-etcdctl-dir", 1024)
	o, _ := testOptions(t, root)
	o.etcdctlPath = t.TempDir() // a directory, not a regular file
	res := Run(context.Background(), o)
	if res.Outcome != OutcomeEtcdctlMissing {
		t.Fatalf("a directory etcdctl path must count as missing, got %q", res.Outcome)
	}
}

// --- 4. Dir safety -----------------------------------------------------------

func TestForbiddenDirs_DefaultDirIsLexicallyAllowed(t *testing.T) {
	if err := checkNotForbidden("/", filepath.Clean(DefaultDir)); err != nil {
		t.Fatalf("DefaultDir must be allowed by the forbidden-prefix check: %v", err)
	}
}

func TestForbiddenDirs_RejectsResetAndStateArtifactPaths(t *testing.T) {
	root := "/custom-root"
	for _, bad := range []string{
		filepath.Join(root, "etc", "kubernetes", "backup"),
		filepath.Join(root, "var", "lib", "etcd", "snap"),
		filepath.Join(root, "var", "lib", "kubelet", "x"),
		filepath.Join(root, "usr", "local", ".state", "x"),
		"/etc/kubernetes/backup",
		"/var/lib/kubelet/x",
		"/var/lib/etcd/snap",
		"/usr/local/.state/x",
	} {
		if err := checkNotForbidden(root, filepath.Clean(bad)); err == nil {
			t.Fatalf("expected checkNotForbidden to reject %q", bad)
		}
	}
}

func TestRun_DirSafetyViolationsFailWithoutSave(t *testing.T) {
	cases := map[string]func(root string) (dir string, ownerUID int){
		"relative":                 func(root string) (string, int) { return "relative/dir", os.Getuid() },
		"forbidden etc/kubernetes": func(root string) (string, int) { return filepath.Join(root, "etc", "kubernetes", "x"), os.Getuid() },
		"forbidden usr/local/.state": func(root string) (string, int) {
			return filepath.Join(root, "usr", "local", ".state", "x"), os.Getuid()
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			root := newStackedRoot(t, "cluster-dirsafety-"+name, 1024)
			dir, uid := mk(root)
			o, _ := testOptions(t, root)
			o.dir = dir
			o.ownerUID = uid
			res := Run(context.Background(), o)
			if res.Outcome != OutcomeFailed {
				t.Fatalf("outcome = %q, want failed (detail=%s)", res.Outcome, res.Detail)
			}
		})
	}
}

func TestRun_WrongOwnerFailsWithoutSave(t *testing.T) {
	root := newStackedRoot(t, "cluster-wrong-owner", 1024)
	o, _ := testOptions(t, root)
	o.ownerUID = os.Getuid() + 424242 // deliberately does not match the real owner
	res := Run(context.Background(), o)
	if res.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %q, want failed (detail=%s)", res.Outcome, res.Detail)
	}
}

func TestRun_SymlinkedLeafDirFailsWithoutSave(t *testing.T) {
	root := newStackedRoot(t, "cluster-symlink-leaf", 1024)
	base := t.TempDir()
	real := filepath.Join(base, "real")
	mustMkdirAll(t, real)
	leaf := filepath.Join(base, "leaf-link")
	if err := os.Symlink(real, leaf); err != nil {
		t.Fatal(err)
	}
	o, _ := testOptions(t, root)
	o.dir = leaf
	res := Run(context.Background(), o)
	if res.Outcome != OutcomeFailed {
		t.Fatalf("a symlinked leaf dir must fail, got %q (detail=%s)", res.Outcome, res.Detail)
	}
}

func TestRun_SymlinkedParentDirFailsWithoutSave(t *testing.T) {
	root := newStackedRoot(t, "cluster-symlink-parent", 1024)
	base := t.TempDir()
	real := filepath.Join(base, "real-parent")
	mustMkdirAll(t, real)
	linkedParent := filepath.Join(base, "linked-parent")
	if err := os.Symlink(real, linkedParent); err != nil {
		t.Fatal(err)
	}
	o, _ := testOptions(t, root)
	o.dir = filepath.Join(linkedParent, "etcd-backup") // does not exist yet
	res := Run(context.Background(), o)
	if res.Outcome != OutcomeFailed {
		t.Fatalf("a dir under a symlinked parent must fail, got %q (detail=%s)", res.Outcome, res.Detail)
	}
}

func TestEnsureDirSafe_CreatesMode0700(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "etcd-backup")
	if err := ensureDirSafe(root, dir, os.Getuid()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %o, want 0700", info.Mode().Perm())
	}
}

func TestEnsureDirSafe_TightensLoosePermTo0700(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "etcd-backup")
	mustMkdirAll(t, dir)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureDirSafe(root, dir, os.Getuid()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %o, want tightened to 0700", info.Mode().Perm())
	}
}

func TestRun_EncryptionConfirmedReceivesExactDir(t *testing.T) {
	root := newStackedRoot(t, "cluster-enc-dir", 1024)
	o, dir := testOptions(t, root)
	var gotDir string
	o.EncryptionConfirmed = func(_ context.Context, d string) bool { gotDir = d; return true }
	o.save = func(_ context.Context, dest string) error { return os.WriteFile(dest, []byte("x"), 0o600) }

	res := Run(context.Background(), o)
	if res.Outcome != OutcomeTaken {
		t.Fatalf("unexpected outcome %q (detail=%s)", res.Outcome, res.Detail)
	}
	if gotDir != dir {
		t.Fatalf("EncryptionConfirmed dir = %q, want %q", gotDir, dir)
	}
}

// --- 5. Encryption refusal ---------------------------------------------------

func TestRun_EncryptionUnconfirmedRefuses(t *testing.T) {
	root := newStackedRoot(t, "cluster-enc-refuse", 1024)
	o, _ := testOptions(t, root)
	o.EncryptionConfirmed = func(context.Context, string) bool { return false }
	res := Run(context.Background(), o)
	if res.Outcome != OutcomeEncryptionUnconfirmed {
		t.Fatalf("outcome = %q, want %q", res.Outcome, OutcomeEncryptionUnconfirmed)
	}
	if !strings.Contains(res.Detail, "/etc/kubernetes/tmp") {
		t.Fatalf("detail must mention /etc/kubernetes/tmp, got %q", res.Detail)
	}
}

// --- 6. Once per (cluster, target) ------------------------------------------

func TestRun_SameClusterAndTargetSkipsSecondRun(t *testing.T) {
	root := newStackedRoot(t, "cluster-once", 1024)
	o, _ := testOptions(t, root)
	var saveCalls int
	o.save = func(_ context.Context, dest string) error {
		saveCalls++
		return os.WriteFile(dest, []byte("snap1"), 0o600)
	}
	first := Run(context.Background(), o)
	if first.Outcome != OutcomeTaken {
		t.Fatalf("first run outcome = %q (detail=%s)", first.Outcome, first.Detail)
	}
	if saveCalls != 1 {
		t.Fatalf("expected exactly 1 save call, got %d", saveCalls)
	}

	// Simulate an apply retry: same options, later timestamp, save must not run.
	o.save = failIfCalled(t)
	o.now = time.Now().Add(time.Minute)
	second := Run(context.Background(), o)
	if second.Outcome != OutcomeAlreadyTaken {
		t.Fatalf("second run outcome = %q, want already-taken (detail=%s)", second.Outcome, second.Detail)
	}
	if second.Path != first.Path {
		t.Fatalf("second run path = %q, want the first snapshot %q", second.Path, first.Path)
	}
	data, err := os.ReadFile(first.Path)
	if err != nil || string(data) != "snap1" {
		t.Fatalf("first snapshot must be untouched: data=%q err=%v", data, err)
	}
}

func TestRun_DifferentClusterCATakesNewAndPrunesOld(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "etcd-backup")

	rootA := newStackedRoot(t, "cluster-A", 1024)
	oA, _ := testOptions(t, rootA)
	oA.dir = dir
	oA.save = func(_ context.Context, dest string) error { return os.WriteFile(dest, []byte("A"), 0o600) }
	rA := Run(context.Background(), oA)
	if rA.Outcome != OutcomeTaken {
		t.Fatalf("rA outcome = %q (detail=%s)", rA.Outcome, rA.Detail)
	}

	rootB := newStackedRoot(t, "cluster-B", 1024)
	oB, _ := testOptions(t, rootB)
	oB.dir = dir
	oB.save = func(_ context.Context, dest string) error { return os.WriteFile(dest, []byte("B"), 0o600) }
	rB := Run(context.Background(), oB)
	if rB.Outcome != OutcomeTaken {
		t.Fatalf("rB outcome = %q (detail=%s)", rB.Outcome, rB.Detail)
	}

	if rA.Path == rB.Path {
		t.Fatalf("expected distinct snapshot names for distinct cluster CAs")
	}
	if _, err := os.Stat(rA.Path); !os.IsNotExist(err) {
		t.Fatalf("cluster A's old snapshot must be pruned once cluster B's lands, stat err=%v", err)
	}
	if _, err := os.Stat(rB.Path); err != nil {
		t.Fatalf("the new snapshot must exist: %v", err)
	}
}

func TestRun_DifferentTargetTakesNewAndPrunesOld(t *testing.T) {
	root := newStackedRoot(t, "cluster-target-prune", 1024)
	dir := filepath.Join(t.TempDir(), "etcd-backup")

	o1, _ := testOptions(t, root)
	o1.dir = dir
	o1.TargetMinor = "1.36"
	o1.save = func(_ context.Context, dest string) error { return os.WriteFile(dest, []byte("v136"), 0o600) }
	r1 := Run(context.Background(), o1)
	if r1.Outcome != OutcomeTaken {
		t.Fatalf("r1 outcome = %q (detail=%s)", r1.Outcome, r1.Detail)
	}

	o2, _ := testOptions(t, root)
	o2.dir = dir
	o2.TargetMinor = "1.37"
	o2.save = func(_ context.Context, dest string) error { return os.WriteFile(dest, []byte("v137"), 0o600) }
	r2 := Run(context.Background(), o2)
	if r2.Outcome != OutcomeTaken {
		t.Fatalf("r2 outcome = %q (detail=%s)", r2.Outcome, r2.Detail)
	}

	if _, err := os.Stat(r1.Path); !os.IsNotExist(err) {
		t.Fatalf("the old target's snapshot must be pruned, stat err=%v", err)
	}
}

func TestAlreadyTaken_PlantedFilesDoNotQualify(t *testing.T) {
	const cid, target = "abc1234567890123", "1.37"
	validName := fmt.Sprintf("etcd-snapshot-%s-to-%s-from-x-20200101T000000Z.db", cid, target)

	t.Run("empty file", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, validName)
		mustWriteFile(t, p, "")
		if err := os.Chmod(p, 0o600); err != nil {
			t.Fatal(err)
		}
		if got, ok := alreadyTaken(dir, cid, target, os.Getuid()); ok {
			t.Fatalf("empty file must not qualify, got %q", got)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "real.db")
		mustWriteFile(t, real, "data")
		if err := os.Chmod(real, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(dir, validName)); err != nil {
			t.Fatal(err)
		}
		if got, ok := alreadyTaken(dir, cid, target, os.Getuid()); ok {
			t.Fatalf("a symlink must not qualify, got %q", got)
		}
	})

	t.Run("wrong mode 0644", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, validName)
		mustWriteFile(t, p, "data")
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
		if got, ok := alreadyTaken(dir, cid, target, os.Getuid()); ok {
			t.Fatalf("a 0644 file must not qualify, got %q", got)
		}
	})

	t.Run("wrong owner", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, validName)
		mustWriteFile(t, p, "data")
		if err := os.Chmod(p, 0o600); err != nil {
			t.Fatal(err)
		}
		if got, ok := alreadyTaken(dir, cid, target, os.Getuid()+424242); ok {
			t.Fatalf("a wrong-owner file must not qualify, got %q", got)
		}
	})

	t.Run("valid file qualifies", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, validName)
		mustWriteFile(t, p, "data")
		if err := os.Chmod(p, 0o600); err != nil {
			t.Fatal(err)
		}
		got, ok := alreadyTaken(dir, cid, target, os.Getuid())
		if !ok || got != p {
			t.Fatalf("expected the valid file to qualify: got=%q ok=%v", got, ok)
		}
	})
}

func TestRun_FailedSaveKeepsOldSnapshot(t *testing.T) {
	root := newStackedRoot(t, "cluster-keep-old", 1024)
	dir := filepath.Join(t.TempDir(), "etcd-backup")

	o1, _ := testOptions(t, root)
	o1.dir = dir
	o1.TargetMinor = "1.36"
	o1.save = func(_ context.Context, dest string) error { return os.WriteFile(dest, []byte("old"), 0o600) }
	r1 := Run(context.Background(), o1)
	if r1.Outcome != OutcomeTaken {
		t.Fatalf("r1 outcome = %q (detail=%s)", r1.Outcome, r1.Detail)
	}

	o2, _ := testOptions(t, root)
	o2.dir = dir
	o2.TargetMinor = "1.37" // a different target: not already-taken, save runs and fails
	o2.save = func(_ context.Context, _ string) error { return fmt.Errorf("boom") }
	r2 := Run(context.Background(), o2)
	if r2.Outcome != OutcomeFailed {
		t.Fatalf("r2 outcome = %q, want failed", r2.Outcome)
	}
	if _, err := os.Stat(r1.Path); err != nil {
		t.Fatalf("the old snapshot must survive a failed retake: %v", err)
	}
}

func TestRun_RemovesStalePartBeforeSave(t *testing.T) {
	root := newStackedRoot(t, "cluster-stale-part", 1024)
	o, dir := testOptions(t, root)
	mustMkdirAll(t, dir)
	stale := filepath.Join(dir, "etcd-snapshot-deadbeef00000000-to-1.30-from-x-20200101T000000Z.db.part")
	mustWriteFile(t, stale, "leftover")

	o.save = func(_ context.Context, dest string) error { return os.WriteFile(dest, []byte("ok"), 0o600) }
	res := Run(context.Background(), o)
	if res.Outcome != OutcomeTaken {
		t.Fatalf("outcome = %q (detail=%s)", res.Outcome, res.Detail)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("a stale .part file must be removed before save, stat err=%v", err)
	}
}

// --- 7. Free space ------------------------------------------------------------

func TestRun_InsufficientSpaceSkipsWithoutSave(t *testing.T) {
	root := newStackedRoot(t, "cluster-space", 10*1024*1024) // 10 MiB db
	o, _ := testOptions(t, root)
	o.freeBytes = func(string) (uint64, error) { return 1024, nil } // far below required
	res := Run(context.Background(), o)
	if res.Outcome != OutcomeInsufficientSpace {
		t.Fatalf("outcome = %q, want %q (detail=%s)", res.Outcome, OutcomeInsufficientSpace, res.Detail)
	}
}

func TestRun_MissingDbFails(t *testing.T) {
	root := newStackedRoot(t, "cluster-nodb", 1024)
	if err := os.Remove(filepath.Join(root, "var", "lib", "etcd", "member", "snap", "db")); err != nil {
		t.Fatal(err)
	}
	o, _ := testOptions(t, root)
	res := Run(context.Background(), o)
	if res.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", res.Outcome)
	}
	if !strings.Contains(res.Detail, "cannot size etcd db") {
		t.Fatalf("detail must explain the db-size failure, got %q", res.Detail)
	}
}

// --- 8. Sub-deadline -----------------------------------------------------------

func TestRun_SubDeadlineKillsHangingSave(t *testing.T) {
	old := snapshotTimeout
	snapshotTimeout = 150 * time.Millisecond
	defer func() { snapshotTimeout = old }()

	root := newStackedRoot(t, "cluster-hang-fake", 1024)
	o, _ := testOptions(t, root)
	var partPath string
	o.save = func(ctx context.Context, dest string) error {
		partPath = dest + ".part"
		mustWriteFile(t, partPath, "partial")
		<-ctx.Done()
		return ctx.Err()
	}

	parentCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	res := Run(parentCtx, o)
	elapsed := time.Since(start)

	if res.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", res.Outcome)
	}
	if !strings.Contains(res.Detail, "deadline exceeded") {
		t.Fatalf("detail should wrap the deadline-exceeded error, got %q", res.Detail)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Run took %s, expected it to return promptly after the shortened snapshotTimeout", elapsed)
	}
	if parentCtx.Err() != nil {
		t.Fatal("the parent ctx must still be alive when Run returns")
	}
	if _, err := os.Stat(partPath); !os.IsNotExist(err) {
		t.Fatalf(".part file created by the fake save must be removed, stat err=%v", err)
	}
}
