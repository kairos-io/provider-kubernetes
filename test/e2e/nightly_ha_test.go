//go:build e2e && nightly

package e2e

// nightly_ha_test.go is the Tier-2 (ADR-13 E4) multi-control-plane, stacked-etcd
// HA bring-up scenario. It proves the ADR-11 keystone end to end with real kubeadm:
// a second control-plane node joins an existing single-CP cluster, the cluster PKI
// is propagated via upload-certs + a freshly minted certificate key, both nodes
// carry the control-plane role label, and stacked etcd grows to two members.
//
// Gating: `//go:build e2e && nightly` -- compiled only under `-tags "e2e nightly"`.
//
// What it proves:
//   - The ADR-11 #3 CP-join keystone: `mint-join --role controlplane` mints a fresh
//     cert-key AND re-uploads certs atomically (MintJoinMaterial), so the emitted
//     cloud-config carries BOTH a controlPlane.certificateKey and caCertHashes. We
//     assert the minted material before joining.
//   - A real control-plane join: cp2 reconciles role=controlplane against cp1's
//     endpoint using the minted joinConfiguration; kubeadm join --control-plane
//     decrypts the uploaded PKI with the cert-key and stands up a second apiserver
//     + etcd member, all CA-pinned (never UnsafeSkipCAVerification).
//   - cp2 converges (status phase=Converged, membership=joined).
//   - Cluster-wide: `kubectl get nodes` shows 2 nodes BOTH carrying the
//     node-role.kubernetes.io/control-plane label.
//   - Stacked etcd has 2 members (asserted via etcdctl in the cp1 etcd static pod).
//
// Single-endpoint reality (ADR-11): controlPlaneEndpoint = cp1's own IP triggers
// the ADR-11 single-endpoint warning. That is fine for in-container bring-up -- we
// need no VIP and deliberately add no kube-vip (principle #3). The warning is
// expected and not asserted against.
//
// Every wait is bounded (#4099-1).

import (
	"strings"
	"testing"
	"time"

	"github.com/kairos-io/kairos-sdk/clusterplugin"
	k8syaml "sigs.k8s.io/yaml"
)

// cpJoinConvergeTimeout bounds the cp2 control-plane join + convergence checks.
const (
	cpNodesWaitTimeout = 3 * time.Minute
	etcdMembersTimeout = 3 * time.Minute
)

// initClusterHA builds a role=init Cluster that, unlike initCluster, sets an
// explicit controlPlaneEndpoint (= the node's own IP:6443). This makes the cluster
// HA-ready: kubeadm writes the endpoint into the kubeadm-config and the CA covers
// it, so a second control plane can join against it. The endpoint equals cp1's own
// IP, which fires the ADR-11 single-endpoint warning -- acceptable for in-container
// HA bring-up (no VIP needed).
func initClusterHA(ip, k8sVer, clusterToken string) clusterplugin.Cluster {
	return clusterplugin.Cluster{
		Role:             clusterplugin.Role(clusterplugin.RoleInit),
		ClusterToken:     clusterToken,
		ControlPlaneHost: ip,
		ProviderOptions:  map[string]string{"cluster_root_path": "/"},
		Options: strings.Join([]string{
			"clusterConfiguration:",
			"  kubernetesVersion: " + k8sVer,
			"  controlPlaneEndpoint: " + ip + ":6443",
			"  networking:",
			"    podSubnet: 10.244.0.0/16",
			"initConfiguration:",
			"  localAPIEndpoint:",
			"    advertiseAddress: " + ip,
		}, "\n") + "\n",
	}
}

