package imageimport

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
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

const testRef = "registry.k8s.io/pause:3.10.2"

// ociFixture is a deterministic, valid canonical-OCI-layout image for ref:
// one config and nLayers layers behind a docker schema2 manifest, exactly
// the shape build/bundle-images.sh produces.
type ociFixture struct {
	ref         string
	arch        string
	indexMT     string // the index descriptor's mediaType
	platform    bool   // include the platform member
	annotations string // raw JSON object body for "annotations" (without braces)

	config      []byte
	configHex   string
	layers      [][]byte
	layerHex    []string
	manifest    []byte
	manifestHex string
	index       []byte
}

func newOCIFixture(ref, seed string, nLayers int) *ociFixture {
	f := &ociFixture{
		ref: ref, arch: bundleArch, indexMT: mediaTypeDockerManifest, platform: true,
		annotations: `"` + annotationRefName + `":"` + ref + `"`,
	}
	f.config = []byte("config-for-" + ref + "-" + seed)
	f.configHex = sha256Hex(f.config)
	var layerDescs []string
	for i := 0; i < nLayers; i++ {
		l := []byte("layer-" + strconv.Itoa(i) + "-" + seed)
		f.layers = append(f.layers, l)
		f.layerHex = append(f.layerHex, sha256Hex(l))
		layerDescs = append(layerDescs, `{"mediaType":"application/vnd.docker.image.rootfs.diff.tar.gzip","size":`+
			strconv.Itoa(len(l))+`,"digest":"sha256:`+sha256Hex(l)+`"}`)
	}
	f.manifest = []byte(`{"schemaVersion":2,"mediaType":"` + mediaTypeDockerManifest + `","config":{"mediaType":"application/vnd.docker.container.image.v1+json","size":` +
		strconv.Itoa(len(f.config)) + `,"digest":"sha256:` + f.configHex + `"},"layers":[` + strings.Join(layerDescs, ",") + `]}`)
	f.manifestHex = sha256Hex(f.manifest)
	f.reindex()
	return f
}

// reindex re-renders index.json from the fixture's current fields.
func (f *ociFixture) reindex() {
	plat := ""
	if f.platform {
		plat = `"platform":{"architecture":"` + f.arch + `","os":"linux"},`
	}
	f.index = []byte(`{"schemaVersion":2,"mediaType":"` + mediaTypeOCIIndex + `","manifests":[{"mediaType":"` + f.indexMT +
		`","size":` + strconv.Itoa(len(f.manifest)) + `,"digest":"sha256:` + f.manifestHex + `",` + plat +
		`"annotations":{` + f.annotations + `}}]}`)
}

func (f *ociFixture) manifestDigest() string { return "sha256:" + f.manifestHex }

// withManifest swaps the manifest bytes (keeping index.json consistent).
func (f *ociFixture) withManifest(m []byte) *ociFixture {
	f.manifest = m
	f.manifestHex = sha256Hex(m)
	f.reindex()
	return f
}

func (f *ociFixture) members() []tarMember {
	ms := []tarMember{
		{name: ociLayoutName, content: []byte(ociLayoutBytes)},
		{name: indexName, content: f.index},
		{name: blobPrefix + f.manifestHex, content: f.manifest},
		{name: blobPrefix + f.configHex, content: f.config},
	}
	for i, l := range f.layers {
		ms = append(ms, tarMember{name: blobPrefix + f.layerHex[i], content: l})
	}
	return ms
}

func (f *ociFixture) tar(t testing.TB) []byte { return buildTar(t, f.members()) }

func TestCheckTarAcceptsValidTarball(t *testing.T) {
	f := newOCIFixture(testRef, "a", 3)
	data := f.tar(t)
	if err := CheckTar(bytes.NewReader(data), testRef); err != nil {
		t.Fatalf("CheckTar: %v", err)
	}
	if err := CheckTarPinned(bytes.NewReader(data), testRef, f.manifestDigest()); err != nil {
		t.Fatalf("CheckTarPinned: %v", err)
	}
}

