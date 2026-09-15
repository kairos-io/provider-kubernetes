package imageimport

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

// validLockDoc returns a single-entry lock and its canonical byte rendering,
// which ParseLock must accept.
func validLockDoc() (lock, []byte) {
	l := lock{
		KubernetesVersion: "v1.37.0",
		ImageRepository:   "registry.k8s.io",
		VerifiedBy: lockVerifiedBy{
			Identity: "krel-trust@k8s-releng-prod.iam.gserviceaccount.com",
			Issuer:   "https://accounts.google.com",
		},
		Images: []lockImage{
			{
				Ref:          "registry.k8s.io/pause:3.10.2",
				Digest:       "sha256:" + strings.Repeat("a", 64),
				Tarball:      "registry.k8s.io_pause_3.10.2.tar",
				Verified:     true,
				VerifyReason: "",
			},
		},
	}
	return l, renderLock(l)
}

func TestParseLockAcceptsCanonicalDocument(t *testing.T) {
	l, data := validLockDoc()
	got, err := ParseLock(data)
	if err != nil {
		t.Fatalf("ParseLock: %v", err)
	}
	if got.KubernetesVersion != l.KubernetesVersion || got.ImageRepository != l.ImageRepository {
		t.Fatalf("got %+v, want %+v", got, l)
	}
	if len(got.Images) != 1 || got.Images[0].Ref != l.Images[0].Ref {
		t.Fatalf("got images %+v", got.Images)
	}
}

// TestParseLockGoldenFiles proves ParseLock accepts real bundler output for
// every currently supported minor (ADR-16-A2 O-5), byte-for-byte.
func TestParseLockGoldenFiles(t *testing.T) {
	for _, name := range []string{
		"images.lock.v1.35.8",
		"images.lock.v1.36.4",
		"images.lock.v1.37.0",
	} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile("testdata/" + name)
			if err != nil {
				t.Fatalf("read golden file: %v", err)
			}
			l, err := ParseLock(data)
			if err != nil {
				t.Fatalf("ParseLock(%s): %v", name, err)
			}
			if len(l.Images) < minLockEntries || len(l.Images) > maxLockEntries {
				t.Fatalf("%s: %d images, out of bounds", name, len(l.Images))
			}
			for _, img := range l.Images {
				if !strings.HasPrefix(img.Ref, l.ImageRepository+"/") {
					t.Errorf("%s: ref %q not under imageRepository %q", name, img.Ref, l.ImageRepository)
				}
			}
		})
	}
}

// TestParseLockRejects is ADR-16-A2 O-5's negative-test set: each variant
// must be refused as lock-invalid (a non-nil ParseLock error), never
// partially accepted.
func TestParseLockRejects(t *testing.T) {
	_, valid := validLockDoc()

	replaceOnce := func(data []byte, old, new string) []byte {
		return []byte(strings.Replace(string(data), old, new, 1))
	}

	cases := map[string][]byte{
		"CRLF":          bytes.ReplaceAll(valid, []byte("\n"), []byte("\r\n")),
		"BOM":           append([]byte{0xEF, 0xBB, 0xBF}, valid...),
		"trailing data": append(append([]byte{}, valid...), '\n', '{', '}'),
		"duplicate key": replaceOnce(valid,
			`"kubernetesVersion": "v1.37.0",`,
			`"kubernetesVersion": "v1.37.0",`+"\n"+`  "kubernetesVersion": "v1.37.0",`),
		"case-variant key": replaceOnce(valid, `"kubernetesVersion"`, `"KubernetesVersion"`),
		"backslash-u escape in a string": replaceOnce(valid,
			`"registry.k8s.io/pause:3.10.2"`, "\"registry.k8s.io/pa\\u0075se:3.10.2\""),
		"reordered keys": replaceOnce(
			replaceOnce(valid, `"kubernetesVersion": "v1.37.0",`+"\n"+`  "imageRepository": "registry.k8s.io",`,
				`__SWAP__`),
			`__SWAP__`,
			`"imageRepository": "registry.k8s.io",`+"\n"+`  "kubernetesVersion": "v1.37.0",`),
		"extra field": replaceOnce(valid, `"kubernetesVersion": "v1.37.0",`,
			`"kubernetesVersion": "v1.37.0",`+"\n"+`  "extra": "x",`),
		"oversize lock": bytes.Repeat([]byte("x"), maxLockSize+1),
	}

	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseLock(data); err == nil {
				t.Fatalf("ParseLock accepted the %s variant, want an error\n--- data ---\n%s", name, data)
			}
		})
	}
}

