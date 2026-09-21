// Package etcdsnapshot takes a best-effort etcd snapshot before a destructive
// kubeadm upgrade apply (ADR-12 U5, revised by ADR-12-A1 / F-ETCDCTL).
//
// SECURITY POSTURE (ADR-12-A1): an etcd snapshot is a full plaintext dump of
// every cluster Secret and the cluster PKI.
//
//   - Custody: the snapshot dir (DefaultDir) is expected to be a DIRECT mount of
//     COS_PERSISTENT (/usr/local), not an overlay -- so the fail-closed encryption
//     gate below is checking the device the bytes actually land on. Durability and
//     off-node copying are the operator's responsibility; the provider never
//     offboards a snapshot.
//   - Fail-closed encryption gate: EncryptedAtRest inspects the snapshot dir's OWN
//     backing device for a dm-crypt (LUKS) signature via sysfs/statfs only -- no
//     exec, no findmnt/lsblk, no "trust any ancestor in the device stack". Any
//     error or ambiguity yields false, and Run REFUSES to write a plaintext
//     snapshot when the gate is not satisfied.
//   - Bounded: the save itself always runs under its own sub-deadline
//     (SnapshotTimeout), strictly inside the ctx passed by the caller, so it can
//     never itself become the reason the upgrade hangs (#4099-1).
//   - Once per (cluster, target): Run takes at most one snapshot per (cluster CA,
//     target minor) pair, identified by a deterministic file name. A retried
//     `upgrade apply` (e.g. after a lost-race re-check) never re-snapshots or
//     prunes the already-taken, clean pre-upgrade snapshot for that target.
//   - Retention: once a NEW snapshot for a different (cluster, target) is
//     successfully taken and verified, older completed snapshots in the directory
//     are pruned (the directory retains the most recent snapshot, not an
//     unbounded history) -- but only ever on the success path, never on failure.
//   - Outcome is a closed enum (Outcome/Result): the caller logs it and ALWAYS
//     proceeds with the upgrade regardless of outcome -- this package is strictly
//     best-effort and never blocks (#4099-1).
//
// It is bounded by the ctx the caller supplies for everything except the save
// step, which additionally bounds itself to SnapshotTimeout so it can never
// consume the caller's entire budget.
package etcdsnapshot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/kairos-io/provider-kubernetes/internal/hostexec"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadm/credential"
)

// DefaultDir is the production etcd-snapshot directory: a direct mount of
// COS_PERSISTENT (/usr/local), so it survives reboots and is off the Kairos
// RW_PATHS tmpfs overlay (/var, /etc, /srv) and off every reset artifact path.
const DefaultDir = "/usr/local/provider-kubernetes/etcd-backup"

// EtcdctlPath is the path to the etcdctl binary shipped in the image (a static
// binary extracted from the cosign-verified etcd image at build time).
const EtcdctlPath = hostexec.EtcdctlPath

// SnapshotTimeout bounds the etcdctl snapshot-save step. It is deliberately
// larger than the etcdctl --command-timeout (commandTimeout) baked into
// SaveCommand, so a well-behaved etcdctl always times out on its own terms
// first; SnapshotTimeout is the outer backstop that guarantees Run itself never
// blocks the caller beyond this bound.
const SnapshotTimeout = 2 * time.Minute

// snapshotTimeout is the bound Run actually applies to the save step; it
// defaults to SnapshotTimeout. It is an unexported var (not the exported const)
// solely so tests can shorten it to exercise the timeout/kill path without a
// real 2-minute wait; production always runs with the documented SnapshotTimeout.
var snapshotTimeout = SnapshotTimeout

const (
	// dialTimeout bounds etcdctl's initial connection to the local etcd member.
	dialTimeout = 10 * time.Second
	// commandTimeout bounds each etcdctl RPC (snapshot save is one long-running
	// RPC). It MUST stay strictly below SnapshotTimeout so etcdctl's own timeout
	// fires before our outer context deadline does.
	commandTimeout = 100 * time.Second
	// saveWaitDelay bounds how long we wait for the child (and its stdio) to exit
	// after ctx is done, before exec gives up (os/exec WaitDelay).
	saveWaitDelay = 5 * time.Second
	// freeSpaceHeadroom is the fixed extra free space required beyond 2x the
	// current etcd db size, to absorb WAL growth during the copy.
	freeSpaceHeadroom = 1 << 30 // 1 GiB
	// errorDetailCap bounds how much sanitized command output we retain in a
	// Result's Detail, so a verbose/looping etcdctl can never blow up the status
	// document or logs.
	errorDetailCap = 1024 // 1 KiB
)

