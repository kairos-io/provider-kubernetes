//go:build e2e

package e2e

// exec_hardening.go is the harness side of E-B7, the security block-merge
// condition for ADR-1-A1 (F-ABSPATH): every exec the provider performs uses an
// absolute /usr/bin path and a closed environment. TestSingleNodeInitConverges
// proves it on the real image:
//
//	(a) image invariants: the binaries internal/hostexec names are root-owned,
//	    writable only by root, not setuid/setgid, and not reached through a
//	    symlink; kubeadm's ChildPATH stays inside /usr and out of /usr/local.
//	(b) shadow shims: shims planted first in PATH (/usr/local/bin, persistent,
//	    writable and first in PATH on Kairos) record any exec that resolves one
//	    of the provider's tools by name.
//	(c) hostile KUBERC on reconcile: a malformed kuberc must not reach the
//	    provider's kubectl.
//	(c2) hostile kuberc at kubectl's default location: with no HOME and working
//	    directory /, a valid kuberc at /.kube/kuberc must not change what the
//	    provider's kubectl does (KUBECTL_KUBERC=false, KUBERC=off).
//	(d) hostile CONTAINERD_ADDRESS on import-images: it must not reach ctr, and
//	    every images.lock entry must still import (summary outcome=success).
//
// systemd's own PATH lookups for the units (the containerd shim, the kubelet's
// helpers, and the persistent /opt directories containerd executes from) are
// ADR-19 U1 and live in daemon_exec_path.go, which runs after this phase on the
// same node container. Unit FILE placement and the cleanup of the stale
// persistent copies under /etc/systemd are ADR-19 U2 and live in
// image_owned_units.go, which runs BEFORE this phase (it reboots the node). The
// CNI plugin directory is still open (U3).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	k8syaml "sigs.k8s.io/yaml"

	"github.com/kairos-io/provider-kubernetes/internal/hostexec"
)

// --- docker exec -e / -w ---------------------------------------------------

// execOptions are the extra `docker exec` flags for one exec.
type execOptions struct {
	// Env holds KEY=VALUE variables (docker exec -e), layered over the
	// container's own environment.
	Env []string
	// Workdir is the working directory (docker exec -w). Empty keeps the image
	// default.
	Workdir string
}

// envKeyRE is the shape of an environment variable name the harness may set.
var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// dockerExecFlags turns execOptions into `docker exec` flags. Anything that is
// not a plain KEY=VALUE pair or a clean absolute directory is rejected before
// docker runs, so a mistaken entry can never be read as a different flag. Env
// values are never echoed in errors.
func dockerExecFlags(opts execOptions) ([]string, error) {
	args := make([]string, 0, 2*len(opts.Env)+2)
	for i, kv := range opts.Env {
		key, _, ok := strings.Cut(kv, "=")
		if !ok || !envKeyRE.MatchString(key) {
			return nil, fmt.Errorf("docker exec env entry #%d is not KEY=VALUE with KEY matching %s", i, envKeyRE)
		}
		args = append(args, "-e", kv)
	}
	if opts.Workdir != "" {
		if !path.IsAbs(opts.Workdir) || path.Clean(opts.Workdir) != opts.Workdir {
			return nil, fmt.Errorf("docker exec workdir %q is not a clean absolute path", opts.Workdir)
		}
		args = append(args, "-w", opts.Workdir)
	}
	return args, nil
}

// exitCode returns the exit status behind a docker exec error (docker exec exits
// with the in-container process status): 0 for nil, -1 when the process did not
// exit normally (e.g. killed by the harness timeout).
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// --- (a) image invariants --------------------------------------------------

// statFormat is the `stat -c` format for the image checks: file type, numeric
// owner, numeric group, and the octal mode including the setuid/setgid/sticky
// digit. stat without -L reports the path itself, so a symlink shows up as
// "symbolic link" instead of the file it points to.
const statFormat = "%F|%u|%g|%a"

// Permission bits as `stat -c %a` prints them (octal). These are the traditional
// Unix values, not os.FileMode's, which is why the mode is parsed by hand.
const (
	modeSetuid     = 0o4000
	modeSetgid     = 0o2000
	modeOwnerExec  = 0o100
	modeGroupWrite = 0o020
	modeOtherWrite = 0o002
)

