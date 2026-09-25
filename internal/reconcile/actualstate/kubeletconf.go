package actualstate

import (
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

// kubeletConfMaxBytes bounds how much of kubelet.conf the prober reads. A
// kubeadm-written kubelet.conf with an embedded certificate and key is a few
// KiB; a file past this bound is not one kubeadm wrote.
const kubeletConfMaxBytes = 64 << 10

// initIncomplete reports whether this control plane's `kubeadm init` stopped
// before its kubelet-finalize phase.
//
// kubeadm's kubeconfig phase writes kubelet.conf with the node's client
// certificate and key embedded (client-certificate-data). kubelet-finalize,
// the phase after bootstrap-token, rewrites it to reference the kubelet's
// rotated certificate, /var/lib/kubelet/pki/kubelet-client-current.pem, and
// only when that file exists; otherwise it returns without error and init
// still succeeds (cmd/kubeadm/app/cmd/phases/init/kubeletfinalize.go,
// identical in release-1.35, 1.36 and 1.37). So the embedded form shows an
// unfinished init only when the rotated certificate exists: then the phase
// would have rewritten the file had it run.
//
// Every case that does not support that conclusion reports false: no rotated
// certificate (rotation disabled, or a custom --cert-dir this does not
// follow), or kubelet.conf missing, unreadable, not a regular file, oversize
// or unparseable. A false negative leaves the status as it was before this
// check existed; a false positive would report a healthy cluster as broken on
// every boot.
//
// A node that joined never matches: its kubelet.conf comes from TLS bootstrap
// and always references the certificate by path.
func initIncomplete(rootPath string) bool {
	pem := filepath.Join(rootPath, "var", "lib", "kubelet", "pki", "kubelet-client-current.pem")
	if _, err := os.Stat(pem); err != nil {
		return false
	}
	data, ok := readRegularFile(filepath.Join(rootPath, "etc", "kubernetes", "kubelet.conf"), kubeletConfMaxBytes)
	if !ok {
		return false
	}
	return embedsClientCertificate(data)
}

// embedsClientCertificate reports whether any user in a kubeconfig carries an
// inline client certificate. The document also holds a private key, so the
// result is the only thing that leaves this function: neither the content nor
// a parse error is logged or returned.
func embedsClientCertificate(kubeconfig []byte) bool {
	var doc struct {
		Users []struct {
			User struct {
				ClientCertificateData string `json:"client-certificate-data"`
			} `json:"user"`
		} `json:"users"`
	}
	if err := yaml.Unmarshal(kubeconfig, &doc); err != nil {
		return false
	}
	for _, u := range doc.Users {
		if u.User.ClientCertificateData != "" {
			return true
		}
	}
	return false
}