// Outcome is a closed enum describing what Run did. Callers must treat any value
// other than taken/skipped-already-taken as "no snapshot exists for this attempt"
// and proceed with the upgrade regardless (this package is best-effort).
type Outcome string

const (
	// OutcomeTaken means a new snapshot was written and verified.
	OutcomeTaken Outcome = "taken"
	// OutcomeAlreadyTaken means a prior, still-valid snapshot for this exact
	// (cluster, target) pair already exists; Run did not touch it.
	OutcomeAlreadyTaken Outcome = "skipped-already-taken"
	// OutcomeExternalEtcd means this node does not run a stacked etcd member.
	OutcomeExternalEtcd Outcome = "skipped-external-etcd"
	// OutcomeEncryptionUnconfirmed means the snapshot dir's at-rest encryption
	// could not be confirmed, so Run refused to write a plaintext snapshot.
	OutcomeEncryptionUnconfirmed Outcome = "skipped-encryption-unconfirmed"
	// OutcomeEtcdctlMissing means the etcdctl binary is not present (or not a
	// regular file) at the configured path.
	OutcomeEtcdctlMissing Outcome = "skipped-etcdctl-missing"
	// OutcomeInsufficientSpace means the snapshot dir's filesystem does not have
	// enough free space for a safe copy of the current etcd db.
	OutcomeInsufficientSpace Outcome = "skipped-insufficient-space"
	// OutcomeFailed means Run attempted the snapshot but it did not complete and
	// verify successfully.
	OutcomeFailed Outcome = "failed"
)

// Result is the outcome of one Run call.
type Result struct {
	// Outcome is the closed-enum verdict (see the Outcome* constants).
	Outcome Outcome
	// Path is the snapshot file, set only for OutcomeTaken and
	// OutcomeAlreadyTaken.
	Path string
	// Detail is an operator-facing, secret-free explanation. It never contains
	// raw etcdctl output verbatim: any command output is passed through
	// kubeadm.Sanitize and capped at errorDetailCap bytes first.
	Detail string
}

// Options configures Run.
type Options struct {
	// RootPath is the cluster root (cluster_root_path); locates the etcd
	// manifest, member dir, and PKI.
	RootPath string
	// TargetMinor is the upgrade target minor (e.g. "1.37"), used in the
	// snapshot's file name and in the once-per-(cluster,target) identity.
	TargetMinor string
	// EncryptionConfirmed reports whether dir's backing storage is confirmed
	// encrypted at rest. nil uses the package's fail-closed sysfs gate
	// (EncryptedAtRest).
	EncryptionConfirmed func(ctx context.Context, dir string) bool

	// --- Test seams. Deliberately UNEXPORTED: the snapshot directory and
	// binary path are constants in production; nothing in the production
	// path may redirect them (ADR-12-A1 M7). ---

	// dir overrides DefaultDir. Empty -> DefaultDir.
	dir string
	// etcdctlPath overrides EtcdctlPath. Empty -> EtcdctlPath.
	etcdctlPath string
	// save overrides the default `etcdctl snapshot save` exec. nil -> the
	// default exec of SaveCommand.
	save func(ctx context.Context, dest string) error
	// freeBytes overrides the default statfs-based free-space probe. nil ->
	// statfs Bavail*Bsize on dir.
	freeBytes func(dir string) (uint64, error)
	// ownerUID is the expected owner of the snapshot dir and file. Zero value
	// (0 = root) is the production default; tests set os.Getuid().
	ownerUID int
	// now overrides the timestamp used in the snapshot file name. Zero ->
	// time.Now().
	now time.Time
}

// SaveCommand builds the exact argv and env for `etcdctl snapshot save`, used by
// BOTH the production default save and the e2e test (so the e2e proof exercises
// literally the same command construction as production). env is a non-nil,
// EMPTY slice: etcdctl fills unset TLS/behavior flags from ETCDCTL_* environment
// variables (e.g. ETCDCTL_INSECURE_SKIP_TLS_VERIFY), so nothing may be inherited
// from the parent process -- the child must run with no environment at all.
func SaveCommand(etcdctlPath, rootPath, dest string) (argv []string, env []string) {
	pki := filepath.Join(rootPath, "etc", "kubernetes", "pki", "etcd")
	argv = []string{
		etcdctlPath,
		"--endpoints=https://127.0.0.1:2379",
		"--cacert=" + filepath.Join(pki, "ca.crt"),
		"--cert=" + filepath.Join(pki, "healthcheck-client.crt"),
		"--key=" + filepath.Join(pki, "healthcheck-client.key"),
		"--dial-timeout=" + durSeconds(dialTimeout),
		"--command-timeout=" + durSeconds(commandTimeout),
		"snapshot", "save", dest,
	}
	return argv, []string{}
}

