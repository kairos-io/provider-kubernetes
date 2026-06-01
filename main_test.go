package main

import "testing"

// TestHandleClusterResetIsSafe ensures the reset handler returns a response
// without error while it is still a no-op. Provider behavior is tested in
// internal/provider.
func TestHandleClusterResetIsSafe(t *testing.T) {
	resp := handleClusterReset(nil)
	if resp.Error != "" {
		t.Fatalf("expected no error from no-op reset handler, got %q", resp.Error)
	}
}
