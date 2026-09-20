package provider

import (
	"context"
	"crypto/tls"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadmconfig"
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
// the first control plane runs `upgrade apply`). Best-effort: it parses
// Result.Stdout only (never Stderr, ADR-1-A1) and returns "" on any error.
func clusterVersionViaKubectl(rootPath string, r kubeadm.Runner) func(ctx context.Context) string {
	return func(ctx context.Context) string {
		kc := kubeconfigFor(rootPath)
		if kc == "" {
			return ""
		}
		res, err := r.Run(ctx, "--kubeconfig", kc,
			"-n", "kube-system", "get", "configmap", "kubeadm-config",
			"-o", "jsonpath={.data.ClusterConfiguration}")
		if err != nil {
			return ""
		}
		if m := kubernetesVersionRe.FindStringSubmatch(res.Stdout); len(m) == 2 {
			return m[1]
		}
		return ""
	}
}

// runningKubeletVersionViaKubectl reads this node's RUNNING kubelet version from
// its Node object (status.nodeInfo.kubeletVersion). Best-effort: it parses
// Result.Stdout only (never Stderr, ADR-1-A1) and returns "" on any error.
func runningKubeletVersionViaKubectl(rootPath string, r kubeadm.Runner) func(ctx context.Context) string {
	return func(ctx context.Context) string {
		kc := kubeconfigFor(rootPath)
		if kc == "" {
			return ""
		}
		host, err := os.Hostname()
		if err != nil || host == "" {
			return ""
		}
		res, err := r.Run(ctx, "--kubeconfig", kc,
			"get", "node", strings.ToLower(host),
			"-o", "jsonpath={.status.nodeInfo.kubeletVersion}")
		if err != nil {
			return ""
		}
		return strings.TrimSpace(res.Stdout)
	}
}

// localAPIHealthyProbe reports whether the LOCAL apiserver answers /healthz
// (ADR-12-R1). Liveness only (InsecureSkipVerify); used to decide whether the
// kubelet config needs repair after an A/B image swap and to gate upgrade apply.
//
// bindPort is the cluster's localAPIEndpoint.bindPort, which kubeadm renders
// as the apiserver's --secure-port. It has to be threaded in: a cluster that
// pins a non-default port serves /healthz only there, and probing 6443 would
// report a healthy control plane as down on every pass.
func localAPIHealthyProbe(bindPort int32) func(ctx context.Context) bool {
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // liveness probe only
	}
	healthz := kubeadmconfig.LocalAPIHealthzURL(bindPort)
	return func(ctx context.Context) bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthz, nil)
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