// fileStat is one parsed statFormat line.
type fileStat struct {
	Type string // %F, e.g. "regular file", "directory", "symbolic link"
	UID  uint32
	GID  uint32
	Mode uint32 // %a parsed as octal, at most 07777
}

// parseFileStat parses one statFormat line.
func parseFileStat(out string) (fileStat, error) {
	line := strings.TrimSpace(out)
	fields := strings.Split(line, "|")
	if len(fields) != 4 {
		return fileStat{}, fmt.Errorf("stat output %q: want 4 |-separated fields, got %d", line, len(fields))
	}
	uid, err := strconv.ParseUint(fields[1], 10, 32)
	if err != nil {
		return fileStat{}, fmt.Errorf("stat output %q: uid: %w", line, err)
	}
	gid, err := strconv.ParseUint(fields[2], 10, 32)
	if err != nil {
		return fileStat{}, fmt.Errorf("stat output %q: gid: %w", line, err)
	}
	mode, err := strconv.ParseUint(fields[3], 8, 32)
	if err != nil {
		return fileStat{}, fmt.Errorf("stat output %q: mode is not octal: %w", line, err)
	}
	if mode > 0o7777 {
		return fileStat{}, fmt.Errorf("stat output %q: mode %o exceeds 07777", line, mode)
	}
	return fileStat{Type: fields[0], UID: uint32(uid), GID: uint32(gid), Mode: uint32(mode)}, nil
}

// execBinaryViolations lists every invariant a provider-executed binary breaks.
// An absolute path is only as trustworthy as the file behind it (ADR-1-A1):
//   - a regular file, not a symlink: a link could lead into the persistent,
//     writable /usr/local, and the path would lie about what runs;
//   - owned 0:0 with no group or other write bit: only root can replace it;
//   - owner-executable: the provider can actually run it;
//   - no setuid or setgid bit: none of these tools needs privilege on exec, and a
//     setuid kubectl or ctr would hand root to any local user.
func execBinaryViolations(st fileStat) []string {
	var v []string
	if st.Type != "regular file" {
		v = append(v, fmt.Sprintf("type %q, want %q (not following symlinks)", st.Type, "regular file"))
	}
	if st.UID != 0 || st.GID != 0 {
		v = append(v, fmt.Sprintf("owner %d:%d, want 0:0", st.UID, st.GID))
	}
	if st.Mode&modeOwnerExec == 0 {
		v = append(v, fmt.Sprintf("mode %04o is not owner-executable", st.Mode))
	}
	if st.Mode&(modeGroupWrite|modeOtherWrite) != 0 {
		v = append(v, fmt.Sprintf("mode %04o is group- or other-writable", st.Mode))
	}
	if st.Mode&(modeSetuid|modeSetgid) != 0 {
		v = append(v, fmt.Sprintf("mode %04o has the setuid or setgid bit", st.Mode))
	}
	return v
}

// systemDirViolations lists every invariant /usr or /usr/bin breaks: a directory
// anyone but root can write lets that user swap a binary in place under an
// unchanged absolute path.
func systemDirViolations(st fileStat) []string {
	var v []string
	if st.Type != "directory" {
		v = append(v, fmt.Sprintf("type %q, want %q (not following symlinks)", st.Type, "directory"))
	}
	if st.UID != 0 || st.GID != 0 {
		v = append(v, fmt.Sprintf("owner %d:%d, want 0:0", st.UID, st.GID))
	}
	if st.Mode&(modeGroupWrite|modeOtherWrite) != 0 {
		v = append(v, fmt.Sprintf("mode %04o is group- or other-writable", st.Mode))
	}
	return v
}

// childPathViolation returns "" when a canonical (readlink -f) ChildPATH entry is
// inside /usr and outside /usr/local; otherwise it describes the violation. The
// helpers kubeadm looks up itself (systemctl, kubelet, mount, ...) must come from
// the image's /usr, never from the persistent /usr/local an attacker can populate.
func childPathViolation(resolved string) string {
	p := path.Clean(resolved)
	switch {
	case !path.IsAbs(p):
		return fmt.Sprintf("resolves to %q, want an absolute path", resolved)
	case !pathWithin(p, "/usr"):
		return fmt.Sprintf("resolves to %q, outside /usr", resolved)
	case pathWithin(p, "/usr/local"):
		return fmt.Sprintf("resolves to %q, inside the persistent, writable /usr/local", resolved)
	}
	return ""
}

