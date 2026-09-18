package provider

import (
	"context"
	"io"
	"net/http"
	"time"
)

// This file holds the production default for Options.KubeletHealthyProbe
// (D-2, PROJECT_CONTEXT.md ADR-16-A2/ADR-19 U2 security sign-off + the
// 2026-09-18 maintainer follow-up). It is the SAME check kubeadm itself waits
// on before considering a kubelet up: a loopback GET to the kubelet's own
// healthz endpoint. It is a boolean liveness probe, not a diagnostic: every
// failure mode (dial error, timeout, non-200, unreadable body) collapses to
// "not healthy" and is never surfaced as an error, so reconcile.Plan can
// never be blocked or made to fail loud by a probe hiccup (#4099-1). This is
// wired in run.go only when Options.KubeletHealthyProbe is nil; tests inject
// their own fake through that field.

const (
	// kubeletHealthzHost/kubeletHealthzPort are the kubelet's own healthz
	// defaults (k8s.io/kubernetes pkg/kubelet/apis/config/v1beta1/defaults.go:
	// healthzBindAddress "127.0.0.1", healthzPort 10248). Named so a future
	// kubelet default change is greppable from here.
	kubeletHealthzHost = "127.0.0.1"
	kubeletHealthzPort = "10248"

	// kubeletHealthzURL is the production target: loopback only, never a
	// hostname, so DNS cannot redirect it.
	kubeletHealthzURL = "http://" + kubeletHealthzHost + ":" + kubeletHealthzPort + "/healthz"

	// kubeletHealthzTimeout bounds the whole HTTP round trip. 2s is generous
	// for a loopback call and short enough that a masked/hung kubelet can
	// never make one reconcile pass (which runs this on every boot) stall.
	kubeletHealthzTimeout = 2 * time.Second

	// kubeletHealthzBodyCap bounds how much of the response body this probe
	// will read. A real healthz answer is a few bytes ("ok"); this is
	// generous headroom, not a size the endpoint is expected to approach.
	kubeletHealthzBodyCap = 4096
)

// kubeletHealthyProbe returns the production default: a probe of the
// kubelet's own loopback healthz endpoint.
func kubeletHealthyProbe() func(ctx context.Context) bool {
	return kubeletHealthyProbeAt(kubeletHealthzURL, kubeletHealthzTimeout)
}

// kubeletHealthyProbeAt is kubeletHealthyProbe's testable core: url and
// timeout are parameters so tests can point it at an httptest server and use
// a short timeout, without touching the real loopback port 10248 or the
// package-level constants.
//
// Healthy is EXACTLY "the server answered HTTP 200 and the body was fully
// readable within the bound". Everything else -- a dial error (including
// ECONNREFUSED, which is exactly the masked/stopped-kubelet case this exists
// to detect), a timeout, a non-200 status, or a body that fails to read in
// full -- reports false. No case is surfaced as an error: this is a boolean
// liveness signal, matching actualstate.FileProber's documented "nil is
// treated as not healthy" contract for this same field.
//
// Security-relevant (2026-09-18 maintainer decision): the Transport is built
// explicitly with Proxy: nil, so HTTP_PROXY/HTTPS_PROXY/NO_PROXY (upper- or
// lower-case) in the process environment can NEVER redirect this loopback
// check through a proxy. http.Transport's zero value already has a nil
// Proxy, but it is set explicitly here so the intent is not one accidental
// field addition away from a proxied, off-node health answer deciding
// whether the provider reports success -- the same class of problem
// ADR-1-A1 closed for exec argv/env.
//
// The client.Timeout bounds the call independent of ctx; the request also
// carries ctx (http.NewRequestWithContext), so reconcile's own deadline is
// honored too. Whichever fires first stops the call.
func kubeletHealthyProbeAt(url string, timeout time.Duration) func(ctx context.Context) bool {
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: nil, // never http.ProxyFromEnvironment -- see doc comment above
		},
	}
	return func(ctx context.Context) bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			// Dial error (incl. connection refused), TLS error, client-side
			// timeout, or ctx cancellation: not healthy, never an error.
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, kubeletHealthzBodyCap)); err != nil {
			return false
		}
		return resp.StatusCode == http.StatusOK
	}
}
