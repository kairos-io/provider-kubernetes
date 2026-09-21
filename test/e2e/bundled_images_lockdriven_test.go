//go:build e2e

package e2e

// bundled_images_lockdriven_test.go is the O-10 e2e gate for ADR-16-A2
// (F-OPTBIND): `import-images` reads only the image-only bundle directory,
// imports only what its images.lock lists, and refuses a tampered entry with the
// documented reason instead of importing it. Each case plants one hostile input
// that one specific importer regression would accept, so every recorded mutation
// fails a case:
//
//	reads /opt again                        -> (2) the /opt canary is used
//	globs *.tar instead of reading the lock -> (3) evil.tar is imported
//	follows symlinks (no O_NOFOLLOW)        -> (4) symlink is imported
//	opens a FIFO blocking (no O_NONBLOCK)   -> (4) fifo does not return in bound
//	no structural check                     -> (4) the wrong RepoTag is imported
//	no filesystem (device) check            -> (5) bind-mounted copies used
//
// It runs in its own node container and never pre-pulls. It changes the bundle
// only inside that container (its writable root filesystem and mount namespace)
// and restores, and re-checks, the bundle after each case, so a case cannot pass
// or fail because of the one before. Every exec is argv-only and bounded.

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kairos-io/provider-kubernetes/internal/hostexec"
)

const (
	// legacyBundleRoot is where images were bundled before ADR-16-A2. /opt is
	// persistent on Kairos, so the image must not ship it and the importer must
	// never read it.
	legacyBundleRoot = "/opt/provider-kubernetes"
	legacyBundleDir  = legacyBundleRoot + "/images"

	// The e2e-only references below reuse the bundled pause image's content under
	// names nothing else uses, so their presence in containerd can only come from
	// the planted tarball.
	optCanaryRef = "registry.k8s.io/pause:e2e-opt-canary"
	evilTarball  = "evil.tar"
	evilRef      = "registry.k8s.io/pause:e2e-evil"
	wrongTagRef  = "registry.k8s.io/pause:e2e-wrong-tag"

	// pristineDir holds known-good copies on the container's root filesystem,
	// which is the provider binary's filesystem, so the symlink case differs from
	// a real entry only in being a symlink.
	pristineDir = "/e2e-bundle-pristine"
	// retagWorkDir is scratch space for building retagged tarballs.
	retagWorkDir = "/tmp/e2e-retag"
	// /tmp is a tmpfs in the node container (startNode), so copies there are on
	// another filesystem than the provider binary.
	tmpfsBundleCopy  = "/tmp/e2e-bundle-copy"
	tmpfsTarballCopy = "/tmp/e2e-tarball-copy.tar"

	// tamperImportTimeout bounds each import-images run in this test. It is well
	// under the importer's own 5 minute deadline, so a run blocked where the
	// deadline cannot reach (a blocking open of a FIFO) fails here, and far above a
	// normal run (seconds: every image is already in containerd).
	tamperImportTimeout = 2 * time.Minute
	// bundleCopyTimeout bounds copying the bundle (all tarballs) into the tmpfs.
	bundleCopyTimeout = 5 * time.Minute
)

