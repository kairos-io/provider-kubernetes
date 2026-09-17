//go:build e2e

package e2e

// daemon_exec_path.go is the harness side of ADR-19 U1 (F-UNITPATH): containerd,
// the kubelet, and everything they start must resolve names against the read-only
// image, never against a Kairos-persistent directory. It is the daemon-side twin
// of exec_hardening.go, which covers the provider's own execs (ADR-1-A1).
//
//	U1-a  the settings are live on the node: the image-only drop-ins are the
//	      root:root 0644 files we shipped, directive for directive; systemd reports
//	      them for both units; containerd, the kubelet and a RUNNING
//	      containerd-shim-runc-v2 have a PATH equal, by STRING EQUALITY, to one of
//	      two named constants and no LD_LIBRARY_PATH (the shim is what proves
//	      containerd's OWN environment, because the `opt` plugin rewrote it with
//	      os.Setenv, which /proc/<containerd>/environ never shows); containerd also
//	      carries the two CONTAINERD_DISABLE_* variables; the shim really is
//	      /usr/bin/containerd-shim-runc-v2; and CRI reports the absolute
//	      runtime_path and BinaryName.
//	U1-b  shadow binaries in every directory a lookup could reach -- /usr/local/bin
//	      and /usr/local/sbin (persistent, ahead of /usr/bin in systemd's default
//	      PATH), /opt/containerd/bin (persistent, what the `opt` plugin prepends to
//	      containerd's own PATH) and the filesystem root (the shim's
//	      working-directory probe) -- then containerd, the import unit and the
//	      kubelet are restarted and must record zero hits.
//
// The positive control matters as much as the zero-hit assertion: it first proves,
// through a transient systemd unit running with the MANAGER's default PATH, that
// these shadows really are found when nothing stops them. Without it "no hits"
// could mean "nothing was ever looked up".

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kairos-io/provider-kubernetes/internal/hostexec"
)

// --- shipped U1 artifacts --------------------------------------------------

const (
	// The image-only drop-ins (ADR-19 U1 decision 1). Under /usr/lib, so they are
	// replaced by every A/B upgrade and apply to whichever unit fragment wins,
	// including the persistent /etc copy every pre-U2 node boots from.
	containerdExecPathDropIn = "/usr/lib/systemd/system/containerd.service.d/50-provider-kubernetes-exec-path.conf"
	kubeletExecPathDropIn    = "/usr/lib/systemd/system/kubelet.service.d/50-provider-kubernetes-exec-path.conf"

	// containerdShimPath is the shim the CRI runc runtime is pinned to
	// (runtime_path), and runcPath is its BinaryName.
	containerdShimPath = "/usr/bin/containerd-shim-runc-v2"
	runcPath           = "/usr/bin/runc"

	// optContainerdBinDir is the directory containerd's `opt` internal plugin
	// creates and prepends to the daemon's own PATH unless it is disabled. It is
	// under the Kairos-persistent /opt, so it must never be searched.
	optContainerdBinDir = "/opt/containerd/bin"
	// optContainerdDir is its parent: on a U1 image nothing creates it, which is
	// the simplest runtime evidence that the plugin never initialized.
	optContainerdDir = "/opt/containerd"

	// rootShimShadow is the path containerd's working-directory probe would reach
	// for a unit with no WorkingDirectory ("./containerd-shim-runc-v2" from /).
	rootShimShadow = "/containerd-shim-runc-v2"

	containerdUnit = "containerd.service"
	kubeletUnit    = "kubelet.service"
)

// u1ShadowDirs are the persistent directories a lookup could reach ahead of the
// image. Every name in u1ShadowedTools is planted in each of them.
//
// Which of them systemd's default PATH actually lists is build-dependent and is
// read from the node rather than assumed (managerDefaultPATH): on a merged-/usr
// base such as Hadron it is /usr/local/bin:/usr/bin, with no sbin entries, so the
// /usr/local/sbin copies are there for a split-bin base and as a regression guard
// on the PATH value itself. /opt/containerd/bin is never on a PATH at all: it is
// only searched because containerd's `opt` plugin prepends it to the daemon's own.
var u1ShadowDirs = []string{"/usr/local/sbin", "/usr/local/bin", optContainerdBinDir}

