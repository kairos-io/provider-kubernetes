// Package imageimport imports the control-plane container-image tarballs that
// the image build pre-bundles (ADR-16, revised by ADR-16-A2 / F-OPTBIND) into
// containerd's "k8s.io" namespace, so that kubeadm init finds the
// control-plane images locally and never pulls from a registry -- enabling a
// first boot to converge fully air-gapped.
//
// The bundle lives at hostexec.BundleDir (/system/provider-kubernetes/images),
// read-only OS-image content alongside the provider binary itself -- not
// under the persistent /opt. At every boot, Import opens the directory, the
// providers anchor and images.lock with a no-follow, owner/mode/device-
// checked walk (ADR-16-A2 decision 3), strictly parses and validates
// images.lock (decision 4), and imports ONLY the tarballs images.lock lists,
// one at a time, each re-checked structurally (decision 6) immediately before
// `ctr -n k8s.io images import -` reads it from fd 0 (decision 5/7) -- never a
// path, and never a *.tar glob. A file present in the bundle directory but not
// named by images.lock is never opened.
//
// It is invoked at boot by a systemd oneshot (ordered After=containerd.service,
// Before=kubelet.service) via the `agent-provider-kubernetes import-images`
// subcommand, bounded by the caller's context (production: 5 minutes). Every
// failure is reflected in the returned Result and logged per-entry; Import
// itself never panics and never blocks past its context (#4099-1).
package imageimport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/sirupsen/logrus"

	"github.com/kairos-io/provider-kubernetes/internal/hostexec"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
)

// Bounds on one tarball file and the whole bundle (ADR-16-A2 O-4).
const (
	minTarballSize    = 2560
	maxTarballSize    = 1 << 30 // 1 GiB
	maxBundleTotal    = 4 << 30 // 4 GiB
	maxReaddirEntries = 1024
	maxUnlistedWarn   = 16
	maxUnlistedName   = 128
	// errorDetailCap bounds sanitized ctr stderr retained in a log line.
	errorDetailCap = 1024 // 1 KiB
)

// Reason is the closed set of reasons an entry, or the whole bundle, was
// refused (ADR-16-A2 decision 8).
type Reason string

// The closed Reason enum. Every refusal/failure Import reports uses exactly
// one of these.
const (
	ReasonDirUnsafe    Reason = "dir-unsafe"
	ReasonAnchorUnsafe Reason = "anchor-unsafe"
	ReasonLockMissing  Reason = "lock-missing"
	ReasonLockInvalid  Reason = "lock-invalid"
	ReasonMissing      Reason = "missing"
	ReasonSymlink      Reason = "symlink"
	ReasonNotRegular   Reason = "not-regular"
	ReasonOwner        Reason = "owner"
	ReasonMode         Reason = "mode"
	ReasonSize         Reason = "size"
	ReasonDevice       Reason = "device"
	ReasonTarStructure Reason = "tar-structure"
	ReasonManifest     Reason = "manifest"
	ReasonConfigDigest Reason = "config-digest"
	ReasonCtrFailed    Reason = "ctr-failed"
	ReasonDeadline     Reason = "deadline"
)

// Outcome is the closed enum reported in the one summary line Import always
// logs last (ADR-16-A2 decision 8).
type Outcome string

const (
	OutcomeSuccess    Outcome = "success"
	OutcomePartial    Outcome = "partial"
	OutcomeRefused    Outcome = "refused"
	OutcomeFailed     Outcome = "failed"
	OutcomeNotBundled Outcome = "not-bundled"
	OutcomeVerified   Outcome = "verified"
)

// ExitCode maps o to the process exit code `import-images` returns: 0 for
// success/verified/not-bundled, 1 otherwise (usage errors are 2, decided by
// the caller before Import ever runs).
func ExitCode(o Outcome) int {
	switch o {
	case OutcomeSuccess, OutcomeVerified, OutcomeNotBundled:
		return 0
	default:
		return 1
	}
}

// Result summarizes one Import call.
type Result struct {
	Outcome  Outcome
	Entries  int
	Imported int
	Refused  int
	Failed   int
	Unlisted int
	ReadOnly bool
}

// StdinRunner executes `ctr` (or a fake, in tests) with a tarball wired
// directly as its stdin. kubeadm.ExecRunner (via kubeadm.CtrRunner())
// satisfies this.
type StdinRunner interface {
	RunStdin(ctx context.Context, stdin *os.File, args ...string) (kubeadm.Result, error)
}

