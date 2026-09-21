package imageimport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Bounds on images.lock itself (ADR-16-A2 O-4).
const (
	// maxLockSize bounds the whole lock file.
	maxLockSize = 64 * 1024
	// minLockFileSize excludes a literally empty file with a dedicated "size"
	// reason, rather than falling through to a generic parse failure.
	minLockFileSize = 1
	// minLockEntries / maxLockEntries bound the number of images entries.
	minLockEntries = 1
	maxLockEntries = 32
)

// lockImage is one images.lock entry.
type lockImage struct {
	Ref          string `json:"ref"`
	Digest       string `json:"digest"`
	Tarball      string `json:"tarball"`
	Verified     bool   `json:"verified"`
	VerifyReason string `json:"verifyReason"`
}

// lockVerifiedBy is the cosign identity that verified the bundle's images.
type lockVerifiedBy struct {
	Identity string `json:"identity"`
	Issuer   string `json:"issuer"`
}

// lock is images.lock's full document shape, exactly as build/bundle-images.sh
// writes it (see its final printf block). Field order here does not matter for
// decoding; renderLock fixes the canonical byte order.
type lock struct {
	KubernetesVersion string         `json:"kubernetesVersion"`
	ImageRepository   string         `json:"imageRepository"`
	VerifiedBy        lockVerifiedBy `json:"verifiedBy"`
	Images            []lockImage    `json:"images"`
}

var (
	// reRef is IR-1's reference pattern: a host[:port]/path:tag under
	// imageRepository (checked separately below).
	reRef = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?/[a-z0-9]+([._/-][a-z0-9]+)*:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	// reDigest matches a content digest.
	reDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// reKubernetesVersion matches the top-level build version.
	reKubernetesVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
	// reImageRepositoryCharset bounds imageRepository to a conservative,
	// lowercase host[:port][/path...] charset.
	reImageRepositoryCharset = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)
	// rePrintableASCII bounds verifiedBy.identity/issuer.
	rePrintableASCII = regexp.MustCompile(`^[\x20-\x7E]*$`)
)