// u1ShadowedTools is every binary containerd, the kubelet or their children look up
// by NAME, from the ADR-19 inventory and the security review. Some of these lookups
// no longer happen at all on a U1 image, which is the point: they stay in the set as
// a regression net, so that a setting silently reverting, or a containerd bump
// adding a lookup back, is caught rather than assumed away.
//   - containerd-shim-runc-v2, runc: the shim and the runtime the shim starts. Both
//     are taken by absolute path today (runtime_path, BinaryName), so a hit means
//     one of those two settings stopped being honored;
//   - mkfs.erofs: probed by the erofs differ during ITS OWN plugin init, while
//     containerd's PATH is still ChildPATH, and by the erofs snapshotter. Both
//     plugins are disabled in containerd/config.toml (S19-18b), so no lookup should
//     remain;
//   - igzip, unpigz: containerd's optional gzip decompressors, looked up once per
//     daemon on the first layer decompression. Closed by the drop-in's
//     CONTAINERD_DISABLE_IGZIP/PIGZ (S19-18a), which make containerd skip the
//     lookup before it reaches PATH;
//   - mount, umount, systemd-run: the kubelet's mount-utils, used for every secret
//     and projected service-account-token volume. These DO still happen on every
//     kubelet start, and the image provides all three, so they are what actually
//     exercises the kubelet drop-in here;
//   - iptables, ip6tables, iptables-save, iptables-restore: the kubelet's startup
//     rules and its periodic canary, also live;
//   - losetup, blkid: the kubelet's block-volume and filesystem helpers;
//   - ip: reached by CNI reference plugins, which inherit containerd's environment.
//
// scripts/verify-image-files.sh names the same set, split into the names the image
// must provide and the ones closed at the source; TestShadowedToolsMatchTheImageGate
// keeps the two lists from drifting.
var u1ShadowedTools = []string{
	"containerd-shim-runc-v2",
	"runc",
	"mkfs.erofs",
	"igzip",
	"unpigz",
	"mount",
	"umount",
	"systemd-run",
	"iptables",
	"ip6tables",
	"iptables-save",
	"iptables-restore",
	"losetup",
	"blkid",
	"ip",
}

const (
	// u1ShadowHitsPath is the U1 marker. It is separate from E-B7's so the two
	// phases never read each other's hits; u1ShadowShimScript hard-codes the same
	// path (a unit test keeps the two in sync).
	u1ShadowHitsPath = "/tmp/e2e-u1-shadow-hits"
	// u1ProbeArg is the argv marker the positive control passes.
	u1ProbeArg = "e2e-u1-shadow-probe"
)

// u1ShadowShimScript is the fixed content of every U1 shim. Unlike E-B7's it
// records the FULL $0, because the same name is planted in several directories and
// the finding is only actionable if it names the directory that was searched. It
// uses shell builtins only, so a shim never looks anything up in PATH itself, and
// it exits shadowShimExit so a by-name exec fails loudly as well as being recorded.
const u1ShadowShimScript = `#!/bin/sh
# provider-kubernetes e2e shadow binary (ADR-19 U1). Planted where a persistent
# directory would be searched before the image: anything that reaches this file
# resolved a name through a path that survives an image upgrade.
parent=unknown
read -r parent < "/proc/$PPID/comm" || parent=unknown
printf '%s argv=[%s] parent=%s\n' "$0" "$*" "$parent" >> /tmp/e2e-u1-shadow-hits
exit 97
`

// --- /proc and systemd readers ---------------------------------------------

// pidRE is the shape of a PID the harness will build a /proc path from, so a
// surprising systemctl value can never turn into a different path.
var pidRE = regexp.MustCompile(`^[1-9][0-9]*$`)

// procEnviron parses /proc/<pid>/environ (NUL-separated KEY=VALUE) into a map.
// Values are never logged: a daemon environment can hold proxy credentials.
func (nc *nodeContainer) procEnviron(pid string) (map[string]string, error) {
	if !pidRE.MatchString(pid) {
		return nil, fmt.Errorf("%q is not a PID", pid)
	}
	raw, err := nc.execErr(binCat, "/proc/"+pid+"/environ")
	if err != nil {
		return nil, fmt.Errorf("read /proc/%s/environ: %v", pid, err)
	}
	env := map[string]string{}
	for _, kv := range strings.Split(raw, "\x00") {
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		env[k] = v
	}
	return env, nil
}

// unitProps runs `systemctl show` for one unit and parses the properties.
func (nc *nodeContainer) unitProps(unit string, props ...string) (map[string]string, error) {
	out, err := nc.execErr(hostexec.SystemctlPath, "show", "-p", strings.Join(props, ","), unit)
	if err != nil {
		return nil, fmt.Errorf("systemctl show %s: %v: %s", unit, err, strings.TrimSpace(out))
	}
	return parseSystemdShow(out)
}

