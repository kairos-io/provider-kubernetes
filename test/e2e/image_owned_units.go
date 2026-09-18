//go:build e2e

package e2e

// image_owned_units.go is the harness side of ADR-19 U2 (F-UNITPATH): the units
// this provider owns are content of the BOOTED IMAGE, and the copies earlier
// releases left in the persistent /etc/systemd/system are cleaned up once, at
// boot, before anything starts from them.
//
//	U2-a  every provider unit's FragmentPath is under /usr/lib/systemd/system, its
//	      drop-ins are all under /usr/lib, `systemctl is-enabled` says "static" and
//	      exits 0, each unit is linked from multi-user.target.wants by a relative
//	      ../<unit> symlink, and no provider-owned name exists anywhere under
//	      /etc/systemd. A separate check proves kubeadm's own preflight prints no
//	      "service is not enabled" warning, which is what "static" buys.
//	U2-b  the trap, then the migration, on the BOOT path (S19-12). The frozen bytes
//	      a pre-U2 release installed are planted in /etc/systemd/system together
//	      with the absolute .wants links `systemctl enable` used to write and an
//	      operator drop-in; after a daemon-reload the FragmentPath of all three
//	      units is /etc, which is the state every upgraded node is in. The node
//	      container is then RESTARTED, so systemd boots again as PID 1 and the
//	      migrate unit runs in its normal ordering -- a manual `systemctl start`
//	      would prove nothing about ordering. The migration must have removed the
//	      seven files, reloaded the manager, and finished BEFORE containerd, the
//	      import unit and the kubelet were started, with FragmentPath back under
//	      /usr/lib on that same boot and without the harness reloading anything.
//	      The operator drop-in and its directory must survive.
//	U2-c  an EDITED copy is kept, the migrate unit fails, and the warning names the
//	      path. Removing the edit makes the next run clean again.
//
// Everything here reads typed systemd properties (`systemctl show`) and the
// journal as JSON, never free-form status text, and every wait is bounded.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kairos-io/provider-kubernetes/internal/hostexec"
)

// --- what the image ships ---------------------------------------------------

const (
	// unitDir is the image's unit directory: part of the A/B-upgraded read-only
	// system, in no persistent bind.
	unitDir = "/usr/lib/systemd/system"
	// wantsDir holds the enablement links. An image cannot run `systemctl enable`
	// (that writes absolute links into the persistent /etc and needs an [Install]
	// section), so the links are shipped, relative, and named after the unit.
	wantsDir = unitDir + "/multi-user.target.wants"

	// etcUnitDir and the two below are the persistent locations a pre-U2 release
	// installed into and that the migration cleans up.
	etcUnitDir          = "/etc/systemd/system"
	etcWantsDir         = etcUnitDir + "/multi-user.target.wants"
	etcKubeletDropInDir = etcUnitDir + "/kubelet.service.d"

	// unitMigrateUnit runs `agent-provider-kubernetes migrate-units` once per boot.
	unitMigrateUnit = "provider-kubernetes-unit-migrate.service"

	// operatorDropIn is an override an operator could reasonably have written. The
	// migration must leave it, and its directory, alone: its scope is a fixed list
	// of the names WE shipped, never a glob over the directory.
	operatorDropIn        = etcKubeletDropInDir + "/20-operator.conf"
	operatorDropInContent = "[Service]\n" +
		"Environment=\"PROVIDER_KUBERNETES_E2E_OPERATOR_DROPIN=1\"\n"
)

// providerUnits are the four units the image owns. The migration unit is one of
// them, so "the migrate unit is present, static and linked" needs no separate case.
var providerUnits = []string{containerdUnit, kubeletUnit, imageImportUnit, unitMigrateUnit}

// migratedUnits are the three whose stale /etc copies the migration removes. The
// migrate unit itself never existed under /etc.
var migratedUnits = []string{containerdUnit, kubeletUnit, imageImportUnit}

// --- the frozen /etc copies -------------------------------------------------

// frozenCopy is one file a release before U2 installed into the persistent
// /etc/systemd/system, with the sha256 of the exact bytes it installed. These are
// the contents the migration's frozen table must recognize; the fixtures under
// testdata are those bytes, and readFrozen re-hashes them, so a fixture that
// drifts fails here rather than quietly planting something the migration is right
// to keep.
//
// The hashes are the ADR-19 set, re-derived from the full history of the four
// paths at 013483d (U1). U2 moves the same files to /usr/lib and edits them, which
// changes their content: that is exactly why the table is FROZEN on the old bytes.
type frozenCopy struct {
	EtcPath string
	Fixture string
	SHA256  string
}

var frozenEtcCopies = []frozenCopy{
	{
		EtcPath: etcUnitDir + "/containerd.service",
		Fixture: "testdata/u2-frozen-etc/containerd.service",
		SHA256:  "b47a1e62d00497a7363f987f2c2a6ecd8e41778de7cd81696e431f28c4d7bbb5",
	},
	{
		EtcPath: etcUnitDir + "/kubelet.service",
		Fixture: "testdata/u2-frozen-etc/kubelet.service",
		SHA256:  "3e5647fb9b90d1fbe10fc23e5f17eaa890c27630c910c6ebafc8aa85519fbb52",
	},
	{
		EtcPath: etcUnitDir + "/provider-kubernetes-image-import.service",
		Fixture: "testdata/u2-frozen-etc/provider-kubernetes-image-import.service",
		SHA256:  "fcf2b3a936107385efb66c8699a2a6417221512d97ea3573a5124aae8dcf5d65",
	},
	{
		EtcPath: etcKubeletDropInDir + "/10-kubeadm.conf",
		Fixture: "testdata/u2-frozen-etc/kubelet.service.d/10-kubeadm.conf",
		SHA256:  "47f61bc9b23acfcb56f35f547a6f0e8bdc2cfb729dfd7dbf1fa49d7534d5aff3",
	},
}

