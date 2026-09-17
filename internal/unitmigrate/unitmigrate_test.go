package unitmigrate

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

// TestFrozenHashesMatchFixtures is S19-11's gate: every hash in the FROZEN
// table must equal sha256 of the historical bytes checked into testdata/, so
// a typo'd or hand-edited hash can never silently widen (or narrow) what
// Migrate is willing to delete.
func TestFrozenHashesMatchFixtures(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		fixture string
	}{
		{"containerd.service", pathContainerdFragment, "testdata/containerd.service"},
		{"kubelet.service", pathKubeletFragment, "testdata/kubelet.service"},
		{"10-kubeadm.conf", pathKubeadmDropin, "testdata/10-kubeadm.conf"},
		{"image-import v0.3.0 (1127 B)", pathImportFragment, "testdata/provider-kubernetes-image-import.service.1127"},
		{"image-import #36 (1630 B)", pathImportFragment, "testdata/provider-kubernetes-image-import.service.1630"},
	}

	seen := make(map[string]map[string]bool, len(frozenHashes))
	for path, set := range frozenHashes {
		seen[path] = make(map[string]bool, len(set))
	}

	for _, c := range cases {
		data, err := os.ReadFile(c.fixture)
		if err != nil {
			t.Fatalf("%s: read fixture: %v", c.name, err)
		}
		sum := sha256.Sum256(data)
		hexSum := hex.EncodeToString(sum[:])
		if !isFrozen(c.path, hexSum) {
			t.Errorf("%s: sha256(%s) = %s is not in frozenHashes[%q]; table and fixtures have drifted",
				c.name, c.fixture, hexSum, c.path)
		}
		seen[c.path][hexSum] = true
	}

	// The reverse direction: every hash actually in the table must be backed
	// by a fixture (otherwise a hash could be added by hand with no
	// reproducing bytes, which is exactly the mistake this test exists to catch).
	for path, set := range frozenHashes {
		for hexSum := range set {
			if !seen[path][hexSum] {
				t.Errorf("frozenHashes[%q] contains %s with no matching testdata fixture in this test's case table", path, hexSum)
			}
		}
	}
}

func TestExitCode(t *testing.T) {
	cases := []struct {
		outcome Outcome
		want    int
	}{
		{OutcomeClean, 0},
		{OutcomeMigrated, 0},
		{OutcomeKeptModified, 1},
		{OutcomeFailed, 1},
	}
	for _, c := range cases {
		if got := ExitCode(c.outcome); got != c.want {
			t.Errorf("ExitCode(%s) = %d, want %d", c.outcome, got, c.want)
		}
	}
}

// TestIsFrozenRejectsUnknownPathsAndHashes guards against a path/hash mix-up
// (e.g. checking a hash from one unit against another's frozen set).
func TestIsFrozenRejectsUnknownPathsAndHashes(t *testing.T) {
	if isFrozen("/etc/systemd/system/does-not-exist.service", "deadbeef") {
		t.Error("isFrozen matched an unknown path")
	}
	if isFrozen(pathContainerdFragment, "deadbeef") {
		t.Error("isFrozen matched an unknown hash for a known path")
	}
	// Cross-contamination: kubelet's frozen hash must not validate containerd's path.
	for hexSum := range frozenHashes[pathKubeletFragment] {
		if isFrozen(pathContainerdFragment, hexSum) {
			t.Errorf("kubelet.service hash %s incorrectly validated against containerd.service's path", hexSum)
		}
	}
}

func TestWantsLinkTargetIsAbsoluteFragmentPath(t *testing.T) {
	cases := map[string]string{
		nameContainerd: "/etc/systemd/system/containerd.service",
		nameKubelet:    "/etc/systemd/system/kubelet.service",
		nameImport:     "/etc/systemd/system/provider-kubernetes-image-import.service",
	}
	for name, want := range cases {
		if got := wantsLinkTarget(name); got != want {
			t.Errorf("wantsLinkTarget(%q) = %q, want %q", name, got, want)
		}
	}
}
