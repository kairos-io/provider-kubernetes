//go:build linux

package actualstate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	kubeletConfRel = "etc/kubernetes/kubelet.conf"
	rotatedPEMRel  = "var/lib/kubelet/pki/kubelet-client-current.pem"
)

func writeContent(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeRotatedPEM lays the rotated certificate out the way the kubelet does:
// kubelet-client-current.pem is a symlink to a dated file.
func writeRotatedPEM(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "var", "lib", "kubelet", "pki")
	writeContent(t, filepath.Join(dir, "kubelet-client-2026-09-24-10-00-00.pem"), "placeholder\n")
	if err := os.Symlink("kubelet-client-2026-09-24-10-00-00.pem", filepath.Join(dir, "kubelet-client-current.pem")); err != nil {
		t.Fatal(err)
	}
}

func TestInitIncomplete(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, root string)
		want  bool
	}{
		{
			name: "embedded and rotated certificate present: finalize never ran",
			setup: func(t *testing.T, root string) {
				writeContent(t, filepath.Join(root, kubeletConfRel), kubeletConfEmbedded)
				writeRotatedPEM(t, root)
			},
			want: true,
		},
		{
			name: "finalized: init completed",
			setup: func(t *testing.T, root string) {
				writeContent(t, filepath.Join(root, kubeletConfRel), kubeletConfFinalized)
				writeRotatedPEM(t, root)
			},
		},
		{
			// kubeadm skips the rewrite without error when the rotated
			// certificate is absent, so the embedded form proves nothing.
			name: "embedded but no rotated certificate: cannot tell",
			setup: func(t *testing.T, root string) {
				writeContent(t, filepath.Join(root, kubeletConfRel), kubeletConfEmbedded)
			},
		},
		{
			name: "rotated certificate is a dangling symlink: cannot tell",
			setup: func(t *testing.T, root string) {
				writeContent(t, filepath.Join(root, kubeletConfRel), kubeletConfEmbedded)
				pem := filepath.Join(root, rotatedPEMRel)
				if err := os.MkdirAll(filepath.Dir(pem), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("gone.pem", pem); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:  "kubelet.conf missing",
			setup: func(t *testing.T, root string) { writeRotatedPEM(t, root) },
		},
		{
			name: "kubelet.conf is a symlink to an embedded file: not followed",
			setup: func(t *testing.T, root string) {
				target := filepath.Join(root, "elsewhere.conf")
				writeContent(t, target, kubeletConfEmbedded)
				link := filepath.Join(root, kubeletConfRel)
				if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				writeRotatedPEM(t, root)
			},
		},
		{
			name: "kubelet.conf is a directory",
			setup: func(t *testing.T, root string) {
				if err := os.MkdirAll(filepath.Join(root, kubeletConfRel), 0o755); err != nil {
					t.Fatal(err)
				}
				writeRotatedPEM(t, root)
			},
		},
		{
			// The embedded marker sits inside the bound; the file only fails
			// because it is one byte past it, so this proves the size limit
			// itself, not the parser.
			name: "oversize kubelet.conf is refused",
			setup: func(t *testing.T, root string) {
				pad := kubeletConfMaxBytes + 1 - len(kubeletConfEmbedded) - len("#\n")
				writeContent(t, filepath.Join(root, kubeletConfRel), kubeletConfEmbedded+"#"+strings.Repeat("x", pad)+"\n")
				writeRotatedPEM(t, root)
			},
		},
		{
			name: "kubelet.conf exactly at the bound is read",
			setup: func(t *testing.T, root string) {
				pad := kubeletConfMaxBytes - len(kubeletConfEmbedded) - len("#\n")
				writeContent(t, filepath.Join(root, kubeletConfRel), kubeletConfEmbedded+"#"+strings.Repeat("x", pad)+"\n")
				writeRotatedPEM(t, root)
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			tt.setup(t, root)
			if got := initIncomplete(root); got != tt.want {
				t.Fatalf("initIncomplete = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestInitIncompleteFIFONeverBlocks plants a FIFO at kubelet.conf with no
// writer. A blocking open would hang this boot-stage probe forever; the
// probe must return "cannot tell" promptly instead.
func TestInitIncompleteFIFONeverBlocks(t *testing.T) {
	root := t.TempDir()
	writeRotatedPEM(t, root)
	fifo := filepath.Join(root, kubeletConfRel)
	if err := os.MkdirAll(filepath.Dir(fifo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() { done <- initIncomplete(root) }()
	select {
	case got := <-done:
		if got {
			t.Fatal("initIncomplete = true for a FIFO, want false")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("initIncomplete blocked on a FIFO at kubelet.conf")
	}
}

// TestReadRegularFileRefusesAFIFOSwappedInAfterLstat covers the second type
// check: the entry is a regular file at lstat time and a FIFO by the time it
// is opened. Only the fstat on the opened descriptor can catch that, and the
// O_NONBLOCK open is what keeps it from hanging first.
func TestReadRegularFileRefusesAFIFOSwappedInAfterLstat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kubelet.conf")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	// Exercise the open and fstat path directly, as if lstat had passed.
	done := make(chan bool, 1)
	go func() {
		_, ok := readRegularFileAfterLstat(path, kubeletConfMaxBytes)
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("a FIFO was read as a regular file")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the open blocked on a FIFO")
	}
}

// TestReadRegularFileRefusesASymlinkSwappedInAfterLstat is the same race
// with a symlink: the lstat gate saw a regular file, the entry is a link to
// an embedded kubelet.conf by the time it is opened. Only O_NOFOLLOW on the
// open refuses it; the lstat gate alone would have let the link be read.
func TestReadRegularFileRefusesASymlinkSwappedInAfterLstat(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.conf")
	writeContent(t, target, kubeletConfEmbedded)
	link := filepath.Join(dir, "kubelet.conf")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if data, ok := readRegularFileAfterLstat(link, kubeletConfMaxBytes); ok {
		t.Fatalf("followed a symlink and read %d bytes", len(data))
	}
}

func TestFileProberInitIncompleteOnlyForInitialized(t *testing.T) {
	t.Run("initialized node with an unfinished init", func(t *testing.T) {
		root := t.TempDir()
		writeContent(t, filepath.Join(root, "etc", "kubernetes", "admin.conf"), "apiVersion: v1\n")
		writeContent(t, filepath.Join(root, kubeletConfRel), kubeletConfEmbedded)
		writeRotatedPEM(t, root)
		s, err := FileProber{RootPath: root}.Probe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if s.Membership != Initialized || !s.InitIncomplete {
			t.Fatalf("got membership=%q initIncomplete=%v, want initialized/true", s.Membership, s.InitIncomplete)
		}
	})
	t.Run("joined node is never judged", func(t *testing.T) {
		root := t.TempDir()
		writeContent(t, filepath.Join(root, kubeletConfRel), kubeletConfEmbedded)
		writeRotatedPEM(t, root)
		s, err := FileProber{RootPath: root}.Probe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if s.Membership != Joined || s.InitIncomplete {
			t.Fatalf("got membership=%q initIncomplete=%v, want joined/false", s.Membership, s.InitIncomplete)
		}
	})
}
