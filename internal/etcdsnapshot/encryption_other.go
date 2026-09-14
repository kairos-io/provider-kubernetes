//go:build !linux

package etcdsnapshot

import "context"

// EncryptedAtRest always reports false on non-Linux platforms. The provider only
// ships for Linux; this stub exists solely so the package builds under
// cross-platform tooling (e.g. `go vet` on a non-Linux dev machine). Fail-closed
// is the correct posture here too: an unconfirmable encryption state must never
// permit a plaintext snapshot.
func EncryptedAtRest(_ context.Context, _ string) bool {
	return false
}
