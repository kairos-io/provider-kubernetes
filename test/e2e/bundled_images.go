//go:build e2e

package e2e

// bundled_images.go holds the pure parsers and checks behind IR-9, the Tier-1
// gate for ADR-16-A1 / F-IMPORTREF, and behind TestBundledImagesLockDrivenImport,
// the gate for ADR-16-A2 / F-OPTBIND.
//
// IR-9: after the boot-time import, every image the bundled kubeadm needs must be
// present in containerd under the EXACT reference kubeadm and containerd look it
// up by. kubeadm decides whether to pull by asking the CRI for the image status
// of the exact ref string (kubeadm v1.37.0
// cmd/kubeadm/app/util/runtime/runtime.go:247-259), and containerd resolves its
// sandbox_image the same way. Each bundled tarball is a canonical OCI image
// layout (ADR-16-A3): oci-layout, index.json, then the manifest, config and layer
// blobs under blobs/sha256/. `ctr images import` names the image after the
// org.opencontainers.image.ref.name annotation on index.json's one manifest
// descriptor, unless an io.containerd.image.name annotation is present, which
// takes precedence (containerd v2.3.5 core/images/archive/importer.go
// imageName()); with no usable name it falls back to "import-<date>"
// (cmd/ctr/commands/images/import.go:131-132). A tarball pulled by digest with
// crane carries the placeholder tag "<repo>:i-was-a-digest"
// (go-containerregistry pkg/crane/pull.go), and bundle-images.sh's pinned crane
// is CRANE_VERSION=v0.22.1 (Dockerfile:338). Imported as is, the images exist
// but under names nothing asks for, so an air-gapped first boot tries to pull and
// fails. Counting imported tarballs cannot see that; these checks can.
//
// Why OCI layout and not docker-save: a docker-save tarball carries no manifest
// document, so containerd re-serializes one at import, and its digest exists in
// no registry. A later CRI pull of the same ref re-points the image record at the
// registry's index, orphans the synthesized manifest and lets GC delete it, and
// CreateContainer then fails with "content digest ...: not found". Under OCI
// layout the record targets the registry's own manifest, so a later pull finds it
// already local and nothing is orphaned. IR-9 cannot see that class of defect
// (it checks right after import, before any pull exists); IR-10
// (bundled_images_pull_test.go) is the gate that can.
//
// ADR-16-A2: the bundle lives in the image-only hostexec.BundleDir, and
// `import-images` imports only the images.lock entries after checking each one.
// It reports one line per entry and ends with one summary line, which the
// helpers below parse.

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/kairos-io/provider-kubernetes/internal/hostexec"
)

const (
	// imagesLockPath is the lock bundle-images.sh writes next to the tarballs.
	imagesLockPath = hostexec.BundleDir + "/images.lock"
	// imageImportUnit is the boot-time oneshot that runs `import-images`.
	imageImportUnit = "provider-kubernetes-image-import.service"
	// containerdConfigPath holds the sandbox_image containerd resolves by ref.
	containerdConfigPath = "/etc/containerd/config.toml"
)

var (
	sha256HexRE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	lockDigestRE  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	lockTarballRE = regexp.MustCompile(`^[A-Za-z0-9._-]+\.tar$`)
)

// imagesLock is the part of images.lock the e2e checks rely on. Fields the checks
// do not use are tolerated, so an additive change to the lock does not break the
// gate; every field that is used is validated.
type imagesLock struct {
	KubernetesVersion string               `json:"kubernetesVersion"`
	ImageRepository   string               `json:"imageRepository"`
	VerifiedBy        imagesLockVerifiedBy `json:"verifiedBy"`
	Images            []imagesLockItem     `json:"images"`
}

type imagesLockVerifiedBy struct {
	Identity string `json:"identity"`
	Issuer   string `json:"issuer"`
}

type imagesLockItem struct {
	Ref string `json:"ref"`
	// Digest is the cosign-verified index digest.
	Digest string `json:"digest"`
	// ManifestDigest is the per-platform (architecture-specific) manifest digest
	// the tarball's index.json must name.
	ManifestDigest string `json:"manifestDigest"`
	Tarball        string `json:"tarball"`
	Verified       bool   `json:"verified"`
	VerifyReason   string `json:"verifyReason"`
}

// parseImagesLock parses images.lock and rejects entries the checks could not
// use safely: an empty or duplicated ref, a digest that is not sha256, or a
// tarball name that is not a plain file name (it is joined onto the bundle
// directory, so no separators or traversal).
func parseImagesLock(raw string) (imagesLock, error) {
	var lock imagesLock
	if err := json.Unmarshal([]byte(raw), &lock); err != nil {
		return imagesLock{}, fmt.Errorf("parse images.lock: %w", err)
	}
	if lock.KubernetesVersion == "" || lock.ImageRepository == "" {
		return imagesLock{}, fmt.Errorf("images.lock: kubernetesVersion %q or imageRepository %q is empty", lock.KubernetesVersion, lock.ImageRepository)
	}
	if len(lock.Images) == 0 {
		return imagesLock{}, fmt.Errorf("images.lock lists no images")
	}
	refs := map[string]bool{}
	tarballs := map[string]bool{}
	for i, img := range lock.Images {
		if img.Ref == "" || strings.ContainsAny(img.Ref, " \t\r\n") {
			return imagesLock{}, fmt.Errorf("images.lock entry %d: ref %q is empty or contains whitespace", i, img.Ref)
		}
		if refs[img.Ref] {
			return imagesLock{}, fmt.Errorf("images.lock lists ref %q more than once", img.Ref)
		}
		refs[img.Ref] = true
		if !lockDigestRE.MatchString(img.Digest) {
			return imagesLock{}, fmt.Errorf("images.lock entry %s: digest %q is not sha256:<64 hex>", img.Ref, img.Digest)
		}
		if !lockDigestRE.MatchString(img.ManifestDigest) {
			return imagesLock{}, fmt.Errorf("images.lock entry %s: manifestDigest %q is not sha256:<64 hex>", img.Ref, img.ManifestDigest)
		}
		if !lockTarballRE.MatchString(img.Tarball) || img.Tarball == ".tar" {
			return imagesLock{}, fmt.Errorf("images.lock entry %s: tarball %q is not a plain *.tar file name", img.Ref, img.Tarball)
		}
		if tarballs[img.Tarball] {
			return imagesLock{}, fmt.Errorf("images.lock lists tarball %q more than once", img.Tarball)
		}
		tarballs[img.Tarball] = true
	}
	return lock, nil
}

