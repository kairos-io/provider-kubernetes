package provider

import (
	"strings"
	"testing"

	"github.com/kairos-io/kairos-sdk/clusterplugin"
	"gopkg.in/yaml.v3"
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
	// Valid input -> a named YipConfig with the reconcile stage, no panic.
	cfg := Provider(clusterplugin.Cluster{Role: clusterplugin.RoleInit, ClusterToken: validToken()})
	if cfg.Name == "" {
		t.Fatal("expected a named YipConfig")
	}
	// Invalid input -> configuration-error config, still named, no panic.
	bad := Provider(clusterplugin.Cluster{Role: clusterplugin.RoleInit})
	if bad.Name == "" {
		t.Fatal("expected a named YipConfig on bad input")
	}
	if len(bad.Stages) != 0 {
		t.Fatal("Provider must NOT emit stages on invalid input")
	}
}

func TestProviderEmitsBootStage(t *testing.T) {
	cfg := Provider(clusterplugin.Cluster{
		Role:             clusterplugin.RoleInit,
		ClusterToken:     validToken(),
		ControlPlaneHost: "10.0.0.1",
	})
	stages, ok := cfg.Stages["network.after"]
	if !ok || len(stages) < 2 {
		t.Fatalf("expected at least two stages under network.after, got: %+v", cfg.Stages)
	}
	// First stage writes the cluster state file at 0600 to the tmpfs path.
	if len(stages[0].Files) != 1 {
		t.Fatalf("expected first stage to write one File, got %+v", stages[0].Files)
	}
	f := stages[0].Files[0]
	if f.Path != ClusterStatePath {
		t.Fatalf("expected cluster file at %s, got %s", ClusterStatePath, f.Path)
	}
	if f.Permissions != 0o600 {
		t.Fatalf("cluster state file must be 0600, got %o", f.Permissions)
	}
	// The serialized cluster must NOT contain the secret-equivalent bootstrap
	// token or cert key (joiner samples may carry these via joinConfiguration,
	// but they go through user-config; we just confirm the structure here).
	var got clusterplugin.Cluster
	if err := yaml.Unmarshal([]byte(f.Content), &got); err != nil {
		t.Fatalf("cluster file is not valid YAML: %v", err)
	}
	if got.Role != clusterplugin.RoleInit {
		t.Fatalf("cluster role round-trip failed: %q", got.Role)
	}
	// Second stage imports the pre-bundled control-plane images (ADR-16). It MUST
	// come before the reconcile stage so kubeadm init/join finds images locally.
	if len(stages) < 3 {
		t.Fatalf("expected three stages under network.after (write, import, reconcile), got %d", len(stages))
	}
	importCmd := stages[1].Commands[0]
	if !strings.Contains(importCmd, ProviderBinaryPath+" import-images") {
		t.Fatalf("expected import-images step before reconcile, got %q", importCmd)
	}
	// Third stage invokes the reconcile subcommand.
	if len(stages[2].Commands) != 1 {
		t.Fatalf("expected reconcile stage to have exactly one Command, got %+v", stages[2].Commands)
	}
	cmd := stages[2].Commands[0]
	if !strings.Contains(cmd, ProviderBinaryPath+" reconcile") {
		t.Fatalf("reconcile command shape wrong: %q", cmd)
	}
	if !strings.Contains(cmd, "--cluster-file="+ClusterStatePath) {
		t.Fatalf("reconcile command must reference the cluster file: %q", cmd)
	}
}
