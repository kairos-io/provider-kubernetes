//go:build e2e && nightly

package e2e

// nightly_upgrade_test.go is the Tier-2 (ADR-13 E4) kubeadm-LAYER in-place minor
// upgrade scenario -- the hardest nightly scenario, designed carefully below.
//
// Gating: `//go:build e2e && nightly` -- compiled only under `-tags "e2e nightly"`.
//
// SCOPE / BOUNDARY (load-bearing -- read before changing):
//   - This proves the KUBEADM-LAYER upgrade orchestration: a real
//     `kubeadm upgrade apply <higher>` flips a running cluster from minor N to
//     minor N+1, driven by the provider's real reconcile + ADR-12 upgrade path.
//   - It deliberately does NOT reboot and does NOT do a Kairos A/B image swap. The
//     A/B-reboot ORDERING hazard (ADR-12-R1's empirical deadlock: the NEW kubelet
//     boots first against an OLD kubeadm-written flags file and crashloops) is
//     fundamentally a reboot/binary-swap-at-boot event and stays a VM-only smoke
//     (ADR-13 explicit CI-coverage boundary). Here the OLD kubelet keeps the old
//     static control plane healthy while we swap binaries, so the local API stays
//     reachable and the plan is the clean [upgrade-apply] path (no kubelet-repair
//     prelude). kubeadm itself regenerates the kubelet config during apply.
//
// MECHANISM (the in-container binary swap):
//   The node image bundles exactly ONE Kubernetes minor. The nightly workflow
//   builds TWO adjacent-minor node images (lower + higher) and:
//     - runs this test in the LOWER-minor environment (E2E_NODE_IMAGE +
//       E2E_KUBERNETES_VERSION = the lower minor), and
//     - exposes the HIGHER-minor node image tag via E2E_UPGRADE_TO_NODE_IMAGE and
//       its version via E2E_UPGRADE_TO_VERSION.
//   The test:
//     1. init+converge a single-node control plane at the LOWER minor (real
//        kubeadm via reconcile).
//     2. Extract the HIGHER-minor /usr/bin/{kubeadm,kubelet,kubectl} from the
//        higher-minor node image (docker create + docker cp, never started) and
//        copy them OVER the running container's binaries in place. After this,
//        `kubeadm version` in the container reports the HIGHER minor, so ADR-3
//        Resolve(detected=higher, pin=higher) passes and target=higher.
//     3. Pre-pull the higher-minor control-plane images so `upgrade apply` does not
//        race the budget on a cold pull.
//     4. reconcile with clusterConfiguration.kubernetesVersion pinned to the HIGHER
//        minor. The FileProber reads NodeComponentVersion from the still-LOWER
//        kube-apiserver manifest; planUpgrade sees manifest-minor < target and (API
//        still reachable via the old kubelet) emits [upgrade-apply]; the executor
//        runs `kubeadm upgrade apply <higher> --yes --certificate-renewal=true` and
//        restarts the kubelet.
//   Assertions: status Converged; the kube-apiserver manifest image tag is now the
//   higher minor; the node's kubeletVersion is the higher minor; status.lastAction
//   is upgrade-apply (the real upgrade action ran).
//
// If the upgrade-to image/version env is not provided (e.g. running the nightly
// suite locally without staging the second image), the test SKIPS with a clear
// message rather than faking a no-op upgrade (the task constraint: do not weaken
// the assertion).

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kairos-io/kairos-sdk/clusterplugin"
)

// upgradeReconcileTimeout bounds the upgrade reconcile exec. It sits above the
// provider's UpgradeBudget Total (20m) so the docker exec never masks the
// provider's own bounded upgrade budget.
const upgradeReconcileTimeout = 22 * time.Minute

// upgradeToNodeImage / upgradeToVersion read the higher-minor staging env the
// nightly workflow sets. Empty -> the test skips (no second image staged).
func upgradeToNodeImage() string { return os.Getenv("E2E_UPGRADE_TO_NODE_IMAGE") }
func upgradeToVersion() string   { return os.Getenv("E2E_UPGRADE_TO_VERSION") }