// shimPIDs returns the PIDs whose /proc/<pid>/exe points exactly at
// containerdShimPath. `find -lname` compares the symlink target itself, so this is
// both the enumeration and the proof that the running shim is the image's.
func (nc *nodeContainer) shimPIDs() []string {
	// find walks /proc while processes come and go, so a non-zero exit with usable
	// stdout is normal; stdout is read separately from stderr for that reason.
	stdout, _, _ := nc.ExecStdoutTimeout(dockerTimeout,
		binFind, "/proc", "-maxdepth", "2", "-name", "exe", "-lname", containerdShimPath, "-print")
	var pids []string
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "/proc/")
		if !ok {
			continue
		}
		pid, _, ok := strings.Cut(rest, "/")
		if ok && pidRE.MatchString(pid) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// --- U1-a: the settings are live -------------------------------------------

// The only two PATH values a process under U1 may have. Every check below is a
// STRING EQUALITY against one of these named constants: no prefix match, no regex,
// no "contains". A prefix match would accept a prepended persistent directory, which
// is exactly the failure U1 exists to prevent (ADR-19 S19-3 as revised).
const (
	// execPathExact is what the drop-ins set and what every process must have
	// unless it is a child containerd started after the append below.
	execPathExact = hostexec.ChildPATH

	// execPathWithNRIV010 is the ONE tolerated variant, and it is spelled out here
	// so it can never pass unnoticed.
	//
	// containerd v2.3.5's CRI pod-sandbox service calls the deprecated NRI v0.1
	// client on every sandbox create and delete
	// (internal/cri/server/podsandbox/sandbox_run.go:274-290, opts.go). That client
	// runs, once per daemon and guarded by a sync.Once,
	// os.Setenv("PATH", os.Getenv("PATH")+":"+DefaultBinaryPath) with
	// DefaultBinaryPath = "/opt/nri/bin"
	// (vendor/github.com/containerd/nri/client.go:42-57 at v2.3.5), and it does so
	// with no v0.1 plugin configured, because a missing /etc/nri/conf.json returns
	// an empty config rather than an error (:191-201). Nothing disables it: the
	// io.containerd.nri.v1.nri keys govern the modern NRI adaptation, not this
	// client, which carries its own constant.
	//
	// So the APPEND happens at the FIRST SANDBOX CREATE, not at daemon start: the
	// shim of that first sandbox is started before it and has execPathExact, later
	// ones have this value. Both forms are legitimate, and nothing else is.
	//
	// Being appended, it can never shadow a name the image provides - only supply
	// one the image lacks. R-19-13. If a containerd bump moves, renames or drops the
	// append, this constant stops matching and the gate fails, which is the point.
	execPathWithNRIV010 = hostexec.ChildPATH + ":/opt/nri/bin"
)

// containerdDropInEnv are the other variables the containerd drop-in sets, which
// must therefore be in the daemon's environment. They close containerd's two
// unconditional lookups of names the image does not ship (ADR-19 S19-18a): with
// either set, containerd skips the lookup entirely and uses its built-in Go gzip,
// so the appended /opt/nri/bin can never answer for igzip or unpigz.
var containerdDropInEnv = map[string]string{
	"CONTAINERD_DISABLE_IGZIP": "1",
	"CONTAINERD_DISABLE_PIGZ":  "1",
}

// environPATHViolations lists how one process's PATH and LD_LIBRARY_PATH differ
// from what U1 promises. The LD_LIBRARY_PATH half is not cosmetic: containerd's
// `opt` plugin sets it to the persistent /opt/containerd/lib, and every dynamically
// linked helper a child runs would resolve its libraries there first, so its absence
// is what proves that plugin is off.
//
// allowNRIV010 widens the accepted set from {execPathExact} to
// {execPathExact, execPathWithNRIV010}, and only for processes containerd started.
// containerd's own /proc environ is exec-time and must always be execPathExact.
func environPATHViolations(env map[string]string, allowNRIV010 bool) []string {
	var v []string
	got := env["PATH"]
	switch {
	case got == execPathExact:
	case allowNRIV010 && got == execPathWithNRIV010:
		// Known and reported by the caller; see execPathWithNRIV010.
	case allowNRIV010:
		v = append(v, fmt.Sprintf("PATH=%q, want exactly %q or %q", got, execPathExact, execPathWithNRIV010))
	default:
		v = append(v, fmt.Sprintf("PATH=%q, want exactly %q", got, execPathExact))
	}
	// Belt and braces, and a clearer message when the value does differ: no entry
	// may come from the persistent /usr/local, whatever the rest of the value is.
	for _, dir := range strings.Split(got, ":") {
		if dir == "/usr/local" || strings.HasPrefix(dir, "/usr/local/") {
			v = append(v, fmt.Sprintf("PATH entry %q is under the persistent /usr/local", dir))
		}
	}
	if got, ok := env["LD_LIBRARY_PATH"]; ok {
		v = append(v, fmt.Sprintf("LD_LIBRARY_PATH is set (%q); nothing in the image should add one", got))
	}
	return v
}

