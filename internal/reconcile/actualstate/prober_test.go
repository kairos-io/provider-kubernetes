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
