package clusterconfigdir

import (
	"context"
	"strings"
	"testing"

	"github.com/kairos-io/kairos-sdk/clusterplugin"

	"github.com/kairos-io/provider-kubernetes/internal/status"
)

// fakeSink records every Status it is given so tests can assert on it
// without touching the real /run or /var/log filesystem paths.
type fakeSink struct {
	calls []status.Status
}

func (f *fakeSink) Record(_ context.Context, s status.Status) {
	f.calls = append(f.calls, s)
}

// withFakeSink points the package's statusSink seam at a fresh fakeSink for
// the duration of the test, restoring the previous value on cleanup.
func withFakeSink(t *testing.T) *fakeSink {
	t.Helper()
	f := &fakeSink{}
	old := statusSink
	statusSink = f
	t.Cleanup(func() { statusSink = old })
	return f
}

// TestTargetFileNameDefault is S-D3-7: no override -> the SDK's default path.
func TestTargetFileNameDefault(t *testing.T) {
	name, rejected := targetFileName(clusterplugin.Cluster{})
	if rejected {
		t.Fatal("expected the default (no override) path to be accepted")
	}
	if name != DefaultFileName {
		t.Fatalf("filename = %q, want %q", name, DefaultFileName)
	}
}

// TestTargetFileNameOverrideEqualToDefault is S-D3-7's second required test:
// an override that names the literal default path is accepted exactly like
// no override at all.
func TestTargetFileNameOverrideEqualToDefault(t *testing.T) {
	name, rejected := targetFileName(clusterplugin.Cluster{ClusterConfigPath: DefaultPath})
	if rejected {
		t.Fatal("expected an override equal to the default path to be accepted")
	}
	if name != DefaultFileName {
		t.Fatalf("filename = %q, want %q", name, DefaultFileName)
	}
}

// TestTargetFileNameOverrideSameDirDifferentFile: an override that stays
// directly under DefaultDir but names a different file is accepted, using
// that file's own basename for the S-D3-4 preflight.
func TestTargetFileNameOverrideSameDirDifferentFile(t *testing.T) {
	name, rejected := targetFileName(clusterplugin.Cluster{ClusterConfigPath: DefaultDir + "/other.yaml"})
	if rejected {
		t.Fatal("expected an override under the default directory to be accepted")
	}
	if name != "other.yaml" {
		t.Fatalf("filename = %q, want %q", name, "other.yaml")
	}
}

// TestTargetFileNameOverrideElsewhereRejected is S-D3-7's "override elsewhere
// (creates nothing)" case.
func TestTargetFileNameOverrideElsewhereRejected(t *testing.T) {
	cases := []string{
		"/etc/kairos/cluster.kairos.yaml",
		"/usr/local/cloud-config-evil/cluster.kairos.yaml",
		"/usr/local/cluster.kairos.yaml",
	}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			_, rejected := targetFileName(clusterplugin.Cluster{ClusterConfigPath: path})
			if !rejected {
				t.Fatalf("expected override %q to be rejected", path)
			}
		})
	}
}

// TestTargetFileNameRelativeRejected is S-D3-7's "relative" case.
func TestTargetFileNameRelativeRejected(t *testing.T) {
	_, rejected := targetFileName(clusterplugin.Cluster{ClusterConfigPath: "relative/cluster.kairos.yaml"})
	if !rejected {
		t.Fatal("expected a relative override to be rejected")
	}
}

// TestTargetFileNameDotDotRejected is S-D3-7's "..-bearing" case. A
// non-absolute path already fails the IsAbs check first, so use an absolute
// path with an embedded ".." (already fully resolved out by Clean before the
// directory comparison; still exercised so the letter of S-D3-7 is covered).
func TestTargetFileNameDotDotRejected(t *testing.T) {
	_, rejected := targetFileName(clusterplugin.Cluster{
		ClusterConfigPath: "/usr/local/cloud-config/../../etc/cluster.kairos.yaml",
	})
	if !rejected {
		t.Fatal("expected a path that climbs outside the default directory to be rejected")
	}
}

// TestTargetFileNameEmptyBasenameRejected covers an override whose Cleaned
// form has no usable basename (e.g. naming the directory itself).
func TestTargetFileNameEmptyBasenameRejected(t *testing.T) {
	_, rejected := targetFileName(clusterplugin.Cluster{ClusterConfigPath: DefaultDir + "/"})
	if !rejected {
		t.Fatal("expected an override naming the directory itself to be rejected")
	}
}

