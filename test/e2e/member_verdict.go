//go:build e2e

package e2e

// member_verdict.go covers the verdict an already-initialized control plane
// reports when reconcile runs again, which is what every boot after the first
// does, against a real `kubeadm init` on each supported minor.
//
//	(a) kubeadm's kubelet-finalize phase left /etc/kubernetes/kubelet.conf
//	    referencing the rotated client certificate, with none embedded, and
//	    that certificate exists. The provider's InitIncomplete verdict rests on
//	    exactly this behavior of `kubeadm init`, so it is asserted per minor: if
//	    a kubeadm release stops doing it, every healthy init node would report
//	    InitIncomplete, and this fails first.
//	(b) a second reconcile on the healthy node takes no action, reports
//	    Converged, and does not wait for an apiserver that is already serving.
//	(c) with kubelet.conf put back in the form kubeadm writes before
//	    kubelet-finalize, while the addon phase's objects exist: Converged. That
//	    is kubeadm's own recovery for a failed kubelet client-certificate
//	    rotation with its last step skipped, and the cluster check dismisses it;
//	(c2) the same kubelet.conf with CoreDNS and kube-proxy removed, which is
//	    what an init that stopped before kubelet-finalize leaves: Degraded/
//	    InitIncomplete, promptly, with no action taken. The objects are restored;
//	(d) with the finalized file restored, Converged again.
//
// Neither kubelet.conf nor the rotated certificate file is ever printed: the
// certificate file holds the kubelet's private key, and (c) puts it inline.

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kairos-io/kairos-sdk/clusterplugin"

	"github.com/kairos-io/provider-kubernetes/internal/hostexec"
)

const (
	kubeletConfPath   = "/etc/kubernetes/kubelet.conf"
	kubeletRotatedPEM = "/var/lib/kubelet/pki/kubelet-client-current.pem"

	// memberVerdictPrompt bounds a reconcile pass that must not wait for the
	// apiserver. It is well under the provider's 3 minute grace period, so a
	// pass that waited it out fails here rather than passing slowly.
	memberVerdictPrompt = 90 * time.Second
)

func assertMemberVerdicts(t *testing.T, nc *nodeContainer, cluster clusterplugin.Cluster) {
	t.Helper()

	// (a) kubeadm's own output, before the provider judges it.
	finalized, err := nc.ReadFile(kubeletConfPath)
	if err != nil {
		t.Fatalf("read %s: %v", kubeletConfPath, err)
	}
	if strings.Contains(finalized, "client-certificate-data:") {
		t.Fatalf("%s still embeds its client certificate after a successful kubeadm init: kubelet-finalize did not rewrite it, "+
			"so the provider's InitIncomplete check would report every healthy init node as broken", kubeletConfPath)
	}
	for _, key := range []string{"client-certificate: ", "client-key: "} {
		if !strings.Contains(finalized, key+kubeletRotatedPEM) {
			t.Fatalf("%s has no %q line for %s: kubeadm's kubelet-finalize rewrite changed shape, re-check the provider's InitIncomplete check",
				kubeletConfPath, strings.TrimSpace(key), kubeletRotatedPEM)
		}
	}
	if out, err := nc.execErr(binStat, "-L", "-c", "%F", kubeletRotatedPEM); err != nil || strings.TrimSpace(out) != "regular file" {
		t.Fatalf("%s is not a readable regular file (stat: %q, %v): the InitIncomplete check could never decide", kubeletRotatedPEM, strings.TrimSpace(out), err)
	}
	t.Logf("member verdict: kubeadm left %s finalized (references %s, embeds no certificate)", kubeletConfPath, kubeletRotatedPEM)

	// (b) the healthy re-run.
	st, out, elapsed := reconcileAgain(t, nc, cluster, "healthy control plane")
	if st.Phase != "Converged" || st.Reason != "" {
		t.Fatalf("second reconcile on the healthy init node: phase=%q reason=%q, want Converged with no reason\n%s", st.Phase, st.Reason, out)
	}
	if strings.Contains(out, "waiting up to") {
		t.Errorf("second reconcile waited for an apiserver that was already serving (%s):\n%s", elapsed, out)
	}

	// (c) the state a failed init leaves behind.
	pem, err := nc.ReadFile(kubeletRotatedPEM)
	if err != nil || !strings.Contains(pem, "BEGIN CERTIFICATE") {
		t.Fatalf("read %s: %v (content withheld: it holds a private key)", kubeletRotatedPEM, err)
	}
	inline := base64.StdEncoding.EncodeToString([]byte(pem))
	unfinalized := strings.Replace(finalized, "client-certificate: "+kubeletRotatedPEM, "client-certificate-data: "+inline, 1)
	unfinalized = strings.Replace(unfinalized, "client-key: "+kubeletRotatedPEM, "client-key-data: "+inline, 1)
	restored := false
	t.Cleanup(func() {
		if !restored {
			if stderr, err := nc.writeContent(kubeletConfPath, finalized); err != nil {
				t.Errorf("restore %s: %v\n%s", kubeletConfPath, err, stderr)
			}
		}
	})
	nc.WriteFile(t, kubeletConfPath, unfinalized, "0600")

	// (c) With every addon still in place this is kubeadm's documented
	// recovery for a failed kubelet client-certificate rotation with its last
	// step skipped, on a cluster whose init finished: the cluster check must
	// dismiss what the files alone suggest.
	st, out, _ = reconcileAgain(t, nc, cluster, "re-issued kubelet.conf, addons present")
	if st.Phase != "Converged" || st.Reason != "" {
		t.Fatalf("re-issued kubelet.conf on a finished init: phase=%q reason=%q, want Converged\n%s", st.Phase, st.Reason, out)
	}
	if !strings.Contains(out, "kubelet.conf was re-issued later") {
		t.Errorf("the pass did not report that the cluster check dismissed the finding:\n%s", out)
	}

	// (c2) Without the addon phase's objects, the cluster confirms what the
	// files say: the state an init that stopped before kubelet-finalize
	// leaves. The objects are put back right after.
	restoreAddons := removeAddons(t, nc)
	st, out, _ = reconcileAgain(t, nc, cluster, "unfinalized kubelet.conf, no addons")
	if st.Phase != "Degraded" || st.Reason != "InitIncomplete" {
		t.Fatalf("reconcile with an unfinalized kubelet.conf and no addons: phase=%q reason=%q, want Degraded/InitIncomplete\n%s", st.Phase, st.Reason, out)
	}
	if st.Terminal || st.Outcome != "failure" {
		t.Errorf("InitIncomplete: terminal=%t outcome=%q, want a non-terminal failure", st.Terminal, st.Outcome)
	}
	if strings.Contains(out, "waiting up to") {
		t.Errorf("the apiserver was serving, but the pass waited for it:\n%s", out)
	}
	restoreAddons()

	// (d) back to the file kubeadm left.
	nc.WriteFile(t, kubeletConfPath, finalized, "0600")
	restored = true
	st, out, _ = reconcileAgain(t, nc, cluster, "restored kubelet.conf")
	if st.Phase != "Converged" || st.Reason != "" {
		t.Fatalf("reconcile after restoring kubelet.conf: phase=%q reason=%q, want Converged\n%s", st.Phase, st.Reason, out)
	}
}

