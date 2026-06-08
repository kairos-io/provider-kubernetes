//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kairos-io/kairos-sdk/clusterplugin"
)

// kubernetesVersion is the version the bundled kubeadm provides (the e2e node
// image FROM-derives a kairos-kubeadm built at this version). `make e2e` exports
// E2E_KUBERNETES_VERSION; default to the v1 local target.
func kubernetesVersion() string {
	if v := os.Getenv("E2E_KUBERNETES_VERSION"); v != "" {
		return v
	}
	return "v1.34.0"
}

// randomToken returns a 256-bit hex string -- well above the 128-bit cluster_token
// entropy recommendation, so no token warning fires. Ephemeral per run (never a
// real long-lived secret), satisfying the ADR-13 secret-handling constraint.
func randomToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generate cluster_token: %v", err)
	}
	return hex.EncodeToString(b)
}

// the 7 closed-enum own-Node annotation suffixes the provider writes (Layer-2).
var wantAnnotationSuffixes = []string{
	"phase", "outcome", "reason", "terminal",
	"last-action", "updated-at", "version",
}

// TestSingleNodeInitConverges proves the ADR-13 mechanism end to end: the REAL
// `agent-provider-kubernetes reconcile` drives REAL kubeadm to init a single-node
// control plane inside a privileged systemd node container, the apiserver comes
// up healthy, the node registers, the provider writes a Converged status (0640),
// and the own-Node provider-kubernetes.kairos.io/* annotations are present.
//
// The node will be NotReady (no CNI installed) -- that is expected and correct;
// we assert on convergence + registration, NOT Ready.
func TestSingleNodeInitConverges(t *testing.T) {
	nc := startNode(t, uniqueName("init"))
	ip := nc.IP(t)

	// Build the Cluster exactly as Kairos would hand it to the provider:
	//   role: init
	//   control_plane_host: the node's own IP (single-node container; this is the
	//     stable in-container address kubeadm advertises and the apiserver binds).
	//   cluster_token: high-entropy ephemeral value.
	//   cluster_root_path: "/" (kubeadm writes /etc/kubernetes/admin.conf there).
	// The kubernetesVersion pin (in cluster.config) matches the bundled kubeadm so
	// ADR-3 Resolve is a no-op and we exercise the explicit-pin path.
	cluster := clusterplugin.Cluster{
		Role:             clusterplugin.Role(clusterplugin.RoleInit),
		ClusterToken:     randomToken(t),
		ControlPlaneHost: ip,
		ProviderOptions:  map[string]string{"cluster_root_path": "/"},
		Options: strings.Join([]string{
			"clusterConfiguration:",
			"  kubernetesVersion: " + kubernetesVersion(),
			"  networking:",
			"    podSubnet: 10.244.0.0/16",
			"initConfiguration:",
			"  localAPIEndpoint:",
			"    advertiseAddress: " + ip,
		}, "\n") + "\n",
	}

	// Warm the control-plane images so init fits the per-attempt budget on a cold
	// container (does not touch the reconcile path).
	prepullControlPlaneImages(t, nc, kubernetesVersion())

	// Run reconcile -- the real binary, real kubeadm, mirroring the yip stage.
	out, err := writeClusterAndReconcile(t, nc, cluster)
	if err != nil {
		t.Fatalf("reconcile failed: %v\n--- reconcile output ---\n%s", err, out)
	}
	t.Logf("reconcile output:\n%s", out)

	// 1. kubeadm init succeeded -> admin.conf exists.
	if _, err := nc.ReadFile(adminConf); err != nil {
		t.Fatalf("admin.conf missing after init: %v", err)
	}

	// 2. apiserver /healthz OK (bounded wait; apiserver may still be settling).
	nc.waitFor(t, "apiserver /healthz ok", 90*time.Second, func() bool {
		o, e := nc.execErr("kubectl", "--kubeconfig", adminConf,
			"get", "--raw", "/healthz")
		return e == nil && strings.TrimSpace(o) == "ok"
	})

	// 3. node registered (bounded wait for kubelet to register with the apiserver).
	var nodeName string
	nc.waitFor(t, "node registered", 90*time.Second, func() bool {
		o, e := nc.execErr("kubectl", "--kubeconfig", adminConf,
			"get", "nodes", "-o", "jsonpath={.items[*].metadata.name}")
		if e != nil {
			return false
		}
		nodeName = strings.TrimSpace(o)
		return nodeName != ""
	})
	t.Logf("registered node: %q (will be NotReady -- no CNI, expected)", nodeName)

	// 4. status.yaml: phase=Converged, outcome=success, mode 0640.
	st := readStatus(t, nc)
	if st.Phase != "Converged" {
		t.Errorf("status phase = %q, want Converged (full status: %+v)", st.Phase, st)
	}
	if st.Outcome != "success" {
		t.Errorf("status outcome = %q, want success", st.Outcome)
	}
	if st.Role != "init" {
		t.Errorf("status role = %q, want init", st.Role)
	}
	if st.APIVersion != "provider-kubernetes.kairos.io/v1" {
		t.Errorf("status apiVersion = %q, want provider-kubernetes.kairos.io/v1", st.APIVersion)
	}
	if mode := nc.FileMode(t, statusRunPath); mode != "640" {
		t.Errorf("status.yaml mode = %q, want 640", mode)
	}

	// 5. the own-Node provider-kubernetes.kairos.io/* annotations are present
	//    (Layer-2 proof; the NodeAnnotationSink ran post-membership).
	//    Bounded retry: the annotate kubectl call races the just-registered node.
	var annotations map[string]string
	nc.waitFor(t, "own-Node provider annotations present", 60*time.Second, func() bool {
		annotations = nodeAnnotations(t, nc, nodeName)
		return len(annotations) >= len(wantAnnotationSuffixes)
	})
	for _, suffix := range wantAnnotationSuffixes {
		if _, ok := annotations[suffix]; !ok {
			t.Errorf("missing own-Node annotation provider-kubernetes.kairos.io/%s (got %v)", suffix, annotations)
		}
	}
	if got := annotations["phase"]; got != "Converged" {
		t.Errorf("annotation phase = %q, want Converged", got)
	}
	if got := annotations["outcome"]; got != "success" {
		t.Errorf("annotation outcome = %q, want success", got)
	}
}
