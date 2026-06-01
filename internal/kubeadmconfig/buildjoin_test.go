package kubeadmconfig

import "testing"

func TestBuildJoinConfigurationTokenDiscovery(t *testing.T) {
	in := Input{
		ControlPlaneEndpoint: "10.0.0.1:6443",
		JoinToken:            "abcdef.0123456789abcdef",
		CACertHashes:         []string{"sha256:deadbeef"},
		NodeName:             "worker-1",
		CRISocket:            "unix:///run/containerd/containerd.sock",
	}
	j, err := BuildJoinConfiguration(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if j.APIVersion != KubeadmAPIVersion || j.Kind != KindJoinConfiguration {
		t.Fatalf("TypeMeta not set: %+v", j.TypeMeta)
	}
	if j.Discovery.BootstrapToken == nil {
		t.Fatal("expected bootstrap-token discovery")
	}
	bt := j.Discovery.BootstrapToken
	if bt.Token != in.JoinToken || bt.APIServerEndpoint != in.ControlPlaneEndpoint {
		t.Fatalf("discovery fields not propagated: %+v", bt)
	}
	if len(bt.CACertHashes) != 1 || bt.CACertHashes[0] != "sha256:deadbeef" {
		t.Fatalf("CA cert hashes not propagated: %+v", bt.CACertHashes)
	}
	if bt.UnsafeSkipCAVerification {
		t.Fatal("UnsafeSkipCAVerification must never be true")
	}
	if j.ControlPlane != nil {
		t.Fatal("worker join must not set ControlPlane")
	}
}

func TestBuildJoinConfigurationRejectsTokenWithoutCAHashes(t *testing.T) {
	_, err := BuildJoinConfiguration(Input{
		ControlPlaneEndpoint: "10.0.0.1:6443",
		JoinToken:            "abcdef.0123456789abcdef",
		// no CACertHashes
	})
	if err == nil {
		t.Fatal("expected refusal: token discovery without CA hashes (CA pinning is mandatory)")
	}
}

func TestBuildJoinConfigurationRejectsNoAnchor(t *testing.T) {
	_, err := BuildJoinConfiguration(Input{ControlPlaneEndpoint: "10.0.0.1:6443"})
	if err == nil {
		t.Fatal("expected refusal: no discovery file and no token+hashes")
	}
}

func TestBuildJoinConfigurationFileDiscovery(t *testing.T) {
	j, err := BuildJoinConfiguration(Input{DiscoveryFilePath: "/run/discovery.conf"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if j.Discovery.File == nil || j.Discovery.File.KubeConfigPath != "/run/discovery.conf" {
		t.Fatalf("expected file discovery, got %+v", j.Discovery)
	}
	if j.Discovery.BootstrapToken != nil {
		t.Fatal("file discovery must not also set bootstrap token")
	}
}

func TestBuildJoinConfigurationControlPlane(t *testing.T) {
	in := Input{
		ControlPlaneEndpoint: "10.0.0.1:6443",
		JoinToken:            "abcdef.0123456789abcdef",
		CACertHashes:         []string{"sha256:deadbeef"},
		JoinAsControlPlane:   true,
		CertificateKey:       "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		AdvertiseAddress:     "10.0.0.2",
		BindPort:             6443,
	}
	j, err := BuildJoinConfiguration(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if j.ControlPlane == nil || j.ControlPlane.CertificateKey != in.CertificateKey {
		t.Fatalf("control-plane join not configured: %+v", j.ControlPlane)
	}
	if j.ControlPlane.LocalAPIEndpoint.AdvertiseAddress != "10.0.0.2" {
		t.Fatalf("localAPIEndpoint not propagated: %+v", j.ControlPlane.LocalAPIEndpoint)
	}
}

func TestBuildJoinConfigurationControlPlaneRequiresCertKey(t *testing.T) {
	_, err := BuildJoinConfiguration(Input{
		ControlPlaneEndpoint: "10.0.0.1:6443",
		JoinToken:            "abcdef.0123456789abcdef",
		CACertHashes:         []string{"sha256:deadbeef"},
		JoinAsControlPlane:   true,
		// no CertificateKey
	})
	if err == nil {
		t.Fatal("expected error: control-plane join without certificate key")
	}
}