// refs returns the lock's image references, sorted.
func (l imagesLock) refs() []string {
	out := make([]string, 0, len(l.Images))
	for _, img := range l.Images {
		out = append(out, img.Ref)
	}
	sort.Strings(out)
	return out
}

// tarballs returns the lock's tarball names, sorted.
func (l imagesLock) tarballs() []string {
	out := make([]string, 0, len(l.Images))
	for _, img := range l.Images {
		out = append(out, img.Tarball)
	}
	sort.Strings(out)
	return out
}

// tarballNameForRef is the tarball name bundle-images.sh gives a ref: '/' and ':'
// become '_', plus ".tar".
func tarballNameForRef(ref string) string {
	return strings.NewReplacer("/", "_", ":", "_").Replace(ref) + ".tar"
}

// plainLockValue reports whether s can be written into images.lock as is:
// printable ASCII without '"' or '\', the rule bundle-images.sh and the importer
// both apply.
func plainLockValue(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c > 0x7e || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

// renderImagesLock writes l in the exact byte layout bundle-images.sh produces,
// which the importer requires byte for byte (ADR-16-A2 decision 4). The e2e test
// uses it to plant a lock the importer would accept, so only its location decides
// whether it is read. It refuses values it cannot write as plain JSON strings.
func renderImagesLock(l imagesLock) (string, error) {
	values := []string{l.KubernetesVersion, l.ImageRepository, l.VerifiedBy.Identity, l.VerifiedBy.Issuer}
	for _, img := range l.Images {
		values = append(values, img.Ref, img.Digest, img.ManifestDigest, img.Tarball, img.VerifyReason)
	}
	for _, v := range values {
		if !plainLockValue(v) {
			return "", fmt.Errorf("images.lock value %q is not printable ASCII without '\"' or '\\'", v)
		}
	}
	var b strings.Builder
	b.WriteString("{\n")
	fmt.Fprintf(&b, "  \"kubernetesVersion\": \"%s\",\n", l.KubernetesVersion)
	fmt.Fprintf(&b, "  \"imageRepository\": \"%s\",\n", l.ImageRepository)
	fmt.Fprintf(&b, "  \"verifiedBy\": {\"identity\": \"%s\", \"issuer\": \"%s\"},\n", l.VerifiedBy.Identity, l.VerifiedBy.Issuer)
	b.WriteString("  \"images\": [\n")
	for i, img := range l.Images {
		if i > 0 {
			b.WriteString(",\n")
		}
		fmt.Fprintf(&b, "    {\"ref\": \"%s\", \"digest\": \"%s\", \"manifestDigest\": \"%s\", \"tarball\": \"%s\", \"verified\": %t, \"verifyReason\": \"%s\"}",
			img.Ref, img.Digest, img.ManifestDigest, img.Tarball, img.Verified, img.VerifyReason)
	}
	b.WriteString("\n  ]\n}\n")
	return b.String(), nil
}

// Media types and names of the canonical OCI layout (ADR-16-A3).
const (
	ociIndexMediaType       = "application/vnd.oci.image.index.v1+json"
	ociManifestMediaType    = "application/vnd.oci.image.manifest.v1+json"
	dockerManifestMediaType = "application/vnd.docker.distribution.manifest.v2+json"
	// ociRefNameAnnotation is the only annotation index.json carries.
	ociRefNameAnnotation = "org.opencontainers.image.ref.name"
	ociBlobPrefix        = "blobs/sha256/"
	ociMaxMembers        = 256
)

// ociIndexInfo is what IR-9 reads from a bundled tarball's index.json.
type ociIndexInfo struct {
	// ManifestDigest is the digest of the one manifest, "sha256:<hex>".
	ManifestDigest string
	// MediaType is that manifest's media type.
	MediaType string
	// RefName is the image name the import will give the image.
	RefName string
}

// parseOCIIndex strictly parses a bundled tarball's index.json: exactly one
// manifest descriptor, an allowed manifest media type (never an index), a sha256
// digest, a platform that is absent or linux, and exactly one annotation,
// org.opencontainers.image.ref.name. A second annotation is refused because
// io.containerd.image.name, if present, would override the name.
func parseOCIIndex(raw string) (ociIndexInfo, error) {
	var doc struct {
		SchemaVersion int    `json:"schemaVersion"`
		MediaType     string `json:"mediaType"`
		Manifests     []struct {
			MediaType string `json:"mediaType"`
			Size      int64  `json:"size"`
			Digest    string `json:"digest"`
			Platform  *struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
			} `json:"platform"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return ociIndexInfo{}, fmt.Errorf("parse index.json: %w", err)
	}
	if dec.More() {
		return ociIndexInfo{}, fmt.Errorf("parse index.json: trailing data")
	}
	if doc.SchemaVersion != 2 || doc.MediaType != ociIndexMediaType {
		return ociIndexInfo{}, fmt.Errorf("index.json schemaVersion %d mediaType %q, want 2 and %s", doc.SchemaVersion, doc.MediaType, ociIndexMediaType)
	}
	if len(doc.Manifests) != 1 {
		return ociIndexInfo{}, fmt.Errorf("index.json has %d manifests, want exactly 1", len(doc.Manifests))
	}
	m := doc.Manifests[0]
	if m.MediaType != ociManifestMediaType && m.MediaType != dockerManifestMediaType {
		return ociIndexInfo{}, fmt.Errorf("index.json manifest mediaType %q is not %s or %s", m.MediaType, ociManifestMediaType, dockerManifestMediaType)
	}
	if !lockDigestRE.MatchString(m.Digest) {
		return ociIndexInfo{}, fmt.Errorf("index.json manifest digest %q is not sha256:<64 hex>", m.Digest)
	}
	if m.Size < 1 {
		return ociIndexInfo{}, fmt.Errorf("index.json manifest size %d is not positive", m.Size)
	}
	if m.Platform != nil && m.Platform.OS != "linux" {
		return ociIndexInfo{}, fmt.Errorf("index.json platform os %q, want linux", m.Platform.OS)
	}
	if len(m.Annotations) != 1 {
		return ociIndexInfo{}, fmt.Errorf("index.json manifest has %d annotations, want exactly 1 (%s)", len(m.Annotations), ociRefNameAnnotation)
	}
	name, ok := m.Annotations[ociRefNameAnnotation]
	if !ok || name == "" {
		return ociIndexInfo{}, fmt.Errorf("index.json manifest lacks the %s annotation", ociRefNameAnnotation)
	}
	return ociIndexInfo{ManifestDigest: m.Digest, MediaType: m.MediaType, RefName: name}, nil
}

// ociManifestInfo is what IR-9 and IR-10 read from the image manifest blob.
type ociManifestInfo struct {
	// ConfigID is the config digest as the CRI reports an image ID ("sha256:<hex>").
	ConfigID string
	// Layers are the layer digests in manifest order (a repeat stays repeated:
	// containerd labels layers by their index in the manifest).
	Layers []string
}

// parseOCIManifest parses an image manifest (OCI or docker schema 2) far enough
// to reach its config and layer digests, which is the second step from
// index.json to the config digest IR-9 (c) compares to the CRI image ID.
func parseOCIManifest(raw string) (ociManifestInfo, error) {
	var doc struct {
		SchemaVersion int `json:"schemaVersion"`
		Config        struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return ociManifestInfo{}, fmt.Errorf("parse image manifest: %w", err)
	}
	if doc.SchemaVersion != 2 {
		return ociManifestInfo{}, fmt.Errorf("image manifest schemaVersion %d, want 2", doc.SchemaVersion)
	}
	if !lockDigestRE.MatchString(doc.Config.Digest) {
		return ociManifestInfo{}, fmt.Errorf("image manifest config digest %q is not sha256:<64 hex>", doc.Config.Digest)
	}
	if len(doc.Layers) == 0 {
		return ociManifestInfo{}, fmt.Errorf("image manifest has no layers")
	}
	info := ociManifestInfo{ConfigID: doc.Config.Digest}
	for i, l := range doc.Layers {
		if !lockDigestRE.MatchString(l.Digest) {
			return ociManifestInfo{}, fmt.Errorf("image manifest layers[%d] digest %q is not sha256:<64 hex>", i, l.Digest)
		}
		info.Layers = append(info.Layers, l.Digest)
	}
	return info, nil
}

// renderOCIIndexRefName replaces the ref.name annotation of a bundled tarball's
// compact index.json, `"org.opencontainers.image.ref.name":"<oldRef>"`, with
// newRef by exact string replacement, the way bundle-images.sh names a tarball.
// It fails unless that text occurs exactly once, so the result differs from raw
// only there (the manifest digest, and so the lock pin, is unchanged).
func renderOCIIndexRefName(raw, oldRef, newRef string) (string, error) {
	oldText := `"` + ociRefNameAnnotation + `":"` + oldRef + `"`
	if n := strings.Count(raw, oldText); n != 1 {
		return "", fmt.Errorf("index.json contains %s %d times, want exactly once", oldText, n)
	}
	if !plainLockValue(newRef) || newRef == "" {
		return "", fmt.Errorf("new ref %q is not a plain reference", newRef)
	}
	return strings.Replace(raw, oldText, `"`+ociRefNameAnnotation+`":"`+newRef+`"`, 1), nil
}

// ociBlobMemberRE is the shape of every blob member name in a bundled tarball.
var ociBlobMemberRE = regexp.MustCompile(`^blobs/sha256/[0-9a-f]{64}$`)

// parseTarMemberNames parses `tar -tf` output of a bundled tarball and enforces
// the member rule of the canonical layout: oci-layout, then index.json, then
// blob members, no directory members, no duplicates, 3..256 in all. Every name is
// a bundle member name, so it is safe to pass back to tar as an argument. The
// order is kept.
func parseTarMemberNames(raw string) ([]string, error) {
	var names []string
	seen := map[string]bool{}
	for i, line := range strings.Split(strings.TrimSuffix(raw, "\n"), "\n") {
		switch {
		case i == 0 && line != "oci-layout":
			return nil, fmt.Errorf("tar member 1 is %q, want oci-layout", line)
		case i == 1 && line != "index.json":
			return nil, fmt.Errorf("tar member 2 is %q, want index.json", line)
		case i >= 2 && !ociBlobMemberRE.MatchString(line):
			return nil, fmt.Errorf("tar member %q is not a blob member name (blobs/sha256/<64 hex>)", line)
		}
		if seen[line] {
			return nil, fmt.Errorf("tar member %q appears more than once", line)
		}
		seen[line] = true
		names = append(names, line)
	}
	if len(names) < 3 || len(names) > ociMaxMembers {
		return nil, fmt.Errorf("tar has %d members, want 3..%d", len(names), ociMaxMembers)
	}
	return names, nil
}

// ctrImageCheck is one row of `ctr -n k8s.io images check`.
type ctrImageCheck struct {
	Ref string
	// Type and Digest are the record's target media type and digest.
	Type   string
	Digest string
	// Status is complete, incomplete, unavailable or error.
	Status string
	// Present and Required are the N/M of "complete (N/M)"; Required is -1 when
	// containerd cannot tell ("unavailable (N/?)").
	Present, Required int
	Unpacked          bool
}

// Complete reports whether every blob the record needs is present: status
// complete with N == M.
func (c ctrImageCheck) Complete() bool {
	return c.Status == "complete" && c.Required > 0 && c.Present == c.Required
}

var ctrCheckCountRE = regexp.MustCompile(`^\(([0-9]+)/([0-9]+|\?)\)$`)

// parseCtrImagesCheck parses `ctr images check` output (containerd v2.3.5
// cmd/ctr/commands/images/check.go): a REF TYPE DIGEST STATUS SIZE UNPACKED
// header, then per image
//
//	<ref> <type> <digest> complete (18/18) <present>/<required> <unpacked>
//
// (the "error" status has no count and a "-" size). Names never contain
// whitespace. It rejects a missing header, a malformed row and a repeated ref.
func parseCtrImagesCheck(raw string) ([]ctrImageCheck, error) {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) == 0 || strings.Join(strings.Fields(lines[0]), " ") != "REF TYPE DIGEST STATUS SIZE UNPACKED" {
		return nil, fmt.Errorf("ctr images check output lacks the REF TYPE DIGEST STATUS SIZE UNPACKED header: %q", trimForLog(raw))
	}
	var rows []ctrImageCheck
	seen := map[string]bool{}
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		if len(f) < 6 {
			return nil, fmt.Errorf("ctr images check row %q has %d fields, want at least 6", line, len(f))
		}
		row := ctrImageCheck{Ref: f[0], Type: f[1], Digest: f[2], Status: f[3], Required: -1}
		if !lockDigestRE.MatchString(row.Digest) {
			return nil, fmt.Errorf("ctr images check row %q: digest %q is not sha256:<64 hex>", line, row.Digest)
		}
		switch row.Status {
		case "complete", "incomplete", "unavailable":
			m := ctrCheckCountRE.FindStringSubmatch(f[4])
			if m == nil || len(f) < 7 {
				return nil, fmt.Errorf("ctr images check row %q: want a (N/M) count after %s", line, row.Status)
			}
			row.Present, _ = strconv.Atoi(m[1])
			if m[2] != "?" {
				row.Required, _ = strconv.Atoi(m[2])
			}
		case "error":
		default:
			return nil, fmt.Errorf("ctr images check row %q: unknown status %q", line, row.Status)
		}
		switch f[len(f)-1] {
		case "true":
			row.Unpacked = true
		case "false":
		default:
			return nil, fmt.Errorf("ctr images check row %q: last field %q is not true or false", line, f[len(f)-1])
		}
		if seen[row.Ref] {
			return nil, fmt.Errorf("ctr images check lists %q more than once", row.Ref)
		}
		seen[row.Ref] = true
		rows = append(rows, row)
	}
	return rows, nil
}

// ctrContent is one blob of `ctr -n k8s.io content ls`.
type ctrContent struct {
	Digest string
	Labels map[string]string
}

// parseCtrContentLs parses `ctr content ls` output (cmd/ctr/commands/content/
// content.go): a DIGEST SIZE AGE LABELS header, then one blob per line with the
// digest first and the LABELS column last ("-" for none, else sorted k=v pairs
// joined by commas). SIZE is one token; AGE may hold spaces ("3 minutes"), so
// only the first and last fields are used. `ctr content info` does not exist in
// containerd 2.3.5: this listing is the only way to read a blob's labels. It
// rejects a missing header, a malformed row and a repeated digest.
func parseCtrContentLs(raw string) (map[string]ctrContent, error) {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) == 0 || strings.Join(strings.Fields(lines[0]), " ") != "DIGEST SIZE AGE LABELS" {
		return nil, fmt.Errorf("ctr content ls output lacks the DIGEST SIZE AGE LABELS header: %q", trimForLog(raw))
	}
	out := map[string]ctrContent{}
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		if len(f) < 4 {
			return nil, fmt.Errorf("ctr content ls row %q has %d fields, want at least 4", line, len(f))
		}
		c := ctrContent{Digest: f[0], Labels: map[string]string{}}
		if !lockDigestRE.MatchString(c.Digest) {
			return nil, fmt.Errorf("ctr content ls row %q: digest %q is not sha256:<64 hex>", line, c.Digest)
		}
		if last := f[len(f)-1]; last != "-" {
			for _, kv := range strings.Split(last, ",") {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || k == "" {
					return nil, fmt.Errorf("ctr content ls row %q: label %q is not key=value", line, kv)
				}
				c.Labels[k] = v
			}
		}
		if _, dup := out[c.Digest]; dup {
			return nil, fmt.Errorf("ctr content ls lists %s more than once", c.Digest)
		}
		out[c.Digest] = c
	}
	return out, nil
}

// gcRefLabel is containerd's label prefix that roots a blob's children: without
// them the config and layers of an imported image are unreferenced and GC may
// delete them.
const gcRefLabel = "containerd.io/gc.ref.content."

// gcRootViolations lists how a manifest blob's labels differ from the rooting
// containerd writes when it walks a manifest on import: .config naming the
// config digest and .l.<i> naming layer i (by manifest index), nothing missing
// or wrong. Extra labels are not a violation.
func gcRootViolations(m ociManifestInfo, labels map[string]string) []string {
	var v []string
	want := map[string]string{gcRefLabel + "config": m.ConfigID}
	for i, l := range m.Layers {
		want[gcRefLabel+"l."+strconv.Itoa(i)] = l
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch got, ok := labels[k]; {
		case !ok:
			v = append(v, fmt.Sprintf("label %s is missing (want %s)", k, want[k]))
		case got != want[k]:
			v = append(v, fmt.Sprintf("label %s=%s, want %s", k, got, want[k]))
		}
	}
	return v
}

// crictlImageStatus is the part of `crictl inspecti -o json <one ref>` IR-9
// reads (cri-tools v1.37.0 cmd/crictl/util.go outputStatusData: a single object
// with the CRI Image message under "status", protojson field names).
type crictlImageStatus struct {
	Status *struct {
		ID       string   `json:"id"`
		RepoTags []string `json:"repoTags"`
	} `json:"status"`
}

// parseCrictlImageStatus returns the image ID and repo tags from crictl's
// inspecti JSON for one image. Other fields (size, spec, info) are ignored.
func parseCrictlImageStatus(raw string) (id string, repoTags []string, err error) {
	var st crictlImageStatus
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := dec.Decode(&st); err != nil {
		return "", nil, fmt.Errorf("parse crictl inspecti JSON: %w", err)
	}
	if dec.More() {
		return "", nil, fmt.Errorf("parse crictl inspecti JSON: trailing data (more than one image?)")
	}
	if st.Status == nil {
		return "", nil, fmt.Errorf("crictl inspecti JSON has no status")
	}
	if !lockDigestRE.MatchString(st.Status.ID) {
		return "", nil, fmt.Errorf("crictl inspecti status.id %q is not sha256:<64 hex>", st.Status.ID)
	}
	return st.Status.ID, st.Status.RepoTags, nil
}

// sandboxImageLineRE matches a whole, trimmed sandbox_image assignment: a basic
// string without '"' or '\' and nothing after it, not even a comment. (RE2 \s is
// tab, LF, FF, CR and space.)
var sandboxImageLineRE = regexp.MustCompile(`^sandbox_image\s*=\s*"([^"\\]+)"$`)

// asciiSpace is the whitespace parseSandboxImage trims: exactly what [:space:]
// is in the C locale, so scripts/verify-bundle-refs.sh trims the same bytes.
const asciiSpace = " \t\n\v\f\r"

// parseSandboxImage returns the sandbox_image of a containerd version-2 config
// (the shape containerd/config.toml ships and the Dockerfile rewrites). It
// requires exactly one uncommented assignment, inside the
// [plugins."io.containerd.grpc.v1.cri"] table, so a stray or commented copy
// cannot be picked up instead of the one containerd uses.
//
// scripts/verify-bundle-refs.sh parse_sandbox_image applies the SAME rule (a
// parity test runs both on the same inputs): no NUL byte; lines trimmed of ASCII
// whitespace; blank and '#' lines ignored; a '[' line names the current table
// (without its '[' / ']' characters); every other line starting with
// sandbox_image must match sandboxImageLineRE in the cri table. A trailing
// comment is refused, not stripped: the Dockerfile rewrites the whole line, so
// one can only come from an edit that bypassed that rewrite.
func parseSandboxImage(toml string) (string, error) {
	const criTable = `plugins."io.containerd.grpc.v1.cri"`
	if strings.IndexByte(toml, 0) >= 0 {
		return "", fmt.Errorf("config.toml contains a NUL byte")
	}
	table := ""
	var found []string
	for i, line := range strings.Split(toml, "\n") {
		s := strings.Trim(line, asciiSpace)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		if strings.HasPrefix(s, "[") {
			table = strings.Trim(strings.Trim(s, "[]"), asciiSpace)
			continue
		}
		if !strings.HasPrefix(s, "sandbox_image") {
			continue
		}
		m := sandboxImageLineRE.FindStringSubmatch(s)
		if m == nil {
			return "", fmt.Errorf("config.toml line %d: unsupported sandbox_image assignment %q", i+1, s)
		}
		if table != criTable {
			return "", fmt.Errorf("config.toml line %d: sandbox_image is in [%s], want [%s]", i+1, table, criTable)
		}
		found = append(found, m[1])
	}
	if len(found) != 1 {
		return "", fmt.Errorf("config.toml has %d sandbox_image assignments, want exactly 1", len(found))
	}
	return found[0], nil
}

// parseSystemdShow parses `systemctl show --property=...` output ("Key=Value"
// lines; a value may itself contain '=').
func parseSystemdShow(raw string) (map[string]string, error) {
	props := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("systemctl show: line %q is not Key=Value", line)
		}
		props[k] = v
	}
	return props, nil
}

