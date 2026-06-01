package kubeadm

import (
	"context"
	"fmt"
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
