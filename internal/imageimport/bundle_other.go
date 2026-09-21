//go:build !linux

package imageimport

// walkBundle has no implementation outside linux: the O-3 walk relies on
// Linux-specific openat/fstat/fcntl semantics (O_NOFOLLOW, O_NONBLOCK,
// device comparison). Kairos images are Linux-only in production; on any
// other GOOS this fails closed rather than silently skipping every check.
func walkBundle() (preparedBundle, error) {
	return nil, &bundleError{ReasonDirUnsafe, "image import is only supported on linux"}
}
