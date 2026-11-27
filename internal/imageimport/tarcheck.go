package imageimport

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"runtime"
	"strconv"
	"strings"
)

// Bounds on one tarball's internal structure (ADR-16-A2 O-4/O-6, revised by
// ADR-16-A3 to the canonical OCI image layout).
const (
	// indexMaxSize bounds index.json.
	indexMaxSize = 8 * 1024
	// manifestMaxSize bounds the manifest blob.
	manifestMaxSize = 64 * 1024
	// configMaxSize bounds the single OCI config blob.
	configMaxSize = 1 << 20
	// minTarMembers / maxTarMembers bound the member count: at least
	// oci-layout, index.json and one blob; generously above any real
	// control-plane image.
	minTarMembers = 3
	maxTarMembers = 256
)

// Fixed member names.
const (
	ociLayoutName = "oci-layout"
	indexName     = "index.json"
	blobPrefix    = "blobs/sha256/"
	// legacyManifestName is docker-save's manifest member. It must never
	// appear: containerd takes the OCI branch (and returns early) when
	// oci-layout is present, so a both-formats tar would validate as one
	// format and import as the other.
	legacyManifestName = "manifest.json"
)

// Media types the index may name for its single manifest. An index/manifest-
// list media type is refused: the bundle carries one platform's manifest.
const (
	mediaTypeOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	mediaTypeOCIIndex       = "application/vnd.oci.image.index.v1+json"

	// annotationRefName is the ONLY annotation index.json may carry. The
	// io.containerd.image.name annotation takes precedence over it in
	// containerd's importer, so it must never be present.
	annotationRefName = "org.opencontainers.image.ref.name"
	// annotationContainerdName is checked explicitly purely for a precise
	// error: with it present the image record's NAME would be chosen by the
	// tarball while a ref.name-only validator reported success.
	annotationContainerdName = "io.containerd.image.name"
)

// ociLayoutBytes is the exact oci-layout member, as the build renders it
// (crane/go-containerregistry's pretty-printed form, 37 bytes, no trailing
// newline). Byte-exact, never parsed.
const ociLayoutBytes = "{\n    \"imageLayoutVersion\": \"1.0.0\"\n}"

// bundleArch is the architecture the bundle's single manifest must be built
// for (the lock's manifestDigest is arch-specific). A variable so tests can
// pin it; production is always the running binary's architecture.
var bundleArch = runtime.GOARCH

// Sentinel reasons a CheckTar failure is wrapped in (errors.Is), naming
// exactly which O-6 rule was violated -- the caller maps these 1:1 to the
// closed reason set's "tar-structure" / "manifest" / "config-digest" values.
var (
	// ErrTarStructure covers every fixed-shape violation: a non-regular
	// member, PAX records/xattrs, a disallowed, malformed or duplicate member
	// name, a member-count/size bound, or members out of the required order.
	ErrTarStructure = errors.New("tar-structure")
	// ErrManifest covers index.json's exact-bytes shape, its single
	// annotation, the pin against the lock's manifestDigest, and the manifest
	// blob's shape and closure (the config and layer members it names are
	// exactly the members present, in order).
	ErrManifest = errors.New("manifest")
	// ErrConfigDigest covers the config blob's sha256 not matching the hex
	// digest encoded in its own name.
	ErrConfigDigest = errors.New("config-digest")
)

// CheckTar performs ADR-16-A3's per-boot structural check on r (an already
// owner/mode/device/size-checked tarball, positioned at its start) without a
// provenance pin: see CheckTarPinned, which production (doImport) uses.
func CheckTar(r io.Reader, expectedRef string) error {
	return CheckTarPinned(r, expectedRef, "")
}