// TestNightlyControlPlaneJoinHA (ADR-13 Tier-2): a 2-control-plane stacked-etcd HA
// bring-up. The 2-CP pass is the core assertion (an optional cp3 is NOT included
// here to protect the nightly time budget; see the ADR-13 nightly list which scopes
// HA to "2 control-plane containers").
func TestNightlyControlPlaneJoinHA(t *testing.T) {
	k8sVer := kubernetesVersion()

	// --- cp1: init with a stable controlPlaneEndpoint (its own IP). ---------
	cp1NC := startNode(t, uniqueName("ha-cp1"))
	cp1IP := cp1NC.IP(t)
	prepullControlPlaneImages(t, cp1NC, k8sVer)

	cp1Cluster := initClusterHA(cp1IP, k8sVer, randomToken(t))
	out, err := writeClusterAndReconcile(t, cp1NC, cp1Cluster)
	if err != nil {
		t.Fatalf("cp1 reconcile (init) failed: %v\n--- output ---\n%s", err, out)
	}
	t.Logf("cp1 reconcile (init) output:\n%s", out)

	cp1NC.waitFor(t, "cp1 apiserver /healthz ok", 120*time.Second, func() bool {
		o, e := cp1NC.execErr("kubectl", "--kubeconfig", adminConf, "get", "--raw", "/healthz")
		return e == nil && strings.TrimSpace(o) == "ok"
	})

	// --- cp2: start it now so we know its IP before minting (HA-2 needs the
	//     joining CP's own advertiseAddress baked into the minted config). -----
	cp2NC := startNode(t, uniqueName("ha-cp2"))
	cp2IP := cp2NC.IP(t)
	cpEndpoint := cp1IP + ":6443"

	// --- mint CP-join material on cp1 (the ADR-11 keystone). ----------------
	// --advertise-address = cp2's own IP (HA-2): the minting CP cannot know the
	// joiner's IP, so the operator supplies it. MintJoinMaterial pairs a fresh
	// cert-key with upload-certs atomically (ADR-11 #3), so the emitted config must
	// carry BOTH certificateKey and caCertHashes.
	mintOut, err := cp1NC.ExecTimeout(90*time.Second,
		providerBinaryPath, "mint-join",
		"--role", "controlplane",
		"--root-path", "/",
		"--endpoint", cpEndpoint,
		"--advertise-address", cp2IP,
		"--ttl", "30m",
	)
	if err != nil {
		t.Fatalf("mint-join --role controlplane on cp1 failed: %v\n--- output ---\n%s", err, mintOut)
	}
	t.Logf("mint-join (controlplane) output:\n%s", mintOut)

	cc := parseMintJoinOutput(t, mintOut)
	if cc.Cluster.Role != "controlplane" {
		t.Fatalf("minted role = %q, want controlplane", cc.Cluster.Role)
	}
	// ADR-11 keystone assertions: the CP-join cloud-config MUST carry the cert-key
	// (pairs with upload-certs) and the CA pin (caCertHashes). Without the cert-key
	// the cluster PKI cannot be decrypted by the joining CP; without the CA hash the
	// join would be unpinned (forbidden).
	if !strings.Contains(cc.Cluster.Config, "certificateKey:") {
		t.Fatalf("CP-join config missing certificateKey -- the ADR-11 upload-certs/cert-key keystone is not present:\n%s", cc.Cluster.Config)
	}
	if !strings.Contains(cc.Cluster.Config, "caCertHashes") || !strings.Contains(cc.Cluster.Config, "sha256:") {
		t.Fatalf("CP-join config missing caCertHashes/sha256 CA pin:\n%s", cc.Cluster.Config)
	}
	// HA-2: the operator-supplied advertiseAddress must be cp2's own IP, not the
	// FILL-IN placeholder.
	if !strings.Contains(cc.Cluster.Config, "advertiseAddress: \""+cp2IP+"\"") &&
		!strings.Contains(cc.Cluster.Config, "advertiseAddress: "+cp2IP) {
		t.Fatalf("CP-join config advertiseAddress is not cp2's IP %q (HA-2):\n%s", cp2IP, cc.Cluster.Config)
	}

	// --- cp2 reconcile as a control-plane join. -----------------------------
	prepullControlPlaneImages(t, cp2NC, k8sVer)

	cp2Cluster := clusterplugin.Cluster{
		Role:             clusterplugin.Role(clusterplugin.RoleControlPlane),
		ClusterToken:     randomToken(t),
		ControlPlaneHost: cpEndpoint,
		ProviderOptions:  map[string]string{"cluster_root_path": "/"},
		// The minted joinConfiguration carries the bootstrap token, CA pin,
		// certificateKey, and cp2's localAPIEndpoint.advertiseAddress.
		Options: cc.Cluster.Config,
	}
	cp2Out, cp2Err := writeClusterAndReconcile(t, cp2NC, cp2Cluster)
	if cp2Err != nil {
		t.Fatalf("cp2 reconcile (control-plane join) failed: %v\n--- output ---\n%s", cp2Err, cp2Out)
	}
	t.Logf("cp2 reconcile (control-plane join) output:\n%s", cp2Out)

	// 1. cp2 status: Converged + joined.
	cp2Status := readStatus(t, cp2NC)
	if cp2Status.Phase != "Converged" {
		t.Errorf("cp2 status phase = %q, want Converged (full: %+v)", cp2Status.Phase, cp2Status)
	}
	if cp2Status.Outcome != "success" {
		t.Errorf("cp2 status outcome = %q, want success", cp2Status.Outcome)
	}
	if cp2Status.Membership != "joined" {
		t.Errorf("cp2 status membership = %q, want joined", cp2Status.Membership)
	}
	if cp2Status.Role != "controlplane" {
		t.Errorf("cp2 status role = %q, want controlplane", cp2Status.Role)
	}

	// 2. Cluster-wide: 2 nodes, BOTH control-plane role-labeled. We read the full
	//    node list as JSON and count nodes carrying the
	//    node-role.kubernetes.io/control-plane label (assert typed behavior, not a
	//    log scrape). Bounded wait: cp2's node object + label race the join.
	var cpLabeled int
	cp1NC.waitFor(t, "2 control-plane-labeled nodes on cp1", cpNodesWaitTimeout, func() bool {
		cpLabeled = countControlPlaneNodes(t, cp1NC)
		return cpLabeled >= 2
	})
	if cpLabeled < 2 {
		t.Errorf("expected 2 control-plane nodes after cp2 join, got %d", cpLabeled)
	}

	// 3. Stacked etcd has 2 members. The most robust in-container check is to exec
	//    etcdctl INSIDE the cp1 etcd static pod (it ships etcdctl and the pod has
	//    the etcd PKI + endpoints mounted), via `kubectl -n kube-system exec
	//    etcd-<cp1>`. We discover the etcd pod name dynamically (it is
	//    etcd-<nodename>, and the nodename may be lowercased/derived).
	cp1NC.waitFor(t, "etcd has 2 members", etcdMembersTimeout, func() bool {
		return etcdMemberCount(t, cp1NC) >= 2
	})
}

