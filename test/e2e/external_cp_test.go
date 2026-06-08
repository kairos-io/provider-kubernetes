//go:build e2e

package e2e

// external_cp_test.go proves design principle 7 / issue #4099-5: a node joins a
// control plane the provider did NOT bootstrap, using operator-supplied trust
// anchors.
//
// This is distinct from TestWorkerJoin (scenarios_test.go), where the CP is
// provider-init'd and the join material comes from `mint-join`. Here the CP is
// brought up by PLAIN `kubeadm init` -- the provider never runs on it, so it is a
// genuinely externally-managed control plane. The operator then hands the worker
// only:
//   - the CP's CA certificate in PEM form (cluster.ca_certs), and
//   - a bootstrap token (kubeadm token create), and the endpoint.
//
// Crucially the worker config carries NO caCertHashes. The provider must DERIVE
// the SPKI pin from the operator-supplied CA PEM (resolveCACertHashes, OQ-9/ADR-10)
// and join with CA pinning -- never UnsafeSkipCAVerification. A successful join
// with only the PEM supplied is the proof that the derive-and-pin path works
// end-to-end against a real CA, not just in unit tests.

import (
	"strings"
	"testing"
	"time"

	"github.com/kairos-io/kairos-sdk/clusterplugin"
)

// externalKubeadmInit brings up a single-node control plane WITHOUT the provider:
// it runs `kubeadm init` directly in the container with plain flags. The provider
// binary is never invoked here, so the resulting CP is "externally managed" from
// the provider's point of view. Waits for the apiserver to be healthy.
func externalKubeadmInit(t *testing.T, nc *nodeContainer, ip, k8sVer string) {
	t.Helper()
	out, err := nc.ExecTimeout(reconcileTimeout,
		"kubeadm", "init",
		"--apiserver-advertise-address", ip,
		"--pod-network-cidr", "10.244.0.0/16",
		"--kubernetes-version", k8sVer,
		"--cri-socket", "unix:///run/containerd/containerd.sock",
	)
	if err != nil {
		t.Fatalf("plain kubeadm init (external CP): %v\n--- output ---\n%s", err, out)
	}
	nc.waitFor(t, "external CP apiserver /healthz ok", 120*time.Second, func() bool {
		o, e := nc.execErr("kubectl", "--kubeconfig", adminConf, "get", "--raw", "/healthz")
		return e == nil && strings.TrimSpace(o) == "ok"
	})
}

