package provider

import (
	"strings"
	"testing"

	"github.com/kairos-io/kairos-sdk/clusterplugin"
)

func validToken() string { return "Tok3n-with-pLENTY-of-entropy-0123456789" }

func TestValidateClusterToken(t *testing.T) {
	tests := []struct {
		name      string
		token     string
		wantWarn  bool
		wantError bool
	}{
		{name: "empty", token: "", wantError: true},
		{name: "whitespace", token: "      ", wantError: true},
		{name: "too short", token: "short", wantError: true},
		{name: "16 lowercase only warns", token: "abcdefghijklmnop", wantWarn: true},
		{name: "high entropy ok", token: validToken()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warn, err := ValidateClusterToken(tt.token)
			if tt.wantError {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantWarn && warn == "" {
				t.Fatalf("expected a warning, got none")
			}
			if !tt.wantWarn && warn != "" {
				t.Fatalf("expected no warning, got %q", warn)
			}
		})
	}
}

func TestValidateClusterTokenNeverLeaksValue(t *testing.T) {
	secret := "abcdefghijklmnop" // low entropy -> produces a warning
	warn, err := ValidateClusterToken(secret)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(warn, secret) {
		t.Fatalf("warning must not contain the token value")
	}
}

func TestNewContextDefaultsRootPath(t *testing.T) {
	ctx, err := NewContext(clusterplugin.Cluster{
		Role:         clusterplugin.RoleInit,
		ClusterToken: validToken(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ctx.RootPath != "/" {
		t.Fatalf("expected default root path '/', got %q", ctx.RootPath)
	}
	if ctx.Role != "init" {
		t.Fatalf("expected role 'init', got %q", ctx.Role)
	}
}

func TestNewContextHonorsClusterRootPath(t *testing.T) {
	ctx, err := NewContext(clusterplugin.Cluster{
		Role:            clusterplugin.RoleWorker,
		ClusterToken:    validToken(),
		ProviderOptions: map[string]string{"cluster_root_path": "/persistent/root"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ctx.RootPath != "/persistent/root" {
		t.Fatalf("expected honored root path, got %q", ctx.RootPath)
	}
}

func TestNewContextRejectsBadToken(t *testing.T) {
	if _, err := NewContext(clusterplugin.Cluster{Role: clusterplugin.RoleInit}); err == nil {
		t.Fatal("expected error for empty cluster_token")
	}
}

func TestProviderReturnsPromptlyAndNamed(t *testing.T) {
	// Valid input -> inert (foundation) config, named, no panic.
	cfg := Provider(clusterplugin.Cluster{Role: clusterplugin.RoleInit, ClusterToken: validToken()})
	if cfg.Name == "" {
		t.Fatal("expected a named YipConfig")
	}
	// Invalid input -> configuration-error config, still named, no panic.
	bad := Provider(clusterplugin.Cluster{Role: clusterplugin.RoleInit})
	if bad.Name == "" {
		t.Fatal("expected a named YipConfig on bad input")
	}
}