// preparedBundle is what a successful walkBundle() returns: the open bundle
// directory (or, on a platform without a real implementation, nothing --
// walkBundle always errors there) ready for per-entry tarball opens, plus
// facts gathered once for the whole bundle. Defined here (not per-platform)
// so Import's orchestration logic needs no build tag; bundle_linux.go and
// bundle_other.go each provide the platform's walkBundle.
type preparedBundle interface {
	// LockData returns images.lock's raw bytes.
	LockData() []byte
	// OpenTarball opens name (a lock-listed tarball name) with the O-3 file
	// rules and returns it positioned at offset 0, or a Reason/detail.
	OpenTarball(name string, minSize, maxSize int64, missingReason Reason) (*os.File, Reason, string)
	// Unlisted returns every images-dir entry that is not images.lock and not
	// a key of lockedNames (bounded scan, O-3).
	Unlisted(lockedNames map[string]bool) []string
	// ReadOnly reports the images dir's fstatfs ST_RDONLY bit (informational
	// only -- never gates import).
	ReadOnly() bool
	// Close releases any resources walkBundle opened.
	Close()
}

// bundleError is a whole-bundle refusal (as opposed to a single entry): the
// directory walk, the anchor, or images.lock itself is unsafe or unreadable.
type bundleError struct {
	reason Reason
	detail string
}

func (e *bundleError) Error() string { return string(e.reason) + ": " + e.detail }

// errNotBundled is walkBundle's sentinel for the ONE case that is not a
// refusal: /system/provider-kubernetes itself does not exist (ADR-16-A2
// decision 3 -- this image variant simply did not bundle anything).
var errNotBundled = errors.New("not-bundled")

// Import imports every images.lock-listed tarball under hostexec.BundleDir
// into containerd's k8s.io namespace (ADR-16-A2). With verifyOnly it runs
// every check but never calls runner (`import-images --verify-only`): the
// returned Result's Imported is always 0 and Outcome is verified/refused/
// not-bundled. It always returns within ctx and never panics; every failure
// is reflected in the returned Result and logged per-entry (decision 8/O-9).
func Import(ctx context.Context, runner StdinRunner, verifyOnly bool) Result {
	return doImport(ctx, runner, verifyOnly, walkBundle)
}

// doImport is Import's testable core: walk is injected so unit tests can
// supply a fake preparedBundle without touching the filesystem.
func doImport(ctx context.Context, runner StdinRunner, verifyOnly bool, walk func() (preparedBundle, error)) Result {
	wr, err := walk()
	if errors.Is(err, errNotBundled) {
		logrus.Infof("image-import: %s is not present; nothing to import", hostexec.BundleDir)
		res := Result{Outcome: OutcomeNotBundled}
		logSummary(res)
		return res
	}
	if err != nil {
		reason, detail := ReasonDirUnsafe, err.Error()
		var be *bundleError
		if errors.As(err, &be) {
			reason, detail = be.reason, be.detail
		}
		logrus.Errorf("image-import: bundle refused reason=%s detail=%q", reason, detail)
		res := Result{Outcome: OutcomeRefused}
		logSummary(res)
		return res
	}
	defer wr.Close()

	lockDoc, lockErr := ParseLock(wr.LockData())
	if lockErr != nil {
		logrus.Errorf("image-import: bundle refused reason=%s detail=%q", ReasonLockInvalid, lockErr.Error())
		res := Result{Outcome: OutcomeRefused, ReadOnly: wr.ReadOnly()}
		logSummary(res)
		return res
	}

	lockedNames := make(map[string]bool, len(lockDoc.Images))
	for _, img := range lockDoc.Images {
		lockedNames[img.Tarball] = true
	}
	unlisted := wr.Unlisted(lockedNames)
	warnUnlisted(unlisted)

	res := Result{Entries: len(lockDoc.Images), ReadOnly: wr.ReadOnly(), Unlisted: len(unlisted)}

	var totalBytes int64
	for _, img := range lockDoc.Images {
		if ctx.Err() != nil {
			res.Failed++
			logrus.Errorf("image-import: failed %s ref=%s reason=%s detail=%q",
				img.Tarball, img.Ref, ReasonDeadline, "context deadline exceeded before this entry was checked")
			continue
		}

		f, reason, detail := wr.OpenTarball(img.Tarball, minTarballSize, maxTarballSize, ReasonMissing)
		if reason != "" {
			res.Refused++
			logrus.Errorf("image-import: refused %s ref=%s reason=%s detail=%q", img.Tarball, img.Ref, reason, detail)
			continue
		}

		info, statErr := f.Stat()
		if statErr != nil {
			_ = f.Close()
			res.Refused++
			logrus.Errorf("image-import: refused %s ref=%s reason=%s detail=%q", img.Tarball, img.Ref, ReasonSize, statErr.Error())
			continue
		}
		if totalBytes+info.Size() > maxBundleTotal {
			_ = f.Close()
			res.Refused++
			logrus.Errorf("image-import: refused %s ref=%s reason=%s detail=%q",
				img.Tarball, img.Ref, ReasonSize, fmt.Sprintf("bundle total would exceed %d bytes", maxBundleTotal))
			continue
		}

		if checkErr := CheckTar(f, img.Ref); checkErr != nil {
			_ = f.Close()
			res.Refused++
			logrus.Errorf("image-import: refused %s ref=%s reason=%s detail=%q",
				img.Tarball, img.Ref, reasonForTarCheckErr(checkErr), checkErr.Error())
			continue
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			_ = f.Close()
			res.Refused++
			logrus.Errorf("image-import: refused %s ref=%s reason=%s detail=%q",
				img.Tarball, img.Ref, ReasonTarStructure, fmt.Sprintf("seek to 0: %v", err))
			continue
		}
		totalBytes += info.Size()

		if verifyOnly {
			_ = f.Close()
			continue
		}

		runRes, runErr := runner.RunStdin(ctx, f, "-n", "k8s.io", "images", "import", "-")
		_ = f.Close()
		if runErr != nil {
			res.Failed++
			logrus.Errorf("image-import: failed %s ref=%s reason=%s detail=%q",
				img.Tarball, img.Ref, ReasonCtrFailed, capDetail(kubeadm.Sanitize(runRes.Stderr)))
			continue
		}
		res.Imported++
		logrus.Infof("image-import: imported %s ref=%s", img.Tarball, img.Ref)
	}

	if !verifyOnly && res.Imported == res.Entries && res.Refused == 0 && res.Failed == 0 {
		logrus.Infof("image-import: imported %d tarball(s) from %s", res.Imported, hostexec.BundleDir)
	}

	res.Outcome = computeOutcome(res, verifyOnly)
	logSummary(res)
	return res
}

