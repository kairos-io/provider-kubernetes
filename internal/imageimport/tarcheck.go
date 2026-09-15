package imageimport

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
)

// Bounds on one tarball's internal structure (ADR-16-A2 O-4/O-6).
const (
	// manifestMaxSize bounds manifest.json.
	manifestMaxSize = 64 * 1024
	// configMaxSize bounds the single OCI config member.
	configMaxSize = 1 << 20
	// minTarMembers / maxTarMembers bound the member count: at least one
	// config, one layer and manifest.json; generously above any real
	// control-plane image.
	minTarMembers = 3
	maxTarMembers = 256
)

const manifestName = "manifest.json"

// Sentinel reasons a CheckTar failure is wrapped in (errors.Is), naming
// exactly which O-6 rule was violated -- the caller maps these 1:1 to the
// closed reason set's "tar-structure" / "manifest" / "config-digest" values.
var (
	// ErrTarStructure covers every fixed-shape violation: a non-regular
	// member, PAX records/xattrs, a disallowed or duplicate member name, a
	// missing/misplaced/extra manifest.json or config member, or a
	// member-count/size bound.
	ErrTarStructure = errors.New("tar-structure")
	// ErrManifest covers manifest.json's exact-bytes shape and its Layers
	// list not matching the layer members.
	ErrManifest = errors.New("manifest")
	// ErrConfigDigest covers the config member's sha256 not matching the hex
	// digest encoded in its own name.
	ErrConfigDigest = errors.New("config-digest")
)

var (
	reConfigMember = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	reLayerMember  = regexp.MustCompile(`^[0-9a-f]{64}\.tar\.gz$`)
	reLayersList   = regexp.MustCompile(`^"[0-9a-f]{64}\.tar\.gz"(,"[0-9a-f]{64}\.tar\.gz")*$`)
)

// CheckTar performs ADR-16-A2 decision 6's per-boot structural check on r (an
// already owner/mode/device/size-checked tarball, positioned at its start).
// Every member must be a plain regular file: no PAX records or extended
// attributes (so no symlink, hardlink, directory, device, FIFO, GNU sparse or
// PAX-preceded member survives), an exact, unique name (manifest.json, one
// "sha256:<64hex>" config, or "<64hex>.tar.gz" layers), and the member/byte-
// size bounds above. manifest.json must appear exactly once, LAST, followed
// by a clean EOF, with bytes exactly
//
//	[{"Config":"<config>","RepoTags":["<expectedRef>"],"Layers":[...]}]
//
// naming the single config member and expectedRef, with set(Layers) equal to
// the set of layer members (repeats within Layers are allowed). The config
// member's own sha256 must equal the hex digest encoded in its name. Layer
// member CONTENT is never read: tar.Reader skips it (efficiently, via Seek,
// when r is an io.Seeker such as *os.File) since nothing here calls Read on a
// layer member.
func CheckTar(r io.Reader, expectedRef string) error {
	tr := tar.NewReader(r)

	var (
		members      int
		seenNames    = make(map[string]bool)
		configMember string
		layerMembers = make(map[string]bool)
		manifestSeen bool
		manifestData []byte
	)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: read member %d: %v", ErrTarStructure, members+1, err)
		}
		if manifestSeen {
			return fmt.Errorf("%w: member %q follows manifest.json, which must be last", ErrTarStructure, hdr.Name)
		}
		members++
		if members > maxTarMembers {
			return fmt.Errorf("%w: more than %d members", ErrTarStructure, maxTarMembers)
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("%w: member %q is not a regular file (typeflag %q)", ErrTarStructure, hdr.Name, string(hdr.Typeflag))
		}
		if len(hdr.PAXRecords) > 0 {
			return fmt.Errorf("%w: member %q carries PAX records", ErrTarStructure, hdr.Name)
		}
		if len(hdr.Xattrs) > 0 { //nolint:staticcheck // deprecated field checked defensively alongside PAXRecords
			return fmt.Errorf("%w: member %q carries extended attributes", ErrTarStructure, hdr.Name)
		}
		if seenNames[hdr.Name] {
			return fmt.Errorf("%w: duplicate member %q", ErrTarStructure, hdr.Name)
		}
		seenNames[hdr.Name] = true

		switch {
		case hdr.Name == manifestName:
			if hdr.Size > manifestMaxSize {
				return fmt.Errorf("%w: manifest.json is %d bytes, exceeds %d", ErrTarStructure, hdr.Size, manifestMaxSize)
			}
			data, err := readExactly(tr, hdr.Size)
			if err != nil {
				return fmt.Errorf("%w: read manifest.json: %v", ErrTarStructure, err)
			}
			manifestData = data
			manifestSeen = true
		case reConfigMember.MatchString(hdr.Name):
			if configMember != "" {
				return fmt.Errorf("%w: more than one config member (%q and %q)", ErrTarStructure, configMember, hdr.Name)
			}
			if hdr.Size > configMaxSize {
				return fmt.Errorf("%w: config member %q is %d bytes, exceeds %d", ErrTarStructure, hdr.Name, hdr.Size, configMaxSize)
			}
			data, err := readExactly(tr, hdr.Size)
			if err != nil {
				return fmt.Errorf("%w: read config member %q: %v", ErrTarStructure, hdr.Name, err)
			}
			sum := sha256.Sum256(data)
			gotHex := hex.EncodeToString(sum[:])
			wantHex := hdr.Name[len("sha256:"):]
			if gotHex != wantHex {
				return fmt.Errorf("%w: config member %q has sha256 %s", ErrConfigDigest, hdr.Name, gotHex)
			}
			configMember = hdr.Name
		case reLayerMember.MatchString(hdr.Name):
			layerMembers[hdr.Name] = true
		default:
			return fmt.Errorf("%w: unexpected member name %q", ErrTarStructure, hdr.Name)
		}
	}

	if !manifestSeen {
		return fmt.Errorf("%w: manifest.json is missing", ErrTarStructure)
	}
	if members < minTarMembers {
		return fmt.Errorf("%w: %d member(s), want at least %d", ErrTarStructure, members, minTarMembers)
	}
	if configMember == "" {
		return fmt.Errorf("%w: no config member (sha256:<64hex>)", ErrTarStructure)
	}
	if len(layerMembers) == 0 {
		return fmt.Errorf("%w: no layer member (<64hex>.tar.gz)", ErrTarStructure)
	}

	return checkManifestBytes(manifestData, configMember, expectedRef, layerMembers)
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

