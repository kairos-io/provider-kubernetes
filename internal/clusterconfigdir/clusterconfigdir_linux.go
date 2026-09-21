//go:build linux

package clusterconfigdir

import (
	"errors"

	"golang.org/x/sys/unix"
)

// rootPath and expectedOwnerUID are package-private seams (mirrors
// internal/imageimport/bundle_linux.go and
// internal/unitmigrate/unitmigrate_linux.go): production always walks from
// "/" and requires uid 0 -- neither has a runtime override anywhere in the
// exported API. Tests point rootPath at a temp directory tree standing in
// for "/" and set expectedOwnerUID to the test's own uid, since an
// unprivileged test cannot create root-owned fixtures.
var (
	rootPath         = "/"
	expectedOwnerUID = 0
)

// notPersistentOverride is S-D3-8's test seam, in the same style as
// bundle_linux.go's readOnlyOverride (package-private, nil in production,
// never settable from the exported API): it lets tests simulate "/usr/local"
// NOT being a separate mount without needing a real second filesystem.
// Production leaves this nil and falls through to a real st_dev comparison
// of the two already-open fds.
var notPersistentOverride func(rootFd, localFd int) bool

// openDirNoFollow opens name under parentFd as a directory, refusing to
// follow a symlink. Mirrors internal/imageimport/bundle_linux.go's helper of
// the same name (and internal/unitmigrate/unitmigrate_linux.go's copy of
// it); duplicated here rather than imported because it is unexported in both
// packages and this task's scope is internal/clusterconfigdir only. R-19-14
// (PROJECT_CONTEXT.md) records that the three copies can drift apart
// undetected; the condition for accepting the duplication is that the flags
// stay byte-identical: O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC, nothing else.
func openDirNoFollow(parentFd int, name string) (int, error) {
	return unix.Openat(parentFd, name, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}

// ensureDir implements S-D3-2/S-D3-3/S-D3-4/S-D3-8: an openat walk
// "/" -> "usr" -> "local" with O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC (refusing any
// ancestor that is missing, a symlink, not a directory, or not owned by
// uid 0 -- never os.MkdirAll, which follows symlinks and creates ancestors),
// then a single mkdirat(fd, "cloud-config", 0o700) -- only the final
// component is ever created. fileName is the token-file basename to
// preflight (S-D3-4): DefaultFileName unless the operator's
// ClusterConfigPath override names a different file directly under
// DefaultDir (targetFileName in clusterconfigdir.go already enforced that).
//
// S-D3-5a (security review amendment, 2026-09-21): an ancestor that is a
// symlink, not a directory, or not owned by expectedOwnerUID withholds
// (ReasonAncestorUnsafe) -- our walk verified nothing about where that
// component actually leads, and the SDK's own open at plugin.go:59 resolves
// the full path by name straight through whatever we just refused to
// follow, so handing it the real token there is exactly backwards. A
// missing ancestor (ENOENT) does NOT withhold (ReasonAncestorMissing): that
// is not a safety refusal, the SDK's open then fails the identical ENOENT,
// and no token goes anywhere either way, so withholding would only replace
// one legible error with a second one.
func ensureDir(fileName string) Report {
	rootFd, err := unix.Open(rootPath, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return ancestorErrReport(err)
	}
	defer func() { _ = unix.Close(rootFd) }()

	usrFd, err := openDirNoFollow(rootFd, "usr")
	if err != nil {
		return ancestorErrReport(err)
	}
	defer func() { _ = unix.Close(usrFd) }()
	if !rootOwnedDir(usrFd) {
		return Report{Reason: ReasonAncestorUnsafe, Withhold: true}
	}

	localFd, err := openDirNoFollow(usrFd, "local")
	if err != nil {
		return ancestorErrReport(err)
	}
	defer func() { _ = unix.Close(localFd) }()
	if !rootOwnedDir(localFd) {
		return Report{Reason: ReasonAncestorUnsafe, Withhold: true}
	}

	// S-D3-8: compare BEFORE creating anything, on the fds we already have.
	notPersistent := checkNotPersistent(rootFd, localFd)

	dirReason, withhold := createOrCheckCloudConfigDir(localFd)
	if withhold {
		return Report{Reason: dirReason, Withhold: true}
	}
	if dirReason == ReasonDirCreateFailed {
		return withNotPersistent(Report{Reason: dirReason}, notPersistent)
	}

	// dirReason is now "" (freshly created, or an existing clean directory)
	// or ReasonDirWritable (existing, root-owned, but group/other-writable --
	// reported, never refused). Either way the directory is usable: preflight
	// the token file inside it (S-D3-4).
	dirFd, err := openDirNoFollow(localFd, "cloud-config")
	if err != nil {
		// Vanished or changed type between mkdirat/fstatat and this open: we
		// cannot safely proceed to the token-file preflight, but this is not
		// one of S-D3-3's EEXIST-and-unsafe cases (that already returned
		// above) -- treat as a create failure, not a withhold.
		return withNotPersistent(Report{Reason: ReasonDirCreateFailed}, notPersistent)
	}
	defer func() { _ = unix.Close(dirFd) }()

	if tokenReason := checkTokenFile(dirFd, fileName); tokenReason != ReasonNone {
		return Report{Reason: tokenReason, Withhold: true}
	}

	return withNotPersistent(Report{Reason: dirReason}, notPersistent)
}

// ancestorErrReport classifies a failure to open an ancestor directory
// component (S-D3-2) into the correct Reason and withhold decision
// (S-D3-5a): ENOENT means the ancestor is simply absent, which is reported
// only; any other error (ELOOP: a symlink; ENOTDIR: a non-directory
// component; or any other unexpected errno) means the walk could not verify
// where that component leads, which withholds.
func ancestorErrReport(err error) Report {
	if errors.Is(err, unix.ENOENT) {
		return Report{Reason: ReasonAncestorMissing}
	}
	return Report{Reason: ReasonAncestorUnsafe, Withhold: true}
}

// withNotPersistent folds S-D3-8's report-only finding into rep, but only
// when nothing more severe already claimed the single Reason slot.
func withNotPersistent(rep Report, notPersistent bool) Report {
	if rep.Reason == ReasonNone && notPersistent {
		rep.Reason = ReasonNotPersistent
	}
	return rep
}

// rootOwnedDir applies the S-D3-2 ancestor rule to an already no-follow-opened
// directory fd: S_ISDIR (guaranteed by O_DIRECTORY, checked again
// defensively) and owned by expectedOwnerUID (production: uid 0).
func rootOwnedDir(fd int) bool {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return false
	}
	return st.Mode&unix.S_IFMT == unix.S_IFDIR && int(st.Uid) == expectedOwnerUID
}

