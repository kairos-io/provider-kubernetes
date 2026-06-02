package provider

import (
	"fmt"
	"net"
	"strings"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadmconfig"
	"github.com/kairos-io/provider-kubernetes/internal/reconcile/actualstate"

	"sigs.k8s.io/yaml"
)

const (
	defaultAPIServerPort = "6443"
	defaultDNSDomain     = "cluster.local"
	defaultCRISocket     = "unix:///run/containerd/containerd.sock"
)

// UserConfig is the subset of the operator-supplied `cluster.config` YAML that the
// provider reads. It uses kubeadm v1beta4 key names. Fields are added as needed;
// this is intentionally not the full schema.
type UserConfig struct {
	ClusterConfiguration struct {
		KubernetesVersion    string `json:"kubernetesVersion,omitempty"`
		ControlPlaneEndpoint string `json:"controlPlaneEndpoint,omitempty"`
		ImageRepository      string `json:"imageRepository,omitempty"`
		Networking           struct {
			PodSubnet     string `json:"podSubnet,omitempty"`
			ServiceSubnet string `json:"serviceSubnet,omitempty"`
			DNSDomain     string `json:"dnsDomain,omitempty"`
		} `json:"networking,omitempty"`
		APIServer struct {
			CertSANs []string `json:"certSANs,omitempty"`
		} `json:"apiServer,omitempty"`
	} `json:"clusterConfiguration,omitempty"`

	InitConfiguration struct {
		LocalAPIEndpoint struct {
			AdvertiseAddress string `json:"advertiseAddress,omitempty"`
			BindPort         int32  `json:"bindPort,omitempty"`
		} `json:"localAPIEndpoint,omitempty"`
		NodeRegistration struct {
			Name             string                `json:"name,omitempty"`
			CRISocket        string                `json:"criSocket,omitempty"`
			KubeletExtraArgs []kubeadmconfig.Arg   `json:"kubeletExtraArgs,omitempty"`
			Taints           []kubeadmconfig.Taint `json:"taints,omitempty"`
		} `json:"nodeRegistration,omitempty"`
	} `json:"initConfiguration,omitempty"`

	// JoinConfiguration carries the operator-delivered join material (ADR-10:
	// minted on a CP and delivered out-of-band via this config). Read for
	// controlplane/worker roles.
	JoinConfiguration struct {
		Discovery struct {
			BootstrapToken struct {
				Token             string   `json:"token,omitempty"`
				APIServerEndpoint string   `json:"apiServerEndpoint,omitempty"`
				CACertHashes      []string `json:"caCertHashes,omitempty"`
			} `json:"bootstrapToken,omitempty"`
			File struct {
				KubeConfigPath string `json:"kubeConfigPath,omitempty"`
			} `json:"file,omitempty"`
		} `json:"discovery,omitempty"`
		ControlPlane struct {
			CertificateKey string `json:"certificateKey,omitempty"`
			// LocalAPIEndpoint carries the advertise address and bind port for the
			// joining control-plane node's own API server (HA-2). When set, it is
			// preferred over initConfiguration.localAPIEndpoint for CP joins.
			LocalAPIEndpoint struct {
				AdvertiseAddress string `json:"advertiseAddress,omitempty"`
				BindPort         int32  `json:"bindPort,omitempty"`
			} `json:"localAPIEndpoint,omitempty"`
		} `json:"controlPlane,omitempty"`
	} `json:"joinConfiguration,omitempty"`
}

// ParseUserConfig parses the raw `cluster.config` YAML string. An empty string
// yields a zero UserConfig (defaults apply downstream).
func ParseUserConfig(raw string) (UserConfig, error) {
	var uc UserConfig
	if raw == "" {
		return uc, nil
	}
	if err := yaml.Unmarshal([]byte(raw), &uc); err != nil {
		return UserConfig{}, fmt.Errorf("parse cluster config: %w", err)
	}
	return uc, nil
}

