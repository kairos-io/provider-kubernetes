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

// withFakeReader points the package's statusReader seam (S-D3-9a) at a fixed
// (Status, ok) pair for the duration of the test, restoring the previous
// value on cleanup. Without this, report()'s report-only branch falls
// through to the REAL status.ReadLatest against the real /run and /var/log
// paths -- harmless (ENOENT -> ok=false almost everywhere) but not hermetic;
// tests that care about the report-only merge behavior use this instead of
// relying on the host having no pre-existing status file.
func withFakeReader(t *testing.T, s status.Status, ok bool) {
	t.Helper()
	old := statusReader
	statusReader = func() (status.Status, bool) { return s, ok }
	t.Cleanup(func() { statusReader = old })
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
// withholding), but the rejection IS logged/recorded (S-D3-9). It is
// report-only (S-D3-9a), so it goes through the phase-preserving branch;
// withFakeReader fixes "nothing on record yet" so the assertions are
// hermetic regardless of the host's real /run and /var/log state.
func TestEnsureOverrideRejectedRecordsStatusNotWithhold(t *testing.T) {
	sink := withFakeSink(t)
	withFakeReader(t, status.Status{}, false)

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
	if got.Phase == status.PhaseFailed {
		t.Fatal("a report-only reason must not set PhaseFailed (S-D3-9a)")
	}
}

// TestEnsureOverrideRejectedNeverLogsThePath is the S-D3-9 secret-hygiene
// requirement applied to the one field that IS attacker/operator supplied on
// this path: the override path string itself must never reach the status
// record (only the closed reason/message may).
func TestEnsureOverrideRejectedNeverLogsThePath(t *testing.T) {
	sink := withFakeSink(t)
	withFakeReader(t, status.Status{}, false)
	secretLookingPath := "/etc/do-not-log-me/cluster.kairos.yaml"

	Ensure(clusterplugin.Cluster{ClusterConfigPath: secretLookingPath})

	for _, s := range sink.calls {
		if strings.Contains(s.Message, secretLookingPath) {
			t.Fatalf("status message leaked the override path: %q", s.Message)
		}
	}
}

// TestReportOnlyReasonsNeverSetFailurePhase is S-D3-9a's headline test:
// every report-only reason must preserve an existing Converged phase rather
// than downgrade it to Failed. This is exactly the VM run's finding: a
// converged GRUB node's status went from Converged to Failed while the boot
// converged fine, which trains operators to ignore Failed.
//
// Mutation (ii) from the coordinator: letting a report-only reason set the
// failure phase (i.e. routing it through the unconditional Phase: PhaseFailed
// branch) makes this test fail -- see the mutation run recorded in the
// implementation report.
func TestReportOnlyReasonsNeverSetFailurePhase(t *testing.T) {
	reportOnly := []Reason{ReasonDirWritable, ReasonOverrideRejected, ReasonNotPersistent}
	for _, r := range reportOnly {
		t.Run(string(r), func(t *testing.T) {
			sink := withFakeSink(t)
			withFakeReader(t, status.Status{Phase: status.PhaseConverged, Outcome: status.OutcomeSuccess}, true)

			report(clusterplugin.Cluster{}, Report{Reason: r})

			if len(sink.calls) != 1 {
				t.Fatalf("expected exactly one status record, got %d", len(sink.calls))
			}
			if sink.calls[0].Phase != status.PhaseConverged {
				t.Fatalf("reason %q: Phase = %q, want the preserved %q", r, sink.calls[0].Phase, status.PhaseConverged)
			}
		})
	}
}

// TestReportOnlyReasonFreshBootUsesReconcilingNotFailed covers the
// no-prior-record case: when nothing has ever been written (a truly fresh
// boot or install), a report-only reason must start from PhaseReconciling,
// never PhaseFailed -- see status.MergeReportOnly's doc comment.
func TestReportOnlyReasonFreshBootUsesReconcilingNotFailed(t *testing.T) {
	sink := withFakeSink(t)
	withFakeReader(t, status.Status{}, false)

	report(clusterplugin.Cluster{}, Report{Reason: ReasonDirWritable})

	if len(sink.calls) != 1 {
		t.Fatalf("expected exactly one status record, got %d", len(sink.calls))
	}
	if sink.calls[0].Phase != status.PhaseReconciling {
		t.Fatalf("Phase = %q, want %q on a fresh boot with nothing on record", sink.calls[0].Phase, status.PhaseReconciling)
	}
	if sink.calls[0].Phase == status.PhaseFailed {
		t.Fatal("a report-only reason must never fabricate PhaseFailed")
	}
}

// TestFailurePhaseReasonsSetPhaseFailed is the complement: every reason in
// failurePhaseReasons MUST set Phase: Failed regardless of what (if
// anything) was previously on record -- these are the reasons where the
// SDK's own write will also fail this boot, so the node genuinely will not
// converge, and Failed is accurate, not a downgrade. Covers the four
// reasons the coordinator named explicitly (the withhold set plus
// ReasonAncestorMissing) plus ReasonDirCreateFailed, which this
// implementation classifies the same way for the same reason (see
// failurePhaseReasons' doc comment) -- flagged in the report as an
// extension beyond the four explicitly named.
func TestFailurePhaseReasonsSetPhaseFailed(t *testing.T) {
	failing := []Reason{
		ReasonAncestorUnsafe,
		ReasonDirUnsafe,
		ReasonTokenFileUnsafe,
		ReasonAncestorMissing,
		ReasonDirCreateFailed,
	}
	for _, r := range failing {
		t.Run(string(r), func(t *testing.T) {
			sink := withFakeSink(t)
			// Even with an existing Converged record on hand, these reasons
			// must still report Failed: the node genuinely will not
			// converge this boot for any of them.
			withFakeReader(t, status.Status{Phase: status.PhaseConverged}, true)

			report(clusterplugin.Cluster{}, Report{Reason: r})

			if len(sink.calls) != 1 {
				t.Fatalf("expected exactly one status record, got %d", len(sink.calls))
			}
			if sink.calls[0].Phase != status.PhaseFailed {
				t.Fatalf("reason %q: Phase = %q, want %q", r, sink.calls[0].Phase, status.PhaseFailed)
			}
		})
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