func TestCheckTarAcceptsVariants(t *testing.T) {
	t.Run("OCI manifest media type", func(t *testing.T) {
		f := newOCIFixture(testRef, "a", 2)
		m := strings.Replace(string(f.manifest), mediaTypeDockerManifest, mediaTypeOCIManifest, 1)
		f.indexMT = mediaTypeOCIManifest
		f.withManifest([]byte(m))
		if err := CheckTar(bytes.NewReader(f.tar(t)), testRef); err != nil {
			t.Fatalf("CheckTar: %v", err)
		}
	})
	t.Run("OCI manifest without a mediaType field", func(t *testing.T) {
		f := newOCIFixture(testRef, "a", 2)
		m := strings.Replace(string(f.manifest), `"mediaType":"`+mediaTypeDockerManifest+`",`, "", 1)
		f.indexMT = mediaTypeOCIManifest
		f.withManifest([]byte(m))
		if err := CheckTar(bytes.NewReader(f.tar(t)), testRef); err != nil {
			t.Fatalf("CheckTar: %v", err)
		}
	})
	t.Run("platform absent", func(t *testing.T) {
		f := newOCIFixture(testRef, "a", 1)
		f.platform = false
		f.reindex()
		if err := CheckTar(bytes.NewReader(f.tar(t)), testRef); err != nil {
			t.Fatalf("CheckTar: %v", err)
		}
	})
	t.Run("layer repeated in the manifest is stored once", func(t *testing.T) {
		f := newOCIFixture(testRef, "a", 2)
		var doc map[string]any
		if err := json.Unmarshal(f.manifest, &doc); err != nil {
			t.Fatal(err)
		}
		layers := doc["layers"].([]any)
		doc["layers"] = append(layers, layers[0])
		m, _ := json.Marshal(doc)
		f.withManifest(m)
		if err := CheckTar(bytes.NewReader(f.tar(t)), testRef); err != nil {
			t.Fatalf("CheckTar: %v", err)
		}
	})
	t.Run("layer content is not hashed", func(t *testing.T) {
		// A layer whose bytes do not match its path is deliberately accepted:
		// containerd's content store is keyed by the computed digest, so it is
		// unreachable and the import fails closed.
		f := newOCIFixture(testRef, "a", 2)
		f.layers[1] = []byte("layer-1-a")
		ms := f.members()
		ms[5].content = []byte("layer-1-a") // same size, different bytes
		ms[5].content[0] = 'L'
		if err := CheckTar(bytes.NewReader(buildTar(t, ms)), testRef); err != nil {
			t.Fatalf("CheckTar: %v", err)
		}
	})
}