// execPathDropIns are the two shipped drop-ins and the exact directive list each
// one must carry on the booted node, in file order. image_exec_path_test.go asserts
// the same lists against the build context; this repeats them against the image as
// it actually booted, where an overlay or a sysext could have replaced the file.
var execPathDropIns = []struct {
	Unit, Path string
	Directives []string
}{
	{containerdUnit, containerdExecPathDropIn, []string{
		"Environment=PATH=" + execPathExact,
		"Environment=CONTAINERD_DISABLE_IGZIP=1",
		"Environment=CONTAINERD_DISABLE_PIGZ=1",
	}},
	{kubeletUnit, kubeletExecPathDropIn, []string{
		"Environment=PATH=" + execPathExact,
	}},
}

// dropInViolations lists how a shipped drop-in differs from the file U1 defines: a
// root:root 0644 regular file carrying exactly want, in order. "Exactly" matters as
// much as the values: an extra EnvironmentFile= would outrank the PATH this file is
// here to set, and a missing CONTAINERD_DISABLE_* reopens a lookup for a name the
// image does not ship.
func dropInViolations(st fileStat, content string, want []string) []string {
	v := []string{}
	if st.Type != "regular file" {
		v = append(v, fmt.Sprintf("type %q, want %q (not following symlinks)", st.Type, "regular file"))
	}
	if st.UID != 0 || st.GID != 0 {
		v = append(v, fmt.Sprintf("owner %d:%d, want 0:0", st.UID, st.GID))
	}
	if st.Mode != 0o644 {
		v = append(v, fmt.Sprintf("mode %04o, want 0644", st.Mode))
	}
	var directives []string
	for _, line := range strings.Split(content, "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") || strings.HasPrefix(s, ";") || strings.HasPrefix(s, "[") {
			continue
		}
		directives = append(directives, s)
	}
	if !slices.Equal(directives, want) {
		v = append(v, fmt.Sprintf("sets %d directive(s) %q, want exactly %q", len(directives), directives, want))
	}
	return v
}

// criRuntimeConfig is the part of `crictl info` that names how the runc runtime is
// started. crictl prints the CRI plugin's own config, so this reads back what
// containerd resolved, not what the file says.
type criRuntimeConfig struct {
	Config struct {
		Containerd struct {
			Runtimes map[string]struct {
				RuntimePath string `json:"runtimePath"`
				Options     struct {
					BinaryName string `json:"BinaryName"`
				} `json:"options"`
			} `json:"runtimes"`
		} `json:"containerd"`
	} `json:"config"`
}

