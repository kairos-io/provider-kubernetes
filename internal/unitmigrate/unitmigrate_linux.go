//go:build linux

package unitmigrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"time"

	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
)

// Package-private seams (mirrors internal/imageimport/bundle_linux.go): rootPath
// is always "/" in production -- there is no runtime override anywhere in the
// exported API; tests point it at a temp directory tree. expectedOwnerUID is
// always 0 (root) in production; tests set it to their own uid, since an
// unprivileged test cannot create root-owned fixtures.
var (
	rootPath         = "/"
	expectedOwnerUID = 0
)

// Test-only seams (security review 2026-09-17, proof-gap closure): both are
// nil in production and run nowhere in the exported API -- only this
// package's own _test.go files (same package, unexported identifiers) can
// assign them, and the default (nil) code path is exactly the one shipped.
// They exist because two of S19-7's branches cannot otherwise be hit
// deterministically without real concurrent processes: a file growing after
// fstat (testHookBeforeRead runs after the fstat(fd) check, before the
// bounded read begins) and the entry changing between the hash and the
// unlink (testHookBeforeUnlink runs after the frozen-hash match, before the
// immediate re-fstatat that guards unlinkat).
var (
	testHookBeforeRead   func(name string)
	testHookBeforeUnlink func(name string)
)

const (
	// maxUnitSize is S19-7's 64 KiB bound: a fragment or drop-in larger than
	// this is refused without ever trusting st_size alone -- the read itself is
	// bounded to maxUnitSize+1 bytes so a file that grows after fstat is caught
	// as oversize rather than silently truncated.
	maxUnitSize = 64 * 1024
	// reloadTimeout bounds the daemon-reload call (S19-10).
	reloadTimeout = 15 * time.Second
	// overrideCap bounds the post-cleanup override report (S19-7: "at most 16 names").
	overrideCap = 16
)

// chainResult classifies the outcome of opening one no-follow directory
// component.
type chainResult int

const (
	chainOK chainResult = iota
	chainAbsent
	chainUnsafe
	chainIOErr
)

