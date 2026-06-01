// Command agent-provider-kubernetes is a Kairos cluster provider that bootstraps
// upstream Kubernetes clusters with kubeadm. It is shipped inside a Kairos image;
// kairos-agent discovers it (agent-provider-* prefix) and invokes it over the
// clusterplugin event bus.
//
// Modes:
//   - default (no argv): run as a clusterplugin event handler (Provider +
//     EventClusterReset). This is what kairos-agent invokes at boot.
//   - "reconcile": run one bounded reconcile pass for a Cluster read from
//     --cluster-file. This is what the boot-time yip stage invokes.
//   - "version": print the build version.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/kairos-io/kairos-sdk/clusterplugin"
	"github.com/mudler/go-pluggable"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
	"github.com/kairos-io/provider-kubernetes/internal/provider"
	"github.com/kairos-io/provider-kubernetes/version"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "reconcile":
			os.Exit(runReconcile(args[1:]))
		case "version", "--version", "-v":
			fmt.Println(version.Version)
			return
		case "help", "--help", "-h":
			printUsage(os.Stdout)
			return
		}
	}

	logrus.Infof("starting agent-provider-kubernetes %s", version.Version)
	plugin := clusterplugin.ClusterPlugin{Provider: provider.Provider}
	if err := plugin.Run(pluggable.FactoryPlugin{
		EventType:     clusterplugin.EventClusterReset,
		PluginHandler: provider.HandleClusterReset,
	}); err != nil {
		logrus.Fatal(err)
	}
}

// runReconcile executes one bounded reconcile pass. It is invoked from the
// boot-time yip stage Provider() emits; the stage runs at network.after so the
// network is up and CP reachability checks make sense.
func runReconcile(args []string) int {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	clusterFile := fs.String("cluster-file", provider.ClusterStatePath, "path to the serialized Cluster YAML (written 0600 on tmpfs by Provider)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	logrus.Infof("provider-kubernetes reconcile %s: reading %s", version.Version, *clusterFile)

	data, err := os.ReadFile(*clusterFile)
	if err != nil {
		logrus.Errorf("read cluster file: %v", err)
		return 1
	}
	var cluster clusterplugin.Cluster
	if err := yaml.Unmarshal(data, &cluster); err != nil {
		logrus.Errorf("parse cluster file: %v", err)
		return 1
	}

	if err := provider.Run(context.Background(), cluster, provider.Options{Runner: kubeadm.ExecRunner{}}); err != nil {
		logrus.Errorf("reconcile: %v", err)
		return 1
	}
	logrus.Info("provider-kubernetes reconcile: done")
	return 0
}

func printUsage(w *os.File) {
	_, _ = fmt.Fprintf(w, `agent-provider-kubernetes %s

Usage:
  agent-provider-kubernetes                 run the Kairos clusterplugin event handler (default)
  agent-provider-kubernetes reconcile [...] run one bounded reconcile pass for a serialized Cluster
  agent-provider-kubernetes version         print the build version

`, version.Version)
}
