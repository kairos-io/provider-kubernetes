package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kairos-io/kairos-sdk/bus"
	"github.com/kairos-io/kairos-sdk/clusterplugin"
	"github.com/mudler/go-pluggable"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
	"github.com/kairos-io/provider-kubernetes/internal/reconcile/actualstate"
	"github.com/kairos-io/provider-kubernetes/internal/reset"
	"github.com/kairos-io/provider-kubernetes/internal/status"
	"github.com/kairos-io/provider-kubernetes/version"
)

// HandleClusterReset is the Kairos "cluster.reset" event handler. It parses the
// payload and performs a bounded, idempotent reset (ADR-4). It deliberately does
// NOT run cluster_token validation: reset must succeed regardless of token state.
func HandleClusterReset(event *pluggable.Event) pluggable.EventResponse {
	return handleClusterReset(event, kubeadm.ExecRunner{}, status.NewFileSink())
}

// maxResetPayloadBytes caps the event payload size before unmarshaling, to bound
// CPU/memory of (in-process but still untrusted-by-shape) YAML/JSON parsing.
const maxResetPayloadBytes = 1 << 20 // 1 MiB

// handleClusterReset is the testable core (runner and sink injected).
func handleClusterReset(event *pluggable.Event, runner kubeadm.Runner, sink status.StatusSink) pluggable.EventResponse {
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
	resetErr := reset.Run(context.Background(), reset.Options{
		Runner:    runner,
		RootPath:  rootPath,
		CRISocket: criSocket,
	})

	// S4: write terminal reset status. Best-effort, bounded, swallowed on error.
	writeResetStatus(sink, actualstate.Role(string(config.Cluster.Role)), resetErr)

	if resetErr != nil {
		resp.Error = fmt.Sprintf("cluster reset: %s", resetErr)
	}
	return resp
}

// writeResetStatus records a terminal Phase=Reset status after a cluster reset.
// It is best-effort: write errors are swallowed so they never mask the reset result.
func writeResetStatus(sink status.StatusSink, role actualstate.Role, resetErr error) {
	if sink == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sink.Record(ctx, status.BuildStatus(status.BuildParams{
		Role:     role,
		IsReset:  true,
		ResetErr: resetErr,
		Now:      time.Now().UTC().Format(time.RFC3339),
		BootID:   readBootID(),
		Version:  version.Version,
	}))
}
