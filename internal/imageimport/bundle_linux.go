//go:build linux

package imageimport

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// Path components of the walk (ADR-16-A2 decision 3). rootPath is a
// package-private seam: production always walks from "/" (there is no
// runtime override anywhere in the exported API); tests set it to a temp
// directory tree.
var (
	rootPath = "/"

	// expectedOwnerUID is the uid every directory and file on the walk must
	// be owned by. Production is always 0 (root); tests set it to their own
	// uid, since an unprivileged test cannot create root-owned fixtures.
	expectedOwnerUID = 0

	// fstatFn is the fstat implementation the walk uses. Tests override it
	// to inject a device value that would otherwise require a second real
	// filesystem to exercise (the O-3 device-mismatch case).
	fstatFn = unix.Fstat
)

const (
	systemDirName      = "system"
	providerDirName    = "provider-kubernetes"
	imagesDirName      = "images"
	providersDirName   = "providers"
	providerBinaryName = "agent-provider-kubernetes"
	lockFileName       = "images.lock"
)

// linuxBundle is preparedBundle's production (and test-seam) implementation:
// an open, checked images directory fd plus the facts gathered once for the
// whole bundle.
type linuxBundle struct {
	imagesFd  int
	anchorDev uint64
	readOnly  bool
	lockData  []byte
}

func (b *linuxBundle) LockData() []byte { return b.lockData }
func (b *linuxBundle) ReadOnly() bool   { return b.readOnly }
func (b *linuxBundle) Close()           { _ = unix.Close(b.imagesFd) }

func (b *linuxBundle) OpenTarball(name string, minSize, maxSize int64, missingReason Reason) (*os.File, Reason, string) {
	return openFileChecked(b.imagesFd, name, b.anchorDev, minSize, maxSize, missingReason)
}

func (b *linuxBundle) Unlisted(lockedNames map[string]bool) []string {
	return listUnlisted(b.imagesFd, lockedNames)
}

// walkBundle performs ADR-16-A2 decision 3's no-follow walk from rootPath: it
// opens (and owner/mode-checks) "/", "system", "provider-kubernetes" and
// "images" in turn; separately walks "system/providers/<providerBinaryName>"
// as the device anchor; opens and checks images.lock; and lists the images
// dir's unlisted entries. It returns errNotBundled ONLY when
// "provider-kubernetes" itself is ENOENT under "system" -- anything
// missing/unsafe below that point is a *bundleError (a whole-bundle
// refusal).
func walkBundle() (preparedBundle, error) {
	rootFd, err := unix.Open(rootPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &bundleError{ReasonDirUnsafe, fmt.Sprintf("open %q: %v", rootPath, err)}
	}
	defer func() { _ = unix.Close(rootFd) }()
	if ok, detail := checkDirSafe(rootFd, rootPath); !ok {
		return nil, &bundleError{ReasonDirUnsafe, detail}
	}

	systemFd, err := openDirNoFollow(rootFd, systemDirName)
	if err != nil {
		return nil, &bundleError{ReasonDirUnsafe, fmt.Sprintf("open %q: %v", systemDirName, err)}
	}
	defer func() { _ = unix.Close(systemFd) }()
	if ok, detail := checkDirSafe(systemFd, systemDirName); !ok {
		return nil, &bundleError{ReasonDirUnsafe, detail}
	}

	anchorDev, aErr := anchorDevice(systemFd)
	if aErr != nil {
		return nil, aErr
	}

	providerFd, err := openDirNoFollow(systemFd, providerDirName)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, errNotBundled
		}
		return nil, &bundleError{ReasonDirUnsafe, fmt.Sprintf("open %q: %v", providerDirName, err)}
	}
	defer func() { _ = unix.Close(providerFd) }()
	if ok, detail := checkDirSafe(providerFd, providerDirName); !ok {
		return nil, &bundleError{ReasonDirUnsafe, detail}
	}

	imagesFd, err := openDirNoFollow(providerFd, imagesDirName)
	if err != nil {
		return nil, &bundleError{ReasonDirUnsafe, fmt.Sprintf("open %q: %v", imagesDirName, err)}
	}
	ok := false
	defer func() {
		if !ok {
			_ = unix.Close(imagesFd)
		}
	}()
	if dirOK, detail := checkDirSafe(imagesFd, imagesDirName); !dirOK {
		return nil, &bundleError{ReasonDirUnsafe, detail}
	}

	readOnly := statfsReadOnly(imagesFd)

	lockFile, reason, detail := openFileChecked(imagesFd, lockFileName, anchorDev, minLockFileSize, maxLockSize, ReasonLockMissing)
	if reason != "" {
		return nil, &bundleError{reason, detail}
	}
	// Bound the read too: fstat checked the size, but the file could grow before
	// it is read on a writable root. ParseLock rejects anything over maxLockSize.
	lockData, readErr := io.ReadAll(io.LimitReader(lockFile, maxLockSize+1))
	_ = lockFile.Close()
	if readErr != nil {
		return nil, &bundleError{ReasonLockMissing, fmt.Sprintf("read images.lock: %v", readErr)}
	}

	ok = true
	return &linuxBundle{imagesFd: imagesFd, anchorDev: anchorDev, readOnly: readOnly, lockData: lockData}, nil
}

