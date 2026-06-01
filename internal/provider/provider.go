// Package provider implements the Kairos clusterplugin entrypoint for
// provider-kubernetes: it translates a Cluster definition into a yip
// configuration.
//
// STATUS (foundation slice): inputs are parsed and validated, but bootstrap
// stages are not yet emitted. That lands with the kubeadm-flow and credential
// layers. Provider must always return promptly and never hang or panic (a
// provider that blocks stalls every later Kairos boot stage, issue #4099-1).
package provider

import (
	"github.com/kairos-io/kairos-sdk/clusterplugin"
	yip "github.com/mudler/yip/pkg/schema"
	"github.com/sirupsen/logrus"
)

// Provider is the clusterplugin.ClusterProvider implementation.
func Provider(cluster clusterplugin.Cluster) yip.YipConfig {
	ctx, err := NewContext(cluster)
	if err != nil {
		// Invalid configuration: fail loud, return an inert config. We do not emit
		// half-formed stages on bad input.
		logrus.Errorf("provider-kubernetes: invalid cluster configuration: %v", err)
		return yip.YipConfig{Name: "provider-kubernetes (configuration error)"}
	}

	if ctx.TokenWarning != "" {
		logrus.Warnf("provider-kubernetes: %s", ctx.TokenWarning)
	}

	logrus.Infof("provider-kubernetes: parsed cluster context for role %q (bootstrap not yet implemented)", ctx.Role)

	// Foundation slice: no stages emitted yet.
	return yip.YipConfig{Name: "provider-kubernetes (under development)"}
}
