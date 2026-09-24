// Package clusterconfigdir implements the D-3 / F-UKIBOOT fix (security
// review 2026-09-18, PROJECT_CONTEXT.md "Security review (2026-09-18,
// security-architect): D-3 / F-UKIBOOT fix", conditions S-D3-1..S-D3-10).
//
// THE DEFECT: kairos-sdk@v0.5.0's clusterplugin.ClusterPlugin.onBoot opens
// /usr/local/cloud-config/cluster.kairos.yaml with
// os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600 (clusterplugin/plugin.go:59) and
// never creates the parent directory. On a UKI node the directory does not
// exist yet on the first boot after install (or after a state reset): the
// GRUB install path creates it as a chroot side effect at install time, but
// the UKI install/reset path never does. The open fails ENOENT, onBoot
// swallows it into response.Error, and immucore discards that too -- the
// node never bootstraps and nothing says why (see the F-UKIBOOT / D-3
// investigation block in PROJECT_CONTEXT.md).
//
// THE FIX (S-D3-1): a wrapper clusterplugin.ClusterProvider, composed in
// main.go, calls Ensure before calling the real provider.Provider. Ensure
// creates only the final path component ("cloud-config") under
// /usr/local with a Linux openat/mkdirat walk that never follows a symlink
// and never touches an ancestor (S-D3-2) -- deliberately NOT os.MkdirAll,
// which follows symlinks and returns nil when the final component already
// resolves to a directory (exactly the hole this closes).
//
// Withhold set (stated exactly once; the single source of truth for
// Report.Withhold -- see its doc comment, which must stay in sync with this
// one): Ensure reports Withhold=true when an ANCESTOR is unsafe (a symlink,
// not a directory, or not owned by uid 0 -- S-D3-2, amended S-D3-5a), when
// the EEXIST cloud-config directory itself is unsafe (S-D3-3), or when the
// token-file preflight is unsafe (S-D3-4, inverted to an allowlist by
// amendment S-D3-4a). A MISSING ancestor (ENOENT) does NOT withhold: the
// SDK's own open then fails the identical ENOENT and no token goes anywhere
// either way, so withholding would only replace one legible error with a
// second one (S-D3-5a). S-D3-8's not-persistent signal never withholds
// either: it stays detect-and-report, since treating it as a withhold would
// be a convergence regression on any layout where /usr/local is not a
// separate mount. When Withhold is true, the wrapper must return the inert
// YipConfig (provider.InertConfig) instead of the real one, so the SDK's
// unavoidable O_TRUNC write carries no cluster_token and no Commands
// (S-D3-5). Every non-empty Reason -- withheld or not -- is logged at error
// and recorded through the existing internal/status FileSink (S-D3-9);
// nothing beyond a closed Reason token ever reaches a log line or the status
// document -- never the token, the config blob, or a path taken from the
// cluster_config_path override.
//
// Phase classification (S-D3-9a, security review 2026-09-21, following the
// D-3/F-UKIBOOT VM run): the withhold set plus ReasonAncestorMissing and
// ReasonDirCreateFailed (failurePhaseReasons, in report()) MAY set the
// status document's Phase to Failed -- for those the SDK's own write will
// also fail this boot, so the node genuinely will not converge, and Failed
// is accurate. Every other reason (ReasonDirWritable, ReasonOverrideRejected,
// ReasonNotPersistent) is report-only and MUST NOT touch an existing Phase:
// the VM run found the original, unconditional Phase: PhaseFailed write
// driving a converged GRUB node's status from Converged to Failed while the
// boot converged fine. A diagnostic that downgrades a healthy node is a
// security-relevant defect in its own right -- it trains operators to
// ignore Failed. report()'s report-only branch reads whatever Status is
// already on record (status.ReadLatest) and preserves it
// (status.MergeReportOnly): Phase/Outcome/Membership/etc. pass through
// unchanged, only Reason/Message/timestamp move -- and when the record
// already carries a Reason, not even those move and nothing is written at
// all. Passing them through on top of a recorded failure relabelled it: the
// preserved Phase/Outcome/Terminal/Budget/LastAction described that failure
// while Reason and Message named this finding, which is the same defect the
// VM run caught, pointed the other way. See status.MergeReportOnly.
//
// Honest mode claim (S-D3-6, Q2 of the review): creating this directory
// 0700 root:root buys exactly one thing -- on the boot where we create it,
// the window between our mkdirat and the SDK's OpenFile cannot be
// pre-populated by a non-root actor. It is NOT a boundary against a
// root-equivalent or hostPath-capable actor, it protects nothing on boots
// 2..N (the platform's own 10_accounting.yaml widens the directory to 0770
// root:admin from the very next boot's initramfs stage onward, and we never
// chmod an existing directory back down), and it does not make
// cluster.kairos.yaml itself safe once it exists. Nothing in this package,
// its logs, or its docs may describe it as a security boundary.
package clusterconfigdir

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kairos-io/kairos-sdk/clusterplugin"
	"github.com/sirupsen/logrus"

	"github.com/kairos-io/provider-kubernetes/internal/status"
	"github.com/kairos-io/provider-kubernetes/version"
)