// assertDaemonExecPathLive is U1-a.
func assertDaemonExecPathLive(t *testing.T, nc *nodeContainer) {
	t.Helper()
	violations := 0
	report := func(subject, msg string) {
		t.Helper()
		violations++
		t.Errorf("U1-a %s: %s", subject, msg)
	}

	// 1. The drop-ins on the booted node are the files we shipped, directive for
	//    directive.
	for _, d := range execPathDropIns {
		st, err := nc.lstat(d.Path)
		if err != nil {
			report(d.Path, err.Error())
			continue
		}
		content, err := nc.ReadFile(d.Path)
		if err != nil {
			report(d.Path, fmt.Sprintf("read: %v", err))
			continue
		}
		for _, v := range dropInViolations(st, content, d.Directives) {
			report(d.Path, v)
		}
		t.Logf("U1-a %s: %q %d:%d %04o, %q", d.Path, st.Type, st.UID, st.GID, st.Mode, d.Directives)
	}

	// 2. systemd applies them, and reports the unit fragment they apply to. On a
	//    pre-U2 image the fragment is the persistent /etc copy, which is exactly the
	//    state every upgraded node is in: proving the /usr/lib drop-in still lands
	//    there is the point of putting it under /usr/lib (S19-4).
	for _, d := range execPathDropIns {
		props, err := nc.unitProps(d.Unit, "Environment", "DropInPaths", "FragmentPath")
		if err != nil {
			report(d.Unit, err.Error())
			continue
		}
		// systemd merges Environment= from every drop-in into one space-separated
		// property, so each directive is checked for its own presence.
		for _, want := range d.Directives {
			kv := strings.TrimPrefix(want, "Environment=")
			if got := props["Environment"]; !strings.Contains(got, kv) {
				report(d.Unit, fmt.Sprintf("Environment=%q does not contain %q", got, kv))
			}
		}
		if got := props["DropInPaths"]; !strings.Contains(got, d.Path) {
			report(d.Unit, fmt.Sprintf("DropInPaths=%q does not include %q", got, d.Path))
		}
		t.Logf("U1-a %s: FragmentPath=%s DropInPaths=%s Environment=%s",
			d.Unit, props["FragmentPath"], props["DropInPaths"], props["Environment"])
	}

	// 3. The running daemons have it. /proc/<pid>/environ is the exec-time
	//    environment, so this is what every child they fork inherits.
	for _, unit := range []string{containerdUnit, kubeletUnit} {
		props, err := nc.unitProps(unit, "MainPID")
		if err != nil {
			report(unit, err.Error())
			continue
		}
		pid := props["MainPID"]
		if !pidRE.MatchString(pid) {
			report(unit, fmt.Sprintf("MainPID=%q: the unit is not running, so its environment cannot be checked", pid))
			continue
		}
		env, err := nc.procEnviron(pid)
		if err != nil {
			report(unit, err.Error())
			continue
		}
		// The daemons' own environments are exec-time and untouched by any Setenv a
		// daemon does later, so they must equal execPathExact.
		for _, v := range environPATHViolations(env, false) {
			report(unit+" (pid "+pid+")", v)
		}
		if unit == containerdUnit {
			// The S19-18a variables reach the daemon only if the drop-in was applied
			// AND systemd kept it; checking the file alone would not prove that.
			for k, want := range containerdDropInEnv {
				if got, ok := env[k]; !ok || got != want {
					report(unit+" (pid "+pid+")", fmt.Sprintf("%s=%q (present=%t), want %q; without it containerd looks up a decompressor by name and the image ships none", k, got, ok, want))
				}
			}
		}
		t.Logf("U1-a %s pid %s: PATH is exactly %q, LD_LIBRARY_PATH unset", unit, pid, env["PATH"])
	}

	// 4. A RUNNING shim. This is the only view of containerd's OWN post-Setenv
	//    environment (the `opt` plugin's rewrite never shows in
	//    /proc/<containerd>/environ), and it covers CNI plugins too, which get
	//    os.Environ() plus CNI_*.
	//    A PID enumerated from /proc can exit before its environ is read, so a read
	//    failure skips that PID; at least one shim must still be checked, otherwise
	//    the claim rests on nothing and that is reported instead.
	exact := make([]string, 0, 8)
	withNRIV010 := make([]string, 0, 8)
	for _, pid := range nc.shimPIDs() {
		env, err := nc.procEnviron(pid)
		if err != nil {
			t.Logf("U1-a: shim pid %s vanished before its environment could be read (%v); skipping it", pid, err)
			continue
		}
		switch env["PATH"] {
		case execPathExact:
			exact = append(exact, pid)
		case execPathWithNRIV010:
			withNRIV010 = append(withNRIV010, pid)
		}
		for _, v := range environPATHViolations(env, true) {
			report("containerd-shim-runc-v2 (pid "+pid+")", v)
		}
	}
	switch {
	case len(exact)+len(withNRIV010) == 0:
		report("containerd-shim-runc-v2", "no running shim whose /proc/<pid>/exe is "+containerdShimPath+
			" could be read with a recognized PATH; without one, containerd's own PATH (which os.Setenv can change invisibly) is unproven")
	default:
		t.Logf("U1-a %d running shim(s): /proc/<pid>/exe is %s; PATH is exactly %q for pids %v, LD_LIBRARY_PATH unset",
			len(exact)+len(withNRIV010), containerdShimPath, execPathExact, exact)
		if len(withNRIV010) > 0 {
			t.Logf("U1-a RESIDUAL R-19-13: pids %v have exactly %q instead. containerd's deprecated NRI v0.1 client appends that element to the daemon's own PATH at the first sandbox create and no configuration key disables it (vendor/github.com/containerd/nri/client.go:42-57 at v2.3.5). Being appended it cannot shadow a name the image ships; the names it could supply are closed by the drop-in's CONTAINERD_DISABLE_* and by disabling both erofs plugins.",
				withNRIV010, execPathWithNRIV010)
		}
	}

	// 5. CRI reports the absolute shim and runtime binary, so the shim is taken
	//    by path instead of being searched for (and runc is not searched either).
	stdout, stderr, err := nc.ExecStdoutTimeout(dockerTimeout, binCrictl, "--runtime-endpoint", criEndpoint, "info")
	switch {
	case err != nil:
		report("crictl info", fmt.Sprintf("%v: %s", err, strings.TrimSpace(stderr)))
	default:
		var info criRuntimeConfig
		if err := json.Unmarshal([]byte(stdout), &info); err != nil {
			report("crictl info", fmt.Sprintf("parse json: %v", err))
			break
		}
		runc, ok := info.Config.Containerd.Runtimes["runc"]
		switch {
		case !ok:
			report("crictl info", "no runc runtime in config.containerd.runtimes")
		default:
			if runc.RuntimePath != containerdShimPath {
				report("crictl info", fmt.Sprintf("runc runtimePath = %q, want %q", runc.RuntimePath, containerdShimPath))
			}
			if runc.Options.BinaryName != runcPath {
				report("crictl info", fmt.Sprintf("runc options.BinaryName = %q, want %q", runc.Options.BinaryName, runcPath))
			}
			t.Logf("U1-a crictl info: runc runtimePath=%q BinaryName=%q", runc.RuntimePath, runc.Options.BinaryName)
		}
	}

	if violations > 0 {
		t.Fatalf("U1-a: %d violation(s); the daemons do not run with the image-only PATH, so the U1-b shadow check below would be meaningless", violations)
	}
	t.Logf("U1-a: containerd and the kubelet have PATH exactly %q; every running shim has exactly that or exactly %q, and nothing else",
		execPathExact, execPathWithNRIV010)
}