// importUnitProps are the properties the import-unit checks read. Type and
// RemainAfterExit are read so the checks notice when the unit stops being the
// oneshot they are written for.
var importUnitProps = []string{
	"Type", "RemainAfterExit",
	"LoadState", "UnitFileState", "ActiveState", "SubState",
	"Result", "ConditionResult", "ConditionTimestampMonotonic", "ExecMainStatus",
}

// importUnitShapeViolations reports when the unit is no longer Type=oneshot with
// RemainAfterExit=yes. The settled/success checks below are strict on purpose and
// only valid for that shape (a oneshot with RemainAfterExit stays active/exited
// after a successful run); a different shape needs different checks, so it is
// reported as its own violation instead of a confusing state mismatch.
func importUnitShapeViolations(p map[string]string) []string {
	if p["Type"] == "oneshot" && p["RemainAfterExit"] == "yes" {
		return nil
	}
	return []string{fmt.Sprintf("Type=%q RemainAfterExit=%q: the import-unit checks assume Type=oneshot with RemainAfterExit=yes; update importUnitSettled and importUnitViolations together with the unit",
		p["Type"], p["RemainAfterExit"])}
}

// importUnitSettled reports whether the boot-time import unit has reached a state
// it will not leave on its own, so the checks neither race a running import nor
// wait for a unit that will never start: finished (active or failed), inactive
// with its conditions evaluated, or not startable at all (not loaded, or not
// wired into the boot). A unit that is not a oneshot with RemainAfterExit counts
// as settled so importUnitViolations reports that at once.
//
// The wired-in state is "static", not "enabled": since ADR-19 U2 the unit ships in
// /usr/lib/systemd/system with no [Install] section and a relative link in
// /usr/lib/systemd/system/multi-user.target.wants, which systemd reports as static
// (src/shared/install.c:3197-3203 at v260.2). "enabled" would mean an [Install]
// section and a persistent /etc link are back.
func importUnitSettled(p map[string]string) bool {
	switch {
	case p["LoadState"] != "loaded", p["UnitFileState"] != importUnitFileState:
		return true
	case len(importUnitShapeViolations(p)) > 0:
		return true
	case p["ActiveState"] == "active", p["ActiveState"] == "failed":
		return true
	case p["ActiveState"] == "inactive":
		ts := p["ConditionTimestampMonotonic"]
		return ts != "" && ts != "0"
	}
	return false
}