// TestBundledImagesLockDrivenImport (O-10) proves that the import reads only
// images.lock from hostexec.BundleDir and refuses tampered entries.
func TestBundledImagesLockDrivenImport(t *testing.T) {
	nc := startNode(t, uniqueName("lockimport"))
	const label = "ADR-16-A2"

	// (1) The image ships nothing at the legacy path, before anything is planted.
	assertPathAbsent(t, nc, legacyBundleRoot, "(1) the image must not ship the pre-ADR-16-A2 bundle location")

	// The node image's bundle as the importer walks it: every directory from / to
	// the bundle and to the provider binary root-owned without group/other write,
	// the provider binary likewise, and images.lock plus every tarball root:root
	// 0644 regular files as the image build sets them.
	lock := readBundleLock(t, nc, label)
	assertBundleWalk(t, nc, lock)

	// Let the boot-time import finish before changing anything, so no case races
	// it, and require that it imported every entry.
	props := waitImportUnitSettled(t, nc)
	if v := importUnitViolations(props); len(v) > 0 {
		t.Fatalf("%s: %s did not complete successfully: %s (properties %v)", label, imageImportUnit, strings.Join(v, "; "), props)
	}
	boot := waitImportJournalSummary(t, nc, label+" boot import")
	if v := importRunViolations(importOutput{Summary: boot}, lock, wantImport{Outcome: "success", SummaryOnly: true}); len(v) > 0 {
		t.Fatalf("%s: the boot-time import did not import every images.lock entry: %s (summary %s)", label, strings.Join(v, "; "), boot)
	}
	e2eRefs := []string{optCanaryRef, evilRef, wrongTagRef}
	assertCtrLacks(t, nc, label+" before any case (otherwise the absence checks are vacuous)", e2eRefs...)

	pause := pauseLockEntry(t, lock)
	pausePath := hostexec.BundleDir + "/" + pause.Tarball
	anchorDev := statDevice(t, nc, providerBinaryPath)
	pauseSHA := sha256InContainer(t, nc, pausePath)

	// Pristine copies on the provider binary's filesystem: the pause tarball, and
	// the same tarball with a RepoTag that is not its lock ref.
	if _, err := nc.execErr(binTest, "-e", pristineDir); err == nil {
		t.Fatalf("%s: %s already exists in the node image", label, pristineDir)
	}
	t.Cleanup(func() { _, _ = nc.execErr(binRm, "-rf", pristineDir) })
	execBounded(t, nc, dockerTimeout, binMkdir, "-p", pristineDir)
	pristinePause := pristineDir + "/" + pause.Tarball
	execBounded(t, nc, bundleCopyTimeout, binCp, "-a", pausePath, pristinePause)
	if dev := statDevice(t, nc, pristinePause); dev != anchorDev {
		t.Fatalf("%s: %s is on device %d, the provider binary on %d; the symlink case needs a same-filesystem target", label, pristineDir, dev, anchorDev)
	}
	wrongTagTar := pristineDir + "/wrong-tag.tar"
	buildRetaggedTarball(t, nc, pristinePause, wrongTagTar, pause.Ref, wrongTagRef)

	// (2) A VALID bundle under /opt: a lock in the exact build layout listing one
	// tarball, root-owned 0644 on the provider binary's filesystem, named
	// optCanaryRef. An importer that read /opt (the old default) would accept it;
	// the real one must import the image's own lock entries and never mention it.
	t.Run("opt canary ignored", func(t *testing.T) {
		t.Cleanup(func() { _, _ = nc.execErr(binRm, "-rf", legacyBundleRoot) })
		canaryTarball := tarballNameForRef(optCanaryRef)
		execBounded(t, nc, dockerTimeout, binMkdir, "-p", legacyBundleDir)
		buildRetaggedTarball(t, nc, pristinePause, legacyBundleDir+"/"+canaryTarball, pause.Ref, optCanaryRef)
		canaryLock, err := renderImagesLock(imagesLock{
			KubernetesVersion: lock.KubernetesVersion,
			ImageRepository:   lock.ImageRepository,
			VerifiedBy:        lock.VerifiedBy,
			Images:            []imagesLockItem{{Ref: optCanaryRef, Digest: pause.Digest, Tarball: canaryTarball, Verified: true}},
		})
		if err != nil {
			t.Fatalf("render the /opt canary lock: %v", err)
		}
		nc.WriteFile(t, legacyBundleDir+"/images.lock", canaryLock, "0644")
		// Prove the planted bundle would pass the file checks if it were read, so
		// the case cannot pass merely because the canary is malformed.
		for _, dir := range walkDirs(legacyBundleDir) {
			assertBundlePath(t, nc, dir, "directory", false)
		}
		for _, f := range []string{legacyBundleDir + "/images.lock", legacyBundleDir + "/" + canaryTarball} {
			assertBundlePath(t, nc, f, "regular file", true)
			if dev := statDevice(t, nc, f); dev != anchorDev {
				t.Fatalf("planted %s is on device %d, the provider binary on %d", f, dev, anchorDev)
			}
		}

		run := runImportImages(t, nc, "import-images with a valid bundle under /opt", tamperImportTimeout, execOptions{})
		v := importRunViolations(run.importOutput, lock, wantImport{Outcome: "success"})
		if strings.Contains(run.Out, optCanaryRef) || strings.Contains(run.Out, canaryTarball) {
			v = append(v, "the output mentions the /opt canary")
		}
		if run.Code != 0 || len(v) > 0 {
			t.Errorf("exit %d, want 0 with exactly the %d entries of %s imported: %s\n%s",
				run.Code, len(lock.Images), imagesLockPath, strings.Join(v, "; "), trimForLog(run.Out))
		}
		absent := assertCtrLacks(t, nc, "after the /opt canary run", optCanaryRef)
		t.Logf("exit %d, %s; %s absent from containerd: %t", run.Code, run.Summary, optCanaryRef, absent)
	})
	assertPathAbsentAfterCleanup(t, nc, legacyBundleRoot)

	// (3) A valid docker-save tarball in the bundle directory that images.lock does
	// not list. A *.tar glob would import it; the importer must only report it as
	// unlisted, without opening it (so it cannot know its ref), and import nothing
	// else than the lock entries.
	t.Run("unlisted tarball not imported", func(t *testing.T) {
		evilPath := hostexec.BundleDir + "/" + evilTarball
		t.Cleanup(func() { _, _ = nc.execErr(binRm, "-f", evilPath) })
		buildRetaggedTarball(t, nc, pristinePause, evilPath, pause.Ref, evilRef)
		assertBundlePath(t, nc, evilPath, "regular file", true)

		run := runImportImages(t, nc, "import-images with an unlisted evil.tar", tamperImportTimeout, execOptions{})
		v := importRunViolations(run.importOutput, lock, wantImport{Outcome: "success", Unlisted: 1})
		if unlisted := importUnlistedLines(run.Out); len(unlisted) != 1 || !strings.Contains(unlisted[0], evilTarball) {
			v = append(v, fmt.Sprintf("unlisted lines %q, want exactly one naming %s", unlisted, evilTarball))
		}
		if strings.Contains(run.Out, evilRef) {
			v = append(v, "the output mentions "+evilRef+", so the unlisted file was read")
		}
		if run.Code != 0 || len(v) > 0 {
			t.Errorf("exit %d, want 0: %s\n%s", run.Code, strings.Join(v, "; "), trimForLog(run.Out))
		}
		absent := assertCtrLacks(t, nc, "after the unlisted evil.tar run", evilRef)
		t.Logf("exit %d, %s; unlisted %q; %s absent from containerd: %t", run.Code, run.Summary, importUnlistedLines(run.Out), evilRef, absent)
	})
	assertPathAbsentAfterCleanup(t, nc, hostexec.BundleDir+"/"+evilTarball)

	// (4) One tampered entry at a time, in the container's writable root
	// filesystem. Every other entry must still import (per-entry refusal), the
	// run exits 1, and the pause tarball is restored and re-checked afterwards.
	tamperCases := []struct {
		name   string
		reason string
		// why this case exists
		why    string
		tamper func(t *testing.T)
		// took proves the tamper is in place (lstat of the entry), so the case
		// cannot pass vacuously.
		took func(st fileStat) bool
	}{
		{
			name: "group-writable", reason: "mode",
			why:    "a group- or other-writable tarball can be replaced without root",
			tamper: func(t *testing.T) { execBounded(t, nc, dockerTimeout, binChmod, "g+w", pausePath) },
			took:   func(st fileStat) bool { return st.Type == "regular file" && st.Mode&modeGroupWrite != 0 },
		},
		{
			name: "not root-owned", reason: "owner",
			why:    "a tarball owned by another user can be replaced by that user",
			tamper: func(t *testing.T) { execBounded(t, nc, dockerTimeout, binChown, "1000", pausePath) },
			took:   func(st fileStat) bool { return st.Type == "regular file" && st.UID == 1000 },
		},
		{
			name: "symlink", reason: "symlink",
			why: "a symlink can point anywhere; here at a valid copy on the same filesystem, so only " +
				"refusing to follow it (O_NOFOLLOW) stops the import",
			tamper: func(t *testing.T) {
				execBounded(t, nc, dockerTimeout, binRm, "-f", pausePath)
				execBounded(t, nc, dockerTimeout, binLn, "-s", pristinePause, pausePath)
			},
			took: func(st fileStat) bool { return st.Type == "symbolic link" },
		},
		{
			name: "wrong RepoTag", reason: "manifest",
			why: "a well-formed, root-owned tarball whose manifest names another ref; only the structural " +
				"check against the lock ref stops ctr from naming the image " + wrongTagRef,
			tamper: func(t *testing.T) {
				execBounded(t, nc, dockerTimeout, binRm, "-f", pausePath)
				execBounded(t, nc, bundleCopyTimeout, binCp, "-a", wrongTagTar, pausePath)
			},
			took: func(st fileStat) bool { return st.Type == "regular file" && st.UID == 0 && st.Mode == 0o644 },
		},
		// Keep FIFO last: if it hangs, the loop stops the test (see below).
		{
			name: "FIFO", reason: "not-regular",
			why: "opening a FIFO without O_NONBLOCK blocks until a writer appears, which would hang the " +
				"boot import; the run must return within its bound",
			tamper: func(t *testing.T) {
				execBounded(t, nc, dockerTimeout, binRm, "-f", pausePath)
				execBounded(t, nc, dockerTimeout, binMkfifo, "-m", "0644", pausePath)
			},
			took: func(st fileStat) bool { return st.Type == "fifo" },
		},
	}
	for _, tc := range tamperCases {
		// returned stays false only if runImportImages stopped the case because the
		// run did not finish within its bound (t.Fatalf skips the assignment).
		returned := false
		ok := t.Run("refused "+tc.name, func(t *testing.T) {
			t.Logf("why: %s", tc.why)
			tc.tamper(t)
			st, err := nc.lstat(pausePath)
			if err != nil || !tc.took(st) {
				returned = true
				t.Fatalf("tamper did not take effect on %s: %+v (%v)", pausePath, st, err)
			}
			run := runImportImages(t, nc, "import-images with a "+tc.name+" tarball", tamperImportTimeout, execOptions{})
			returned = true
			v := importRunViolations(run.importOutput, lock, wantImport{Outcome: "partial", Refused: map[string]string{pause.Tarball: tc.reason}})
			if run.Code != 1 || len(v) > 0 {
				t.Errorf("exit %d, want 1 with %s refused (reason=%s) and every other entry imported: %s\n%s",
					run.Code, pause.Tarball, tc.reason, strings.Join(v, "; "), trimForLog(run.Out))
			}
			if tc.reason == "manifest" {
				// Only this case plants a tarball carrying wrongTagRef; checking it in the
				// other cases would blame them for this one (the final check covers all).
				assertCtrLacks(t, nc, "after the "+tc.name+" run", wrongTagRef)
			}
			t.Logf("exit %d in %s, %s; entry lines: %s", run.Code, run.Elapsed.Round(time.Second), run.Summary, formatImportEntries(run.Entries, pause.Tarball))
		})
		restoreTarball(t, nc, pausePath, pristinePause, pauseSHA, anchorDev)
		switch {
		case !ok && !returned:
			// The run was abandoned at its bound. An importer blocked opening the FIFO
			// stays blocked in the container, so nothing later here is trustworthy.
			t.Fatalf("case %q did not return within %s; stopping: the importer may still be blocked in the container", tc.name, tamperImportTimeout)
		case !ok:
			t.Logf("case %q failed; the bundle was restored and re-checked, continuing", tc.name)
		}
	}

	// (5) The same bytes on another filesystem, bind-mounted over the bundle:
	// what a persistent CUSTOM_BIND_MOUNTS entry or a later mount would do. Every
	// file must be on the provider binary's filesystem (directories are not
	// compared). Privileged node container; mounts live only in its mount
	// namespace and are removed after each case.
	//
	// Whole directory: images.lock is the first file checked and is itself on the
	// tmpfs, so the importer refuses the WHOLE bundle with reason=device before
	// looking at any entry (one "bundle refused" line, all counts 0).
	t.Run("refused bind-mounted bundle directory", func(t *testing.T) {
		t.Cleanup(func() {
			_, _ = nc.execErr(binUmount, hostexec.BundleDir)
			_, _ = nc.ExecTimeout(bundleCopyTimeout, binRm, "-rf", tmpfsBundleCopy)
		})
		execBounded(t, nc, bundleCopyTimeout, binCp, "-a", hostexec.BundleDir, tmpfsBundleCopy)
		execBounded(t, nc, dockerTimeout, binMount, "--bind", tmpfsBundleCopy, hostexec.BundleDir)
		for _, f := range []string{imagesLockPath, pausePath} {
			if dev := statDevice(t, nc, f); dev == anchorDev {
				t.Fatalf("after the bind mount %s is still on the provider binary's device %d; the case would be vacuous", f, dev)
			}
		}
		// Same owner, mode and bytes as the real bundle, so only the filesystem differs.
		assertBundleWalk(t, nc, lock)
		run := runImportImages(t, nc, "import-images with a tmpfs copy bound over the bundle directory", tamperImportTimeout, execOptions{})
		v := importRunViolations(run.importOutput, lock, wantImport{Outcome: "refused", BundleRefused: "device"})
		if run.Code != 1 || len(v) > 0 {
			t.Errorf("exit %d, want 1 with the whole bundle refused (reason=device): %s\n%s", run.Code, strings.Join(v, "; "), trimForLog(run.Out))
		}
		t.Logf("exit %d, bundle refused reason=%s, %s", run.Code, run.BundleRefusal, run.Summary)
		execBounded(t, nc, dockerTimeout, binUmount, hostexec.BundleDir)
	})
	assertBundleOnAnchorDevice(t, nc, lock, anchorDev)

	// One file: only that entry is on the tmpfs, so only it is refused
	// (reason=device) and every other entry imports.
	t.Run("refused bind-mounted tarball", func(t *testing.T) {
		t.Cleanup(func() {
			_, _ = nc.execErr(binUmount, pausePath)
			_, _ = nc.execErr(binRm, "-f", tmpfsTarballCopy)
		})
		execBounded(t, nc, bundleCopyTimeout, binCp, "-a", pausePath, tmpfsTarballCopy)
		execBounded(t, nc, dockerTimeout, binMount, "--bind", tmpfsTarballCopy, pausePath)
		if dev := statDevice(t, nc, pausePath); dev == anchorDev {
			t.Fatalf("after the bind mount %s is still on the provider binary's device %d; the case would be vacuous", pausePath, dev)
		}
		run := runImportImages(t, nc, "import-images with a tmpfs copy bound over one tarball", tamperImportTimeout, execOptions{})
		v := importRunViolations(run.importOutput, lock, wantImport{Outcome: "partial", Refused: map[string]string{pause.Tarball: "device"}})
		if run.Code != 1 || len(v) > 0 {
			t.Errorf("exit %d, want 1 with only %s refused (reason=device): %s\n%s", run.Code, pause.Tarball, strings.Join(v, "; "), trimForLog(run.Out))
		}
		t.Logf("exit %d, %s; entry lines: %s", run.Code, run.Summary, formatImportEntries(run.Entries, pause.Tarball))
		execBounded(t, nc, dockerTimeout, binUmount, pausePath)
	})
	restoreTarball(t, nc, pausePath, pristinePause, pauseSHA, anchorDev)
	assertBundleOnAnchorDevice(t, nc, lock, anchorDev)

	// Finally the untouched bundle imports cleanly again, and no e2e-only name ever
	// reached containerd.
	final := runImportImages(t, nc, label+" final import-images", tamperImportTimeout, execOptions{})
	if v := importRunViolations(final.importOutput, lock, wantImport{Outcome: "success"}); final.Code != 0 || len(v) > 0 {
		t.Errorf("%s: after the cases import-images exited %d, want 0: %s\n%s", label, final.Code, strings.Join(v, "; "), trimForLog(final.Out))
	}
	assertCtrLacks(t, nc, label+" at the end", e2eRefs...)
	t.Logf("%s: final import-images exit %d, %s", label, final.Code, final.Summary)
}

