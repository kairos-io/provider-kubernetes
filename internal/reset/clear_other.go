//go:build !linux

package reset

import "fmt"

// clearArtifact has no implementation outside linux: refusing to cross a mount
// point relies on Linux openat2 RESOLVE_NO_XDEV, and Kairos images are
// Linux-only. It removes nothing rather than removing without that check.
func clearArtifact(path string) ([]string, error) {
	return nil, fmt.Errorf("reset: clearing %s is only supported on linux", path)
}