// pathWithin reports whether the clean absolute path p is dir or below it.
func pathWithin(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// lstat runs `stat -c statFormat` on p inside the container.
func (nc *nodeContainer) lstat(p string) (fileStat, error) {
	out, err := nc.execErr(binStat, "-c", statFormat, p)
	if err != nil {
		return fileStat{}, fmt.Errorf("stat: %v: %s", err, strings.TrimSpace(out))
	}
	return parseFileStat(out)
}

// canonicalPath runs `readlink -f` on p inside the container.
func (nc *nodeContainer) canonicalPath(p string) (string, error) {
	out, err := nc.execErr(binReadlink, "-f", p)
	if err != nil {
		return "", fmt.Errorf("readlink -f: %v: %s", err, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out), nil
}

// assertExecPathImageInvariants is E-B7 (a). It runs before any shim or hostile
// input exists and stops the test on any violation, because every later E-B7
// check assumes the absolute paths themselves can be trusted.
func assertExecPathImageInvariants(t *testing.T, nc *nodeContainer) {
	t.Helper()
	violations := 0
	report := func(subject, msg string) {
		t.Helper()
		violations++
		t.Errorf("E-B7 image invariant: %s: %s", subject, msg)
	}

	for _, p := range hostexec.Paths() {
		st, err := nc.lstat(p)
		if err != nil {
			report(p, err.Error())
			continue
		}
		for _, v := range execBinaryViolations(st) {
			report(p, v)
		}
		// readlink -f canonicalizes every component, so equality proves that no
		// symlink anywhere in the path (not only the last element) redirects it.
		got, err := nc.canonicalPath(p)
		switch {
		case err != nil:
			report(p, err.Error())
		case got != p:
			report(p, fmt.Sprintf("readlink -f = %q, want the path itself", got))
		}
		t.Logf("E-B7 (a) %s: %q owner %d:%d mode %04o, readlink -f %q", p, st.Type, st.UID, st.GID, st.Mode, got)
	}

	for _, dir := range strings.Split(hostexec.ChildPATH, ":") {
		got, err := nc.canonicalPath(dir)
		if err != nil {
			report("ChildPATH entry "+dir, err.Error())
			continue
		}
		if v := childPathViolation(got); v != "" {
			report("ChildPATH entry "+dir, v)
		}
		t.Logf("E-B7 (a) ChildPATH entry %s: readlink -f %q", dir, got)
	}

	for _, dir := range []string{"/usr", "/usr/bin"} {
		st, err := nc.lstat(dir)
		if err != nil {
			report(dir, err.Error())
			continue
		}
		for _, v := range systemDirViolations(st) {
			report(dir, v)
		}
		t.Logf("E-B7 (a) %s: %q owner %d:%d mode %04o", dir, st.Type, st.UID, st.GID, st.Mode)
	}

	if violations > 0 {
		t.Fatalf("E-B7: %d exec-path image invariant violation(s); the provider's absolute exec paths cannot be trusted on this image", violations)
	}
	t.Logf("E-B7 (a): no exec-path image invariant violations")
}

// --- (b) shadow shims ------------------------------------------------------

const (
	// shadowShimDir is first in the image PATH and, on Kairos, on the persistent
	// COS_PERSISTENT mount: exactly where a tampered or careless install would put
	// a binary that a name-based lookup picks over /usr/bin.
	shadowShimDir = "/usr/local/bin"
	// shadowHitsPath is the marker every shim appends to. shadowShimScript
	// hard-codes the same path (a unit test keeps the two in sync).
	shadowHitsPath = "/tmp/e2e-shadow-hits"
	// shadowShimExit is the status every shim exits with, so a by-name exec fails
	// loudly as well as being recorded.
	shadowShimExit = 97
)

// shadowedTools are the tools the provider runs, or kubeadm runs on its behalf.
// Binaries that systemd, containerd or the kubelet run for the units
// (containerd-shim-runc-v2, runc, mount, ...) belong to the ADR-19 U1 phase and
// are shadowed there, by u1ShadowedTools in daemon_exec_path.go, which plants them
// in more directories than this phase needs.
var shadowedTools = []string{"kubeadm", "kubectl", "kubelet", "systemctl", "ctr"}

// shadowShimScript is the fixed content of every shim. It records its own name
// (from $0, so no per-tool text is ever spliced into the script), its argv, and
// its parent's command name, then exits shadowShimExit. It uses shell builtins
// only, so a shim never looks anything up in PATH itself.
const shadowShimScript = `#!/bin/sh
# provider-kubernetes e2e shadow shim (ADR-1-A1, E-B7). Planted first in PATH:
# anything that runs this tool by name lands here instead of /usr/bin.
parent=unknown
read -r parent < "/proc/$PPID/comm" || parent=unknown
printf '%s argv=[%s] parent=%s\n' "${0##*/}" "$*" "$parent" >> /tmp/e2e-shadow-hits
exit 97
`

// shadowHit is one marker line: the shim that ran and the full recorded line.
type shadowHit struct {
	Tool string
	Line string
}

// parseShadowHits splits the marker into hits, ignoring blank lines.
func parseShadowHits(raw string) []shadowHit {
	var hits []shadowHit
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		tool, _, _ := strings.Cut(line, " ")
		hits = append(hits, shadowHit{Tool: tool, Line: line})
	}
	return hits
}

// readShadowHits returns the raw marker. plantShadowShims creates it empty, so a
// read failure is a harness fault, never "no hits".
func readShadowHits(t *testing.T, nc *nodeContainer) string {
	t.Helper()
	raw, err := nc.ReadFile(shadowHitsPath)
	if err != nil {
		t.Fatalf("read E-B7 shadow marker %s: %v\n%s", shadowHitsPath, err, raw)
	}
	return raw
}

// plantShadowShims is E-B7 (b). It plants a shim for each shadowed tool in
// shadowShimDir, then proves the shims really shadow: each tool run by name
// through the container's own PATH must land in its shim. Without that probe the
// final "no hits" assertion could pass vacuously (for example if PATH stopped
// putting /usr/local/bin first). The marker is then reset, so every later hit is
// a finding.
//
// The shims and marker are removed in t.Cleanup. On failure the hits are logged
// first; they hold only argv from execs inside a throwaway container whose
// credentials are all ephemeral per-run values.
func plantShadowShims(t *testing.T, nc *nodeContainer) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			if raw, err := nc.ReadFile(shadowHitsPath); err == nil && strings.TrimSpace(raw) != "" {
				t.Logf("=== E-B7 shadow-shim hits (%s) ===\n%s", shadowHitsPath, raw)
			}
		}
		rm := []string{binRm, "-f", shadowHitsPath}
		for _, tool := range shadowedTools {
			rm = append(rm, shadowShimDir+"/"+tool)
		}
		if out, err := nc.execErr(rm...); err != nil {
			t.Logf("remove E-B7 shadow shims (best effort; the container is removed next): %v\n%s", err, out)
		}
	})

	// Never overwrite something the image already ships there: that would hide a
	// real shadowing binary instead of reporting it.
	for _, tool := range shadowedTools {
		shim := shadowShimDir + "/" + tool
		if _, err := nc.execErr(binTest, "-e", shim); err == nil {
			t.Fatalf("E-B7: the image already ships %s, which shadows /usr/bin/%s for any name-based lookup", shim, tool)
		}
	}

	nc.WriteFile(t, shadowHitsPath, "", "0644")
	for _, tool := range shadowedTools {
		nc.WriteFile(t, shadowShimDir+"/"+tool, shadowShimScript, "0755")
	}

	// The probe is the only place the harness runs a tool by name, on purpose.
	const probeArg = "e2e-shadow-probe"
	for _, tool := range shadowedTools {
		out, err := nc.execErr(tool, probeArg)
		if code := exitCode(err); code != shadowShimExit {
			t.Fatalf("E-B7 probe: %q run by name exited %d (%v), want %d from its shim; the container PATH does not reach %s first, so the no-hits check would be vacuous\n%s",
				tool, code, err, shadowShimExit, shadowShimDir, out)
		}
	}
	raw := readShadowHits(t, nc)
	hits := parseShadowHits(raw)
	if len(hits) != len(shadowedTools) {
		t.Fatalf("E-B7 probe: want %d shim hits (one per tool), got %d:\n%s", len(shadowedTools), len(hits), raw)
	}
	for i, tool := range shadowedTools {
		if hits[i].Tool != tool || !strings.Contains(hits[i].Line, probeArg) {
			t.Fatalf("E-B7 probe: hit %d = %q, want tool %q with argv %q:\n%s", i, hits[i].Line, tool, probeArg, raw)
		}
	}
	t.Logf("E-B7 (b) probe: %d/%d by-name execs landed in their %s shims (exit %d):\n%s",
		len(hits), len(shadowedTools), shadowShimDir, shadowShimExit, strings.TrimSpace(raw))

	nc.WriteFile(t, shadowHitsPath, "", "0644")
	t.Logf("E-B7 (b): marker %s reset to empty; every later hit is a finding", shadowHitsPath)
}