// DefaultDir mirrors the directory component of kairos-sdk@v0.5.0
// clusterplugin/plugin.go:14's clusterProviderCloudConfigFile constant
// ("/usr/local/cloud-config/cluster.kairos.yaml"). S-D3-10's pin-guard test
// (main_test.go) fails the build if the kairos-sdk dependency moves off
// v0.5.0, so this hardcoded value is re-verified by hand at any new pin.
const DefaultDir = "/usr/local/cloud-config"

// DefaultFileName is the token-file basename kairos-sdk@v0.5.0 writes at
// DefaultDir when clusterplugin.Cluster.ClusterConfigPath is unset.
const DefaultFileName = "cluster.kairos.yaml"

// DefaultPath is the full default path, byte-identical to kairos-sdk@v0.5.0's
// unexported clusterProviderCloudConfigFile constant.
const DefaultPath = DefaultDir + "/" + DefaultFileName

// Reason is clusterconfigdir's own closed reason enum (S-D3-9). Every
// non-empty value here has a corresponding status.Reason it is recorded as
// (see reasonToStatus) and a fixed, secret-free message (see messages).
type Reason string

const (
	// ReasonNone is the empty, nothing-to-report value.
	ReasonNone Reason = ""
	// ReasonAncestorUnsafe: S-D3-2, withhold criterion amended by S-D3-5a. An
	// ancestor of the target ("/", "usr" or "local") is a symlink, not a
	// directory, or not owned by uid 0. Withholds cluster_token: our walk
	// verified nothing about where that component leads, and the SDK's own
	// open resolves the full path by name straight through it.
	ReasonAncestorUnsafe Reason = "ancestor-unsafe"
	// ReasonAncestorMissing: S-D3-2/S-D3-5a. An ancestor of the target is
	// simply absent (ENOENT). Reported, never withheld: this is not a safety
	// refusal, the SDK's own open then fails the identical ENOENT, and no
	// token goes anywhere either way.
	ReasonAncestorMissing Reason = "ancestor-missing"
	// ReasonDirCreateFailed: mkdirat failed for a reason other than EEXIST.
	ReasonDirCreateFailed Reason = "dir-create-failed"
	// ReasonDirUnsafe: S-D3-3. The EEXIST target is not a root-owned
	// directory. Withholds cluster_token.
	ReasonDirUnsafe Reason = "dir-unsafe"
	// ReasonDirWritable: S-D3-3. The EEXIST target is a root-owned directory
	// but OTHER-writable (narrowed from group-or-other by the 2026-09-21 VM
	// run: kairos-init's 10_accounting.yaml chmods it 0770 root:admin ~130ms
	// after creation, so a group-writable check trips on the platform's own
	// expected state every boot from the second onward). Reported, never
	// refused.
	ReasonDirWritable Reason = "dir-writable"
	// ReasonTokenFileUnsafe: S-D3-4, inverted to an allowlist by amendment
	// S-D3-4a. The token-file preflight found anything other than ENOENT or
	// an intact root-owned 0600 regular file with nlink 1. Withholds
	// cluster_token.
	ReasonTokenFileUnsafe Reason = "token-file-unsafe"
	// ReasonOverrideRejected: S-D3-7. cluster_config_path was set but is not
	// directly under DefaultDir (or is relative, or ".."-bearing post-Clean).
	ReasonOverrideRejected Reason = "override-rejected"
	// ReasonNotPersistent: S-D3-8. /usr/local's device equals /'s: the
	// persistent mount is not actually there. Reported, never refused.
	ReasonNotPersistent Reason = "not-persistent"
)

// messages is the fixed, secret-free, reason-per-path text S-D3-9 requires.
// No field here is ever built from cluster config, a path, or an error
// string -- every value is a compile-time literal.
var messages = map[Reason]string{
	ReasonAncestorUnsafe:   "cluster-config directory ancestor is not root-owned or not a plain directory; withholding cluster_token",
	ReasonAncestorMissing:  "cluster-config directory ancestor is missing",
	ReasonDirCreateFailed:  "failed to create /usr/local/cloud-config",
	ReasonDirUnsafe:        "an existing /usr/local/cloud-config is not a root-owned directory; withholding cluster_token",
	ReasonDirWritable:      "/usr/local/cloud-config exists, is root-owned, but is other-writable",
	ReasonTokenFileUnsafe:  "an existing token-file target is unsafe; withholding cluster_token",
	ReasonOverrideRejected: "cluster_config_path override is not directly under /usr/local/cloud-config; ignoring it",
	ReasonNotPersistent:    "/usr/local is not a separate persistent mount; cluster_token would be written to ephemeral storage",
}

