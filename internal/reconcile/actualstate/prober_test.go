package actualstate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFileProberMembership(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  Membership
	}{
		{name: "uninitialized", want: Uninitialized},
		{name: "joined", files: []string{"etc/kubernetes/kubelet.conf"}, want: Joined},
		{name: "initialized", files: []string{"etc/kubernetes/admin.conf", "etc/kubernetes/kubelet.conf"}, want: Initialized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for _, f := range tt.files {
				writeFile(t, filepath.Join(root, f))
			}
			s, err := FileProber{RootPath: root}.Probe(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if s.Membership != tt.want {
				t.Fatalf("got membership %q want %q", s.Membership, tt.want)
			}
		})
	}
}

func TestFileProberLivenessInjected(t *testing.T) {
	root := t.TempDir()
	p := FileProber{
		RootPath:              root,
		KubeletHealthy:        func(context.Context) bool { return true },
		ControlPlaneReachable: func(context.Context) bool { return true },
	}
	s, err := p.Probe(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !s.KubeletHealthy || !s.ControlPlaneReachable {
		t.Fatalf("expected injected liveness to propagate: %+v", s)
	}
}

// ADR-12: version signals (NodeComponentVersion from the apiserver manifest;
// ClusterVersion/RunningKubeletVersion from injected funcs).
func TestFileProberVersionSignals(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "etc", "kubernetes", "manifests", "kube-apiserver.yaml")
	if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "spec:\n  containers:\n  - image: registry.k8s.io/kube-apiserver:v1.34.8\n    name: kube-apiserver\n"
	if err := os.WriteFile(manifest, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	p := FileProber{
		RootPath:              root,
		ClusterVersion:        func(context.Context) string { return "v1.34.8" },
		RunningKubeletVersion: func(context.Context) string { return "v1.34.8" },
	}
	s, err := p.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if s.NodeComponentVersion != "v1.34.8" {
		t.Fatalf("NodeComponentVersion = %q, want v1.34.8 (parsed from apiserver manifest)", s.NodeComponentVersion)
	}
	if s.ClusterVersion != "v1.34.8" {
		t.Fatalf("ClusterVersion = %q, want v1.34.8", s.ClusterVersion)
	}
	if s.RunningKubeletVersion != "v1.34.8" {
		t.Fatalf("RunningKubeletVersion = %q, want v1.34.8", s.RunningKubeletVersion)
	}
}

func TestFileProberNoManifestNoVersion(t *testing.T) {
	// Worker / pre-init: no apiserver manifest -> empty NodeComponentVersion, and
	// nil injected funcs -> empty cluster/kubelet versions (no panic).
	p := FileProber{RootPath: t.TempDir()}
	s, err := p.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if s.NodeComponentVersion != "" || s.ClusterVersion != "" || s.RunningKubeletVersion != "" {
		t.Fatalf("expected empty version signals, got %+v", s)
	}
}