// countControlPlaneNodes returns how many Nodes carry the
// node-role.kubernetes.io/control-plane label, read via kubectl get nodes -o json
// on the given (control-plane) container. Typed parse, not a string scrape.
func countControlPlaneNodes(t *testing.T, nc *nodeContainer) int {
	t.Helper()
	out, err := nc.execErr("kubectl", "--kubeconfig", adminConf, "get", "nodes", "-o", "json")
	if err != nil {
		return 0
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := k8syaml.Unmarshal([]byte(out), &list); err != nil {
		return 0
	}
	const cpLabel = "node-role.kubernetes.io/control-plane"
	n := 0
	for _, item := range list.Items {
		if _, ok := item.Metadata.Labels[cpLabel]; ok {
			n++
		}
	}
	t.Logf("control-plane-labeled nodes: %d of %d total", n, len(list.Items))
	return n
}

// etcdMemberCount returns the number of etcd members reported by `etcdctl member
// list` executed inside the cp1 etcd static pod. The etcd container bundles etcdctl
// and the etcd peer/server PKI under /etc/kubernetes/pki/etcd; we point etcdctl at
// them and at the local etcd https endpoint. Returns 0 on any error (the bounded
// caller retries). argv only -- no shell interpolation (ADR-1).
func etcdMemberCount(t *testing.T, nc *nodeContainer) int {
	t.Helper()
	// Discover the etcd pod name (etcd-<nodename>) in kube-system.
	podOut, err := nc.execErr("kubectl", "--kubeconfig", adminConf,
		"-n", "kube-system", "get", "pods",
		"-l", "component=etcd",
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		return 0
	}
	etcdPod := strings.TrimSpace(podOut)
	if etcdPod == "" {
		return 0
	}

	// `etcdctl member list -w simple` prints one CSV line per member; counting
	// non-empty lines yields the member count. We supply the static-pod cert paths
	// and the loopback client endpoint kubeadm configures.
	listOut, err := nc.execErr("kubectl", "--kubeconfig", adminConf,
		"-n", "kube-system", "exec", etcdPod, "--",
		"etcdctl",
		"--endpoints=https://127.0.0.1:2379",
		"--cacert=/etc/kubernetes/pki/etcd/ca.crt",
		"--cert=/etc/kubernetes/pki/etcd/server.crt",
		"--key=/etc/kubernetes/pki/etcd/server.key",
		"member", "list")
	if err != nil {
		t.Logf("etcdctl member list (pod %s) not ready yet: %v\n%s", etcdPod, err, listOut)
		return 0
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(listOut), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	t.Logf("etcd member list (%d members):\n%s", count, listOut)
	return count
}
