//go:build e2e

package e2e

// Container-free tests for the failure-path diagnostics in nodecontainer.go.
// These run on every e2e invocation precisely because the code under test only
// ever runs when something else has already failed: a silent regression here is
// invisible until the next outage, which is exactly how the original bug
// survived (it gated on err == nil and printed a bare header with no body).

import (
	"errors"
	"strings"
	"testing"
)

func TestFormatDiagSectionReportsEveryOutcome(t *testing.T) {
	boom := errors.New("exit status 1")

	tests := []struct {
		name        string
		out         string
		err         error
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "output is reproduced under its label",
			out:         "containerd.service loaded failed failed\n",
			wantContain: []string{"--- failed units:", "containerd.service loaded failed failed"},
			wantAbsent:  []string{"UNAVAILABLE", "(empty)"},
		},
		{
			// The original bug: a successful capture with nothing in it must be
			// distinguishable from a capture that never happened.
			name:        "empty success is called out, not skipped",
			out:         "",
			wantContain: []string{"--- failed units:", "(empty)"},
			wantAbsent:  []string{"UNAVAILABLE"},
		},
		{
			name:        "error with no output says why",
			err:         boom,
			wantContain: []string{"--- failed units:", "UNAVAILABLE", "exit status 1"},
			wantAbsent:  []string{"(empty)", "partial output"},
		},
		{
			// journalctl can write a usable tail and still exit non-zero; losing
			// that body would discard the only evidence we have.
			name:        "error with output keeps the partial body",
			out:         "Nov 17 21:20:15 containerd[412]: panic: runtime error\n",
			err:         boom,
			wantContain: []string{"UNAVAILABLE", "exit status 1", "partial output", "panic: runtime error"},
		},
		{
			name:        "whitespace-only output counts as empty",
			out:         "\n\n",
			wantContain: []string{"(empty)"},
			wantAbsent:  []string{"UNAVAILABLE"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatDiagSection("failed units", tc.out, tc.err)
			for _, want := range tc.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("section is missing %q\ngot:\n%s", want, got)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("section should not contain %q\ngot:\n%s", absent, got)
				}
			}
			if strings.HasSuffix(got, "\n") {
				t.Errorf("section should not end in a newline (the caller joins): %q", got)
			}
		})
	}
}

// The ordering and composition of the sources is the actual contract: docker
// logs on its own is what failed us, so the journal sources must be present and
// must come first, and PID 1's output must still be collected for the case where
// systemd never came up at all.
func TestDiagSourcesCoverTheJournalAndPID1(t *testing.T) {
	srcs := diagSources("deadbeef")
	if len(srcs) < 2 {
		t.Fatalf("expected several diagnostic sources, got %d", len(srcs))
	}

	var labels []string
	journalBefore, sawDockerLogs, sawContainerdUnit := false, false, false
	sawQueuedJobs := false
	for i, s := range srcs {
		labels = append(labels, s.label)
		if s.label == "" {
			t.Errorf("source %d has an empty label", i)
		}
		if len(s.args) == 0 {
			t.Fatalf("source %q has no argv", s.label)
		}
		joined := strings.Join(s.args, " ")

		switch s.args[0] {
		case "exec":
			if s.args[1] != "deadbeef" {
				t.Errorf("exec source %q targets %q, want the container id", s.label, s.args[1])
			}
			if strings.Contains(joined, "journalctl") && !sawDockerLogs {
				journalBefore = true
			}
			if strings.Contains(joined, "journalctl") && strings.Contains(joined, "-u containerd") {
				sawContainerdUnit = true
			}
			if strings.Contains(joined, "list-jobs") {
				sawQueuedJobs = true
			}
		case "logs":
			sawDockerLogs = true
			if s.args[len(s.args)-1] != "deadbeef" {
				t.Errorf("logs source %q does not end with the container id: %v", s.label, s.args)
			}
		default:
			t.Errorf("source %q uses unexpected docker subcommand %q", s.label, s.args[0])
		}

		// argv only, never a shell (ADR-1): nothing may smuggle in shell syntax.
		for _, a := range s.args {
			if strings.ContainsAny(a, "|;&$`") {
				t.Errorf("source %q argv element %q contains shell metacharacters", s.label, a)
			}
		}
	}

	if !sawQueuedJobs {
		t.Errorf("no source runs `systemctl list-jobs`; a readiness timeout usually means the unit never\n"+
			"started, and list-jobs is the only source that names what it is queued behind\nlabels: %v", labels)
	}
	if !sawContainerdUnit {
		t.Errorf("no source reads the containerd unit journal; that is the wait that times out\nlabels: %v", labels)
	}
	if !sawDockerLogs {
		t.Errorf("no source reads PID 1 stdout/stderr; needed when systemd never starts\nlabels: %v", labels)
	}
	if !journalBefore {
		t.Errorf("journal sources must precede docker logs, which is the least informative\nlabels: %v", labels)
	}
}
