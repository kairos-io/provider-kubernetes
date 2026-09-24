//go:build e2e && nightly

package e2e

// nightly_control_plane_test.go is the Tier-2 (ADR-13 E4) scenario for a
// control plane whose own apiserver is not serving while its kubelet is
// healthy: the case a live kubelet cannot reveal, and the one a failed
// control-plane join or a crashlooping apiserver or etcd leaves behind.
//
// It is nightly rather than per-PR because it has to sit through the
// provider's 3 minute grace period, which exists because after a reboot the
// apiserver is often still starting while reconcile runs. The per-PR tier
// covers the verdict decided from files (member_verdict.go), which never
// waits; the grace logic itself is unit-tested against a live TLS /healthz.
//
// What it proves, on a real single-node control plane:
//   - with the kube-apiserver static pod removed, a reconcile pass takes no
//     action, waits the grace period (not less: a healthy node that is still
//     booting must not be reported), and records Degraded with
//     reason=ControlPlaneUnhealthy, non-terminal, exiting 0;
//   - the pass is bounded: it ends shortly after the grace period;
//   - with the manifest back and the apiserver serving, the next pass reports
//     Converged without waiting.

import (
	"strings"
	"testing"
	"time"

	"github.com/kairos-io/provider-kubernetes/internal/hostexec"
)

const (
	apiserverManifest = "/etc/kubernetes/manifests/kube-apiserver.yaml"
	// providerAPIServerGrace mirrors internal/provider.controlPlaneGrace (the
	// harness depends only on the binary, so this is a copy; see provider.go).
	providerAPIServerGrace = 3 * time.Minute
)

func TestNightlyControlPlaneNotServingReportsDegraded(t *testing.T) {
	nc, _ := initAndConverge(t, uniqueName("nightly-cpdown"))
	// A member re-run takes no action, so the token is never used; it only has
	// to pass the provider's validation like any other.
	cluster := initCluster(nc.IP(t), kubernetesVersion(), randomToken(t))

	apiserverHealthy := func() bool {
		o, e := nc.execErr(hostexec.KubectlPath, "--kubeconfig", adminConf, "get", "--raw", "/healthz")
		return e == nil && strings.TrimSpace(o) == "ok"
	}

	// Park the apiserver manifest; the kubelet stops the static pod.
	manifest, err := nc.ReadFile(apiserverManifest)
	if err != nil || !strings.Contains(manifest, "kube-apiserver") {
		t.Fatalf("read %s: %v", apiserverManifest, err)
	}
	parked := true
	t.Cleanup(func() {
		if parked {
			if stderr, err := nc.writeContent(apiserverManifest, manifest); err != nil {
				t.Errorf("restore %s: %v\n%s", apiserverManifest, err, stderr)
			}
		}
	})
	if out, err := nc.execErr(binRm, "-f", apiserverManifest); err != nil {
		t.Fatalf("park %s: %v\n%s", apiserverManifest, err, out)
	}
	nc.waitFor(t, "apiserver stopped after its manifest was removed", 3*time.Minute, func() bool {
		return !apiserverHealthy()
	})

	start := time.Now()
	out, err := writeClusterAndReconcile(t, nc, cluster)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("a degraded member is not a failed pass, want exit 0: %v\n%s", err, out)
	}
	if !strings.Contains(out, "actions=[none]") {
		t.Errorf("reconcile planned an action on an existing member, want none:\n%s", out)
	}
	if elapsed < providerAPIServerGrace || elapsed > providerAPIServerGrace+2*time.Minute {
		t.Errorf("reconcile took %s, want the %s grace period and not much more", elapsed, providerAPIServerGrace)
	}
	st := readStatus(t, nc)
	if st.Phase != "Degraded" || st.Reason != "ControlPlaneUnhealthy" {
		t.Fatalf("with the apiserver down: phase=%q reason=%q, want Degraded/ControlPlaneUnhealthy\n%s", st.Phase, st.Reason, out)
	}
	if st.Terminal || st.Outcome != "failure" {
		t.Errorf("ControlPlaneUnhealthy: terminal=%t outcome=%q, want a non-terminal failure", st.Terminal, st.Outcome)
	}
	t.Logf("apiserver down: Degraded/ControlPlaneUnhealthy after %s", elapsed)

	// Put it back; once it serves again the verdict follows without waiting.
	nc.WriteFile(t, apiserverManifest, manifest, "0600")
	parked = false
	nc.waitFor(t, "apiserver serving again after its manifest was restored", 3*time.Minute, apiserverHealthy)
	out, err = writeClusterAndReconcile(t, nc, cluster)
	if err != nil {
		t.Fatalf("reconcile after restoring the apiserver: %v\n%s", err, out)
	}
	if st := readStatus(t, nc); st.Phase != "Converged" {
		t.Fatalf("after restoring the apiserver: phase=%q reason=%q, want Converged\n%s", st.Phase, st.Reason, out)
	}
	if strings.Contains(out, "waiting up to") {
		t.Errorf("waited for an apiserver that was already serving:\n%s", out)
	}
}
