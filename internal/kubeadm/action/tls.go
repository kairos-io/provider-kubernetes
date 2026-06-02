package action

import "crypto/tls"

// tlsInsecure returns a TLS config that skips certificate verification. This is
// intentionally used ONLY for the /readyz liveness probe (HA-4 health gate): we
// are checking whether the API server process is healthy, not authenticating. The
// real CA-pinned kubeadm join follows immediately after this probe, restoring the
// full trust model (ADR-2 CA pinning). This is NOT used in any kubeadm invocation.
func tlsInsecure() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true} //nolint:gosec // intentional for liveness probe only
}
