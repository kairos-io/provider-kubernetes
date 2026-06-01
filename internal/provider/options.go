package provider

import (
	"github.com/kairos-io/kairos-sdk/clusterplugin"
)

const (
	defaultRootPath        = "/"
	providerOptRootPathKey = "cluster_root_path"
)

// Context is the parsed, validated provider input derived from a Cluster. It is a
// plain value with no I/O, so it (and the parsing) is unit-testable without
// hardware (design principle 6).
type Context struct {
	Role              string
	RootPath          string
	ControlPlaneHost  string
	ClusterToken      string
	UserOptions       string
	Env               map[string]string
	CACerts           []string
	ClusterConfigPath string

	// TokenWarning is a non-fatal advisory from cluster_token validation (OQ-8).
	TokenWarning string
}

// NewContext parses and validates a Cluster into a provider Context. It fails
// fast on invalid input (e.g. an empty/too-short cluster_token) so the provider
// never proceeds on bad configuration.
func NewContext(cluster clusterplugin.Cluster) (Context, error) {
	warning, err := ValidateClusterToken(cluster.ClusterToken)
	if err != nil {
		return Context{}, err
	}

	rootPath := defaultRootPath
	if v := cluster.ProviderOptions[providerOptRootPathKey]; v != "" {
		rootPath = v
	}

	return Context{
		Role:              string(cluster.Role),
		RootPath:          rootPath,
		ControlPlaneHost:  cluster.ControlPlaneHost,
		ClusterToken:      cluster.ClusterToken,
		UserOptions:       cluster.Options,
		Env:               cluster.Env,
		CACerts:           cluster.CACerts,
		ClusterConfigPath: cluster.ClusterConfigPath,
		TokenWarning:      warning,
	}, nil
}
