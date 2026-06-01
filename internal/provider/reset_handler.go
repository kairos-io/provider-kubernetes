package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kairos-io/kairos-sdk/bus"
	"github.com/kairos-io/kairos-sdk/clusterplugin"
	"github.com/mudler/go-pluggable"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
	"github.com/kairos-io/provider-kubernetes/internal/reset"
)

// HandleClusterReset is the Kairos "cluster.reset" event handler. It parses the
// payload and performs a bounded, idempotent reset (ADR-4). It deliberately does
// NOT run cluster_token validation: reset must succeed regardless of token state.
func HandleClusterReset(event *pluggable.Event) pluggable.EventResponse {
	return handleClusterReset(event, kubeadm.ExecRunner{})
}

// handleClusterReset is the testable core (runner injected).
func handleClusterReset(event *pluggable.Event, runner kubeadm.Runner) pluggable.EventResponse {
	var resp pluggable.EventResponse
	if event == nil {
		return resp
	}

	var payload bus.EventPayload
	if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
		resp.Error = fmt.Sprintf("parse reset event: %s", err)
		return resp
	}
	var config clusterplugin.Config
	if err := yaml.Unmarshal([]byte(payload.Config), &config); err != nil {
		resp.Error = fmt.Sprintf("parse reset config: %s", err)
		return resp
	}
	if config.Cluster == nil {
		return resp // nothing to reset
	}

	rootPath := defaultRootPath
	if v := config.Cluster.ProviderOptions[providerOptRootPathKey]; v != "" {
		rootPath = v
	}

	// Best-effort CRI socket from the user config (optional).
	var criSocket string
	if uc, err := ParseUserConfig(config.Cluster.Options); err == nil {
		criSocket = uc.InitConfiguration.NodeRegistration.CRISocket
	}

	logrus.Infof("provider-kubernetes: handling cluster reset (root=%s)", rootPath)
	if err := reset.Run(context.Background(), reset.Options{
		Runner:    runner,
		RootPath:  rootPath,
		CRISocket: criSocket,
	}); err != nil {
		resp.Error = fmt.Sprintf("cluster reset: %s", err)
	}
	return resp
}