// openDirNoFollow opens name under parentFd as a directory, refusing to
// follow a symlink (mirrors internal/imageimport/bundle_linux.go's helper of
// the same name; duplicated here rather than imported because it is
// unexported in that package and this task's scope is internal/unitmigrate
// only). Security review 2026-09-17: duplication ACCEPTED as written (the
// content checks around it are purpose-different -- imageimport binds a
// device anchor and a tarball size band, this package binds uid 0, a unit
// size bound, the frozen hash and dev/ino -- so this is not a weaker second
// copy of the same rule). Condition: keep these flags byte-identical to
// imageimport's copy. R-19-14 records that the two copies can drift apart
// undetected; backlog F-NOFOLLOW extracts a shared helper the next time
// either package is touched for another reason.
func openDirNoFollow(parentFd int, name string) (int, error) {
	return unix.Openat(parentFd, name, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}

// classifyOpenErr maps an openDirNoFollow error to a chainResult.
func classifyOpenErr(err error) chainResult {
	switch {
	case err == nil:
		return chainOK
	case errors.Is(err, unix.ENOENT):
		return chainAbsent
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		return chainUnsafe
	default:
		return chainIOErr
	}
}

// openChain opens each of names in turn under startFd with the no-follow
// directory rules, closing every intermediate fd it opens (but never
// startFd). It returns the final fd (caller must close it) only on chainOK.
func openChain(startFd int, names []string) (fd int, result chainResult) {
	cur := startFd
	opened := false
	for _, name := range names {
		next, err := openDirNoFollow(cur, name)
		if opened {
			_ = unix.Close(cur)
		}
		if err != nil {
			return -1, classifyOpenErr(err)
		}
		cur = next
		opened = true
	}
	return cur, chainOK
}

// entryAction is what one scope-entry check decided.
type entryAction struct {
	removed bool
	// logged is true when a "kept" line was emitted for this entry (a silent
	// symlink mask sets logged=false and removed=false).
	logged bool
	reason Reason
}

// Migrate implements ADR-19 U2 decision 3 / S19-7..S19-10. See the package
// doc comment for the full behavior. It never panics and always returns
// within ctx (#4099-1); on any unexpected condition the file in question is
// left in place.
func Migrate(ctx context.Context, runner kubeadm.Runner) Result {
	if err := ctx.Err(); err != nil {
		logrus.Errorf("unit-migrate: deadline exceeded before the walk started: %v", err)
		res := Result{Outcome: OutcomeFailed}
		logSummary(res)
		return res
	}

	rootFd, err := unix.Open(rootPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		logrus.Errorf("unit-migrate: open %q: %v", rootPath, err)
		res := Result{Outcome: OutcomeFailed}
		logSummary(res)
		return res
	}
	defer func() { _ = unix.Close(rootFd) }()

	systemFd, chainState := openChain(rootFd, []string{"etc", "systemd", "system"})

	var removedFragments, removedDropins, removedLinks, kept int
	var hardFailure bool

	switch chainState {
	case chainAbsent:
		// The entire scope is absent: fresh install, live/recovery boot, or a
		// second run after a prior successful migration. Nothing to do.
	case chainUnsafe:
		kept += keepWholeScope(ReasonWalkUnsafe)
		hardFailure = true
	case chainIOErr:
		kept += keepWholeScope(ReasonIO)
		hardFailure = true
	case chainOK:
		defer func() { _ = unix.Close(systemFd) }()

		for _, name := range fragmentNames {
			act := checkAndRemoveFragment(systemFd, name, fragmentPath(name))
			tally(act, &removedFragments, &kept)
		}

		dRemoved, dKept, dHard := processDropinSubtree(systemFd)
		removedDropins += dRemoved
		kept += dKept
		hardFailure = hardFailure || dHard

		lRemoved, lKept, lHard := processWantsSubtree(systemFd)
		removedLinks += lRemoved
		kept += lKept
		hardFailure = hardFailure || lHard
	}

	removed := removedFragments + removedDropins + removedLinks

	var reloaded bool
	// S19-10: reload only when a fragment or drop-in was removed; removing
	// only .wants links needs none (systemd's in-memory unit definitions are
	// unaffected by a symlink under multi-user.target.wants disappearing).
	if removedFragments+removedDropins > 0 {
		rctx, cancel := context.WithTimeout(ctx, reloadTimeout)
		_, reloadErr := runner.Run(rctx, "daemon-reload")
		cancel()
		if reloadErr != nil {
			logrus.Errorf("unit-migrate: daemon-reload: %v", reloadErr)
			hardFailure = true
		} else {
			reloaded = true
			logrus.Infof("unit-migrate: reloaded")
		}
	}

	overrides := reportOverrides()

	res := Result{
		Outcome:   computeOutcome(removed, kept, hardFailure),
		Removed:   removed,
		Kept:      kept,
		Overrides: overrides,
		Reloaded:  reloaded,
	}
	logSummary(res)
	return res
}

// tally folds one entryAction into the running removed/kept counters.
func tally(act entryAction, removed, kept *int) {
	if act.removed {
		*removed++
		return
	}
	if act.logged {
		*kept++
	}
}

// computeOutcome applies the outcome priority: a hard failure (an unsafe walk
// or an unexpected I/O error, including a failed reload) always reports
// failed, even if some entries were also removed or kept, because "removals
// already done stay done" but the run itself did not complete cleanly.
// Otherwise any kept entry (other than a silent symlink mask, which never
// reaches this counter) means the image unit is shadowed by content this
// project did not ship: kept-modified. Otherwise any removal means migrated;
// none of either means clean.
func computeOutcome(removed, kept int, hardFailure bool) Outcome {
	switch {
	case hardFailure:
		return OutcomeFailed
	case kept > 0:
		return OutcomeKeptModified
	case removed > 0:
		return OutcomeMigrated
	default:
		return OutcomeClean
	}
}

// keepWholeScope logs a "kept" line with reason for all seven scope entries
// (used when a parent directory component that every entry depends on --
// /etc, /etc/systemd or /etc/systemd/system itself -- is unsafe or
// unreachable for an unexpected reason) and returns the count.
func keepWholeScope(reason Reason) int {
	for _, name := range fragmentNames {
		logrus.Warnf("unit-migrate: kept %q reason=%s", fragmentPath(name), reason)
	}
	logrus.Warnf("unit-migrate: kept %q reason=%s", pathKubeadmDropin, reason)
	for _, name := range fragmentNames {
		logrus.Warnf("unit-migrate: kept %q reason=%s", linkPath(name), reason)
	}
	return len(fragmentNames)*2 + 1
}

// processDropinSubtree opens kubelet.service.d under systemFd, checks/removes
// its 10-kubeadm.conf drop-in, and -- only immediately after removing it --
// attempts to rmdir the now-possibly-empty directory (ENOTEMPTY, e.g. an
// operator's own drop-in still present, silently leaves it).
func processDropinSubtree(systemFd int) (removed, kept int, hardFailure bool) {
	dropinDirFd, err := openDirNoFollow(systemFd, nameDropinDir)
	switch classifyOpenErr(err) {
	case chainAbsent:
		return 0, 0, false
	case chainUnsafe:
		logrus.Warnf("unit-migrate: kept %q reason=%s", pathKubeadmDropin, ReasonWalkUnsafe)
		return 0, 1, true
	case chainIOErr:
		logrus.Warnf("unit-migrate: kept %q reason=%s", pathKubeadmDropin, ReasonIO)
		return 0, 1, true
	}
	defer func() { _ = unix.Close(dropinDirFd) }()

	act := checkAndRemoveFragment(dropinDirFd, nameDropin, pathKubeadmDropin)
	if act.removed {
		if rmErr := unix.Unlinkat(systemFd, nameDropinDir, unix.AT_REMOVEDIR); rmErr != nil &&
			!errors.Is(rmErr, unix.ENOTEMPTY) && !errors.Is(rmErr, unix.EEXIST) {
			// Anything other than "not empty" here is unexpected (EBUSY, EPERM,
			// a race); the drop-in is already gone, so this does not undo the
			// removal -- it only means the (now presumably empty, but
			// unverifiable) directory is left behind. Not itself a hard
			// failure: no content was mis-kept or mis-deleted.
			logrus.Debugf("unit-migrate: rmdir %q: %v", pathDropinDir, rmErr)
		}
		return 1, 0, false
	}
	if act.logged {
		return 0, 1, false
	}
	return 0, 0, false
}

// processWantsSubtree opens multi-user.target.wants under systemFd and
// checks/removes each of the three .wants links.
func processWantsSubtree(systemFd int) (removed, kept int, hardFailure bool) {
	wantsDirFd, err := openDirNoFollow(systemFd, nameWantsDir)
	switch classifyOpenErr(err) {
	case chainAbsent:
		return 0, 0, false
	case chainUnsafe:
		for _, name := range fragmentNames {
			logrus.Warnf("unit-migrate: kept %q reason=%s", linkPath(name), ReasonWalkUnsafe)
		}
		return 0, len(fragmentNames), true
	case chainIOErr:
		for _, name := range fragmentNames {
			logrus.Warnf("unit-migrate: kept %q reason=%s", linkPath(name), ReasonIO)
		}
		return 0, len(fragmentNames), true
	}
	defer func() { _ = unix.Close(wantsDirFd) }()

	for _, name := range fragmentNames {
		act := checkAndRemoveLink(wantsDirFd, name)
		tally(act, &removed, &kept)
	}
	return removed, kept, false
}

// checkAndRemoveFragment applies S19-7's fragment/drop-in rules to name under
// parentFd: it is removed only if fstatat(AT_SYMLINK_NOFOLLOW) is a regular
// file, owned by expectedOwnerUID, no larger than maxUnitSize; reopened
// O_RDONLY|O_NOFOLLOW|O_NONBLOCK|O_NOCTTY|O_CLOEXEC; fstat on that fd is
// still a regular file with the same owner; a bounded read's sha256 is in the
// FROZEN set for path; and an immediate re-fstatat before unlinkat still
// shows the same dev/ino. Mode and mtime are never checked (0664 copies exist
// in the field). A symlink at path is left silently (an operator mask); any
// other mismatch is kept and logged with its Reason. Deletion is unlinkat on
// parentFd, never by path.
func checkAndRemoveFragment(parentFd int, name, path string) entryAction {
	var st unix.Stat_t
	if err := unix.Fstatat(parentFd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return entryAction{}
		}
		return keptEntry(path, ReasonIO)
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		// S19-14 / upgrades.md: `systemctl mask` is the supported way to
		// disable one of our units post-U2. Left silently: no log line, not
		// counted as kept, does not affect the outcome.
		return entryAction{}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return keptEntry(path, ReasonNotRegular)
	}
	if int(st.Uid) != expectedOwnerUID {
		return keptEntry(path, ReasonOwner)
	}
	if st.Size > maxUnitSize {
		return keptEntry(path, ReasonSize)
	}

	fd, err := unix.Openat(parentFd, name,
		unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return entryAction{}
		}
		if errors.Is(err, unix.ELOOP) {
			// The fstatat gate just above saw a regular file, but O_NOFOLLOW
			// refused this open: something replaced it with a symlink in the
			// interim.
			return keptEntry(path, ReasonRace)
		}
		return keptEntry(path, ReasonIO)
	}
	defer func() { _ = unix.Close(fd) }()

	var st2 unix.Stat_t
	if fstatErr := unix.Fstat(fd, &st2); fstatErr != nil {
		return keptEntry(path, ReasonIO)
	}
	if st2.Mode&unix.S_IFMT != unix.S_IFREG {
		return keptEntry(path, ReasonNotRegular)
	}
	if int(st2.Uid) != expectedOwnerUID {
		return keptEntry(path, ReasonOwner)
	}

	if testHookBeforeRead != nil {
		testHookBeforeRead(name)
	}

	// Never trust st_size: read at most maxUnitSize+1 bytes and hash exactly
	// those bytes, so a file that grew after fstat is caught as oversize.
	buf := make([]byte, maxUnitSize+1)
	total := 0
	for total < len(buf) {
		n, readErr := unix.Read(fd, buf[total:])
		if readErr != nil {
			if errors.Is(readErr, unix.EINTR) {
				continue
			}
			return keptEntry(path, ReasonIO)
		}
		if n == 0 {
			break
		}
		total += n
	}
	if total > maxUnitSize {
		return keptEntry(path, ReasonSize)
	}

	sum := sha256.Sum256(buf[:total])
	hexSum := hex.EncodeToString(sum[:])
	if !isFrozen(path, hexSum) {
		return keptEntry(path, ReasonModified)
	}

	if testHookBeforeUnlink != nil {
		testHookBeforeUnlink(name)
	}

	// Immediately re-check dev/ino before unlinkat (S19-7): require the path
	// to still be the exact regular file we just hashed. /etc/systemd/system
	// and kubelet.service.d are root-writable only and nothing else rewrites
	// them this early in boot, so a mismatch here is not a mundane I/O error:
	// it is ReasonRace, not ReasonIO (security review 2026-09-17).
	var st3 unix.Stat_t
	if fstatErr := unix.Fstatat(parentFd, name, &st3, unix.AT_SYMLINK_NOFOLLOW); fstatErr != nil {
		return keptEntry(path, reasonForRecheckErr(fstatErr))
	}
	if st3.Mode&unix.S_IFMT != unix.S_IFREG || uint64(st3.Dev) != uint64(st2.Dev) || uint64(st3.Ino) != uint64(st2.Ino) {
		return keptEntry(path, ReasonRace)
	}

	if unlinkErr := unix.Unlinkat(parentFd, name, 0); unlinkErr != nil {
		return keptEntry(path, ReasonIO)
	}
	logRemoved(path, kindFor(name))
	return entryAction{removed: true}
}

