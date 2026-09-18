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
// sandbox_image the same way. `ctr images import` names each image after the
// RepoTags in the tarball's manifest.json (containerd v2.3.5
// core/images/archive/importer.go:213-231), or "import-<date>" when there are
// none (cmd/ctr/commands/images/import.go:131-132). A tarball pulled by digest
// with crane carries RepoTags "<repo>:i-was-a-digest" (go-containerregistry
// v0.20.3 pkg/crane/pull.go:33). Imported as is, the images exist but under
// names nothing asks for, so an air-gapped first boot tries to pull and fails.
// Counting imported tarballs cannot see that; these checks can.
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
	Ref          string `json:"ref"`
	Digest       string `json:"digest"`
	Tarball      string `json:"tarball"`
	Verified     bool   `json:"verified"`
	VerifyReason string `json:"verifyReason"`
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
		values = append(values, img.Ref, img.Digest, img.Tarball, img.VerifyReason)
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
		fmt.Fprintf(&b, "    {\"ref\": \"%s\", \"digest\": \"%s\", \"tarball\": \"%s\", \"verified\": %t, \"verifyReason\": \"%s\"}",
			img.Ref, img.Digest, img.Tarball, img.Verified, img.VerifyReason)
	}
	b.WriteString("\n  ]\n}\n")
	return b.String(), nil
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

// retagDockerSaveManifest replaces the RepoTags of a bundled tarball's
// manifest.json, `"RepoTags":["<oldRef>"]`, with newRef by exact string
// replacement, the way bundle-images.sh names a tarball. It fails unless the old
// RepoTags text occurs exactly once, so the result differs from raw only there.
func retagDockerSaveManifest(raw, oldRef, newRef string) (string, error) {
	oldText := `"RepoTags":["` + oldRef + `"]`
	if n := strings.Count(raw, oldText); n != 1 {
		return "", fmt.Errorf("manifest.json contains %s %d times, want exactly once", oldText, n)
	}
	if !plainLockValue(newRef) || newRef == "" {
		return "", fmt.Errorf("new ref %q is not a plain reference", newRef)
	}
	return strings.Replace(raw, oldText, `"RepoTags":["`+newRef+`"]`, 1), nil
}

// dockerSaveMemberRE is the shape of every member name in a bundled tarball.
var dockerSaveMemberRE = regexp.MustCompile(`^(manifest\.json|sha256:[0-9a-f]{64}|[0-9a-f]{64}\.tar\.gz)$`)

// parseTarMemberNames parses `tar -tf` output of a bundled tarball: one member per
// line, every name a bundle member name (so it is safe to pass back to tar as an
// argument), no duplicates, manifest.json last. The order is kept.
func parseTarMemberNames(raw string) ([]string, error) {
	var names []string
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(raw, "\n"), "\n") {
		if !dockerSaveMemberRE.MatchString(line) {
			return nil, fmt.Errorf("tar member %q is not a bundle member name", line)
		}
		if seen[line] {
			return nil, fmt.Errorf("tar member %q appears more than once", line)
		}
		seen[line] = true
		names = append(names, line)
	}
	if len(names) < 3 || names[len(names)-1] != "manifest.json" {
		return nil, fmt.Errorf("tar members %v: want a config, layers and manifest.json last", names)
	}
	return names, nil
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
