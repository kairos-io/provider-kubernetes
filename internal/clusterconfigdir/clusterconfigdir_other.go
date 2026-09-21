//go:build !linux

package clusterconfigdir

// ensureDir has no implementation outside linux: S-D3-2/S-D3-3/S-D3-4/S-D3-8
// rely on Linux-specific openat/mkdirat/fstatat semantics (O_NOFOLLOW,
// AT_SYMLINK_NOFOLLOW, st_dev). Kairos images are Linux-only in production.
//
// Unlike internal/imageimport and internal/unitmigrate (which fail closed on
// other platforms because they are explicit, separately-invoked
// subcommands), Ensure sits on the clusterplugin.Provider hot path on every
// GOOS this binary might be built for -- including a developer's non-Linux
// workstation running `go test ./...` or `go vet ./...`. The only safe
// non-Linux behavior is a silent no-op: nothing is created, nothing is
// reported, and -- critically -- nothing is withheld, so provider.Provider's
// real output is never suppressed on a platform where the underlying defect
// cannot even occur.
func ensureDir(_ string) Report {
	return Report{}
}