// CheckTarPinned checks r is exactly a canonical OCI image layout for
// expectedRef and, when wantManifestDigest is non-empty, that index.json's
// single manifest descriptor names that digest (the lock's manifestDigest --
// the only runtime link between the bundle and what was verified at build).
//
// The governing rule: this validator must never be looser than containerd's
// importer. Members, in EXACTLY this order and with no directory members:
//
//	oci-layout                       exact bytes ociLayoutBytes
//	index.json                       compact, byte-exact against a template
//	blobs/sha256/<manifest-hex>      the manifest index.json names
//	blobs/sha256/<config-hex>        the config that manifest names
//	blobs/sha256/<layer-hex> ...     that manifest's layers, in manifest order
//
// (a layer digest repeated within the manifest is stored once, at its first
// position). Every member is a plain regular file with no PAX records or
// xattrs, an anchored byte-exact allowlisted name, unique both by exact name
// and by path.Clean(name) (containerd keys on the cleaned name and a later
// duplicate silently overwrites). The checker is index-driven: the manifest
// blob is located via index.json and the config/layers via the manifest,
// because a config blob is indistinguishable from a layer blob by name.
// index.json must carry exactly one manifest descriptor with exactly the one
// annotation org.opencontainers.image.ref.name == expectedRef and a platform
// that is absent or exactly linux/<bundleArch>.
//
// The manifest and config blobs are hashed; their sha256 must equal the name.
// Layer blobs are NEVER read (tar.Reader skips them, efficiently via Seek
// when r is an io.Seeker such as *os.File): containerd's content store is
// keyed by the COMPUTED digest, so a layer whose bytes do not match its path
// is unreachable and the import fails closed. Their member SIZE is compared
// to the manifest's declared size, which costs only the header.
func CheckTarPinned(r io.Reader, expectedRef, wantManifestDigest string) error {
	if wantManifestDigest != "" && !reDigest.MatchString(wantManifestDigest) {
		return fmt.Errorf("%w: pinned manifest digest %q is not sha256:<64 hex>", ErrManifest, wantManifestDigest)
	}
	tr := tar.NewReader(r)

	var (
		members   int
		seenExact = make(map[string]bool)
		seenClean = make(map[string]bool)

		manifestHex string
		manifestMT  string
		manifestSz  int64
		// order is the remaining blob members still owed, in order, once the
		// manifest has been read: config first, then layers.
		order    []blobWant
		orderPos int
	)

	// fixed names, in order, preceding the blobs.
	fixed := []string{ociLayoutName, indexName}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: read member %d: %v", ErrTarStructure, members+1, err)
		}
		members++
		if members > maxTarMembers {
			return fmt.Errorf("%w: more than %d members", ErrTarStructure, maxTarMembers)
		}
		if err := checkMemberHeader(hdr, seenExact, seenClean); err != nil {
			return err
		}

		switch {
		case members <= len(fixed):
			want := fixed[members-1]
			if hdr.Name != want {
				return fmt.Errorf("%w: member %d is %q, want %q", ErrTarStructure, members, hdr.Name, want)
			}
			if want == ociLayoutName {
				if err := checkOCILayout(tr, hdr); err != nil {
					return err
				}
				continue
			}
			dg, mt, sz, err := checkIndex(tr, hdr, expectedRef, wantManifestDigest)
			if err != nil {
				return err
			}
			manifestHex, manifestMT, manifestSz = strings.TrimPrefix(dg, "sha256:"), mt, sz
		case members == len(fixed)+1:
			wantName := blobPrefix + manifestHex
			if hdr.Name != wantName {
				return fmt.Errorf("%w: member %d is %q, want the manifest blob %q named by index.json", ErrManifest, members, hdr.Name, wantName)
			}
			if hdr.Size != manifestSz {
				return fmt.Errorf("%w: manifest blob is %d bytes, index.json declares %d", ErrManifest, hdr.Size, manifestSz)
			}
			if hdr.Size > manifestMaxSize {
				return fmt.Errorf("%w: manifest blob is %d bytes, exceeds %d", ErrManifest, hdr.Size, manifestMaxSize)
			}
			data, err := readExactly(tr, hdr.Size)
			if err != nil {
				return fmt.Errorf("%w: read manifest blob: %v", ErrTarStructure, err)
			}
			if got := sha256Hex(data); got != manifestHex {
				return fmt.Errorf("%w: manifest blob %q has sha256 %s", ErrManifest, hdr.Name, got)
			}
			order, err = parseManifestBlob(data, manifestMT, manifestHex)
			if err != nil {
				return err
			}
			if len(order)+len(fixed)+1 > maxTarMembers {
				return fmt.Errorf("%w: manifest implies %d members, exceeds %d", ErrManifest, len(order)+len(fixed)+1, maxTarMembers)
			}
		default:
			if orderPos >= len(order) {
				return fmt.Errorf("%w: unexpected member %q after the last blob the manifest names", ErrManifest, hdr.Name)
			}
			w := order[orderPos]
			orderPos++
			if hdr.Name != blobPrefix+w.hex {
				return fmt.Errorf("%w: member %d is %q, want %q (manifest order)", ErrManifest, members, hdr.Name, blobPrefix+w.hex)
			}
			if hdr.Size != w.size {
				return fmt.Errorf("%w: blob %q is %d bytes, manifest declares %d", ErrManifest, hdr.Name, hdr.Size, w.size)
			}
			if !w.isConfig {
				continue // layers are deliberately never read
			}
			if hdr.Size > configMaxSize {
				return fmt.Errorf("%w: config blob %q is %d bytes, exceeds %d", ErrTarStructure, hdr.Name, hdr.Size, configMaxSize)
			}
			data, err := readExactly(tr, hdr.Size)
			if err != nil {
				return fmt.Errorf("%w: read config blob %q: %v", ErrTarStructure, hdr.Name, err)
			}
			if got := sha256Hex(data); got != w.hex {
				return fmt.Errorf("%w: config blob %q has sha256 %s", ErrConfigDigest, hdr.Name, got)
			}
		}
	}

	if members < minTarMembers {
		return fmt.Errorf("%w: %d member(s), want at least %d", ErrTarStructure, members, minTarMembers)
	}
	if members < len(fixed)+1 {
		return fmt.Errorf("%w: archive ends after %d member(s); no manifest blob", ErrManifest, members)
	}
	if orderPos != len(order) {
		return fmt.Errorf("%w: archive ends before %d blob(s) the manifest names", ErrManifest, len(order)-orderPos)
	}
	return nil
}

