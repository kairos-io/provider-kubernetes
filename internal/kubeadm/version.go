package kubeadm

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/mod/semver"
)

// SupportedMinors is the rolling window of supported Kubernetes minors (ADR-3:
// latest three in-support upstream minors). Update as the window rolls forward.
var SupportedMinors = []string{"1.34", "1.35", "1.36"}

// DetectVersion runs `kubeadm version -o short` and returns the parsed version
// (e.g. "v1.34.2"). The caller is responsible for bounding ctx.
func DetectVersion(ctx context.Context, r Runner) (string, error) {
	res, err := r.Run(ctx, "version", "-o", "short")
	if err != nil {
		return "", fmt.Errorf("detect kubeadm version: %w", err)
	}
	v := strings.TrimSpace(res.Stdout)
	if !semver.IsValid(v) {
		return "", fmt.Errorf("kubeadm reported an unparseable version %q", v)
	}
	return v, nil
}

// Minor returns the "major.minor" of a semver version, e.g. "1.34" for "v1.34.2".
func Minor(version string) string {
	return strings.TrimPrefix(semver.MajorMinor(version), "v")
}

// IsSupported reports whether version's minor is within SupportedMinors.
func IsSupported(version string) bool {
	m := Minor(version)
	for _, s := range SupportedMinors {
		if s == m {
			return true
		}
	}
	return false
}

// Resolve reconciles an operator-pinned target version against the detected
// kubeadm binary version (ADR-3 / OQ-5):
//   - the detected version must be valid and within the supported window;
//   - if pinned is non-empty, its minor MUST match the detected binary's minor,
//     otherwise it is a hard error (fail loud and early; no best-effort).
//
// It returns the version to target (the detected binary version) on success.
func Resolve(detected, pinned string) (string, error) {
	if !semver.IsValid(detected) {
		return "", fmt.Errorf("detected kubeadm version %q is not valid semver", detected)
	}
	if !IsSupported(detected) {
		return "", fmt.Errorf("kubeadm version %s (minor %s) is outside the supported window %v", detected, Minor(detected), SupportedMinors)
	}
	if pinned != "" {
		if !semver.IsValid(pinned) {
			return "", fmt.Errorf("pinned kubernetesVersion %q is not valid semver", pinned)
		}
		if Minor(pinned) != Minor(detected) {
			return "", fmt.Errorf("pinned kubernetesVersion %s (minor %s) does not match the installed kubeadm binary %s (minor %s)", pinned, Minor(pinned), detected, Minor(detected))
		}
	}
	return detected, nil
}

// UpgradeDecision is the result of evaluating a possible upgrade (ADR-12).
type UpgradeDecision struct {
	// Due is true when a minor upgrade from clusterVersion to Target should run.
	Due bool
	// Target is the version to upgrade to (the bundled binary / pinned version)
	// when Due; empty otherwise.
	Target string
}

// UpgradePath decides whether a cluster currently at clusterVersion should upgrade
// to targetVersion (the operator-pinned version, which by ADR-3 equals the bundled
// kubeadm binary version). It is a pure function (ADR-4 testability).
//
// It returns a TERMINAL error for any unsafe transition so the caller fails loud
// rather than attempting it (ADR-12 skew enforcement): a downgrade, a skip-level
// jump (more than one minor), a cross-major jump, or a target outside the supported
// window. A same-minor input yields Due=false (no minor upgrade; reboot-safe no-op).
// kubeadm only supports +1 minor per upgrade, so exactly one minor of distance is
// the only "due" case.
func UpgradePath(clusterVersion, targetVersion string) (UpgradeDecision, error) {
	if !semver.IsValid(clusterVersion) {
		return UpgradeDecision{}, fmt.Errorf("cluster version %q is not valid semver", clusterVersion)
	}
	if !semver.IsValid(targetVersion) {
		return UpgradeDecision{}, fmt.Errorf("target version %q is not valid semver", targetVersion)
	}
	if !IsSupported(targetVersion) {
		return UpgradeDecision{}, fmt.Errorf("refusing upgrade: target %s (minor %s) is outside the supported window %v", targetVersion, Minor(targetVersion), SupportedMinors)
	}
	if semver.Major(clusterVersion) != semver.Major(targetVersion) {
		return UpgradeDecision{}, fmt.Errorf("refusing cross-major change: cluster is %s, target is %s", clusterVersion, targetVersion)
	}

	switch semver.Compare(semver.MajorMinor(clusterVersion), semver.MajorMinor(targetVersion)) {
	case 0:
		// Same minor: no minor upgrade is due (patch-level handling is out of scope
		// here; a re-applied same-minor upgrade is intentionally a no-op).
		return UpgradeDecision{Due: false}, nil
	case 1:
		// Cluster minor is newer than target: downgrade.
		return UpgradeDecision{}, fmt.Errorf("refusing downgrade: cluster is %s, target is %s", clusterVersion, targetVersion)
	default:
		// Cluster minor is older than target: must be exactly one minor apart.
		cn, okc := minorNum(clusterVersion)
		tn, okt := minorNum(targetVersion)
		if !okc || !okt {
			return UpgradeDecision{}, fmt.Errorf("cannot parse minor of %q or %q", clusterVersion, targetVersion)
		}
		if tn-cn != 1 {
			return UpgradeDecision{}, fmt.Errorf("refusing skip-level upgrade: cluster is %s, target is %s; upgrade one minor at a time", clusterVersion, targetVersion)
		}
		return UpgradeDecision{Due: true, Target: targetVersion}, nil
	}
}

// minorNum extracts the integer minor component, e.g. 34 from "v1.34.2".
func minorNum(version string) (int, bool) {
	parts := strings.SplitN(Minor(version), ".", 2)
	if len(parts) != 2 {
		return 0, false
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, false
	}
	return n, true
}
