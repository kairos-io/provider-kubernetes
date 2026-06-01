package kubeadmconfig

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestMarshalSetsTypeMetaAndFields(t *testing.T) {
	cc := NewClusterConfiguration()
	cc.KubernetesVersion = "v1.34.0"
	cc.ControlPlaneEndpoint = "10.0.0.1:6443"
	cc.Networking = Networking{PodSubnet: "10.244.0.0/16", ServiceSubnet: "10.96.0.0/12"}
	cc.APIServer = APIServer{CertSANs: []string{"10.0.0.1"}}

	out, err := Marshal(cc)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}

	// Round-trip back into the typed object and assert behavior, not raw strings.
	var got ClusterConfiguration
	if err := yaml.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("could not unmarshal produced YAML: %v", err)
	}
	if got.APIVersion != KubeadmAPIVersion || got.Kind != KindClusterConfiguration {
		t.Fatalf("TypeMeta not set correctly: apiVersion=%q kind=%q", got.APIVersion, got.Kind)
	}
	if got.KubernetesVersion != "v1.34.0" {
		t.Fatalf("kubernetesVersion round-trip failed: %q", got.KubernetesVersion)
	}
	if got.Networking.PodSubnet != "10.244.0.0/16" {
		t.Fatalf("podSubnet round-trip failed: %q", got.Networking.PodSubnet)
	}
}

func TestMarshalJoinsMultipleDocsWithSeparator(t *testing.T) {
	cc := NewClusterConfiguration()
	cc.KubernetesVersion = "v1.35.1"
	ic := NewInitConfiguration()
	ic.NodeRegistration = NodeRegistration{CRISocket: "unix:///run/containerd/containerd.sock"}

	out, err := Marshal(cc, ic)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if !strings.Contains(out, "\n---\n") {
		t.Fatalf("expected a document separator between two docs, got:\n%s", out)
	}
	if c := strings.Count(out, "apiVersion:"); c != 2 {
		t.Fatalf("expected 2 apiVersion lines, got %d:\n%s", c, out)
	}
	if !strings.Contains(out, KindClusterConfiguration) || !strings.Contains(out, KindInitConfiguration) {
		t.Fatalf("expected both kinds present, got:\n%s", out)
	}
}

func TestMarshalEmptyIsEmpty(t *testing.T) {
	out, err := Marshal()
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}
	if out != "" {
		t.Fatalf("expected empty output for no docs, got %q", out)
	}
}