// checkMemberHeader enforces the per-member rules independent of position:
// plain regular file, no PAX records/xattrs, an anchored allowlisted name,
// and uniqueness by exact name AND by path.Clean(name).
func checkMemberHeader(hdr *tar.Header, seenExact, seenClean map[string]bool) error {
	if hdr.Typeflag != tar.TypeReg {
		return fmt.Errorf("%w: member %q is not a regular file (typeflag %q)", ErrTarStructure, hdr.Name, string(hdr.Typeflag))
	}
	if len(hdr.PAXRecords) > 0 {
		return fmt.Errorf("%w: member %q carries PAX records", ErrTarStructure, hdr.Name)
	}
	if len(hdr.Xattrs) > 0 { //nolint:staticcheck // deprecated field checked defensively alongside PAXRecords
		return fmt.Errorf("%w: member %q carries extended attributes", ErrTarStructure, hdr.Name)
	}

	// Duplicates first, so a "./index.json" after "index.json" is reported as
	// the duplicate-after-clean it is rather than as a mere bad name.
	if seenExact[hdr.Name] {
		return fmt.Errorf("%w: duplicate member %q", ErrTarStructure, hdr.Name)
	}
	cleaned := path.Clean(hdr.Name)
	if seenClean[cleaned] {
		return fmt.Errorf("%w: member %q duplicates an earlier member after path.Clean (%q)", ErrTarStructure, hdr.Name, cleaned)
	}
	seenExact[hdr.Name] = true
	seenClean[cleaned] = true

	if hdr.Name == legacyManifestName || cleaned == legacyManifestName {
		return fmt.Errorf("%w: member %q is a docker-save manifest; an OCI layout must not carry one", ErrTarStructure, hdr.Name)
	}
	if err := checkMemberName(hdr.Name); err != nil {
		return err
	}
	return nil
}