// TestParseLockRejectsDuplicateRefAndTarball covers the two distinct
// duplicate-identity cases: the same ref repeated (which also collides its
// derived tarball), and two DIFFERENT refs whose '/'/':' -> '_' substitution
// happens to derive the identical tarball name.
func TestParseLockRejectsDuplicateRefAndTarball(t *testing.T) {
	base := func() lock {
		l, _ := validLockDoc()
		return l
	}

	t.Run("duplicate ref", func(t *testing.T) {
		l := base()
		l.Images = append(l.Images, l.Images[0])
		if _, err := ParseLock(renderLock(l)); err == nil {
			t.Fatal("want an error for a duplicated ref")
		}
	})

	t.Run("duplicate tarball via different refs", func(t *testing.T) {
		l := base()
		l.Images = []lockImage{
			{Ref: "registry.k8s.io/a:b_c", Digest: "sha256:" + strings.Repeat("a", 64),
				Tarball: "registry.k8s.io_a_b_c.tar", Verified: true, VerifyReason: ""},
			{Ref: "registry.k8s.io/a_b:c", Digest: "sha256:" + strings.Repeat("b", 64),
				Tarball: "registry.k8s.io_a_b_c.tar", Verified: true, VerifyReason: ""},
		}
		if _, err := ParseLock(renderLock(l)); err == nil {
			t.Fatal("want an error for two refs deriving the same tarball name")
		}
	})
}

// TestParseLockRejectsEntryCountBounds covers O-5's 0/33-entry cases.
func TestParseLockRejectsEntryCountBounds(t *testing.T) {
	t.Run("0 entries", func(t *testing.T) {
		l, _ := validLockDoc()
		l.Images = nil
		if _, err := ParseLock(renderLock(l)); err == nil {
			t.Fatal("want an error for 0 images")
		}
	})

	t.Run("33 entries", func(t *testing.T) {
		l, _ := validLockDoc()
		l.Images = nil
		for i := 0; i < maxLockEntries+1; i++ {
			tag := fmt.Sprintf("t%d", i)
			ref := "registry.k8s.io/x:" + tag
			l.Images = append(l.Images, lockImage{
				Ref: ref, Digest: fmt.Sprintf("sha256:%064d", i),
				Tarball: tarballForRef(ref), Verified: true, VerifyReason: "",
			})
		}
		if _, err := ParseLock(renderLock(l)); err == nil {
			t.Fatal("want an error for 33 images")
		}
	})
}

// TestParseLockRejectsBadValues is O-5's per-field value-rule negative set.
func TestParseLockRejectsBadValues(t *testing.T) {
	mutate := func(f func(*lock)) []byte {
		l, _ := validLockDoc()
		f(&l)
		return renderLock(l)
	}

	cases := map[string][]byte{
		"bad ref (uppercase path)": mutate(func(l *lock) {
			l.Images[0].Ref = "registry.k8s.io/Pause:3.10.2"
			l.Images[0].Tarball = tarballForRef(l.Images[0].Ref)
		}),
		"ref not under imageRepository": mutate(func(l *lock) {
			l.Images[0].Ref = "quay.io/pause:3.10.2"
			l.Images[0].Tarball = tarballForRef(l.Images[0].Ref)
		}),
		"bad digest (not 64 hex)": mutate(func(l *lock) {
			l.Images[0].Digest = "sha256:deadbeef"
		}),
		"bad digest (missing prefix)": mutate(func(l *lock) {
			l.Images[0].Digest = strings.Repeat("a", 64)
		}),
		"tarball does not match ref": mutate(func(l *lock) {
			l.Images[0].Tarball = "something-else.tar"
		}),
		"bad kubernetesVersion (no leading v)": mutate(func(l *lock) {
			l.KubernetesVersion = "1.37.0"
		}),
		"bad imageRepository (no host)": mutate(func(l *lock) {
			l.ImageRepository = "myregistry"
			l.Images[0].Ref = "myregistry/pause:3.10.2"
			l.Images[0].Tarball = tarballForRef(l.Images[0].Ref)
		}),
		"verified true with non-empty reason": mutate(func(l *lock) {
			l.Images[0].VerifyReason = "no-upstream-signature"
		}),
		"verified false with empty reason": mutate(func(l *lock) {
			l.Images[0].Verified = false
		}),
		"verified false with wrong reason": mutate(func(l *lock) {
			l.Images[0].Verified = false
			l.Images[0].VerifyReason = "not-signed"
		}),
	}

	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseLock(data); err == nil {
				t.Fatalf("ParseLock accepted the %s variant, want an error\n--- data ---\n%s", name, data)
			}
		})
	}
}

func TestTarballForRef(t *testing.T) {
	cases := map[string]string{
		"registry.k8s.io/pause:3.10.2":            "registry.k8s.io_pause_3.10.2.tar",
		"registry.k8s.io/coredns/coredns:v1.14.6": "registry.k8s.io_coredns_coredns_v1.14.6.tar",
	}
	for ref, want := range cases {
		if got := tarballForRef(ref); got != want {
			t.Errorf("tarballForRef(%q) = %q, want %q", ref, got, want)
		}
	}
}
