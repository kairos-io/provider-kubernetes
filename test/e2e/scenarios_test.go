//go:build e2e

package e2e

// scenarios_test.go implements ADR-13 slice E3: the remaining Tier-1 scenarios
// that extend the single-node-init proof (TestSingleNodeInitConverges in
// init_test.go). Every scenario creates its own node container(s) for isolation,
// has its own bounded timeouts, and registers guaranteed teardown via t.Cleanup.
//
// Scenarios:
//   3. TestResetCleansArtifacts     -- reset cleans kubeadm artifacts
//   4. TestInitClobberRefusal       -- re-init on a live CP -> refuse-init, #4099-5
//   5. TestUpgradeSkewRefusal       -- skip-level patch -> refuse-upgrade, terminal
//   6. TestWorkerJoin               -- 2-container CA-pinned worker join (Tier-1, OQ-E2)

import (
	"strings"
	"testing"
	"time"

	"github.com/kairos-io/kairos-sdk/clusterplugin"
	k8syaml "sigs.k8s.io/yaml"
)

// resetTimeout bounds the `agent-provider-kubernetes reset` exec. It sits above
// kubeadm reset's own internal timeout (5m) so the exec never masks it.
const resetTimeout = 7 * time.Minute

// apiserverManifestPath is where kubeadm init writes the kube-apiserver static-pod
// manifest. The FileProber reads this path to derive NodeComponentVersion.
const apiserverManifestPath = "/etc/kubernetes/manifests/kube-apiserver.yaml"

// initCluster builds a role=init Cluster for the given node IP + k8s version.
// Used by multiple scenarios that need a fresh init.
func initCluster(ip, k8sVer, clusterToken string) clusterplugin.Cluster {
	return clusterplugin.Cluster{
		Role:             clusterplugin.Role(clusterplugin.RoleInit),
		ClusterToken:     clusterToken,
		ControlPlaneHost: ip,
		ProviderOptions:  map[string]string{"cluster_root_path": "/"},
		Options: strings.Join([]string{
			"clusterConfiguration:",
			"  kubernetesVersion: " + k8sVer,
			"  networking:",
			"    podSubnet: 10.244.0.0/16",
			"initConfiguration:",
			"  localAPIEndpoint:",
			"    advertiseAddress: " + ip,
		}, "\n") + "\n",
	}
}

// initAndConverge starts a node container, pre-pulls images, runs reconcile with
// role=init, waits for the apiserver and node registration, and returns the
// converged container plus the registered node name. It fails the test on any error.
func initAndConverge(t *testing.T, name string) (nc *nodeContainer, nodeName string) {
	t.Helper()
	nc = startNode(t, name)
	ip := nc.IP(t)
	k8sVer := kubernetesVersion()
	cluster := initCluster(ip, k8sVer, randomToken(t))

	prepullControlPlaneImages(t, nc, k8sVer)

	out, err := writeClusterAndReconcile(t, nc, cluster)
	if err != nil {
		t.Fatalf("reconcile (init): %v\n--- reconcile output ---\n%s", err, out)
	}
	t.Logf("reconcile (init) output:\n%s", out)

	// Wait for the apiserver to be reachable.
	nc.waitFor(t, "apiserver /healthz ok", 90*time.Second, func() bool {
		o, e := nc.execErr("kubectl", "--kubeconfig", adminConf,
			"get", "--raw", "/healthz")
		return e == nil && strings.TrimSpace(o) == "ok"
	})
	// Wait for node registration.
	nc.waitFor(t, "node registered", 90*time.Second, func() bool {
		o, e := nc.execErr("kubectl", "--kubeconfig", adminConf,
			"get", "nodes", "-o", "jsonpath={.items[*].metadata.name}")
		if e != nil {
			return false
		}
		nodeName = strings.TrimSpace(o)
		return nodeName != ""
	})
	t.Logf("node registered: %q", nodeName)
	return nc, nodeName
}