// assertPathAbsent requires that p does not exist in the container (not as a
// file, directory or dangling symlink). test exits 1 for "absent"; any other
// status is an error, not an answer.
func assertPathAbsent(t *testing.T, nc *nodeContainer, p, why string) {
	t.Helper()
	out, err := nc.execErr(binTest, "-e", p, "-o", "-L", p)
	switch code := exitCode(err); code {
	case 1:
		t.Logf("%s does not exist (%s)", p, why)
	case 0:
		t.Fatalf("%s exists: %s", p, why)
	default:
		t.Fatalf("test -e %s: exit %d (%v)\n%s", p, code, err, out)
	}
}

// assertPathAbsentAfterCleanup requires that a case's cleanup removed p, so the
// next case starts from the shipped bundle.
func assertPathAbsentAfterCleanup(t *testing.T, nc *nodeContainer, p string) {
	t.Helper()
	assertPathAbsent(t, nc, p, "removed after its case")
}

// assertBundlePath requires p to satisfy the importer's rule for its type
// (bundlePathViolations); exact additionally requires the root:root 0644 the image
// build gives bundle files.
func assertBundlePath(t *testing.T, nc *nodeContainer, p, wantType string, exact bool) {
	t.Helper()
	st, err := nc.lstat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	v := bundlePathViolations(st, wantType)
	if exact && (st.GID != 0 || st.Mode != 0o644) {
		v = append(v, fmt.Sprintf("group %d mode %04o, want 0 and 0644", st.GID, st.Mode))
	}
	if len(v) > 0 {
		t.Fatalf("%s: %s", p, strings.Join(v, "; "))
	}
}