// reasonForRecheckErr classifies a failure of the immediate pre-unlink
// re-fstatat: ENOENT means the entry vanished in the race window (ReasonRace);
// anything else is a genuine, mundane I/O error (ReasonIO).
func reasonForRecheckErr(err error) Reason {
	if errors.Is(err, unix.ENOENT) {
		return ReasonRace
	}
	return ReasonIO
}

// kindFor reports the Kind a scope name logs as (KindDropin for the drop-in
// filename, KindFragment otherwise).
func kindFor(name string) Kind {
	if name == nameDropin {
		return KindDropin
	}
	return KindFragment
}

// checkAndRemoveLink applies S19-7's link rule: name under wantsDirFd is
// removed only if lstat says symlink and its target is exactly the fragment
// it is supposed to enable; any other target (or type) is kept and reported.
func checkAndRemoveLink(wantsDirFd int, name string) entryAction {
	path := linkPath(name)
	var st unix.Stat_t
	if err := unix.Fstatat(wantsDirFd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return entryAction{}
		}
		return keptEntry(path, ReasonIO)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFLNK {
		return keptEntry(path, ReasonNotRegular)
	}

	buf := make([]byte, 4096)
	n, err := unix.Readlinkat(wantsDirFd, name, buf)
	if err != nil {
		return keptEntry(path, ReasonIO)
	}
	target := string(buf[:n])
	if target != wantsLinkTarget(name) {
		return keptEntry(path, ReasonLinkTarget)
	}

	// Immediately re-check before unlinkat: still a symlink (same ReasonRace
	// vs ReasonIO split as the fragment path's pre-unlink recheck).
	var st2 unix.Stat_t
	if err := unix.Fstatat(wantsDirFd, name, &st2, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return keptEntry(path, reasonForRecheckErr(err))
	}
	if st2.Mode&unix.S_IFMT != unix.S_IFLNK {
		return keptEntry(path, ReasonRace)
	}

	if err := unix.Unlinkat(wantsDirFd, name, 0); err != nil {
		return keptEntry(path, ReasonIO)
	}
	logRemoved(path, KindLink)
	return entryAction{removed: true}
}