// TestCheckTarRejects holds one negative case per hazard of the OCI contract
// (ADR-16-A3 / ADR-16-A2 O-6).
func TestCheckTarRejects(t *testing.T) {
	type row struct {
		name    string
		build   func() []tarMember
		pin     string // non-empty overrides the fixture's own digest as the pin
		wantErr error
	}
	fix := func() *ociFixture { return newOCIFixture(testRef, "a", 2) }
	rows := []row{
		{
			name: "io.containerd.image.name annotation present",
			build: func() []tarMember {
				f := fix()
				f.annotations = `"io.containerd.image.name":"docker.io/evil/x:1","` + annotationRefName + `":"` + testRef + `"`
				f.reindex()
				return f.members()
			},
			wantErr: ErrManifest,
		},
		{
			name: "io.containerd.image.name annotation alone",
			build: func() []tarMember {
				f := fix()
				f.annotations = `"io.containerd.image.name":"` + testRef + `"`
				f.reindex()
				return f.members()
			},
			wantErr: ErrManifest,
		},
		{
			name: "second annotation key",
			build: func() []tarMember {
				f := fix()
				f.annotations += `,"org.example/x":"y"`
				f.reindex()
				return f.members()
			},
			wantErr: ErrManifest,
		},
		{
			name: "no annotation",
			build: func() []tarMember {
				f := fix()
				f.annotations = ""
				f.reindex()
				return f.members()
			},
			wantErr: ErrManifest,
		},
		{
			name: "ref.name names another ref",
			build: func() []tarMember {
				f := fix()
				f.annotations = `"` + annotationRefName + `":"registry.k8s.io/other:1.0"`
				f.reindex()
				return f.members()
			},
			wantErr: ErrManifest,
		},
		{
			name: "non-matching platform architecture",
			build: func() []tarMember {
				f := fix()
				f.arch = "s390x"
				f.reindex()
				return f.members()
			},
			wantErr: ErrManifest,
		},
		{
			name: "index mediaType in the descriptor",
			build: func() []tarMember {
				f := fix()
				f.indexMT = mediaTypeOCIIndex
				f.reindex()
				return f.members()
			},
			wantErr: ErrManifest,
		},
		{
			name: "descriptor mediaType outside the allowlist",
			build: func() []tarMember {
				f := fix()
				f.indexMT = "application/octet-stream"
				f.reindex()
				return f.members()
			},
			wantErr: ErrManifest,
		},
		{
			name: "index.json pretty-printed (not byte-exact)",
			build: func() []tarMember {
				f := fix()
				var buf bytes.Buffer
				if err := json.Indent(&buf, f.index, "", "  "); err != nil {
					t.Fatal(err)
				}
				ms := f.members()
				ms[1].content = buf.Bytes()
				return ms
			},
			wantErr: ErrManifest,
		},
		{
			name: "index.json trailing newline",
			build: func() []tarMember {
				f := fix()
				ms := f.members()
				ms[1].content = append(append([]byte{}, f.index...), '\n')
				return ms
			},
			wantErr: ErrManifest,
		},
		{
			name: "index.json unknown field",
			build: func() []tarMember {
				f := fix()
				ms := f.members()
				ms[1].content = []byte(strings.Replace(string(f.index), `"schemaVersion":2,`, `"schemaVersion":2,"extra":1,`, 1))
				return ms
			},
			wantErr: ErrManifest,
		},
		{
			name: "two manifest descriptors",
			build: func() []tarMember {
				f := fix()
				ms := f.members()
				i := string(f.index)
				start := strings.Index(i, `"manifests":[`) + len(`"manifests":[`)
				end := strings.LastIndex(i, `]`)
				d := i[start:end]
				ms[1].content = []byte(i[:start] + d + "," + d + i[end:])
				return ms
			},
			wantErr: ErrManifest,
		},
		{
			name: "index.json garbage",
			build: func() []tarMember {
				ms := fix().members()
				ms[1].content = []byte("not json")
				return ms
			},
			wantErr: ErrManifest,
		},
		{
			name: "oversize index.json",
			build: func() []tarMember {
				ms := fix().members()
				ms[1].content = bytes.Repeat([]byte{' '}, indexMaxSize+1)
				return ms
			},
			wantErr: ErrTarStructure,
		},
		{
			name:    "manifestDigest mismatch against the pin",
			pin:     "sha256:" + strings.Repeat("0", 64),
			build:   func() []tarMember { return fix().members() },
			wantErr: ErrManifest,
		},
		{
			name: "manifest blob bytes do not match its path",
			build: func() []tarMember {
				ms := fix().members()
				ms[2].content = append([]byte(nil), ms[2].content...)
				ms[2].content[2] ^= 0x20 // same size, different bytes
				return ms
			},
			wantErr: ErrManifest,
		},
		{
			name: "config blob bytes do not match its path",
			build: func() []tarMember {
				ms := fix().members()
				ms[3].content = append([]byte(nil), ms[3].content...)
				ms[3].content[0] ^= 0x20
				return ms
			},
			wantErr: ErrConfigDigest,
		},
		{
			name: "manifest.json member alongside oci-layout",
			build: func() []tarMember {
				return append(fix().members(), tarMember{name: legacyManifestName, content: []byte(`[]`)})
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "manifest.json member first",
			build: func() []tarMember {
				return append([]tarMember{{name: legacyManifestName, content: []byte(`[]`)}}, fix().members()...)
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "./index.json duplicate after clean",
			build: func() []tarMember {
				ms := fix().members()
				dup := tarMember{name: "./" + indexName, content: ms[1].content}
				return append(ms[:2], append([]tarMember{dup}, ms[2:]...)...)
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "blob duplicate after clean (blobs/sha256//hex)",
			build: func() []tarMember {
				ms := fix().members()
				dup := tarMember{name: "blobs/sha256//" + strings.TrimPrefix(ms[2].name, blobPrefix), content: ms[2].content}
				return append(ms[:3], append([]tarMember{dup}, ms[3:]...)...)
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "exact duplicate layer member",
			build: func() []tarMember {
				ms := fix().members()
				return append(ms, ms[len(ms)-1])
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "oci-layout compact (not the canonical bytes)",
			build: func() []tarMember {
				ms := fix().members()
				ms[0].content = []byte(`{"imageLayoutVersion":"1.0.0"}`)
				return ms
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "oci-layout wrong version",
			build: func() []tarMember {
				ms := fix().members()
				ms[0].content = []byte(strings.Replace(ociLayoutBytes, "1.0.0", "1.0.1", 1))
				return ms
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "members out of order (index.json first)",
			build: func() []tarMember {
				ms := fix().members()
				ms[0], ms[1] = ms[1], ms[0]
				return ms
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "layers out of manifest order",
			build: func() []tarMember {
				ms := fix().members()
				ms[4], ms[5] = ms[5], ms[4]
				return ms
			},
			wantErr: ErrManifest,
		},
		{
			name: "layer named by the manifest is missing",
			build: func() []tarMember {
				ms := fix().members()
				return ms[:len(ms)-1]
			},
			wantErr: ErrManifest,
		},
		{
			name: "extra blob not named by the manifest",
			build: func() []tarMember {
				ms := fix().members()
				return append(ms, tarMember{name: blobPrefix + strings.Repeat("c", 64), content: []byte("x")})
			},
			wantErr: ErrManifest,
		},
		{
			name: "layer size differs from the manifest's declared size",
			build: func() []tarMember {
				ms := fix().members()
				ms[4].content = append(append([]byte{}, ms[4].content...), 'x')
				return ms
			},
			wantErr: ErrManifest,
		},
		{
			name: "manifest names zero layers",
			build: func() []tarMember {
				f := newOCIFixture(testRef, "a", 0)
				return f.members()
			},
			wantErr: ErrManifest,
		},
		{
			name: "manifest wrong schemaVersion",
			build: func() []tarMember {
				f := fix()
				f.withManifest([]byte(strings.Replace(string(f.manifest), `"schemaVersion":2`, `"schemaVersion":1`, 1)))
				return f.members()
			},
			wantErr: ErrManifest,
		},
		{
			name: "manifest mediaType disagrees with the index descriptor",
			build: func() []tarMember {
				f := fix()
				f.withManifest([]byte(strings.Replace(string(f.manifest), mediaTypeDockerManifest, mediaTypeOCIManifest, 1)))
				return f.members()
			},
			wantErr: ErrManifest,
		},
		{
			name: "manifest config digest malformed",
			build: func() []tarMember {
				f := fix()
				f.withManifest([]byte(strings.Replace(string(f.manifest), `"sha256:`+f.configHex, `"sha256:XYZ`, 1)))
				return f.members()
			},
			wantErr: ErrManifest,
		},
		{
			name: "manifest is not JSON",
			build: func() []tarMember {
				f := fix()
				f.withManifest([]byte("not json"))
				return f.members()
			},
			wantErr: ErrManifest,
		},
		{
			name: "symlink member",
			build: func() []tarMember {
				ms := fix().members()
				ms[5] = tarMember{name: ms[5].name, typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}
				return ms
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "hardlink member",
			build: func() []tarMember {
				ms := fix().members()
				ms[5] = tarMember{name: ms[5].name, typeflag: tar.TypeLink, linkname: ms[4].name}
				return ms
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "directory member",
			build: func() []tarMember {
				ms := fix().members()
				return append([]tarMember{{name: "blobs/", typeflag: tar.TypeDir}}, ms...)
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "type-7 (contiguous file) member",
			build: func() []tarMember {
				ms := fix().members()
				ms[5].typeflag = tar.TypeCont
				return ms
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "'./' prefixed name",
			build: func() []tarMember {
				ms := fix().members()
				ms[5].name = "./" + ms[5].name
				return ms
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "'../' prefixed name",
			build: func() []tarMember {
				ms := fix().members()
				ms[5].name = "../" + ms[5].name
				return ms
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "'/' prefixed name",
			build: func() []tarMember {
				ms := fix().members()
				ms[5].name = "/" + ms[5].name
				return ms
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "'..' element inside a name",
			build: func() []tarMember {
				ms := fix().members()
				ms[5].name = "blobs/sha256/../sha256/" + strings.TrimPrefix(ms[5].name, blobPrefix)
				return ms
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "backslash in a name",
			build: func() []tarMember {
				ms := fix().members()
				ms[5].name = `blobs\sha256\` + strings.TrimPrefix(ms[5].name, blobPrefix)
				return ms
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "uppercase hex blob name",
			build: func() []tarMember {
				ms := fix().members()
				ms[5].name = blobPrefix + strings.ToUpper(strings.TrimPrefix(ms[5].name, blobPrefix))
				return ms
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "other digest algorithm directory",
			build: func() []tarMember {
				ms := fix().members()
				ms[5].name = "blobs/sha512/" + strings.TrimPrefix(ms[5].name, blobPrefix)
				return ms
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "unexpected member name",
			build: func() []tarMember {
				ms := fix().members()
				return append(ms, tarMember{name: "not-a-valid-name.txt", content: []byte("x")})
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "PAX record on the config member",
			build: func() []tarMember {
				ms := fix().members()
				ms[3].pax = map[string]string{"comment": "injected"}
				return ms
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "PAX xattr on a layer member",
			build: func() []tarMember {
				ms := fix().members()
				ms[4].pax = map[string]string{"SCHILY.xattr.security.capability": "x"}
				return ms
			},
			wantErr: ErrTarStructure,
		},
		{
			name: "too few members",
			build: func() []tarMember {
				return fix().members()[:2]
			},
			wantErr: ErrTarStructure,
		},
		{
			name:    "empty archive",
			build:   func() []tarMember { return nil },
			wantErr: ErrTarStructure,
		},
		{
			name: "manifest implies more members than the bound",
			build: func() []tarMember {
				f := newOCIFixture(testRef, "a", maxTarMembers-3)
				return f.members()
			},
			wantErr: ErrManifest,
		},
		{
			name: "far more blobs than the manifest names",
			build: func() []tarMember {
				ms := fix().members()
				for i := 0; i < maxTarMembers; i++ {
					ms = append(ms, tarMember{name: blobPrefix + sha256Hex([]byte(strconv.Itoa(i))), content: []byte("x")})
				}
				return ms
			},
			wantErr: ErrManifest,
		},
		{
			name: "oversize config blob",
			build: func() []tarMember {
				f := fix()
				f.config = bytes.Repeat([]byte{'c'}, configMaxSize+1)
				f.configHex = sha256Hex(f.config)
				// Re-render a manifest that honestly names the oversize config.
				f.withManifest([]byte(`{"schemaVersion":2,"mediaType":"` + mediaTypeDockerManifest + `","config":{"size":` +
					strconv.Itoa(len(f.config)) + `,"digest":"sha256:` + f.configHex + `"},"layers":[{"size":1,"digest":"sha256:` + f.layerHex[0] + `"}]}`))
				return f.members()[:5]
			},
			wantErr: ErrManifest,
		},
		{
			name: "oversize manifest blob",
			build: func() []tarMember {
				f := fix()
				f.withManifest(bytes.Repeat([]byte{' '}, manifestMaxSize+1))
				return f.members()
			},
			wantErr: ErrManifest,
		},
	}

	for _, c := range rows {
		t.Run(c.name, func(t *testing.T) {
			data := buildTar(t, c.build())
			var err error
			if c.pin != "" {
				err = CheckTarPinned(bytes.NewReader(data), testRef, c.pin)
			} else {
				err = CheckTar(bytes.NewReader(data), testRef)
			}
			if err == nil {
				t.Fatalf("CheckTar accepted the %q variant, want an error", c.name)
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("CheckTar error = %v, want errors.Is(err, %v)", err, c.wantErr)
			}
		})
	}
}

func TestCheckMemberNameRejectsTrailingSlash(t *testing.T) {
	// archive/tar cannot write a regular-file header with a trailing slash, so
	// the name rule is exercised directly.
	for _, name := range []string{"blobs/", "blobs/sha256/", indexName + "/", ""} {
		if err := checkMemberName(name); !errors.Is(err, ErrTarStructure) {
			t.Errorf("checkMemberName(%q) = %v, want ErrTarStructure", name, err)
		}
	}
	if err := checkMemberName("index.json\x00"); !errors.Is(err, ErrTarStructure) {
		t.Errorf("NUL-bearing name = %v, want ErrTarStructure", err)
	}
}

func TestCheckTarPinnedRejectsMalformedPin(t *testing.T) {
	data := newOCIFixture(testRef, "a", 1).tar(t)
	if err := CheckTarPinned(bytes.NewReader(data), testRef, "sha256:short"); !errors.Is(err, ErrManifest) {
		t.Fatalf("err = %v, want ErrManifest", err)
	}
}

// TestCheckTarTrailingGarbageAfterEndMarker documents that bytes after the tar
// end-of-archive marker are not part of the archive: tar.Reader (like
// containerd's importer) stops at the marker.
func TestCheckTarEndsAtArchiveMarker(t *testing.T) {
	data := newOCIFixture(testRef, "a", 1).tar(t)
	if err := CheckTar(bytes.NewReader(append(data, []byte("trailing")...)), testRef); err != nil {
		t.Fatalf("CheckTar: %v", err)
	}
}

// FuzzTarCheck exercises CheckTarPinned against arbitrary byte inputs (ADR-16-A2
// O-6): it must never panic and must always terminate promptly, whatever the
// bytes contain -- CheckTarPinned runs on data that has already passed the O-3
// owner/mode/device/size checks but is otherwise attacker-controlled bundle
// content.
func FuzzTarCheck(f *testing.F) {
	fx := newOCIFixture(testRef, "a", 2)
	f.Add(buildTar(f, fx.members()))
	fx2 := newOCIFixture(testRef, "b", 1)
	fx2.platform = false
	fx2.reindex()
	f.Add(buildTar(f, fx2.members()))

	f.Fuzz(func(t *testing.T, data []byte) {
		_ = CheckTar(bytes.NewReader(data), testRef)
		_ = CheckTarPinned(bytes.NewReader(data), testRef, fx.manifestDigest())
	})
}