// TestExternallyManagedControlPlaneJoin (design principle 7 / #4099-5): a worker
// joins a control plane the provider did not create, with the provider deriving
// the CA pin from an operator-supplied CA PEM.
//
// Flow:
//  1. Start the CP container; bring up the CP with PLAIN kubeadm init (no provider).
//  2. Operator extracts trust anchors out-of-band: the CA cert PEM
//     (/etc/kubernetes/pki/ca.crt) and a fresh bootstrap token (kubeadm token
//     create). NO SPKI hash is computed by the operator.
//  3. Build a role=worker Cluster supplying ONLY ca_certs (the PEM) + the token +
//     endpoint -- no caCertHashes. The provider must derive the pin from the PEM.
//  4. Start a second container (worker), reconcile via the provider.
//  5. Assert: worker converges/joined; the externally-managed CP shows 2 nodes;
//     and the CP never had the provider run on it (no provider status file) --
//     confirming it is genuinely externally managed.
func TestExternallyManagedControlPlaneJoin(t *testing.T) {
	k8sVer := kubernetesVersion()

	// 1. Externally-managed CP: plain kubeadm init, provider never involved.
	cpNC := startNode(t, uniqueName("ext-cp"))
	cpIP := cpNC.IP(t)
	prepullControlPlaneImages(t, cpNC, k8sVer)
	externalKubeadmInit(t, cpNC, cpIP, k8sVer)

	// Sanity: the CP is externally managed -- the provider never ran on it, so it
	// has no provider status file. This distinguishes the scenario from TestWorkerJoin.
	if _, statErr := cpNC.execErr("test", "-e", statusRunPath); statErr == nil {
		t.Fatalf("external CP unexpectedly has a provider status file at %s; it should be externally managed (provider never ran on it)", statusRunPath)
	}

	// 2. Operator extracts trust anchors out-of-band.
	caPEM, err := cpNC.ReadFile("/etc/kubernetes/pki/ca.crt")
	if err != nil {
		t.Fatalf("read external CP ca.crt: %v\n%s", err, caPEM)
	}
	if !strings.Contains(caPEM, "BEGIN CERTIFICATE") {
		t.Fatalf("external CP ca.crt does not look like a PEM cert:\n%s", caPEM)
	}
	tokenOut, err := cpNC.ExecTimeout(60*time.Second, "kubeadm", "token", "create")
	if err != nil {
		t.Fatalf("kubeadm token create on external CP: %v\n%s", err, tokenOut)
	}
	token := strings.TrimSpace(tokenOut)
	if token == "" {
		t.Fatal("kubeadm token create returned an empty token")
	}
	cpEndpoint := cpIP + ":6443"
	t.Logf("external CP up at %s; operator extracted CA PEM (%d bytes) + bootstrap token", cpEndpoint, len(caPEM))

	// 3. Worker Cluster: supply ONLY the CA PEM (ca_certs) + token + endpoint.
	//    No caCertHashes -- the provider derives the SPKI pin from the PEM
	//    (resolveCACertHashes). CA pinning is mandatory; UnsafeSkipCAVerification is
	//    never set.
	workerCluster := clusterplugin.Cluster{
		Role:             clusterplugin.Role(clusterplugin.RoleWorker),
		ClusterToken:     randomToken(t),
		ControlPlaneHost: cpEndpoint,
		CACerts:          []string{caPEM},
		ProviderOptions:  map[string]string{"cluster_root_path": "/"},
		Options: strings.Join([]string{
			"joinConfiguration:",
			"  discovery:",
			"    bootstrapToken:",
			"      token: " + token,
			"      apiServerEndpoint: " + cpEndpoint,
			// Deliberately NO caCertHashes: the provider must derive the pin from
			// ca_certs (the operator-supplied PEM above).
		}, "\n") + "\n",
	}

	// 4. Worker container joins the external CP via the provider.
	workerNC := startNode(t, uniqueName("ext-worker"))
	prepullControlPlaneImages(t, workerNC, k8sVer)

	workerOut, workerErr := writeClusterAndReconcile(t, workerNC, workerCluster)
	if workerErr != nil {
		t.Fatalf("worker reconcile joining external CP failed: %v\n--- output ---\n%s", workerErr, workerOut)
	}
	t.Logf("worker reconcile output:\n%s", workerOut)

	// 5a. Worker status: converged + joined.
	ws := readStatus(t, workerNC)
	if ws.Phase != "Converged" {
		t.Errorf("worker status phase = %q, want Converged (full: %+v)", ws.Phase, ws)
	}
	if ws.Outcome != "success" {
		t.Errorf("worker status outcome = %q, want success", ws.Outcome)
	}
	if ws.Membership != "joined" {
		t.Errorf("worker status membership = %q, want joined", ws.Membership)
	}

	// 5b. The externally-managed CP now sees 2 registered nodes.
	var nodeCount int
	cpNC.waitFor(t, "2 nodes registered on external CP", 90*time.Second, func() bool {
		o, e := cpNC.execErr("kubectl", "--kubeconfig", adminConf,
			"get", "nodes", "-o", "jsonpath={.items[*].metadata.name}")
		if e != nil {
			return false
		}
		names := strings.Fields(strings.TrimSpace(o))
		nodeCount = len(names)
		t.Logf("nodes registered on external CP: %v", names)
		return nodeCount >= 2
	})
	if nodeCount < 2 {
		t.Errorf("expected 2 registered nodes on the external CP after join, got %d", nodeCount)
	}
}
