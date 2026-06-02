package provider

import (
	"fmt"
	"net/url"
	"strings"
	"text/template"

	"sigs.k8s.io/yaml"
)

// JoinSnippet is the input to RenderJoinCloudConfig. It carries the freshly minted
// credential-bearing join values (Token, CACertHashes, CertificateKey) alongside
// the non-credential shape (Role, Endpoint). It is assembled by the mint-join CLI
// on a control-plane node and rendered into a ready-to-paste worker/controlplane
// cloud-config; it is never persisted (ADR-2/OQ-7).
type JoinSnippet struct {
	Role           string   // "worker" or "controlplane"
	Endpoint       string   // apiServerEndpoint, host:port
	Token          string   // bootstrap token "id.secret"
	CACertHashes   []string // "sha256:..." SPKI pins (at least one; CA pinning is mandatory)
	CertificateKey string   // control-plane joins only
	ClusterToken   string   // correlation value; must match the control plane's cluster_token
	TTL            string   // token TTL, surfaced in the header comment only
	Device         string   // install.device for the snippet (default "auto")
	// AdvertiseAddress is the joining CP's own API server advertise address (HA-2).
	// The minting CP cannot know the joiner's IP; the operator fills this in before
	// delivering the cloud-config. Empty means kubeadm's default-route heuristic
	// applies (acceptable for single-homed nodes, warn-worthy for multi-homed).
	AdvertiseAddress string
}