// reconcileAgain runs one more reconcile pass on a node that is already a
// member, requires it to exit 0, take no action and finish within
// memberVerdictPrompt, and returns the status it recorded.
func reconcileAgain(t *testing.T, nc *nodeContainer, cluster clusterplugin.Cluster, what string) (statusDoc, string, time.Duration) {
	t.Helper()
	start := time.Now()
	out, err := writeClusterAndReconcile(t, nc, cluster)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("reconcile (%s) failed: %v\n%s", what, err, out)
	}
	if !strings.Contains(out, "actions=[none]") {
		t.Errorf("reconcile (%s) planned an action on an existing member, want none:\n%s", what, out)
	}
	if elapsed > memberVerdictPrompt {
		t.Errorf("reconcile (%s) took %s, want under %s", what, elapsed, memberVerdictPrompt)
	}
	st := readStatus(t, nc)
	t.Logf("member verdict (%s): phase=%q reason=%q after %s", what, st.Phase, st.Reason, elapsed.Round(time.Millisecond))
	return st, out, elapsed
}

// removeAddons deletes the objects kubeadm init's addon phase creates (the
// CoreDNS Deployment and the kube-proxy DaemonSet) and returns a function that
// creates them again from what the apiserver held. A cleanup restores them if
// the test stops in between.
func removeAddons(t *testing.T, nc *nodeContainer) func() {
	t.Helper()
	// Read through the helper that keeps stdout apart: the kube-proxy
	// DaemonSet carries the proxy variables kubeadm copies from the host,
	// which can hold credentials, so a failure prints stderr only.
	saved, stderr, err := nc.ExecStdoutTimeout(dockerTimeout, hostexec.KubectlPath, "--kubeconfig", adminConf,
		"-n", "kube-system", "get", "deployments.apps/coredns", "daemonsets.apps/kube-proxy", "-o", "json")
	if err != nil {
		t.Fatalf("read the addon objects: %v\n%s", err, stderr)
	}
	var list map[string]any
	if err := json.Unmarshal([]byte(saved), &list); err != nil {
		t.Fatalf("parse the saved addon objects: %v", err)
	}
	items, _ := list["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("expected the CoreDNS Deployment and the kube-proxy DaemonSet, got %d objects", len(items))
	}
	for _, it := range items {
		obj, _ := it.(map[string]any)
		delete(obj, "status")
		if md, ok := obj["metadata"].(map[string]any); ok {
			for _, k := range []string{"resourceVersion", "uid", "creationTimestamp", "generation", "managedFields", "selfLink"} {
				delete(md, k)
			}
		}
	}
	manifest, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("re-encode the saved addon objects: %v", err)
	}
	kubectl(t, nc, "-n", "kube-system", "delete", "deployments.apps/coredns", "daemonsets.apps/kube-proxy", "--wait=true", "--timeout=60s")

	done := false
	restore := func() {
		if done {
			return
		}
		done = true
		if out, err := nc.ExecInput(string(manifest), hostexec.KubectlPath, "--kubeconfig", adminConf, "create", "-f", "-"); err != nil {
			t.Errorf("restore the addon objects: %v\n%s", err, out)
		}
	}
	t.Cleanup(restore)
	return restore
}