// plantedMode is the mode the copies are planted with. The provider's build has
// always COPYed them with the checkout's mode, which is 0664 under umask 002, and
// that is what a VM node was observed carrying, so planting 0664 also proves the
// migration ignores the mode rather than matching on it (ADR-19 decision 3).
const plantedMode = "0664"

// wantRemoved is the number of paths a full pre-U2 /etc layout costs: three
// fragments, one drop-in, three .wants links.
const wantRemoved = 7

// readFrozen returns the fixture's bytes after checking them against the hash the
// migration's table must carry for that path.
func readFrozen(t *testing.T, f frozenCopy) string {
	t.Helper()
	b, err := os.ReadFile(f.Fixture)
	if err != nil {
		t.Fatalf("U2: read frozen fixture %s: %v", f.Fixture, err)
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != f.SHA256 {
		t.Fatalf("U2: frozen fixture %s hashes to %s, want %s; it is no longer the content this project shipped at %s, so planting it would not exercise the migration",
			f.Fixture, got, f.SHA256, f.EtcPath)
	}
	return string(b)
}

// --- the migrate unit's output ----------------------------------------------

// The log contract of `migrate-units` (ADR-19 U2). Paths are %q-quoted, and the
// provider logs through logrus, whose text formatter quotes the whole message and
// escapes those inner quotes, so each pattern accepts an optional backslash before
// the quote and works on both the raw stream and the journal.
var (
	migrateSummaryRE = regexp.MustCompile(
		`unit-migrate: summary outcome=(clean|migrated|kept-modified|failed) removed=([0-9]+) kept=([0-9]+) overrides=([0-9]+) reloaded=(true|false)`)
	migrateRemovedRE  = regexp.MustCompile(`unit-migrate: removed \\?"(.+?)\\?" kind=(fragment|dropin|link)`)
	migrateKeptRE     = regexp.MustCompile(`unit-migrate: kept \\?"(.+?)\\?" reason=([a-z-]+)`)
	migrateOverrideRE = regexp.MustCompile(`unit-migrate: override \\?"(.+?)\\?"`)
	migrateReloadedRE = regexp.MustCompile(`unit-migrate: reloaded(\\?"|$|\s)`)
)

// migrateKeptReasons is the closed set of reasons the contract allows, so an
// unknown one is a finding rather than something the test skips over.
var migrateKeptReasons = map[string]bool{
	"modified": true, "symlink": true, "not-regular": true, "owner": true,
	"size": true, "link-target": true, "walk-unsafe": true, "io": true,
}

// migrateSummary is one parsed summary line.
type migrateSummary struct {
	Outcome   string
	Removed   int
	Kept      int
	Overrides int
	Reloaded  bool
}

// migrateEvent is one removed/kept/override line.
type migrateEvent struct {
	Verb   string // removed, kept, override
	Path   string
	Kind   string // fragment, dropin, link (removed only)
	Reason string // kept only
}

// migrateRun is everything one `migrate-units` process logged: its events, whether
// it logged a reload, and the summary that must be its LAST line.
type migrateRun struct {
	Events   []migrateEvent
	Reloaded bool
	Summary  migrateSummary
	// SummaryAt and ReloadedAt are CLOCK_MONOTONIC microseconds from the journal,
	// comparable with systemd's *TimestampMonotonic properties.
	SummaryAt  uint64
	ReloadedAt uint64
}

// removedPaths returns the paths of the run's removed lines, sorted.
func (r migrateRun) removedPaths() []string {
	var p []string
	for _, e := range r.Events {
		if e.Verb == "removed" {
			p = append(p, e.Path)
		}
	}
	sort.Strings(p)
	return p
}

// kept returns the run's kept events.
func (r migrateRun) kept() []migrateEvent {
	var k []migrateEvent
	for _, e := range r.Events {
		if e.Verb == "kept" {
			k = append(k, e)
		}
	}
	return k
}

// parseMigrateRuns splits the unit's journal into one migrateRun per process: the
// summary is the last line a run prints, so a summary closes a run. Lines after
// the last summary mean a run that has not finished (or crashed before its
// summary), and are reported by the caller as a missing run rather than merged
// into the previous one.
func parseMigrateRuns(entries []journalEntry) ([]migrateRun, error) {
	var runs []migrateRun
	cur := migrateRun{}
	open := false
	for _, e := range entries {
		msg := e.Message
		switch {
		case migrateSummaryRE.MatchString(msg):
			m := migrateSummaryRE.FindStringSubmatch(msg)
			s := migrateSummary{Outcome: m[1], Reloaded: m[5] == "true"}
			for i, dst := range []*int{&s.Removed, &s.Kept, &s.Overrides} {
				n, err := strconv.Atoi(m[i+2])
				if err != nil {
					return nil, fmt.Errorf("summary count %q: %w", m[i+2], err)
				}
				*dst = n
			}
			cur.Summary = s
			cur.SummaryAt = e.Monotonic
			runs = append(runs, cur)
			cur, open = migrateRun{}, false
		case migrateRemovedRE.MatchString(msg):
			m := migrateRemovedRE.FindStringSubmatch(msg)
			cur.Events = append(cur.Events, migrateEvent{Verb: "removed", Path: m[1], Kind: m[2]})
			open = true
		case migrateKeptRE.MatchString(msg):
			m := migrateKeptRE.FindStringSubmatch(msg)
			if !migrateKeptReasons[m[2]] {
				return nil, fmt.Errorf("kept line has unknown reason %q: %q", m[2], msg)
			}
			cur.Events = append(cur.Events, migrateEvent{Verb: "kept", Path: m[1], Reason: m[2]})
			open = true
		case migrateOverrideRE.MatchString(msg):
			m := migrateOverrideRE.FindStringSubmatch(msg)
			cur.Events = append(cur.Events, migrateEvent{Verb: "override", Path: m[1]})
			open = true
		case migrateReloadedRE.MatchString(msg):
			cur.Reloaded = true
			cur.ReloadedAt = e.Monotonic
			open = true
		}
	}
	if open {
		return runs, fmt.Errorf("%d unit-migrate line(s) after the last summary: a run logged no summary, which the contract makes its last line", len(cur.Events))
	}
	return runs, nil
}

// --- journal reading ---------------------------------------------------------

// journalEntry is one journal record: its message and the CLOCK_MONOTONIC
// microsecond stamp systemd's *TimestampMonotonic properties use as well, so the
// two can be compared directly.
type journalEntry struct {
	Message   string
	Monotonic uint64
}

// journalForUnit returns every record the unit's OWN processes logged on this
// boot, oldest first. _SYSTEMD_UNIT= matches only the unit's processes, so
// systemd's own "Starting/Finished" messages are not mistaken for the binary's
// output. JSON, not free text: MESSAGE is taken as a field, not scraped.
func (nc *nodeContainer) journalForUnit(t *testing.T, unit string) []journalEntry {
	t.Helper()
	stdout, stderr, err := nc.ExecStdoutTimeout(dockerTimeout,
		binJournalctl, "--no-pager", "-o", "json", "-b", "_SYSTEMD_UNIT="+unit)
	if err != nil {
		t.Fatalf("U2: journalctl -o json for %s: %v\n%s", unit, err, strings.TrimSpace(stderr))
	}
	var entries []journalEntry
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec struct {
			Message   json.RawMessage `json:"MESSAGE"`
			Monotonic string          `json:"__MONOTONIC_TIMESTAMP"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("U2: journal record is not JSON: %v\n%s", err, trimForLog(line))
		}
		var msg string
		if err := json.Unmarshal(rec.Message, &msg); err != nil {
			// A non-UTF-8 message is encoded as an array of bytes. Nothing the
			// provider logs is binary, so skip it rather than guess.
			continue
		}
		us, err := strconv.ParseUint(rec.Monotonic, 10, 64)
		if err != nil {
			t.Fatalf("U2: journal record has no usable __MONOTONIC_TIMESTAMP (%q): %v", rec.Monotonic, err)
		}
		entries = append(entries, journalEntry{Message: msg, Monotonic: us})
	}
	return entries
}

// currentMigrateRun returns what the CURRENT activation of the migrate unit
// logged, polling (bounded) so a check never races journald's write.
//
// The activation is scoped by systemd's own InactiveExitTimestampMonotonic, not by
// `journalctl -b`: systemd running as PID 1 in a container takes its boot id from
// the host's /proc/sys/kernel/random/boot_id, which does NOT change when the
// container is restarted, so -b spans every boot of the container and would also
// return the previous boot's run. The journal's __MONOTONIC_TIMESTAMP and systemd's
// *TimestampMonotonic are the same clock, so comparing them is exact.
func (nc *nodeContainer) currentMigrateRun(t *testing.T, what string) migrateRun {
	t.Helper()
	const timeout = 90 * time.Second
	startedAt := nc.unitMonotonic(t, unitMigrateUnit, "InactiveExitTimestampMonotonic")
	if startedAt == 0 {
		t.Fatalf("U2 (%s): %s has InactiveExitTimestampMonotonic=0, so it never started", what, unitMigrateUnit)
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		runs, err := parseMigrateRuns(nc.journalForUnit(t, unitMigrateUnit))
		switch {
		case err != nil:
			lastErr = err
		default:
			var current []migrateRun
			for _, r := range runs {
				if r.SummaryAt >= startedAt {
					current = append(current, r)
				}
			}
			switch len(current) {
			case 1:
				return current[0]
			case 0:
				lastErr = fmt.Errorf("none of the %d run(s) in the journal is newer than the activation at %d us", len(runs), startedAt)
			default:
				t.Fatalf("U2 (%s): %d runs of %s since its activation at %d us, want exactly 1 -- the unit ran more than once: %+v",
					what, len(current), unitMigrateUnit, startedAt, current)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("U2 (%s): %s logged no complete run since its activation at %d us within %s: %v",
				what, unitMigrateUnit, startedAt, timeout, lastErr)
		}
		time.Sleep(2 * time.Second)
	}
}

// --- systemd property helpers -----------------------------------------------

// monotonicProps are the timestamps the boot-ordering proof reads. systemd reports
// them in CLOCK_MONOTONIC microseconds, the same clock and unit as the journal's
// __MONOTONIC_TIMESTAMP, so no conversion or log scraping is involved.
var monotonicProps = []string{
	"InactiveExitTimestampMonotonic",  // the unit's job started
	"ActiveEnterTimestampMonotonic",   // it became active (a oneshot: ExecStart returned)
	"ExecMainStartTimestampMonotonic", // the main process was exec'd
}

// unitMonotonic returns one monotonic property of a unit, in microseconds. A zero
// value means "never happened this boot" and is reported as such by the caller.
func (nc *nodeContainer) unitMonotonic(t *testing.T, unit, prop string) uint64 {
	t.Helper()
	props, err := nc.unitProps(unit, prop)
	if err != nil {
		t.Fatalf("U2: %v", err)
	}
	v, err := strconv.ParseUint(strings.TrimSpace(props[prop]), 10, 64)
	if err != nil {
		t.Fatalf("U2: %s %s=%q is not a monotonic microsecond value: %v", unit, prop, props[prop], err)
	}
	return v
}

// fragmentPath returns the unit fragment systemd actually loaded.
func (nc *nodeContainer) fragmentPath(t *testing.T, unit string) string {
	t.Helper()
	props, err := nc.unitProps(unit, "FragmentPath")
	if err != nil {
		t.Fatalf("U2: %v", err)
	}
	return strings.TrimSpace(props["FragmentPath"])
}

// exists reports whether a path exists inside the container, without following a
// final symlink (test -e would call a dangling link absent; -L catches it).
func (nc *nodeContainer) exists(path string) bool {
	if _, err := nc.execErr(binTest, "-e", path); err == nil {
		return true
	}
	_, err := nc.execErr(binTest, "-L", path)
	return err == nil
}

// restart stops and starts the node container, so systemd boots again as PID 1 and
// every unit runs in its normal boot ordering. /run is a tmpfs, so the journal and
// anything else there starts empty on the new boot, which is what makes "this boot"
// unambiguous. Bounded: docker gets 60s to let systemd shut down cleanly
// (STOPSIGNAL SIGRTMIN+3) before it kills the container.
func (nc *nodeContainer) restart(t *testing.T, why string) {
	t.Helper()
	t.Logf("U2: restarting the node container (%s)", why)
	if out, err := dockerErr("restart", "--time", "60", nc.id); err != nil {
		t.Fatalf("U2: docker restart %s: %v\n%s", nc.name, err, out)
	}
	nc.waitReady(t)
}

// --- U2-a: the units belong to the image ------------------------------------

// assertImageOwnedUnitsLive is U2-a. It returns the number of violations instead
// of stopping, so one run reports everything that is wrong.
//
// allowedExternalDropIns are drop-ins outside /usr/lib the phase expects, which is
// only ever the operator drop-in U2-b plants: everywhere else a drop-in outside the
// image is a finding.
func assertImageOwnedUnitsLive(t *testing.T, nc *nodeContainer, phase string, allowedExternalDropIns ...string) int {
	t.Helper()
	violations := 0
	report := func(subject, msg string) {
		t.Helper()
		violations++
		t.Errorf("U2-a (%s) %s: %s", phase, subject, msg)
	}

	for _, unit := range providerUnits {
		props, err := nc.unitProps(unit, "FragmentPath", "DropInPaths", "UnitFileState", "LoadState")
		if err != nil {
			report(unit, err.Error())
			continue
		}
		if got, want := props["LoadState"], "loaded"; got != want {
			report(unit, fmt.Sprintf("LoadState=%q, want %q", got, want))
		}
		if got, want := props["FragmentPath"], unitDir+"/"+unit; got != want {
			report(unit, fmt.Sprintf("FragmentPath=%q, want %q; a fragment outside %s is persistent on Kairos and outranks the image (ADR-19 U2)", got, want, unitDir))
		}
		for _, d := range strings.Fields(props["DropInPaths"]) {
			if !strings.HasPrefix(d, unitDir+"/") && !slices.Contains(allowedExternalDropIns, d) {
				report(unit, fmt.Sprintf("DropInPaths includes %q, which is outside %s", d, unitDir))
			}
		}
		// UnitFileState is the same value `systemctl is-enabled` prints, read as a
		// property; the command itself is checked below for its exit code, which is
		// what kubeadm's preflight actually looks at.
		if got, want := props["UnitFileState"], "static"; got != want {
			report(unit, fmt.Sprintf("UnitFileState=%q, want %q; the unit has an [Install] section again, so `systemctl disable` would write a persistent override into %s", got, want, etcUnitDir))
		}
		t.Logf("U2-a (%s) %s: FragmentPath=%s DropInPaths=[%s] UnitFileState=%s",
			phase, unit, props["FragmentPath"], props["DropInPaths"], props["UnitFileState"])
	}

	// `systemctl is-enabled` must print "static" and exit 0. kubeadm's preflight
	// ServiceCheck reads exactly this exit code and only warns when it is non-zero
	// (1.37.0 preflight/checks.go:166-189), which is why dropping [Install] is safe.
	for _, unit := range providerUnits {
		stdout, stderr, err := nc.ExecStdoutTimeout(dockerTimeout, hostexec.SystemctlPath, "is-enabled", unit)
		if err != nil {
			report(unit, fmt.Sprintf("systemctl is-enabled exited %d (%v): %q %q", exitCode(err), err, strings.TrimSpace(stdout), strings.TrimSpace(stderr)))
			continue
		}
		if got := strings.TrimSpace(stdout); got != "static" {
			report(unit, fmt.Sprintf("systemctl is-enabled printed %q, want \"static\"", got))
		}
	}

	// The enablement links: a symlink named after the unit whose target is exactly
	// ../<unit>. systemd takes the dependency from the entry NAME and reads the
	// target only to compare basenames (v260.2 src/core/load-dropin.c:62-73,75-98,100),
	// so a relative target is both sufficient and unable to leave the image.
	for _, unit := range providerUnits {
		link := wantsDir + "/" + unit
		st, err := nc.lstat(link)
		if err != nil {
			report(link, err.Error())
			continue
		}
		if st.Type != "symbolic link" {
			report(link, fmt.Sprintf("is a %q, want a symbolic link; systemd ignores a .wants entry that is not one (load-dropin.c:50-60)", st.Type))
			continue
		}
		target, err := nc.execErr(binReadlink, "--", link)
		if err != nil {
			report(link, fmt.Sprintf("readlink: %v", err))
			continue
		}
		if got, want := strings.TrimSpace(target), "../"+unit; got != want {
			report(link, fmt.Sprintf("points at %q, want exactly %q", got, want))
		}
	}

	// And nothing provider-owned under /etc/systemd at all.
	for _, path := range providerOwnedEtcPaths() {
		if nc.exists(path) {
			report(path, "exists under the persistent /etc/systemd, where it outranks the image's unit on every future boot")
		}
	}

	if violations == 0 {
		t.Logf("U2-a (%s): %d units are static fragments under %s, linked with ../<unit>, and nothing provider-owned is under /etc/systemd",
			phase, len(providerUnits), unitDir)
	}
	return violations
}

// providerOwnedEtcPaths is every persistent path a pre-U2 release created, plus
// the drop-in directory. It is the list the migration works on, written out here
// independently so the two cannot drift silently.
func providerOwnedEtcPaths() []string {
	paths := []string{etcKubeletDropInDir + "/10-kubeadm.conf"}
	for _, unit := range migratedUnits {
		paths = append(paths, etcUnitDir+"/"+unit, etcWantsDir+"/"+unit)
	}
	paths = append(paths, etcUnitDir+"/"+unitMigrateUnit, etcWantsDir+"/"+unitMigrateUnit)
	return paths
}

// --- U2-a: kubeadm's own preflight ------------------------------------------

// kubeadmPreflightConfigPath holds the smallest config that keeps `kubeadm init
// phase preflight` from resolving a Kubernetes version over the network: with
// kubernetesVersion set, everything else is defaulted.
const kubeadmPreflightConfigPath = "/tmp/e2e-u2-preflight.yaml"

// assertNoServiceEnabledPreflightWarning runs kubeadm's OWN preflight and requires
// that it does not warn about a service being disabled. ServiceCheck only warns
// (it never errors) when `systemctl is-enabled` fails, so this could not break a
// bootstrap either way -- but "static, exit 0" is the whole reason dropping
// [Install] is safe, and a silent regression to "disabled" would otherwise only
// show up as noise in an operator's logs.
//
// --ignore-preflight-errors=all turns every preflight ERROR into a warning, so the
// command exits 0 whatever else the container is missing, and the warning machinery
// is exercised rather than short-circuited by the first failure.
func assertNoServiceEnabledPreflightWarning(t *testing.T, nc *nodeContainer) {
	t.Helper()
	cfg := strings.Join([]string{
		"apiVersion: kubeadm.k8s.io/v1beta4",
		"kind: ClusterConfiguration",
		"kubernetesVersion: " + kubernetesVersion(),
	}, "\n") + "\n"
	nc.WriteFile(t, kubeadmPreflightConfigPath, cfg, "0600")
	t.Cleanup(func() {
		_, _ = nc.execErr(binRm, "-f", kubeadmPreflightConfigPath)
	})

	out, err := nc.ExecTimeout(2*time.Minute, hostexec.KubeadmPath,
		"init", "phase", "preflight",
		"--config", kubeadmPreflightConfigPath,
		"--ignore-preflight-errors=all")
	if err != nil {
		t.Fatalf("U2-a: kubeadm init phase preflight exited %d (%v); the check below cannot be read from a run that did not complete\n%s",
			exitCode(err), err, trimForLog(out))
	}
	// Non-vacuity: prove kubeadm's preflight really ran and really printed its
	// phase output, so "no warning" is a statement about a run that happened.
	if !strings.Contains(out, "[preflight]") {
		t.Fatalf("U2-a: kubeadm printed no [preflight] output, so the absence of a service warning proves nothing:\n%s", trimForLog(out))
	}
	for _, marker := range []string{"service is not enabled", "[WARNING Service-"} {
		if strings.Contains(out, marker) {
			t.Errorf("U2-a: kubeadm's preflight warned about a disabled service (%q). The units are meant to report is-enabled \"static\", exit 0; a unit that has [Install] again but no /etc link reports \"disabled\" and produces this warning:\n%s",
				marker, trimForLog(out))
		}
	}
	t.Logf("U2-a: kubeadm's own preflight ran and printed no service-enablement warning")
}

// --- U2-b: the trap, then the migration on the boot path --------------------

// plantPreU2EtcCopies recreates, on a U2 node, exactly what an upgraded node
// carries: the frozen bytes of the four files, the three absolute .wants links
// `systemctl enable` wrote (install.c:572-601), and an operator drop-in that must
// survive. It returns after a daemon-reload, having proved the trap is armed
// (FragmentPath is /etc).
func plantPreU2EtcCopies(t *testing.T, nc *nodeContainer) {
	t.Helper()
	for _, f := range frozenEtcCopies {
		nc.WriteFile(t, f.EtcPath, readFrozen(t, f), plantedMode)
	}
	nc.WriteFile(t, operatorDropIn, operatorDropInContent, "0644")
	if out, err := nc.execErr(binMkdir, "-p", etcWantsDir); err != nil {
		t.Fatalf("U2-b: mkdir %s: %v\n%s", etcWantsDir, err, out)
	}
	for _, unit := range migratedUnits {
		if out, err := nc.execErr(binLn, "-sfn", etcUnitDir+"/"+unit, etcWantsDir+"/"+unit); err != nil {
			t.Fatalf("U2-b: link %s: %v\n%s", etcWantsDir+"/"+unit, err, out)
		}
	}
	// One reload, by the harness, to load the planted copies. It is the LAST thing
	// the harness reloads: everything after the restart must be the migration's own
	// doing.
	if out, err := nc.ExecTimeout(dockerTimeout, hostexec.SystemctlPath, "daemon-reload"); err != nil {
		t.Fatalf("U2-b: systemctl daemon-reload after planting: %v\n%s", err, out)
	}

	trapped := 0
	for _, unit := range migratedUnits {
		got := nc.fragmentPath(t, unit)
		want := etcUnitDir + "/" + unit
		if got != want {
			t.Fatalf("U2-b: after planting the frozen bytes, %s still loads %q, not %q; the trap is not armed, so the migration would have nothing to prove",
				unit, got, want)
		}
		trapped++
	}
	props, err := nc.unitProps(kubeletUnit, "DropInPaths")
	if err != nil {
		t.Fatalf("U2-b: %v", err)
	}
	for _, want := range []string{etcKubeletDropInDir + "/10-kubeadm.conf", operatorDropIn} {
		if !strings.Contains(props["DropInPaths"], want) {
			t.Fatalf("U2-b: kubelet DropInPaths=%q does not include the planted %q", props["DropInPaths"], want)
		}
	}
	t.Logf("U2-b: planted %d frozen copies (%s), %d absolute .wants links and %s; %d unit(s) now load from %s, and the kubelet's drop-ins are %s",
		len(frozenEtcCopies), plantedMode, len(migratedUnits), operatorDropIn, trapped, etcUnitDir, props["DropInPaths"])
}

// assertMigrationRanAtBoot is U2-b's verdict, after the container has been
// restarted. S19-12: the proof has to be the BOOT path, so everything here is read
// from the boot that just happened -- the unit's own result, its journal, and the
// monotonic timestamps of the three units it is ordered before.
func assertMigrationRanAtBoot(t *testing.T, nc *nodeContainer) {
	t.Helper()

	// 1. The unit succeeded, as the oneshot it is.
	props, err := nc.unitProps(unitMigrateUnit,
		"Type", "RemainAfterExit", "ActiveState", "Result", "ExecMainStatus", "NRestarts")
	if err != nil {
		t.Fatalf("U2-b: %v", err)
	}
	for _, want := range []struct{ key, value string }{
		{"Type", "oneshot"},
		{"ActiveState", "active"},
		{"Result", "success"},
		{"ExecMainStatus", "0"},
	} {
		if got := props[want.key]; got != want.value {
			t.Errorf("U2-b: %s %s=%q, want %q (full properties: %v)", unitMigrateUnit, want.key, got, want.value, props)
		}
	}

	// 2. The run of this boot's activation migrated the seven paths.
	run := nc.currentMigrateRun(t, "boot")
	if run.Summary.Outcome != "migrated" {
		t.Errorf("U2-b: summary outcome=%q, want \"migrated\"; the run saw %d removed and %d kept: %+v",
			run.Summary.Outcome, run.Summary.Removed, run.Summary.Kept, run.Events)
	}
	if run.Summary.Removed != wantRemoved {
		t.Errorf("U2-b: summary removed=%d, want %d (3 fragments + 1 drop-in + 3 links); removed paths: %v",
			run.Summary.Removed, wantRemoved, run.removedPaths())
	}
	if run.Summary.Kept != 0 {
		t.Errorf("U2-b: summary kept=%d, want 0; kept: %+v", run.Summary.Kept, run.kept())
	}
	if !run.Summary.Reloaded {
		t.Errorf("U2-b: summary reloaded=false; without a daemon-reload the queued containerd, kubelet and import jobs would still start from the stale /etc copies on this boot (S19-10)")
	}
	if !run.Reloaded {
		t.Errorf("U2-b: the run logged no \"unit-migrate: reloaded\" line although its summary says reloaded=%t", run.Summary.Reloaded)
	}
	if len(run.removedPaths()) != run.Summary.Removed {
		t.Errorf("U2-b: %d removed lines but summary removed=%d: %v", len(run.removedPaths()), run.Summary.Removed, run.removedPaths())
	}
	wantPaths := providerOwnedEtcPathsThatWerePlanted()
	sort.Strings(wantPaths)
	if got := run.removedPaths(); !slices.Equal(got, wantPaths) {
		t.Errorf("U2-b: removed %v, want exactly %v", got, wantPaths)
	}
	// The operator's drop-in is what the override report is for: named, never acted
	// on. It is reported by the drop-in directory's or the file's name.
	overrides := []string{}
	for _, e := range run.Events {
		if e.Verb == "override" {
			overrides = append(overrides, e.Path)
		}
	}
	if len(overrides) != run.Summary.Overrides {
		t.Errorf("U2-b: %d override lines but summary overrides=%d: %v", len(overrides), run.Summary.Overrides, overrides)
	}
	named := false
	for _, p := range overrides {
		if p == operatorDropIn || p == etcKubeletDropInDir {
			named = true
		}
	}
	if !named {
		t.Errorf("U2-b: the override report %v names neither %s nor %s, so an operator is not told what still overrides the image's unit", overrides, operatorDropIn, etcKubeletDropInDir)
	}
	t.Logf("U2-b: %s: outcome=%s removed=%d kept=%d overrides=%d %v reloaded=%t",
		unitMigrateUnit, run.Summary.Outcome, run.Summary.Removed, run.Summary.Kept,
		run.Summary.Overrides, overrides, run.Summary.Reloaded)

	// 3. S19-12, the point of restarting the container: the migration and its
	//    reload happened BEFORE the three units it is ordered before were started.
	//    The journal's __MONOTONIC_TIMESTAMP and systemd's *TimestampMonotonic are
	//    the same clock, so these are direct comparisons, not log correlation.
	migrateDone := nc.unitMonotonic(t, unitMigrateUnit, "ActiveEnterTimestampMonotonic")
	if migrateDone == 0 {
		t.Fatalf("U2-b: %s never became active this boot, so there is no ordering to check", unitMigrateUnit)
	}
	if run.SummaryAt == 0 {
		t.Fatalf("U2-b: the journal gave the summary line no monotonic timestamp")
	}
	for _, after := range []struct {
		unit string
		prop string
		why  string
	}{
		{containerdUnit, "ExecMainStartTimestampMonotonic", "containerd would have started from the stale /etc fragment"},
		{kubeletUnit, "ExecMainStartTimestampMonotonic", "the kubelet would have started from the stale /etc fragment and drop-in"},
		{imageImportUnit, "InactiveExitTimestampMonotonic", "the import oneshot would have started from the stale /etc fragment"},
	} {
		props, err := nc.unitProps(after.unit, after.prop, "Job", "ActiveState")
		if err != nil {
			t.Errorf("U2-b: %v", err)
			continue
		}
		started, perr := strconv.ParseUint(strings.TrimSpace(props[after.prop]), 10, 64)
		if perr != nil {
			t.Errorf("U2-b: %s %s=%q is not a monotonic microsecond value: %v", after.unit, after.prop, props[after.prop], perr)
			continue
		}
		if started == 0 {
			// The unit has not started yet. That cannot be "started before the
			// migration", but it only says anything at all if the unit is genuinely
			// part of this boot, so require the queued start job. kubelet.service
			// sits here for most of a container boot: it is ordered after
			// network-online.target, which waits for systemd-networkd-wait-online to
			// time out. Job is systemd's own property, empty when there is none.
			if props["Job"] == "" {
				t.Errorf("U2-b: %s neither started this boot (%s=0, ActiveState=%q) nor has a queued start job, so nothing is pulling it into the boot: the %s/%s link is not doing its job",
					after.unit, after.prop, props["ActiveState"], wantsDir, after.unit)
				continue
			}
			t.Logf("U2-b (S19-12): %s has not started yet (ActiveState=%q, start job %s still queued), so it cannot have started before the migration finished at %d us",
				after.unit, props["ActiveState"], props["Job"], run.SummaryAt)
			continue
		}
		for _, ev := range []struct {
			what string
			at   uint64
		}{
			{"the migration's summary", run.SummaryAt},
			{"the migration's reload", run.ReloadedAt},
			{"the migrate unit becoming active", migrateDone},
		} {
			if ev.at == 0 {
				continue
			}
			if ev.at >= started {
				t.Errorf("U2-b (S19-12): %s is at %d us but %s %s=%d, i.e. NOT before it: %s",
					ev.what, ev.at, after.unit, after.prop, started, after.why)
			}
		}
		t.Logf("U2-b (S19-12): %s %s=%d us, after the reload (%d us) and the summary (%d us)",
			after.unit, after.prop, started, run.ReloadedAt, run.SummaryAt)
	}

	// 4. On THIS boot, without the harness reloading anything, the three units load
	//    from the image again.
	for _, unit := range migratedUnits {
		got := nc.fragmentPath(t, unit)
		want := unitDir + "/" + unit
		if got != want {
			t.Errorf("U2-b: after the migration %s loads %q, want %q. The removals happened but systemd is still holding the definitions it read at boot, which is what the migration's own daemon-reload exists to prevent (S19-10)", unit, got, want)
		}
	}

	// 5. The planted files are gone and the operator's are not.
	for _, path := range providerOwnedEtcPathsThatWerePlanted() {
		if nc.exists(path) {
			t.Errorf("U2-b: %s still exists after outcome=migrated", path)
		}
	}
	for _, path := range []string{operatorDropIn, etcKubeletDropInDir} {
		if !nc.exists(path) {
			t.Errorf("U2-b: %s was removed; the migration works on a FIXED list of the names we shipped and must never touch an operator's override, nor rmdir a directory that still holds one", path)
		}
	}
	props, err = nc.unitProps(kubeletUnit, "DropInPaths")
	if err != nil {
		t.Fatalf("U2-b: %v", err)
	}
	if !strings.Contains(props["DropInPaths"], operatorDropIn) {
		t.Errorf("U2-b: kubelet DropInPaths=%q no longer includes the operator's %q", props["DropInPaths"], operatorDropIn)
	}
	t.Logf("U2-b: %s and %s survived; kubelet DropInPaths=%s", operatorDropIn, etcKubeletDropInDir, props["DropInPaths"])
}

// providerOwnedEtcPathsThatWerePlanted is the subset of providerOwnedEtcPaths the
// planting step creates, which is exactly what the migration must remove.
func providerOwnedEtcPathsThatWerePlanted() []string {
	paths := []string{etcKubeletDropInDir + "/10-kubeadm.conf"}
	for _, unit := range migratedUnits {
		paths = append(paths, etcUnitDir+"/"+unit, etcWantsDir+"/"+unit)
	}
	return paths
}

// --- U2-c: an edited copy is kept -------------------------------------------

// editedCopySuffix turns a frozen copy into something we never shipped. The
// migration must then keep the file, name it, and fail -- a kept copy is still
// shadowing the image's unit, and `systemctl --failed` is the only way an operator
// finds out.
const editedCopySuffix = "# edited on the node: this line is why the file is kept\n"

// assertEditedCopyIsKept is U2-c. It runs the migration through `systemctl start`
// rather than another reboot: what is under test here is the decision, not the
// ordering (U2-b proved the ordering), and the unit's failure has to be visible.
func assertEditedCopyIsKept(t *testing.T, nc *nodeContainer) {
	t.Helper()
	edited := frozenEtcCopies[1] // kubelet.service
	nc.WriteFile(t, edited.EtcPath, readFrozen(t, edited)+editedCopySuffix, plantedMode)

	out, err := nc.ExecTimeout(2*time.Minute, hostexec.SystemctlPath, "restart", unitMigrateUnit)
	if err == nil {
		t.Errorf("U2-c: `systemctl restart %s` succeeded with an edited copy at %s; a kept copy still shadows the image's unit, so the unit must fail and be visible in `systemctl --failed` (ADR-19 OQ-19-2)\n%s",
			unitMigrateUnit, edited.EtcPath, trimForLog(out))
	}

	props, perr := nc.unitProps(unitMigrateUnit, "ActiveState", "Result", "ExecMainStatus")
	if perr != nil {
		t.Fatalf("U2-c: %v", perr)
	}
	for _, want := range []struct{ key, value string }{
		{"ActiveState", "failed"},
		{"Result", "exit-code"},
		{"ExecMainStatus", "1"},
	} {
		if got := props[want.key]; got != want.value {
			t.Errorf("U2-c: %s %s=%q, want %q (full properties: %v)", unitMigrateUnit, want.key, got, want.value, props)
		}
	}

	run := nc.currentMigrateRun(t, "edited copy")
	if run.Summary.Outcome != "kept-modified" {
		t.Errorf("U2-c: summary outcome=%q, want \"kept-modified\"", run.Summary.Outcome)
	}
	if run.Summary.Removed != 0 {
		t.Errorf("U2-c: summary removed=%d, want 0; nothing else was planted: %v", run.Summary.Removed, run.removedPaths())
	}
	if run.Summary.Reloaded {
		t.Errorf("U2-c: summary reloaded=true although nothing was removed; a reload is only warranted when a fragment or drop-in went away (S19-10)")
	}
	named := false
	for _, k := range run.kept() {
		if k.Path == edited.EtcPath {
			named = true
			if k.Reason != "modified" {
				t.Errorf("U2-c: kept %q with reason=%q, want \"modified\"", k.Path, k.Reason)
			}
		}
	}
	if !named {
		t.Errorf("U2-c: no kept line named %q; an operator cannot act on a warning that does not say which file is shadowing the unit: %+v", edited.EtcPath, run.Events)
	}
	// S19-9: the file's content, size and hash must never be logged -- unit files
	// can carry proxy credentials in Environment=.
	for _, e := range nc.journalForUnit(t, unitMigrateUnit) {
		if strings.Contains(e.Message, editedCopySuffix[:20]) {
			t.Errorf("U2-c (S19-9): the journal quotes the content of the kept file, which may hold credentials: %q", e.Message)
		}
	}
	t.Logf("U2-c: %s failed with outcome=kept-modified and named %s", unitMigrateUnit, edited.EtcPath)

	// Removing the edit makes the node clean again, which is also the idempotence
	// check: a second run with nothing to do removes nothing and reloads nothing.
	if out, err := nc.execErr(binRm, "-f", edited.EtcPath); err != nil {
		t.Fatalf("U2-c: remove the edited copy: %v\n%s", err, out)
	}
	if out, err := nc.ExecTimeout(2*time.Minute, hostexec.SystemctlPath, "restart", unitMigrateUnit); err != nil {
		t.Fatalf("U2-c: `systemctl restart %s` still fails after the edited copy was removed: %v\n%s", unitMigrateUnit, err, trimForLog(out))
	}
	run = nc.currentMigrateRun(t, "clean rerun")
	if run.Summary.Outcome != "clean" || run.Summary.Removed != 0 || run.Summary.Reloaded {
		t.Errorf("U2-c: the rerun with nothing left to do reported outcome=%s removed=%d reloaded=%t, want clean/0/false (the migration must be idempotent)",
			run.Summary.Outcome, run.Summary.Removed, run.Summary.Reloaded)
	}
	t.Logf("U2-c: after removing the edit the rerun is outcome=%s removed=%d reloaded=%t",
		run.Summary.Outcome, run.Summary.Removed, run.Summary.Reloaded)
}

// --- the phase --------------------------------------------------------------

// assertImageOwnedUnits runs U2-a, U2-b and U2-c in order, and leaves the node in
// the state the image ships: no provider-owned file under /etc/systemd.
//
// It runs EARLY in the single-node test, before any shim is planted and before
// reconcile, for two reasons: U2-b restarts the container, which is only safe while
// nothing depends on the node's state (/run is a tmpfs and would be emptied); and
// an E-B7 or U1 shadow binary planted first would be resolved by name by whatever
// the boot runs, which is a different test's subject.
func assertImageOwnedUnits(t *testing.T, nc *nodeContainer) {
	t.Helper()
	if v := assertImageOwnedUnitsLive(t, nc, "as booted"); v > 0 {
		t.Fatalf("U2-a: %d violation(s) on the image as booted; the migration checks below would be measuring a different image", v)
	}
	assertNoServiceEnabledPreflightWarning(t, nc)

	plantPreU2EtcCopies(t, nc)
	nc.restart(t, "S19-12: the migration must be proven on the BOOT path, not by a manual start")
	assertMigrationRanAtBoot(t, nc)
	// The operator drop-in is still there on purpose at this point: U2-b has just
	// proved the migration kept it.
	assertImageOwnedUnitsLive(t, nc, "after the boot-time migration", operatorDropIn)

	assertEditedCopyIsKept(t, nc)

	// Leave no test residue: the operator drop-in U2-b proved is kept would
	// otherwise stay on the node for every later phase. Removing it needs a reload,
	// which is safe here (every assertion that depended on there being none has
	// already run).
	if out, err := nc.execErr(binRm, "-rf", etcKubeletDropInDir); err != nil {
		t.Fatalf("U2: remove %s: %v\n%s", etcKubeletDropInDir, err, out)
	}
	if out, err := nc.ExecTimeout(dockerTimeout, hostexec.SystemctlPath, "daemon-reload"); err != nil {
		t.Fatalf("U2: final daemon-reload: %v\n%s", err, out)
	}
	if v := assertImageOwnedUnitsLive(t, nc, "after the U2 phase"); v > 0 {
		t.Fatalf("U2: %d violation(s) left behind by the U2 phase", v)
	}
}
