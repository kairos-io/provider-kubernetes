//go:build e2e

package e2e

// Container-free tests for the E-B7 helpers in exec_hardening.go. The e2e checks
// can only fail closed if these predicates and parsers are right, so they are
// pinned here against the exact shapes the real image produces (GNU coreutils
// stat/readlink on Hadron, logrus text output).

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	k8syaml "sigs.k8s.io/yaml"
)

func TestParseFileStat(t *testing.T) {
	good := []struct {
		in   string
		want fileStat
	}{
		{"regular file|0|0|755\n", fileStat{Type: "regular file", Mode: 0o755}},
		{"regular file|0|0|4755", fileStat{Type: "regular file", Mode: 0o4755}},
		{"symbolic link|0|0|777", fileStat{Type: "symbolic link", Mode: 0o777}},
		{"directory|1000|100|1777", fileStat{Type: "directory", UID: 1000, GID: 100, Mode: 0o1777}},
	}
	for _, tc := range good {
		got, err := parseFileStat(tc.in)
		if err != nil {
			t.Errorf("parseFileStat(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseFileStat(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}

	bad := []string{
		"",
		"regular file|0|0",
		"regular file|0|0|755|extra",
		"regular file|root|0|755",
		"regular file|0|-1|755",
		"regular file|0|0|rwxr-xr-x",
		"regular file|0|0|789", // not octal: a decimal parse would accept it
		"regular file|0|0|17777",
	}
	for _, in := range bad {
		if got, err := parseFileStat(in); err == nil {
			t.Errorf("parseFileStat(%q) = %+v, want an error", in, got)
		}
	}
}

func TestExecBinaryViolations(t *testing.T) {
	ok := fileStat{Type: "regular file", Mode: 0o755}
	if v := execBinaryViolations(ok); len(v) != 0 {
		t.Errorf("root-owned 0755 regular file: unexpected violations %q", v)
	}
	if v := execBinaryViolations(fileStat{Type: "regular file", Mode: 0o500}); len(v) != 0 {
		t.Errorf("root-owned 0500 regular file: unexpected violations %q", v)
	}

	// A real symlink (lstat mode 0777) breaks both the type and the write rule.
	if v := execBinaryViolations(fileStat{Type: "symbolic link", Mode: 0o777}); len(v) != 2 {
		t.Errorf("0777 symlink: violations %q, want the type and write violations", v)
	}

	// Each case isolates one predicate, so it must produce exactly one violation.
	cases := []struct {
		name string
		st   fileStat
		want string
	}{
		{"symlink type", fileStat{Type: "symbolic link", Mode: 0o755}, "type"},
		{"empty file", fileStat{Type: "regular empty file", Mode: 0o755}, "type"},
		{"non-root owner", fileStat{Type: "regular file", UID: 1000, Mode: 0o755}, "owner"},
		{"non-root group", fileStat{Type: "regular file", GID: 100, Mode: 0o755}, "owner"},
		{"not owner-executable", fileStat{Type: "regular file", Mode: 0o644}, "owner-executable"},
		{"group-writable", fileStat{Type: "regular file", Mode: 0o775}, "writable"},
		{"other-writable", fileStat{Type: "regular file", Mode: 0o757}, "writable"},
		{"setuid", fileStat{Type: "regular file", Mode: 0o4755}, "setuid"},
		{"setgid", fileStat{Type: "regular file", Mode: 0o2755}, "setgid"},
	}
	for _, tc := range cases {
		v := execBinaryViolations(tc.st)
		if len(v) != 1 || !strings.Contains(v[0], tc.want) {
			t.Errorf("%s (%+v): violations %q, want exactly one mentioning %q", tc.name, tc.st, v, tc.want)
		}
	}
}

func TestSystemDirViolations(t *testing.T) {
	if v := systemDirViolations(fileStat{Type: "directory", Mode: 0o755}); len(v) != 0 {
		t.Errorf("root-owned 0755 directory: unexpected violations %q", v)
	}
	cases := []struct {
		name string
		st   fileStat
		want string
	}{
		{"symlink", fileStat{Type: "symbolic link", Mode: 0o777}, "type"},
		{"group-writable", fileStat{Type: "directory", Mode: 0o775}, "writable"},
		{"world-writable sticky", fileStat{Type: "directory", Mode: 0o1777}, "writable"},
		{"non-root owner", fileStat{Type: "directory", UID: 1000, Mode: 0o755}, "owner"},
	}
	for _, tc := range cases {
		v := systemDirViolations(tc.st)
		if len(v) == 0 || !strings.Contains(strings.Join(v, ";"), tc.want) {
			t.Errorf("%s (%+v): violations %q, want one mentioning %q", tc.name, tc.st, v, tc.want)
		}
	}
}

func TestChildPathViolation(t *testing.T) {
	for _, ok := range []string{"/usr/bin", "/usr/sbin", "/usr", "/usr/bin/"} {
		if v := childPathViolation(ok); v != "" {
			t.Errorf("childPathViolation(%q) = %q, want none", ok, v)
		}
	}
	for _, bad := range []string{
		"/usr/local/bin",
		"/usr/local",
		"/usr/bin/../local/bin", // cleans to /usr/local/bin
		"/bin",                  // a non-merged /usr: outside the image's /usr
		"/usrlocal/bin",         // prefix without a separator is not inside /usr
		"usr/bin",
		"",
	} {
		if v := childPathViolation(bad); v == "" {
			t.Errorf("childPathViolation(%q) = none, want a violation", bad)
		}
	}
}

func TestDockerExecFlags(t *testing.T) {
	got, err := dockerExecFlags(execOptions{Env: []string{"KUBERC=/tmp/x", "A_1=b=c", "EMPTY="}, Workdir: "/"})
	if err != nil {
		t.Fatalf("valid options: unexpected error %v", err)
	}
	want := []string{"-e", "KUBERC=/tmp/x", "-e", "A_1=b=c", "-e", "EMPTY=", "-w", "/"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dockerExecFlags = %q, want %q", got, want)
	}
	if got, err := dockerExecFlags(execOptions{}); err != nil || len(got) != 0 {
		t.Errorf("dockerExecFlags(zero) = %q, %v; want empty, nil", got, err)
	}
	if got, err := dockerExecFlags(execOptions{Workdir: "/tmp"}); err != nil || !reflect.DeepEqual(got, []string{"-w", "/tmp"}) {
		t.Errorf("dockerExecFlags(workdir /tmp) = %q, %v", got, err)
	}

	for _, bad := range []string{"NOEQUALS", "=value", "-e=x", "--privileged", "1ABC=x", "A B=x"} {
		_, err := dockerExecFlags(execOptions{Env: []string{bad}})
		if err == nil {
			t.Errorf("env entry %q: want an error", bad)
			continue
		}
		if strings.Contains(err.Error(), bad) {
			t.Errorf("env entry %q: error %q echoes the entry", bad, err)
		}
	}
	for _, bad := range []string{"relative", "-w", "--privileged", "/tmp/", "/a/../b", "//"} {
		if got, err := dockerExecFlags(execOptions{Workdir: bad}); err == nil {
			t.Errorf("workdir %q: got %q, want an error", bad, got)
		}
	}
}

func TestKubercCommandDefaults(t *testing.T) {
	got, err := kubercCommandDefaults(hostileKuberc, "annotate")
	if err != nil {
		t.Fatalf("hostileKuberc: unexpected error %v", err)
	}
	want := map[string]string{"dry-run": "server", "local": "true"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hostileKuberc annotate defaults = %v, want %v", got, want)
	}
	if got, err := kubercCommandDefaults(hostileKuberc, "get"); err != nil || len(got) != 0 {
		t.Errorf("hostileKuberc get defaults = %v, %v; want none", got, err)
	}

	const head = "apiVersion: kubectl.config.k8s.io/v1beta1\nkind: Preference\n"
	bad := map[string]string{
		"malformed YAML":      malformedKuberc,
		"other apiVersion":    "apiVersion: kubectl.config.k8s.io/v1alpha1\nkind: Preference\ndefaults: []\n",
		"other kind":          "apiVersion: kubectl.config.k8s.io/v1beta1\nkind: Config\ndefaults: []\n",
		"unknown field":       head + "defaults:\n- command: annotate\n  flags:\n  - name: dry-run\n    default: server\n",
		"dashed option name":  head + "defaults:\n- command: annotate\n  options:\n  - name: --dry-run\n    default: server\n",
		"empty option name":   head + "defaults:\n- command: annotate\n  options:\n  - default: server\n",
		"repeated option":     head + "defaults:\n- command: annotate\n  options:\n  - name: local\n    default: \"true\"\n  - name: local\n    default: \"false\"\n",
		"option as a boolean": head + "defaults:\n- command: annotate\n  options:\n  - name: local\n    default: true\n",
	}
	for name, content := range bad {
		if got, err := kubercCommandDefaults(content, "annotate"); err == nil {
			t.Errorf("%s: got %v, want an error", name, got)
		}
	}
}

func TestKubercProbeManifest(t *testing.T) {
	var obj struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := k8syaml.UnmarshalStrict([]byte(kubercProbeManifest), &obj); err != nil {
		t.Fatalf("kubercProbeManifest: %v", err)
	}
	if obj.APIVersion != "v1" || obj.Kind != "Node" || obj.Metadata.Name != kubercProbeNodeName {
		t.Errorf("kubercProbeManifest = %+v, want a v1 Node named %q", obj, kubercProbeNodeName)
	}
	if !strings.HasPrefix(defaultKubercPath, defaultKubercDir+"/") || reconcileWorkdir != "/" {
		t.Errorf("default kuberc %q must sit in %q relative to working directory %q", defaultKubercPath, defaultKubercDir, reconcileWorkdir)
	}
}

func TestShadowShimScript(t *testing.T) {
	if !strings.HasPrefix(shadowShimScript, "#!/bin/sh\n") {
		t.Errorf("shim must start with a #!/bin/sh line")
	}
	if !strings.Contains(shadowShimScript, ">> "+shadowHitsPath+"\n") {
		t.Errorf("shim does not append to shadowHitsPath %q", shadowHitsPath)
	}
	if !strings.HasSuffix(shadowShimScript, "\nexit "+strconv.Itoa(shadowShimExit)+"\n") {
		t.Errorf("shim does not end with exit %d", shadowShimExit)
	}
	for _, tool := range shadowedTools {
		if strings.Contains(shadowShimScript, tool) {
			t.Errorf("shim text mentions %q; the tool name must come from $0 only", tool)
		}
	}
	for _, forbidden := range []string{"containerd-shim-runc-v2", "runc", "mount", "cp"} {
		for _, tool := range shadowedTools {
			if tool == forbidden {
				t.Errorf("%q must not be shadowed (F-UNITPATH is out of scope)", forbidden)
			}
		}
	}
}

func TestParseShadowHits(t *testing.T) {
	if hits := parseShadowHits(""); len(hits) != 0 {
		t.Errorf("empty marker: got %v, want no hits", hits)
	}
	if hits := parseShadowHits("\n  \n"); len(hits) != 0 {
		t.Errorf("blank marker: got %v, want no hits", hits)
	}
	raw := "kubectl argv=[--kubeconfig /etc/kubernetes/admin.conf annotate node n] parent=agent-provider-\n" +
		"\n" +
		"systemctl argv=[restart kubelet] parent=kubeadm\n"
	want := []shadowHit{
		{Tool: "kubectl", Line: "kubectl argv=[--kubeconfig /etc/kubernetes/admin.conf annotate node n] parent=agent-provider-"},
		{Tool: "systemctl", Line: "systemctl argv=[restart kubelet] parent=kubeadm"},
	}
	if got := parseShadowHits(raw); !reflect.DeepEqual(got, want) {
		t.Errorf("parseShadowHits = %+v, want %+v", got, want)
	}
}

func TestMalformedKubercIsNotYAML(t *testing.T) {
	// kubectl only fails hard on a non-strict decode error; prove the fixture is
	// a syntax error for the YAML parser kubectl's decoder is built on.
	if _, err := k8syaml.YAMLToJSON([]byte(malformedKuberc)); err == nil {
		t.Fatalf("malformedKuberc parses as YAML; kubectl would at most warn about it")
	}
}

func TestImportedTarballCount(t *testing.T) {
	const dir = "/opt/provider-kubernetes/images"
	good := []struct {
		name string
		out  string
		want int
	}{
		{
			name: "logrus text without a TTY",
			out: `time="2026-09-14T10:00:00Z" level=info msg="provider-kubernetes import-images dev: importing from /opt/provider-kubernetes/images"` + "\n" +
				`time="2026-09-14T10:00:01Z" level=info msg="image-import: imported registry.k8s.io_etcd_3.6.5-0.tar"` + "\n" +
				`time="2026-09-14T10:00:02Z" level=info msg="image-import: imported 7 tarball(s) from /opt/provider-kubernetes/images"` + "\n" +
				`time="2026-09-14T10:00:02Z" level=info msg="provider-kubernetes import-images: done"` + "\n",
			want: 7,
		},
		{
			name: "logrus text with a TTY",
			out:  "INFO[0002] image-import: imported 12 tarball(s) from /opt/provider-kubernetes/images\n",
			want: 12,
		},
	}
	for _, tc := range good {
		n, gotDir, err := importedTarballCount(tc.out)
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
			continue
		}
		if n != tc.want || gotDir != dir {
			t.Errorf("%s: got (%d, %q), want (%d, %q)", tc.name, n, gotDir, tc.want, dir)
		}
	}

	bad := map[string]string{
		"no summary (nothing bundled)": `level=info msg="image-import: no tarballs under /opt/provider-kubernetes/images; nothing to import"`,
		"only per-tarball lines":       `level=info msg="image-import: imported registry.k8s.io_pause_3.10.2.tar"`,
		"import failed":                `level=error msg="import-images: one or more image imports failed: import a.tar: exit status 97"`,
		"summary repeated": "msg=\"image-import: imported 1 tarball(s) from /a\"\n" +
			"msg=\"image-import: imported 1 tarball(s) from /a\"\n",
	}
	for name, out := range bad {
		if n, d, err := importedTarballCount(out); err == nil {
			t.Errorf("%s: got (%d, %q), want an error", name, n, d)
		}
	}

	if n, _, err := importedTarballCount(`msg="image-import: imported 0 tarball(s) from /x"`); err != nil || n != 0 {
		t.Errorf("a zero count must parse as 0 (the caller rejects it): got %d, %v", n, err)
	}
}

func TestExitCode(t *testing.T) {
	if got := exitCode(nil); got != 0 {
		t.Errorf("exitCode(nil) = %d, want 0", got)
	}
	if got := exitCode(strconv.ErrRange); got != -1 {
		t.Errorf("exitCode(non-exit error) = %d, want -1", got)
	}
}
