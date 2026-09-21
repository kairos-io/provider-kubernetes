package imageimport

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// tarMember is one archive/tar entry to write via buildTar.
type tarMember struct {
	name     string
	typeflag byte
	content  []byte
	linkname string
	pax      map[string]string
}

// buildTar writes members as a tar archive (in order) and returns its bytes.
// t is testing.TB so both *testing.T (table tests) and *testing.F (fuzz
// seeding) can call it.
func buildTar(t testing.TB, members []tarMember) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, m := range members {
		typeflag := m.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		// Only TypeReg/TypeCont carry data in a real tar archive; a symlink,
		// hardlink or directory header must declare Size 0 regardless of
		// what content the test case happened to set.
		hasData := typeflag == tar.TypeReg || typeflag == tar.TypeCont
		size := int64(0)
		if hasData {
			size = int64(len(m.content))
		}
		hdr := &tar.Header{
			Name:     m.name,
			Typeflag: typeflag,
			Size:     size,
			Mode:     0o644,
			Linkname: m.linkname,
		}
		if len(m.pax) > 0 {
			hdr.PAXRecords = m.pax
			hdr.Format = tar.FormatPAX
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", m.name, err)
		}
		if hasData && len(m.content) > 0 {
			if _, err := tw.Write(m.content); err != nil {
				t.Fatalf("write content %q: %v", m.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

const testRef = "registry.k8s.io/pause:3.10.2"

// validTarFixture returns a minimal, valid three-member tarball (config,
// layer, manifest.json) for testRef, plus its config/layer member names.
func validTarFixture() (members []tarMember, configName, layerName string, manifest []byte) {
	configContent := []byte("this-is-the-config-blob")
	configName = "sha256:" + sha256Hex(configContent)
	layerName = strings.Repeat("b", 64) + ".tar.gz"
	manifest = []byte(`[{"Config":"` + configName + `","RepoTags":["` + testRef + `"],"Layers":["` + layerName + `"]}]`)
	members = []tarMember{
		{name: configName, content: configContent},
		{name: layerName, content: []byte("this-is-a-layer-blob")},
		{name: manifestName, content: manifest},
	}
	return members, configName, layerName, manifest
}

func TestCheckTarAcceptsValidTarball(t *testing.T) {
	members, _, _, _ := validTarFixture()
	data := buildTar(t, members)
	if err := CheckTar(bytes.NewReader(data), testRef); err != nil {
		t.Fatalf("CheckTar: %v", err)
	}
}

func TestCheckTarAcceptsRepeatedLayerInManifest(t *testing.T) {
	// A manifest's Layers list may repeat a physical member's name (the same
	// layer appearing twice in the chain); the physical tar member itself is
	// still stored exactly once.
	members, configName, layerName, _ := validTarFixture()
	manifest := []byte(`[{"Config":"` + configName + `","RepoTags":["` + testRef + `"],"Layers":["` +
		layerName + `","` + layerName + `"]}]`)
	members[2].content = manifest
	data := buildTar(t, members)
	if err := CheckTar(bytes.NewReader(data), testRef); err != nil {
		t.Fatalf("CheckTar: %v", err)
	}
}

// TestCheckTarRejects is the Go analog of bundle-images.sh's
// validate_image_tar b01-b20 negative cases (ADR-16-A2 O-6).
func TestCheckTarRejects(t *testing.T) {
	sixtyFourB := strings.Repeat("c", 64)

	cases := []struct {
		name    string
		build   func() []tarMember
		wantErr error // nil = only require *some* error (errors.Is checked when non-nil)
	}{
		{
			name: "duplicate member",
			build: func() []tarMember {
				m, _, _, _ := validTarFixture()
				// Duplicate the layer member (same name) before manifest.json.
				dup := m[1]
				return []tarMember{m[0], m[1], dup, m[2]}
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "symlink member",
			build: func() []tarMember {
				m, _, _, _ := validTarFixture()
				m[1] = tarMember{name: m[1].name, typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}
				return m
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "hardlink member",
			build: func() []tarMember {
				m, configName, _, _ := validTarFixture()
				m[1] = tarMember{name: m[1].name, typeflag: tar.TypeLink, linkname: configName}
				return m
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "directory member",
			build: func() []tarMember {
				m, _, layerName, _ := validTarFixture()
				m[1] = tarMember{name: layerName, typeflag: tar.TypeDir}
				return m
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "type-7 (contiguous file) member",
			build: func() []tarMember {
				m, _, _, _ := validTarFixture()
				m[1].typeflag = tar.TypeCont
				return m
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "'./' prefixed name",
			build: func() []tarMember {
				m, _, layerName, _ := validTarFixture()
				m[1].name = "./" + layerName
				return m
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "'../' prefixed name",
			build: func() []tarMember {
				m, _, layerName, _ := validTarFixture()
				m[1].name = "../" + layerName
				return m
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "'/' prefixed name",
			build: func() []tarMember {
				m, _, layerName, _ := validTarFixture()
				m[1].name = "/" + layerName
				return m
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "manifest.json not last",
			build: func() []tarMember {
				m, _, _, _ := validTarFixture()
				extra := tarMember{name: strings.Repeat("d", 64) + ".tar.gz", content: []byte("x")}
				return []tarMember{m[0], m[1], m[2], extra} // manifest.json (m[2]) is now not last
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "two config members",
			build: func() []tarMember {
				m, _, _, _ := validTarFixture()
				secondConfig := tarMember{name: "sha256:" + sha256Hex([]byte("other-config")), content: []byte("other-config")}
				return []tarMember{m[0], secondConfig, m[1], m[2]}
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "manifest trailing newline",
			build: func() []tarMember {
				m, _, _, manifest := validTarFixture()
				m[2].content = append(append([]byte{}, manifest...), '\n')
				return m
			},
			wantErr: ErrManifest,
		},
		{
			name: "manifest case-variant key",
			build: func() []tarMember {
				m, configName, layerName, _ := validTarFixture()
				m[2].content = []byte(`[{"Config":"` + configName + `","repoTags":["` + testRef + `"],"Layers":["` + layerName + `"]}]`)
				return m
			},
			wantErr: ErrManifest,
		},
		{
			name: "RepoTags mismatch",
			build: func() []tarMember {
				m, configName, layerName, _ := validTarFixture()
				m[2].content = []byte(`[{"Config":"` + configName + `","RepoTags":["registry.k8s.io/other:1.0"],"Layers":["` + layerName + `"]}]`)
				return m
			},
			wantErr: ErrManifest,
		},
		{
			name: "extra key in manifest",
			build: func() []tarMember {
				m, configName, layerName, _ := validTarFixture()
				m[2].content = []byte(`[{"Config":"` + configName + `","RepoTags":["` + testRef +
					`"],"Layers":["` + layerName + `"],"Extra":"x"}]`)
				return m
			},
			wantErr: ErrManifest,
		},
		{
			name: "Layers set mismatch (extra name not a member)",
			build: func() []tarMember {
				m, configName, layerName, _ := validTarFixture()
				m[2].content = []byte(`[{"Config":"` + configName + `","RepoTags":["` + testRef +
					`"],"Layers":["` + layerName + `","` + sixtyFourB + `.tar.gz"]}]`)
				return m
			},
			wantErr: ErrManifest,
		},
		{
			name: "Layers set mismatch (member omitted)",
			build: func() []tarMember {
				m, configName, _, _ := validTarFixture()
				m[2].content = []byte(`[{"Config":"` + configName + `","RepoTags":["` + testRef + `"],"Layers":[]}]`)
				return m
			},
			wantErr: ErrManifest,
		},
		{
			name: "oversize manifest",
			build: func() []tarMember {
				m, configName, layerName, _ := validTarFixture()
				pad := strings.Repeat("z", manifestMaxSize)
				m[2].content = []byte(`[{"Config":"` + configName + `","RepoTags":["` + testRef +
					pad + `"],"Layers":["` + layerName + `"]}]`)
				return m
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "PAX record on the config member",
			build: func() []tarMember {
				m, _, _, _ := validTarFixture()
				m[0].pax = map[string]string{"comment": "injected"}
				return m
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "config hash mismatch",
			build: func() []tarMember {
				m, _, _, manifest := validTarFixture()
				m[0].content = []byte("tampered-config-content")
				m[2].content = manifest // manifest still names the ORIGINAL (untampered) hash
				return m
			},
			wantErr: ErrConfigDigest,
		},
		{
			name: "too few members (no layer)",
			build: func() []tarMember {
				m, configName, _, _ := validTarFixture()
				manifest := []byte(`[{"Config":"` + configName + `","RepoTags":["` + testRef + `"],"Layers":[]}]`)
				return []tarMember{m[0], {name: manifestName, content: manifest}}
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "unexpected member name",
			build: func() []tarMember {
				m, _, _, _ := validTarFixture()
				m[1].name = "not-a-valid-name.txt"
				return m
			},
			wantErr: ErrTarStructure,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := buildTar(t, c.build())
			err := CheckTar(bytes.NewReader(data), testRef)
			if err == nil {
				t.Fatalf("CheckTar accepted the %q variant, want an error", c.name)
			}
			if c.wantErr != nil && !errors.Is(err, c.wantErr) {
				t.Fatalf("CheckTar error = %v, want errors.Is(err, %v)", err, c.wantErr)
			}
		})
	}
}

// FuzzTarCheck exercises CheckTar against arbitrary byte inputs (ADR-16-A2
// O-6): it must never panic and must always terminate promptly, whatever the
// bytes contain -- CheckTar runs on data that has already passed the O-3
// owner/mode/device/size checks but is otherwise attacker-controlled bundle
// content.
func FuzzTarCheck(f *testing.F) {
	members, _, _, _ := validTarFixture()
	f.Add(buildTar(f, members))

	members2, configName2, layerName2, _ := validTarFixture()
	_ = configName2
	manifest2 := []byte(`[{"Config":"` + configName2 + `","RepoTags":["` + testRef + `"],"Layers":["` +
		layerName2 + `","` + layerName2 + `"]}]`)
	members2[2].content = manifest2
	f.Add(buildTar(f, members2))

	f.Fuzz(func(t *testing.T, data []byte) {
		_ = CheckTar(bytes.NewReader(data), testRef)
	})
}
