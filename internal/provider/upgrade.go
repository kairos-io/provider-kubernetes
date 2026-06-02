package provider

import (
	"context"
	"crypto/tls"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// This file holds the production probes the upgrade path (ADR-12 U6) wires into
// the prober and executor: the cluster's current version, this node's running
// kubelet version, and whether the persistent partition is encrypted. They exec
// kubectl/lsblk/findmnt and are intentionally best-effort (return ""/false on any
// error) so a probe failure never blocks reconcile. They are injectable via
// Options for hardware-free tests; these defaults run only on a real node.

// kubeconfigFor returns the kubeconfig to use for cluster reads under rootPath:
// admin.conf on a control plane, else kubelet.conf. Empty if neither exists.
func kubeconfigFor(rootPath string) string {
	for _, name := range []string{"admin.conf", "kubelet.conf"} {
		p := filepath.Join(rootPath, "etc", "kubernetes", name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

var kubernetesVersionRe = regexp.MustCompile(`kubernetesVersion:\s*(v[0-9]+\.[0-9]+\.[0-9]+[^\s"']*)`)

// clusterVersionViaKubectl reads the cluster's current Kubernetes version from the
// kube-system/kubeadm-config ConfigMap (the authoritative source; it flips when
// the first control plane runs `upgrade apply`). Best-effort.
func clusterVersionViaKubectl(rootPath string) func(ctx context.Context) string {
	return func(ctx context.Context) string {
		kc := kubeconfigFor(rootPath)
		if kc == "" {
			return ""
		}
		out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kc,
			"-n", "kube-system", "get", "configmap", "kubeadm-config",
			"-o", "jsonpath={.data.ClusterConfiguration}").CombinedOutput()
		if err != nil {
			return ""
		}
		if m := kubernetesVersionRe.FindStringSubmatch(string(out)); len(m) == 2 {
			return m[1]
		}
		return ""
	}
}

// runningKubeletVersionViaKubectl reads this node's RUNNING kubelet version from
// its Node object (status.nodeInfo.kubeletVersion). Best-effort.
func runningKubeletVersionViaKubectl(rootPath string) func(ctx context.Context) string {
	return func(ctx context.Context) string {
		kc := kubeconfigFor(rootPath)
		if kc == "" {
			return ""
		}
		host, err := os.Hostname()
		if err != nil || host == "" {
			return ""
		}
		out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kc,
			"get", "node", strings.ToLower(host),
			"-o", "jsonpath={.status.nodeInfo.kubeletVersion}").CombinedOutput()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
}

// localAPIHealthyProbe reports whether the LOCAL apiserver answers /healthz
// (ADR-12-R1). Liveness only (InsecureSkipVerify); used to decide whether the
// kubelet config needs repair after an A/B image swap and to gate upgrade apply.
func localAPIHealthyProbe() func(ctx context.Context) bool {
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // liveness probe only
	}
	return func(ctx context.Context) bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://127.0.0.1:6443/healthz", nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode == http.StatusOK
	}
}

// encryptionConfirmedDefault reports whether dir's backing block device is part of
// an encrypted (LUKS/crypt) stack, by walking the device dependency tree. It is
// conservative: any error or an absent crypt layer yields false, so the etcd
// snapshot is refused rather than writing plaintext (ADR-12 U5 security). A false
// negative is safe (snapshot skipped); a false positive would be dangerous but
// requires the resolved source device to genuinely carry a dm-crypt node.
//
// Precondition (documented, like RunDir-must-be-tmpfs): the snapshot dir must be a
// DIRECT mount. Under an overlay/bind mount, `findmnt --target` may resolve to a
// backing device that differs from where bytes actually land, so the check is only
// trustworthy for a plain mount of the persistent partition.
func encryptionConfirmedDefault(dir string) func(ctx context.Context) bool {
	return func(ctx context.Context) bool {
		src, err := exec.CommandContext(ctx, "findmnt", "-no", "SOURCE", "--target", dir).CombinedOutput()
		dev := strings.TrimSpace(string(src))
		if err != nil || dev == "" {
			return false
		}
		// -s walks down the dependency tree to the physical devices; any "crypt"
		// node means the data at rest is encrypted.
		out, err := exec.CommandContext(ctx, "lsblk", "-rno", "TYPE", "-s", dev).CombinedOutput()
		if err != nil {
			return false
		}
		for _, line := range strings.Fields(string(out)) {
			if line == "crypt" {
				return true
			}
		}
		return false
	}
}
