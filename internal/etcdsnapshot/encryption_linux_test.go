//go:build linux

package etcdsnapshot

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

// --- 9. Encryption gate: pure decision table ---------------------------------

func TestEncryptedAtRestDecision(t *testing.T) {
	luks := func(string) ([]byte, error) { return []byte("CRYPT-LUKS2-abcdef0123456789"), nil }
	lowercase := func(string) ([]byte, error) { return []byte("crypt-luks2-abcdef"), nil }
	lvm := func(string) ([]byte, error) { return []byte("LVM-abcdef0123456789"), nil }
	readErr := func(string) ([]byte, error) { return nil, errors.New("no such file or directory") }

	const (
		overlayMagic = 0x794c7630
		tmpfsMagic   = 0x01021994
		btrfsMagic   = 0x9123683e
	)

	cases := []struct {
		name     string
		fsType   int64
		major    uint32
		minor    uint32
		readFile func(string) ([]byte, error)
		want     bool
	}{
		{"ext4 + CRYPT-LUKS2 -> true", unix.EXT4_SUPER_MAGIC, 253, 0, luks, true},
		{"xfs + CRYPT -> true", unix.XFS_SUPER_MAGIC, 253, 1, luks, true},
		{"ext4 + LVM -> false", unix.EXT4_SUPER_MAGIC, 253, 2, lvm, false},
		{"ext4 + no dm/uuid (read error) -> false", unix.EXT4_SUPER_MAGIC, 253, 3, readErr, false},
		{"overlay magic -> false", overlayMagic, 253, 4, luks, false},
		{"tmpfs magic -> false", tmpfsMagic, 253, 5, luks, false},
		{"btrfs magic -> false", btrfsMagic, 253, 6, luks, false},
		{"major 0 -> false", unix.EXT4_SUPER_MAGIC, 0, 0, luks, false},
		{"CRYPT uuid but btrfs fs -> false", btrfsMagic, 253, 7, luks, false},
		{"lowercase crypt- prefix -> false", unix.EXT4_SUPER_MAGIC, 253, 8, lowercase, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := encryptedAtRestDecision(tc.fsType, tc.major, tc.minor, tc.readFile)
			if got != tc.want {
				t.Errorf("encryptedAtRestDecision(fsType=%#x, major=%d, minor=%d) = %v, want %v",
					tc.fsType, tc.major, tc.minor, got, tc.want)
			}
		})
	}
}

// TestEncryptedAtRest_SmokeNoPanic proves the real probe runs cleanly against a
// live path without panicking. It intentionally does NOT assert the returned
// value: the test host's temp filesystem is not a production COS_PERSISTENT
// mount, so either answer is a legitimate probe result here.
func TestEncryptedAtRest_SmokeNoPanic(t *testing.T) {
	_ = EncryptedAtRest(context.Background(), t.TempDir())
}
