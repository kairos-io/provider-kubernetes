//go:build e2e

package e2e

// bundled_images.go holds the pure parsers and checks behind IR-9, the Tier-1
// gate for ADR-16-A1 / F-IMPORTREF: after the boot-time import, every image the
// bundled kubeadm needs must be present in containerd under the EXACT reference
// kubeadm and containerd look it up by.
//
// Why exact references matter: kubeadm decides whether to pull by asking the
// CRI for the image status of the exact ref string (kubeadm v1.37.0
// cmd/kubeadm/app/util/runtime/runtime.go:247-259), and containerd resolves its
// sandbox_image the same way. `ctr images import` names each image after the
// RepoTags in the tarball's manifest.json (containerd v2.3.5
// core/images/archive/importer.go:213-231), or "import-<date>" when there are
// none (cmd/ctr/commands/images/import.go:131-132). A tarball pulled by digest
// with crane carries RepoTags "<repo>:i-was-a-digest" (go-containerregistry
// v0.20.3 pkg/crane/pull.go:33). Imported as is, the images exist but under
// names nothing asks for, so an air-gapped first boot tries to pull and fails.
// Counting imported tarballs (E-B7 (d)) cannot see that; these checks can.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	// imagesLockPath is the lock bundle-images.sh writes next to the tarballs.
	imagesLockPath = bundledImagesDir + "/images.lock"
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

// imagesLock is the part of images.lock IR-9 relies on. Fields the checks do not
// use are tolerated, so an additive change to the lock does not break the gate;
// every field that is used is validated.
type imagesLock struct {
	KubernetesVersion string           `json:"kubernetesVersion"`
	ImageRepository   string           `json:"imageRepository"`
	Images            []imagesLockItem `json:"images"`
}

type imagesLockItem struct {
	Ref     string `json:"ref"`
	Digest  string `json:"digest"`
	Tarball string `json:"tarball"`
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

// dockerSaveEntry is one manifest.json entry of a docker-save tarball as crane
// writes it (go-containerregistry v0.20.3 pkg/v1/tarball/image.go:124-131).
type dockerSaveEntry struct {
	Config       string
	RepoTags     []string
	Layers       []string
	LayerSources map[string]json.RawMessage `json:",omitempty"`
}

// parseDockerSaveManifest strictly parses a tarball's manifest.json, which must
// describe exactly one image, and returns that image's config digest as the CRI
// reports an image ID ("sha256:<hex>") plus its RepoTags. It accepts the Config
// spellings the docker-save family uses: "sha256:<hex>" (crane), "<hex>.json"
// (docker save) and "blobs/sha256/<hex>" (OCI-layout docker save).
func parseDockerSaveManifest(raw string) (configID string, repoTags []string, err error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var entries []dockerSaveEntry
	if err := dec.Decode(&entries); err != nil {
		return "", nil, fmt.Errorf("parse manifest.json: %w", err)
	}
	if dec.More() {
		return "", nil, fmt.Errorf("parse manifest.json: trailing data after the manifest array")
	}
	if len(entries) != 1 {
		return "", nil, fmt.Errorf("manifest.json describes %d images, want exactly 1", len(entries))
	}
	e := entries[0]
	if len(e.Layers) == 0 {
		return "", nil, fmt.Errorf("manifest.json image has no layers")
	}
	hex := e.Config
	switch {
	case strings.HasPrefix(hex, "sha256:"):
		hex = strings.TrimPrefix(hex, "sha256:")
	case strings.HasPrefix(hex, "blobs/sha256/"):
		hex = strings.TrimPrefix(hex, "blobs/sha256/")
	case strings.HasSuffix(hex, ".json"):
		hex = strings.TrimSuffix(hex, ".json")
	}
	if !sha256HexRE.MatchString(hex) {
		return "", nil, fmt.Errorf("manifest.json Config %q is not a sha256 config digest", e.Config)
	}
	return "sha256:" + hex, e.RepoTags, nil
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

// sandboxImageLineRE matches a sandbox_image assignment with a basic string.
var sandboxImageLineRE = regexp.MustCompile(`^sandbox_image\s*=\s*"([^"\\]+)"\s*(#.*)?$`)

// parseSandboxImage returns the sandbox_image of a containerd version-2 config
// (the shape containerd/config.toml ships and the Dockerfile rewrites). It
// requires exactly one uncommented assignment, inside the
// [plugins."io.containerd.grpc.v1.cri"] table, so a stray or commented copy
// cannot be picked up instead of the one containerd uses.
func parseSandboxImage(toml string) (string, error) {
	const criTable = `plugins."io.containerd.grpc.v1.cri"`
	table := ""
	var found []string
	for i, line := range strings.Split(toml, "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		if strings.HasPrefix(s, "[") {
			table = strings.TrimSpace(strings.Trim(s, "[]"))
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

// importUnitProps are the properties the import-unit checks read.
var importUnitProps = []string{
	"LoadState", "UnitFileState", "ActiveState", "SubState",
	"Result", "ConditionResult", "ConditionTimestampMonotonic", "ExecMainStatus",
}

// importUnitSettled reports whether the boot-time import unit has reached a state
// it will not leave on its own, so the checks neither race a running import nor
// wait for a unit that will never start: finished (active or failed), skipped by
// its condition (inactive with the condition evaluated), or not startable at all
// (not loaded, or not enabled).
func importUnitSettled(p map[string]string) bool {
	switch {
	case p["LoadState"] != "loaded", p["UnitFileState"] != "enabled":
		return true
	case p["ActiveState"] == "active", p["ActiveState"] == "failed":
		return true
	case p["ActiveState"] == "inactive":
		ts := p["ConditionTimestampMonotonic"]
		return ts != "" && ts != "0"
	}
	return false
}

// importUnitViolations lists how a settled import unit differs from a completed,
// successful oneshot (Type=oneshot, RemainAfterExit=yes): loaded and enabled (so
// the node image did not mask it), active/exited, Result=success, its
// ConditionPathExists met (a skipped unit also reports Result=success), and main
// process exit status 0.
func importUnitViolations(p map[string]string) []string {
	want := []struct{ key, value string }{
		{"LoadState", "loaded"},
		{"UnitFileState", "enabled"},
		{"ActiveState", "active"},
		{"SubState", "exited"},
		{"Result", "success"},
		{"ConditionResult", "yes"},
		{"ExecMainStatus", "0"},
	}
	var v []string
	for _, w := range want {
		if got := p[w.key]; got != w.value {
			v = append(v, fmt.Sprintf("%s=%q, want %q", w.key, got, w.value))
		}
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
