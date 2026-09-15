package hostexec

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// hostileLookup simulates a provider environment carrying every variable the
// closed environments must drop, plus both proxy casings and an empty proxy.
func hostileLookup(key string) (string, bool) {
	env := map[string]string{
		"PATH":                       "/usr/local/bin:/usr/bin",
		"HOME":                       "/root",
		"KUBECONFIG":                 "/root/.kube/config",
		"KUBERC":                     "/root/.kube/kuberc",
		"GODEBUG":                    "tlsrsakex=1",
		"SSL_CERT_FILE":              "/tmp/evil-ca.pem",
		"KUBEADM_UPGRADE_DRYRUN_DIR": "/tmp/dry",
		"SYSTEMD_OFFLINE":            "1",
		"CONTAINERD_ADDRESS":         "/run/evil.sock",
		"ALL_PROXY":                  "socks5://all:1080",
		"ftp_proxy":                  "http://ftp:21",
		"HTTP_PROXY":                 "http://upper:3128",
		"HTTPS_PROXY":                "",
		"NO_PROXY":                   "10.0.0.0/8",
		"http_proxy":                 "http://lower:3128",
		"https_proxy":                "http://lower:3129",
	}
	v, ok := env[key]
	return v, ok
}

func TestPathsAreCleanAbsoluteUsrBin(t *testing.T) {
	want := []string{KubeadmPath, KubectlPath, CtrPath, SystemctlPath, EtcdctlPath}
	got := Paths()
	if !slices.Equal(got, want) {
		t.Fatalf("Paths() = %v, want %v", got, want)
	}
	seen := map[string]bool{}
	for _, p := range got {
		if seen[p] {
			t.Errorf("duplicate path %q", p)
		}
		seen[p] = true
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			t.Errorf("path %q must be absolute and clean", p)
		}
		if !strings.HasPrefix(p, "/usr/bin/") {
			t.Errorf("path %q must be under /usr/bin/", p)
		}
	}
	for _, c := range []Command{Kubeadm(nil), Kubectl(nil), Ctr(), Systemctl()} {
		if !seen[c.Path] {
			t.Errorf("builder path %q missing from Paths()", c.Path)
		}
	}
}

func TestChildPATHAndCacheDirAvoidPersistentLocations(t *testing.T) {
	for _, dir := range strings.Split(ChildPATH, ":") {
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
			t.Errorf("ChildPATH entry %q must be absolute and clean", dir)
		}
		if dir == "/usr/local" || strings.HasPrefix(dir, "/usr/local/") {
			t.Errorf("ChildPATH entry %q must not be under /usr/local", dir)
		}
	}
	if !strings.HasPrefix(KubectlCacheDir, "/run/") || filepath.Clean(KubectlCacheDir) != KubectlCacheDir {
		t.Errorf("KubectlCacheDir %q must be a clean path under /run", KubectlCacheDir)
	}
}

func TestKubeadmEnvIsExactAllowlist(t *testing.T) {
	want := []string{
		"PATH=" + ChildPATH,
		"HTTP_PROXY=http://upper:3128",
		"NO_PROXY=10.0.0.0/8",
		"http_proxy=http://lower:3128",
		"https_proxy=http://lower:3129",
	}
	if got := Kubeadm(hostileLookup).Env; !slices.Equal(got, want) {
		t.Fatalf("kubeadm env = %q, want %q", got, want)
	}
}

func TestKubectlEnvIsExactAllowlist(t *testing.T) {
	want := []string{
		"KUBECACHEDIR=" + KubectlCacheDir,
		"KUBECTL_KUBERC=false",
		"KUBERC=off",
		"HTTP_PROXY=http://upper:3128",
		"NO_PROXY=10.0.0.0/8",
		"http_proxy=http://lower:3128",
		"https_proxy=http://lower:3129",
	}
	if got := Kubectl(hostileLookup).Env; !slices.Equal(got, want) {
		t.Fatalf("kubectl env = %q, want %q", got, want)
	}
}

func TestCtrAndSystemctlEnvAreEmptyNonNil(t *testing.T) {
	for name, c := range map[string]Command{"ctr": Ctr(), "systemctl": Systemctl()} {
		if c.Env == nil || len(c.Env) != 0 {
			t.Errorf("%s env = %#v, want an empty non-nil slice", name, c.Env)
		}
	}
}

