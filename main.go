// Command agent-provider-kubernetes is a Kairos cluster provider that bootstraps
// upstream Kubernetes clusters with kubeadm. It is shipped inside a Kairos image;
// kairos-agent discovers it (agent-provider-* prefix) and invokes it over the
// clusterplugin event bus.
//
// STATUS: under active development. The provider is wired into the clusterplugin
// contract and implements input parsing/validation and cluster reset; the
// boot-time reconcile stage emission (OQ-3) is not yet wired. See PROJECT_CONTEXT.md
// (local, not committed) for the roadmap.
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
		PluginHandler: provider.HandleClusterReset,
	}); err != nil {
		logrus.Fatal(err)
	}
}
