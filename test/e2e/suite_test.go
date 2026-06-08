//go:build e2e

package e2e

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain is the suite entry point. It runs a one-shot leaked-container sweep
// BEFORE any test so containers leaked by a prior crashed run are removed without
// accidentally removing containers that belong to active tests within this run.
//
// After sweeping, it waits at two levels:
//  1. Docker layer: polls until no labeled containers remain in `docker ps -aq`.
//  2. Kernel layer: polls until each swept container's cgroup scope disappears
//     from /sys/fs/cgroup/system.slice/. cgroup scope removal is the reliable
//     signal that the kernel has reaped all subprocesses (kubeadm, kubelet, etcd,
//     kube-apiserver, containerd) from the prior run. Without this, a new
//     container started immediately after the sweep may have its processes killed
//     by stale cgroup teardown, manifesting as exit 137 on the first reconcile.
//
// sweepLeaked was previously called inside startNode, which caused it to remove
// live containers started by earlier tests in the same suite invocation (e.g. the
// CP container in TestInitClobberRefusal was swept when startNode was called for
// the fresh second container). The sweep is now here: one shot before m.Run().
func TestMain(m *testing.M) {
	// Collect the full container IDs before sweeping so we can poll their cgroups.
	var sweptIDs []string
	if out, err := dockerErr("ps", "-aq", "--filter", "label="+labelKey+"="+labelValue); err == nil {
		for _, id := range strings.Fields(out) {
			// Resolve to full 64-char ID for the cgroup path.
			if full, err := dockerErr("inspect", "--format", "{{.Id}}", id); err == nil {
				sweptIDs = append(sweptIDs, strings.TrimSpace(full))
			}
			_, _ = dockerErr("rm", "-fv", id)
		}
	}

	// Phase 1: wait until Docker no longer lists any labeled containers.
	dockerDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(dockerDeadline) {
		out, err := dockerErr("ps", "-aq", "--filter", "label="+labelKey+"="+labelValue)
		if err != nil || strings.TrimSpace(out) == "" {
			break
		}
		time.Sleep(1 * time.Second)
	}

	// Phase 2: wait until each swept container's kernel cgroup scope disappears.
	// Bounded at 90 s total; any still-present cgroup after the deadline is
	// logged to stderr (best-effort; we do not abort the run).
	if len(sweptIDs) > 0 {
		cgroupDeadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(cgroupDeadline) {
			allGone := true
			for _, id := range sweptIDs {
				p := "/sys/fs/cgroup/system.slice/docker-" + id + ".scope"
				if _, err := os.Stat(p); err == nil {
					allGone = false
					break
				}
			}
			if allGone {
				break
			}
			time.Sleep(2 * time.Second)
		}
	}

	os.Exit(m.Run())
}
