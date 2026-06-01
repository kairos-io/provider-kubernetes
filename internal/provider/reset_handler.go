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

// maxResetPayloadBytes caps the event payload size before unmarshaling, to bound
// CPU/memory of (in-process but still untrusted-by-shape) YAML/JSON parsing.
const maxResetPayloadBytes = 1 << 20 // 1 MiB

// handleClusterReset is the testable core (runner injected).
func handleClusterReset(event *pluggable.Event, runner kubeadm.Runner) pluggable.EventResponse {
	var resp pluggable.EventResponse
	if event == nil {
		return resp
	}
	if len(event.Data) > maxResetPayloadBytes {
		resp.Error = fmt.Sprintf("reset event payload too large (%d bytes; max %d)", len(event.Data), maxResetPayloadBytes)
		return resp
	}

	var payload bus.EventPayload
	if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
		resp.Error = fmt.Sprintf("parse reset event: %s", err)
		return resp
	}
	if len(payload.Config) > maxResetPayloadBytes {
		resp.Error = fmt.Sprintf("reset config payload too large (%d bytes; max %d)", len(payload.Config), maxResetPayloadBytes)
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
	if uc, err := ParseUserConfig(config.Cluster.Options); err != nil {
		logrus.Warnf("provider-kubernetes: reset could not parse user config for CRI socket (continuing without): %v", err)
	} else {
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