// importUnitFileState is the UnitFileState an image-owned, statically linked unit
// has (ADR-19 U2). See importUnitSettled.
const importUnitFileState = "static"

// importUnitViolations lists how a settled import unit differs from a completed,
// successful run. It assumes, and first checks, Type=oneshot with
// RemainAfterExit=yes and a statically linked unit: then success is loaded,
// static, active/exited, Result=success, conditions met (a unit skipped by a
// condition also reports Result=success) and main process exit status 0. Exit
// status 0 alone does not prove images were imported (outcome=not-bundled also
// exits 0); the callers check the summary line for that.
func importUnitViolations(p map[string]string) []string {
	v := importUnitShapeViolations(p)
	want := []struct{ key, value string }{
		{"LoadState", "loaded"},
		{"UnitFileState", importUnitFileState},
		{"ActiveState", "active"},
		{"SubState", "exited"},
		{"Result", "success"},
		{"ConditionResult", "yes"},
		{"ExecMainStatus", "0"},
	}
	for _, w := range want {
		if got := p[w.key]; got != w.value {
			v = append(v, fmt.Sprintf("%s=%q, want %q", w.key, got, w.value))
		}
	}
	return v
}

// importSummary is the one summary line `import-images` ends every run with
// (ADR-16-A2 decision 8):
//
//	image-import: summary outcome=<outcome> entries=M imported=N refused=R failed=F unlisted=K readonly=<bool> dir=<dir>
type importSummary struct {
	Outcome  string
	Entries  int
	Imported int
	Refused  int
	Failed   int
	Unlisted int
	ReadOnly bool
	Dir      string
}

