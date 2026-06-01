package provider

import (
	"fmt"
	"net"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadmconfig"

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
func BuildInput(ctx Context, uc UserConfig, resolvedVersion string) (kubeadmconfig.Input, error) {
	endpoint, host, err := normalizeControlPlane(ctx.ControlPlaneHost)
	if err != nil {
		return kubeadmconfig.Input{}, err
	}

	cpEndpoint := uc.ClusterConfiguration.ControlPlaneEndpoint
	if cpEndpoint == "" {
		cpEndpoint = endpoint
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

	return kubeadmconfig.Input{
		KubernetesVersion:    version,
		ControlPlaneEndpoint: cpEndpoint,
		ImageRepository:      uc.ClusterConfiguration.ImageRepository,
		PodSubnet:            uc.ClusterConfiguration.Networking.PodSubnet,
		ServiceSubnet:        uc.ClusterConfiguration.Networking.ServiceSubnet,
		DNSDomain:            dnsDomain,
		CertSANs:             appendIfMissing(uc.ClusterConfiguration.APIServer.CertSANs, host),
		NodeName:             uc.InitConfiguration.NodeRegistration.Name,
		CRISocket:            criSocket,
		KubeletExtraArgs:     uc.InitConfiguration.NodeRegistration.KubeletExtraArgs,
		Taints:               uc.InitConfiguration.NodeRegistration.Taints,
		AdvertiseAddress:     uc.InitConfiguration.LocalAPIEndpoint.AdvertiseAddress,
		BindPort:             uc.InitConfiguration.LocalAPIEndpoint.BindPort,
	}, nil
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
