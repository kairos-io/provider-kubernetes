package actualstate

import "testing"

// kubeletConfEmbedded is kubelet.conf as kubeadm's kubeconfig phase writes it
// on the init node: the client certificate and key inline. The base64 values
// are placeholders, not key material.
const kubeletConfEmbedded = `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: ZmFrZS1jYQ==
    server: https://10.0.0.10:6443
  name: kubernetes
contexts:
- context:
    cluster: kubernetes
    user: system:node:cp1
  name: system:node:cp1@kubernetes
current-context: system:node:cp1@kubernetes
kind: Config
preferences: {}
users:
- name: system:node:cp1
  user:
    client-certificate-data: ZmFrZS1jZXJ0
    client-key-data: ZmFrZS1rZXk=
`

// kubeletConfFinalized is the same file after kubelet-finalize rewrote it to
// reference the rotated certificate (and what TLS bootstrap writes on a node
// that joined).
const kubeletConfFinalized = `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: ZmFrZS1jYQ==
    server: https://10.0.0.10:6443
  name: kubernetes
contexts:
- context:
    cluster: kubernetes
    user: system:node:cp1
  name: system:node:cp1@kubernetes
current-context: system:node:cp1@kubernetes
kind: Config
preferences: {}
users:
- name: system:node:cp1
  user:
    client-certificate: /var/lib/kubelet/pki/kubelet-client-current.pem
    client-key: /var/lib/kubelet/pki/kubelet-client-current.pem
`

func TestEmbedsClientCertificate(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want bool
	}{
		{name: "embedded, as kubeadm init writes it", doc: kubeletConfEmbedded, want: true},
		{name: "finalized, referenced by path", doc: kubeletConfFinalized, want: false},
		{name: "second user embeds", doc: "users:\n- name: a\n  user:\n    client-certificate: /x.pem\n- name: b\n  user:\n    client-certificate-data: ZmFrZQ==\n", want: true},
		{name: "empty value is not embedded", doc: "users:\n- name: a\n  user:\n    client-certificate-data: \"\"\n", want: false},
		{name: "no users", doc: "apiVersion: v1\nkind: Config\n", want: false},
		{name: "empty document", doc: "", want: false},
		{name: "unparseable", doc: "users: [\n", want: false},
		{name: "users of the wrong shape", doc: "users: 7\n", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := embedsClientCertificate([]byte(tt.doc)); got != tt.want {
				t.Fatalf("embedsClientCertificate = %v, want %v", got, tt.want)
			}
		})
	}
}
