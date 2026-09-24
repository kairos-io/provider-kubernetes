//go:build !linux

package actualstate

// readRegularFile has no implementation outside linux: the non-blocking,
// no-follow read relies on Linux open(2) semantics, and Kairos images are
// Linux-only in production. Reporting "unreadable" keeps InitIncomplete false,
// so the status falls back to its other signals instead of guessing.
func readRegularFile(string, int) ([]byte, bool) {
	return nil, false
}
