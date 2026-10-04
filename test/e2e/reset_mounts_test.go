//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/kairos-io/provider-kubernetes/internal/hostexec"
)

// TestResetKeepsABusyVolumeMount reproduces the reset that used to delete a
// volume's data. kubeadm reset unmounts everything under /var/lib/kubelet
// first; when one unmount fails (a pod that will not let go of its volume, a
// hung network mount) it returns an error and deliberately leaves the
// directory alone. The provider then removed /var/lib/kubelet itself with a
// walk that crossed into the still-mounted volume and deleted what it held.
//
// Here a "volume" holding a sentinel file is bind-mounted under
// /var/lib/kubelet and kept busy by a transient unit whose working directory is
// inside it, so kubeadm's umount fails with EBUSY exactly as it would in the
// field. It is not placed under pods/<uid>/volumes: the kubelet's housekeeping
// deletes a pods/<uid> directory it does not know as soon as nothing is mounted
// in it, which races the mkdir and the mount. The reset must then:
//   - exit non-zero and name the mount it left in place;
//   - leave the volume's data untouched, at its source and through the mount;
//   - record Phase=Reset/ResetFailed;
//   - still remove the node's own state around it.
//
// Once the unit is stopped and the volume unmounted, the same reset run again
// succeeds, and the volume's source is still untouched.
func TestResetKeepsABusyVolumeMount(t *testing.T) {
	nc, _ := initAndConverge(t, uniqueName("reset-mount"))
	cluster := initCluster(nc.IP(t), kubernetesVersion(), randomToken(t))
	nc.WriteFile(t, clusterStatePath, serializeCluster(t, cluster), "0600")

	const (
		source     = "/var/e2e-volume-source"
		mountpoint = "/var/lib/kubelet/e2e-reset-volume/data"
		unit       = "e2e-busy-volume"
		precious   = "volume data that a reset must never delete\n"
	)
	nc.WriteFile(t, source+"/precious", precious, "0600")
	nc.Exec(binMkdir, "-p", mountpoint)
	nc.Exec(binMount, "--bind", source, mountpoint)
	busy, mounted := false, true
	t.Cleanup(func() {
		if busy {
			_, _ = nc.execErr(hostexec.SystemctlPath, "stop", unit)
		}
		if mounted {
			_, _ = nc.execErr(binUmount, mountpoint)
		}
	})
	// Type=exec: systemd-run returns only once the process runs, so its working
	// directory is inside the mount before the reset starts.
	nc.Exec(binSystemdRun, "--unit", unit, "--property", "Type=exec", "--property", "WorkingDirectory="+mountpoint, binSleep, "900")
	busy = true

	// 1. With the volume busy.
	out, err := nc.ExecTimeout(resetTimeout, providerBinaryPath, "reset", "--cluster-file="+clusterStatePath)
	t.Logf("reset with a busy volume (exit error %v):\n%s", err, out)
	if err == nil {
		t.Fatal("reset reported success with a volume still mounted under /var/lib/kubelet")
	}
	if !strings.Contains(out, "mount points left in place") || !strings.Contains(out, mountpoint) {
		t.Errorf("reset did not name the mount it left in place (%s)", mountpoint)
	}
	for _, p := range []string{source + "/precious", mountpoint + "/precious"} {
		if got, rerr := nc.ReadFile(p); rerr != nil || got != precious {
			t.Fatalf("the volume's data was deleted through the mount: %s = %q, %v", p, got, rerr)
		}
	}
	st := readStatus(t, nc)
	if st.Phase != "Reset" || st.Reason != "ResetFailed" {
		t.Errorf("status after the incomplete reset: phase=%q reason=%q, want Reset/ResetFailed", st.Phase, st.Reason)
	}
	// The status says how many and what to do; the paths are in the log above.
	for _, want := range []string{"1 mount point", "unmount them and run the reset again"} {
		if !strings.Contains(st.Message, want) {
			t.Errorf("status message %q does not carry %q", st.Message, want)
		}
	}
	for _, gone := range []string{"/etc/kubernetes/admin.conf", "/var/lib/kubelet/kubeadm-flags.env", "/var/lib/kubelet/config.yaml"} {
		if _, serr := nc.execErr(binTest, "-e", gone); serr == nil {
			t.Errorf("%s survived the reset; the node's own state around the kept mount must still go", gone)
		}
	}

	// 2. Released and unmounted: the same reset completes.
	nc.Exec(hostexec.SystemctlPath, "stop", unit)
	busy = false
	nc.Exec(binUmount, mountpoint)
	mounted = false
	out, err = nc.ExecTimeout(resetTimeout, providerBinaryPath, "reset", "--cluster-file="+clusterStatePath)
	t.Logf("reset after unmounting (exit error %v):\n%s", err, out)
	if err != nil {
		t.Fatalf("reset failed with nothing left mounted: %v", err)
	}
	if st := readStatus(t, nc); st.Phase != "Reset" || st.Reason != "ResetOK" {
		t.Errorf("status after the completed reset: phase=%q reason=%q, want Reset/ResetOK", st.Phase, st.Reason)
	}
	if got, rerr := nc.ReadFile(source + "/precious"); rerr != nil || got != precious {
		t.Fatalf("the volume's source changed: %q, %v", got, rerr)
	}
}
