//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/kairos-io/kairos-sdk/clusterplugin"
	provideryaml "gopkg.in/yaml.v3"
	k8syaml "sigs.k8s.io/yaml"
)

// reconcileTimeout bounds a single reconcile exec. It sits just above the
// provider's own DefaultBudget Total (12m) so the exec never masks the provider's
// internal budget; it is the never-hang backstop for the docker exec call.
const reconcileTimeout = 14 * time.Minute

// imagePullTimeout bounds the control-plane image pre-pull.
const imagePullTimeout = 5 * time.Minute

// These mirror the production contract constants exactly (kept as local copies so
// the e2e package depends only on the public binary, not on internal/*):
//   - providerBinaryPath: where the Kairos image installs the binary.
//   - clusterStatePath:   the 0600 tmpfs path Provider() serializes the Cluster
//     to and `reconcile --cluster-file` reads by default.
//   - statusRunPath:      the 0640 tmpfs status doc every reconcile writes.
//
// If any of these change in internal/provider or internal/status, this harness
// must change too -- that coupling is intentional (we test the real contract).
const (
	providerBinaryPath = "/system/providers/agent-provider-kubernetes"
	clusterStatePath   = "/run/provider-kubernetes/cluster.json"
	statusRunPath      = "/run/provider-kubernetes/status.yaml"
	adminConf          = "/etc/kubernetes/admin.conf"
)

// serializeCluster marshals a Cluster the SAME way Provider() does (yaml.Marshal
// via gopkg.in/yaml.v3) so the reconcile subcommand parses it byte-for-byte as in
// production. (Provider() chose YAML over JSON because clusterplugin.Role's JSON
// unmarshal is broken in older kairos-sdk; we match that decision.)
func serializeCluster(t *testing.T, c clusterplugin.Cluster) string {
	t.Helper()
	b, err := provideryaml.Marshal(c)
	if err != nil {
		t.Fatalf("serialize cluster: %v", err)
	}
	return string(b)
}

// writeClusterAndReconcile mirrors the production yip stage: write the serialized
// Cluster to the 0600 tmpfs path, then run the real
// `agent-provider-kubernetes reconcile --cluster-file=<path>` via docker exec.
// Returns the combined reconcile output and the exec error (nil on exit 0).
func writeClusterAndReconcile(t *testing.T, nc *nodeContainer, c clusterplugin.Cluster) (string, error) {
	t.Helper()
	nc.WriteFile(t, clusterStatePath, serializeCluster(t, c), "0600")
	return nc.ExecTimeout(reconcileTimeout, providerBinaryPath, "reconcile", "--cluster-file="+clusterStatePath)
}

// prepullControlPlaneImages warms the kubeadm control-plane images before
// reconcile so `kubeadm init` does not race the per-attempt budget on a cold
// container pulling apiserver/etcd/etc. A real Kairos image typically pre-bundles
// or pre-pulls these; doing it here makes the e2e deterministic without changing
// the reconcile path. Bounded; failures fail the test loudly.
func prepullControlPlaneImages(t *testing.T, nc *nodeContainer, k8sVersion string) {
	t.Helper()
	out, err := nc.ExecTimeout(imagePullTimeout,
		"kubeadm", "config", "images", "pull",
		"--kubernetes-version", k8sVersion,
		"--cri-socket", "unix:///run/containerd/containerd.sock")
	if err != nil {
		t.Fatalf("pre-pull control-plane images: %v\n%s", err, out)
	}
}

// statusDoc is the subset of the provider status.yaml the scenarios assert on. We
// parse the typed YAML (sigs.k8s.io/yaml -> json tags) rather than grepping
// strings, asserting BEHAVIOR (design principle 6 / ADR-13).
type statusDoc struct {
	APIVersion string `json:"apiVersion"`
	Phase      string `json:"phase"`
	Role       string `json:"role"`
	Membership string `json:"membership"`
	Outcome    string `json:"outcome"`
	Reason     string `json:"reason"`
	Terminal   bool   `json:"terminal"`
	LastAction string `json:"lastAction"`
	Version    string `json:"version"`
}

// readStatus reads and parses /run/provider-kubernetes/status.yaml from inside
// the container.
func readStatus(t *testing.T, nc *nodeContainer) statusDoc {
	t.Helper()
	raw, err := nc.ReadFile(statusRunPath)
	if err != nil {
		t.Fatalf("read status.yaml: %v\n%s", err, raw)
	}
	var s statusDoc
	if err := k8syaml.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("parse status.yaml: %v\nraw:\n%s", err, raw)
	}
	return s
}

// kubectl runs kubectl inside the container against the admin kubeconfig kubeadm
// init wrote, returning trimmed stdout. argv only.
func kubectl(t *testing.T, nc *nodeContainer, args ...string) string {
	t.Helper()
	full := append([]string{"kubectl", "--kubeconfig", adminConf}, args...)
	return strings.TrimSpace(nc.Exec(full...))
}

// nodeAnnotations returns the provider-kubernetes.kairos.io/* annotations present
// on the given node, as a parsed map (suffix -> value). It reads the full
// annotation map as YAML and filters to our prefix.
func nodeAnnotations(t *testing.T, nc *nodeContainer, nodeName string) map[string]string {
	t.Helper()
	out := kubectl(t, nc, "get", "node", nodeName, "-o", "json")
	var node struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := k8syaml.Unmarshal([]byte(out), &node); err != nil {
		t.Fatalf("parse node json: %v", err)
	}
	const prefix = "provider-kubernetes.kairos.io/"
	got := map[string]string{}
	for k, v := range node.Metadata.Annotations {
		if strings.HasPrefix(k, prefix) {
			got[strings.TrimPrefix(k, prefix)] = v
		}
	}
	return got
}