// assertBundleWalk checks the node image's bundle as the importer walks it.
func assertBundleWalk(t *testing.T, nc *nodeContainer, lock imagesLock) {
	t.Helper()
	for _, dir := range walkDirs(hostexec.BundleDir, path.Dir(hostexec.ProviderBinaryPath)) {
		assertBundlePath(t, nc, dir, "directory", false)
	}
	assertBundlePath(t, nc, hostexec.ProviderBinaryPath, "regular file", false)
	assertBundlePath(t, nc, imagesLockPath, "regular file", true)
	for _, tarball := range lock.tarballs() {
		assertBundlePath(t, nc, hostexec.BundleDir+"/"+tarball, "regular file", true)
	}
	t.Logf("bundle walk %v, %s, images.lock and %d tarballs: root-owned, no group/other write",
		walkDirs(hostexec.BundleDir, path.Dir(hostexec.ProviderBinaryPath)), hostexec.ProviderBinaryPath, len(lock.Images))
}

// assertBundleOnAnchorDevice requires images.lock and every tarball to be back on
// the provider binary's filesystem (no mount left over from a case).
func assertBundleOnAnchorDevice(t *testing.T, nc *nodeContainer, lock imagesLock, anchorDev uint64) {
	t.Helper()
	files := []string{imagesLockPath}
	for _, tarball := range lock.tarballs() {
		files = append(files, hostexec.BundleDir+"/"+tarball)
	}
	for _, f := range files {
		if dev := statDevice(t, nc, f); dev != anchorDev {
			t.Fatalf("%s is on device %d after a case, want the provider binary's %d (a mount was left behind)", f, dev, anchorDev)
		}
	}
}

