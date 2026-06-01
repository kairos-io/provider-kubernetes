package main

import (
	"testing"

	"github.com/kairos-io/kairos-sdk/clusterplugin"
)

// TestProviderIsInertButValid is the hardware-free smoke test for the provider
// entrypoint. It proves the contract holds (provider() accepts a Cluster and
// returns a YipConfig) without panicking. As real stage generation lands, this
// grows into behavior assertions on the produced YipConfig.
func TestProviderIsInertButValid(t *testing.T) {
	cluster := clusterplugin.Cluster{
		Role:             clusterplugin.RoleInit,
		ControlPlaneHost: "10.0.0.1",
		ClusterToken:     "test-token",
	}

	cfg := provider(cluster)

	if cfg.Name == "" {
		t.Fatal("expected provider to return a named YipConfig")
	}
}

// TestHandleClusterResetIsSafe ensures the reset handler returns a response
// without error while it is still a no-op.
func TestHandleClusterResetIsSafe(t *testing.T) {
	resp := handleClusterReset(nil)
	if resp.Error != "" {
		t.Fatalf("expected no error from no-op reset handler, got %q", resp.Error)
	}
}