// keptEntry logs the one-line "kept" warning (S19-9: %q path, fixed reason,
// never content/size/hash) and returns the corresponding entryAction.
func keptEntry(path string, reason Reason) entryAction {
	logrus.Warnf("unit-migrate: kept %q reason=%s", path, reason)
	return entryAction{logged: true, reason: reason}
}

// logRemoved logs the one-line "removed" info message (S19-9: %q path only).
func logRemoved(path string, kind Kind) {
	logrus.Infof("unit-migrate: removed %q kind=%s", path, kind)
}

// overrideBases lists every persistent unit search directory outranking or
// beside /etc/systemd/system, other than /usr/lib itself, where a remaining
// override of our three units is reported after cleanup (ADR-19 U2 decision
// 3, "report never act").
var overrideBases = []string{
	filepath.Join("etc", "systemd", "system"),
	filepath.Join("etc", "systemd", "system.control"),
	filepath.Join("etc", "systemd", "system.attached"),
	filepath.Join("run", "systemd", "system"),
	filepath.Join("usr", "local", "lib", "systemd", "system"),
}

// reportOverrides lstats (never opens) the three unit names and their
// `<unit>.d/` drop-in directory under each of overrideBases, logging up to
// overrideCap "override" lines and returning how many were found. It never
// removes or otherwise acts on anything it finds (report only).
func reportOverrides() int {
	count := 0
	for _, base := range overrideBases {
		for _, name := range fragmentNames {
			if count >= overrideCap {
				return count
			}
			if lstatExists(filepath.Join(rootPath, base, name)) {
				logrus.Warnf("unit-migrate: override %q", "/"+filepath.Join(base, name))
				count++
			}
			if count >= overrideCap {
				return count
			}
			dropinName := name + ".d"
			if lstatExistsDir(filepath.Join(rootPath, base, dropinName)) {
				logrus.Warnf("unit-migrate: override %q", "/"+filepath.Join(base, dropinName))
				count++
			}
		}
	}
	return count
}

func lstatExists(path string) bool {
	var st unix.Stat_t
	err := unix.Lstat(path, &st)
	return err == nil
}

func lstatExistsDir(path string) bool {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return false
	}
	return st.Mode&unix.S_IFMT == unix.S_IFDIR
}

// logSummary emits ADR-19 U2's exactly-one, always-last summary line.
func logSummary(res Result) {
	logrus.Infof("unit-migrate: summary outcome=%s removed=%d kept=%d overrides=%d reloaded=%t",
		res.Outcome, res.Removed, res.Kept, res.Overrides, res.Reloaded)
}
