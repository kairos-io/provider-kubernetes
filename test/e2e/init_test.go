//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kairos-io/kairos-sdk/clusterplugin"

	"github.com/kairos-io/provider-kubernetes/internal/etcdsnapshot"
	"github.com/kairos-io/provider-kubernetes/internal/hostexec"
)

// kubernetesVersion is the version the bundled kubeadm provides (the e2e node
// image FROM-derives a kairos-kubeadm built at this version). `make e2e` exports
// E2E_KUBERNETES_VERSION; default to the v1 local target.
func kubernetesVersion() string {
	if v := os.Getenv("E2E_KUBERNETES_VERSION"); v != "" {
		return v
	}
	return "v1.37.0"
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
//
// It also carries E-B7, the ADR-1-A1 security gate that the provider runs every
// tool by absolute path with a closed environment (see exec_hardening.go): image
// invariants first, then PATH-first shadow shims and a hostile KUBERC around
// reconcile, and a hostile CONTAINERD_ADDRESS around import-images.
func TestSingleNodeInitConverges(t *testing.T) {
	nc := startNode(t, uniqueName("init"))

	// E-B7 (a): the binaries the provider runs by absolute path are trustworthy
	// on this image. Before anything else, so no later step can have altered them.
	assertExecPathImageInvariants(t, nc)

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

	// E-B7 (b): from here on, running kubeadm, kubectl, kubelet, systemctl or ctr
	// by name lands in a shim that records the exec and fails. The prepull above
	// is not under test, so it runs before the shims exist. From this point every
	// harness exec inside the container uses an absolute path too.
	plantShadowShims(t, nc)

	// E-B7 (c): reconcile runs with KUBERC pointing at a malformed kuberc. kubectl
	// fails every command under it, so step 5 (annotations the provider writes
	// through kubectl) passes only if the provider's kubectl environment is closed.
	hostileReconcileEnv := plantMalformedKuberc(t, nc)

	// E-B7 (c2): a valid kuberc at /.kube/kuberc, the path kubectl reads with no
	// HOME from working directory /, stops every annotate from landing. Reconcile
	// runs from / like the kairos-agent service, so step 5 also passes only if
	// the provider's kubectl keeps KUBECTL_KUBERC=false and KUBERC=off.
	plantDefaultLocationKuberc(t, nc)

	// Run reconcile -- the real binary, real kubeadm, mirroring the yip stage.
	out, err := writeClusterAndReconcileOpts(t, nc, cluster,
		execOptions{Env: hostileReconcileEnv, Workdir: reconcileWorkdir})
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
		o, e := nc.execErr(hostexec.KubectlPath, "--kubeconfig", adminConf,
			"get", "--raw", "/healthz")
		return e == nil && strings.TrimSpace(o) == "ok"
	})

	// 3. node registered (bounded wait for kubelet to register with the apiserver).
	var nodeName string
	nc.waitFor(t, "node registered", 90*time.Second, func() bool {
		o, e := nc.execErr(hostexec.KubectlPath, "--kubeconfig", adminConf,
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
	//    Under E-B7 (c) and (c2) this also proves the provider's kubectl ignored
	//    both the malformed KUBERC set on the reconcile exec and the hostile
	//    /.kube/kuberc.
	var annotations map[string]string
	nc.waitFor(t, "own-Node provider annotations present (if missing, also suspect E-B7 (c)/(c2): the provider's kubectl inherited KUBERC or read /.kube/kuberc)", 60*time.Second, func() bool {
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

	// 6. F-ETCDCTL (ADR-12-A1) proof: the image's shipped /usr/bin/etcdctl, the
	// provider's EXACT production argv/env (etcdsnapshot.SaveCommand -- the same
	// builder the default save path uses), and kubeadm's real etcd PKI can
	// together take a real snapshot of a real, running etcd, with an env -i
	// (empty) environment exactly as production runs it. The fail-closed
	// encryption gate and persistence to COS_PERSISTENT are VM-only (see
	// docs/upgrades.md "etcd backups"); the plaintext write here is acceptable
	// ONLY because /tmp is the container's throwaway tmpfs holding disposable
	// single-node PKI. Nothing that bypasses the gate may be added to non-test
	// code -- this proves only the save mechanics.
	const snapshotDest = "/tmp/e2e-etcd-snapshot.db"
	argv, env := etcdsnapshot.SaveCommand(etcdsnapshot.EtcdctlPath, "/", snapshotDest)
	saveArgv := append(append([]string{binEnv, "-i"}, env...), argv...)
	if out, err := nc.ExecTimeout(3*time.Minute, saveArgv...); err != nil {
		t.Fatalf("etcd snapshot save failed: %v\n--- output ---\n%s", err, out)
	}

	if mode := nc.FileMode(t, snapshotDest); mode != "600" {
		t.Errorf("etcd snapshot file mode = %q, want 600", mode)
	}

	// Only the status JSON is ever logged/parsed here -- never the snapshot
	// contents (which include cluster Secrets and PKI key material).
	statusOut := nc.Exec("/usr/bin/etcdutl", "snapshot", "status", snapshotDest, "-w", "json")
	t.Logf("etcdutl snapshot status: %s", statusOut)
	var status struct {
		Hash      uint32 `json:"hash"`
		Revision  int64  `json:"revision"`
		TotalKey  int64  `json:"totalKey"`
		TotalSize int64  `json:"totalSize"`
		Version   string `json:"version"`
	}
	if err := json.Unmarshal([]byte(statusOut), &status); err != nil {
		t.Fatalf("parse etcdutl snapshot status json: %v\nraw: %s", err, statusOut)
	}
	if status.Revision <= 0 {
		t.Errorf("snapshot revision = %d, want > 0", status.Revision)
	}
	if status.TotalKey <= 0 {
		t.Errorf("snapshot totalKey = %d, want > 0", status.TotalKey)
	}

	nc.Exec(binRm, "-f", snapshotDest)

	// 7. E-B7 (d): a real air-gap import with the ctr shim still planted and
	//    CONTAINERD_ADDRESS pointing at a socket that does not exist. It succeeds
	//    only if the provider runs /usr/bin/ctr by absolute path with an empty
	//    environment (ADR-1-A1): a PATH lookup hits the shim, an inherited address
	//    cannot reach containerd. The expected count is the images.lock entry count
	//    (the importer imports exactly those, never a *.tar glob), and the summary
	//    must say outcome=success; re-importing images the boot-time oneshot already
	//    imported is idempotent.
	lock := readBundleLock(t, nc, "E-B7 (d)")
	importRun := runImportImages(t, nc, "E-B7 (d) import-images under a hostile CONTAINERD_ADDRESS", importImagesTimeout,
		execOptions{Env: []string{"CONTAINERD_ADDRESS=" + hostileContainerdAddress}})
	t.Logf("import-images output:\n%s", importRun.Out)
	if v := importRunViolations(importRun.importOutput, lock, wantImport{Outcome: "success"}); importRun.Code != 0 || len(v) > 0 {
		t.Errorf("E-B7 (d): import-images under CONTAINERD_ADDRESS=%s exited %d, want 0 with all %d images.lock entries imported: %s",
			hostileContainerdAddress, importRun.Code, len(lock.Images), strings.Join(v, "; "))
	}
	ctrHits := 0
	for _, hit := range parseShadowHits(readShadowHits(t, nc)) {
		if hit.Tool == "ctr" {
			ctrHits++
			t.Errorf("E-B7: ctr was run by name through PATH (ADR-1-A1): %s", hit.Line)
		}
	}
	t.Logf("E-B7 (d): import-images exited %d under CONTAINERD_ADDRESS=%s: %s; images.lock entries: %d; ctr shim hits: %d",
		importRun.Code, hostileContainerdAddress, importRun.Summary, len(lock.Images), ctrHits)

	// 8. E-B7 (b): across reconcile, the assertions, and the import, nothing ran a
	//    shadowed tool by name. A hit names the tool, its argv, and its parent
	//    (agent-provider-, kubeadm, ...), which points at the exec to fix.
	if raw := readShadowHits(t, nc); strings.TrimSpace(raw) != "" {
		t.Errorf("E-B7: %d exec(s) resolved a tool by name through PATH instead of its absolute path (ADR-1-A1); shadow-shim hits in %s:\n%s",
			len(parseShadowHits(raw)), shadowHitsPath, raw)
	} else {
		t.Logf("E-B7 (b): shadow marker %s is empty after reconcile, the assertions and import-images", shadowHitsPath)
	}

	// 9. ADR-19 U1 (F-UNITPATH), the daemon side of the same property: containerd,
	//    the kubelet and everything they start resolve names against the read-only
	//    image (see daemon_exec_path.go). It runs LAST, after E-B7's verdict, for
	//    two reasons: it restarts containerd, the import unit and the kubelet, which
	//    no earlier step should have to tolerate; and it needs a converged node, so
	//    that real shims, real projected-token mounts and the kubelet's iptables
	//    work are what the shadow binaries are measured against.
	assertDaemonExecPath(t, nc)
}
