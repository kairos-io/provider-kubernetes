package provider

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
	"github.com/kairos-io/provider-kubernetes/internal/kubeadmconfig"
)

// This file holds the grace period Run gives a control plane's own apiserver
// before reporting it as not serving (reconcile.VerdictControlPlaneUnhealthy),
// and the cluster check that confirms or dismisses an apparently unfinished
// init (reconcile.VerdictInitIncomplete), which needs that apiserver too.
//
// The kubelet starts the static pods only once it is up itself, so after a
// reboot a healthy control plane's apiserver routinely comes up while the
// reconcile pass is already running. On the 2026-09-18 trusted-boot VM run
// the pass ran from 12:01:36 to 12:01:38 and the kube-apiserver container was
// created at about 12:01:37: a single probe would have reported that healthy
// node as degraded for the whole boot, since nothing re-evaluates the status
// until the next one.

// controlPlaneGrace bounds how long one reconcile pass waits for the local
// apiserver. Three minutes is several times what a healthy node needs and
// stays under the 4m kubeadm itself allows a new control plane to come up.
// Only a control plane that really is down pays it, once per boot. A variable
// rather than a constant only so tests can shorten it.
var controlPlaneGrace = 3 * time.Minute

// controlPlanePoll is the interval between /healthz attempts during the grace
// period; the same 3s the post-repair local-API wait uses (ADR-12-R1).
var controlPlanePoll = 3 * time.Second

// awaitLocalAPIServer polls probe every poll interval until it reports
// healthy, grace has elapsed, or ctx is done, and reports whether it saw the
// apiserver healthy. The caller has just seen it down, so the first attempt
// waits one interval. Every probe call carries the bounded context, so a
// stalled attempt cannot outlive the grace period, and no attempt starts once
// that context is done: when a tick and the deadline are ready together, the
// select may pick either.
func awaitLocalAPIServer(ctx context.Context, probe func(context.Context) bool, grace, poll time.Duration) bool {
	if probe == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
		if ctx.Err() != nil {
			return false
		}
		if probe(ctx) {
			return true
		}
	}
}

// initFinishedTimeout bounds the one cluster read that decides whether an
// apparently unfinished init did finish.
const initFinishedTimeout = 15 * time.Second

// addonObjects are what kubeadm init's addon phase creates in kube-system:
// CoreDNS's Deployment and kube-proxy's DaemonSet, as `kubectl get -o name`
// prints them (kubeadmconstants.CoreDNSDeploymentName and KubeProxy). addon
// is the only phase after kubelet-finalize that leaves anything behind
// (cmd/kubeadm/app/cmd/init.go phase order, release-1.35 to 1.37), so either
// one existing proves init ran past kubelet-finalize.
var addonObjects = []string{"deployment.apps/coredns", "daemonset.apps/kube-proxy"}

// initFinishedViaKubectl is the production default for
// Options.InitFinishedProbe. It asks THIS node's apiserver, not the
// controlPlaneEndpoint admin.conf names, so a load balancer that is down
// cannot keep the finding alive: --server is the loopback URL and the serving
// certificate is still verified against the cluster CA, under a name kubeadm
// always includes. admin.conf, because the kubelet's own identity cannot read
// kube-system Deployments; the call is a read, argv carries only the
// kubeconfig path, and only stdout is parsed (ADR-1-A1).
//
// It reports (true, nil) when the apiserver lists one of the addon objects,
// and (false, nil) when it answered and lists neither. When kubectl fails
// (expired admin.conf credentials, forbidden, refused, timeout, a certificate
// that does not verify) the cluster could not be asked: the error says so
// with the exit code only, never the argv or kubectl's output, and the
// InitIncomplete finding stands unconfirmed.
//
// The resources are named with their API group so a custom resource that
// shares a short name cannot answer in their place.
func initFinishedViaKubectl(rootPath string, bindPort int32, r kubeadm.Runner) func(ctx context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		ctx, cancel := context.WithTimeout(ctx, initFinishedTimeout)
		defer cancel()
		res, err := r.Run(ctx,
			"--kubeconfig", filepath.Join(rootPath, "etc", "kubernetes", "admin.conf"),
			"--server", kubeadmconfig.LocalAPIServerURL(bindPort),
			"--tls-server-name", kubeadmconfig.LocalAPIServerTLSName,
			"--request-timeout", "10s",
			"-n", "kube-system", "get", "deployments.apps/coredns", "daemonsets.apps/kube-proxy",
			"-o", "name", "--ignore-not-found")
		if err != nil {
			return false, fmt.Errorf("kubectl exit %d", res.ExitCode)
		}
		for _, line := range strings.Split(res.Stdout, "\n") {
			if slices.Contains(addonObjects, strings.TrimSpace(line)) {
				return true, nil
			}
		}
		return false, nil
	}
}