// --- (c) hostile KUBERC ----------------------------------------------------

const malformedKubercPath = "/tmp/e2e-malformed-kuberc"

// malformedKuberc is deliberately not YAML (an unterminated flow sequence).
// kubectl 1.35 through 1.37 fails every command when a kuberc named explicitly via
// KUBERC cannot be decoded. A strict-decoding problem such as an unknown field
// only warns, so the fixture must be a syntax error.
const malformedKuberc = "apiVersion: kubectl.config.k8s.io/v1beta1\nkind: Preference\ndefaults: [\n"

// plantMalformedKuberc writes the E-B7 (c) fixture and returns the hostile
// environment for the reconcile exec. It first runs a control pair with the
// harness's own kubectl: `kubectl version --client` must succeed without KUBERC
// and fail, because of the kuberc, with it. Without the pair, a kubectl build that
// tolerated the file would let the step-5 check pass without testing anything.
func plantMalformedKuberc(t *testing.T, nc *nodeContainer) []string {
	t.Helper()
	t.Cleanup(func() {
		_, _ = nc.execErr(binRm, "-f", malformedKubercPath)
	})
	nc.WriteFile(t, malformedKubercPath, malformedKuberc, "0644")

	if out, err := nc.ExecTimeout(dockerTimeout, hostexec.KubectlPath, "version", "--client"); err != nil {
		t.Fatalf("E-B7 control: kubectl version --client failed without KUBERC: %v\n%s", err, out)
	}
	env := []string{"KUBERC=" + malformedKubercPath}
	out, err := nc.ExecEnvTimeout(dockerTimeout, env, hostexec.KubectlPath, "version", "--client")
	if err == nil {
		t.Fatalf("E-B7 control: kubectl accepted the malformed KUBERC=%s, so an inherited KUBERC would go unnoticed\n%s", malformedKubercPath, out)
	}
	if !strings.Contains(strings.ToLower(out), "kuberc") {
		t.Fatalf("E-B7 control: kubectl failed under KUBERC=%s, but not with a kuberc error: %v\n%s", malformedKubercPath, err, out)
	}
	t.Logf("E-B7 (c) control: kubectl version --client exits 0 without KUBERC and %d with KUBERC=%s: %s",
		exitCode(err), malformedKubercPath, strings.TrimSpace(out))
	return env
}