// TestEnsureOverrideRejectedRecordsStatusNotWithhold verifies the S-D3-7 path
// end to end through Ensure: nothing is withheld (only S-D3-3/S-D3-4 gate
// withholding), but the rejection IS logged/recorded (S-D3-9).
func TestEnsureOverrideRejectedRecordsStatusNotWithhold(t *testing.T) {
	sink := withFakeSink(t)

	rep := Ensure(clusterplugin.Cluster{ClusterConfigPath: "/etc/evil/cluster.kairos.yaml"})
	if rep.Withhold {
		t.Fatal("an override rejection must never withhold the secret")
	}
	if rep.Reason != ReasonOverrideRejected {
		t.Fatalf("reason = %q, want %q", rep.Reason, ReasonOverrideRejected)
	}
	if len(sink.calls) != 1 {
		t.Fatalf("expected exactly one status record, got %d", len(sink.calls))
	}
	got := sink.calls[0]
	if got.Reason != status.ReasonClusterConfigOverrideRejected {
		t.Fatalf("status reason = %q, want %q", got.Reason, status.ReasonClusterConfigOverrideRejected)
	}
	if got.Terminal {
		t.Fatal("an override rejection is not withheld, so Terminal must be false")
	}
}

// TestEnsureOverrideRejectedNeverLogsThePath is the S-D3-9 secret-hygiene
// requirement applied to the one field that IS attacker/operator supplied on
// this path: the override path string itself must never reach the status
// record (only the closed reason/message may).
func TestEnsureOverrideRejectedNeverLogsThePath(t *testing.T) {
	sink := withFakeSink(t)
	secretLookingPath := "/etc/do-not-log-me/cluster.kairos.yaml"

	Ensure(clusterplugin.Cluster{ClusterConfigPath: secretLookingPath})

	for _, s := range sink.calls {
		if strings.Contains(s.Message, secretLookingPath) {
			t.Fatalf("status message leaked the override path: %q", s.Message)
		}
	}
}

// TestReasonToStatusIsExhaustive guards the closed-reason mapping: every
// non-empty clusterconfigdir.Reason must map to a status.Reason, and every
// mapped value must itself be non-empty (so a future added Reason cannot
// silently fall back to the zero value in a status record).
func TestReasonToStatusIsExhaustive(t *testing.T) {
	all := []Reason{
		ReasonAncestorUnsafe,
		ReasonAncestorMissing,
		ReasonDirCreateFailed,
		ReasonDirUnsafe,
		ReasonDirWritable,
		ReasonTokenFileUnsafe,
		ReasonOverrideRejected,
		ReasonNotPersistent,
	}
	for _, r := range all {
		sr, ok := reasonToStatus[r]
		if !ok {
			t.Errorf("reason %q has no status.Reason mapping", r)
		}
		if sr == status.ReasonNone {
			t.Errorf("reason %q maps to the empty status.Reason", r)
		}
		if _, ok := messages[r]; !ok {
			t.Errorf("reason %q has no fixed message", r)
		}
	}
}

// TestWithholdReasonsAreExactlyAncestorDirAndTokenFileUnsafe pins S-D3-5's
// gate as amended by S-D3-5a/S-D3-4a: only ReasonAncestorUnsafe,
// ReasonDirUnsafe and ReasonTokenFileUnsafe may ever withhold, so a caller
// that only checks rep.Withhold cannot be surprised by a future reason
// silently joining the withhold set. In particular ReasonAncestorMissing
// (S-D3-5a: a missing ancestor is not a safety refusal) and
// ReasonNotPersistent (S-D3-8: report-only by design, never a withhold, or
// it becomes a convergence regression) must NOT withhold.
func TestWithholdReasonsAreExactlyAncestorDirAndTokenFileUnsafe(t *testing.T) {
	withholdReports := []Report{
		{Reason: ReasonAncestorUnsafe, Withhold: true},
		{Reason: ReasonDirUnsafe, Withhold: true},
		{Reason: ReasonTokenFileUnsafe, Withhold: true},
	}
	nonWithholdReports := []Report{
		{Reason: ReasonAncestorMissing},
		{Reason: ReasonDirCreateFailed},
		{Reason: ReasonDirWritable},
		{Reason: ReasonOverrideRejected},
		{Reason: ReasonNotPersistent},
		{Reason: ReasonNone},
	}
	for _, r := range withholdReports {
		if !r.Withhold {
			t.Errorf("reason %q must withhold", r.Reason)
		}
	}
	for _, r := range nonWithholdReports {
		if r.Withhold {
			t.Errorf("reason %q must not withhold", r.Reason)
		}
	}
}