// checkMemberName requires name to be exactly one of the allowlisted shapes:
// "oci-layout", "index.json" or "blobs/sha256/<64 lowercase hex>". The
// specific hazards are called out for diagnosis; the anchored allowlist is
// what actually enforces them.
func checkMemberName(name string) error {
	switch {
	case strings.ContainsRune(name, 0):
		return fmt.Errorf("%w: member name %q contains NUL", ErrTarStructure, name)
	case strings.Contains(name, `\`):
		return fmt.Errorf("%w: member name %q contains a backslash", ErrTarStructure, name)
	case strings.HasPrefix(name, "/"):
		return fmt.Errorf("%w: member name %q is absolute", ErrTarStructure, name)
	case strings.HasPrefix(name, "./"):
		return fmt.Errorf("%w: member name %q has a './' prefix", ErrTarStructure, name)
	case strings.Contains(name, "//"):
		return fmt.Errorf("%w: member name %q contains '//'", ErrTarStructure, name)
	case strings.HasSuffix(name, "/"):
		return fmt.Errorf("%w: member name %q has a trailing slash (directory member)", ErrTarStructure, name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." || seg == "." {
			return fmt.Errorf("%w: member name %q contains a %q path element", ErrTarStructure, name, seg)
		}
	}
	switch name {
	case ociLayoutName, indexName:
		return nil
	}
	if hexPart, ok := strings.CutPrefix(name, blobPrefix); ok && isLowerHex64(hexPart) {
		return nil
	}
	return fmt.Errorf("%w: unexpected member name %q", ErrTarStructure, name)
}

func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func checkOCILayout(tr io.Reader, hdr *tar.Header) error {
	if hdr.Size != int64(len(ociLayoutBytes)) {
		return fmt.Errorf("%w: oci-layout is %d bytes, want exactly %d", ErrTarStructure, hdr.Size, len(ociLayoutBytes))
	}
	data, err := readExactly(tr, hdr.Size)
	if err != nil {
		return fmt.Errorf("%w: read oci-layout: %v", ErrTarStructure, err)
	}
	if string(data) != ociLayoutBytes {
		return fmt.Errorf("%w: oci-layout is not the exact canonical bytes", ErrTarStructure)
	}
	return nil
}

// indexDoc / indexDescriptor decode index.json just far enough to extract the
// fields; the byte-exact template comparison in checkIndex is the real gate.
type indexDoc struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Manifests     []indexDescriptor `json:"manifests"`
}

type indexDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Size        int64             `json:"size"`
	Digest      string            `json:"digest"`
	Platform    *indexPlatform    `json:"platform"`
	Annotations map[string]string `json:"annotations"`
}

type indexPlatform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

// checkIndex validates index.json and returns its single manifest's digest,
// media type and size.
func checkIndex(tr io.Reader, hdr *tar.Header, expectedRef, wantManifestDigest string) (digest, mediaType string, size int64, err error) {
	if hdr.Size > indexMaxSize {
		return "", "", 0, fmt.Errorf("%w: index.json is %d bytes, exceeds %d", ErrTarStructure, hdr.Size, indexMaxSize)
	}
	data, err := readExactly(tr, hdr.Size)
	if err != nil {
		return "", "", 0, fmt.Errorf("%w: read index.json: %v", ErrTarStructure, err)
	}

	var doc indexDoc
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return "", "", 0, fmt.Errorf("%w: index.json: %v", ErrManifest, err)
	}
	if len(doc.Manifests) != 1 {
		return "", "", 0, fmt.Errorf("%w: index.json has %d manifests, want exactly 1", ErrManifest, len(doc.Manifests))
	}
	m := doc.Manifests[0]

	switch m.MediaType {
	case mediaTypeOCIManifest, mediaTypeDockerManifest:
	case mediaTypeOCIIndex, "application/vnd.docker.distribution.manifest.list.v2+json":
		return "", "", 0, fmt.Errorf("%w: index.json descriptor is an index (%q); the bundle must name one platform manifest", ErrManifest, m.MediaType)
	default:
		return "", "", 0, fmt.Errorf("%w: index.json descriptor mediaType %q is not allowed", ErrManifest, m.MediaType)
	}
	if !reDigest.MatchString(m.Digest) {
		return "", "", 0, fmt.Errorf("%w: index.json descriptor digest %q is not sha256:<64 hex>", ErrManifest, m.Digest)
	}
	if m.Size < 1 || m.Size > manifestMaxSize {
		return "", "", 0, fmt.Errorf("%w: index.json descriptor size %d out of range 1..%d", ErrManifest, m.Size, manifestMaxSize)
	}
	if _, ok := m.Annotations[annotationContainerdName]; ok {
		return "", "", 0, fmt.Errorf("%w: index.json carries %s, which would override the image name", ErrManifest, annotationContainerdName)
	}
	if len(m.Annotations) != 1 {
		return "", "", 0, fmt.Errorf("%w: index.json descriptor has %d annotations, want exactly 1 (%s)", ErrManifest, len(m.Annotations), annotationRefName)
	}
	if got, ok := m.Annotations[annotationRefName]; !ok || got != expectedRef {
		return "", "", 0, fmt.Errorf("%w: index.json %s is %q, want %q", ErrManifest, annotationRefName, got, expectedRef)
	}
	if m.Platform != nil && (m.Platform.OS != "linux" || m.Platform.Architecture != bundleArch) {
		return "", "", 0, fmt.Errorf("%w: index.json platform %s/%s, want linux/%s (a mismatch makes containerd skip rooting the children)",
			ErrManifest, m.Platform.OS, m.Platform.Architecture, bundleArch)
	}
	if wantManifestDigest != "" && m.Digest != wantManifestDigest {
		return "", "", 0, fmt.Errorf("%w: index.json names manifest %s, the lock pins %s", ErrManifest, m.Digest, wantManifestDigest)
	}

	if want := renderIndex(m, expectedRef); !bytes.Equal(data, want) {
		return "", "", 0, fmt.Errorf("%w: index.json is not the canonical compact rendering", ErrManifest)
	}
	return m.Digest, m.MediaType, m.Size, nil
}

// renderIndex renders index.json in the ONE canonical compact layout (the
// platform member is included iff m has one). Every interpolated value was
// validated before this is reached; jsonString escapes defensively.
func renderIndex(m indexDescriptor, ref string) []byte {
	var b strings.Builder
	b.WriteString(`{"schemaVersion":2,"mediaType":` + jsonString(mediaTypeOCIIndex) +
		`,"manifests":[{"mediaType":` + jsonString(m.MediaType) +
		`,"size":` + strconv.FormatInt(m.Size, 10) +
		`,"digest":` + jsonString(m.Digest))
	if m.Platform != nil {
		b.WriteString(`,"platform":{"architecture":` + jsonString(m.Platform.Architecture) +
			`,"os":` + jsonString(m.Platform.OS) + `}`)
	}
	b.WriteString(`,"annotations":{` + jsonString(annotationRefName) + `:` + jsonString(ref) + `}}]}`)
	return []byte(b.String())
}

// blobWant is one blob member still owed after the manifest, in order.
type blobWant struct {
	hex      string
	size     int64
	isConfig bool
}

type manifestDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type manifestDoc struct {
	SchemaVersion int                  `json:"schemaVersion"`
	MediaType     string               `json:"mediaType"`
	Config        manifestDescriptor   `json:"config"`
	Layers        []manifestDescriptor `json:"layers"`
}

// parseManifestBlob validates the manifest document and returns the blob
// members it owes after itself: the config first, then each distinct layer in
// first-seen manifest order. It decodes with the same encoding/json semantics
// containerd uses, and the blob's bytes are already bound to the (lock-
// pinned) manifest digest, so parser differentials cannot select different
// content than the one verified.
func parseManifestBlob(data []byte, indexMediaType, manifestHex string) ([]blobWant, error) {
	var doc manifestDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: manifest blob: %v", ErrManifest, err)
	}
	if doc.SchemaVersion != 2 {
		return nil, fmt.Errorf("%w: manifest blob schemaVersion %d, want 2", ErrManifest, doc.SchemaVersion)
	}
	// A docker schema2 manifest must say so; an OCI manifest may omit it.
	omittedOCI := doc.MediaType == "" && indexMediaType == mediaTypeOCIManifest
	if doc.MediaType != indexMediaType && !omittedOCI {
		return nil, fmt.Errorf("%w: manifest blob mediaType %q does not match index.json's %q", ErrManifest, doc.MediaType, indexMediaType)
	}
	if !reDigest.MatchString(doc.Config.Digest) {
		return nil, fmt.Errorf("%w: manifest config digest %q is not sha256:<64 hex>", ErrManifest, doc.Config.Digest)
	}
	if doc.Config.Size < 1 || doc.Config.Size > configMaxSize {
		return nil, fmt.Errorf("%w: manifest config size %d out of range 1..%d", ErrManifest, doc.Config.Size, configMaxSize)
	}
	if len(doc.Layers) < 1 {
		return nil, fmt.Errorf("%w: manifest names no layers", ErrManifest)
	}
	if len(doc.Layers) > maxTarMembers {
		return nil, fmt.Errorf("%w: manifest names %d layers, exceeds %d", ErrManifest, len(doc.Layers), maxTarMembers)
	}

	configHex := strings.TrimPrefix(doc.Config.Digest, "sha256:")
	if configHex == manifestHex {
		return nil, fmt.Errorf("%w: manifest config is the manifest blob itself", ErrManifest)
	}
	order := []blobWant{{hex: configHex, size: doc.Config.Size, isConfig: true}}
	seen := map[string]int64{manifestHex: 0, configHex: doc.Config.Size}
	for i, l := range doc.Layers {
		if !reDigest.MatchString(l.Digest) {
			return nil, fmt.Errorf("%w: layers[%d] digest %q is not sha256:<64 hex>", ErrManifest, i, l.Digest)
		}
		if l.Size < 1 {
			return nil, fmt.Errorf("%w: layers[%d] size %d is not positive", ErrManifest, i, l.Size)
		}
		h := strings.TrimPrefix(l.Digest, "sha256:")
		if h == manifestHex || h == configHex {
			return nil, fmt.Errorf("%w: layers[%d] reuses the manifest or config digest", ErrManifest, i)
		}
		if sz, dup := seen[h]; dup {
			if sz != l.Size {
				return nil, fmt.Errorf("%w: layers[%d] repeats a digest with a different size", ErrManifest, i)
			}
			continue // a repeated layer is stored once, at its first position
		}
		seen[h] = l.Size
		order = append(order, blobWant{hex: h, size: l.Size})
	}
	return order, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// readExactly reads exactly want bytes then requires no further bytes to
// follow within the same member (defense in depth: tar.Reader already bounds
// Read to the member's declared size).
func readExactly(r io.Reader, want int64) ([]byte, error) {
	buf := make([]byte, want)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	var extra [1]byte
	if n, err := r.Read(extra[:]); n > 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return nil, fmt.Errorf("member has more than its declared %d byte(s)", want)
	}
	return buf, nil
}