// assertOptPluginNeverInitialized proves, from the filesystem, that containerd's
// `opt` internal plugin did not run: its InitFn creates <path>/bin and <path>/lib
// (mode 0711) and only then rewrites the daemon's PATH and LD_LIBRARY_PATH. The
// absence of a directory containerd would otherwise have created is the one check
// that does not depend on reading an environment.
//
// dir narrows across the run, because U1-b itself plants shadow binaries in
// optContainerdBinDir: before that happens the whole of optContainerdDir must be
// absent, and afterwards only the lib/ that the plugin alone would create can still
// be asserted.
func assertOptPluginNeverInitialized(t *testing.T, nc *nodeContainer, phase, dir string) {
	t.Helper()
	if out, err := nc.execErr(binTest, "-e", dir); err == nil {
		t.Errorf("U1-a (%s): %s exists; containerd's `opt` internal plugin creates it at startup, so the plugin is still enabled and the daemon's PATH and LD_LIBRARY_PATH were rewritten to search %s first\n%s",
			phase, dir, optContainerdBinDir, out)
		return
	}
	t.Logf("U1-a (%s): %s does not exist, so the `opt` plugin did not initialize and containerd's PATH and LD_LIBRARY_PATH were not rewritten", phase, dir)
}

// --- U1-b: shadow binaries and the positive control ------------------------

// u1ShadowPaths lists every file plantU1Shadows creates, in creation order.
func u1ShadowPaths() []string {
	paths := make([]string, 0, len(u1ShadowDirs)*len(u1ShadowedTools)+1)
	for _, dir := range u1ShadowDirs {
		for _, tool := range u1ShadowedTools {
			paths = append(paths, dir+"/"+tool)
		}
	}
	return append(paths, rootShimShadow)
}

// managerDefaultPATH returns the PATH a service started by the systemd MANAGER
// gets, read from a transient unit rather than assumed: the exact value is
// build-dependent (systemd's split-bin configuration), and everything U1-b claims
// rests on it listing /usr/local before /usr/bin.
func managerDefaultPATH(t *testing.T, nc *nodeContainer) string {
	t.Helper()
	out, err := nc.ExecTimeout(dockerTimeout, binSystemdRun, "--wait", "--pipe", "--quiet", "--collect", binEnv)
	if err != nil {
		t.Fatalf("U1-b: could not read the manager's default PATH via systemd-run: %v\n%s", err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "PATH="); ok {
			return v
		}
	}
	t.Fatalf("U1-b: a transient systemd unit reported no PATH:\n%s", out)
	return ""
}

// plantU1Shadows is U1-b's setup. It refuses to overwrite anything the image
// already ships at those paths (that would hide a real shadowing binary instead of
// reporting it), plants a shim for every name in every directory, and removes them
// all in t.Cleanup.
func plantU1Shadows(t *testing.T, nc *nodeContainer) {
	t.Helper()
	paths := u1ShadowPaths()

	t.Cleanup(func() {
		if t.Failed() {
			if raw, err := nc.ReadFile(u1ShadowHitsPath); err == nil && strings.TrimSpace(raw) != "" {
				t.Logf("=== U1 shadow hits (%s) ===\n%s", u1ShadowHitsPath, raw)
			}
		}
		rm := append([]string{binRm, "-f", u1ShadowHitsPath}, paths...)
		if out, err := nc.execErr(rm...); err != nil {
			t.Logf("remove U1 shadow binaries (best effort; the container is removed next): %v\n%s", err, out)
		}
	})

	for _, p := range paths {
		if _, err := nc.execErr(binTest, "-e", p); err == nil {
			t.Fatalf("U1-b: the image already ships %s, which shadows the image copy for any by-name lookup that reaches that directory", p)
		}
	}
	nc.WriteFile(t, u1ShadowHitsPath, "", "0644")
	for _, p := range paths {
		nc.WriteFile(t, p, u1ShadowShimScript, "0755")
	}
	t.Logf("U1-b: planted %d shadow binaries (%d names in %s, plus %s)",
		len(paths), len(u1ShadowedTools), strings.Join(u1ShadowDirs, ", "), rootShimShadow)
}