// ParseLock strictly parses and validates an images.lock document (ADR-16-A2
// decision 4 / O-5): exactly one JSON value with no unknown fields, followed
// by clean EOF; re-rendered by the ONE canonical Go renderer with byte-for-
// byte equality to data (this alone rejects CRLF, a BOM, trailing data,
// duplicate or case-variant keys, `\u` escapes, reordered keys and extra
// fields); then per-field value rules (ref/digest/tarball shape, the
// (verified, verifyReason) pairing, kubernetesVersion, imageRepository,
// verifiedBy, and ref/tarball uniqueness) and the entry-count bound. Every
// failure is reported as a single, non-specific error: the caller reports it
// under the closed "lock-invalid" reason (ADR-16-A2 never distinguishes
// finer-grained lock reasons than that).
func ParseLock(data []byte) (lock, error) {
	if len(data) < minLockFileSize || len(data) > maxLockSize {
		return lock{}, fmt.Errorf("lock is %d byte(s), want %d..%d", len(data), minLockFileSize, maxLockSize)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var l lock
	if err := dec.Decode(&l); err != nil {
		return lock{}, fmt.Errorf("decode: %w", err)
	}
	if dec.More() {
		return lock{}, errors.New("trailing data after the JSON value")
	}

	if err := validateLockValues(l); err != nil {
		return lock{}, err
	}

	rendered := renderLock(l)
	if !bytes.Equal(rendered, data) {
		return lock{}, errors.New("lock is not the canonical byte-for-byte rendering " +
			"(whitespace, key order/casing, escapes, or duplicate/extra content)")
	}

	return l, nil
}

// validateLockValues checks every semantic rule ParseLock's byte-equality
// check cannot: bounds, regex shapes, cross-field consistency, and
// uniqueness.
func validateLockValues(l lock) error {
	if n := len(l.Images); n < minLockEntries || n > maxLockEntries {
		return fmt.Errorf("images has %d entries, want %d..%d", n, minLockEntries, maxLockEntries)
	}
	if !reKubernetesVersion.MatchString(l.KubernetesVersion) {
		return fmt.Errorf("kubernetesVersion %q does not match %s", l.KubernetesVersion, reKubernetesVersion.String())
	}
	if err := validateImageRepository(l.ImageRepository); err != nil {
		return err
	}
	if !isPrintableASCIINoQuoteOrBackslash(l.VerifiedBy.Identity) {
		return errors.New("verifiedBy.identity is not printable ASCII without '\"' or '\\'")
	}
	if !isPrintableASCIINoQuoteOrBackslash(l.VerifiedBy.Issuer) {
		return errors.New("verifiedBy.issuer is not printable ASCII without '\"' or '\\'")
	}

	refs := make(map[string]bool, len(l.Images))
	tarballs := make(map[string]bool, len(l.Images))
	for i, img := range l.Images {
		if !reRef.MatchString(img.Ref) {
			return fmt.Errorf("images[%d].ref %q does not match the allowed reference pattern", i, img.Ref)
		}
		if !strings.HasPrefix(img.Ref, l.ImageRepository+"/") {
			return fmt.Errorf("images[%d].ref %q is not under imageRepository %q", i, img.Ref, l.ImageRepository)
		}
		if !reDigest.MatchString(img.Digest) {
			return fmt.Errorf("images[%d].digest %q does not match sha256:<64 hex>", i, img.Digest)
		}
		if want := tarballForRef(img.Ref); img.Tarball != want {
			return fmt.Errorf("images[%d].tarball %q, want %q (derived from ref)", i, img.Tarball, want)
		}
		switch {
		case img.Verified && img.VerifyReason == "":
		case !img.Verified && img.VerifyReason == "no-upstream-signature":
		default:
			return fmt.Errorf("images[%d] has an invalid (verified, verifyReason) pair: (%v, %q)", i, img.Verified, img.VerifyReason)
		}
		if refs[img.Ref] {
			return fmt.Errorf("images[%d].ref %q is a duplicate", i, img.Ref)
		}
		refs[img.Ref] = true
		if tarballs[img.Tarball] {
			return fmt.Errorf("images[%d].tarball %q is a duplicate", i, img.Tarball)
		}
		tarballs[img.Tarball] = true
	}
	return nil
}

// validateImageRepository applies IR-1's host rule (the first path component
// must contain '.' or ':' or be exactly "localhost") plus a conservative
// charset bound.
func validateImageRepository(repo string) error {
	if repo == "" {
		return errors.New("imageRepository must not be empty")
	}
	if !reImageRepositoryCharset.MatchString(repo) {
		return fmt.Errorf("imageRepository %q contains characters outside the allowed set", repo)
	}
	first, _, _ := strings.Cut(repo, "/")
	if !strings.ContainsAny(first, ".:") && first != "localhost" {
		return fmt.Errorf("imageRepository %q must start with a registry host (containing '.' or ':', or 'localhost')", repo)
	}
	return nil
}

func isPrintableASCIINoQuoteOrBackslash(s string) bool {
	return rePrintableASCII.MatchString(s) && !strings.ContainsAny(s, `"\`)
}

// tarballForRef derives a lock entry's expected tarball name from its ref:
// every '/' and ':' replaced by '_', plus ".tar" (mirrors bundle-images.sh's
// `echo "${ref}" | tr '/:' '__'`).
func tarballForRef(ref string) string {
	repl := strings.NewReplacer("/", "_", ":", "_")
	return repl.Replace(ref) + ".tar"
}

// renderLock renders l in the ONE canonical byte layout: exactly what
// build/bundle-images.sh's final printf block writes (LF only, this exact
// indentation and key order, no trailing whitespace). ParseLock requires
// byte-for-byte equality against this output.
func renderLock(l lock) []byte {
	var b bytes.Buffer
	b.WriteString("{\n")
	b.WriteString("  \"kubernetesVersion\": " + jsonString(l.KubernetesVersion) + ",\n")
	b.WriteString("  \"imageRepository\": " + jsonString(l.ImageRepository) + ",\n")
	b.WriteString("  \"verifiedBy\": {\"identity\": " + jsonString(l.VerifiedBy.Identity) +
		", \"issuer\": " + jsonString(l.VerifiedBy.Issuer) + "},\n")
	b.WriteString("  \"images\": [\n")
	for i, img := range l.Images {
		b.WriteString("    {\"ref\": " + jsonString(img.Ref) +
			", \"digest\": " + jsonString(img.Digest) +
			", \"tarball\": " + jsonString(img.Tarball) +
			", \"verified\": " + strconv.FormatBool(img.Verified) +
			", \"verifyReason\": " + jsonString(img.VerifyReason) + "}")
		if i < len(l.Images)-1 {
			b.WriteString(",\n")
		} else {
			b.WriteString("\n")
		}
	}
	b.WriteString("  ]\n")
	b.WriteString("}\n")
	return b.Bytes()
}

// jsonString renders s as a minimal, correct JSON string literal. Every field
// renderLock uses this for is already value-validated to a safe charset
// (never requiring escapes) before rendering is reached; this still escapes
// defensively so a mismatch (rather than a malformed byte stream) is what a
// hostile value would produce.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