// reasonForTarCheckErr maps a CheckTar error to its closed Reason via
// errors.Is, defaulting to tar-structure for anything unrecognized (never
// happens in practice: CheckTar only ever returns one of the three).
func reasonForTarCheckErr(err error) Reason {
	switch {
	case errors.Is(err, ErrManifest):
		return ReasonManifest
	case errors.Is(err, ErrConfigDigest):
		return ReasonConfigDigest
	default:
		return ReasonTarStructure
	}
}

// computeOutcome applies decision 8's outcome table.
func computeOutcome(res Result, verifyOnly bool) Outcome {
	if verifyOnly {
		if res.Refused > 0 || res.Failed > 0 {
			return OutcomeRefused
		}
		return OutcomeVerified
	}
	switch {
	case res.Imported == res.Entries && res.Refused == 0 && res.Failed == 0:
		return OutcomeSuccess
	case res.Imported == 0 && res.Refused > 0:
		return OutcomeRefused
	case res.Imported == 0 && res.Failed > 0:
		return OutcomeFailed
	case res.Imported > 0 && (res.Refused > 0 || res.Failed > 0):
		return OutcomePartial
	default:
		return OutcomeSuccess
	}
}

// warnUnlisted logs at most maxUnlistedWarn of unlisted's names, each capped
// to maxUnlistedName bytes and %q-quoted (O-3/O-9). It never opens any of
// them.
func warnUnlisted(unlisted []string) {
	n := len(unlisted)
	if n > maxUnlistedWarn {
		n = maxUnlistedWarn
	}
	for _, name := range unlisted[:n] {
		if len(name) > maxUnlistedName {
			name = name[:maxUnlistedName]
		}
		logrus.Warnf("image-import: unlisted %q not imported", name)
	}
}

// capDetail bounds a sanitized ctr stderr string to errorDetailCap bytes
// (from the tail, so the most recent output survives truncation).
func capDetail(s string) string {
	if len(s) > errorDetailCap {
		s = s[len(s)-errorDetailCap:]
	}
	return s
}

// logSummary emits ADR-16-A2 decision 8's exactly-one, always-last summary
// line.
func logSummary(res Result) {
	logrus.Infof("image-import: summary outcome=%s entries=%d imported=%d refused=%d failed=%d unlisted=%d readonly=%t dir=%s",
		res.Outcome, res.Entries, res.Imported, res.Refused, res.Failed, res.Unlisted, res.ReadOnly, hostexec.BundleDir)
}