// assertCtrLacks requires none of refs to be an image name in containerd's
// k8s.io namespace, and reports whether that held.
func assertCtrLacks(t *testing.T, nc *nodeContainer, label string, refs ...string) bool {
	t.Helper()
	names := ctrImageNames(t, nc, label)
	ok := true
	for _, ref := range refs {
		if containsString(names, ref) {
			ok = false
			t.Errorf("%s: containerd k8s.io has %s, which only a planted tarball carries", label, ref)
		}
	}
	return ok
}

// formatImportEntries renders the per-entry lines for the tampered tarball, and a
// count of the others, for a case's log line.
func formatImportEntries(entries []importEntry, tarball string) string {
	var parts []string
	others := 0
	for _, e := range entries {
		if e.Tarball != tarball {
			others++
			continue
		}
		s := e.Verb + " " + e.Tarball + " ref=" + e.Ref
		if e.Reason != "" {
			s += " reason=" + e.Reason
		}
		parts = append(parts, s)
	}
	return fmt.Sprintf("[%s] + %d other entry line(s)", strings.Join(parts, "; "), others)
}

// pauseLockEntry returns the bundle's pause entry: the smallest image, used as the
// victim of every tamper case.
func pauseLockEntry(t *testing.T, lock imagesLock) imagesLockItem {
	t.Helper()
	var found []imagesLockItem
	for _, img := range lock.Images {
		if strings.Contains(img.Ref, "/pause:") {
			found = append(found, img)
		}
	}
	if len(found) != 1 {
		t.Fatalf("images.lock has %d pause entries, want 1", len(found))
	}
	return found[0]
}

