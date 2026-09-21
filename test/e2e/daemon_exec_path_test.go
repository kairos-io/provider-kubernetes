//go:build e2e

package e2e

// Pure unit tests for the ADR-19 U1 harness helpers. They need no container, so a
// mistake in the shadow script, the environment rules or the /proc parsing fails
// fast instead of producing a confusing e2e result an hour later.

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/kairos-io/provider-kubernetes/internal/hostexec"
)

func TestU1ShadowShimScript(t *testing.T) {
	if !strings.HasPrefix(u1ShadowShimScript, "#!/bin/sh\n") {
		t.Errorf("shadow must start with a #!/bin/sh line")
	}
	if !strings.Contains(u1ShadowShimScript, ">> "+u1ShadowHitsPath+"\n") {
		t.Errorf("shadow does not append to u1ShadowHitsPath %q", u1ShadowHitsPath)
	}
	if u1ShadowHitsPath == shadowHitsPath {
		t.Errorf("the U1 marker must not be E-B7's %q: the two phases would read each other's hits", shadowHitsPath)
	}
	if !strings.HasSuffix(u1ShadowShimScript, "\nexit "+strconv.Itoa(shadowShimExit)+"\n") {
		t.Errorf("shadow does not end with exit %d", shadowShimExit)
	}
	// The recorded name must come from $0 only: a per-tool text substitution would
	// mean the script differs per file, and a hit could then reflect the harness's
	// own templating rather than what was executed.
	if !strings.Contains(u1ShadowShimScript, `printf '%s argv=[%s] parent=%s\n' "$0"`) {
		t.Errorf("shadow does not record the full $0 as the first field")
	}
	for _, tool := range u1ShadowedTools {
		if strings.Contains(u1ShadowShimScript, tool) {
			t.Errorf("shadow text mentions %q; the name must come from $0 only", tool)
		}
	}
}

func TestU1ShadowPaths(t *testing.T) {
	paths := u1ShadowPaths()
	want := len(u1ShadowDirs)*len(u1ShadowedTools) + 1
	if len(paths) != want {
		t.Fatalf("u1ShadowPaths returned %d paths, want %d", len(paths), want)
	}
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[p] {
			t.Errorf("duplicate shadow path %q: plantU1Shadows would write it twice", p)
		}
		seen[p] = true
		if !strings.HasPrefix(p, "/") || strings.Contains(p, "//") {
			t.Errorf("shadow path %q is not a clean absolute path", p)
		}
	}
	// Every directory a Kairos node keeps on the persistent partition and searches
	// ahead of the image must be covered, plus the shim's working-directory probe.
	for _, p := range []string{
		"/usr/local/bin/containerd-shim-runc-v2",
		"/usr/local/sbin/mount",
		optContainerdBinDir + "/mkfs.erofs",
		rootShimShadow,
	} {
		if !seen[p] {
			t.Errorf("u1ShadowPaths does not cover %q", p)
		}
	}
	// A shadow inside the image's own PATH would break the node instead of
	// detecting a lookup, and would make "zero hits" impossible to reach.
	for _, dir := range strings.Split(hostexec.ChildPATH, ":") {
		for _, p := range paths {
			if strings.HasPrefix(p, dir+"/") {
				t.Errorf("shadow %q sits inside ChildPATH entry %q", p, dir)
			}
		}
	}
}

