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
	r := ExecRunner{Path: "/bin/sh"}
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