// durSeconds formats d as a whole-second Go duration string (e.g. "100s"),
// regardless of how the constant is expressed, so SaveCommand's argv is stable.
func durSeconds(d time.Duration) string {
	return fmt.Sprintf("%ds", int64(d/time.Second))
}

// IsStackedEtcd reports whether this node runs a stacked etcd, by the presence of
// the etcd static-pod manifest or the etcd member dir under root. Pure file check.
func IsStackedEtcd(root string) bool {
	for _, p := range []string{
		filepath.Join(root, "etc", "kubernetes", "manifests", "etcd.yaml"),
		filepath.Join(root, "var", "lib", "etcd", "member"),
	} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// Run performs a best-effort, bounded etcd snapshot. It NEVER blocks the caller
// beyond ctx (and, for the save step, SnapshotTimeout) and never returns an
// error: every failure mode is surfaced as a Result the caller logs and then
// proceeds past (#4099-1). See the package doc for the full security posture.
func Run(ctx context.Context, o Options) Result {
	if !IsStackedEtcd(o.RootPath) {
		return Result{
			Outcome: OutcomeExternalEtcd,
			Detail:  "etcd is external/non-stacked on this node; the operator must snapshot etcd before upgrading",
		}
	}

	etcdctlPath := o.etcdctlPath
	if etcdctlPath == "" {
		etcdctlPath = EtcdctlPath
	}
	if !isRegularFile(etcdctlPath) {
		return Result{
			Outcome: OutcomeEtcdctlMissing,
			Detail:  fmt.Sprintf("etcdctl not found (or not a regular file) at %s", etcdctlPath),
		}
	}

	dir := o.dir
	if dir == "" {
		dir = DefaultDir
	}
	ownerUID := o.ownerUID

	if err := ensureDirSafe(o.RootPath, dir, ownerUID); err != nil {
		return Result{Outcome: OutcomeFailed, Detail: err.Error()}
	}

	confirmed := o.EncryptionConfirmed
	if confirmed == nil {
		confirmed = EncryptedAtRest
	}
	if !confirmed(ctx, dir) {
		return Result{
			Outcome: OutcomeEncryptionUnconfirmed,
			Detail: "refusing to write a plaintext etcd snapshot; kubeadm upgrade will still write a " +
				"plaintext etcd data-dir copy under /etc/kubernetes/tmp and renewed certificates/keys " +
				"under /etc/kubernetes/pki; take a manual off-node backup, see docs/upgrades.md " +
				"\"etcd backups\"",
		}
	}

	cid, err := clusterID(o.RootPath)
	if err != nil {
		return Result{Outcome: OutcomeFailed, Detail: fmt.Sprintf("compute cluster id: %v", err)}
	}
	srcTag := sourceEtcdTag(o.RootPath)
	for _, c := range []string{cid, o.TargetMinor, srcTag} {
		if err := validateComponent(c); err != nil {
			return Result{Outcome: OutcomeFailed, Detail: err.Error()}
		}
	}

	now := o.now
	if now.IsZero() {
		now = time.Now()
	}
	name := fmt.Sprintf("etcd-snapshot-%s-to-%s-from-%s-%s.db", cid, o.TargetMinor, srcTag, now.UTC().Format("20060102T150405Z"))
	dest := filepath.Join(dir, name)
	if filepath.Dir(dest) != dir {
		return Result{Outcome: OutcomeFailed, Detail: "computed snapshot path escapes the snapshot dir"}
	}

	if existing, ok := alreadyTaken(dir, cid, o.TargetMinor, ownerUID); ok {
		return Result{Outcome: OutcomeAlreadyTaken, Path: existing}
	}

	dbPath := filepath.Join(o.RootPath, "var", "lib", "etcd", "member", "snap", "db")
	dbInfo, err := os.Stat(dbPath)
	if err != nil {
		return Result{Outcome: OutcomeFailed, Detail: fmt.Sprintf("cannot size etcd db at %s: %v", dbPath, err)}
	}
	required := uint64(dbInfo.Size())*2 + freeSpaceHeadroom

	freeBytesFn := o.freeBytes
	if freeBytesFn == nil {
		freeBytesFn = defaultFreeBytes
	}
	free, err := freeBytesFn(dir)
	if err != nil {
		return Result{Outcome: OutcomeFailed, Detail: fmt.Sprintf("statfs snapshot dir %s: %v", dir, err)}
	}
	if free < required {
		return Result{
			Outcome: OutcomeInsufficientSpace,
			Detail:  fmt.Sprintf("insufficient free space on %s: have %d bytes, need %d bytes", dir, free, required),
		}
	}

	removeStalePartFiles(dir)

	saveFn := o.save
	if saveFn == nil {
		saveFn = func(ctx context.Context, dest string) error {
			return defaultSave(ctx, etcdctlPath, o.RootPath, dest)
		}
	}

	subctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()

	if err := saveFn(subctx, dest); err != nil {
		cleanupAfterFailedSave(dest)
		return Result{Outcome: OutcomeFailed, Detail: fmt.Sprintf("etcd snapshot save: %v", err)}
	}

	verified, err := verifyAndFinalize(dest, ownerUID)
	if err != nil {
		cleanupAfterFailedSave(dest)
		return Result{Outcome: OutcomeFailed, Detail: err.Error()}
	}

	// Prune only now, after a NEW snapshot is verified in place: never on any
	// failure path (an old, valid snapshot must survive a failed retake).
	pruneOthers(dir, verified)

	return Result{Outcome: OutcomeTaken, Path: verified}
}

// isRegularFile reports whether path Lstat's as a regular file (never follows a
// symlink, and a directory does not qualify).
func isRegularFile(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}

// forbiddenDirs returns the reset-artifact / secret-adjacent paths a snapshot dir
// must never equal or nest under, both root-joined (for a non-default
// cluster_root_path) and at their absolute production location.
func forbiddenDirs(root string) []string {
	rel := []string{
		filepath.Join("etc", "kubernetes"),
		filepath.Join("var", "lib", "kubelet"),
		filepath.Join("var", "lib", "etcd"),
		filepath.Join("usr", "local", ".state"),
	}
	out := make([]string, 0, len(rel)*2)
	for _, r := range rel {
		out = append(out, filepath.Clean(filepath.Join(root, r)), filepath.Clean(filepath.Join("/", r)))
	}
	return out
}

// checkNotForbidden requires clean (already filepath.Clean'd) to not equal or
// nest under any reserved path. Pure/side-effect-free so it can be checked
// without ever touching the filesystem (e.g. to sanity-check DefaultDir).
func checkNotForbidden(root, clean string) error {
	for _, f := range forbiddenDirs(root) {
		if clean == f || strings.HasPrefix(clean, f+string(os.PathSeparator)) {
			return fmt.Errorf("snapshot dir %q must not be under the reserved path %q", clean, f)
		}
	}
	return nil
}

// ensureDirSafe validates and (re)establishes the snapshot dir per ADR-12-A1 M7:
// absolute, off every reserved path, no symlink anywhere on the path (checked
// both before creation, via the deepest existing ancestor, and after), created
// 0700, and owned by ownerUID (tightened to 0700 if looser).
func ensureDirSafe(root, dir string, ownerUID int) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("snapshot dir must be absolute, got %q", dir)
	}
	clean := filepath.Clean(dir)
	if err := checkNotForbidden(root, clean); err != nil {
		return err
	}

	if err := verifyDeepestAncestorUnlinked(clean); err != nil {
		return err
	}
	if err := os.MkdirAll(clean, 0o700); err != nil {
		return fmt.Errorf("create snapshot dir %q: %w", clean, err)
	}

	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return fmt.Errorf("resolve snapshot dir %q: %w", clean, err)
	}
	if resolved != clean {
		return fmt.Errorf("snapshot dir %q contains a symlink (resolves to %q)", clean, resolved)
	}

	if err := verifyOwnedDir(clean, ownerUID); err != nil {
		return err
	}
	if err := verifyOwnedDir(filepath.Dir(clean), ownerUID); err != nil {
		return err
	}
	return tightenPerm(clean, 0o700)
}