// String renders the summary fields the way the provider prints them.
func (s importSummary) String() string {
	return fmt.Sprintf("outcome=%s entries=%d imported=%d refused=%d failed=%d unlisted=%d readonly=%t dir=%s",
		s.Outcome, s.Entries, s.Imported, s.Refused, s.Failed, s.Unlisted, s.ReadOnly, s.Dir)
}

// importOutcomes is the closed set of summary outcomes.
var importOutcomes = map[string]bool{
	"success": true, "partial": true, "refused": true, "failed": true, "not-bundled": true, "verified": true,
}

// importSummaryRE matches the summary inside a log line. dir stops at whitespace,
// a quote or a backslash, because logrus's non-TTY text format wraps the message
// in msg="...".
var importSummaryRE = regexp.MustCompile(`image-import: summary outcome=([a-z-]+) entries=([0-9]+) imported=([0-9]+) refused=([0-9]+) failed=([0-9]+) unlisted=([0-9]+) readonly=(true|false) dir=([^\s"\\]+)`)

// parseImportSummary parses the import-images summary. It requires exactly one
// summary line and requires it to be the last non-blank line of out, as the
// provider guarantees, so a missing, repeated or not-final summary fails instead
// of being guessed at.
func parseImportSummary(out string) (importSummary, error) {
	var summaries []string
	last := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		last = line
		if strings.Contains(line, "image-import: summary ") {
			summaries = append(summaries, line)
		}
	}
	if len(summaries) != 1 {
		return importSummary{}, fmt.Errorf("want exactly one image-import summary line, found %d", len(summaries))
	}
	if last != summaries[0] {
		return importSummary{}, fmt.Errorf("the image-import summary is not the last line; last line: %q", last)
	}
	m := importSummaryRE.FindStringSubmatch(last)
	if m == nil {
		return importSummary{}, fmt.Errorf("malformed image-import summary line %q", last)
	}
	if !importOutcomes[m[1]] {
		return importSummary{}, fmt.Errorf("image-import summary outcome %q is not a known outcome", m[1])
	}
	s := importSummary{Outcome: m[1], ReadOnly: m[7] == "true", Dir: m[8]}
	for i, dst := range []*int{&s.Entries, &s.Imported, &s.Refused, &s.Failed, &s.Unlisted} {
		n, err := strconv.Atoi(m[i+2])
		if err != nil {
			return importSummary{}, fmt.Errorf("image-import summary count %q: %w", m[i+2], err)
		}
		*dst = n
	}
	return s, nil
}

