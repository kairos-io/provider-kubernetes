//go:build e2e

package e2e

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kairos-io/provider-kubernetes/internal/hostexec"
)

const (
	// lbProxyDir and lbProxyPath are where the load balancer stand-in is
	// copied in the node.
	lbProxyDir  = "/opt/e2e"
	lbProxyPath = lbProxyDir + "/lbproxy"
	// lbPort is the port the load balancer stand-in accepts on.
	lbPort = "7443"
	// lbUnit is the transient unit it runs as.
	lbUnit = "e2e-lbproxy"
)

// TestInitBehindLoadBalancerWithoutBackend runs the HA bring-up order kubeadm
// documents: the load balancer for the controlPlaneEndpoint is up first, in
// TCP mode, and has no backend until the first control plane exists. Such a
// load balancer accepts every connection and closes it when no backend
// answers. The provider used to treat any accepted TCP connection as a
// control plane already serving, so this first role: init was refused,
// terminally, before it could create the backend the load balancer waited for.
//
// The stand-in (testdata/lbproxy) listens on the node's IP at lbPort and
// relays to the node's apiserver at 6443 once it answers. The cluster's
// controlPlaneEndpoint is the stand-in, and it is accepting before reconcile
// starts. The init must converge after run-init: the provider decided no
// control plane answered while the stand-in accepted at the endpoint, which
// is the decision the TCP check got wrong. The stand-in's log must show
// connections it closed for want of a backend and later ones it relayed, so
// the endpoint was the stand-in on both sides of the init.
//
// A second, fresh node configured role: init against the same endpoint, now
// with a backend, must still be refused: the guard keeps working through the
// load balancer.
func TestInitBehindLoadBalancerWithoutBackend(t *testing.T) {
	proxyBin := buildLBProxy(t)

	nc := startNode(t, uniqueName("lb-init"))
	ip := nc.IP(t)
	k8sVer := kubernetesVersion()
	endpoint := net.JoinHostPort(ip, lbPort)
	startLBProxy(t, nc, proxyBin, endpoint, net.JoinHostPort(ip, "6443"))

	cluster := initCluster(ip, k8sVer, randomToken(t))
	cluster.ControlPlaneHost = endpoint

	prepullControlPlaneImages(t, nc, k8sVer)

	out, err := writeClusterAndReconcile(t, nc, cluster)
	t.Logf("reconcile (init behind the load balancer) output:\n%s", out)
	if err != nil {
		st := readStatus(t, nc)
		t.Fatalf("role: init behind a load balancer without a backend failed: %v (status phase=%q reason=%q)", err, st.Phase, st.Reason)
	}
	if st := readStatus(t, nc); st.Phase != "Converged" || st.LastAction != "run-init" {
		t.Fatalf("status phase=%q reason=%q lastAction=%q, want Converged after run-init", st.Phase, st.Reason, st.LastAction)
	}

	// The endpoint really is the load balancer: kubeadm recorded it, and the
	// stand-in turned connections away before the apiserver was up (the
	// provider's check among them, and the kubelet's first attempts) and
	// relayed afterwards.
	cpe := strings.TrimSpace(nc.Exec(hostexec.KubectlPath, "--kubeconfig", adminConf,
		"-n", "kube-system", "get", "configmap", "kubeadm-config", "-o", "jsonpath={.data.ClusterConfiguration}"))
	if !strings.Contains(cpe, "controlPlaneEndpoint: "+endpoint) {
		t.Errorf("kubeadm-config does not name the load balancer %s as controlPlaneEndpoint:\n%s", endpoint, cpe)
	}
	lbLog := lbProxyLog(t, nc)
	if !strings.Contains(lbLog, "no backend for") {
		t.Errorf("the load balancer stand-in never closed a connection for want of a backend:\n%s", lbLog)
	}
	if !strings.Contains(lbLog, "relaying") {
		t.Errorf("the load balancer stand-in never relayed a connection, so nothing went through the endpoint:\n%s", lbLog)
	}

	// The guard still refuses a stray role: init behind the same endpoint once
	// a control plane answers there. The kubectl read above went through the
	// stand-in to the apiserver, so it relays to a live one at this point.
	stray := startNode(t, uniqueName("lb-stray"))
	strayCluster := initCluster(stray.IP(t), k8sVer, randomToken(t))
	strayCluster.ControlPlaneHost = endpoint
	out, err = writeClusterAndReconcile(t, stray, strayCluster)
	t.Logf("reconcile (stray role: init behind the load balancer) output:\n%s", out)
	if err == nil {
		t.Fatal("a stray role: init behind a load balancer with a live control plane was not refused")
	}
	if st := readStatus(t, stray); st.Reason != "InitRefused" || !st.Terminal {
		t.Errorf("stray node status reason=%q terminal=%v, want InitRefused, terminal", st.Reason, st.Terminal)
	}
	if _, rerr := stray.ReadFile(adminConf); rerr == nil {
		t.Error("the stray node ran kubeadm init: it has an admin.conf")
	}
}

// buildLBProxy builds testdata/lbproxy as a static binary for the node
// container, which runs on this host's architecture.
func buildLBProxy(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "lbproxy")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", bin, "./testdata/lbproxy")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the load balancer stand-in: %v\n%s", err, out)
	}
	return bin
}

// startLBProxy copies the stand-in into nc and runs it as a transient unit,
// accepting on listen and relaying to backend, and waits until it accepts.
func startLBProxy(t *testing.T, nc *nodeContainer, bin, listen, backend string) {
	t.Helper()
	nc.Exec(binMkdir, "-p", lbProxyDir)
	docker(t, "cp", bin, nc.id+":"+lbProxyPath)
	nc.Exec(binChmod, "0755", lbProxyPath)
	nc.Exec(binSystemdRun, "--unit", lbUnit, "--property", "Type=exec", lbProxyPath, "-listen", listen, "-backend", backend)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("load balancer stand-in log:\n%s", lbProxyLog(t, nc))
		}
		_, _ = nc.execErr(hostexec.SystemctlPath, "stop", lbUnit)
	})
	nc.waitFor(t, "load balancer stand-in listening", 30*time.Second, func() bool {
		return strings.Contains(lbProxyLog(t, nc), "listening on "+listen)
	})
}

func lbProxyLog(t *testing.T, nc *nodeContainer) string {
	t.Helper()
	out, _ := nc.execErr(binJournalctl, "--no-pager", "-o", "cat", "-u", lbUnit)
	return out
}