// TestNightlyKubeadmInPlaceUpgrade (ADR-13 Tier-2): a real kubeadm-layer minor
// upgrade (N -> N+1) inside a container, driven by the provider's reconcile.
func TestNightlyKubeadmInPlaceUpgrade(t *testing.T) {
	toImage := upgradeToNodeImage()
	toVersion := upgradeToVersion()
	if toImage == "" || toVersion == "" {
		t.Skip("E2E_UPGRADE_TO_NODE_IMAGE / E2E_UPGRADE_TO_VERSION not set; " +
			"the nightly workflow stages the higher-minor node image. Skipping the in-place upgrade scenario " +
			"(it must not be faked as a no-op).")
	}
	// Sanity: the upgrade-to image must exist locally (the workflow builds it).
	if out, err := dockerErr("image", "inspect", toImage); err != nil {
		t.Fatalf("upgrade-to node image %q not present locally (%v); the nightly workflow must build it\n%s", toImage, err, out)
	}

	lowerVersion := kubernetesVersion()
	t.Logf("in-place kubeadm upgrade: %s -> %s (toImage=%s)", lowerVersion, toVersion, toImage)

	// --- 1. init + converge at the LOWER minor. -----------------------------
	nc, nodeName := initAndConverge(t, uniqueName("upgrade"))

	// Record the pre-upgrade apiserver manifest minor as a baseline.
	preManifest, err := nc.ReadFile(apiserverManifestPath)
	if err != nil {
		t.Fatalf("read pre-upgrade kube-apiserver manifest: %v\n%s", err, preManifest)
	}
	if !strings.Contains(preManifest, "kube-apiserver:"+lowerVersion) &&
		!strings.Contains(preManifest, "kube-apiserver:v"+strings.TrimPrefix(lowerVersion, "v")) {
		// Non-fatal: the patch tag may include a build suffix; log for diagnosis.
		t.Logf("note: pre-upgrade manifest does not contain the exact lower tag %q (it may carry a patch suffix)", lowerVersion)
	}

	// --- 2. swap in the HIGHER-minor toolchain binaries in place. -----------
	// Extract from the higher-minor node image (never started) onto the host, then
	// copy over the running container's binaries. After this `kubeadm version`
	// reports the higher minor.
	stageDir := t.TempDir()
	srcBins := []string{"/usr/bin/kubeadm", "/usr/bin/kubelet", "/usr/bin/kubectl"}
	hostBins := extractBinariesFromImage(t, toImage, stageDir, srcBins...)
	for i, hostPath := range hostBins {
		nc.CopyInto(t, hostPath, srcBins[i])
		nc.Exec("chmod", "0755", srcBins[i])
	}

	// Confirm the bundled kubeadm now reports the higher minor (the precondition for
	// ADR-3 Resolve to accept the higher pin).
	verOut := nc.Exec("kubeadm", "version", "-o", "short")
	if !strings.Contains(verOut, minorOf(toVersion)) {
		t.Fatalf("after binary swap, kubeadm version = %q, want minor %s (the higher-minor binary did not take effect)",
			strings.TrimSpace(verOut), minorOf(toVersion))
	}
	t.Logf("post-swap kubeadm version: %s", strings.TrimSpace(verOut))

	// --- 3. pre-pull the higher-minor control-plane images. -----------------
	// upgrade apply pulls the new control-plane images; warm them so the apply fits
	// the UpgradeBudget without racing a cold containerd pull.
	prepullControlPlaneImages(t, nc, toVersion)

	// --- 4. reconcile with the HIGHER-minor pin -> real upgrade apply. ------
	ip := nc.IP(t)
	upgradeCluster := clusterplugin.Cluster{
		Role:             clusterplugin.Role(clusterplugin.RoleInit),
		ClusterToken:     randomToken(t),
		ControlPlaneHost: ip,
		ProviderOptions:  map[string]string{"cluster_root_path": "/"},
		Options: strings.Join([]string{
			"clusterConfiguration:",
			"  kubernetesVersion: " + toVersion, // the pin = higher minor (== swapped binary)
			"  networking:",
			"    podSubnet: 10.244.0.0/16",
			"initConfiguration:",
			"  localAPIEndpoint:",
			"    advertiseAddress: " + ip,
		}, "\n") + "\n",
	}

	// Use the upgrade-sized exec timeout (above UpgradeBudget Total). writeCluster
	// AndReconcile uses the default reconcileTimeout (14m), which is below the
	// UpgradeBudget Total (20m), so for the upgrade we write the file then run with
	// the larger bound explicitly.
	nc.WriteFile(t, clusterStatePath, serializeCluster(t, upgradeCluster), "0600")
	out, upErr := nc.ExecTimeout(upgradeReconcileTimeout,
		providerBinaryPath, "reconcile", "--cluster-file="+clusterStatePath)
	if upErr != nil {
		t.Fatalf("upgrade reconcile failed: %v\n--- output ---\n%s", upErr, out)
	}
	t.Logf("upgrade reconcile output:\n%s", out)

	// --- assertions ---------------------------------------------------------

	// a. status: Converged + the real upgrade action ran (lastAction=upgrade-apply).
	st := readStatus(t, nc)
	if st.Phase != "Converged" {
		t.Errorf("status phase = %q, want Converged after upgrade (full: %+v)", st.Phase, st)
	}
	if st.Outcome != "success" {
		t.Errorf("status outcome = %q, want success", st.Outcome)
	}
	// The provider sets lastAction to the action that completed. A real upgrade runs
	// ActionUpgradeApply ("upgrade-apply"); a no-op would record "none". Asserting
	// this prevents the test from silently passing on a no-op (constraint: the
	// upgrade must actually run).
	if st.LastAction != upgradeApplyAction {
		t.Errorf("status lastAction = %q, want %q -- the real upgrade apply must have run, not a no-op (full: %+v)",
			st.LastAction, upgradeApplyAction, st)
	}

	// b. the kube-apiserver static-pod manifest image tag is now the higher minor.
	//    This is the authoritative per-node CP convergence signal (ADR-12-R1): it
	//    only reaches the target after THIS node's `upgrade apply` ran.
	nc.waitFor(t, "kube-apiserver manifest at higher minor", 90*time.Second, func() bool {
		m, e := nc.ReadFile(apiserverManifestPath)
		if e != nil {
			return false
		}
		return strings.Contains(m, "kube-apiserver:"+toVersion) ||
			strings.Contains(m, "kube-apiserver:"+minorOf(toVersion)+".")
	})

	// c. the node's running kubeletVersion is the higher minor. kubeadm upgrade apply
	//    + the kubelet restart move the running kubelet to the new version.
	nc.waitFor(t, "node kubeletVersion at higher minor", 120*time.Second, func() bool {
		o, e := nc.execErr("kubectl", "--kubeconfig", adminConf,
			"get", "node", nodeName, "-o", "jsonpath={.status.nodeInfo.kubeletVersion}")
		if e != nil {
			return false
		}
		kv := strings.TrimSpace(o)
		t.Logf("node kubeletVersion: %q (want minor %s)", kv, minorOf(toVersion))
		return strings.HasPrefix(kv, minorOf(toVersion)+".") || kv == toVersion
	})

	// d. the apiserver is still healthy after the upgrade (the control plane stayed
	//    up across the in-place upgrade).
	nc.waitFor(t, "apiserver /healthz ok after upgrade", 90*time.Second, func() bool {
		o, e := nc.execErr("kubectl", "--kubeconfig", adminConf, "get", "--raw", "/healthz")
		return e == nil && strings.TrimSpace(o) == "ok"
	})
}

// upgradeApplyAction is a local copy of reconcile.ActionUpgradeApply's string value
// ("upgrade-apply"). Kept local so the e2e package depends only on the public
// status.yaml wire contract, not internal/* (same discipline as provider.go's path
// constants). If the action string changes in internal/reconcile, change this too.
const upgradeApplyAction = "upgrade-apply"

// minorOf returns the "vMAJOR.MINOR" prefix of a "vMAJOR.MINOR.PATCH" version
// (e.g. "v1.35.0" -> "v1.35"). Used to match version strings that may carry a
// differing patch level between the bundled binary and the manifest/kubelet.
func minorOf(version string) string {
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return version
	}
	return parts[0] + "." + parts[1]
}