// anchorDevice walks system/providers/<providerBinaryName> with the same
// no-follow dir rules plus the O-3 regular-file rules, and returns its
// st_dev. Every failure here is ReasonAnchorUnsafe (as opposed to
// ReasonDirUnsafe for the main bundle-dir walk): it names a distinct problem
// space (the device anchor itself), not the bundle being imported.
func anchorDevice(systemFd int) (uint64, error) {
	providersFd, err := openDirNoFollow(systemFd, providersDirName)
	if err != nil {
		return 0, &bundleError{ReasonAnchorUnsafe, fmt.Sprintf("open %q: %v", providersDirName, err)}
	}
	defer func() { _ = unix.Close(providersFd) }()
	if ok, detail := checkDirSafe(providersFd, providersDirName); !ok {
		return 0, &bundleError{ReasonAnchorUnsafe, detail}
	}

	binFd, err := unix.Openat(providersFd, providerBinaryName,
		unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, &bundleError{ReasonAnchorUnsafe, fmt.Sprintf("open %q: %v", providerBinaryName, err)}
	}
	defer func() { _ = unix.Close(binFd) }()

	var st unix.Stat_t
	if err := fstatFn(binFd, &st); err != nil {
		return 0, &bundleError{ReasonAnchorUnsafe, fmt.Sprintf("fstat %q: %v", providerBinaryName, err)}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return 0, &bundleError{ReasonAnchorUnsafe, fmt.Sprintf("%q is not a regular file (mode %#o)", providerBinaryName, st.Mode)}
	}
	if int(st.Uid) != expectedOwnerUID {
		return 0, &bundleError{ReasonAnchorUnsafe, fmt.Sprintf("%q is owned by uid %d, want %d", providerBinaryName, st.Uid, expectedOwnerUID)}
	}
	if st.Mode&0o022 != 0 || st.Mode&(unix.S_ISUID|unix.S_ISGID) != 0 {
		return 0, &bundleError{ReasonAnchorUnsafe, fmt.Sprintf("%q has mode %#o", providerBinaryName, st.Mode&0o7777)}
	}
	return uint64(st.Dev), nil
}

