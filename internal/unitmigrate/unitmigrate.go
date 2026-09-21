// Package unitmigrate implements ADR-19 U2 decision 3: the one-time cleanup of
// the stale provider-owned unit copies that older releases left under the
// persistent /etc/systemd/system (ADR-16-A2's rsync-update-only mtime trap).
// Now that containerd.service, kubelet.service,
// provider-kubernetes-image-import.service and the kubelet drop-in are
// image-owned under /usr/lib/systemd/system (U2 decision 2), a copy left
// behind at the old /etc path would shadow the image's unit forever (higher
// in systemd's unit search order) and never be refreshed by an upgrade.
//
// Migrate removes exactly four fragment/drop-in paths and three .wants
// symlinks -- a FIXED list, never a glob (S19-7) -- and ONLY when each is
// byte-identical to one of the blobs this project has ever shipped at that
// exact path (the FROZEN set below) or, for a link, points at exactly the
// fragment it is supposed to enable. Anything else -- an edited copy, a
// directory, a FIFO, a wrong owner, an oversized file, a mismatched link
// target, something that changed between the hash read and the unlink
// (Reason "race"), or any other unexpected error (Reason "io") -- is left in
// place and reported; unit files can carry proxy credentials in their
// Environment= lines, so their content, size and hash are never logged
// (S19-9). The full closed reason set is: modified, symlink, not-regular,
// owner, size, link-target, walk-unsafe, race, io. A symlink at a fragment or
// drop-in path (for example an operator `systemctl mask` to /dev/null, or to
// any other target -- security review 2026-09-17) is left silently: masking
// is the documented, supported way to disable one of our units post-U2
// (S19-14, docs/upgrades.md), and it must not make every future boot report a
// problem. It is still visible: reportOverrides() lstats the fragment path
// independently of the deletion walk, so a mask still produces an
// `override %q` line and is never invisible.
//
// It is invoked at boot by a new image-only oneshot,
// provider-kubernetes-unit-migrate.service (Type=oneshot,
// Before=containerd.service kubelet.service
// provider-kubernetes-image-import.service, TimeoutStartSec=45), via the
// `agent-provider-kubernetes migrate-units` subcommand (no arguments). If a
// fragment or drop-in was removed, it reloads systemd (S19-10) so the queued
// unit starts on this same boot read the image's /usr/lib definitions rather
// than a copy that was just deleted out from under an already-parsed
// in-memory unit. It always returns within its caller's context and never
// panics (#4099-1); on any unexpected error the file in question is kept
// (fail-soft), the run is marked failed, and nothing already removed is
// restored.
package unitmigrate

// Kind is the closed set of scope-entry kinds a "removed" log line names.
type Kind string

const (
	KindFragment Kind = "fragment"
	KindDropin   Kind = "dropin"
	KindLink     Kind = "link"
)

// Reason is the closed set of reasons a scope entry was kept (S19-7/S19-9).
// Every "kept" log line uses exactly one of these.
const (
	// ReasonModified means the entry is a regular file, owned by root, within
	// the size bound, but its content hash is not in the FROZEN set for that
	// path: this is the "shadowed by content we did not ship" case that fails
	// the migrate unit.
	ReasonModified Reason = "modified"
	// ReasonSymlink is reserved for the closed reason set (S19-9); a symlink at
	// a fragment or drop-in path is deliberately left WITHOUT a log line (an
	// operator mask, S19-14), so this value is not currently emitted by
	// Migrate. It stays part of the enum for completeness/future use, exactly
	// as unused Reason values would if a future revision decides to report
	// masks too.
	ReasonSymlink Reason = "symlink"
	// ReasonNotRegular covers a directory, FIFO, socket or device planted at a
	// path that must be a regular file (fragment/drop-in), or anything other
	// than a symlink planted at a path that must be a symlink (a .wants link).
	ReasonNotRegular Reason = "not-regular"
	ReasonOwner      Reason = "owner"
	ReasonSize       Reason = "size"
	// ReasonLinkTarget means a .wants entry is a symlink, but not to the exact
	// fragment path it is supposed to enable.
	ReasonLinkTarget Reason = "link-target"
	// ReasonWalkUnsafe means a parent directory component (for example
	// /etc/systemd/system/kubelet.service.d itself) was a symlink or a
	// non-directory, so the entries nominally beneath it could not be
	// reached safely and that subtree was skipped.
	ReasonWalkUnsafe Reason = "walk-unsafe"
	// ReasonRace means the entry changed between an earlier check (the hashed
	// open/fstat) and a later one (the immediate re-fstatat before unlinkat,
	// or a re-open that unexpectedly hit ELOOP): the file vanished, changed
	// type, or changed identity (dev/ino) in that window. Security review
	// 2026-09-17: /etc/systemd/system and its kubelet.service.d/
	// multi-user.target.wants subdirectories are root-writable only, and
	// nothing else rewrites them this early in boot, so a mismatch in that
	// narrow window is not a mundane I/O error -- it is the one signal that
	// says something else raced this migration, and folding it into ReasonIO
	// would lose that signal.
	ReasonRace Reason = "race"
	// ReasonIO covers any other unexpected errno (EROFS, EBUSY, EPERM, and
	// the like) that is not one of the more specific reasons above.
	ReasonIO Reason = "io"
)

// Reason is the closed enum type for a "kept" log line's reason field.
type Reason string

