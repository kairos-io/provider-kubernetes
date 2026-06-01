// Command agent-provider-kubernetes is a Kairos cluster provider that
// bootstraps upstream Kubernetes clusters with kubeadm.
//
// STATUS: under active development. The provider is wired into the Kairos
// clusterplugin contract and parses/validates input, but does not yet generate
// bootstrap stages. See PROJECT_CONTEXT.md (local, not committed) for the roadmap.
package main

import (
	"github.com/kairos-io/provider-kubernetes/internal/provider"
	"github.com/kairos-io/provider-kubernetes/version"

	"github.com/kairos-io/kairos-sdk/clusterplugin"
	"github.com/mudler/go-pluggable"
	"github.com/sirupsen/logrus"
)

func main() {
	logrus.Infof("starting agent-provider-kubernetes %s", version.Version)

	plugin := clusterplugin.ClusterPlugin{
		Provider: provider.Provider,
	}

	if err := plugin.Run(pluggable.FactoryPlugin{
		EventType:     clusterplugin.EventClusterReset,
		PluginHandler: handleClusterReset,
	}); err != nil {
		logrus.Fatal(err)
	}
}

// handleClusterReset handles the Kairos "cluster.reset" event.
//
// NOT YET IMPLEMENTED: currently a logged no-op.
func handleClusterReset(_ *pluggable.Event) pluggable.EventResponse {
	logrus.Warn("cluster reset handling is under active development and is currently a no-op")
	return pluggable.EventResponse{}
}
