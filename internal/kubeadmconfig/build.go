package kubeadmconfig

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