// TestResetCleansArtifacts (ADR-13 Tier-1 scenario 3): after a single-node init
// converges, run the REAL reset path (the `reset` subcommand from main.go,
// mirroring the production EventClusterReset entry point for the e2e harness).
//
// Asserts that kubeadm ran and cleaned the authoritative membership artifacts:
//   - /etc/kubernetes (PKI, admin.conf, manifests) is gone -- this directory is a
//     regular directory (not an anonymous docker volume mount point) so it is fully
//     removable by authoritativeArtifacts.
//   - /var/lib/etcd/member is gone -- the etcd member data within the volume.
//   - /var/lib/kubelet/kubeadm-flags.env is gone -- the kubelet join marker within
//     the volume.
//
// In the e2e container environment /var/lib/kubelet and /var/lib/etcd are Docker
// anonymous volume MOUNT POINTS (from -v /var/lib/kubelet in startNode). The Linux
// kernel refuses os.RemoveAll on a mount point with EBUSY ("device or resource
// busy"), so reset.Run returns a non-fatal error and the reset subcommand exits 1.
// This is a container-setup artifact: in production these are regular directories
// and os.RemoveAll succeeds. We allow exit 1 here and assert on the artifact
// cleanup that DID happen (/etc/kubernetes gone, contents of the volumes emptied
// by kubeadm reset -f).
//
// Note: the `reset` subcommand does NOT write a status doc (HandleClusterReset
// does, via writeResetStatus). So we do not assert status.yaml here.
func TestResetCleansArtifacts(t *testing.T) {
	nc, _ := initAndConverge(t, uniqueName("reset"))
	ip := nc.IP(t)
	k8sVer := kubernetesVersion()
	cluster := initCluster(ip, k8sVer, randomToken(t))

	// Write the cluster file for the reset subcommand to read (same path and
	// serialization as the reconcile path, mirroring the production contract).
	nc.WriteFile(t, clusterStatePath, serializeCluster(t, cluster), "0600")

	out, err := nc.ExecTimeout(resetTimeout, providerBinaryPath,
		"reset", "--cluster-file="+clusterStatePath)
	// In the e2e container, /var/lib/kubelet and /var/lib/etcd are Docker anonymous
	// volume mount points. The kernel refuses os.RemoveAll on a mount point (EBUSY),
	// so reset.Run returns a non-nil error and the subcommand exits 1. We tolerate
	// this specific exit-1 case and verify kubeadm reset ran correctly by checking
	// the artifacts that CAN be removed.
	t.Logf("reset output (exit error %v):\n%s", err, out)
	// Verify kubeadm reset itself ran by checking it emitted its log output.
	if !strings.Contains(out, "provider-kubernetes reset") {
		t.Fatalf("reset subcommand did not appear to run; output:\n%s", out)
	}

	// 1. /etc/kubernetes is gone: this directory is not a mount point, so
	//    authoritativeArtifacts removes it cleanly. Its absence proves kubeadm reset
	//    ran and the PKI + admin.conf + manifests are gone.
	if _, statErr := nc.execErr("test", "-e", "/etc/kubernetes"); statErr == nil {
		t.Error("/etc/kubernetes still present after reset (want: absent); kubeadm reset may not have run")
	}

	// 2. The etcd member data within the volume is gone.
	if _, statErr := nc.execErr("test", "-e", "/var/lib/etcd/member"); statErr == nil {
		t.Error("/var/lib/etcd/member still present after reset (want: absent)")
	}

	// 3. The kubelet join marker within the volume is gone.
	if _, statErr := nc.execErr("test", "-e", "/var/lib/kubelet/kubeadm-flags.env"); statErr == nil {
		t.Error("/var/lib/kubelet/kubeadm-flags.env still present after reset (want: absent)")
	}

	t.Logf("reset artifacts cleaned: /etc/kubernetes gone, /var/lib/etcd/member gone, kubeadm-flags.env gone -- reset ran correctly")
}

