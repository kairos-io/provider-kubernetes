package kubeadm

import (
	"context"
	"errors"
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
