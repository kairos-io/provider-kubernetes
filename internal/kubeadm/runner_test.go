package kubeadm

import (
	"context"
	"strings"
	"testing"
)

func TestSanitizeRedactsSecrets(t *testing.T) {
	token := "abcdef.0123456789abcdef"
	hex64 := strings.Repeat("a", 64)
	pem := "-----BEGIN CERTIFICATE-----\nMIIB...\n-----END CERTIFICATE-----"

	in := "joined with token " + token + " key " + hex64 + " cert " + pem
	out := Sanitize(in)

	for _, secret := range []string{token, hex64, pem} {
		if strings.Contains(out, secret) {
			t.Fatalf("Sanitize leaked a secret %q in: %s", secret, out)
		}
	}
	if !strings.Contains(out, "[REDACTED-TOKEN]") || !strings.Contains(out, "[REDACTED-HEX]") || !strings.Contains(out, "[REDACTED-PEM]") {
		t.Fatalf("expected redaction markers, got: %s", out)
	}
}

// TestRunSanitizesStderrInError verifies the error returned by Run never carries a
// secret printed to stderr. The token is assembled inside the shell so its literal
// form is not present in argv (only in the produced stderr).
func TestRunSanitizesStderrInError(t *testing.T) {
	r := ExecRunner{path: "/bin/sh", env: []string{}}
	_, err := r.Run(context.Background(), "-c", "printf 'abcdef.%s\\n' 0123456789abcdef >&2; exit 1")
	if err == nil {
		t.Fatal("expected a non-zero exit error")
	}
	if strings.Contains(err.Error(), "abcdef.0123456789abcdef") {
		t.Fatalf("error leaked the token from stderr: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED-TOKEN]") {
		t.Fatalf("expected redacted token marker in error: %v", err)
	}
}

// TestRunRefusesEmptyAndRelativePath proves the zero value ExecRunner{} (and any
// relative path) errors before exec'ing anything (ADR-1-A1): never resolve a bare
// name via the provider's own PATH.
func TestRunRefusesEmptyAndRelativePath(t *testing.T) {
	for _, path := range []string{"", "kubeadm", "./kubeadm"} {
		r := ExecRunner{path: path, env: []string{}}
		if _, err := r.Run(context.Background(), "version"); err == nil {
			t.Errorf("path %q: expected an error, got nil", path)
		}
	}
}

// TestRunNilEnvNeverInherits proves a nil env is normalized to an empty (never
// the calling process's) environment, even when the runner is built by hand
// rather than via newExecRunner.
func TestRunNilEnvNeverInherits(t *testing.T) {
	t.Setenv("PROVIDER_KUBERNETES_TEST_CANARY", "leak-me")
	r := ExecRunner{path: "/usr/bin/env", env: nil}
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(res.Stdout, "PROVIDER_KUBERNETES_TEST_CANARY") {
		t.Fatalf("nil env inherited the calling process's environment: %q", res.Stdout)
	}
}