// BuildInput merges the parsed user config, the provider Context, and defaults
// into a non-credential kubeadmconfig.Input. resolvedVersion is the kubeadm
// version resolved at runtime (ADR-3); pass "" to fall back to the user-specified
// kubernetesVersion. User-specified controlPlaneEndpoint takes precedence over the
// value derived from ControlPlaneHost.
//
// HA-1: at role=init, a warning is emitted (not a hard fail) if the
// controlPlaneEndpoint is absent or equals only the bare node IP (single-node
// default). At role=controlplane, an empty endpoint is a hard failure.
//
// HA-1 certSANs: the port-stripped host from controlPlaneEndpoint is also added to
// certSANs so a stable VIP/LB endpoint works with TLS. Both the ControlPlaneHost-
// derived host and the controlPlaneEndpoint host are added, since they may differ.
func BuildInput(ctx Context, uc UserConfig, resolvedVersion string) (kubeadmconfig.Input, string, error) {
	endpoint, host, err := normalizeControlPlane(ctx.ControlPlaneHost)
	if err != nil {
		return kubeadmconfig.Input{}, "", err
	}

	cpEndpoint := uc.ClusterConfiguration.ControlPlaneEndpoint
	if cpEndpoint == "" {
		cpEndpoint = endpoint
	}

	// HA-1: role-aware endpoint validation (pure). The non-fatal warning is returned
	// to Run, which logs it; a hard failure (e.g. role=controlplane with no endpoint)
	// is returned as an error. Returning the warning (rather than mutating ctx, which
	// is passed by value) ensures the advisory actually reaches the caller.
	//
	// The "is the endpoint just this node's own address" check compares against the
	// node's real advertiseAddress when set, NOT control_plane_host: an operator who
	// legitimately points control_plane_host AT the VIP would otherwise trip a false
	// "endpoint resolves to this node's own IP" advisory. Fall back to the
	// control_plane_host-derived host only when no advertiseAddress is configured.
	nodeIP := host
	if adv := uc.InitConfiguration.LocalAPIEndpoint.AdvertiseAddress; adv != "" {
		nodeIP = adv
	}
	endpointWarn, ferr := ValidateControlPlaneEndpoint(actualstate.Role(ctx.Role), cpEndpoint, nodeIP)
	if ferr != nil {
		return kubeadmconfig.Input{}, "", ferr
	}

	version := resolvedVersion
	if version == "" {
		version = uc.ClusterConfiguration.KubernetesVersion
	}

	dnsDomain := uc.ClusterConfiguration.Networking.DNSDomain
	if dnsDomain == "" {
		dnsDomain = defaultDNSDomain
	}

	criSocket := uc.InitConfiguration.NodeRegistration.CRISocket
	if criSocket == "" {
		criSocket = defaultCRISocket
	}

	// HA-1 certSANs: add both the ControlPlaneHost-derived host (as before) and
	// the port-stripped controlPlaneEndpoint host (which may be a VIP or LB DNS)
	// so the certificate covers both. appendIfMissing is idempotent.
	cpeHost := stripPort(cpEndpoint)
	sans := appendIfMissing(uc.ClusterConfiguration.APIServer.CertSANs, host)
	sans = appendIfMissing(sans, cpeHost)

	// HA-2: for CP joins, prefer joinConfiguration.controlPlane.localAPIEndpoint
	// for AdvertiseAddress/BindPort; fall back to initConfiguration.localAPIEndpoint.
	advertiseAddress := uc.InitConfiguration.LocalAPIEndpoint.AdvertiseAddress
	bindPort := uc.InitConfiguration.LocalAPIEndpoint.BindPort
	if actualstate.Role(ctx.Role) == actualstate.RoleControlPlane {
		if uc.JoinConfiguration.ControlPlane.LocalAPIEndpoint.AdvertiseAddress != "" {
			advertiseAddress = uc.JoinConfiguration.ControlPlane.LocalAPIEndpoint.AdvertiseAddress
		}
		if uc.JoinConfiguration.ControlPlane.LocalAPIEndpoint.BindPort != 0 {
			bindPort = uc.JoinConfiguration.ControlPlane.LocalAPIEndpoint.BindPort
		}
	}

	return kubeadmconfig.Input{
		KubernetesVersion:    version,
		ControlPlaneEndpoint: cpEndpoint,
		ImageRepository:      uc.ClusterConfiguration.ImageRepository,
		PodSubnet:            uc.ClusterConfiguration.Networking.PodSubnet,
		ServiceSubnet:        uc.ClusterConfiguration.Networking.ServiceSubnet,
		DNSDomain:            dnsDomain,
		CertSANs:             sans,
		NodeName:             uc.InitConfiguration.NodeRegistration.Name,
		CRISocket:            criSocket,
		KubeletExtraArgs:     uc.InitConfiguration.NodeRegistration.KubeletExtraArgs,
		Taints:               uc.InitConfiguration.NodeRegistration.Taints,
		AdvertiseAddress:     advertiseAddress,
		BindPort:             bindPort,
	}, endpointWarn, nil
}