// checkManifestBytes requires data to be exactly
// `[{"Config":"<configMember>","RepoTags":["<expectedRef>"],"Layers":[...]}]`
// with set(Layers) == layerMembers.
func checkManifestBytes(data []byte, configMember, expectedRef string, layerMembers map[string]bool) error {
	prefix := `[{"Config":"` + configMember + `","RepoTags":["` + expectedRef + `"],"Layers":[`
	suffix := `]}]`
	if len(data) < len(prefix)+len(suffix) ||
		string(data[:len(prefix)]) != prefix ||
		string(data[len(data)-len(suffix):]) != suffix {
		return fmt.Errorf("%w: manifest.json is not exactly %s...%s", ErrManifest, prefix, suffix)
	}
	mid := data[len(prefix) : len(data)-len(suffix)]

	layerNames, err := parseLayersList(mid)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrManifest, err)
	}
	got := make(map[string]bool, len(layerNames))
	for _, n := range layerNames {
		got[n] = true
	}
	if len(got) != len(layerMembers) {
		return fmt.Errorf("%w: manifest.json Layers set does not match the layer members", ErrManifest)
	}
	for n := range layerMembers {
		if !got[n] {
			return fmt.Errorf("%w: manifest.json Layers set does not match the layer members", ErrManifest)
		}
	}
	return nil
}

// parseLayersList requires mid to be a plain comma-separated list of
// "<hex>.tar.gz" string literals (no whitespace, no other JSON syntax).
func parseLayersList(mid []byte) ([]string, error) {
	if len(mid) == 0 || !reLayersList.Match(mid) {
		return nil, errors.New(`layers is not a plain comma-separated list of "<hex>.tar.gz" strings`)
	}
	parts := splitTopLevelComma(string(mid))
	names := make([]string, len(parts))
	for i, p := range parts {
		names[i] = p[1 : len(p)-1] // strip the surrounding quotes reLayersList already proved are there
	}
	return names, nil
}

// splitTopLevelComma splits s on ',' -- safe here because reLayersList has
// already proven s contains nothing but quoted "<hex>.tar.gz" tokens and
// commas (no comma can occur inside a token).
func splitTopLevelComma(s string) []string {
	n := 1
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			n++
		}
	}
	parts := make([]string, 0, n)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}
