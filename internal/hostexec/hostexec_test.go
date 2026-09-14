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

func TestNilLookupPassesNoProxy(t *testing.T) {
	if got := Kubeadm(nil).Env; !slices.Equal(got, []string{"PATH=" + ChildPATH}) {
		t.Errorf("kubeadm env with nil lookup = %q", got)
	}
	if got := Kubectl(nil).Env; len(got) != 3 {
		t.Errorf("kubectl env with nil lookup = %q, want only the three fixed entries", got)
	}
}