// kairosInitRWPaths mirrors kairos-init v0.14.6
// pkg/bundled/cloudconfigs/00_rootfs.yaml RW_PATHS: an ephemeral, ram-backed
// overlay recreated fresh at every boot. Content placed there does not
// survive the read-only OS image's own attestation.
var kairosInitRWPaths = []string{"/var", "/etc", "/srv"}

// kairosInitPersistentStatePaths mirrors kairos-init v0.14.6
// pkg/bundled/cloudconfigs/00_rootfs.yaml PERSISTENT_STATE_PATHS (see
// PROJECT_CONTEXT.md ADR-16-A2 "Grounding" / the F-OPTBIND investigation):
// content there is bind-mounted from the persistent COS_PERSISTENT partition
// and survives an image upgrade, so it is not read-only OS-image content and
// not a safe anchor/bundle location.
var kairosInitPersistentStatePaths = []string{
	"/etc/cni", "/etc/init.d", "/etc/iscsi", "/etc/k0s", "/etc/kubernetes",
	"/etc/modprobe.d", "/etc/pwx", "/etc/rancher", "/etc/runlevels", "/etc/ssh",
	"/etc/ssl/certs", "/etc/sysconfig", "/etc/systemd", "/etc/zfs", "/home",
	"/opt", "/root", "/usr/libexec", "/var/cores", "/var/lib/ca-certificates",
	"/var/lib/cni", "/var/lib/containerd", "/var/lib/calico", "/var/lib/dbus",
	"/var/lib/etcd", "/var/lib/extensions", "/var/lib/confexts", "/var/lib/k0s",
	"/var/lib/kubelet", "/var/lib/longhorn", "/var/lib/osd", "/var/lib/rancher",
	"/var/lib/rook", "/var/lib/tailscale", "/var/lib/wicked", "/var/lib/kairos",
	"/var/log",
}

// TestBundleDirAndProviderBinaryPathAvoidPersistentAndEphemeralLocations is
// ADR-16-A2 O-1: BundleDir and ProviderBinaryPath must fall under NEITHER a
// sysext hierarchy / tmpfs-adjacent location (/usr, /usr/local, /oem, /run,
// /tmp) NOR a kairos-init RW_PATHS or PERSISTENT_STATE_PATHS entry -- either
// property would make the bundle something other than fixed, read-only
// OS-image content sharing the provider binary's own device.
func TestBundleDirAndProviderBinaryPathAvoidPersistentAndEphemeralLocations(t *testing.T) {
	disallowed := []string{"/usr", "/usr/local", "/oem", "/run", "/tmp"}
	disallowed = append(disallowed, kairosInitRWPaths...)
	disallowed = append(disallowed, kairosInitPersistentStatePaths...)

	for _, p := range []string{BundleDir, ProviderBinaryPath} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			t.Errorf("%q must be an absolute, clean path", p)
		}
		for _, d := range disallowed {
			if p == d || strings.HasPrefix(p, d+"/") {
				t.Errorf("%q must not be under %q", p, d)
			}
		}
	}
}

// TestBundleDirAndProviderBinaryPathShareSystemHierarchy locks in ADR-16-A2
// decision 1: both live under /system, so the bundle walk's device anchor
// (derived from ProviderBinaryPath) is meaningful for BundleDir.
func TestBundleDirAndProviderBinaryPathShareSystemHierarchy(t *testing.T) {
	for _, p := range []string{BundleDir, ProviderBinaryPath} {
		if !strings.HasPrefix(p, "/system/") {
			t.Errorf("%q must be under /system", p)
		}
	}
}

func TestNilLookupPassesNoProxy(t *testing.T) {
	if got := Kubeadm(nil).Env; !slices.Equal(got, []string{"PATH=" + ChildPATH}) {
		t.Errorf("kubeadm env with nil lookup = %q", got)
	}
	if got := Kubectl(nil).Env; len(got) != 3 {
		t.Errorf("kubectl env with nil lookup = %q, want only the three fixed entries", got)
	}
}