// execBounded runs argv in the container within timeout and stops the test on
// failure.
func execBounded(t *testing.T, nc *nodeContainer, timeout time.Duration, args ...string) {
	t.Helper()
	if out, err := nc.ExecTimeout(timeout, args...); err != nil {
		t.Fatalf("exec %s: %v\n%s", strings.Join(args, " "), err, trimForLog(out))
	}
}

// statDevice returns the st_dev of p itself (stat without -L).
func statDevice(t *testing.T, nc *nodeContainer, p string) uint64 {
	t.Helper()
	out, stderr, err := nc.ExecStdoutTimeout(dockerTimeout, binStat, "-c", "%d", p)
	if err != nil {
		t.Fatalf("stat -c %%d %s: %v\n%s", p, err, stderr)
	}
	dev, err := strconv.ParseUint(strings.TrimSpace(out), 10, 64)
	if err != nil {
		t.Fatalf("stat -c %%d %s: %q: %v", p, out, err)
	}
	return dev
}

// sha256InContainer returns the sha256 of the file at p.
func sha256InContainer(t *testing.T, nc *nodeContainer, p string) string {
	t.Helper()
	out, stderr, err := nc.ExecStdoutTimeout(tarballReadTimeout, binSha256sum, p)
	if err != nil {
		t.Fatalf("sha256sum %s: %v\n%s", p, err, stderr)
	}
	sum, _, _ := strings.Cut(strings.TrimSpace(out), " ")
	if !sha256HexRE.MatchString(sum) {
		t.Fatalf("sha256sum %s: unexpected output %q", p, out)
	}
	return sum
}

