// Package hostexec is the single source of truth for the host binaries the
// provider executes and the complete environment each one runs with (ADR-1-A1).
//
// Every privileged exec uses an absolute path inside the booted image's /usr and
// an environment built here from an allowlist: nothing is resolved via PATH and
// nothing is inherited from the provider's own environment. On Kairos /usr/local
// is the persistent, writable COS_PERSISTENT mount and comes first in the default
// PATH, so a name-based lookup would run whatever was placed there instead of the
// verified bundled binary; an inherited environment would let variables such as
// SYSTEMD_OFFLINE, CONTAINERD_ADDRESS, KUBERC or GODEBUG change what a tool does.
//
// The paths are constants: they are never joined with cluster_root_path and have
// no runtime override. A downstream distro that installs the tools elsewhere
// needs a build-tag-selected const file that passes the same invariant tests.
package hostexec

const (
	// KubeadmPath is the bundled kubeadm (the upgrade target, ADR-12 U-B7).
	KubeadmPath = "/usr/bin/kubeadm"
	// KubectlPath is the bundled kubectl.
	KubectlPath = "/usr/bin/kubectl"
	// CtrPath is the static ctr built alongside containerd (ADR-16 image import).
	CtrPath = "/usr/bin/ctr"
	// SystemctlPath is the base image's systemctl (Hadron has a merged /usr).
	SystemctlPath = "/usr/bin/systemctl"
	// EtcdctlPath is the etcdctl extracted from the verified etcd image (ADR-12-A1).
	EtcdctlPath = "/usr/bin/etcdctl"

	// ChildPATH is the PATH kubeadm receives for the helpers it looks up itself
	// (systemctl, kubelet, cp, mount, losetup, modprobe). It excludes /usr/local.
	ChildPATH = "/usr/sbin:/usr/bin:/sbin:/bin"

	// KubectlCacheDir is kubectl's discovery cache (KUBECACHEDIR): on tmpfs,
	// owned by the provider, and wiped at reboot. Without it kubectl would use
	// $HOME/.kube/cache, which is relative to the working directory when HOME is
	// unset (as it is for the kairos-agent service).
	KubectlCacheDir = "/run/provider-kubernetes/kubectl-cache"

	// BundleDir is where the image build embeds the pre-verified kubeadm
	// control-plane image tarballs and their images.lock (ADR-16-A2 /
	// F-OPTBIND). It is read-only OS-image content: not under /opt (a
	// PERSISTENT_STATE_PATHS entry backed by the persistent COS_PERSISTENT
	// mount) and not under /usr or /usr/local (sysext hierarchies a
	// persistent extension could shadow across an upgrade, and where
	// overlayfs st_dev is not a reliable device anchor). It shares /system
	// with ProviderBinaryPath, which the no-follow bundle walk uses as its
	// device anchor.
	BundleDir = "/system/provider-kubernetes/images"

	// ProviderBinaryPath is where the Kairos image installs the provider
	// binary (Kairos discovery convention: agent-provider-* under
	// /system/providers/). It also anchors BundleDir's no-follow walk
	// (ADR-16-A2 decision 3): a same-filesystem bind over BundleDir can only
	// expose content already in the image, and a foreign-filesystem bind is
	// refused by the device check.
	ProviderBinaryPath = "/system/providers/agent-provider-kubernetes"
)

// proxyVars are the proxy variables Go's HTTP client honors, in the fixed order
// they are passed on. kubeadm also copies them into the control-plane static
// pods and the kube-proxy DaemonSet, so this preserves existing behavior.
var proxyVars = []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"}

// LookupFunc reads one variable from the provider's own environment
// (os.LookupEnv in production).
type LookupFunc func(key string) (string, bool)

// Command is an absolute binary path plus the complete environment it runs
// with. Env is never nil; an empty slice means an empty environment.
type Command struct {
	Path string
	Env  []string
}

// Kubeadm returns kubeadm with PATH=ChildPATH and the non-empty proxy variables.
func Kubeadm(lookup LookupFunc) Command {
	return Command{Path: KubeadmPath, Env: withProxy([]string{"PATH=" + ChildPATH}, lookup)}
}

// Kubectl returns kubectl with a provider-owned cache, the operator kuberc
// disabled (its defaults would inject flags such as --server or
// --insecure-skip-tls-verify), and the non-empty proxy variables. It gets no
// PATH and no HOME: every call passes --kubeconfig and runs no helpers.
func Kubectl(lookup LookupFunc) Command {
	env := []string{"KUBECACHEDIR=" + KubectlCacheDir, "KUBECTL_KUBERC=false", "KUBERC=off"}
	return Command{Path: KubectlPath, Env: withProxy(env, lookup)}
}

// Ctr returns ctr with an empty environment, so CONTAINERD_ADDRESS and
// CONTAINERD_SNAPSHOTTER cannot redirect the import.
func Ctr() Command {
	return Command{Path: CtrPath, Env: []string{}}
}

// Systemctl returns systemctl with an empty environment, so SYSTEMD_OFFLINE
// cannot turn a restart into a successful no-op and the bus cannot be rerouted.
func Systemctl() Command {
	return Command{Path: SystemctlPath, Env: []string{}}
}

// Paths lists every binary the provider executes, for the image checks.
func Paths() []string {
	return []string{KubeadmPath, KubectlPath, CtrPath, SystemctlPath, EtcdctlPath}
}

func withProxy(env []string, lookup LookupFunc) []string {
	if lookup == nil {
		return env
	}
	for _, k := range proxyVars {
		if v, ok := lookup(k); ok && v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}