// importEntry is one per-entry import-images line:
//
//	image-import: imported <tarball> ref=<ref>
//	image-import: refused <tarball> ref=<ref> reason=<reason> detail=<quoted>
//	image-import: failed <tarball> ref=<ref> reason=ctr-failed detail=<quoted>
type importEntry struct {
	Verb    string
	Tarball string
	Ref     string
	Reason  string
}

// importReasons is the closed set of refusal and failure reasons.
var importReasons = map[string]bool{
	"dir-unsafe": true, "anchor-unsafe": true, "lock-missing": true, "lock-invalid": true,
	"missing": true, "symlink": true, "not-regular": true, "owner": true, "mode": true,
	"size": true, "device": true, "tar-structure": true, "manifest": true,
	"config-digest": true, "ctr-failed": true, "deadline": true,
}

// importEntryRE matches a per-entry line. The "imported N tarball(s) from DIR"
// line does not match: its second word is a count, not a *.tar name followed by
// ref=.
var importEntryRE = regexp.MustCompile(`image-import: (imported|refused|failed) ([A-Za-z0-9._-]+\.tar) ref=([^\s"\\]+)(?: reason=([a-z-]+))?`)

// parseImportEntries returns every per-entry line in out, in order. A refused or
// failed line must carry a known reason (failed only ctr-failed or deadline); an
// imported line must not carry one.
func parseImportEntries(out string) ([]importEntry, error) {
	var entries []importEntry
	for _, line := range strings.Split(out, "\n") {
		m := importEntryRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		e := importEntry{Verb: m[1], Tarball: m[2], Ref: m[3], Reason: m[4]}
		switch {
		case e.Verb == "imported" && e.Reason != "":
			return nil, fmt.Errorf("imported line carries a reason: %q", line)
		case e.Verb != "imported" && !importReasons[e.Reason]:
			return nil, fmt.Errorf("%s line has no known reason: %q", e.Verb, line)
		case e.Verb == "failed" && e.Reason != "ctr-failed" && e.Reason != "deadline":
			return nil, fmt.Errorf("failed line has reason %q, want ctr-failed or deadline: %q", e.Reason, line)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// bundleRefusedRE matches the one line a whole-bundle refusal logs before its
// summary: "image-import: bundle refused reason=<reason> detail=<quoted>". It is
// deliberately not shaped like a per-tarball refused line. The reason is a
// directory/anchor/lock reason, or the lock file's own file-check reason (for
// example device when images.lock is on another filesystem).
var bundleRefusedRE = regexp.MustCompile(`image-import: bundle refused reason=([a-z-]+)`)

// importOutput is everything parsed from one import-images run's output.
type importOutput struct {
	Summary importSummary
	Entries []importEntry
	// BundleRefusal is the reason of the whole-bundle refusal line, or "".
	BundleRefusal string
}

// parseImportOutput parses the summary (exactly one, last), the per-entry lines
// and at most one well-formed bundle refusal line with a known reason.
func parseImportOutput(out string) (importOutput, error) {
	var o importOutput
	var err error
	if o.Summary, err = parseImportSummary(out); err != nil {
		return importOutput{}, err
	}
	if o.Entries, err = parseImportEntries(out); err != nil {
		return importOutput{}, err
	}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "image-import: bundle refused ") {
			continue
		}
		n++
		m := bundleRefusedRE.FindStringSubmatch(line)
		if m == nil || !importReasons[m[1]] {
			return importOutput{}, fmt.Errorf("malformed bundle refusal line %q", line)
		}
		o.BundleRefusal = m[1]
	}
	if n > 1 {
		return importOutput{}, fmt.Errorf("want at most one bundle refusal line, found %d", n)
	}
	return o, nil
}

// importUnlistedLines returns the text after "image-import: unlisted " for every
// line reporting a bundle file that images.lock does not list.
func importUnlistedLines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if _, rest, ok := strings.Cut(line, "image-import: unlisted "); ok {
			lines = append(lines, strings.TrimRight(rest, "\r"))
		}
	}
	return lines
}