// controlU1Shadows is U1-b's positive control. For each name it starts a transient
// systemd unit -- which runs with the MANAGER's default PATH, i.e. the PATH
// containerd and the kubelet would have WITHOUT the U1 drop-ins -- and has it exec
// the name through /usr/bin/env. Every one must land in a /usr/local shim. It then
// resets the marker, so every later hit is a finding.
//
// Two of the planted locations cannot be controlled this way and are called out
// rather than silently trusted: /opt/containerd/bin is only ever searched because
// containerd's `opt` plugin prepends it to its own PATH (U1-a step 6 proves the
// plugin did not run, and the recorded mutation is removing disabled_plugins), and
// the filesystem root is only reached by the shim's working-directory probe (the
// recorded mutation is removing runtime_path).
func controlU1Shadows(t *testing.T, nc *nodeContainer) {
	t.Helper()
	defaultPATH := managerDefaultPATH(t, nc)
	hasLocal := false
	for _, dir := range strings.Split(defaultPATH, ":") {
		if dir == "/usr/local/bin" || dir == "/usr/local/sbin" {
			hasLocal = true
			break
		}
	}
	if !hasLocal {
		t.Fatalf("U1-b: the manager's default service PATH is %q, which lists no /usr/local directory; the shadow check would pass vacuously on this image", defaultPATH)
	}
	t.Logf("U1-b: manager default service PATH is %q", defaultPATH)

	for _, tool := range u1ShadowedTools {
		out, err := nc.ExecTimeout(dockerTimeout, binSystemdRun, "--wait", "--pipe", "--quiet", "--collect", binEnv, tool, u1ProbeArg)
		if code := exitCode(err); code != shadowShimExit {
			t.Fatalf("U1-b control: a transient systemd unit running %q by name exited %d (%v), want %d from its shadow; the default PATH %q does not reach the planted directories first, so the no-hits check would be vacuous\n%s",
				tool, code, err, shadowShimExit, defaultPATH, out)
		}
	}

	hits := parseShadowHits(readU1ShadowHits(t, nc))
	if len(hits) != len(u1ShadowedTools) {
		t.Fatalf("U1-b control: want %d hits (one per name), got %d:\n%s", len(u1ShadowedTools), len(hits), formatU1Hits(hits))
	}
	for i, tool := range u1ShadowedTools {
		// The first field is the shim's own $0: the directory that was searched.
		want := map[string]bool{"/usr/local/sbin/" + tool: true, "/usr/local/bin/" + tool: true}
		if !want[hits[i].Tool] || !strings.Contains(hits[i].Line, u1ProbeArg) {
			t.Fatalf("U1-b control: hit %d = %q, want %q or %q with argv %q",
				i, hits[i].Line, "/usr/local/sbin/"+tool, "/usr/local/bin/"+tool, u1ProbeArg)
		}
	}
	t.Logf("U1-b control: %d/%d names run by a systemd-started process resolved to a /usr/local shadow (exit %d)",
		len(hits), len(u1ShadowedTools), shadowShimExit)

	nc.WriteFile(t, u1ShadowHitsPath, "", "0644")
	t.Logf("U1-b: marker %s reset to empty; every later hit is a finding", u1ShadowHitsPath)
}

// readU1ShadowHits returns the raw U1 marker. plantU1Shadows creates it empty, so
// a read failure is a harness fault, never "no hits".
func readU1ShadowHits(t *testing.T, nc *nodeContainer) string {
	t.Helper()
	raw, err := nc.ReadFile(u1ShadowHitsPath)
	if err != nil {
		t.Fatalf("read U1 shadow marker %s: %v\n%s", u1ShadowHitsPath, err, raw)
	}
	return raw
}

// formatU1Hits renders hits one per line for a failure message.
func formatU1Hits(hits []shadowHit) string {
	lines := make([]string, 0, len(hits))
	for _, h := range hits {
		lines = append(lines, "  "+h.Line)
	}
	return strings.Join(lines, "\n")
}