// reasonToStatus maps this package's Reason to the status package's own
// closed Reason enum (internal/status is the single schema owner; this map
// is the only place the two enums are tied together, so a rename on either
// side is caught at compile time).
var reasonToStatus = map[Reason]status.Reason{
	ReasonAncestorUnsafe:   status.ReasonClusterConfigAncestorUnsafe,
	ReasonAncestorMissing:  status.ReasonClusterConfigAncestorMissing,
	ReasonDirCreateFailed:  status.ReasonClusterConfigDirCreateFailed,
	ReasonDirUnsafe:        status.ReasonClusterConfigDirUnsafe,
	ReasonDirWritable:      status.ReasonClusterConfigDirWritable,
	ReasonTokenFileUnsafe:  status.ReasonClusterConfigTokenFileUnsafe,
	ReasonOverrideRejected: status.ReasonClusterConfigOverrideRejected,
	ReasonNotPersistent:    status.ReasonClusterConfigNotPersistent,
}

// Report is Ensure's result.
type Report struct {
	// Reason is ReasonNone on a clean run with nothing to report.
	Reason Reason
	// Withhold is true exactly when Reason is ReasonAncestorUnsafe,
	// ReasonDirUnsafe, or ReasonTokenFileUnsafe (S-D3-5, amended S-D3-5a/
	// S-D3-4a; see the package doc's "Withhold set" paragraph, the single
	// source of truth this must stay in sync with): the caller MUST return
	// the inert YipConfig instead of the real one. Every other non-empty
	// Reason (ReasonAncestorMissing, ReasonDirCreateFailed,
	// ReasonDirWritable, ReasonOverrideRejected, ReasonNotPersistent) is
	// report-only and never sets Withhold.
	Withhold bool
}

// ensureDir is implemented per-GOOS (clusterconfigdir_linux.go /
// clusterconfigdir_other.go): the S-D3-2/3/4/8 walk needs Linux-specific
// openat/mkdirat/fstatat semantics.

// statusSink is the destination for Ensure's status record (S-D3-9).
// Package-private test seam (same style as internal/provider's
// newDefaultStatusSink): production leaves this as status.NewFileSink(), the
// real /run + /var/log paths; tests point it at a fake sink or temp-dir
// paths so the write is verifiable without touching the host filesystem.
var statusSink status.StatusSink = status.NewFileSink()

// statusReader is report()'s S-D3-9a companion to statusSink: it reads
// whatever Status document is already on record (production: the real
// /run + /var/log paths, same order as statusSink writes them) so a
// report-only finding can preserve it via status.MergeReportOnly instead of
// overwriting it. Package-private test seam, same style as statusSink.
var statusReader = func() (status.Status, bool) {
	return status.ReadLatest([]string{status.StatusRunPath, status.StatusLogPath})
}

// failurePhaseReasons is the closed set of Reasons that MAY set the status
// document's Phase to Failed (S-D3-9a, security review 2026-09-21,
// following the VM run): the withhold set (ReasonAncestorUnsafe,
// ReasonDirUnsafe, ReasonTokenFileUnsafe) plus the two reasons where the
// SDK's own write will ALSO fail this boot, so the node genuinely will not
// converge (ReasonAncestorMissing, ReasonDirCreateFailed) -- reporting
// Failed for those is not a downgrade, it is accurate. Every other non-empty
// Reason (ReasonDirWritable, ReasonOverrideRejected, ReasonNotPersistent) is
// report-only and MUST NOT touch an existing Phase; see report()'s
// report-only branch.
var failurePhaseReasons = map[Reason]bool{
	ReasonAncestorUnsafe:  true,
	ReasonAncestorMissing: true,
	ReasonDirCreateFailed: true,
	ReasonDirUnsafe:       true,
	ReasonTokenFileUnsafe: true,
}

// Ensure is the S-D3-1 wrapper's entrypoint. It is invoked from main.go's
// ClusterProvider wrapper, which runs inside the SDK's onBoot path (called
// after config.Cluster != nil and strictly before the SDK's OpenFile at
// kairos-sdk@v0.5.0 clusterplugin/plugin.go:59) and nowhere else -- an
// init.provider.info or cluster.reset invocation never reaches this
// function, so it never runs during an image build probe.
//
// Ensure never blocks and never retries (#4099-1): every step is a single
// bounded syscall or, for the status write, a 2s-deadlined best-effort
// record through the existing internal/status FileSink. The write happens
// pre-pivot; whether /run (tmpfs) at that moment survives the later
// switch-root, or only the persistent /var/log mirror does, is established
// by the lead's VM run (S-D3-11(v)), not assumed here.
func Ensure(cluster clusterplugin.Cluster) Report {
	fileName, rejected := targetFileName(cluster)

	var rep Report
	if rejected {
		rep = Report{Reason: ReasonOverrideRejected}
	} else {
		rep = ensureDir(fileName)
	}

	report(cluster, rep)
	return rep
}

