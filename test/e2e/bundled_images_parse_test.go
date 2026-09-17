//go:build e2e

package e2e

// Container-free tests for the pure helpers in bundled_images.go, pinned against
// the shapes the real tools produce (bundle-images.sh's images.lock, crane's
// manifest.json, crictl v1.37 inspecti JSON, systemctl show, containerd's
// shipped config.toml, logrus text output of import-images).

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kairos-io/provider-kubernetes/internal/hostexec"
)

const (
	hexA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hexB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	hexC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
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
	if got, want := lock.tarballs(), []string{"registry.k8s.io_coredns_coredns_v1.14.6.tar", "registry.k8s.io_pause_3.10.2.tar"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tarballs() = %v, want %v", got, want)
	}
	if lock.VerifiedBy.Identity != "krel-trust@k8s-releng-prod.iam.gserviceaccount.com" || !lock.Images[0].Verified {
		t.Errorf("verifiedBy / verified not parsed: %+v", lock)
	}

	// An additive field in the lock is tolerated by this parser (the importer is
	// stricter; the e2e gate only needs the fields it reads).
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

// TestRenderImagesLockMatchesGoldenLocks pins renderImagesLock (used to plant a
// lock the importer would accept) to the real bundle-images.sh output of every
// supported minor: parse then render must give the golden bytes back.
func TestRenderImagesLockMatchesGoldenLocks(t *testing.T) {
	goldens, err := filepath.Glob("../../internal/imageimport/testdata/images.lock.v*")
	if err != nil || len(goldens) == 0 {
		t.Fatalf("no golden images.lock files (%v)", err)
	}
	for _, path := range goldens {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		lock, err := parseImagesLock(string(raw))
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if want := strings.TrimPrefix(filepath.Base(path), "images.lock."); lock.KubernetesVersion != want {
			t.Errorf("%s: kubernetesVersion %s, want %s", path, lock.KubernetesVersion, want)
		}
		got, err := renderImagesLock(lock)
		if err != nil {
			t.Errorf("%s: render: %v", path, err)
			continue
		}
		if got != string(raw) {
			t.Errorf("%s: render(parse(golden)) differs from the golden bytes:\n--- got ---\n%s--- want ---\n%s", path, got, raw)
		}
		for _, img := range lock.Images {
			if name := tarballNameForRef(img.Ref); name != img.Tarball {
				t.Errorf("%s: tarballNameForRef(%s) = %s, lock has %s", path, img.Ref, name, img.Tarball)
			}
		}
	}

	quoted := imagesLock{KubernetesVersion: "v1.37.0", ImageRepository: "registry.k8s.io", Images: []imagesLockItem{{Ref: `a"b`}}}
	if out, err := renderImagesLock(quoted); err == nil {
		t.Errorf("render with a quote in a value: got %q, want an error", out)
	}
	for _, v := range []string{"a\\b", "a\nb", "a\x7fb", "caf\xc3\xa9"} {
		if plainLockValue(v) {
			t.Errorf("plainLockValue(%q) = true, want false", v)
		}
	}
	if !plainLockValue("https://accounts.google.com") || !plainLockValue("") {
		t.Errorf("plainLockValue rejects a plain value")
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

func TestRetagDockerSaveManifest(t *testing.T) {
	const pause = "registry.k8s.io/pause:3.10.2"
	raw := `[{"Config":"sha256:` + hexA + `","RepoTags":["` + pause + `"],"Layers":["` + hexB + `.tar.gz"]}]`
	got, err := retagDockerSaveManifest(raw, pause, "registry.k8s.io/pause:e2e-opt-canary")
	want := `[{"Config":"sha256:` + hexA + `","RepoTags":["registry.k8s.io/pause:e2e-opt-canary"],"Layers":["` + hexB + `.tar.gz"]}]`
	if err != nil || got != want {
		t.Errorf("retag: got (%q, %v), want %q", got, err, want)
	}
	if _, tags, err := parseDockerSaveManifest(got); err != nil || !reflect.DeepEqual(tags, []string{"registry.k8s.io/pause:e2e-opt-canary"}) {
		t.Errorf("retagged manifest parses to %v, %v", tags, err)
	}

	bad := map[string][3]string{
		"old ref absent":      {raw, "registry.k8s.io/pause:3.9", "x/y:z"},
		"old ref twice":       {raw + `,"RepoTags":["` + pause + `"]`, pause, "x/y:z"},
		"new ref with quote":  {raw, pause, `x/y:"z`},
		"new ref empty":       {raw, pause, ""},
		"only a partial name": {raw, "registry.k8s.io/pause", "x/y:z"},
	}
	for name, in := range bad {
		if out, err := retagDockerSaveManifest(in[0], in[1], in[2]); err == nil {
			t.Errorf("%s: got %q, want an error", name, out)
		}
	}
}

func TestParseTarMemberNames(t *testing.T) {
	cfg := "sha256:" + hexA
	layer := hexB + ".tar.gz"
	got, err := parseTarMemberNames(cfg + "\n" + layer + "\n" + hexC + ".tar.gz\nmanifest.json\n")
	if want := []string{cfg, layer, hexC + ".tar.gz", "manifest.json"}; err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("valid listing: got (%v, %v), want %v", got, err, want)
	}
	bad := map[string]string{
		"empty":             "",
		"manifest not last": "manifest.json\n" + cfg + "\n" + layer + "\n",
		"too few members":   cfg + "\nmanifest.json\n",
		"duplicate member":  cfg + "\n" + layer + "\n" + layer + "\nmanifest.json\n",
		"traversal":         cfg + "\n../" + layer + "\nmanifest.json\n",
		"leading slash":     "/" + cfg + "\n" + layer + "\nmanifest.json\n",
		"option-like name":  "-C\n" + cfg + "\n" + layer + "\nmanifest.json\n",
		"blank line inside": cfg + "\n\n" + layer + "\nmanifest.json\n",
	}
	for name, raw := range bad {
		if got, err := parseTarMemberNames(raw); err == nil {
			t.Errorf("%s: got %v, want an error", name, got)
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

// sandboxCase is one containerd config for parseSandboxImage and, through the
// parity test, for scripts/verify-bundle-refs.sh parse_sandbox_image.
type sandboxCase struct {
	name string
	cfg  string
	// want is the accepted sandbox_image; "" means the config must be refused.
	want string
	// wantPrefix accepts any value with this prefix instead of want.
	wantPrefix string
}

func sandboxImageCases(t *testing.T) []sandboxCase {
	t.Helper()
	// The config.toml the image ships (before the Dockerfile pins the pause tag).
	shipped, err := os.ReadFile("../../containerd/config.toml")
	if err != nil {
		t.Fatalf("read shipped containerd config: %v", err)
	}
	const (
		pause = "registry.k8s.io/pause:3.10.2"
		head  = "version = 2\nroot = \"/var/lib/containerd\"\n\n"
		cri   = "[plugins.\"io.containerd.grpc.v1.cri\"]\n"
		runc  = "  [plugins.\"io.containerd.grpc.v1.cri\".containerd.runtimes.runc]\n    runtime_type = \"io.containerd.runc.v2\"\n"
	)
	pinned := head + cri + "  sandbox_image = \"" + pause + "\"\n" + runc
	return []sandboxCase{
		{name: "shipped config.toml", cfg: string(shipped), wantPrefix: "registry.k8s.io/pause:"},
		{name: "pinned by the Dockerfile sed", cfg: pinned, want: pause},
		{name: "tabs, no spaces around =", cfg: head + cri + "\tsandbox_image=\"" + pause + "\"\n", want: pause},
		{name: "form feed around =", cfg: head + cri + "sandbox_image\f=\f\"" + pause + "\"\n", want: pause},
		{name: "trailing whitespace", cfg: head + cri + "  sandbox_image = \"" + pause + "\" \t\n", want: pause},
		{name: "CRLF line endings", cfg: strings.ReplaceAll(pinned, "\n", "\r\n"), want: pause},
		{name: "no final newline", cfg: head + cri + "  sandbox_image = \"" + pause + "\"", want: pause},
		{name: "commented copy ignored", cfg: head + "# sandbox_image = \"registry.k8s.io/pause:3.9\"\n" + cri + "  sandbox_image = \"" + pause + "\"\n", want: pause},
		{name: "spaces inside the table brackets", cfg: head + "[ plugins.\"io.containerd.grpc.v1.cri\" ]\n  sandbox_image = \"" + pause + "\"\n", want: pause},
		{name: "hash inside the value", cfg: head + cri + "  sandbox_image = \"" + pause + "#x\"\n", want: pause + "#x"},

		{name: "none", cfg: head + cri + runc},
		{name: "only commented", cfg: head + cri + "  # sandbox_image = \"" + pause + "\"\n"},
		{name: "two assignments", cfg: head + cri + "  sandbox_image = \"" + pause + "\"\n  sandbox_image = \"registry.k8s.io/pause:3.9\"\n"},
		{name: "wrong table", cfg: head + cri + runc + "    sandbox_image = \"" + pause + "\"\n"},
		{name: "top level", cfg: head + "sandbox_image = \"" + pause + "\"\n" + cri},
		{name: "single quotes", cfg: head + cri + "  sandbox_image = '" + pause + "'\n"},
		{name: "empty value", cfg: head + cri + "  sandbox_image = \"\"\n"},
		{name: "version 3 config", cfg: "version = 3\n[plugins.'io.containerd.cri.v1.images'.pinned_images]\n  sandbox = \"" + pause + "\"\n"},
		{name: "trailing comment", cfg: head + cri + "  sandbox_image = \"" + pause + "\" # pinned\n"},
		{name: "trailing comment without space", cfg: head + cri + "  sandbox_image = \"" + pause + "\"#pinned\n"},
		{name: "backslash in the value", cfg: head + cri + "  sandbox_image = \"registry.k8s.io\\\\pause:3.10.2\"\n"},
		{name: "similar key", cfg: head + cri + "  sandbox_image_pull = \"" + pause + "\"\n"},
		{name: "table header with a trailing comment", cfg: head + "[plugins.\"io.containerd.grpc.v1.cri\"] # cri\n  sandbox_image = \"" + pause + "\"\n"},
		{name: "vertical tab before the value", cfg: head + cri + "  sandbox_image =\v\"" + pause + "\"\n"},
		{name: "no-break space before the key", cfg: head + cri + "\xc2\xa0sandbox_image = \"" + pause + "\"\n"},
		{name: "NUL byte", cfg: head + cri + "  sandbox_image = \"" + pause + "\"\n\x00\n"},
	}
}

func TestParseSandboxImage(t *testing.T) {
	for _, tc := range sandboxImageCases(t) {
		got, err := parseSandboxImage(tc.cfg)
		switch {
		case tc.wantPrefix != "":
			if err != nil || !strings.HasPrefix(got, tc.wantPrefix) {
				t.Errorf("%s: got (%q, %v), want a %s... value", tc.name, got, err, tc.wantPrefix)
			}
		case tc.want == "":
			if err == nil {
				t.Errorf("%s: got %q, want an error", tc.name, got)
			}
		default:
			if err != nil || got != tc.want {
				t.Errorf("%s: got (%q, %v), want %q", tc.name, got, err, tc.want)
			}
		}
	}
}

// sandboxParityWrapper sources scripts/verify-bundle-refs.sh and runs its
// parse_sandbox_image on one file.
const sandboxParityWrapper = "testdata/parse-sandbox-image.sh"

// TestParseSandboxImageMatchesVerifyScript runs every sandbox case through the CI
// script's parser too: both must accept the same configs with the same value and
// refuse the same configs, so the CI image gate and the e2e gate agree.
func TestParseSandboxImageMatchesVerifyScript(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("bash is needed to run the parser in scripts/verify-bundle-refs.sh: %v", err)
	}
	dir := t.TempDir()
	for i, tc := range sandboxImageCases(t) {
		file := filepath.Join(dir, "config-"+strconv.Itoa(i)+".toml")
		if err := os.WriteFile(file, []byte(tc.cfg), 0o600); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, bash, sandboxParityWrapper, file)
		cmd.WaitDelay = execWaitDelay
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		runErr := cmd.Run()
		cancel()
		var exitErr *exec.ExitError
		if runErr != nil && (!errors.As(runErr, &exitErr) || exitErr.ExitCode() != 1) {
			t.Fatalf("%s: %s did not run cleanly: %v\n%s", tc.name, sandboxParityWrapper, runErr, stderr.String())
		}

		goValue, goErr := parseSandboxImage(tc.cfg)
		shValue := strings.TrimSuffix(stdout.String(), "\n")
		switch {
		case (goErr == nil) != (runErr == nil):
			t.Errorf("%s: Go accepted=%v (%q, %v), script accepted=%v (%q, %s)",
				tc.name, goErr == nil, goValue, goErr, runErr == nil, shValue, strings.TrimSpace(stderr.String()))
		case goErr == nil && goValue != shValue:
			t.Errorf("%s: Go took %q, the script took %q", tc.name, goValue, shValue)
		case goErr != nil && stdout.Len() != 0:
			t.Errorf("%s: the script refused but printed %q", tc.name, stdout.String())
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
	const shape = "Type=oneshot\nRemainAfterExit=yes\n"
	// static, not enabled: since ADR-19 U2 the unit ships in /usr/lib with no
	// [Install] section and a relative multi-user.target.wants link.
	const base = shape + "LoadState=loaded\nUnitFileState=static\n"

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
		{"skipped by a condition", base + "ActiveState=inactive\nSubState=dead\nResult=success\nConditionResult=no\nConditionTimestampMonotonic=5170243\nExecMainStatus=0\n", true, "ConditionResult"},
		{"import failed", base + "ActiveState=failed\nSubState=failed\nResult=exit-code\nConditionResult=yes\nConditionTimestampMonotonic=5170243\nExecMainStatus=1\n", true, "Result"},
		{"masked", shape + "LoadState=masked\nUnitFileState=masked\nActiveState=inactive\nSubState=dead\nResult=success\nConditionResult=no\nConditionTimestampMonotonic=0\nExecMainStatus=0\n", true, "LoadState"},
		{"disabled", shape + "LoadState=loaded\nUnitFileState=disabled\nActiveState=inactive\nSubState=dead\nResult=success\nConditionResult=no\nConditionTimestampMonotonic=0\nExecMainStatus=0\n", true, "UnitFileState"},
		// "enabled" is a finding of its own now: it means an [Install] section and a
		// persistent /etc/systemd/system link are back (ADR-19 U2).
		{"enabled again", shape + "LoadState=loaded\nUnitFileState=enabled\nActiveState=active\nSubState=exited\nResult=success\nConditionResult=yes\nConditionTimestampMonotonic=5170243\nExecMainStatus=0\n", true, "UnitFileState"},
		// A changed unit shape is reported as such, and at once (settled), instead of
		// waiting on or misreading states these checks do not model.
		{"no longer a oneshot", "Type=simple\nRemainAfterExit=no\nLoadState=loaded\nUnitFileState=static\nActiveState=active\nSubState=running\nResult=success\nConditionResult=yes\nConditionTimestampMonotonic=5170243\nExecMainStatus=0\n", true, "assume Type=oneshot"},
		{"no RemainAfterExit", "Type=oneshot\nRemainAfterExit=no\nLoadState=loaded\nUnitFileState=static\nActiveState=inactive\nSubState=dead\nResult=success\nConditionResult=yes\nConditionTimestampMonotonic=5170243\nExecMainStatus=0\n", true, "RemainAfterExit=\"no\""},
		{"shape not reported", "LoadState=loaded\nUnitFileState=static\nActiveState=active\nSubState=exited\nResult=success\nConditionResult=yes\nConditionTimestampMonotonic=5170243\nExecMainStatus=0\n", true, "Type=\"\""},
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
	for _, p := range importUnitProps[:2] {
		if p != "Type" && p != "RemainAfterExit" {
			t.Errorf("importUnitProps must read Type and RemainAfterExit, got %v", importUnitProps)
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

func TestParseImportSummary(t *testing.T) {
	dir := hostexec.BundleDir
	good := []struct {
		name string
		out  string
		want importSummary
	}{
		{
			name: "logrus text without a TTY",
			out: `time="2026-09-15T10:00:01Z" level=info msg="image-import: imported registry.k8s.io_pause_3.10.2.tar ref=registry.k8s.io/pause:3.10.2"` + "\n" +
				`time="2026-09-15T10:00:02Z" level=info msg="image-import: imported 7 tarball(s) from ` + dir + `"` + "\n" +
				`time="2026-09-15T10:00:02Z" level=info msg="image-import: summary outcome=success entries=7 imported=7 refused=0 failed=0 unlisted=0 readonly=true dir=` + dir + `"` + "\n",
			want: importSummary{Outcome: "success", Entries: 7, Imported: 7, ReadOnly: true, Dir: dir},
		},
		{
			name: "logrus text with a TTY, trailing blank lines and CRLF",
			out:  "WARN[0002] image-import: refused a.tar ref=r/a:1 reason=mode detail=\"g+w\"\r\nINFO[0002] image-import: summary outcome=partial entries=7 imported=6 refused=1 failed=0 unlisted=2 readonly=false dir=" + dir + "\r\n\n\n",
			want: importSummary{Outcome: "partial", Entries: 7, Imported: 6, Refused: 1, Unlisted: 2, Dir: dir},
		},
		{
			name: "verify-only",
			out:  `level=info msg="image-import: summary outcome=verified entries=7 imported=0 refused=0 failed=0 unlisted=0 readonly=false dir=` + dir + `"`,
			want: importSummary{Outcome: "verified", Entries: 7, Dir: dir},
		},
	}
	for _, tc := range good {
		got, err := parseImportSummary(tc.out)
		if err != nil || got != tc.want {
			t.Errorf("%s: got (%+v, %v), want %+v", tc.name, got, err, tc.want)
		}
	}
	if s := good[0].want.String(); s != "outcome=success entries=7 imported=7 refused=0 failed=0 unlisted=0 readonly=true dir="+dir {
		t.Errorf("String() = %q", s)
	}

	const line = `msg="image-import: summary outcome=success entries=7 imported=7 refused=0 failed=0 unlisted=0 readonly=true dir=/d"`
	bad := map[string]string{
		"no summary":            `msg="image-import: imported 7 tarball(s) from /d"`,
		"summary repeated":      line + "\n" + line + "\n",
		"summary not last":      line + "\n" + `msg="provider-kubernetes import-images: done"` + "\n",
		"missing field":         `msg="image-import: summary outcome=success entries=7 imported=7 refused=0 failed=0 readonly=true dir=/d"`,
		"unknown outcome":       strings.Replace(line, "outcome=success", "outcome=ok", 1),
		"negative count":        strings.Replace(line, "refused=0", "refused=-1", 1),
		"readonly not a bool":   strings.Replace(line, "readonly=true", "readonly=yes", 1),
		"empty output":          "",
		"old glob-era summary":  `msg="image-import: imported 7 tarball(s) from /opt/provider-kubernetes/images"`,
		"count too large (int)": strings.Replace(line, "entries=7", "entries=99999999999999999999", 1),
	}
	for name, out := range bad {
		if got, err := parseImportSummary(out); err == nil {
			t.Errorf("%s: got %+v, want an error", name, got)
		}
	}
}

func TestParseImportEntries(t *testing.T) {
	out := `time="t" level=info msg="image-import: imported registry.k8s.io_etcd_3.7.0-0.tar ref=registry.k8s.io/etcd:3.7.0-0"` + "\n" +
		`time="t" level=warning msg="image-import: refused registry.k8s.io_pause_3.10.2.tar ref=registry.k8s.io/pause:3.10.2 reason=mode detail=\"mode 0664\""` + "\n" +
		`time="t" level=error msg="image-import: failed registry.k8s.io_kube-proxy_v1.37.0.tar ref=registry.k8s.io/kube-proxy:v1.37.0 reason=ctr-failed detail=\"exit status 1\""` + "\n" +
		`time="t" level=warning msg="image-import: unlisted \"evil.tar\" not imported"` + "\n" +
		`time="t" level=info msg="image-import: imported 7 tarball(s) from /system/provider-kubernetes/images"` + "\n" +
		`time="t" level=info msg="image-import: summary outcome=partial entries=3 imported=1 refused=1 failed=1 unlisted=1 readonly=false dir=/system/provider-kubernetes/images"` + "\n"
	got, err := parseImportEntries(out)
	want := []importEntry{
		{Verb: "imported", Tarball: "registry.k8s.io_etcd_3.7.0-0.tar", Ref: "registry.k8s.io/etcd:3.7.0-0"},
		{Verb: "refused", Tarball: "registry.k8s.io_pause_3.10.2.tar", Ref: "registry.k8s.io/pause:3.10.2", Reason: "mode"},
		{Verb: "failed", Tarball: "registry.k8s.io_kube-proxy_v1.37.0.tar", Ref: "registry.k8s.io/kube-proxy:v1.37.0", Reason: "ctr-failed"},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("parseImportEntries = (%+v, %v), want %+v", got, err, want)
	}
	if u := importUnlistedLines(out); len(u) != 1 || !strings.Contains(u[0], "evil.tar") {
		t.Errorf("importUnlistedLines = %q, want one line naming evil.tar", u)
	}

	bad := map[string]string{
		"refused without a reason":  `msg="image-import: refused a.tar ref=r/a:1"`,
		"refused with a bad reason": `msg="image-import: refused a.tar ref=r/a:1 reason=bogus detail=\"\""`,
		"imported with a reason":    `msg="image-import: imported a.tar ref=r/a:1 reason=mode"`,
		"failed with a refusal":     `msg="image-import: failed a.tar ref=r/a:1 reason=mode detail=\"\""`,
	}
	for name, out := range bad {
		if got, err := parseImportEntries(out); err == nil {
			t.Errorf("%s: got %+v, want an error", name, got)
		}
	}
}

func TestImportRunViolations(t *testing.T) {
	lock := imagesLock{Images: []imagesLockItem{
		{Ref: "registry.k8s.io/etcd:3.7.0-0", Tarball: "registry.k8s.io_etcd_3.7.0-0.tar"},
		{Ref: "registry.k8s.io/coredns/coredns:v1.14.6", Tarball: "registry.k8s.io_coredns_coredns_v1.14.6.tar"},
		{Ref: "registry.k8s.io/pause:3.10.2", Tarball: "registry.k8s.io_pause_3.10.2.tar"},
	}}
	dir := hostexec.BundleDir
	imported := func(i int) importEntry {
		return importEntry{Verb: "imported", Tarball: lock.Images[i].Tarball, Ref: lock.Images[i].Ref}
	}
	refused := func(i int, reason string) importEntry {
		return importEntry{Verb: "refused", Tarball: lock.Images[i].Tarball, Ref: lock.Images[i].Ref, Reason: reason}
	}
	pauseMode := map[string]string{lock.Images[2].Tarball: "mode"}
	success := importSummary{Outcome: "success", Entries: 3, Imported: 3, Dir: dir}
	partial := importSummary{Outcome: "partial", Entries: 3, Imported: 2, Refused: 1, Dir: dir}

	ok := []struct {
		name    string
		s       importSummary
		entries []importEntry
		want    wantImport
	}{
		{"full import", success, []importEntry{imported(0), imported(1), imported(2)}, wantImport{Outcome: "success"}},
		{"verify-only", importSummary{Outcome: "verified", Entries: 3, Dir: dir}, nil, wantImport{Outcome: "verified", VerifyOnly: true}},
		{"one refused", partial, []importEntry{imported(0), refused(2, "mode"), imported(1)}, wantImport{Outcome: "partial", Refused: pauseMode}},
		{"summary only ignores lines", success, []importEntry{refused(0, "mode")}, wantImport{Outcome: "success", SummaryOnly: true}},
		{"unlisted counted", importSummary{Outcome: "success", Entries: 3, Imported: 3, Unlisted: 1, Dir: dir}, []importEntry{imported(0), imported(1), imported(2)}, wantImport{Outcome: "success", Unlisted: 1}},
	}
	for _, tc := range ok {
		if v := importRunViolations(importOutput{Summary: tc.s, Entries: tc.entries}, lock, tc.want); len(v) != 0 {
			t.Errorf("%s: unexpected violations %q", tc.name, v)
		}
	}
	bundleRefused := importOutput{Summary: importSummary{Outcome: "refused", Dir: dir}, BundleRefusal: "device"}
	if v := importRunViolations(bundleRefused, lock, wantImport{Outcome: "refused", BundleRefused: "device"}); len(v) != 0 {
		t.Errorf("whole bundle refused: unexpected violations %q", v)
	}
	bundleBad := []struct {
		name    string
		o       importOutput
		want    wantImport
		mention string
	}{
		{"other bundle reason", importOutput{Summary: bundleRefused.Summary, BundleRefusal: "lock-invalid"}, wantImport{Outcome: "refused", BundleRefused: "device"}, "bundle refusal reason"},
		{"bundle not refused", importOutput{Summary: importSummary{Outcome: "refused", Entries: 3, Refused: 3, Dir: dir}}, wantImport{Outcome: "refused", BundleRefused: "device"}, "entries="},
		{"bundle refused unexpectedly", importOutput{Summary: success, Entries: []importEntry{imported(0), imported(1), imported(2)}, BundleRefusal: "device"}, wantImport{Outcome: "success"}, "whole bundle was refused"},
		{"entries after a bundle refusal", importOutput{Summary: bundleRefused.Summary, Entries: []importEntry{refused(2, "device")}, BundleRefusal: "device"}, wantImport{Outcome: "refused", BundleRefused: "device"}, "after the whole bundle was refused"},
	}
	for _, tc := range bundleBad {
		if v := strings.Join(importRunViolations(tc.o, lock, tc.want), "; "); !strings.Contains(v, tc.mention) {
			t.Errorf("%s: violations %q, want one mentioning %q", tc.name, v, tc.mention)
		}
	}

	all := []importEntry{imported(0), imported(1), imported(2)}
	bad := []struct {
		name    string
		s       importSummary
		entries []importEntry
		want    wantImport
		mention string
	}{
		{"wrong outcome", partial, []importEntry{imported(0), imported(1), refused(2, "mode")}, wantImport{Outcome: "refused", Refused: pauseMode}, "outcome"},
		{"wrong reason", partial, []importEntry{imported(0), imported(1), refused(2, "owner")}, wantImport{Outcome: "partial", Refused: pauseMode}, "want reason=mode"},
		{"tampered entry imported", success, all, wantImport{Outcome: "partial", Refused: pauseMode}, "want it refused"},
		{"entry refused unexpectedly", partial, []importEntry{imported(0), imported(1), refused(2, "mode")}, wantImport{Outcome: "partial"}, "want it accepted"},
		{"missing line", success, all[:2], wantImport{Outcome: "success"}, "no imported line"},
		{"unlisted tarball imported", success, append(all, importEntry{Verb: "imported", Tarball: "evil.tar", Ref: "registry.k8s.io/pause:e2e-evil"}), wantImport{Outcome: "success"}, "not an images.lock tarball"},
		{"duplicate line", success, append(all, imported(0)), wantImport{Outcome: "success"}, "already reported"},
		{"ref mismatch", success, []importEntry{imported(0), imported(1), {Verb: "imported", Tarball: lock.Images[2].Tarball, Ref: "registry.k8s.io/pause:e2e-wrong-tag"}}, wantImport{Outcome: "success"}, "images.lock has"},
		{"failed line", importSummary{Outcome: "failed", Entries: 3, Imported: 2, Failed: 1, Dir: dir}, []importEntry{imported(0), imported(1), {Verb: "failed", Tarball: lock.Images[2].Tarball, Ref: lock.Images[2].Ref, Reason: "ctr-failed"}}, wantImport{Outcome: "failed"}, "failed"},
		{"wrong dir", importSummary{Outcome: "success", Entries: 3, Imported: 3, Dir: "/opt/provider-kubernetes/images"}, all, wantImport{Outcome: "success"}, "dir="},
		{"entries not the lock's", importSummary{Outcome: "success", Entries: 1, Imported: 3, Dir: dir}, all, wantImport{Outcome: "success"}, "entries="},
		{"verify-only imported", importSummary{Outcome: "verified", Entries: 3, Dir: dir}, all, wantImport{Outcome: "verified", VerifyOnly: true}, "--verify-only"},
		{"refused entry without a line", partial, all[:2], wantImport{Outcome: "partial", Refused: pauseMode}, "no refused line"},
		{"unlisted count", success, all, wantImport{Outcome: "success", Unlisted: 1}, "unlisted="},
	}
	for _, tc := range bad {
		v := strings.Join(importRunViolations(importOutput{Summary: tc.s, Entries: tc.entries}, lock, tc.want), "; ")
		if !strings.Contains(v, tc.mention) {
			t.Errorf("%s: violations %q, want one mentioning %q", tc.name, v, tc.mention)
		}
	}
}

func TestParseImportOutput(t *testing.T) {
	// A whole-bundle refusal as the provider prints it (logrus text, no TTY),
	// captured from `import-images --verify-only` on a host without /system.
	const refusedRun = `time="2026-09-15T08:15:43Z" level=info msg="provider-kubernetes import-images dev: verifyOnly=true"` + "\n" +
		`time="2026-09-15T08:15:43Z" level=error msg="image-import: bundle refused reason=dir-unsafe detail=\"open \\\"system\\\": no such file or directory\""` + "\n" +
		`time="2026-09-15T08:15:43Z" level=info msg="image-import: summary outcome=refused entries=0 imported=0 refused=0 failed=0 unlisted=0 readonly=false dir=/system/provider-kubernetes/images"` + "\n"
	o, err := parseImportOutput(refusedRun)
	want := importOutput{Summary: importSummary{Outcome: "refused", Dir: hostexec.BundleDir}, BundleRefusal: "dir-unsafe"}
	if err != nil || !reflect.DeepEqual(o, want) {
		t.Errorf("bundle refusal: got (%+v, %v), want %+v", o, err, want)
	}

	deadline := `msg="image-import: failed a.tar ref=r/a:1 reason=deadline detail=\"context deadline exceeded before this entry was checked\""` + "\n" +
		`msg="image-import: summary outcome=failed entries=1 imported=0 refused=0 failed=1 unlisted=0 readonly=true dir=/d"` + "\n"
	if o, err := parseImportOutput(deadline); err != nil || len(o.Entries) != 1 || o.Entries[0].Reason != "deadline" || o.BundleRefusal != "" {
		t.Errorf("deadline entry: got (%+v, %v)", o, err)
	}

	const line = `msg="image-import: bundle refused reason=lock-invalid detail=\"x\""` + "\n"
	const summary = `msg="image-import: summary outcome=refused entries=0 imported=0 refused=0 failed=0 unlisted=0 readonly=true dir=/d"` + "\n"
	bad := map[string]string{
		// The contract: the summary is the LAST line. A line logged after it (such as
		// a trailing "done") must fail the parse, not be skipped.
		"line after the summary":   refusedRun + `time="2026-09-15T08:15:43Z" level=info msg="provider-kubernetes import-images: done (outcome=refused)"` + "\n",
		"two bundle refusals":      line + line + summary,
		"unknown bundle reason":    `msg="image-import: bundle refused reason=bogus detail=\"x\""` + "\n" + summary,
		"bundle refusal no reason": `msg="image-import: bundle refused detail=\"x\""` + "\n" + summary,
		"bad entry line":           `msg="image-import: refused a.tar ref=r/a:1 reason=bogus"` + "\n" + summary,
	}
	for name, out := range bad {
		if o, err := parseImportOutput(out); err == nil {
			t.Errorf("%s: got %+v, want an error", name, o)
		}
	}
}

func TestFormatImportEntries(t *testing.T) {
	entries := []importEntry{
		{Verb: "imported", Tarball: "a.tar", Ref: "r/a:1"},
		{Verb: "refused", Tarball: "p.tar", Ref: "r/p:1", Reason: "mode"},
		{Verb: "imported", Tarball: "b.tar", Ref: "r/b:1"},
	}
	if got, want := formatImportEntries(entries, "p.tar"), "[refused p.tar ref=r/p:1 reason=mode] + 2 other entry line(s)"; got != want {
		t.Errorf("formatImportEntries = %q, want %q", got, want)
	}
	if got, want := formatImportEntries(entries, "x.tar"), "[] + 3 other entry line(s)"; got != want {
		t.Errorf("formatImportEntries without the tarball = %q, want %q", got, want)
	}
}

func TestWalkDirs(t *testing.T) {
	got := walkDirs("/system/provider-kubernetes/images", "/system/providers")
	want := []string{"/", "/system", "/system/provider-kubernetes", "/system/provider-kubernetes/images", "/system/providers"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("walkDirs = %v, want %v", got, want)
	}
	if got := walkDirs("/"); !reflect.DeepEqual(got, []string{"/"}) {
		t.Errorf("walkDirs(/) = %v", got)
	}
	if got := walkDirs("/opt/provider-kubernetes/images/"); !reflect.DeepEqual(got, []string{"/", "/opt", "/opt/provider-kubernetes", "/opt/provider-kubernetes/images"}) {
		t.Errorf("walkDirs with a trailing slash = %v", got)
	}
	// The e2e walk is derived from the importer's own constants.
	walk := walkDirs(hostexec.BundleDir, filepath.Dir(hostexec.ProviderBinaryPath))
	if !containsString(walk, hostexec.BundleDir) || !containsString(walk, filepath.Dir(hostexec.ProviderBinaryPath)) || walk[0] != "/" {
		t.Errorf("walk %v does not cover %s and %s from /", walk, hostexec.BundleDir, hostexec.ProviderBinaryPath)
	}
}

func TestBundlePathViolations(t *testing.T) {
	for _, ok := range []struct {
		st       fileStat
		wantType string
	}{
		{fileStat{Type: "directory", Mode: 0o755}, "directory"},
		{fileStat{Type: "directory", GID: 100, Mode: 0o750}, "directory"}, // group not required
		{fileStat{Type: "regular file", Mode: 0o644}, "regular file"},
		{fileStat{Type: "regular file", Mode: 0o755}, "regular file"},
	} {
		if v := bundlePathViolations(ok.st, ok.wantType); len(v) != 0 {
			t.Errorf("%+v as %s: unexpected violations %q", ok.st, ok.wantType, v)
		}
	}
	cases := []struct {
		name     string
		st       fileStat
		wantType string
		want     string
	}{
		{"symlink", fileStat{Type: "symbolic link", Mode: 0o644}, "regular file", "type"},
		{"fifo", fileStat{Type: "fifo", Mode: 0o644}, "regular file", "type"},
		{"file where a directory belongs", fileStat{Type: "regular file", Mode: 0o644}, "directory", "type"},
		{"not root-owned", fileStat{Type: "regular file", UID: 1000, Mode: 0o644}, "regular file", "owner"},
		{"group-writable", fileStat{Type: "regular file", Mode: 0o664}, "regular file", "writable"},
		{"other-writable directory", fileStat{Type: "directory", Mode: 0o1777}, "directory", "writable"},
		{"setuid", fileStat{Type: "regular file", Mode: 0o4644}, "regular file", "setuid"},
		{"setgid directory", fileStat{Type: "directory", Mode: 0o2755}, "directory", "setgid"},
	}
	for _, tc := range cases {
		v := bundlePathViolations(tc.st, tc.wantType)
		if len(v) != 1 || !strings.Contains(v[0], tc.want) {
			t.Errorf("%s (%+v): violations %q, want exactly one mentioning %q", tc.name, tc.st, v, tc.want)
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
