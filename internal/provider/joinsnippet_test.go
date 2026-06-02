package provider

import (
	"strings"
	"testing"
)

func TestRenderJoinCloudConfig_Worker(t *testing.T) {
	out, err := RenderJoinCloudConfig(JoinSnippet{
		Role:         "worker",
		Endpoint:     "10.0.0.5:6443",
		Token:        "abcdef.0123456789abcdef",
		CACertHashes: []string{"sha256:deadbeef"},
		ClusterToken: "correlation-token-1234567890",
		TTL:          "1h0m0s",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		"role: worker",
		"abcdef.0123456789abcdef",
		"apiServerEndpoint: \"10.0.0.5:6443\"",
		"control_plane_host: \"10.0.0.5:6443\"",
		"sha256:deadbeef",
		"cluster_token: \"correlation-token-1234567890\"",
		"groups: [\"admin\", \"sudo\"]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("worker config missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "certificateKey") {
		t.Errorf("worker config must not include certificateKey:\n%s", out)
	}
	if strings.Contains(out, "controlPlane:") {
		t.Errorf("worker config must not include controlPlane block:\n%s", out)
	}
}

func TestRenderJoinCloudConfig_ControlPlane(t *testing.T) {
	out, err := RenderJoinCloudConfig(JoinSnippet{
		Role:           "controlplane",
		Endpoint:       "cp.example.com:6443",
		Token:          "abcdef.0123456789abcdef",
		CACertHashes:   []string{"sha256:aa", "sha256:bb"},
		CertificateKey: "feedface",
		TTL:            "2h0m0s",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		"role: controlplane",
		"controlPlane:",
		"certificateKey: \"feedface\"",
		"sha256:aa",
		"sha256:bb",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("controlplane config missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderJoinCloudConfig_DefaultsClusterTokenPlaceholder(t *testing.T) {
	out, err := RenderJoinCloudConfig(JoinSnippet{
		Role:         "worker",
		Endpoint:     "10.0.0.5:6443",
		Token:        "abcdef.0123456789abcdef",
		CACertHashes: []string{"sha256:deadbeef"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, clusterTokenPlaceholder) {
		t.Errorf("expected cluster_token placeholder %q in:\n%s", clusterTokenPlaceholder, out)
	}
	// The placeholder must satisfy the >=16-char cluster_token rule so a forgetful
	// operator at least fails on intent (replace-me) rather than length.
	if len(clusterTokenPlaceholder) < 16 {
		t.Errorf("placeholder too short to pass cluster_token validation: %d", len(clusterTokenPlaceholder))
	}
}

func TestRenderJoinCloudConfig_Rejects(t *testing.T) {
	cases := []struct {
		name string
		s    JoinSnippet
	}{
		{"bad role", JoinSnippet{Role: "etcd", Endpoint: "x:6443", Token: "t", CACertHashes: []string{"sha256:a"}}},
		{"empty endpoint", JoinSnippet{Role: "worker", Token: "t", CACertHashes: []string{"sha256:a"}}},
		{"empty token", JoinSnippet{Role: "worker", Endpoint: "x:6443", CACertHashes: []string{"sha256:a"}}},
		{"no hashes", JoinSnippet{Role: "worker", Endpoint: "x:6443", Token: "t"}},
		{"blank hashes", JoinSnippet{Role: "worker", Endpoint: "x:6443", Token: "t", CACertHashes: []string{"  "}}},
		{"cp without cert key", JoinSnippet{Role: "controlplane", Endpoint: "x:6443", Token: "t", CACertHashes: []string{"sha256:a"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := RenderJoinCloudConfig(tc.s); err == nil {
				t.Errorf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

func TestEndpointFromKubeconfig(t *testing.T) {
	kubeconfig := `apiVersion: v1
kind: Config
clusters:
- name: kubernetes
  cluster:
    server: https://10.0.2.15:6443
    certificate-authority-data: REDACTED
`
	got, err := EndpointFromKubeconfig([]byte(kubeconfig))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "10.0.2.15:6443" {
		t.Errorf("got %q, want 10.0.2.15:6443", got)
	}
}

// HA-2: localAPIEndpoint advertiseAddress placeholder in CP join cloud-config.
func TestRenderJoinCloudConfig_CPWithAdvertiseAddress(t *testing.T) {
	out, err := RenderJoinCloudConfig(JoinSnippet{
		Role:             "controlplane",
		Endpoint:         "vip.example.test:6443",
		Token:            "abcdef.0123456789abcdef",
		CACertHashes:     []string{"sha256:deadbeef"},
		CertificateKey:   "feedface",
		AdvertiseAddress: "10.0.0.5",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "advertiseAddress: \"10.0.0.5\"") {
		t.Errorf("expected advertiseAddress in CP join config, got:\n%s", out)
	}
}

func TestRenderJoinCloudConfig_CPWithoutAdvertiseAddressHasPlaceholder(t *testing.T) {
	out, err := RenderJoinCloudConfig(JoinSnippet{
		Role:           "controlplane",
		Endpoint:       "vip.example.test:6443",
		Token:          "abcdef.0123456789abcdef",
		CACertHashes:   []string{"sha256:deadbeef"},
		CertificateKey: "feedface",
		// AdvertiseAddress intentionally empty.
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Must contain the placeholder indicating operator must fill this in.
	if !strings.Contains(out, "FILL-IN-THIS-NODE-IP") {
		t.Errorf("expected placeholder when advertiseAddress is empty, got:\n%s", out)
	}
}

func TestEndpointFromKubeconfig_Errors(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{"no clusters", "apiVersion: v1\nkind: Config\nclusters: []\n"},
		{"empty server", "clusters:\n- cluster:\n    server: \"\"\n"},
		{"garbage", "::: not yaml :::"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := EndpointFromKubeconfig([]byte(tc.data)); err == nil {
				t.Errorf("expected error for %s, got nil", tc.name)
			}
		})
	}
}