// verifyDeepestAncestorUnlinked walks up from dir to the deepest ancestor that
// already exists and requires filepath.EvalSymlinks(a) == filepath.Clean(a) for
// it -- i.e. no symlink anywhere on the existing prefix of the path, before we
// ever create anything under it.
func verifyDeepestAncestorUnlinked(dir string) error {
	a := dir
	for {
		if _, err := os.Lstat(a); err == nil {
			resolved, err := filepath.EvalSymlinks(a)
			if err != nil {
				return fmt.Errorf("resolve existing ancestor %q: %w", a, err)
			}
			if resolved != filepath.Clean(a) {
				return fmt.Errorf("ancestor %q contains a symlink (resolves to %q)", a, resolved)
			}
			return nil
		}
		parent := filepath.Dir(a)
		if parent == a {
			return nil // reached the filesystem root without finding an existing ancestor
		}
		a = parent
	}
}

// verifyOwnedDir requires path to Lstat as a directory owned by ownerUID.
func verifyOwnedDir(path string, ownerUID int) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat %q: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%q is not a directory", path)
	}
	uid, ok := uidOf(info)
	if !ok {
		return fmt.Errorf("cannot determine owner of %q", path)
	}
	if uid != ownerUID {
		return fmt.Errorf("%q is owned by uid %d, want %d", path, uid, ownerUID)
	}
	return nil
}

