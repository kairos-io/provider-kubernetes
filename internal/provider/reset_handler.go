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
	return handleClusterReset(event, ResetOptions{Runner: kubeadm.DefaultRunner(), Sink: status.NewFileSink()})
}

// ResetOptions are the seams of a reset pass. Runner and Sink are required.
type ResetOptions struct {
	Runner kubeadm.Runner
	Sink   status.StatusSink
	// ControlPlaneReachable decides whether reset warns that this stacked-etcd
	// control plane's member may stay registered (HA-5). nil -> this node's
	// own apiserver /healthz on the cluster's bind port: when it answers,
	// `kubeadm reset` can reach etcd to remove the member itself.
	ControlPlaneReachable func(ctx context.Context) bool
}

// ResetCluster resets this node's Kubernetes state for cluster and records the
// Phase=Reset status. The cluster.reset event and the reset subcommand both
// come through here, so both write the status docs/status.md promises and
// both get the same probes.
func ResetCluster(ctx context.Context, cluster clusterplugin.Cluster, opts ResetOptions) error {
	rootPath := defaultRootPath
	if v := cluster.ProviderOptions[providerOptRootPathKey]; v != "" {
		rootPath = v
	}
	role := actualstate.Role(string(cluster.Role))

	// Best-effort: the CRI socket, the node name for the HA-5 advisory and
	// the apiserver port come from the user config; reset proceeds without.
	var criSocket, nodeName string
	var bindPort int32
	if uc, err := ParseUserConfig(cluster.Options); err != nil {
		logrus.Warnf("provider-kubernetes: reset could not parse user config (continuing without it): %v", err)
	} else {
		criSocket = uc.InitConfiguration.NodeRegistration.CRISocket
		nodeName = uc.InitConfiguration.NodeRegistration.Name
		bindPort = localBindPort(role, uc)
	}
	reachable := opts.ControlPlaneReachable
	if reachable == nil {
		reachable = localAPIHealthyProbe(bindPort)
	}

	logrus.Infof("provider-kubernetes: handling cluster reset (root=%s)", rootPath)
	resetErr := reset.Run(ctx, reset.Options{
		Runner:                opts.Runner,
		RootPath:              rootPath,
		CRISocket:             criSocket,
		NodeName:              nodeName,
		ControlPlaneReachable: reachable,
	})
	writeResetStatus(opts.Sink, role, resetErr)
	return resetErr
}

// maxResetPayloadBytes caps the event payload size before unmarshaling, to bound
// CPU/memory of (in-process but still untrusted-by-shape) YAML/JSON parsing.
const maxResetPayloadBytes = 1 << 20 // 1 MiB

// handleClusterReset is the testable core (runner, sink and probe injected).
func handleClusterReset(event *pluggable.Event, opts ResetOptions) pluggable.EventResponse {
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

	resetErr := ResetCluster(context.Background(), *config.Cluster, opts)
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