// createOrCheckCloudConfigDir implements S-D3-2's mkdirat and S-D3-3's
// EEXIST rules. It returns (Reason, withhold): withhold is true only for the
// EEXIST-and-unsafe case, because S-D3-5 gates withholding on S-D3-3
// exactly. It never chmods, chowns or unlinks anything that already exists.
func createOrCheckCloudConfigDir(localFd int) (Reason, bool) {
	err := unix.Mkdirat(localFd, "cloud-config", 0o700)
	if err == nil {
		return ReasonNone, false
	}
	if !errors.Is(err, unix.EEXIST) {
		return ReasonDirCreateFailed, false
	}

	var st unix.Stat_t
	if statErr := unix.Fstatat(localFd, "cloud-config", &st, unix.AT_SYMLINK_NOFOLLOW); statErr != nil {
		return ReasonDirCreateFailed, false
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		// A symlink, FIFO, device or regular file at the target path.
		return ReasonDirUnsafe, true
	}
	if int(st.Uid) != expectedOwnerUID {
		return ReasonDirUnsafe, true
	}
	if st.Mode&0o022 != 0 {
		// Root-owned directory, but group- or other-writable: report, do not
		// refuse (S-D3-3 explicit rule).
		return ReasonDirWritable, false
	}
	return ReasonNone, false
}

// checkTokenFile implements S-D3-4, inverted to an ALLOWLIST per the
// security review's S-D3-4a amendment (2026-09-21): safe = ENOENT (the SDK
// will O_CREATE it fresh at 0600), or a regular file owned by
// expectedOwnerUID with mode&0o177==0 and nlink==1 (a previously-written,
// still-intact 0600 file). Everything else is unsafe and withholds: a
// symlink, FIFO, device, socket, directory, any type invented later, and any
// unexpected fstatat error (safety cannot be established, so fail closed).
// An enumeration of bad types is a standing invitation to be wrong about a
// type nobody has thought of yet; an allowlist of the one shape known safe
// is not.
func checkTokenFile(dirFd int, name string) Reason {
	var st unix.Stat_t
	err := unix.Fstatat(dirFd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return ReasonNone
		}
		return ReasonTokenFileUnsafe
	}
	if st.Mode&unix.S_IFMT == unix.S_IFREG &&
		int(st.Uid) == expectedOwnerUID &&
		st.Mode&0o177 == 0 &&
		st.Nlink == 1 {
		return ReasonNone
	}
	return ReasonTokenFileUnsafe
}

// checkNotPersistent implements S-D3-8: compare st_dev of localFd
// ("/usr/local") against rootFd ("/"). Equal devices mean COS_PERSISTENT is
// not actually mounted there.
func checkNotPersistent(rootFd, localFd int) bool {
	if notPersistentOverride != nil {
		return notPersistentOverride(rootFd, localFd)
	}
	var rootSt, localSt unix.Stat_t
	if err := unix.Fstat(rootFd, &rootSt); err != nil {
		return false
	}
	if err := unix.Fstat(localFd, &localSt); err != nil {
		return false
	}
	return rootSt.Dev == localSt.Dev
}
