package kubeadmconfig

import "fmt"

// Input carries the non-credential values needed to build kubeadm documents.
// Credential-bearing fields (bootstrap tokens, certificate keys, join discovery)
// are populated later by the credential layer, not here.
type Input struct {
	KubernetesVersion    string
	ControlPlaneEndpoint string // host:port
	ImageRepository      string
	PodSubnet            string
	ServiceSubnet        string
	DNSDomain            string
	CertSANs             []string

	NodeName         string
	CRISocket        string
	KubeletExtraArgs []Arg
	Taints           []Taint
	AdvertiseAddress string
	BindPort         int32

	// Credential/join fields (populated by the credential layer for join builds;
	// left zero for init/cluster builds). See ADR-2 / ADR-10.
	JoinToken          string   // bootstrap token "id.secret"
	CACertHashes       []string // SPKI "sha256:..." pins; MANDATORY for token discovery
	DiscoveryFilePath  string   // discovery kubeconfig path (mutually exclusive with JoinToken)
	JoinAsControlPlane bool     // true => join as an additional control-plane node
	CertificateKey     string   // control-plane join only
}

// BuildClusterConfiguration builds a ClusterConfiguration from non-credential input.
func BuildClusterConfiguration(in Input) ClusterConfiguration {
	c := NewClusterConfiguration()
	c.KubernetesVersion = in.KubernetesVersion
	c.ControlPlaneEndpoint = in.ControlPlaneEndpoint
	c.ImageRepository = in.ImageRepository
	c.Networking = Networking{
		PodSubnet:     in.PodSubnet,
		ServiceSubnet: in.ServiceSubnet,
		DNSDomain:     in.DNSDomain,
	}
	c.APIServer = APIServer{CertSANs: in.CertSANs}
	return c
}

// BuildInitConfiguration builds the non-credential parts of an InitConfiguration.
// BootstrapTokens and CertificateKey are populated later by the credential layer.
func BuildInitConfiguration(in Input) InitConfiguration {
	i := NewInitConfiguration()
	i.LocalAPIEndpoint = APIEndpoint{
		AdvertiseAddress: in.AdvertiseAddress,
		BindPort:         in.BindPort,
	}
	i.NodeRegistration = NodeRegistration{
		Name:             in.NodeName,
		CRISocket:        in.CRISocket,
		KubeletExtraArgs: in.KubeletExtraArgs,
		Taints:           in.Taints,
	}
	return i
}

// BuildJoinConfiguration builds a JoinConfiguration from input. It enforces ADR-2's
// join-trust rule by construction: token-based discovery MUST carry CA-cert hashes
// and never sets UnsafeSkipCAVerification; control-plane joins MUST supply a
// certificate key. It returns an error rather than emitting an unsafe config.
func BuildJoinConfiguration(in Input) (JoinConfiguration, error) {
	j := NewJoinConfiguration()
	j.NodeRegistration = NodeRegistration{
		Name:             in.NodeName,
		CRISocket:        in.CRISocket,
		KubeletExtraArgs: in.KubeletExtraArgs,
		Taints:           in.Taints,
	}

	switch {
	case in.DiscoveryFilePath != "":
		// File discovery: the CA is pinned by the (out-of-band, CA-embedded)
		// discovery kubeconfig itself. kubeadm validates that embedded CA at join
		// time; this builder does not parse the file, so the by-construction CA
		// guarantee here covers only the token path.
		j.Discovery = Discovery{File: &FileDiscovery{KubeConfigPath: in.DiscoveryFilePath}}
	case in.JoinToken != "":
		if len(in.CACertHashes) == 0 {
			return JoinConfiguration{}, fmt.Errorf("refusing to build join config: token discovery requires CA cert hashes (CA pinning is mandatory; UnsafeSkipCAVerification is never allowed)")
		}
		j.Discovery = Discovery{
			BootstrapToken: &BootstrapTokenDiscovery{
				Token:                    in.JoinToken,
				APIServerEndpoint:        in.ControlPlaneEndpoint,
				CACertHashes:             in.CACertHashes,
				UnsafeSkipCAVerification: false,
			},
		}
	default:
		return JoinConfiguration{}, fmt.Errorf("refusing to build join config: no trust anchor supplied (need a discovery file or a bootstrap token with CA cert hashes)")
	}

	if in.JoinAsControlPlane {
		if in.CertificateKey == "" {
			return JoinConfiguration{}, fmt.Errorf("control-plane join requires a certificate key")
		}
		j.ControlPlane = &JoinControlPlane{
			LocalAPIEndpoint: APIEndpoint{AdvertiseAddress: in.AdvertiseAddress, BindPort: in.BindPort},
			CertificateKey:   in.CertificateKey,
		}
	}

	return j, nil
}
