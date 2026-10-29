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
// kubelet version, and whether the local apiserver is serving. They exec kubectl
// or speak HTTP, and are intentionally best-effort (return ""/false on any
// error) so a probe failure never blocks reconcile. They are injectable via
// Options for hardware-free tests; these defaults run only on a real node.
//
// Whether the persistent partition is encrypted is NOT probed here: that moved
// to internal/etcdsnapshot, which answers it from sysfs/statfs rather than by
// exec'ing findmnt or lsblk.

// kubeconfigFor returns the kubeconfig the upgrade probes read the cluster with,
// under rootPath: kubelet.conf when it exists, else admin.conf. Empty if neither
// does, and an empty rootPath means "/" so the result is never relative to the
// provider's working directory.
//
// kubelet.conf is preferred for the same reason status.resolveKubeconfig prefers
// it (security Q2): both callers below are read-only, best-effort reads about
// this node, and kubelet.conf's system:node:<name> identity covers both --
// kubeadm binds its kubeadm:nodes-kubeadm-config Role to the system:nodes group,
// and the Node authorizer lets a node read its own Node object. admin.conf is
// system:masters, an unnecessary blast radius here, and it is also the file that
// ages out first: the kubelet rotates its own credential, nothing rotates
// admin.conf, so an expired admin.conf used to silence both probes on an
// otherwise healthy control plane.
//
// initFinishedViaKubectl (controlplanehealth.go) is deliberately NOT part of
// this: it reads Deployments and DaemonSets in kube-system, which system:nodes
// cannot do, so it keeps admin.conf.
func kubeconfigFor(rootPath string) string {
	if rootPath == "" {
		rootPath = "/"
	}
	for _, name := range []string{"kubelet.conf", "admin.conf"} {
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
//
// nodeName is initConfiguration.nodeRegistration.name, the name the provider
// hands kubeadm for both init and join, so it is the name the Node carries.
// Empty means the operator left it unset, which kubeadm fills with the
// lower-cased hostname. Asking for the hostname when a name was configured
// gets a NotFound, and the empty version that follows reads to
// reconcile.planUpgrade as "worker convergence unknown": the worker then
// silently never upgrades (security Finding D, the same resolution
// status.MakeNodeResolver applies to the Node-annotation sink).
func runningKubeletVersionViaKubectl(rootPath, nodeName string, r kubeadm.Runner) func(ctx context.Context) string {
	return func(ctx context.Context) string {
		kc := kubeconfigFor(rootPath)
		if kc == "" {
			return ""
		}
		node := nodeName
		if node == "" {
			host, err := os.Hostname()
			if err != nil || host == "" {
				return ""
			}
			node = strings.ToLower(host)
		}
		res, err := r.Run(ctx, "--kubeconfig", kc,
			"get", "node", node,
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