// wantImport is what one import-images run should have done.
type wantImport struct {
	// Outcome is the summary outcome.
	Outcome string
	// Refused maps each tarball that must be refused to its reason. Every other
	// images.lock entry must be imported (with VerifyOnly: must not be refused).
	Refused map[string]string
	// BundleRefused is the reason the whole bundle must be refused with (one
	// "bundle refused" line, no entry lines, all counts 0); "" means it must not be.
	BundleRefused string
	// Unlisted is the number of bundle files images.lock does not list.
	Unlisted int
	// VerifyOnly marks an `import-images --verify-only` run: nothing is imported.
	VerifyOnly bool
	// SummaryOnly judges the summary alone (the boot run, read from the journal).
	SummaryOnly bool
}

// importRunViolations lists every way a parsed import-images run differs from
// want, for the images.lock the node image ships: the summary outcome and counts,
// the bundle directory, whether the whole bundle was refused, and one line per
// lock entry with the lock's ref, imported or refused with the wanted reason. A
// line for a tarball the lock does not list, a second line for one tarball, or
// any failed line is a violation too.
func importRunViolations(o importOutput, lock imagesLock, want wantImport) []string {
	var v []string
	s := o.Summary
	check := func(field string, got, wantValue any) {
		if got != wantValue {
			v = append(v, fmt.Sprintf("summary %s=%v, want %v", field, got, wantValue))
		}
	}
	check("outcome", s.Outcome, want.Outcome)
	check("dir", s.Dir, hostexec.BundleDir)
	check("failed", s.Failed, 0)
	if want.BundleRefused != "" {
		// Refused before any entry was looked at: the counts are all zero.
		check("entries", s.Entries, 0)
		check("imported", s.Imported, 0)
		check("refused", s.Refused, 0)
		check("unlisted", s.Unlisted, 0)
		if !want.SummaryOnly {
			if o.BundleRefusal != want.BundleRefused {
				v = append(v, fmt.Sprintf("bundle refusal reason=%q, want %q", o.BundleRefusal, want.BundleRefused))
			}
			for _, e := range o.Entries {
				v = append(v, fmt.Sprintf("%s %s after the whole bundle was refused", e.Verb, e.Tarball))
			}
		}
		return v
	}
	wantImported := len(lock.Images) - len(want.Refused)
	if want.VerifyOnly {
		wantImported = 0
	}
	check("entries", s.Entries, len(lock.Images))
	check("imported", s.Imported, wantImported)
	check("refused", s.Refused, len(want.Refused))
	check("unlisted", s.Unlisted, want.Unlisted)
	if want.SummaryOnly {
		return v
	}
	if o.BundleRefusal != "" {
		v = append(v, fmt.Sprintf("the whole bundle was refused (reason=%s)", o.BundleRefusal))
	}
	entries := o.Entries

	byTarball := map[string]imagesLockItem{}
	for _, img := range lock.Images {
		byTarball[img.Tarball] = img
	}
	for tarball := range want.Refused {
		if _, listed := byTarball[tarball]; !listed {
			v = append(v, fmt.Sprintf("test bug: want %s refused, but images.lock does not list it", tarball))
		}
	}
	seen := map[string]string{}
	for _, e := range entries {
		img, listed := byTarball[e.Tarball]
		if !listed {
			v = append(v, fmt.Sprintf("%s %s: not an images.lock tarball", e.Verb, e.Tarball))
			continue
		}
		if prev, dup := seen[e.Tarball]; dup {
			v = append(v, fmt.Sprintf("%s %s: already reported as %s", e.Verb, e.Tarball, prev))
			continue
		}
		seen[e.Tarball] = e.Verb
		if e.Ref != img.Ref {
			v = append(v, fmt.Sprintf("%s %s: ref=%s, images.lock has %s", e.Verb, e.Tarball, e.Ref, img.Ref))
		}
		wantReason, refuse := want.Refused[e.Tarball]
		switch {
		case e.Verb == "failed":
			v = append(v, fmt.Sprintf("failed %s reason=%s", e.Tarball, e.Reason))
		case e.Verb == "refused" && !refuse:
			v = append(v, fmt.Sprintf("refused %s reason=%s, want it accepted", e.Tarball, e.Reason))
		case e.Verb == "refused" && e.Reason != wantReason:
			v = append(v, fmt.Sprintf("refused %s reason=%s, want reason=%s", e.Tarball, e.Reason, wantReason))
		case e.Verb == "imported" && want.VerifyOnly:
			v = append(v, fmt.Sprintf("imported %s in a --verify-only run", e.Tarball))
		case e.Verb == "imported" && refuse:
			v = append(v, fmt.Sprintf("imported %s, want it refused with reason=%s", e.Tarball, wantReason))
		}
	}
	for _, tarball := range lock.tarballs() {
		_, reported := seen[tarball]
		_, refuse := want.Refused[tarball]
		switch {
		case refuse && !reported:
			v = append(v, fmt.Sprintf("%s: no refused line, want reason=%s", tarball, want.Refused[tarball]))
		case !refuse && !want.VerifyOnly && !reported:
			v = append(v, fmt.Sprintf("%s: no imported line", tarball))
		}
	}
	return v
}