// EndpointFromKubeconfig extracts the apiserver endpoint ("host:port") from the
// first cluster's server URL in a kubeconfig. mint-join uses it to default its
// --endpoint from the control plane's own admin.conf. Pure function.
func EndpointFromKubeconfig(data []byte) (string, error) {
	var kc struct {
		Clusters []struct {
			Cluster struct {
				Server string `json:"server"`
			} `json:"cluster"`
		} `json:"clusters"`
	}
	if err := yaml.Unmarshal(data, &kc); err != nil {
		return "", fmt.Errorf("parse kubeconfig: %w", err)
	}
	if len(kc.Clusters) == 0 || kc.Clusters[0].Cluster.Server == "" {
		return "", fmt.Errorf("no cluster server found in kubeconfig")
	}
	server := kc.Clusters[0].Cluster.Server
	u, err := url.Parse(server)
	if err != nil {
		return "", fmt.Errorf("parse server URL %q: %w", server, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("server URL %q has no host", server)
	}
	return u.Host, nil
}

// clusterTokenPlaceholder is emitted when the caller does not supply the cluster's
// correlation token. It is intentionally long enough to pass the >=16-char
// cluster_token validation, but obviously a placeholder so an operator replaces it.
const clusterTokenPlaceholder = "CHANGE-ME-to-match-the-control-plane-cluster_token"

// RenderJoinCloudConfig renders a Kairos cloud-config snippet for a joining node.
// It fails loud (never emits a config) when the join material lacks a trust anchor
// or, for a control-plane join, a certificate key — the same secure-by-default
// posture the runtime join path enforces (ADR-2: never UnsafeSkipCAVerification).
func RenderJoinCloudConfig(s JoinSnippet) (string, error) {
	role := strings.ToLower(strings.TrimSpace(s.Role))
	switch role {
	case "worker", "controlplane":
	default:
		return "", fmt.Errorf("role must be worker or controlplane, got %q", s.Role)
	}
	if strings.TrimSpace(s.Endpoint) == "" {
		return "", fmt.Errorf("endpoint (apiServerEndpoint) must be set")
	}
	if strings.TrimSpace(s.Token) == "" {
		return "", fmt.Errorf("bootstrap token must be set")
	}
	hashes := make([]string, 0, len(s.CACertHashes))
	for _, h := range s.CACertHashes {
		if h = strings.TrimSpace(h); h != "" {
			hashes = append(hashes, h)
		}
	}
	if len(hashes) == 0 {
		return "", fmt.Errorf("at least one CA cert hash is required (CA pinning is mandatory)")
	}
	if role == "controlplane" && strings.TrimSpace(s.CertificateKey) == "" {
		return "", fmt.Errorf("control-plane join requires a certificate key")
	}

	clusterToken := strings.TrimSpace(s.ClusterToken)
	if clusterToken == "" {
		clusterToken = clusterTokenPlaceholder
	}
	device := strings.TrimSpace(s.Device)
	if device == "" {
		device = "auto"
	}

	data := struct {
		Role             string
		Endpoint         string
		Token            string
		Hashes           []string
		CertKey          string
		ClusterToken     string
		TTL              string
		Device           string
		IsCP             bool
		AdvertiseAddress string
	}{
		Role:             role,
		Endpoint:         s.Endpoint,
		Token:            s.Token,
		Hashes:           hashes,
		CertKey:          strings.TrimSpace(s.CertificateKey),
		ClusterToken:     clusterToken,
		TTL:              strings.TrimSpace(s.TTL),
		Device:           device,
		IsCP:             role == "controlplane",
		AdvertiseAddress: strings.TrimSpace(s.AdvertiseAddress),
	}

	var b strings.Builder
	if err := joinTemplate.Execute(&b, data); err != nil {
		return "", fmt.Errorf("render join cloud-config: %w", err)
	}
	return b.String(), nil
}

// joinTemplate renders the cloud-config. It mirrors samples/worker.yaml and
// samples/controlplane.yaml. Comments are deliberate operator guidance, so this is
// a text template rather than a struct marshaled by a YAML encoder (which would
// drop the comments).
var joinTemplate = template.Must(template.New("join").Parse(`#cloud-config
# provider-kubernetes JOIN cloud-config for a {{.Role}} node.
#
# Generated by: agent-provider-kubernetes mint-join
# The bootstrap token below is bounded-TTL{{if .TTL}} (ttl {{.TTL}}){{end}} and the
# CA hash pins the cluster CA. Deliver this to the joining node soon: an expired
# token must be re-minted. The provider persists none of this material.
{{- if .IsCP}}
# The certificateKey decrypts the uploaded control-plane certs (upstream kubeadm
# applies a 2h expiry on the kubeadm-certs secret); re-mint per control-plane join.
#
# OPERATOR ACTION REQUIRED: set joinConfiguration.controlPlane.localAPIEndpoint.
# advertiseAddress below to this joining node's own IP address (HA-2). The
# minting control-plane cannot know the joiner's IP. If you leave it blank,
# kubeadm uses the default-route interface address (acceptable for single-homed
# nodes; warn-worthy for multi-homed).
{{- end}}

install:
  device: "{{.Device}}"
  auto: true
  reboot: true

users:
  # Kairos 3.3+ requires at least one user in the 'admin' group for auto-install
  # to proceed (or set install.nousers: true).
  - name: kairos
    groups: ["admin", "sudo"]
    passwd: kairos

cluster:
  # Must match the control plane's cluster_token (correlation value only).
  cluster_token: "{{.ClusterToken}}"
  control_plane_host: "{{.Endpoint}}"
  role: {{.Role}}

  providerConfig:
    cluster_root_path: "/"

  config: |
    joinConfiguration:
      discovery:
        bootstrapToken:
          token: "{{.Token}}"
          apiServerEndpoint: "{{.Endpoint}}"
          caCertHashes:
{{- range .Hashes}}
            - "{{.}}"
{{- end}}
{{- if .IsCP}}
      controlPlane:
        certificateKey: "{{.CertKey}}"
        localAPIEndpoint:
          # Set to this joining CP node's own IP (HA-2, ADR-11). Operator must fill this in.
          advertiseAddress: "{{if .AdvertiseAddress}}{{.AdvertiseAddress}}{{else}}FILL-IN-THIS-NODE-IP{{end}}"
{{- end}}
`))
