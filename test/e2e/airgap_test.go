//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/kairos-io/provider-kubernetes/internal/hostexec"
)

// airgap_test.go holds IR-11, the gate for the ADR-16 headline claim: a node
// with no registry reachable can still run `kubeadm init`, entirely from the
// pre-bundled, build-time-verified control-plane images.
//
// Why this test has to exist separately from every other scenario: all of them
// call prepullControlPlaneImages (`kubeadm config images pull`) before the
// reconcile, deliberately, to keep their timing deterministic. That warms the
// image store FROM THE REGISTRY, so none of them can see an air-gap regression
// -- and an air-gapped node could not bootstrap at all on v0.3.0 (see
// CHANGELOG.md), with README.md and samples/air-gapped/README.md advertising the
// capability throughout. Until IR-11 the claim had no automated coverage.
//
// IR-11 deliberately does NOT pre-pull. The node joins an --internal docker
// network: it still gets an IP (kubeadm must advertise one) but has no route
// off-host, so a registry fetch genuinely cannot succeed.
//
// The isolation is ASSERTED, not assumed. A test that silently ran with working
// network access would pass while proving nothing -- the same failure mode as a
// skipped gate -- so IR-11 fails if the node can resolve or reach a registry.

const (
	// airgapRegistryHost is the registry every bundled ref names. If the node
	// can reach it, the test is not air-gapped and must fail rather than pass
	// for the wrong reason.
	airgapRegistryHost = "registry.k8s.io"
	// airgapProbeTimeout bounds one reachability probe from inside the node.
	airgapProbeTimeout = 20 * time.Second
	// airgapConvergeTimeout bounds the waits for the apiserver and for node
	// registration after a successful reconcile.
	airgapConvergeTimeout = 120 * time.Second
	// The reachability probes. Absolute paths, argv only, and their presence is
	// asserted before their failure is read as evidence (see assertNodeIsAirGapped).
	binGetent = "/usr/bin/getent"
	binCurl   = "/usr/bin/curl"
)

// TestAirGappedInitConvergesFromTheBundle is IR-11.
func TestAirGappedInitConvergesFromTheBundle(t *testing.T) {
	netName := uniqueName("airgap-net")
	// --internal is what makes this a real air gap: docker attaches no gateway,
	// so there is no route off-host and no DNS upstream, while the node still
	// gets a routable-within-the-network IP for kubeadm to advertise.
	docker(t, "network", "create", "--internal",
		"--label", labelKey+"="+labelValue, netName)
	t.Cleanup(func() {
		// Runs after the container's own t.Cleanup has removed it; docker
		// refuses to remove a network that still has an endpoint attached.
		_, _ = dockerErr("network", "rm", netName)
	})

	nc := startNodeOnNetwork(t, uniqueName("airgap"), netName)

	// (a) Prove the air gap BEFORE trusting anything else this test observes.
	assertNodeIsAirGapped(t, nc)

	// (b) The boot-time import must have run and imported every lock entry.
	// Exit status 0 alone is not enough (outcome=not-bundled also exits 0), so
	// require the unit's own summary, exactly as IR-9 does.
	lock := readBundleLock(t, nc, "IR-11")
	props := waitImportUnitSettled(t, nc)
	if v := importUnitViolations(props); len(v) > 0 {
		journal, _, _ := nc.ExecStdoutTimeout(dockerTimeout, binJournalctl,
			"-u", imageImportUnit, "--no-pager", "-n", "50")
		t.Fatalf("IR-11 (b): %s did not complete successfully: %s (properties %v)\n--- journal ---\n%s",
			imageImportUnit, strings.Join(v, "; "), props, trimForLog(journal))
	}
	t.Logf("IR-11 (b): the boot import reported success for all %d lock entries (%s)",
		len(lock.Images), lock.KubernetesVersion)

	// (c) Every bundled ref must be visible to the CRI, with no registry. This
	// is the lookup kubeadm makes before deciding whether to pull: if it answers
	// here, nothing needs fetching, which is the whole air-gap mechanism.
	for _, ref := range lock.refs() {
		if _, _, err := nc.ExecStdoutTimeout(dockerTimeout, binCrictl,
			"--runtime-endpoint", criEndpoint, "--image-endpoint", criEndpoint,
			"inspecti", "-o", "json", ref); err != nil {
			t.Fatalf("IR-11 (c): crictl inspecti %s failed on the air-gapped node: %v\n"+
				"the bundle did not make this ref usable, so kubeadm would try to pull it and an air-gapped init would fail",
				ref, err)
		}
	}
	t.Logf("IR-11 (c): all %d bundled refs are CRI-visible with no registry reachable", len(lock.Images))

	// (d) The real test: reconcile role=init with NO pre-pull at all.
	ip := nc.IP(t)
	cluster := initCluster(ip, kubernetesVersion(), randomToken(t))
	out, err := writeClusterAndReconcile(t, nc, cluster)
	if err != nil {
		t.Fatalf("IR-11 (d): air-gapped reconcile (init) failed: %v\n"+
			"--- reconcile output ---\n%s", err, out)
	}
	t.Logf("IR-11 (d): air-gapped reconcile (init) succeeded:\n%s", trimForLog(out))

	// (e) Converged for real: the apiserver answers and the node registers.
	nc.waitFor(t, "apiserver /healthz ok (air-gapped)", airgapConvergeTimeout, func() bool {
		o, e := nc.execErr(hostexec.KubectlPath, "--kubeconfig", adminConf,
			"get", "--raw", "/healthz")
		return e == nil && strings.TrimSpace(o) == "ok"
	})
	var nodeName string
	nc.waitFor(t, "node registered (air-gapped)", airgapConvergeTimeout, func() bool {
		o, e := nc.execErr(hostexec.KubectlPath, "--kubeconfig", adminConf,
			"get", "nodes", "-o", "jsonpath={.items[*].metadata.name}")
		if e != nil {
			return false
		}
		nodeName = strings.TrimSpace(o)
		return nodeName != ""
	})
	t.Logf("IR-11 (e): air-gapped control plane converged, node %q registered", nodeName)

	// (f) Nothing may have reached for a registry. This is the assertion that
	// would have caught F-IMPORTBLOB's symptom early: there, a broken image-store
	// entry made consumers fall back to pulling, which only worked because CI had
	// network. On an air-gapped node such a pull cannot succeed, so a single
	// attempt means the bundle is not actually being used.
	assertNoPullAttempts(t, nc)
}

