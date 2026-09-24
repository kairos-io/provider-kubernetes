//go:build linux

package actualstate

import (
	"errors"

	"golang.org/x/sys/unix"
)

// readRegularFile returns the content of path if, and only if, it is a
// regular file of at most limit bytes. The probe runs in a boot stage, so it
// must never block (design principle 4): a FIFO or device at the path is
// refused by lstat before any open, the open itself is O_NONBLOCK and
// O_NOFOLLOW, and the type is checked again on the opened descriptor in case
// the entry was swapped in between. st_size is not trusted: at most limit+1
// bytes are read, and more than limit is refused.
//
// This is a single-file read by absolute path, not a directory walk, so it
// does not share the per-component helpers of imageimport, unitmigrate and
// clusterconfigdir (F-NOFOLLOW).
func readRegularFile(path string, limit int) ([]byte, bool) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, false
	}
	return readRegularFileAfterLstat(path, limit)
}

// readRegularFileAfterLstat is readRegularFile past its lstat gate: the entry
// may have been replaced since, so it is opened without following a link or
// blocking, and the descriptor's own type decides.
func readRegularFileAfterLstat(path string, limit int) ([]byte, bool) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, false
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, false
	}
	buf := make([]byte, limit+1)
	total := 0
	for total < len(buf) {
		n, err := unix.Read(fd, buf[total:])
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, false
		}
		if n == 0 {
			break
		}
		total += n
	}
	if total > limit {
		return nil, false
	}
	return buf[:total], true
}
