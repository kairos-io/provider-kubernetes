package provider

import (
	"testing"
)

// containsAll reports whether got contains all expected values.
func containsAll(got, want []string) bool {
	m := make(map[string]bool, len(got))
	for _, v := range got {
		m[v] = true
	}
	for _, v := range want {
		if !m[v] {
			return false
		}
	}
	return true
}

func TestParseUserConfig(t *testing.T) {
	raw := `
clusterConfiguration:
  kubernetesVersion: v1.34.0
  networking:
    podSubnet: 10.244.0.0/16
    serviceSubnet: 10.96.0.0/12
  apiServer:
    certSANs:
    - api.example.test
initConfiguration:
  localAPIEndpoint:
    advertiseAddress: 10.0.0.5
    bindPort: 6443
  nodeRegistration:
    criSocket: unix:///run/containerd/containerd.sock
    kubeletExtraArgs:
    - name: node-ip
      value: 10.0.0.5
`
	uc, err := ParseUserConfig(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if uc.ClusterConfiguration.KubernetesVersion != "v1.34.0" {
		t.Fatalf("kubernetesVersion not parsed: %q", uc.ClusterConfiguration.KubernetesVersion)
	}
	if uc.ClusterConfiguration.Networking.PodSubnet != "10.244.0.0/16" {
		t.Fatalf("podSubnet not parsed: %q", uc.ClusterConfiguration.Networking.PodSubnet)
	}
	if len(uc.InitConfiguration.NodeRegistration.KubeletExtraArgs) != 1 ||
		uc.InitConfiguration.NodeRegistration.KubeletExtraArgs[0].Name != "node-ip" {
		t.Fatalf("kubeletExtraArgs not parsed: %+v", uc.InitConfiguration.NodeRegistration.KubeletExtraArgs)
	}
}

func TestParseUserConfigEmpty(t *testing.T) {
	uc, err := ParseUserConfig("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if uc.ClusterConfiguration.KubernetesVersion != "" {
		t.Fatalf("expected zero value for empty config")
	}
}

func TestBuildInputDefaultsAndNormalization(t *testing.T) {
	ctx := Context{ControlPlaneHost: "10.0.0.1"} // no port
	uc, _ := ParseUserConfig("")

	in, _, err := BuildInput(ctx, uc, "v1.34.2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if in.ControlPlaneEndpoint != "10.0.0.1:6443" {
		t.Fatalf("expected default port appended, got %q", in.ControlPlaneEndpoint)
	}
	if in.KubernetesVersion != "v1.34.2" {
		t.Fatalf("expected resolved version to win, got %q", in.KubernetesVersion)
	}
	if in.DNSDomain != "cluster.local" {
		t.Fatalf("expected default DNS domain, got %q", in.DNSDomain)
	}
	if in.CRISocket != "unix:///run/containerd/containerd.sock" {
		t.Fatalf("expected default CRI socket, got %q", in.CRISocket)
	}
	if len(in.CertSANs) != 1 || in.CertSANs[0] != "10.0.0.1" {
		t.Fatalf("expected control-plane host (no port) added to certSANs, got %v", in.CertSANs)
	}
}

func TestBuildInputUserPrecedenceAndPortPreserved(t *testing.T) {
	ctx := Context{ControlPlaneHost: "10.0.0.1:7443"} // explicit port
	uc, _ := ParseUserConfig("clusterConfiguration:\n  controlPlaneEndpoint: vip.example.test:8443\n  kubernetesVersion: v1.35.0\n")

	// resolvedVersion empty -> fall back to user-specified kubernetesVersion.
	in, _, err := BuildInput(ctx, uc, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if in.ControlPlaneEndpoint != "vip.example.test:8443" {
		t.Fatalf("expected user controlPlaneEndpoint to win, got %q", in.ControlPlaneEndpoint)
	}
	if in.KubernetesVersion != "v1.35.0" {
		t.Fatalf("expected user version fallback, got %q", in.KubernetesVersion)
	}
	// HA-1: certSANs now includes both the ControlPlaneHost-derived host ("10.0.0.1")
	// and the port-stripped controlPlaneEndpoint host ("vip.example.test"), since a
	// stable VIP/LB endpoint may differ from the node's own IP.
	if !containsAll(in.CertSANs, []string{"10.0.0.1", "vip.example.test"}) {
		t.Fatalf("expected both ControlPlaneHost and controlPlaneEndpoint hosts in certSANs (HA-1), got %v", in.CertSANs)
	}
}

// TestBuildInputReturnsEndpointWarning guards the wiring (not just the pure
// validator): BuildInput must RETURN the HA-1 advisory so Run can log it. A prior
// version mutated the by-value Context, silently dropping the warning.
func TestBuildInputReturnsEndpointWarning(t *testing.T) {
	// role=init with the endpoint equal to the node's own IP -> advisory expected.
	ctx := Context{Role: "init", ControlPlaneHost: "10.0.0.1"}
	uc, _ := ParseUserConfig("")
	_, warn, err := BuildInput(ctx, uc, "v1.34.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if warn == "" {
		t.Fatal("expected a non-empty endpoint advisory to be returned for role=init with a bare-IP endpoint")
	}

	// role=init with a stable, distinct endpoint -> no advisory.
	ctx2 := Context{Role: "init", ControlPlaneHost: "10.0.0.1"}
	uc2, _ := ParseUserConfig("clusterConfiguration:\n  controlPlaneEndpoint: vip.example.test:6443\n")
	_, warn2, err := BuildInput(ctx2, uc2, "v1.34.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if warn2 != "" {
		t.Fatalf("expected no advisory for a stable endpoint, got %q", warn2)
	}
}

func TestBuildInputRequiresControlPlaneHost(t *testing.T) {
	if _, _, err := BuildInput(Context{}, UserConfig{}, "v1.34.0"); err == nil {
		t.Fatal("expected error when control_plane_host is empty")
	}
}

// HA-1 tests for ValidateControlPlaneEndpoint.

func TestValidateControlPlaneEndpointInitNoEndpointWarns(t *testing.T) {
	warn, err := ValidateControlPlaneEndpoint("init", "", "10.0.0.1")
	if err != nil {
		t.Fatalf("role=init with no endpoint must not hard-fail, got error: %v", err)
	}
	if warn == "" {
		t.Fatal("role=init with no endpoint must emit a warning")
	}
}

func TestValidateControlPlaneEndpointInitBareIPWarns(t *testing.T) {
	// endpoint == nodeIP -> single-node default, warn only
	warn, err := ValidateControlPlaneEndpoint("init", "10.0.0.1:6443", "10.0.0.1")
	if err != nil {
		t.Fatalf("role=init with bare node IP endpoint must not hard-fail, got error: %v", err)
	}
	if warn == "" {
		t.Fatal("role=init with bare node IP endpoint must emit a warning")
	}
}

func TestValidateControlPlaneEndpointInitStableEndpointOK(t *testing.T) {
	// Different from node IP -> no warning, no error
	warn, err := ValidateControlPlaneEndpoint("init", "vip.example.test:6443", "10.0.0.1")
	if err != nil {
		t.Fatalf("role=init with stable endpoint must not fail: %v", err)
	}
	if warn != "" {
		t.Fatalf("role=init with stable endpoint must not warn, got: %q", warn)
	}
}

func TestValidateControlPlaneEndpointCPEmptyHardFails(t *testing.T) {
	_, err := ValidateControlPlaneEndpoint("controlplane", "", "10.0.0.1")
	if err == nil {
		t.Fatal("role=controlplane with empty endpoint must hard-fail")
	}
}

func TestValidateControlPlaneEndpointCPNonEmptyOK(t *testing.T) {
	warn, err := ValidateControlPlaneEndpoint("controlplane", "vip.example.test:6443", "10.0.0.2")
	if err != nil {
		t.Fatalf("role=controlplane with stable endpoint must not fail: %v", err)
	}
	if warn != "" {
		t.Fatalf("role=controlplane with stable endpoint must not warn: %q", warn)
	}
}

func TestValidateControlPlaneEndpointWorkerNoValidation(t *testing.T) {
	// worker: no validation regardless of endpoint value
	warn, err := ValidateControlPlaneEndpoint("worker", "", "10.0.0.1")
	if err != nil {
		t.Fatalf("role=worker must not fail on empty endpoint: %v", err)
	}
	if warn != "" {
		t.Fatalf("role=worker must not warn: %q", warn)
	}
}

// HA-2: BuildInput for CP join uses joinConfiguration.controlPlane.localAPIEndpoint
// when set.

func TestBuildInputCPJoinPrefersJoinLocalAPIEndpoint(t *testing.T) {
	ctx := Context{Role: "controlplane", ControlPlaneHost: "vip.example.test:6443"}
	raw := `
joinConfiguration:
  controlPlane:
    localAPIEndpoint:
      advertiseAddress: "10.0.0.5"
      bindPort: 6443
`
	uc, err := ParseUserConfig(raw)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	in, _, err := BuildInput(ctx, uc, "v1.34.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if in.AdvertiseAddress != "10.0.0.5" {
		t.Fatalf("expected joinConfiguration.controlPlane.localAPIEndpoint.advertiseAddress, got %q", in.AdvertiseAddress)
	}
	if in.BindPort != 6443 {
		t.Fatalf("expected joinConfiguration.controlPlane.localAPIEndpoint.bindPort, got %d", in.BindPort)
	}
}

func TestBuildInputWorkerFallsBackToInitLocalAPIEndpoint(t *testing.T) {
	// For a worker, only initConfiguration.localAPIEndpoint applies (not the CP join field).
	ctx := Context{Role: "worker", ControlPlaneHost: "vip.example.test:6443"}
	raw := `
initConfiguration:
  localAPIEndpoint:
    advertiseAddress: "10.0.0.6"
    bindPort: 6443
joinConfiguration:
  controlPlane:
    localAPIEndpoint:
      advertiseAddress: "10.0.0.7"
`
	uc, err := ParseUserConfig(raw)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	in, _, err := BuildInput(ctx, uc, "v1.34.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Worker must use initConfiguration.localAPIEndpoint, not the CP join field.
	if in.AdvertiseAddress != "10.0.0.6" {
		t.Fatalf("worker must use initConfiguration.localAPIEndpoint, got %q", in.AdvertiseAddress)
	}
}