// --- (c2) hostile kuberc at kubectl's default location ----------------------

// Why /.kube/kuberc (kubernetes v1.37.0 source; unchanged in v1.35 and v1.36):
// client-go homedir.HomeDir() returns os.Getenv("HOME") on Linux, "" when unset,
// and kubectl's default kuberc is filepath.Join(HomeDir(), ".kube", "kuberc"),
// which is then the relative path .kube/kuberc. The provider's kubectl gets no
// HOME (hostexec.Kubectl), and kairos-agent runs as a systemd system service
// whose working directory defaults to /, so kubectl would read /.kube/kuberc.
// Only KUBECTL_KUBERC=false and KUBERC=off in the provider's kubectl
// environment stop that; (c) cannot catch their removal, because the closed
// environment never carries the reconcile exec's KUBERC anyway.
//
// The harness's own kubectl calls are unaffected: docker exec sets HOME=/root,
// so they resolve /root/.kube/kuberc instead. The control pair below proves that
// on the image rather than assuming it.
const (
	// reconcileWorkdir pins the reconcile exec's working directory to the one
	// production runs the provider in.
	reconcileWorkdir  = "/"
	defaultKubercDir  = "/.kube"
	defaultKubercPath = "/.kube/kuberc"

	// kubercProbeManifestPath is a local-only Node manifest for the control
	// pair; it is never sent to an API server.
	kubercProbeManifestPath = "/tmp/e2e-kuberc-probe-node.yaml"
	kubercProbeNodeName     = "e2e-kuberc-probe"
	kubercProbeManifest     = "apiVersion: v1\nkind: Node\nmetadata:\n  name: " + kubercProbeNodeName + "\n"
)