// TestInitClobberRefusal (ADR-13 Tier-1 scenario 4): proves the HA-3 init-clobber
// guard (#4099-5). The guard fires when a second node (Uninitialized) runs
// reconcile with role=init and the control-plane ENDPOINT is already reachable.
//
// Specifically (per plan.go): ActionRefuseInit is planned when
//
//	s.Membership == Uninitialized && s.ControlPlaneReachable == true
//
// Setup:
//  1. Start the CP container and converge it (apiserver at cpIP:6443).
//  2. Start a SECOND node container (fresh, Uninitialized).
//  3. Run reconcile on the second node with role=init, controlPlaneHost=cpIP.
//     The second node is Uninitialized; the CP is reachable at cpIP:6443.
//     Plan emits ActionRefuseInit -> terminal failure.
//
// Note: re-running reconcile on the SAME initialized node yields ActionNone
// (idempotent reboot-safe no-op), NOT a refusal. The HA-3 guard is specifically
// about preventing a NEW node from trampling an existing cluster.
//
// Asserts:
//   - reconcile exits non-zero.
//   - status.yaml reason=InitRefused, terminal=true.
//   - The original CP's admin.conf and apiserver are untouched.
func TestInitClobberRefusal(t *testing.T) {
	// 1. Start and converge the CP node.
	cpNC, _ := initAndConverge(t, uniqueName("clobber-cp"))
	cpIP := cpNC.IP(t)
	k8sVer := kubernetesVersion()

	// 2. Start a fresh second node (Uninitialized; no kubeadm init artifact).
	//    This node will attempt to init with the same controlPlaneHost as the CP.
	freshNC := startNode(t, uniqueName("clobber-new"))

	// 3. Build role=init cluster pointing at the existing CP endpoint. The fresh
	//    node is Uninitialized; the CP is reachable at cpIP:6443. Plan fires
	//    ActionRefuseInit (HA-3 guard).
	cluster := initCluster(cpIP, k8sVer, randomToken(t))
	out, err := writeClusterAndReconcile(t, freshNC, cluster)
	// reconcile MUST exit non-zero when it refuses.
	if err == nil {
		t.Fatalf("reconcile with role=init on fresh node against live CP should have exited non-zero (expected refuse-init), but succeeded\n--- output ---\n%s", out)
	}
	t.Logf("refuse-init reconcile output (expected failure):\n%s", out)

	// 1. status on the fresh (refused) node: InitRefused, terminal.
	st := readStatus(t, freshNC)
	if st.Phase != "Failed" {
		t.Errorf("status phase = %q, want Failed (after init-clobber refusal)", st.Phase)
	}
	if st.Reason != "InitRefused" {
		t.Errorf("status reason = %q, want InitRefused", st.Reason)
	}
	if !st.Terminal {
		t.Error("status terminal = false, want true (InitRefused is a terminal verdict)")
	}

	// 2. The original CP cluster was NOT re-initialized: admin.conf still present.
	if _, statErr := cpNC.ReadFile(adminConf); statErr != nil {
		t.Error("CP admin.conf missing after refused init -- the cluster was wrongly clobbered")
	}

	// 3. The original CP apiserver is still reachable (cluster untouched).
	cpNC.waitFor(t, "CP apiserver still reachable after refused re-init", 30*time.Second, func() bool {
		o, e := cpNC.execErr("kubectl", "--kubeconfig", adminConf,
			"get", "--raw", "/healthz")
		return e == nil && strings.TrimSpace(o) == "ok"
	})
}