// targetFileName implements S-D3-7. cluster.ClusterConfigPath mirrors
// kairos-sdk@v0.5.0 clusterplugin/config.go:70-71 / plugin.go:52-56's
// override: when set, onBoot writes to that exact path instead of
// DefaultPath. That value arrives on the merged cloud-config kairos-agent
// scans from {"/oem","/usr/local/cloud-config"} -- a persistent,
// attacker-influenceable location after boot 1 -- so the rule is
// restrictive: act only when the override's directory, once Cleaned, is
// exactly DefaultDir. A non-absolute path, or a path whose Cleaned form
// still contains "..", is rejected outright. Rejection means "create
// nothing": it does not withhold the secret (S-D3-5 gates only on S-D3-3/
// S-D3-4), and the operator who set the override owns pre-creating their own
// directory safely.
func targetFileName(cluster clusterplugin.Cluster) (fileName string, rejected bool) {
	path := cluster.ClusterConfigPath
	if path == "" {
		return DefaultFileName, false
	}
	if !filepath.IsAbs(path) {
		return "", true
	}
	clean := filepath.Clean(path)
	// filepath.Clean fully resolves any ".." that stays within an absolute
	// path (it can never climb above "/"), so this can never trip for a
	// well-formed absolute input; kept for defense-in-depth exactly as
	// S-D3-7 specifies.
	if strings.Contains(clean, "..") {
		return "", true
	}
	dir := filepath.Clean(filepath.Dir(clean))
	if dir != DefaultDir {
		return "", true
	}
	base := filepath.Base(clean)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "", true
	}
	return base, false
}

// report logs every non-empty Reason at error with the closed message and
// records it through the existing internal/status FileSink (S-D3-9). It
// never blocks (2s deadline, same as every other status write in this
// codebase) and never includes anything beyond the closed reason/message
// pair: no token, no config blob, no path taken from the cluster_config_path
// override.
//
// S-D3-9a (security review 2026-09-21, following the D-3/F-UKIBOOT VM run):
// a report-only Reason (failurePhaseReasons[rep.Reason] == false) MUST NOT
// move an existing reconcile verdict backwards. The VM run watched this
// exact defect: a converged GRUB node's status went from Converged to
// Failed while the boot converged fine, because the original write always
// set Phase: PhaseFailed unconditionally. That branch below reads whatever
// is already on record (status.ReadLatest) and preserves it via
// status.MergeReportOnly -- Phase/Outcome/Membership/etc. pass through
// unchanged; only Reason/Message/timestamp move. Only the reasons in
// failurePhaseReasons (the withhold set plus the two "the SDK's write fails
// too" reasons) take the unconditional Phase: PhaseFailed path, because for
// those the node genuinely will not converge this boot, so Failed is
// accurate, not a downgrade.
func report(cluster clusterplugin.Cluster, rep Report) {
	if rep.Reason == ReasonNone {
		return
	}

	msg := messages[rep.Reason]
	logrus.Errorf("provider-kubernetes: cluster-config-dir: %s (reason=%s withhold=%t)", msg, rep.Reason, rep.Withhold)

	statusReason := reasonToStatus[rep.Reason]
	now := time.Now().UTC().Format(time.RFC3339)
	bootID := readBootID()

	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if !failurePhaseReasons[rep.Reason] {
		prev, existing := statusReader()
		merged, record := status.MergeReportOnly(prev, existing, statusReason, msg, bootID, version.Version, now)
		if !record {
			logrus.Infof("provider-kubernetes: cluster-config-dir: leaving reason=%s on record; this finding stays in the log only", prev.Reason)
			return
		}
		statusSink.Record(sctx, merged)
		return
	}

	statusSink.Record(sctx, status.Status{
		APIVersion: status.APIVersion,
		Phase:      status.PhaseFailed,
		Role:       string(cluster.Role),
		Outcome:    status.OutcomeFailure,
		Reason:     statusReason,
		Terminal:   rep.Withhold,
		Message:    msg,
		UpdatedAt:  now,
		BootID:     bootID,
		Version:    version.Version,
	})
}

// readBootID reads /proc/sys/kernel/random/boot_id best-effort. Mirrors
// internal/provider/run.go's helper of the same name (unexported there;
// duplicated here rather than imported to avoid a
// provider->clusterconfigdir dependency edge, since main.go already imports
// both independently). Returns "" if the file cannot be read (containers,
// test environments, non-Linux) -- never treated as an error.
func readBootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