// hostileKuberc is a VALID kuberc that stops `kubectl annotate` from ever
// writing. kubectl applies each default only when that flag is not on the
// command line, and the provider's annotate passes neither:
//   - dry-run=server: the request is not persisted;
//   - local=true: kubectl never talks to the API server. Together with
//     dry-run=server it makes kubectl reject the command up front ("cannot
//     specify --local and --dry-run=server"). The annotation still never lands,
//     and the provider's swallowed-error warning names the injected flags, which
//     points straight at a kuberc when this fires.
const hostileKuberc = `apiVersion: kubectl.config.k8s.io/v1beta1
kind: Preference
defaults:
- command: annotate
  options:
  - name: dry-run
    default: server
  - name: local
    default: "true"
`

// kubercPreference mirrors the kubectl.config.k8s.io/v1beta1 Preference fields
// hostileKuberc uses (kubectl pkg/config/v1beta1 types.go).
type kubercPreference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Defaults   []struct {
		Command string `json:"command"`
		Options []struct {
			Name    string `json:"name"`
			Default string `json:"default"`
		} `json:"options"`
	} `json:"defaults"`
}

// kubercCommandDefaults parses a kuberc and returns the option defaults it sets
// for command (name -> default). It decodes the way kubectl does (YAML to JSON
// without a target type, then a strict JSON decode), so an unquoted `true` is a
// boolean and rejected for a string field, exactly as kubectl rejects it. It also
// rejects anything else kubectl would not apply as written: another apiVersion
// or kind, unknown fields, an option name with dashes (kubectl refuses those), or
// a repeated option.
func kubercCommandDefaults(content, command string) (map[string]string, error) {
	raw, err := k8syaml.YAMLToJSONStrict([]byte(content))
	if err != nil {
		return nil, fmt.Errorf("parse kuberc: %w", err)
	}
	var pref kubercPreference
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&pref); err != nil {
		return nil, fmt.Errorf("decode kuberc: %w", err)
	}
	if pref.APIVersion != "kubectl.config.k8s.io/v1beta1" || pref.Kind != "Preference" {
		return nil, fmt.Errorf("kuberc is %s %s, want kubectl.config.k8s.io/v1beta1 Preference", pref.APIVersion, pref.Kind)
	}
	got := map[string]string{}
	for _, d := range pref.Defaults {
		if d.Command != command {
			continue
		}
		for _, o := range d.Options {
			if o.Name == "" || strings.HasPrefix(o.Name, "-") {
				return nil, fmt.Errorf("kuberc option name %q must be a long flag name without dashes", o.Name)
			}
			if _, dup := got[o.Name]; dup {
				return nil, fmt.Errorf("kuberc sets option %q for %q more than once", o.Name, command)
			}
			got[o.Name] = o.Default
		}
	}
	return got, nil
}