func TestEnvironPATHViolations(t *testing.T) {
	for _, tc := range []struct {
		name           string
		env            map[string]string
		allowNRISuffix bool
		want           int
	}{
		{"exact", map[string]string{"PATH": hostexec.ChildPATH, "HOME": "/root"}, false, 0},
		{"missing PATH", map[string]string{"HOME": "/root"}, false, 1},
		{"opt prepended", map[string]string{"PATH": "/opt/containerd/bin:" + hostexec.ChildPATH}, false, 1},
		{"usr local first", map[string]string{"PATH": "/usr/local/bin:" + hostexec.ChildPATH}, false, 2},
		{"usr local last", map[string]string{"PATH": hostexec.ChildPATH + ":/usr/local/bin"}, true, 2},
		{"ld library path", map[string]string{"PATH": hostexec.ChildPATH, "LD_LIBRARY_PATH": "/opt/containerd/lib"}, false, 1},
		{"empty ld library path still set", map[string]string{"PATH": hostexec.ChildPATH, "LD_LIBRARY_PATH": ""}, false, 1},
		{"both", map[string]string{"PATH": "/opt/containerd/bin:" + hostexec.ChildPATH, "LD_LIBRARY_PATH": "/opt/containerd/lib"}, false, 2},
		// The NRI v0.1 variant: accepted only where it is expected, only as the
		// whole string, and never when something also moved to the front.
		{"nri variant allowed", map[string]string{"PATH": execPathWithNRIV010}, true, 0},
		{"nri variant not allowed here", map[string]string{"PATH": execPathWithNRIV010}, false, 1},
		{"nri prefix never", map[string]string{"PATH": "/opt/nri/bin:" + execPathExact}, true, 1},
		{"nri variant plus more", map[string]string{"PATH": execPathWithNRIV010 + ":/opt/x"}, true, 1},
		// A prefix or "contains" match would accept these; string equality must not.
		{"trailing colon", map[string]string{"PATH": execPathExact + ":"}, true, 1},
		{"extra dir appended", map[string]string{"PATH": execPathExact + ":/opt/anything"}, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := environPATHViolations(tc.env, tc.allowNRISuffix); len(got) != tc.want {
				t.Errorf("got %d violation(s) %q, want %d", len(got), got, tc.want)
			}
		})
	}
}

// TestExecPathConstants pins the shape of the two accepted PATH values: the exact
// one is hostexec.ChildPATH, and the tolerated variant is that value plus one
// APPENDED element. An appended element cannot shadow a name the image provides,
// which is the whole reason it is tolerable; a prepended one could.
func TestExecPathConstants(t *testing.T) {
	if execPathExact != hostexec.ChildPATH {
		t.Errorf("execPathExact = %q, want hostexec.ChildPATH %q", execPathExact, hostexec.ChildPATH)
	}
	extra, ok := strings.CutPrefix(execPathWithNRIV010, execPathExact+":")
	if !ok {
		t.Fatalf("execPathWithNRIV010 %q does not start with execPathExact plus a separator, so the appended element is not appended", execPathWithNRIV010)
	}
	if strings.Contains(extra, ":") {
		t.Errorf("execPathWithNRIV010 appends %q, want exactly one directory", extra)
	}
	if extra != "/opt/nri/bin" {
		t.Errorf("execPathWithNRIV010 appends %q, want the NRI v0.1 DefaultBinaryPath /opt/nri/bin", extra)
	}
}

