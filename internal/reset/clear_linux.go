//go:build linux

package reset

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// maxClearDepth bounds the cleanup walk, which holds one descriptor per level.
// kubeadm's own trees are a few levels deep, but a pod's disk-backed emptyDir
// under /var/lib/kubelet can hold whatever its workload wrote, so the bound is
// generous; a deeper tree is left in place and reported, not recursed into.
const maxClearDepth = 512

// testHookOpenat2 and testHookUnlinkat let tests make one entry look like a
// mount point (EXDEV from openat2, EBUSY from unlinkat) or like a kernel
// without openat2 (ENOSYS). Mounting needs privileges the unit tests do not
// have; the real mount behavior is covered by the e2e reset scenario. nil in
// production.
var (
	testHookOpenat2  func(name string) error
	testHookUnlinkat func(name string) error
)

// clearArtifact empties one artifact directory and keeps the directory itself,
// the way kubeadm's own reset deletes "contents of directories". The directory
// may be a mount point (on Kairos /etc/kubernetes and /var/lib/kubelet are
// persistent binds, in the e2e container docker volumes), and removing it
// would fail with EBUSY after its contents were already gone.
//
// Nothing below it is ever crossed into: a directory that is a mount point is
// refused by openat2 RESOLVE_NO_XDEV, which also catches bind mounts from the
// same filesystem (same st_dev), and a file that is a mount point is refused
// by unlinkat with EBUSY. Both are kept and returned. This is what keeps a
// reset from deleting the data of a volume kubeadm could not unmount (it
// returns an error and leaves /var/lib/kubelet alone when an unmount fails),
// and from blocking on a dead network mount: the mounted filesystem is never
// read.
//
// A symlink at the artifact path is removed, never followed. The returned
// error is the first failure other than a kept mount.
func clearArtifact(path string) (kept []string, err error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, nil // idempotent
		}
		return nil, fmt.Errorf("lstat %s: %w", path, err)
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
	case unix.S_IFLNK:
		logrus.Warnf("provider-kubernetes: refusing to follow symlinked artifact %s (removing the link only, not its target)", path)
		return nil, ignoreNotExist(unix.Unlink(path))
	default:
		return nil, ignoreNotExist(unix.Unlink(path))
	}

	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, nil
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = unix.Close(fd) }()

	c := &clearer{}
	c.clearDir(fd, path, 0)
	return c.kept, c.firstErr
}

// clearer accumulates what one artifact's walk kept and the first failure.
type clearer struct {
	kept     []string
	firstErr error
}

func (c *clearer) keep(path, why string) {
	logrus.Warnf("provider-kubernetes: reset left %s in place: %s", path, why)
	c.kept = append(c.kept, path)
}

// isMountPoint is the reason logged for a kept mount point.
const isMountPoint = "it is a mount point, and nothing on a mounted filesystem is removed"

func (c *clearer) fail(err error) {
	logrus.Warnf("provider-kubernetes: reset: %v", err)
	if c.firstErr == nil {
		c.firstErr = err
	}
}

// clearDir removes every entry of the directory open at fd and reports whether
// it is now empty. Each entry is first unlinked as a non-directory; only a
// directory (EISDIR) is opened, with openat2 refusing to cross a mount, to
// symlinks or out of fd.
func (c *clearer) clearDir(fd int, path string, depth int) bool {
	names, err := readDirNames(fd)
	if err != nil {
		c.fail(fmt.Errorf("read %s: %w", path, err))
		return false
	}
	empty := true
	for _, name := range names {
		child := filepath.Join(path, name)
		err := unlinkat(fd, name, 0)
		switch {
		case err == nil, errors.Is(err, unix.ENOENT):
			continue
		case errors.Is(err, unix.EBUSY):
			c.keep(child, isMountPoint) // a file that is a mount point
			empty = false
			continue
		case errors.Is(err, unix.EISDIR):
			// A directory: cleared below.
		default:
			c.fail(fmt.Errorf("remove %s: %w", child, err))
			empty = false
			continue
		}

		cfd, err := openBeneath(fd, name)
		switch {
		case err == nil:
		case errors.Is(err, unix.ENOENT):
			continue
		case errors.Is(err, unix.EXDEV):
			c.keep(child, isMountPoint)
			empty = false
			continue
		case errors.Is(err, unix.ENOSYS):
			// Without openat2 a mount point cannot be told apart, so nothing
			// is descended into blind.
			c.keep(child, "the kernel has no openat2, so whether it is a mount point cannot be checked")
			c.fail(fmt.Errorf("cannot check %s for mount points: the kernel has no openat2", child))
			empty = false
			continue
		default:
			c.fail(fmt.Errorf("open %s: %w", child, err))
			empty = false
			continue
		}
		if depth >= maxClearDepth {
			_ = unix.Close(cfd)
			c.fail(fmt.Errorf("%s is nested deeper than %d levels; left in place", child, maxClearDepth))
			empty = false
			continue
		}
		childEmpty := c.clearDir(cfd, child, depth+1)
		_ = unix.Close(cfd)
		if !childEmpty {
			// What stayed below is already reported.
			empty = false
			continue
		}
		err = unix.Unlinkat(fd, name, unix.AT_REMOVEDIR)
		switch {
		case err == nil, errors.Is(err, unix.ENOENT):
		case errors.Is(err, unix.EBUSY):
			c.keep(child, isMountPoint)
			empty = false
		default:
			c.fail(fmt.Errorf("remove %s: %w", child, err))
			empty = false
		}
	}
	return empty
}

// unlinkat is unix.Unlinkat with the test seam in front of it.
func unlinkat(fd int, name string, flags int) error {
	if testHookUnlinkat != nil {
		if err := testHookUnlinkat(name); err != nil {
			return err
		}
	}
	return unix.Unlinkat(fd, name, flags)
}

// openBeneath opens the directory name below fd without crossing a mount
// point, without following a symlink anywhere, and without leaving fd.
func openBeneath(fd int, name string) (int, error) {
	if testHookOpenat2 != nil {
		if err := testHookOpenat2(name); err != nil {
			return -1, err
		}
	}
	return unix.Openat2(fd, name, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_XDEV | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_BENEATH,
	})
}

// readDirNames lists the entries of the directory open at fd without stat'ing
// any of them, so a mount point below is never touched. The names are sorted:
// listing order is the filesystem's own (hash order on ext4, creation or
// reverse-creation order on tmpfs depending on the kernel), and sorting makes
// the walk, and the paths it keeps and reports, the same everywhere.
func readDirNames(fd int) ([]string, error) {
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(dup)
	f := os.NewFile(uintptr(dup), "")
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(-1)
	sort.Strings(names)
	return names, err
}

func ignoreNotExist(err error) error {
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}
