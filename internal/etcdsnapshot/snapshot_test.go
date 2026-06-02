package etcdsnapshot

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stackedRoot makes a temp RootPath that looks like a stacked-etcd control plane.
func stackedRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	manifest := filepath.Join(root, "etc", "kubernetes", "manifests", "etcd.yaml")
	if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("kind: Pod\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRun_RefusesWhenEncryptionUnconfirmed(t *testing.T) {
	root := stackedRoot(t)
	called := false
	err := Run(context.Background(), Options{
		RootPath:            root,
		Dir:                 filepath.Join(t.TempDir(), "snap"),
		EncryptionConfirmed: func(context.Context) bool { return false },
		Save:                func(context.Context, string, string) error { called = true; return nil },
		Now:                 time.Unix(0, 0),
	})
	if err == nil {
		t.Fatal("expected refusal when encryption is unconfirmed")
	}
	if called {
		t.Fatal("Save must NOT run when encryption is unconfirmed (no plaintext snapshot)")
	}
}

func TestRun_NonStackedEtcdRefused(t *testing.T) {
	// No etcd manifest/member dir -> not stacked -> operator must snapshot.
	err := Run(context.Background(), Options{
		RootPath:            t.TempDir(),
		EncryptionConfirmed: func(context.Context) bool { return true },
		Save:                func(context.Context, string, string) error { return nil },
	})
	if err == nil {
		t.Fatal("expected refusal on non-stacked/external etcd")
	}
}

func TestRun_WritesSnapshotWhenEncryptedAndStacked(t *testing.T) {
	root := stackedRoot(t)
	dir := filepath.Join(t.TempDir(), "etcd-backup")
	// Seed a stale snapshot to verify single-retain pruning.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "etcd-snapshot-old.db")
	if err := os.WriteFile(stale, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	var savedDest string
	err := Run(context.Background(), Options{
		RootPath:            root,
		Dir:                 dir,
		EncryptionConfirmed: func(context.Context) bool { return true },
		Save: func(_ context.Context, _ string, dest string) error {
			savedDest = dest
			return os.WriteFile(dest, []byte("snap"), 0o600)
		},
		Now: time.Unix(1700000000, 0),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(savedDest); err != nil {
		t.Fatalf("snapshot not written: %v", err)
	}
	info, _ := os.Stat(savedDest)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot mode = %o, want 0600", info.Mode().Perm())
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale snapshot must be pruned (single-retain): %v", err)
	}
}

func TestValidateDir_RejectsArtifactPaths(t *testing.T) {
	root := "/"
	for _, bad := range []string{
		"/etc/kubernetes/backup",
		"/var/lib/etcd/snap",
		"/var/lib/kubelet/x",
		"relative/dir",
	} {
		if err := validateDir(root, bad); err == nil {
			t.Fatalf("validateDir must reject %q", bad)
		}
	}
	if err := validateDir(root, DefaultDir); err != nil {
		t.Fatalf("DefaultDir must be allowed, got %v", err)
	}
}