// Outcome is the closed enum reported in the one summary line Migrate always
// logs last.
type Outcome string

const (
	// OutcomeClean means every scope entry was already absent (a fresh
	// install, a live/recovery boot, or a second run after a prior migration):
	// nothing was removed and nothing was kept.
	OutcomeClean Outcome = "clean"
	// OutcomeMigrated means at least one scope entry was removed and nothing
	// was kept for a reason other than the silent symlink-mask case.
	OutcomeMigrated Outcome = "migrated"
	// OutcomeKeptModified means at least one fragment, drop-in or link exists
	// but does not match what this project shipped (or the exact enable
	// target) and was therefore left in place: the image unit is shadowed by
	// content this project did not ship, which is visible in
	// `systemctl --failed` (OQ-19-2).
	OutcomeKeptModified Outcome = "kept-modified"
	// OutcomeFailed means the walk could not safely reach part of its scope,
	// an unexpected I/O error occurred, or the daemon-reload itself failed.
	// Removals already performed are not undone.
	OutcomeFailed Outcome = "failed"
)

// ExitCode maps o to the process exit code `migrate-units` returns: 0 for
// clean/migrated, 1 otherwise (a usage error is 2, decided by the caller
// before Migrate ever runs).
func ExitCode(o Outcome) int {
	switch o {
	case OutcomeClean, OutcomeMigrated:
		return 0
	default:
		return 1
	}
}

// Result summarizes one Migrate call; its fields are exactly the summary
// line's counters.
type Result struct {
	Outcome   Outcome
	Removed   int
	Kept      int
	Overrides int
	Reloaded  bool
}

// Scope (S19-7/decision 3): a FIXED list, never a glob. Every name below is a
// literal; nothing here is derived from a directory listing.
const (
	nameContainerd = "containerd.service"
	nameKubelet    = "kubelet.service"
	nameImport     = "provider-kubernetes-image-import.service"
	nameDropinDir  = "kubelet.service.d"
	nameDropin     = "10-kubeadm.conf"
	nameWantsDir   = "multi-user.target.wants"
)

// fragmentNames is the fixed list of top-level unit fragments in scope.
var fragmentNames = []string{nameContainerd, nameKubelet, nameImport}

// Canonical, root-independent display paths (S19-9: paths are %q-quoted in
// every log line). These are always the real /etc path regardless of the
// rootPath test seam, which only redirects the actual syscalls.
const (
	pathContainerdFragment = "/etc/systemd/system/" + nameContainerd
	pathKubeletFragment    = "/etc/systemd/system/" + nameKubelet
	pathImportFragment     = "/etc/systemd/system/" + nameImport
	pathDropinDir          = "/etc/systemd/system/" + nameDropinDir
	pathKubeadmDropin      = pathDropinDir + "/" + nameDropin
	pathWantsDir           = "/etc/systemd/system/" + nameWantsDir
)

// fragmentPath returns the canonical display path for a top-level fragment name.
func fragmentPath(name string) string {
	return "/etc/systemd/system/" + name
}

// linkPath returns the canonical display path for a .wants entry name.
func linkPath(name string) string {
	return pathWantsDir + "/" + name
}

// wantsLinkTarget returns the exact absolute target a .wants entry named name
// must resolve to in order to be removed (today's units always live directly
// under /etc/systemd/system, `systemctl enable`'s absolute-target form).
func wantsLinkTarget(name string) string {
	return fragmentPath(name)
}

// frozenHashes is ADR-19 U2's FROZEN set (S19-11): every sha256 this project
// has ever shipped at each of the four scope paths that carry real content
// (the three fragments and the drop-in; the three .wants links carry no
// content of their own, only a target check). Re-derived by the lead from
// full history at 013483d (`git log --all -- systemd/`). Adding a hash here
// is a decision to accept deleting that exact content forever; U2 itself
// ships no new /etc content, so it adds none of its own. Every value here
// must equal sha256(testdata/<fixture>) -- see
// TestFrozenHashesMatchFixtures.
var frozenHashes = map[string]map[string]bool{
	pathContainerdFragment: {
		"b47a1e62d00497a7363f987f2c2a6ecd8e41778de7cd81696e431f28c4d7bbb5": true, // v0.1.0..013483d, 485 B
	},
	pathKubeletFragment: {
		"3e5647fb9b90d1fbe10fc23e5f17eaa890c27630c910c6ebafc8aa85519fbb52": true, // v0.1.0..013483d, 599 B
	},
	pathKubeadmDropin: {
		"47f61bc9b23acfcb56f35f547a6f0e8bdc2cfb729dfd7dbf1fa49d7534d5aff3": true, // v0.1.0..013483d, 659 B
	},
	pathImportFragment: {
		"b9f1aa66fe9ca2653a9a16b691f4a7327a9f885b134833e7d4b136f133e4a15c": true, // v0.3.0 (90abd7a), 1127 B
		"fcf2b3a936107385efb66c8699a2a6417221512d97ea3573a5124aae8dcf5d65": true, // #36 (013483d), 1630 B
	},
}

// isFrozen reports whether hexDigest is one of the blobs ever shipped at path
// (a pure function so the frozen-set gate is unit-testable without a single
// syscall).
func isFrozen(path, hexDigest string) bool {
	set, ok := frozenHashes[path]
	if !ok {
		return false
	}
	return set[hexDigest]
}
