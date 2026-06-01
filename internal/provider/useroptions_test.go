package provider

import "testing"

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

	in, err := BuildInput(ctx, uc, "v1.34.2")
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
	in, err := BuildInput(ctx, uc, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if in.ControlPlaneEndpoint != "vip.example.test:8443" {
		t.Fatalf("expected user controlPlaneEndpoint to win, got %q", in.ControlPlaneEndpoint)
	}
	if in.KubernetesVersion != "v1.35.0" {
		t.Fatalf("expected user version fallback, got %q", in.KubernetesVersion)
	}
	if len(in.CertSANs) != 1 || in.CertSANs[0] != "10.0.0.1" {
		t.Fatalf("expected bare host (port stripped) in certSANs, got %v", in.CertSANs)
	}
}

func TestBuildInputRequiresControlPlaneHost(t *testing.T) {
	if _, err := BuildInput(Context{}, UserConfig{}, "v1.34.0"); err == nil {
		t.Fatal("expected error when control_plane_host is empty")
	}
}
