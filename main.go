// Command agent-provider-kubernetes is a Kairos cluster provider that
// bootstraps upstream Kubernetes clusters with kubeadm.
//
// STATUS: under active development. The provider is wired into the Kairos
// clusterplugin contract but does not yet generate cluster stages. See
// PROJECT_CONTEXT.md (local, not committed) for the roadmap.
package main

import (
	"github.com/kairos-io/provider-kubernetes/version"

	"github.com/kairos-io/kairos-sdk/clusterplugin"
	"github.com/mudler/go-pluggable"
	yip "github.com/mudler/yip/pkg/schema"
	"github.com/sirupsen/logrus"
)

func main() {
	logrus.Infof("starting agent-provider-kubernetes %s", version.Version)

	plugin := clusterplugin.ClusterPlugin{
		Provider: provider,
	}

	if err := plugin.Run(pluggable.FactoryPlugin{
		EventType:     clusterplugin.EventClusterReset,
		PluginHandler: handleClusterReset,
	}); err != nil {
		logrus.Fatal(err)
	}
}

// provider is the entrypoint Kairos invokes to translate a Cluster definition
// into a yip configuration.
//
// NOT YET IMPLEMENTED: this returns an inert configuration so the binary is a
// well-behaved no-op until the bootstrap logic lands. It must never panic or
// block — a provider that hangs blocks every subsequent Kairos boot stage
// (see issue kairos-io/kairos#4099).
func provider(_ clusterplugin.Cluster) yip.YipConfig {
	logrus.Warn("provider-kubernetes is under active development and does not yet generate cluster stages")
	return yip.YipConfig{
		Name: "provider-kubernetes (under development)",
	}
}

// handleClusterReset handles the Kairos "cluster.reset" event.
//
// NOT YET IMPLEMENTED: currently a logged no-op.
func handleClusterReset(_ *pluggable.Event) pluggable.EventResponse {
	logrus.Warn("cluster reset handling is under active development and is currently a no-op")
	return pluggable.EventResponse{}
}
