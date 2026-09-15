//go:build e2e

package e2e

// Container-free tests for the IR-9 parsers in bundled_images.go, pinned against
// the shapes the real tools produce (bundle-images.sh's images.lock, crane's
// manifest.json, crictl v1.37 inspecti JSON, systemctl show, containerd's
// shipped config.toml).

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

const (
	hexA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hexB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// lockJSON renders an images.lock in the exact layout bundle-images.sh writes.
func lockJSON(entries ...string) string {
	return "{\n" +
		`  "kubernetesVersion": "v1.37.0",` + "\n" +
		`  "imageRepository": "registry.k8s.io",` + "\n" +
		`  "verifiedBy": {"identity": "krel-trust@k8s-releng-prod.iam.gserviceaccount.com", "issuer": "https://accounts.google.com"},` + "\n" +
		"  \"images\": [\n" + strings.Join(entries, ",\n") + "\n  ]\n}\n"
}

func lockEntry(ref, digest, tarball string) string {
	return `    {"ref": "` + ref + `", "digest": "` + digest + `", "tarball": "` + tarball + `", "verified": true, "verifyReason": ""}`
}

func TestParseImagesLock(t *testing.T) {
	raw := lockJSON(
		lockEntry("registry.k8s.io/pause:3.10.2", "sha256:"+hexA, "registry.k8s.io_pause_3.10.2.tar"),
		lockEntry("registry.k8s.io/coredns/coredns:v1.14.6", "sha256:"+hexB, "registry.k8s.io_coredns_coredns_v1.14.6.tar"),
	)
	lock, err := parseImagesLock(raw)
	if err != nil {
		t.Fatalf("valid lock: %v", err)
	}
	if lock.KubernetesVersion != "v1.37.0" || lock.ImageRepository != "registry.k8s.io" || len(lock.Images) != 2 {
		t.Errorf("parsed lock = %+v", lock)
	}
	if got, want := lock.refs(), []string{"registry.k8s.io/coredns/coredns:v1.14.6", "registry.k8s.io/pause:3.10.2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("refs() = %v, want %v", got, want)
	}

	// An additive field in the lock (e.g. from the F-IMPORTREF fix) is tolerated.
	if _, err := parseImagesLock(strings.Replace(raw, `"verified": true`, `"verified": true, "repoTag": "x"`, 1)); err != nil {
		t.Errorf("additive field: unexpected error %v", err)
	}

	good := lockEntry("registry.k8s.io/pause:3.10.2", "sha256:"+hexA, "p.tar")
	bad := map[string]string{
		"not JSON":          "{",
		"no images":         lockJSON(),
		"empty version":     strings.Replace(lockJSON(good), `"v1.37.0"`, `""`, 1),
		"duplicate ref":     lockJSON(good, lockEntry("registry.k8s.io/pause:3.10.2", "sha256:"+hexB, "q.tar")),
		"duplicate tarball": lockJSON(good, lockEntry("registry.k8s.io/etcd:3.7.0-0", "sha256:"+hexB, "p.tar")),
		"empty ref":         lockJSON(lockEntry("", "sha256:"+hexA, "p.tar")),
		"ref with space":    lockJSON(lockEntry("registry.k8s.io/pause 3", "sha256:"+hexA, "p.tar")),
		"digest not sha256": lockJSON(lockEntry("registry.k8s.io/pause:3.10.2", "md5:abc", "p.tar")),
		"short digest":      lockJSON(lockEntry("registry.k8s.io/pause:3.10.2", "sha256:abc", "p.tar")),
		"tarball traversal": lockJSON(lockEntry("registry.k8s.io/pause:3.10.2", "sha256:"+hexA, "../p.tar")),
		"tarball subdir":    lockJSON(lockEntry("registry.k8s.io/pause:3.10.2", "sha256:"+hexA, "a/p.tar")),
		"tarball not .tar":  lockJSON(lockEntry("registry.k8s.io/pause:3.10.2", "sha256:"+hexA, "p.tgz")),
		"tarball only .tar": lockJSON(lockEntry("registry.k8s.io/pause:3.10.2", "sha256:"+hexA, ".tar")),
	}
	for name, raw := range bad {
		if got, err := parseImagesLock(raw); err == nil {
			t.Errorf("%s: got %+v, want an error", name, got)
		}
	}
}

func TestParseDockerSaveManifest(t *testing.T) {
	// crane pull of a digest ref (the F-IMPORTREF shape).
	crane := `[{"Config":"sha256:` + hexA + `","RepoTags":["registry.k8s.io/pause:i-was-a-digest"],"Layers":["` + hexB + `.tar.gz"]}]`
	id, tags, err := parseDockerSaveManifest(crane)
	if err != nil || id != "sha256:"+hexA || !reflect.DeepEqual(tags, []string{"registry.k8s.io/pause:i-was-a-digest"}) {
		t.Errorf("crane manifest: got (%q, %v, %v)", id, tags, err)
	}

	for name, raw := range map[string]string{
		"docker save legacy": `[{"Config":"` + hexA + `.json","RepoTags":["registry.k8s.io/pause:3.10.2"],"Layers":["` + hexB + `/layer.tar"]}]`,
		"docker save OCI":    `[{"Config":"blobs/sha256/` + hexA + `","RepoTags":["registry.k8s.io/pause:3.10.2"],"Layers":["blobs/sha256/` + hexB + `"]}]`,
		"foreign layers":     `[{"Config":"sha256:` + hexA + `","RepoTags":null,"Layers":["x.tar.gz"],"LayerSources":{"sha256:` + hexB + `":{"mediaType":"x"}}}]`,
	} {
		if id, _, err := parseDockerSaveManifest(raw); err != nil || id != "sha256:"+hexA {
			t.Errorf("%s: got (%q, %v), want sha256:%s", name, id, err, hexA)
		}
	}

	bad := map[string]string{
		"not an array":   `{"Config":"sha256:` + hexA + `","Layers":["x"]}`,
		"no images":      `[]`,
		"two images":     `[{"Config":"sha256:` + hexA + `","Layers":["x"]},{"Config":"sha256:` + hexB + `","Layers":["y"]}]`,
		"unknown field":  `[{"Config":"sha256:` + hexA + `","Layers":["x"],"Platform":"linux/amd64"}]`,
		"no layers":      `[{"Config":"sha256:` + hexA + `","RepoTags":["r:t"],"Layers":[]}]`,
		"short config":   `[{"Config":"sha256:abc","Layers":["x"]}]`,
		"uppercase hex":  `[{"Config":"sha256:` + strings.ToUpper(hexA) + `","Layers":["x"]}]`,
		"other algo":     `[{"Config":"sha512:` + hexA + `","Layers":["x"]}]`,
		"trailing data":  `[{"Config":"sha256:` + hexA + `","Layers":["x"]}] []`,
		"empty document": ``,
	}
	for name, raw := range bad {
		if id, _, err := parseDockerSaveManifest(raw); err == nil {
			t.Errorf("%s: got %q, want an error", name, id)
		}
	}
}

func TestParseCrictlImageStatus(t *testing.T) {
	// crictl v1.37 inspecti -o json for one image: protojson status (defaults
	// emitted, uint64 size as a string) plus the runtime's info map.
	raw := `{"info":{"imageSpec":{"config":{}}},"status":{"id":"sha256:` + hexA + `","pinned":false,` +
		`"repoDigests":["registry.k8s.io/pause@sha256:` + hexB + `"],"repoTags":["registry.k8s.io/pause:3.10.2"],` +
		`"size":"320368","spec":null,"uid":null,"username":""}}`
	id, tags, err := parseCrictlImageStatus(raw)
	if err != nil || id != "sha256:"+hexA || !reflect.DeepEqual(tags, []string{"registry.k8s.io/pause:3.10.2"}) {
		t.Errorf("valid status: got (%q, %v, %v)", id, tags, err)
	}

	bad := map[string]string{
		"not JSON":             `no such image`,
		"no status":            `{"info":{}}`,
		"null status":          `{"status":null}`,
		"id not sha256":        `{"status":{"id":"abc","repoTags":[]}}`,
		"two images (array)":   `[{"status":{"id":"sha256:` + hexA + `"}},{"status":{"id":"sha256:` + hexB + `"}}]`,
		"two objects appended": `{"status":{"id":"sha256:` + hexA + `"}}{"status":{"id":"sha256:` + hexB + `"}}`,
	}
	for name, raw := range bad {
		if id, _, err := parseCrictlImageStatus(raw); err == nil {
			t.Errorf("%s: got %q, want an error", name, id)
		}
	}
}

func TestParseSandboxImage(t *testing.T) {
	// The config.toml the image ships (before the Dockerfile pins the pause tag).
	shipped, err := os.ReadFile("../../containerd/config.toml")
	if err != nil {
		t.Fatalf("read shipped containerd config: %v", err)
	}
	got, err := parseSandboxImage(string(shipped))
	if err != nil || !strings.HasPrefix(got, "registry.k8s.io/pause:") {
		t.Errorf("shipped config.toml: got (%q, %v), want a registry.k8s.io/pause ref", got, err)
	}

	const head = "version = 2\nroot = \"/var/lib/containerd\"\n\n"
	const cri = "[plugins.\"io.containerd.grpc.v1.cri\"]\n"
	const runc = "  [plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.runc]\n    runtime_type = \"io.containerd.runc.v2\"\n"
	good := map[string]string{
		"pinned by the Dockerfile sed": head + cri + "  sandbox_image = \"registry.k8s.io/pause:3.10.2\"\n" + runc,
		"trailing comment":             head + cri + "  sandbox_image = \"registry.k8s.io/pause:3.10.2\" # pinned\n" + runc,
		"commented copy ignored":       head + "# sandbox_image = \"registry.k8s.io/pause:3.9\"\n" + cri + "  sandbox_image = \"registry.k8s.io/pause:3.10.2\"\n",
	}
	for name, cfg := range good {
		if got, err := parseSandboxImage(cfg); err != nil || got != "registry.k8s.io/pause:3.10.2" {
			t.Errorf("%s: got (%q, %v)", name, got, err)
		}
	}

	bad := map[string]string{
		"none":             head + cri + runc,
		"only commented":   head + cri + "  # sandbox_image = \"registry.k8s.io/pause:3.10.2\"\n",
		"two assignments":  head + cri + "  sandbox_image = \"registry.k8s.io/pause:3.10.2\"\n  sandbox_image = \"registry.k8s.io/pause:3.9\"\n",
		"wrong table":      head + cri + runc + "    sandbox_image = \"registry.k8s.io/pause:3.10.2\"\n",
		"top level":        head + "sandbox_image = \"registry.k8s.io/pause:3.10.2\"\n" + cri,
		"single quotes":    head + cri + "  sandbox_image = 'registry.k8s.io/pause:3.10.2'\n",
		"empty value":      head + cri + "  sandbox_image = \"\"\n",
		"version 3 config": "version = 3\n[plugins.'io.containerd.cri.v1.images'.pinned_images]\n  sandbox = \"registry.k8s.io/pause:3.10.2\"\n",
	}
	for name, cfg := range bad {
		if got, err := parseSandboxImage(cfg); err == nil {
			t.Errorf("%s: got %q, want an error", name, got)
		}
	}
}

func TestImportUnitState(t *testing.T) {
	show := func(s string) map[string]string {
		t.Helper()
		p, err := parseSystemdShow(s)
		if err != nil {
			t.Fatalf("parseSystemdShow(%q): %v", s, err)
		}
		return p
	}
	const base = "LoadState=loaded\nUnitFileState=enabled\n"

	done := show(base + "ActiveState=active\nSubState=exited\nResult=success\nConditionResult=yes\nConditionTimestampMonotonic=5170243\nExecMainStatus=0\n")
	if !importUnitSettled(done) || len(importUnitViolations(done)) != 0 {
		t.Errorf("successful oneshot: settled=%v violations=%v", importUnitSettled(done), importUnitViolations(done))
	}

	cases := []struct {
		name      string
		props     string
		settled   bool
		violation string
	}{
		{"still importing", base + "ActiveState=activating\nSubState=start\nResult=success\nConditionResult=yes\nConditionTimestampMonotonic=5170243\nExecMainStatus=0\n", false, "ActiveState"},
		{"not started yet", base + "ActiveState=inactive\nSubState=dead\nResult=success\nConditionResult=no\nConditionTimestampMonotonic=0\nExecMainStatus=0\n", false, "ActiveState"},
		{"skipped by its condition", base + "ActiveState=inactive\nSubState=dead\nResult=success\nConditionResult=no\nConditionTimestampMonotonic=5170243\nExecMainStatus=0\n", true, "ConditionResult"},
		{"import failed", base + "ActiveState=failed\nSubState=failed\nResult=exit-code\nConditionResult=yes\nConditionTimestampMonotonic=5170243\nExecMainStatus=1\n", true, "Result"},
		{"masked", "LoadState=masked\nUnitFileState=masked\nActiveState=inactive\nSubState=dead\nResult=success\nConditionResult=no\nConditionTimestampMonotonic=0\nExecMainStatus=0\n", true, "LoadState"},
		{"disabled", "LoadState=loaded\nUnitFileState=disabled\nActiveState=inactive\nSubState=dead\nResult=success\nConditionResult=no\nConditionTimestampMonotonic=0\nExecMainStatus=0\n", true, "UnitFileState"},
	}
	for _, tc := range cases {
		p := show(tc.props)
		if got := importUnitSettled(p); got != tc.settled {
			t.Errorf("%s: settled = %v, want %v", tc.name, got, tc.settled)
		}
		if v := strings.Join(importUnitViolations(p), "; "); !strings.Contains(v, tc.violation) {
			t.Errorf("%s: violations %q, want one about %s", tc.name, v, tc.violation)
		}
	}

	if p := show("ExecStart={ path=/x ; argv[]=/x import-images }\n"); p["ExecStart"] != "{ path=/x ; argv[]=/x import-images }" {
		t.Errorf("value containing '=': got %q", p["ExecStart"])
	}
	for _, bad := range []string{"NoEquals\n", "=value\n"} {
		if p, err := parseSystemdShow(bad); err == nil {
			t.Errorf("parseSystemdShow(%q) = %v, want an error", bad, p)
		}
	}
}

func TestParseImageRefList(t *testing.T) {
	kubeadm := "registry.k8s.io/kube-apiserver:v1.37.0\nregistry.k8s.io/pause:3.10.2\n\nregistry.k8s.io/coredns/coredns:v1.14.6\n"
	got, err := parseImageRefList(kubeadm)
	want := []string{"registry.k8s.io/coredns/coredns:v1.14.6", "registry.k8s.io/kube-apiserver:v1.37.0", "registry.k8s.io/pause:3.10.2"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("kubeadm list: got (%v, %v), want %v", got, err, want)
	}
	for name, raw := range map[string]string{
		"empty":     "\n \n",
		"duplicate": "registry.k8s.io/pause:3.10.2\nregistry.k8s.io/pause:3.10.2\n",
		"not a ref": "W0914 could not fetch a Kubernetes version\n",
	} {
		if got, err := parseImageRefList(raw); err == nil {
			t.Errorf("%s: got %v, want an error", name, got)
		}
	}
}

func TestMisnamedImportedImages(t *testing.T) {
	names := []string{
		"registry.k8s.io/pause:3.10.2",
		"registry.k8s.io/pause@sha256:" + hexB,
		"sha256:" + hexA,
		"registry.k8s.io/pause:i-was-a-digest",
		"import-2026-09-14@sha256:" + hexA,
	}
	want := []string{"registry.k8s.io/pause:i-was-a-digest", "import-2026-09-14@sha256:" + hexA}
	if got := misnamedImportedImages(names); !reflect.DeepEqual(got, want) {
		t.Errorf("misnamedImportedImages = %v, want %v", got, want)
	}
	if got := misnamedImportedImages(names[:3]); len(got) != 0 {
		t.Errorf("exact refs, digest refs and IDs must pass, got %v", got)
	}
}

func TestSetDiff(t *testing.T) {
	a := []string{"c", "a", "b"}
	b := []string{"b", "d"}
	if got := setDiff(a, b); !reflect.DeepEqual(got, []string{"a", "c"}) {
		t.Errorf("setDiff(a, b) = %v", got)
	}
	if got := setDiff(b, a); !reflect.DeepEqual(got, []string{"d"}) {
		t.Errorf("setDiff(b, a) = %v", got)
	}
	if got := setDiff(a, a); len(got) != 0 {
		t.Errorf("setDiff(a, a) = %v, want empty", got)
	}
}