// tightenPerm chmods path to want if its current permission bits are looser
// (i.e. different from want); it never widens permissions.
func tightenPerm(path string, want os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat %q: %w", path, err)
	}
	if info.Mode().Perm() != want {
		if err := os.Chmod(path, want); err != nil {
			return fmt.Errorf("chmod %q: %w", path, err)
		}
	}
	return nil
}

// uidOf extracts the owning UID from a FileInfo via the platform Sys() value.
func uidOf(info os.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// clusterID derives the once-per-(cluster,target) identity: the first 16
// lowercase hex characters of the SPKI SHA-256 of the cluster CA certificate.
func clusterID(root string) (string, error) {
	caPath := filepath.Join(root, "etc", "kubernetes", "pki", "ca.crt")
	data, err := os.ReadFile(caPath)
	if err != nil {
		return "", fmt.Errorf("read cluster CA %s: %w", caPath, err)
	}
	hash, err := credential.SPKIHashFromPEM(data)
	if err != nil {
		return "", fmt.Errorf("compute cluster CA SPKI hash: %w", err)
	}
	hexHash := strings.TrimPrefix(hash, "sha256:")
	if len(hexHash) < 16 {
		return "", fmt.Errorf("unexpectedly short SPKI hash %q", hash)
	}
	return strings.ToLower(hexHash[:16]), nil
}

// etcdImageTagRe extracts the tag from an etcd container image reference such as
// "registry.k8s.io/etcd:3.6.8-0".
var etcdImageTagRe = regexp.MustCompile(`/etcd:([0-9A-Za-z._-]+)`)

// sourceEtcdTag parses the running etcd image tag out of the static-pod
// manifest. Returns "unknown" if the manifest is missing or unparseable -- this
// is advisory (part of the snapshot file name), never fatal.
func sourceEtcdTag(root string) string {
	path := filepath.Join(root, "etc", "kubernetes", "manifests", "etcd.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return "unknown"
	}
	m := etcdImageTagRe.FindSubmatch(data)
	if m == nil {
		return "unknown"
	}
	return string(m[1])
}

// componentRe bounds every component placed into the snapshot file name so a
// malformed/hostile value (an oversized or path-breaking CA, tag, or minor)
// cannot corrupt the file name or escape dir.
var componentRe = regexp.MustCompile(`^[0-9A-Za-z._-]{1,64}$`)

func validateComponent(s string) error {
	if !componentRe.MatchString(s) {
		return fmt.Errorf("invalid snapshot name component %q", s)
	}
	return nil
}

// alreadyTakenPattern matches a snapshot file name for this exact (clusterID,
// targetMinor) pair, with the literal (data-derived, though already
// component-validated) parts quoted for safety.
func alreadyTakenPattern(clusterID, targetMinor string) *regexp.Regexp {
	pattern := "^etcd-snapshot-" + regexp.QuoteMeta(clusterID) + "-to-" + regexp.QuoteMeta(targetMinor) +
		`-from-[0-9A-Za-z._-]{1,64}-[0-9]{8}T[0-9]{6}Z\.db$`
	return regexp.MustCompile(pattern)
}

// alreadyTaken reports whether a snapshot for (clusterID, targetMinor) already
// exists and qualifies: a regular file, owned by ownerUID, mode exactly 0600,
// and non-empty. A planted empty file, symlink, or wrong-mode/owner file does
// NOT qualify (and is not removed here -- it is simply not treated as "already
// taken").
func alreadyTaken(dir, clusterID, targetMinor string, ownerUID int) (string, bool) {
	re := alreadyTakenPattern(clusterID, targetMinor)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !re.MatchString(e.Name()) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		info, err := os.Lstat(p)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.Size() <= 0 || info.Mode().Perm() != 0o600 {
			continue
		}
		uid, ok := uidOf(info)
		if !ok || uid != ownerUID {
			continue
		}
		return p, true
	}
	return "", false
}