// openDirNoFollow opens name under parentFd as a directory, refusing to
// follow a symlink.
func openDirNoFollow(parentFd int, name string) (int, error) {
	return unix.Openat(parentFd, name, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}

// checkDirSafe applies the O-3 directory rules (S_ISDIR, uid ==
// expectedOwnerUID, not group/other-writable; gid is NOT checked) to fd.
func checkDirSafe(fd int, label string) (ok bool, detail string) {
	var st unix.Stat_t
	if err := fstatFn(fd, &st); err != nil {
		return false, fmt.Sprintf("fstat %q: %v", label, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return false, fmt.Sprintf("%q is not a directory (mode %#o)", label, st.Mode)
	}
	if int(st.Uid) != expectedOwnerUID {
		return false, fmt.Sprintf("%q is owned by uid %d, want %d", label, st.Uid, expectedOwnerUID)
	}
	if st.Mode&0o022 != 0 {
		return false, fmt.Sprintf("%q is group- or other-writable (mode %#o)", label, st.Mode&0o777)
	}
	return true, ""
}

// openFileChecked opens name under dirFd with the O-3 file-open flags
// (O_NOFOLLOW|O_NONBLOCK|O_NOCTTY, so a planted symlink or FIFO is refused
// instead of hanging or following) and classifies it against the O-3
// fine-grained reasons: not-regular, owner, mode, size, device (and, via
// missingReason, lock-missing or missing). On success it clears O_NONBLOCK
// (fcntl) BEFORE wrapping the descriptor in an *os.File, positioned at
// offset 0.
func openFileChecked(dirFd int, name string, anchorDev uint64, minSize, maxSize int64, missingReason Reason) (*os.File, Reason, string) {
	fd, err := unix.Openat(dirFd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		switch {
		case errors.Is(err, unix.ENOENT):
			return nil, missingReason, fmt.Sprintf("%s: not found", name)
		case errors.Is(err, unix.ELOOP):
			return nil, ReasonSymlink, fmt.Sprintf("%s: is a symlink", name)
		default:
			return nil, ReasonNotRegular, fmt.Sprintf("open %s: %v", name, err)
		}
	}
	closeOnReturn := true
	defer func() {
		if closeOnReturn {
			_ = unix.Close(fd)
		}
	}()

	var st unix.Stat_t
	if err := fstatFn(fd, &st); err != nil {
		return nil, ReasonNotRegular, fmt.Sprintf("fstat %s: %v", name, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, ReasonNotRegular, fmt.Sprintf("%s is not a regular file (mode %#o)", name, st.Mode)
	}
	if int(st.Uid) != expectedOwnerUID {
		return nil, ReasonOwner, fmt.Sprintf("%s is owned by uid %d, want %d", name, st.Uid, expectedOwnerUID)
	}
	if st.Mode&0o022 != 0 || st.Mode&(unix.S_ISUID|unix.S_ISGID) != 0 {
		return nil, ReasonMode, fmt.Sprintf("%s has mode %#o", name, st.Mode&0o7777)
	}
	if st.Size < minSize || st.Size > maxSize {
		return nil, ReasonSize, fmt.Sprintf("%s is %d bytes, want %d..%d", name, st.Size, minSize, maxSize)
	}
	if uint64(st.Dev) != anchorDev {
		return nil, ReasonDevice, fmt.Sprintf("%s is on a different filesystem than the provider binary", name)
	}

	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return nil, ReasonNotRegular, fmt.Sprintf("fcntl F_GETFL %s: %v", name, err)
	}
	if flags&unix.O_NONBLOCK != 0 {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags&^unix.O_NONBLOCK); err != nil {
			return nil, ReasonNotRegular, fmt.Sprintf("fcntl F_SETFL %s: %v", name, err)
		}
	}

	closeOnReturn = false
	return os.NewFile(uintptr(fd), name), "", ""
}

// statfsReadOnly reports fd's filesystem ST_RDONLY bit. Informational only
// (O-3): it is reported in the summary, never used to gate import.
func statfsReadOnly(fd int) bool {
	var sfs unix.Statfs_t
	if err := unix.Fstatfs(fd, &sfs); err != nil {
		return false
	}
	return sfs.Flags&unix.ST_RDONLY != 0
}

// listUnlisted reads up to maxReaddirEntries names from the images directory
// and returns every one that is not images.lock and not a key of lockedNames.
// It reads through a fresh open of "." (a dup would share the directory
// offset, so a second scan would return nothing) and never opens any of the
// listed entries.
func listUnlisted(imagesFd int, lockedNames map[string]bool) []string {
	scanFd, err := unix.Openat(imagesFd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil
	}
	dir := os.NewFile(uintptr(scanFd), imagesDirName)
	defer func() { _ = dir.Close() }()

	names, _ := dir.Readdirnames(maxReaddirEntries)
	var unlisted []string
	for _, n := range names {
		if n == lockFileName || lockedNames[n] {
			continue
		}
		unlisted = append(unlisted, n)
	}
	return unlisted
}