// restoreTarball puts the pristine copy back at p (whatever a case left there),
// then proves it: a root:root 0644 regular file with the original sha256 on the
// provider binary's filesystem.
func restoreTarball(t *testing.T, nc *nodeContainer, p, pristine, wantSHA string, anchorDev uint64) {
	t.Helper()
	execBounded(t, nc, dockerTimeout, binRm, "-f", p)
	execBounded(t, nc, bundleCopyTimeout, binCp, "-a", pristine, p)
	assertBundlePath(t, nc, p, "regular file", true)
	if got := sha256InContainer(t, nc, p); got != wantSHA {
		t.Fatalf("restored %s has sha256 %s, want %s", p, got, wantSHA)
	}
	if dev := statDevice(t, nc, p); dev != anchorDev {
		t.Fatalf("restored %s is on device %d, want %d", p, dev, anchorDev)
	}
}

// buildRetaggedTarball writes dst in the container: the members of src in src's
// order, with only manifest.json's RepoTags changed from oldRef to newRef, the
// way bundle-images.sh names a tarball. The result is a structurally valid bundle
// tarball for newRef (same config and layers), root:root 0644.
func buildRetaggedTarball(t *testing.T, nc *nodeContainer, src, dst, oldRef, newRef string) {
	t.Helper()
	listing, stderr, err := nc.ExecStdoutTimeout(tarballReadTimeout, binTar, "-tf", src)
	if err != nil || strings.TrimSpace(stderr) != "" {
		t.Fatalf("tar -tf %s: %v\n%s", src, err, stderr)
	}
	names, err := parseTarMemberNames(listing)
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	execBounded(t, nc, dockerTimeout, binRm, "-rf", retagWorkDir)
	defer func() { _, _ = nc.execErr(binRm, "-rf", retagWorkDir) }()
	execBounded(t, nc, dockerTimeout, binMkdir, "-p", retagWorkDir)
	execBounded(t, nc, tarballReadTimeout, binTar, "-xf", src, "-C", retagWorkDir)
	manifest, stderr, err := nc.ExecStdoutTimeout(dockerTimeout, binCat, retagWorkDir+"/manifest.json")
	if err != nil {
		t.Fatalf("read manifest.json of %s: %v\n%s", src, err, stderr)
	}
	retagged, err := retagDockerSaveManifest(manifest, oldRef, newRef)
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	nc.WriteFile(t, retagWorkDir+"/manifest.json", retagged, "0644")
	execBounded(t, nc, tarballReadTimeout, append([]string{binTar, "-cf", dst, "-C", retagWorkDir}, names...)...)
	execBounded(t, nc, dockerTimeout, binChmod, "0644", dst)
	t.Logf("built %s from %s with RepoTags [%s]", dst, src, newRef)
}
