package kubeadmconfig

import "testing"

func TestBuildClusterConfiguration(t *testing.T) {
	in := Input{
		KubernetesVersion:    "v1.34.0",
		ControlPlaneEndpoint: "10.0.0.1:6443",
		ImageRepository:      "registry.k8s.io",
		PodSubnet:            "10.244.0.0/16",
		ServiceSubnet:        "10.96.0.0/12",
		DNSDomain:            "cluster.local",
		CertSANs:             []string{"10.0.0.1"},
	}
	c := BuildClusterConfiguration(in)
	if c.APIVersion != KubeadmAPIVersion || c.Kind != KindClusterConfiguration {
		t.Fatalf("TypeMeta not set: %+v", c.TypeMeta)
	}
	if c.KubernetesVersion != "v1.34.0" || c.ControlPlaneEndpoint != "10.0.0.1:6443" {
		t.Fatalf("scalar fields not propagated: %+v", c)
	}
	if c.Networking.PodSubnet != "10.244.0.0/16" || c.Networking.ServiceSubnet != "10.96.0.0/12" {
		t.Fatalf("networking not propagated: %+v", c.Networking)
	}
	if len(c.APIServer.CertSANs) != 1 || c.APIServer.CertSANs[0] != "10.0.0.1" {
		t.Fatalf("certSANs not propagated: %+v", c.APIServer)
	}
}

func TestBuildInitConfigurationHasNoCredentials(t *testing.T) {
	in := Input{
		NodeName:         "node-1",
		CRISocket:        "unix:///run/containerd/containerd.sock",
		AdvertiseAddress: "10.0.0.1",
		BindPort:         6443,
		KubeletExtraArgs: []Arg{{Name: "node-ip", Value: "10.0.0.1"}},
	}
	i := BuildInitConfiguration(in)
	if i.APIVersion != KubeadmAPIVersion || i.Kind != KindInitConfiguration {
		t.Fatalf("TypeMeta not set: %+v", i.TypeMeta)
	}
	if i.NodeRegistration.Name != "node-1" || i.NodeRegistration.CRISocket == "" {
		t.Fatalf("nodeRegistration not propagated: %+v", i.NodeRegistration)
	}
	if i.LocalAPIEndpoint.AdvertiseAddress != "10.0.0.1" || i.LocalAPIEndpoint.BindPort != 6443 {
		t.Fatalf("localAPIEndpoint not propagated: %+v", i.LocalAPIEndpoint)
	}
	// The foundation/slice-2 builder must not populate credential-bearing fields.
	if len(i.BootstrapTokens) != 0 || i.CertificateKey != "" {
		t.Fatalf("non-credential builder must not set bootstrap tokens or certificate key")
	}
}

func TestBuildKubeletConfiguration(t *testing.T) {
	// Custom serviceSubnet -> clusterDNS pinned to the derived 10th host IP, with
	// kubelet v1beta1 TypeMeta. clusterDNS is a single-element list.
	k, err := BuildKubeletConfiguration(Input{ServiceSubnet: "172.20.0.0/16"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if k.APIVersion != KubeletAPIVersion || k.Kind != KindKubeletConfiguration {
		t.Fatalf("wrong TypeMeta: %+v", k.TypeMeta)
	}
	if len(k.ClusterDNS) != 1 || k.ClusterDNS[0] != "172.20.0.10" {
		t.Fatalf("clusterDNS = %v, want [172.20.0.10]", k.ClusterDNS)
	}

	// Empty serviceSubnet -> no clusterDNS (default path: kubeadm derives it).
	k2, err := BuildKubeletConfiguration(Input{ServiceSubnet: ""})
	if err != nil {
		t.Fatalf("unexpected error on empty subnet: %v", err)
	}
	if len(k2.ClusterDNS) != 0 {
		t.Fatalf("expected no clusterDNS for empty serviceSubnet, got %v", k2.ClusterDNS)
	}

	// Malformed serviceSubnet -> error (fail fast, do not silently default).
	if _, err := BuildKubeletConfiguration(Input{ServiceSubnet: "garbage"}); err == nil {
		t.Fatal("expected error for malformed serviceSubnet")
	}
}