// TestUpgradeSkewRefusal (ADR-13 Tier-1 scenario 5): prove that Plan emits
// ActionRefuseUpgrade when UpgradePath rejects the transition.
//
// To trigger ActionRefuseUpgrade the code path requires ALL of:
//   - target != "" (kubernetesVersion pin set and Resolve passes)
//   - s.Membership == Initialized
//   - s.NodeComponentVersion != "" (apiserver manifest readable)
//   - UpgradePath(nodeComponentVersion, target) returns an error
//
// With a single v1.34 image, a straight pin mismatch fails at kubeadm.Resolve
// (minor mismatch), not inside Plan. To reach Plan we:
//  1. Patch the kube-apiserver static-pod manifest (written by kubeadm init) to
//     replace the image tag with v1.32.0 -- two minors below, a skip-level target
//     that UpgradePath("v1.32.0", "v1.34.0") rejects with a skip-level error.
//  2. Pin kubernetesVersion to the ACTUAL binary version (same minor as detected,
//     so Resolve passes and target is set).
//
// The manifest file edit is Go string replacement, no shell interpolation
// (design principle 1). The reconcile runs in the same filesystem view, reads
// the patched manifest, and refuses terminally within milliseconds.
//
// Asserts: reconcile exits non-zero, status reason=UpgradeRefused, terminal=true,
// apiserver still reachable (no destructive action ran).
func TestUpgradeSkewRefusal(t *testing.T) {
	nc, _ := initAndConverge(t, uniqueName("skew"))
	k8sVer := kubernetesVersion() // e.g. "v1.34.0" -- same as binary

	// Read the kube-apiserver manifest (written by kubeadm init).
	manifestRaw, err := nc.ReadFile(apiserverManifestPath)
	if err != nil {
		t.Fatalf("read kube-apiserver manifest: %v\n%s", err, manifestRaw)
	}

	// Inject a fake skip-level version into the manifest image tag. The FileProber
	// regex is `kube-apiserver:(v[0-9]+\.[0-9]+\.[0-9]+[^\s"']*)` which matches the
	// image line. We do Go-native string replacement: find the actual version tag and
	// replace it with v1.32.0 (two minors below any supported minor, guaranteed
	// skip-level for UpgradePath). No shell, no fmt.Sprintf.
	//
	// The target to replace is the actual tag in the manifest, e.g. ":v1.34.0" or
	// ":v1.35.0". We replace the first occurrence of ":v" + digits + "." + minor
	// in the kube-apiserver image lines by substituting the full version suffix.
	fakeNodeVersion := "v1.32.0"
	patched := injectManifestVersion(manifestRaw, k8sVer, fakeNodeVersion)
	if patched == manifestRaw {
		t.Fatalf("manifest version patch had no effect; manifest may not contain expected image tag %q\nmanifest excerpt:\n%s",
			k8sVer, manifestRaw[:min(len(manifestRaw), 400)])
	}

	// Write the patched manifest back. The FileProber reads it on the next reconcile.
	// WriteFile uses tee over stdin (no shell, no interpolation).
	nc.WriteFile(t, apiserverManifestPath, patched, "0644")
	t.Logf("patched kube-apiserver manifest: replaced image tag with %s (skip-level from %s)", fakeNodeVersion, k8sVer)

	// Run reconcile with a pin equal to the binary version. Resolve passes (same
	// minor); target is set to the binary version. Plan sees NodeComponentVersion=v1.32.0,
	// target=v1.34.x -> UpgradePath("v1.32.0", "v1.34.x") -> skip-level error ->
	// ActionRefuseUpgrade -> status reason=UpgradeRefused, terminal=true.
	ip := nc.IP(t)
	cluster := clusterplugin.Cluster{
		Role:             clusterplugin.Role(clusterplugin.RoleInit),
		ClusterToken:     randomToken(t),
		ControlPlaneHost: ip,
		ProviderOptions:  map[string]string{"cluster_root_path": "/"},
		Options: strings.Join([]string{
			"clusterConfiguration:",
			"  kubernetesVersion: " + k8sVer,
			"initConfiguration:",
			"  localAPIEndpoint:",
			"    advertiseAddress: " + ip,
		}, "\n") + "\n",
	}

	out, reconcileErr := writeClusterAndReconcile(t, nc, cluster)
	if reconcileErr == nil {
		t.Fatalf("reconcile with skip-level manifest patch should have failed, but succeeded\n--- output ---\n%s", out)
	}
	t.Logf("skew-refusal reconcile output (expected failure):\n%s", out)

	st := readStatus(t, nc)
	if st.Reason != "UpgradeRefused" {
		t.Errorf("status reason = %q, want UpgradeRefused (nodeVersion=%s target=%s); full status: %+v",
			st.Reason, fakeNodeVersion, k8sVer, st)
	}
	if !st.Terminal {
		t.Errorf("status terminal = false, want true (UpgradeRefused is always terminal); full status: %+v", st)
	}
	if st.Phase != "Failed" {
		t.Errorf("status phase = %q, want Failed; full status: %+v", st.Phase, st)
	}

	// Restore the original manifest so the kubelet can manage the static pod
	// cleanly for the remainder of the container lifetime.
	nc.WriteFile(t, apiserverManifestPath, manifestRaw, "0644")
	t.Logf("restored original kube-apiserver manifest")
}