// defaultFreeBytes reports the free bytes available to an unprivileged writer on
// dir's filesystem (statfs Bavail*Bsize).
func defaultFreeBytes(dir string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}

// stalePartRe matches a leftover partial-save artifact from an interrupted
// `etcdctl snapshot save` (etcdctl writes to "<dest>.part" then renames on
// success).
var stalePartRe = regexp.MustCompile(`^etcd-snapshot-[0-9A-Za-z._-]+\.db\.part$`)

// removeStalePartFiles removes any leftover *.part files from a previous,
// interrupted save attempt before starting a new one.
func removeStalePartFiles(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !stalePartRe.MatchString(e.Name()) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if info, err := os.Lstat(p); err == nil && info.Mode().IsRegular() {
			_ = os.Remove(p)
		}
	}
}

// removeUnverifiedEntry Lstat's path and, if present, removes the entry itself
// (unlink semantics never follow a symlink to its target).
func removeUnverifiedEntry(path string) {
	if _, err := os.Lstat(path); err != nil {
		return
	}
	_ = os.Remove(path)
}

// cleanupAfterFailedSave removes both the partial-write artifact and an
// unverified destination after any non-taken outcome once the save step has
// started (error, timeout/kill, or a later verification failure).
func cleanupAfterFailedSave(dest string) {
	removeUnverifiedEntry(dest + ".part")
	removeUnverifiedEntry(dest)
}

// verifyAndFinalize Lstat's dest and requires it to be a non-empty regular file
// owned by ownerUID; on success it tightens the mode to 0600 if looser. It never
// chmods a path that failed the earlier checks.
func verifyAndFinalize(dest string, ownerUID int) (string, error) {
	info, err := os.Lstat(dest)
	if err != nil {
		return "", fmt.Errorf("verify snapshot %q: %w", dest, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("snapshot %q is not a regular file", dest)
	}
	if info.Size() <= 0 {
		return "", fmt.Errorf("snapshot %q is empty", dest)
	}
	uid, ok := uidOf(info)
	if !ok || uid != ownerUID {
		return "", fmt.Errorf("snapshot %q has an unexpected owner", dest)
	}
	if info.Mode().Perm() != 0o600 {
		if err := os.Chmod(dest, 0o600); err != nil {
			return "", fmt.Errorf("chmod snapshot %q 0600: %w", dest, err)
		}
	}
	return dest, nil
}

// snapshotFileRe matches a completed snapshot file name (any cluster/target).
var snapshotFileRe = regexp.MustCompile(`^etcd-snapshot-[0-9A-Za-z._-]+\.db$`)

// pruneOthers removes every OTHER completed snapshot file in dir, keeping only
// keep. Called only after a new snapshot is taken and verified.
func pruneOthers(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !snapshotFileRe.MatchString(e.Name()) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if p == keep {
			continue
		}
		if info, err := os.Lstat(p); err == nil && info.Mode().IsRegular() {
			_ = os.Remove(p)
		}
	}
}

// defaultSave runs the production `etcdctl snapshot save` via SaveCommand, with
// an empty environment, a bounded WaitDelay, and sanitized/capped error detail.
func defaultSave(ctx context.Context, etcdctlPath, rootPath, dest string) error {
	argv, env := SaveCommand(etcdctlPath, rootPath, dest)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv-only, no shell; path/flags contain no secret
	cmd.Env = env
	cmd.WaitDelay = saveWaitDelay
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, quotedSanitizedTail(out))
	}
	return nil
}

// quotedSanitizedTail sanitizes secret-shaped output, caps it to the last
// errorDetailCap bytes, and quotes it -- bounding and redacting anything that
// ends up in an operator-facing Result.Detail or log line.
func quotedSanitizedTail(out []byte) string {
	s := kubeadm.Sanitize(string(out))
	if len(s) > errorDetailCap {
		s = s[len(s)-errorDetailCap:]
	}
	return fmt.Sprintf("%q", s)
}
