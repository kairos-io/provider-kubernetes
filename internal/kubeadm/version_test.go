package kubeadm

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

type fakeRunner struct {
	out  string
	err  error
	args []string
}

func (f *fakeRunner) Run(_ context.Context, args ...string) (Result, error) {
	f.args = args
	return Result{Stdout: f.out}, f.err
}

func TestDetectVersion(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		fr := &fakeRunner{out: "v1.34.2\n"}
		v, err := DetectVersion(context.Background(), fr)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != "v1.34.2" {
			t.Fatalf("got %q want v1.34.2", v)
		}
	})
	t.Run("unparseable", func(t *testing.T) {
		fr := &fakeRunner{out: "garbage"}
		if _, err := DetectVersion(context.Background(), fr); err == nil {
			t.Fatal("expected error for unparseable version")
		}
	})
	t.Run("runner error", func(t *testing.T) {
		fr := &fakeRunner{err: errors.New("boom")}
		if _, err := DetectVersion(context.Background(), fr); err == nil {
			t.Fatal("expected error when runner fails")
		}
	})
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name      string
		detected  string
		pinned    string
		want      string
		wantError bool
	}{
		{name: "supported no pin", detected: "v1.34.2", want: "v1.34.2"},
		{name: "supported matching pin", detected: "v1.35.0", pinned: "v1.35.3", want: "v1.35.0"},
		{name: "pin minor mismatch", detected: "v1.34.2", pinned: "v1.35.0", wantError: true},
		{name: "detected out of window", detected: "v1.30.0", wantError: true},
		{name: "detected invalid", detected: "nope", wantError: true},
		{name: "pin invalid", detected: "v1.34.0", pinned: "nope", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.detected, tt.pinned)
			if tt.wantError {
				if err == nil {
					t.Fatalf("expected error, got nil (result %q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestMinorAndIsSupported(t *testing.T) {
	if Minor("v1.34.7") != "1.34" {
		t.Fatalf("Minor failed: %q", Minor("v1.34.7"))
	}
	if !IsSupported("v1.36.0") {
		t.Fatal("expected 1.36 supported")
	}
	if IsSupported("v1.33.99") {
		t.Fatal("expected 1.33 unsupported")
	}
}

// TestSupportedWindowResolves locks the SupportedMinors window data itself under
// test: every minor in the window must validate, report supported, and resolve
// without a pin, and the minor immediately above the window must be rejected
// (fail loud, ADR-3). CI derives its image-build matrix from SupportedMinors, so
// keeping this slice honest keeps the matrix meaningful.
func TestSupportedWindowResolves(t *testing.T) {
	if len(SupportedMinors) == 0 {
		t.Fatal("SupportedMinors must not be empty")
	}
	for _, m := range SupportedMinors {
		v := "v" + m + ".0"
		if !IsSupported(v) {
			t.Errorf("%s should be supported (it is in SupportedMinors)", v)
		}
		if got, err := Resolve(v, ""); err != nil || got != v {
			t.Errorf("Resolve(%q, \"\") = %q, %v; want %q, nil", v, got, err, v)
		}
	}
	above := bumpMinor(t, SupportedMinors[len(SupportedMinors)-1], +1)
	if IsSupported("v" + above + ".0") {
		t.Errorf("minor %s is above the window and must be unsupported", above)
	}
	if _, err := Resolve("v"+above+".0", ""); err == nil {
		t.Errorf("Resolve must reject out-of-window minor %s", above)
	}
}

func bumpMinor(t *testing.T, mm string, delta int) string {
	t.Helper()
	parts := strings.SplitN(mm, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("malformed minor %q", mm)
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		t.Fatalf("malformed minor %q: %v", mm, err)
	}
	return parts[0] + "." + strconv.Itoa(n+delta)
}

func TestUpgradePath(t *testing.T) {
	cases := []struct {
		name      string
		cluster   string
		target    string
		wantDue   bool
		wantTgt   string
		wantError bool
	}{
		{name: "one minor up is due", cluster: "v1.34.8", target: "v1.35.5", wantDue: true, wantTgt: "v1.35.5"},
		{name: "one minor up into window", cluster: "v1.35.0", target: "v1.36.1", wantDue: true, wantTgt: "v1.36.1"},
		{name: "same minor not due", cluster: "v1.34.0", target: "v1.34.8", wantDue: false},
		{name: "skip-level refused", cluster: "v1.34.0", target: "v1.36.0", wantError: true},
		{name: "downgrade refused", cluster: "v1.35.0", target: "v1.34.0", wantError: true},
		{name: "target out of window refused", cluster: "v1.36.0", target: "v1.37.0", wantError: true},
		{name: "invalid cluster", cluster: "nope", target: "v1.35.0", wantError: true},
		{name: "invalid target", cluster: "v1.34.0", target: "nope", wantError: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := UpgradePath(c.cluster, c.target)
			if c.wantError {
				if err == nil {
					t.Fatalf("expected error for cluster=%s target=%s, got %+v", c.cluster, c.target, d)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d.Due != c.wantDue {
				t.Fatalf("Due = %v, want %v", d.Due, c.wantDue)
			}
			if d.Due && d.Target != c.wantTgt {
				t.Fatalf("Target = %q, want %q", d.Target, c.wantTgt)
			}
		})
	}
}
