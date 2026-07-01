//go:build e2e

// Package e2e contains the ADR-13 end-to-end harness. Every file is behind the
// `e2e` build tag, so `go test ./...` (and `make test`) never compiles or runs
// it; only `go test -tags e2e ./test/e2e/...` (`make e2e`) does.
//
// The harness drives the REAL provider exactly as production does: it starts a
// kind-style privileged systemd "node container" (the Dockerfile.e2e-node
// image), serializes a clusterplugin.Cluster to the tmpfs path Provider() uses,
// and runs the real `agent-provider-kubernetes reconcile` via docker exec, then
// asserts by reading state back from inside the container. No parallel code
// path: the same binary, same subcommand, same serialized-Cluster contract the
// yip stage invokes at boot.
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// labelKey/labelValue tag every container the harness starts so a label-scoped
// sweep at suite start can reap leaked containers from a crashed prior run, and
// so teardown is unambiguous.
const (
	labelKey   = "provider-kubernetes-e2e"
	labelValue = "1"
)

// dockerTimeout bounds every individual docker CLI call so a wedged daemon can
// never hang the suite (design principle 4 / #4099-1).
const dockerTimeout = 90 * time.Second

// nodeContainer is a running privileged systemd node container.
type nodeContainer struct {
	t    *testing.T
	id   string // full container ID
	name string
}

