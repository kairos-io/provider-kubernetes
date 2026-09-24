package provider

import (
	"context"
	"time"
)

// This file holds the grace period Run gives a control plane's own apiserver
// before reporting it as not serving (reconcile.VerdictControlPlaneUnhealthy).
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
// stalled attempt cannot outlive the grace period.
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
		if probe(ctx) {
			return true
		}
	}
}