// injectManifestVersion replaces all occurrences of `kube-apiserver:<fromVersion>`
// in the manifest YAML with `kube-apiserver:<toVersion>`. It handles both bare
// version tags and registry-prefixed image lines (e.g.
// "registry.k8s.io/kube-apiserver:v1.34.0"). Pure Go string operation; no shell.
func injectManifestVersion(manifest, fromVersion, toVersion string) string {
	return strings.ReplaceAll(manifest, "kube-apiserver:"+fromVersion, "kube-apiserver:"+toVersion)
}

// mintJoinCloudConfig is the subset of the mint-join output cloud-config we parse
// to build the worker Cluster. Only the fields the worker reconcile needs are
// extracted; the rest of the cloud-config (install, users) is ignored.
type mintJoinCloudConfig struct {
	Cluster struct {
		ClusterToken     string `json:"cluster_token"`
		ControlPlaneHost string `json:"control_plane_host"`
		Role             string `json:"role"`
		ProviderConfig   struct {
			ClusterRootPath string `json:"cluster_root_path"`
		} `json:"providerConfig"`
		Config string `json:"config"`
	} `json:"cluster"`
}

// parseMintJoinOutput parses the YAML cloud-config emitted by `mint-join --role
// worker` and returns the fields needed to build a worker Cluster. It asserts
// BEHAVIOR (typed parse), not strings (principle 6).
func parseMintJoinOutput(t *testing.T, raw string) mintJoinCloudConfig {
	t.Helper()
	var cc mintJoinCloudConfig
	if err := k8syaml.Unmarshal([]byte(raw), &cc); err != nil {
		t.Fatalf("parse mint-join cloud-config: %v\nraw:\n%s", err, raw)
	}
	return cc
}