// docker runs the docker CLI with argv only (never sh -c), bounded by
// dockerTimeout, and returns combined stdout. It fails the test on error with
// the captured output so failures surface clearly.
func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := dockerErr(args...)
	if err != nil {
		t.Fatalf("docker %s: %v\noutput:\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// dockerErr is the error-returning form used where a non-zero exit is expected
// or handled by the caller (e.g. teardown best-effort, readiness polling).
func dockerErr(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// nodeImage resolves the node image to run. E2E_NODE_IMAGE is set by `make e2e`;
// it must be present so the harness never silently pulls or builds the wrong
// image (image provenance: our own built image only).
func nodeImage(t *testing.T) string {
	t.Helper()
	img := os.Getenv("E2E_NODE_IMAGE")
	if img == "" {
		t.Skip("E2E_NODE_IMAGE not set; run via `make e2e` (it builds and sets the node image)")
	}
	if out, err := dockerErr("image", "inspect", img); err != nil {
		t.Fatalf("node image %q not present locally (%v); run `make e2e-node-image`\n%s", img, err, out)
	}
	return img
}

// startNode starts the node container with the kind-style privileged systemd
// flags and registers guaranteed teardown via t.Cleanup (runs even on failure).
// It then waits, bounded, for systemd + containerd to be ready before returning.
//
// sweepLeaked is NOT called here: it is called once at suite start by TestMain
// (in suite_test.go), so a single sweep covers the entire run without accidentally
// removing live containers started by earlier tests in the same suite invocation.
func startNode(t *testing.T, name string) *nodeContainer {
	t.Helper()
	img := nodeImage(t)

	// The kind node-container run flags. argv only, no shell.
	//   --privileged           : kubelet/containerd/etcd need real cgroup +
	//                            mount + netns control.
	//   --cgroupns=private     : the node gets its own cgroup namespace so
	//                            systemd manages a clean unified (v2) hierarchy.
	//   --tmpfs /run --tmpfs /tmp : writable tmpfs the way kind sets up; /run is
	//                            also where the provider writes cluster.json +
	//                            status.yaml (tmpfs, wiped on stop).
	//   -v <anon>:/var/lib/... : fresh, isolated, writable volumes for the
	//                            stateful dirs (containerd, kubelet, etcd).
	//   --label                : for the leak sweep + teardown.
	// Host kernel exposure (kind does the same on its node containers): mount the
	// host /lib/modules and /boot read-only so kubeadm's SystemVerification
	// preflight can READ the kernel config (/boot/config-<uname>) WITHOUT us
	// weakening the provider's preflight (no --ignore-preflight-errors in
	// production code). These are read-only and host-provided, not container
	// state. On a host that does not expose them, the mounts are skipped and the
	// test reports the resulting preflight error rather than papering over it.
	args := []string{
		"run", "-d",
		"--name", name,
		"--label", labelKey + "=" + labelValue,
		"--privileged",
		"--cgroupns=private",
		"--tmpfs", "/run",
		"--tmpfs", "/tmp",
		"-v", "/var/lib/containerd",
		"-v", "/var/lib/kubelet",
		"-v", "/var/lib/etcd",
	}
	args = append(args, hostKernelMounts()...)
	args = append(args,
		"--entrypoint", "/sbin/init",
		img,
	)
	out := docker(t, args...)
	id := strings.TrimSpace(out)
	nc := &nodeContainer{t: t, id: id, name: name}

	// Guaranteed teardown: dump a short diagnostic tail on failure, then remove
	// the container regardless of outcome. Registered immediately after the
	// container exists so a readiness-wait failure still cleans up.
	t.Cleanup(func() {
		if t.Failed() {
			if logs, err := dockerErr("logs", "--tail", "60", id); err == nil {
				t.Logf("=== container %s systemd log tail ===\n%s", name, logs)
			}
		}
		_, _ = dockerErr("rm", "-fv", id)
	})

	nc.waitReady(t)
	return nc
}

// hostKernelMounts returns read-only bind-mount flags for the host kernel
// modules and /boot when they exist, mirroring kind's node-container mounts.
// They let kubeadm's SystemVerification read the kernel config without the
// provider relaxing any preflight check. Absent paths are skipped.
func hostKernelMounts() []string {
	var m []string
	for _, p := range []string{"/lib/modules", "/boot"} {
		if _, err := os.Stat(p); err == nil {
			m = append(m, "-v", p+":"+p+":ro")
		}
	}
	return m
}

// waitReady blocks (bounded) until systemd is PID 1, containerd admin API
// answers, and the containerd CRI plugin is ready to handle CRI requests.
// A three-stage readiness check ensures kubeadm (which uses the CRI API) can
// start immediately without racing a not-yet-fully-initialized CRI plugin.
func (nc *nodeContainer) waitReady(t *testing.T) {
	t.Helper()
	// systemd must be PID 1.
	nc.waitFor(t, "systemd is PID 1", 60*time.Second, func() bool {
		out, err := nc.execErr("cat", "/proc/1/comm")
		return err == nil && strings.TrimSpace(out) == "systemd"
	})
	// containerd's admin API socket must answer (proves containerd is up).
	nc.waitFor(t, "containerd admin API ready", 90*time.Second, func() bool {
		_, err := nc.execErr("ctr", "version")
		if err != nil {
			// ctr may not be on PATH; fall back to socket existence.
			_, err = nc.execErr("test", "-S", "/run/containerd/containerd.sock")
		}
		return err == nil
	})
	// The containerd CRI plugin initializes after the admin API. Probe with
	// crictl info (CRI ImageService) so we know CRI is ready before kubeadm
	// starts. kubeadm config images pull uses the CRI API, not the admin API.
	nc.waitFor(t, "containerd CRI plugin ready", 120*time.Second, func() bool {
		_, err := nc.execErr("crictl",
			"--runtime-endpoint", "unix:///run/containerd/containerd.sock",
			"info")
		return err == nil
	})
}

// waitFor polls cond until it returns true or the deadline elapses, failing the
// test with a clear message on timeout. Every wait in the harness is bounded.
func (nc *nodeContainer) waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out after %s waiting for: %s", timeout, what)
}

// Exec runs argv inside the container and fails the test on a non-zero exit.
func (nc *nodeContainer) Exec(args ...string) string {
	nc.t.Helper()
	out, err := nc.execErr(args...)
	if err != nil {
		nc.t.Fatalf("exec %s: %v\noutput:\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// execErr runs argv inside the container and returns (combinedOutput, err). argv
// only -- no shell -- so there is no string-interpolation surface (ADR-1).
func (nc *nodeContainer) execErr(args ...string) (string, error) {
	full := append([]string{"exec", nc.id}, args...)
	return dockerErr(full...)
}

// ExecTimeout runs argv inside the container with an explicit timeout, used for
// long-running operations (e.g. `kubeadm init` via reconcile) that exceed the
// default per-call dockerTimeout. Bounded so it can never hang.
func (nc *nodeContainer) ExecTimeout(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	full := append([]string{"exec", nc.id}, args...)
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", full...)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// ExecInput runs argv inside the container feeding stdin, used to drop files
// without a host bind mount or shell redirection.
func (nc *nodeContainer) ExecInput(stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()
	full := append([]string{"exec", "-i", nc.id}, args...)
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", full...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// WriteFile writes content to path inside the container with mode (octal string,
// e.g. "0600"). It uses `tee` over stdin (no shell, no interpolation of
// content) then chmod, mirroring how the yip File stage materializes the
// serialized Cluster at 0600.
func (nc *nodeContainer) WriteFile(t *testing.T, path, content, mode string) {
	t.Helper()
	dir := path[:strings.LastIndex(path, "/")]
	nc.Exec("mkdir", "-p", dir)
	if out, err := nc.ExecInput(content, "tee", path); err != nil {
		t.Fatalf("write %s: %v\n%s", path, err, out)
	}
	nc.Exec("chmod", mode, path)
}

// ReadFile reads a file from inside the container.
func (nc *nodeContainer) ReadFile(path string) (string, error) {
	return nc.execErr("cat", path)
}

// FileMode returns the octal permission bits of a file inside the container
// (e.g. "640"), via stat -c %a.
func (nc *nodeContainer) FileMode(t *testing.T, path string) string {
	t.Helper()
	return strings.TrimSpace(nc.Exec("stat", "-c", "%a", path))
}

// IP returns the container's primary IP address on the default bridge.
func (nc *nodeContainer) IP(t *testing.T) string {
	t.Helper()
	out := docker(t, "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", nc.id)
	ip := strings.TrimSpace(out)
	if ip == "" {
		t.Fatalf("container %s has no IP address", nc.name)
	}
	return ip
}

// uniqueName builds a stable, collision-resistant container name for a test.
func uniqueName(prefix string) string {
	return fmt.Sprintf("pk-e2e-%s-%d", prefix, time.Now().UnixNano())
}

// CopyInto copies a file from the host into the running container at destPath,
// bounded by dockerTimeout. argv only (no shell). Used by the nightly upgrade
// scenario to swap a higher-minor toolchain binary into a running lower-minor node
// container in place (no reboot). It fails the test on error.
func (nc *nodeContainer) CopyInto(t *testing.T, hostPath, destPath string) {
	t.Helper()
	out := docker(t, "cp", hostPath, nc.id+":"+destPath)
	if strings.TrimSpace(out) != "" {
		t.Logf("docker cp %s -> %s: %s", hostPath, destPath, out)
	}
}

// extractBinariesFromImage creates a throwaway (never-started) container from the
// given image, copies the named files out of it onto the host under destDir, and
// removes the throwaway container. It returns the host paths of the extracted
// files (in the same order as srcPaths). Bounded by dockerTimeout per docker call;
// the throwaway container is removed via t.Cleanup so a failure cannot leak it.
//
// This is the mechanism the nightly upgrade scenario uses to obtain the
// higher-minor kubeadm/kubelet/kubectl binaries from a second node image WITHOUT
// running it, so they can be staged into the running lower-minor container.
func extractBinariesFromImage(t *testing.T, image, destDir string, srcPaths ...string) []string {
	t.Helper()
	// `docker create` makes a container without starting it; its filesystem is
	// readable by `docker cp`.
	createOut, err := dockerErr("create", "--label", labelKey+"="+labelValue, image)
	if err != nil {
		t.Fatalf("docker create %s (to extract binaries): %v\n%s", image, err, createOut)
	}
	cid := strings.TrimSpace(createOut)
	t.Cleanup(func() { _, _ = dockerErr("rm", "-fv", cid) })

	hostPaths := make([]string, 0, len(srcPaths))
	for _, src := range srcPaths {
		base := src[strings.LastIndex(src, "/")+1:]
		dst := destDir + "/" + base
		cpOut := docker(t, "cp", cid+":"+src, dst)
		if strings.TrimSpace(cpOut) != "" {
			t.Logf("docker cp %s:%s -> %s: %s", cid, src, dst, cpOut)
		}
		hostPaths = append(hostPaths, dst)
	}
	return hostPaths
}