// restartUnitsUnderShadows restarts the three units whose lookups U1 covers and
// waits, bounded, for each to be running again. Restarting is what forces the
// lookups to happen with the shadows in place: containerd runs `mkfs.erofs --help`
// by name at every start and re-resolves the shim for CRI introspection, the import
// oneshot drives a real image import through containerd, and the kubelet re-runs
// its mount/umount/systemd-run/iptables startup work for the pods already on the
// node.
func restartUnitsUnderShadows(t *testing.T, nc *nodeContainer) {
	t.Helper()
	// restartUnitTimeout bounds each blocking `systemctl restart`. The import
	// oneshot re-imports every bundled tarball, and its own TimeoutStartSec is 360s,
	// so this sits just above that rather than at the default per-call bound.
	const restartUnitTimeout = 7 * time.Minute

	restart := func(unit string) {
		t.Helper()
		out, err := nc.ExecTimeout(restartUnitTimeout, hostexec.SystemctlPath, "restart", unit)
		if err != nil {
			t.Fatalf("U1-b: systemctl restart %s: %v\n%s", unit, err, out)
		}
		t.Logf("U1-b: restarted %s", unit)
	}
	running := func(unit string) {
		t.Helper()
		nc.waitFor(t, unit+" running again after the U1-b restart", 90*time.Second, func() bool {
			props, err := nc.unitProps(unit, "ActiveState", "MainPID")
			return err == nil && props["ActiveState"] == "active" && pidRE.MatchString(props["MainPID"])
		})
	}

	// containerd first: this is the restart that re-runs `mkfs.erofs --help` and
	// re-resolves the shim. Wait for its admin API before driving anything through
	// it (the import oneshot uses ctr, not CRI).
	restart(containerdUnit)
	running(containerdUnit)
	nc.waitFor(t, "containerd admin API ready again", 90*time.Second, func() bool {
		_, err := nc.execErr(hostexec.CtrPath, "version")
		return err == nil
	})

	// The import oneshot is Requires=containerd.service, so containerd's restart
	// stopped it; running it again drives a real import through the fresh daemon,
	// which is where containerd's decompression helper lookup would happen.
	restart(imageImportUnit)

	// The kubelet last, so its startup mount/umount/systemd-run/iptables work runs
	// against a containerd that is already up.
	restart(kubeletUnit)
	running(kubeletUnit)

	// The CRI plugin initializes after containerd's admin API; wait for it so the
	// shim lookups triggered by CRI introspection have actually happened.
	nc.waitFor(t, "containerd CRI plugin ready again", 120*time.Second, func() bool {
		_, err := nc.execErr(binCrictl, "--runtime-endpoint", criEndpoint, "info")
		return err == nil
	})
	// And for the apiserver to answer again, which means the kubelet has restarted
	// the static pods: sandbox creation, shim starts, and the projected-token
	// tmpfs mounts that exercise the kubelet's `mount` lookup.
	nc.waitFor(t, "apiserver /healthz ok after the U1-b restart", 3*time.Minute, func() bool {
		o, e := nc.execErr(hostexec.KubectlPath, "--kubeconfig", adminConf, "get", "--raw", "/healthz")
		return e == nil && strings.TrimSpace(o) == "ok"
	})
}

// assertNoU1ShadowHits is U1-b's verdict.
func assertNoU1ShadowHits(t *testing.T, nc *nodeContainer) {
	t.Helper()
	raw := readU1ShadowHits(t, nc)
	hits := parseShadowHits(raw)
	if len(hits) > 0 {
		t.Errorf("U1-b: %d by-name exec(s) resolved through a persistent directory instead of the image (ADR-19 U1); each line is the shadow's own path, the argv and the parent:\n%s",
			len(hits), formatU1Hits(hits))
		return
	}
	t.Logf("U1-b: marker %s is empty after restarting %s, %s and %s with %d shadow binaries planted",
		u1ShadowHitsPath, containerdUnit, imageImportUnit, kubeletUnit, len(u1ShadowPaths()))
}

// assertDaemonExecPath runs U1-a and U1-b in order. U1-a must hold before the
// shadows are planted (an already-wrong PATH would turn U1-b into a different
// test), and it is re-run afterwards because the restarts in between produce the
// shims and daemon processes whose environments matter most.
func assertDaemonExecPath(t *testing.T, nc *nodeContainer) {
	t.Helper()
	assertDaemonExecPathLive(t, nc)
	// Before planting: nothing created /opt/containerd at all.
	assertOptPluginNeverInitialized(t, nc, "before planting", optContainerdDir)

	plantU1Shadows(t, nc)
	controlU1Shadows(t, nc)
	restartUnitsUnderShadows(t, nc)

	assertDaemonExecPathLive(t, nc)
	// After the restarts: U1-b created optContainerdBinDir itself, so the check
	// narrows to the sibling only the plugin would create. containerd has since
	// restarted with that bin directory populated, which makes the zero-hit check
	// below the stronger statement: the directory exists, is full of executables,
	// and was still never searched.
	assertOptPluginNeverInitialized(t, nc, "after the restarts", optContainerdDir+"/lib")
	assertNoU1ShadowHits(t, nc)
}