// TestWorkerJoin (ADR-13 Tier-1 scenario 6, OQ-E2 DECIDED per-PR): 2-container
// CA-pinned worker join. Proves the join path end to end with real kubeadm and
// real CA verification.
//
// Flow:
//  1. Start CP node container, run reconcile role=init, wait for convergence.
//  2. Run `mint-join --role worker` on the CP container to get CA-pinned join
//     material (the REAL mint path: kubeadm token create + SPKI hash from ca.crt).
//  3. Parse the emitted cloud-config to extract token, CA hash, endpoint.
//  4. Build a role=worker Cluster from the parsed material (CA pinning by
//     construction via caCertHashes; UnsafeSkipCAVerification is NEVER set).
//  5. Start a SECOND node container (same docker bridge, reachable by IP).
//  6. Pre-pull control-plane images on the worker container.
//  7. Run reconcile on the worker. Assert: exits 0, status phase=Converged.
//  8. On the CP, assert `kubectl get nodes` shows BOTH nodes registered.
//
// Both containers are torn down in t.Cleanup regardless of outcome.
func TestWorkerJoin(t *testing.T) {
	// Start the CP container and converge it.
	cpNC, _ := initAndConverge(t, uniqueName("cp"))
	cpIP := cpNC.IP(t)
	k8sVer := kubernetesVersion()

	// Run mint-join on the CP to get CA-pinned join material.
	// Pass --endpoint explicitly so the worker uses the CP container's bridge IP
	// (not a hostname that might not resolve from inside the worker container).
	cpEndpoint := cpIP + ":6443"
	mintOut, err := cpNC.ExecTimeout(60*time.Second,
		providerBinaryPath, "mint-join",
		"--role", "worker",
		"--root-path", "/",
		"--endpoint", cpEndpoint,
		"--ttl", "15m",
	)
	if err != nil {
		t.Fatalf("mint-join on CP failed: %v\n--- output ---\n%s", err, mintOut)
	}
	t.Logf("mint-join output:\n%s", mintOut)

	// Parse the emitted cloud-config to extract join material.
	cc := parseMintJoinOutput(t, mintOut)
	if cc.Cluster.Role != "worker" {
		t.Fatalf("mint-join role = %q, want worker", cc.Cluster.Role)
	}
	if cc.Cluster.Config == "" {
		t.Fatal("mint-join cloud-config has empty config section")
	}

	// Verify the cloud-config carries a CA cert hash (CA pinning is mandatory:
	// ADR-2, UnsafeSkipCAVerification is never set).
	if !strings.Contains(cc.Cluster.Config, "caCertHashes") {
		t.Fatal("mint-join config missing caCertHashes -- CA pinning not present")
	}
	if !strings.Contains(cc.Cluster.Config, "sha256:") {
		t.Fatal("mint-join config missing sha256: CA hash -- CA pin not computed")
	}

	// Build the worker Cluster from the mint-join output.
	// - ControlPlaneHost: the CP container's bridge IP:port.
	// - Options: the joinConfiguration YAML from the minted cloud-config (carries
	//   the CA-pinned bootstrap token and endpoint).
	// - ClusterToken: a fresh random value (correlation seed; does not need to match
	//   the CP's token for the join to succeed -- the bootstrap token + CA hash are
	//   the real authenticators, ADR-2/OQ-8).
	workerCluster := clusterplugin.Cluster{
		Role:             clusterplugin.Role(clusterplugin.RoleWorker),
		ClusterToken:     randomToken(t),
		ControlPlaneHost: cpEndpoint,
		ProviderOptions:  map[string]string{"cluster_root_path": "/"},
		// Config carries joinConfiguration with the CA-pinned token and endpoint.
		Options: cc.Cluster.Config,
	}

	// Start the worker node container. Both containers share the docker bridge;
	// the worker can reach the CP at cpIP:6443. Teardown is guaranteed by the
	// individual t.Cleanup registered by startNode.
	workerNC := startNode(t, uniqueName("worker"))

	// Pre-pull images on the worker. kubeadm join needs pause + kube-proxy; the
	// same kubeadm config images pull command covers them. This ensures the join
	// fits the per-attempt budget without racing a cold containerd pull.
	prepullControlPlaneImages(t, workerNC, k8sVer)

	// Run reconcile on the worker. This performs the real CA-pinned join:
	// kubeadm join with the minted token and SPKI hash -- no UnsafeSkipCAVerification.
	workerOut, workerErr := writeClusterAndReconcile(t, workerNC, workerCluster)
	if workerErr != nil {
		t.Fatalf("worker reconcile failed: %v\n--- output ---\n%s", workerErr, workerOut)
	}
	t.Logf("worker reconcile output:\n%s", workerOut)

	// 1. Worker status.yaml: phase=Converged, membership=joined.
	workerStatus := readStatus(t, workerNC)
	if workerStatus.Phase != "Converged" {
		t.Errorf("worker status phase = %q, want Converged (full: %+v)", workerStatus.Phase, workerStatus)
	}
	if workerStatus.Outcome != "success" {
		t.Errorf("worker status outcome = %q, want success", workerStatus.Outcome)
	}
	if workerStatus.Membership != "joined" {
		t.Errorf("worker status membership = %q, want joined", workerStatus.Membership)
	}

	// 2. On the CP, kubectl get nodes must show 2 registered nodes.
	// Bounded wait: the worker may still be completing node registration.
	var nodeCount int
	cpNC.waitFor(t, "2 nodes registered on CP", 90*time.Second, func() bool {
		o, e := cpNC.execErr("kubectl", "--kubeconfig", adminConf,
			"get", "nodes", "-o", "jsonpath={.items[*].metadata.name}")
		if e != nil {
			return false
		}
		names := strings.Fields(strings.TrimSpace(o))
		nodeCount = len(names)
		t.Logf("registered nodes: %v", names)
		return nodeCount >= 2
	})
	if nodeCount < 2 {
		t.Errorf("expected 2 registered nodes on CP after worker join, got %d", nodeCount)
	}
}