// assertNodeIsAirGapped fails unless the node genuinely cannot reach a registry.
// Without this the whole test could pass on a node with working network access,
// proving nothing. DNS resolution and a TCP connect are probed separately: an
// --internal network breaks both, but a misconfigured one might break only DNS,
// which a cached or hard-coded address would sail straight past.
func assertNodeIsAirGapped(t *testing.T, nc *nodeContainer) {
	t.Helper()

	// Guard against the instrument before reading anything into its failure: a
	// MISSING probe binary also exits non-zero, which would otherwise look
	// exactly like "unreachable" and turn this gate into a silent pass.
	for _, bin := range []string{binGetent, binCurl} {
		if out, err := nc.ExecTimeout(airgapProbeTimeout, "/usr/bin/test", "-x", bin); err != nil {
			t.Fatalf("IR-11 (a): probe binary %s is absent from the node image (%v, %q); "+
				"its failure could not be distinguished from an unreachable registry, so the air gap cannot be proven",
				bin, err, strings.TrimSpace(out))
		}
	}

	if out, err := nc.ExecTimeout(airgapProbeTimeout,
		binGetent, "hosts", airgapRegistryHost); err == nil {
		t.Fatalf("IR-11 (a): %s RESOLVES from the node (%q), so this test is not air-gapped; "+
			"refusing to report a pass that proves nothing",
			airgapRegistryHost, strings.TrimSpace(out))
	}

	// Belt and braces: even with DNS dead, prove a direct fetch cannot work.
	// curl exits non-zero on DNS failure (6) and on connect failure (7/28); any
	// of those is the air gap we want. Exit 0 means we are not air-gapped.
	if out, err := nc.ExecTimeout(airgapProbeTimeout, binCurl,
		"-sS", "--max-time", "8", "-o", "/dev/null",
		"https://"+airgapRegistryHost+"/v2/"); err == nil {
		t.Fatalf("IR-11 (a): https://%s/v2/ is REACHABLE from the node (%q), so this test is not air-gapped",
			airgapRegistryHost, strings.TrimSpace(out))
	}
	t.Logf("IR-11 (a): confirmed air-gapped: %s neither resolves nor connects from the node", airgapRegistryHost)
}

// assertNoPullAttempts fails if containerd logged any CRI image pull. On an
// air-gapped node a pull cannot succeed, so an attempt means something did not
// find the bundled image and the air-gap property is only holding by luck.
//
// The journal is read from inside the node rather than from a test-side capture:
// the e2e diagnostics dump only runs on failure, so a passing run would show an
// empty journal and make this assertion vacuous.
func assertNoPullAttempts(t *testing.T, nc *nodeContainer) {
	t.Helper()
	journal, _, err := nc.ExecStdoutTimeout(dockerTimeout, binJournalctl, "-b", "--no-pager")
	if err != nil {
		t.Fatalf("IR-11 (f): could not read the node journal to check for pull attempts: %v", err)
	}
	// Guard against the instrument: an empty journal would make every match
	// count below zero for the wrong reason.
	if strings.TrimSpace(journal) == "" {
		t.Fatalf("IR-11 (f): the node journal came back empty, so 'no pull attempts' cannot be concluded")
	}
	var offenders []string
	for _, line := range strings.Split(journal, "\n") {
		if strings.Contains(line, "PullImage") ||
			strings.Contains(line, "ErrImagePull") ||
			strings.Contains(line, "ImagePullBackOff") {
			offenders = append(offenders, strings.TrimSpace(line))
		}
	}
	if len(offenders) > 0 {
		n := len(offenders)
		if n > 5 {
			offenders = offenders[:5]
		}
		t.Fatalf("IR-11 (f): %d image-pull attempt(s) on an air-gapped node; the bundle is not being used:\n%s",
			n, strings.Join(offenders, "\n"))
	}
	t.Logf("IR-11 (f): no image-pull attempt reached the CRI; the init ran entirely from the bundle")
}