// walkDirs returns every directory from / down to and including each of dirs,
// each once, in walk order: the directories the importer opens one by one
// (ADR-16-A2 decision 3).
func walkDirs(dirs ...string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, d := range dirs {
		add("/")
		p := ""
		for _, part := range strings.Split(strings.Trim(path.Clean(d), "/"), "/") {
			if part == "" {
				continue
			}
			p += "/" + part
			add(p)
		}
	}
	return out
}

// bundlePathViolations lists how one path on the importer's walk breaks the rule
// the importer enforces (ADR-16-A2 decision 3): the wanted type, not reached
// through a symlink (stat without -L), owned by uid 0, no group or other write,
// and no setuid or setgid bit. The group is not required, as at runtime.
func bundlePathViolations(st fileStat, wantType string) []string {
	var v []string
	if st.Type != wantType {
		v = append(v, fmt.Sprintf("type %q, want %q (not following symlinks)", st.Type, wantType))
	}
	if st.UID != 0 {
		v = append(v, fmt.Sprintf("owner uid %d, want 0", st.UID))
	}
	if st.Mode&(modeGroupWrite|modeOtherWrite) != 0 {
		v = append(v, fmt.Sprintf("mode %04o is group- or other-writable", st.Mode))
	}
	if st.Mode&(modeSetuid|modeSetgid) != 0 {
		v = append(v, fmt.Sprintf("mode %04o has the setuid or setgid bit", st.Mode))
	}
	return v
}

// parseImageRefList parses a one-reference-per-line listing (`kubeadm config
// images list`, `ctr images ls -q`), rejecting blank-only output, whitespace
// inside a reference, and duplicates.
func parseImageRefList(raw string) ([]string, error) {
	var refs []string
	seen := map[string]bool{}
	for _, line := range strings.Split(raw, "\n") {
		ref := strings.TrimSpace(line)
		if ref == "" {
			continue
		}
		if strings.ContainsAny(ref, " \t") {
			return nil, fmt.Errorf("image list line %q is not a single reference", ref)
		}
		if seen[ref] {
			return nil, fmt.Errorf("image list repeats %q", ref)
		}
		seen[ref] = true
		refs = append(refs, ref)
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("image list is empty")
	}
	sort.Strings(refs)
	return refs, nil
}

// misnamedImportedImages returns the image names that show an import under the
// wrong name: crane's placeholder tag for digest pulls, or ctr's generated
// "import-<date>" prefix for a tarball without RepoTags.
func misnamedImportedImages(names []string) []string {
	var bad []string
	for _, n := range names {
		if strings.Contains(n, "i-was-a-digest") || strings.HasPrefix(n, "import-") {
			bad = append(bad, n)
		}
	}
	return bad
}

// setDiff returns the elements of a missing from b, sorted.
func setDiff(a, b []string) []string {
	in := map[string]bool{}
	for _, s := range b {
		in[s] = true
	}
	var out []string
	for _, s := range a {
		if !in[s] {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// containsString reports whether s is in list.
func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// trimForLog bounds command output quoted in a failure message.
func trimForLog(s string) string {
	s = strings.TrimSpace(s)
	const limit = 2000
	if len(s) > limit {
		return s[:limit] + "...(truncated)"
	}
	return s
}
