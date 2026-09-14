//go:build linux

package etcdsnapshot

import (
	"context"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// EncryptedAtRest reports whether dir's mount is backed by a dm-crypt (LUKS)
// device, checking the mount's OWN backing device rather than "any ancestor" in
// a device-mapper stack (the earlier findmnt/lsblk probe's flaw). It is a pure
// sysfs/statfs probe -- no exec, no findmnt/lsblk -- and is fail-closed: any
// error, an unrecognized filesystem, or an anonymous device (overlay, tmpfs,
// nfs, fuse, a btrfs subvolume, ...) yields false.
//
// lsblk's "crypt" TYPE for a device is itself derived from the presence of a
// /sys/dev/block/<maj>:<min>/dm/uuid value with the "CRYPT-" prefix (the
// cryptsetup/dm-crypt device-mapper target name) -- reading that file directly
// is equivalent to the old probe's premise, scoped correctly to dir's own device.
func EncryptedAtRest(ctx context.Context, dir string) bool {
	_ = ctx // no blocking I/O below; ctx kept for interface symmetry with other gates
	var sfs unix.Statfs_t
	if err := unix.Statfs(dir, &sfs); err != nil {
		return false
	}
	var st unix.Stat_t
	if err := unix.Stat(dir, &st); err != nil {
		return false
	}
	major, minor := unix.Major(st.Dev), unix.Minor(st.Dev)
	return encryptedAtRestDecision(int64(sfs.Type), major, minor, readSysfsFile)
}

// encryptedAtRestDecision is the pure decision core of EncryptedAtRest: given the
// filesystem magic and the device major:minor already resolved by the caller (or
// a table test), and a way to read a sysfs file, it decides whether the backing
// device is dm-crypt encrypted. Table-testable without touching any real device.
func encryptedAtRestDecision(fsType int64, major, minor uint32, readFile func(path string) ([]byte, error)) bool {
	switch fsType {
	case unix.EXT4_SUPER_MAGIC, unix.XFS_SUPER_MAGIC:
		// EXT2_SUPER_MAGIC/EXT3_SUPER_MAGIC/EXT4_SUPER_MAGIC share the same value.
	default:
		return false
	}
	if major == 0 {
		// Anonymous/virtual device: overlay, tmpfs, nfs, fuse, btrfs, ... never trust it.
		return false
	}
	data, err := readFile(fmt.Sprintf("/sys/dev/block/%d:%d/dm/uuid", major, minor))
	if err != nil {
		return false
	}
	return strings.HasPrefix(string(data), "CRYPT-")
}

func readSysfsFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