// plantDefaultLocationKuberc is E-B7 (c2). It plants hostileKuberc at
// /.kube/kuberc (0644) and proves, with the image's own kubectl run from
// working directory /, that the file is live:
//  1. empty environment: kubectl loads it and applies dry-run=server, which it
//     then rejects for the probe's explicit --local;
//  2. empty environment plus the provider's KUBECTL_KUBERC=false KUBERC=off:
//     the file is ignored and the local annotate succeeds;
//  3. the harness's own environment (HOME=/root): the file is ignored, so the
//     harness's kubectl calls are unaffected.
//
// The probe annotates a local manifest only, so no leg ever writes to a cluster.
// Without leg 1, a kubectl that no longer read the default location would let
// step 5 pass without testing anything.
func plantDefaultLocationKuberc(t *testing.T, nc *nodeContainer) {
	t.Helper()
	defaults, err := kubercCommandDefaults(hostileKuberc, "annotate")
	if err != nil || defaults["dry-run"] == "" {
		t.Fatalf("E-B7 (c2): hostileKuberc does not set annotate --dry-run: %v (%v)", defaults, err)
	}

	if _, err := nc.execErr(binTest, "-e", defaultKubercPath); err == nil {
		t.Fatalf("E-B7 (c2): the image already ships %s, which kubectl reads when HOME is unset", defaultKubercPath)
	}
	_, dirErr := nc.execErr(binTest, "-e", defaultKubercDir)
	createdDir := dirErr != nil
	t.Cleanup(func() {
		rm := []string{binRm, "-f", defaultKubercPath, kubercProbeManifestPath}
		if createdDir {
			rm = []string{binRm, "-rf", defaultKubercDir, kubercProbeManifestPath}
		}
		_, _ = nc.execErr(rm...)
	})
	nc.WriteFile(t, defaultKubercPath, hostileKuberc, "0644")
	nc.WriteFile(t, kubercProbeManifestPath, kubercProbeManifest, "0644")

	probe := []string{hostexec.KubectlPath, "annotate", "--local", "-f", kubercProbeManifestPath, "e2e-kuberc-probe=1", "-o", "name"}
	atRoot := execOptions{Workdir: reconcileWorkdir}
	wantApplied := "--dry-run=" + defaults["dry-run"]
	wantIgnored := "node/" + kubercProbeNodeName

	applied, err := nc.ExecOptsTimeout(dockerTimeout, atRoot, append([]string{binEnv, "-i"}, probe...)...)
	appliedExit := exitCode(err)
	if err == nil || !strings.Contains(applied, wantApplied) {
		t.Fatalf("E-B7 (c2) control: with no HOME at %s, kubectl did not apply %s from %s (exit %d), so dropping the provider's kuberc switches would go unnoticed\n%s",
			reconcileWorkdir, wantApplied, defaultKubercPath, appliedExit, applied)
	}
	ignored, err := nc.ExecOptsTimeout(dockerTimeout, atRoot, append([]string{binEnv, "-i", "KUBECTL_KUBERC=false", "KUBERC=off"}, probe...)...)
	if err != nil || !strings.Contains(ignored, wantIgnored) {
		t.Fatalf("E-B7 (c2) control: KUBECTL_KUBERC=false KUBERC=off did not neutralize %s: %v\n%s", defaultKubercPath, err, ignored)
	}
	harness, err := nc.ExecOptsTimeout(dockerTimeout, atRoot, probe...)
	if err != nil || !strings.Contains(harness, wantIgnored) {
		t.Fatalf("E-B7 (c2) control: the harness's own kubectl (HOME=/root) picked up %s: %v\n%s", defaultKubercPath, err, harness)
	}
	t.Logf("E-B7 (c2) control: %s planted; empty env at %s -> exit %d: %s | + KUBECTL_KUBERC=false KUBERC=off -> exit 0: %s | harness env -> exit 0: %s",
		defaultKubercPath, reconcileWorkdir, appliedExit, strings.TrimSpace(applied), strings.TrimSpace(ignored), strings.TrimSpace(harness))
}

// --- (d) import-images under a hostile CONTAINERD_ADDRESS ------------------

const (
	// importImagesTimeout sits above the subcommand's own 5m context bound, so
	// the exec never masks the provider's budget.
	importImagesTimeout = 6 * time.Minute
	// hostileContainerdAddress is a socket that does not exist: a ctr that
	// inherited it could not reach containerd.
	hostileContainerdAddress = "/run/nonexistent.sock"
)

// The import-images output is parsed with parseImportSummary (bundled_images.go),
// and the expected count is the number of images.lock entries: the importer
// imports exactly the lock's entries from hostexec.BundleDir, never a *.tar glob.