// ValidateControlPlaneEndpoint validates the controlPlaneEndpoint for HA-1
// (ADR-11 #1). Pure function; callers log and/or fail on the result.
//   - role=init: warn (non-fatal) if endpoint is absent or equals the bare node IP.
//   - role=controlplane: hard-fail if endpoint is empty.
//   - role=worker: no validation (workers use the endpoint for join, not for HA).
//
// Returns (warning, nil) for a soft advisory, ("", error) for a hard failure,
// ("", nil) when the endpoint is acceptable.
func ValidateControlPlaneEndpoint(role actualstate.Role, endpoint, nodeIP string) (string, error) {
	switch role {
	case actualstate.RoleControlPlane:
		if strings.TrimSpace(endpoint) == "" {
			return "", fmt.Errorf("controlPlaneEndpoint must be set for role=controlplane (HA requires a stable VIP, LB, or DNS endpoint)")
		}
	case actualstate.RoleInit:
		if strings.TrimSpace(endpoint) == "" {
			return "controlPlaneEndpoint is not set; this is valid for a single-CP/worker cluster but REQUIRED for multi-control-plane HA. Add a stable VIP, LB, or DNS endpoint before adding more control-plane nodes (ADR-11).", nil
		}
		// Warn if the endpoint's host equals the bare node IP (single-node default, no stable endpoint).
		epHost := stripPort(endpoint)
		nodeIPClean := strings.TrimSpace(nodeIP)
		if nodeIPClean != "" && epHost == nodeIPClean {
			return fmt.Sprintf("controlPlaneEndpoint %q resolves to this node's own IP %q; this is valid for a single-CP cluster but is NOT a stable HA endpoint. Set a VIP, LB, or DNS name before adding more control-plane nodes (ADR-11).", endpoint, nodeIPClean), nil
		}
	}
	return "", nil
}

// normalizeControlPlane returns the host:port endpoint and the bare host (for the
// cert SAN). A host without a port gets the default API server port appended.
func normalizeControlPlane(cp string) (endpoint, host string, err error) {
	if cp == "" {
		return "", "", fmt.Errorf("control_plane_host must be set")
	}
	if h, _, splitErr := net.SplitHostPort(cp); splitErr == nil {
		return cp, h, nil
	}
	return net.JoinHostPort(cp, defaultAPIServerPort), cp, nil
}

// stripPort returns the host portion of a "host:port" string, or the original
// string if no port separator is found. Used for certSAN derivation.
func stripPort(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// appendIfMissing returns s with v appended if v is non-empty and not already present.
func appendIfMissing(s []string, v string) []string {
	if v == "" {
		return s
	}
	for _, e := range s {
		if e == v {
			return s
		}
	}
	out := make([]string, len(s), len(s)+1)
	copy(out, s)
	return append(out, v)
}