func TestDropInViolations(t *testing.T) {
	good := fileStat{Type: "regular file", UID: 0, GID: 0, Mode: 0o644}
	// The kubelet drop-in's shape: one directive.
	wantOne := []string{"Environment=PATH=" + execPathExact}
	body := "# a comment\n[Service]\nEnvironment=PATH=" + execPathExact + "\n"
	// The containerd drop-in's shape: three, in order.
	wantThree := []string{
		"Environment=PATH=" + execPathExact,
		"Environment=CONTAINERD_DISABLE_IGZIP=1",
		"Environment=CONTAINERD_DISABLE_PIGZ=1",
	}
	bodyThree := "[Service]\n" + strings.Join(wantThree, "\n") + "\n"
	for _, tc := range []struct {
		name    string
		st      fileStat
		content string
		want    []string
		n       int
	}{
		{"as shipped", good, body, wantOne, 0},
		{"no trailing newline", good, strings.TrimSuffix(body, "\n"), wantOne, 0},
		{"semicolon comment", good, ";c\n[Service]\nEnvironment=PATH=" + execPathExact, wantOne, 0},
		{"symlink", fileStat{Type: "symbolic link", Mode: 0o777}, body, wantOne, 2},
		{"group writable", fileStat{Type: "regular file", Mode: 0o664}, body, wantOne, 1},
		{"foreign owner", fileStat{Type: "regular file", UID: 1001, Mode: 0o644}, body, wantOne, 1},
		{"wrong path value", good, "[Service]\nEnvironment=PATH=/usr/local/bin:/usr/bin\n", wantOne, 1},
		{"extra directive", good, body + "EnvironmentFile=-/etc/default/containerd\n", wantOne, 1},
		{"empty", good, "", wantOne, 1},
		{"containerd as shipped", good, bodyThree, wantThree, 0},
		// The two S19-18a variables are load-bearing, not decoration: dropping
		// either one reopens a by-name lookup for a binary the image does not ship.
		{"containerd without CONTAINERD_DISABLE_PIGZ", good,
			"[Service]\n" + strings.Join(wantThree[:2], "\n") + "\n", wantThree, 1},
		{"containerd directives reordered", good,
			"[Service]\n" + wantThree[0] + "\n" + wantThree[2] + "\n" + wantThree[1] + "\n", wantThree, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dropInViolations(tc.st, tc.content, tc.want); len(got) != tc.n {
				t.Errorf("got %d violation(s) %q, want %d", len(got), got, tc.n)
			}
		})
	}
}

// TestExecPathDropInsCoverBothUnits keeps the harness's expected drop-in table in
// step with the paths it asserts, so a new drop-in cannot be shipped without the
// e2e checking its contents.
func TestExecPathDropInsCoverBothUnits(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range execPathDropIns {
		seen[d.Path] = true
		if len(d.Directives) == 0 {
			t.Errorf("%s has no expected directives", d.Path)
		}
		if d.Directives[0] != "Environment=PATH="+execPathExact {
			t.Errorf("%s: first expected directive is %q, want the PATH one", d.Path, d.Directives[0])
		}
		if !strings.HasPrefix(d.Path, "/usr/lib/systemd/system/") {
			t.Errorf("%s is not under /usr/lib/systemd/system, so an upgrade would not replace it", d.Path)
		}
	}
	for _, p := range []string{containerdExecPathDropIn, kubeletExecPathDropIn} {
		if !seen[p] {
			t.Errorf("execPathDropIns does not cover %s", p)
		}
	}
}

func TestPIDRE(t *testing.T) {
	for _, ok := range []string{"1", "42", "999999"} {
		if !pidRE.MatchString(ok) {
			t.Errorf("pidRE rejects %q", ok)
		}
	}
	// "0" is systemd's MainPID for a unit that is not running, and the rest would
	// turn a /proc path into a different path.
	for _, bad := range []string{"", "0", "01", "-1", "1 ", "1/../2", "12a"} {
		if pidRE.MatchString(bad) {
			t.Errorf("pidRE accepts %q", bad)
		}
	}
}

func TestParseU1ShadowHitsRecordsTheShadowPath(t *testing.T) {
	raw := "/usr/local/sbin/mount argv=[-t tmpfs tmpfs /var/lib/kubelet/pods/x] parent=kubelet\n" +
		"/opt/containerd/bin/mkfs.erofs argv=[--help] parent=containerd\n"
	want := []shadowHit{
		{Tool: "/usr/local/sbin/mount", Line: "/usr/local/sbin/mount argv=[-t tmpfs tmpfs /var/lib/kubelet/pods/x] parent=kubelet"},
		{Tool: "/opt/containerd/bin/mkfs.erofs", Line: "/opt/containerd/bin/mkfs.erofs argv=[--help] parent=containerd"},
	}
	if got := parseShadowHits(raw); !reflect.DeepEqual(got, want) {
		t.Errorf("parseShadowHits = %+v, want %+v", got, want)
	}
}
