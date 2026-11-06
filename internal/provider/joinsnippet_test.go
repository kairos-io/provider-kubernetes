package provider

import (
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
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

func TestRenderJoinCloudConfig_CPWithoutAdvertiseAddressIsEmpty(t *testing.T) {
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
	// An empty address lets kubeadm fall back to the default-route interface.
	// A placeholder string is not an IP, so kubeadm would reject the join.
	if !strings.Contains(out, "advertiseAddress: \"\"\n") {
		t.Errorf("expected an empty advertiseAddress when none is given, got:\n%s", out)
	}
	if strings.Contains(out, "FILL-IN-THIS-NODE-IP") {
		t.Errorf("rendered a placeholder kubeadm rejects as advertiseAddress:\n%s", out)
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

func parseJoinCloudConfig(t *testing.T, out string) (outer, inner map[string]any) {
	t.Helper()
	if err := yamlv3.Unmarshal([]byte(out), &outer); err != nil {
		t.Fatalf("outer cloud-config does not parse: %v\n---\n%s", err, out)
	}
	cluster, ok := outer["cluster"].(map[string]any)
	if !ok {
		t.Fatalf("outer cloud-config has no cluster map:\n%s", out)
	}
	cfg, ok := cluster["config"].(string)
	if !ok {
		t.Fatalf("cluster.config is not a string:\n%s", out)
	}
	if err := yamlv3.Unmarshal([]byte(cfg), &inner); err != nil {
		t.Fatalf("cluster.config does not parse: %v\n---\n%s", err, cfg)
	}
	return outer, inner
}

func dig(t *testing.T, m map[string]any, path ...string) any {
	t.Helper()
	var cur any = m
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v: %q is not under a map (got %T)", path, k, cur)
		}
		cur = mm[k]
	}
	return cur
}

func TestRenderJoinCloudConfig_ValuesRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		s    JoinSnippet
	}{
		{"ordinary values", JoinSnippet{
			Endpoint: "10.0.0.5:6443", Token: "FAKE-token.0123456789abcdef",
			CACertHashes: []string{"sha256:FAKE-hash"}, CertificateKey: "FAKE-certkey",
			ClusterToken: "FAKE-cluster-token-0123456789", Device: "/dev/vda", AdvertiseAddress: "10.0.0.6",
		}},
		{"token with a double quote", JoinSnippet{
			Endpoint: "10.0.0.5:6443", Token: `FAKE-token"quoted`,
			CACertHashes: []string{"sha256:FAKE-hash"}, CertificateKey: "FAKE-certkey",
			ClusterToken: `FAKE-cluster"token-0123456789`, AdvertiseAddress: "10.0.0.6",
		}},
		{"token with literal backslash sequences", JoinSnippet{
			Endpoint: "10.0.0.5:6443", Token: `FAKE-token\t\x41\\end\`,
			CACertHashes: []string{`sha256:FAKE\hash`}, CertificateKey: `FAKE-certkey\n`,
			ClusterToken: `FAKE-cluster-token\x41\t-0123456789`, Device: `C:\dev\vda`, AdvertiseAddress: "10.0.0.6",
		}},
		{"endpoint with a trailing quote", JoinSnippet{
			Endpoint: `FAKE-endpoint.test:6443"`, Token: "FAKE-token.0123456789abcdef",
			CACertHashes: []string{"sha256:FAKE-hash"}, CertificateKey: "FAKE-certkey",
			ClusterToken: "FAKE-cluster-token-0123456789", AdvertiseAddress: "10.0.0.6",
		}},
		{"advertise address with a quote", JoinSnippet{
			Endpoint: "10.0.0.5:6443", Token: "FAKE-token.0123456789abcdef",
			CACertHashes: []string{"sha256:FAKE-hash"}, CertificateKey: "FAKE-certkey",
			ClusterToken: "FAKE-cluster-token-0123456789", AdvertiseAddress: `10.0.0."6`,
		}},
		{"line breaks and YAML syntax", JoinSnippet{
			Endpoint: "FAKE-endpoint.test:6443\"\n  role: controlplane", Token: "FAKE-token\r\n# comment: x\t\u2028\u0085\x7f",
			CACertHashes: []string{"sha256:FAKE\"\n- injected", "sha256:FAKE-two"}, CertificateKey: "FAKE-cert'key: {x}",
			ClusterToken: "FAKE-cluster-token\n  role: worker", AdvertiseAddress: "10.0.0.6\"\n  bindPort: 1",
		}},
		{"non-ASCII", JoinSnippet{
			Endpoint: "FAKE-ñandú.test:6443", Token: "FAKE-token-\U0001F511-\ufeff",
			CACertHashes: []string{"sha256:FAKE-hash"}, CertificateKey: "FAKE-certkey",
			ClusterToken: "FAKE-cluster-token-ü-0123456789", AdvertiseAddress: "10.0.0.6",
		}},
	}
	for _, tc := range cases {
		for _, role := range []string{"worker", "controlplane"} {
			t.Run(tc.name+"/"+role, func(t *testing.T) {
				s := tc.s
				s.Role = role
				out, err := RenderJoinCloudConfig(s)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				outer, inner := parseJoinCloudConfig(t, out)

				wantDevice := s.Device
				if wantDevice == "" {
					wantDevice = "auto"
				}
				type check struct {
					where string
					got   any
					want  string
				}
				checks := []check{
					{"install.device", dig(t, outer, "install", "device"), wantDevice},
					{"cluster.cluster_token", dig(t, outer, "cluster", "cluster_token"), s.ClusterToken},
					{"cluster.control_plane_host", dig(t, outer, "cluster", "control_plane_host"), s.Endpoint},
					{"cluster.role", dig(t, outer, "cluster", "role"), role},
					{"bootstrapToken.token", dig(t, inner, "joinConfiguration", "discovery", "bootstrapToken", "token"), s.Token},
					{"bootstrapToken.apiServerEndpoint", dig(t, inner, "joinConfiguration", "discovery", "bootstrapToken", "apiServerEndpoint"), s.Endpoint},
				}
				if role == "controlplane" {
					checks = append(checks,
						check{"controlPlane.certificateKey", dig(t, inner, "joinConfiguration", "controlPlane", "certificateKey"), s.CertificateKey},
						check{"localAPIEndpoint.advertiseAddress", dig(t, inner, "joinConfiguration", "controlPlane", "localAPIEndpoint", "advertiseAddress"), s.AdvertiseAddress},
					)
				} else if _, ok := inner["joinConfiguration"].(map[string]any)["controlPlane"]; ok {
					t.Errorf("worker config grew a controlPlane block:\n%s", out)
				}
				for _, c := range checks {
					if c.got != c.want {
						t.Errorf("%s = %q, want %q\n---\n%s", c.where, c.got, c.want, out)
					}
				}

				hashes, _ := dig(t, inner, "joinConfiguration", "discovery", "bootstrapToken", "caCertHashes").([]any)
				if len(hashes) != len(s.CACertHashes) {
					t.Fatalf("caCertHashes = %q, want %q\n---\n%s", hashes, s.CACertHashes, out)
				}
				for i, h := range hashes {
					if h != s.CACertHashes[i] {
						t.Errorf("caCertHashes[%d] = %q, want %q", i, h, s.CACertHashes[i])
					}
				}
			})
		}
	}
}

func TestRenderJoinCloudConfig_RejectsInvalidUTF8(t *testing.T) {
	_, err := RenderJoinCloudConfig(JoinSnippet{
		Role:         "worker",
		Endpoint:     "10.0.0.5:6443",
		Token:        "FAKE-token-\xff",
		CACertHashes: []string{"sha256:FAKE-hash"},
	})
	if err == nil {
		t.Fatal("expected an error for a token that is not valid UTF-8")
	}
	if strings.Contains(err.Error(), "FAKE-token") {
		t.Errorf("error leaks the token: %v", err)
	}
}
