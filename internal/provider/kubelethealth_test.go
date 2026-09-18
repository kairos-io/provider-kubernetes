package provider

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestKubeletHealthyProbeAt is table-driven over the straightforward
// request/response outcomes: exactly HTTP 200 is healthy, everything else is
// not, and neither case is ever surfaced as an error.
func TestKubeletHealthyProbeAt(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		want       bool
	}{
		{name: "200 OK is healthy", statusCode: http.StatusOK, want: true},
		{name: "500 is not healthy", statusCode: http.StatusInternalServerError, want: false},
		{name: "503 is not healthy", statusCode: http.StatusServiceUnavailable, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.statusCode)
				_, _ = w.Write([]byte("ok"))
			}))
			defer srv.Close()

			probe := kubeletHealthyProbeAt(srv.URL+"/healthz", 2*time.Second)
			if got := probe(context.Background()); got != tc.want {
				t.Fatalf("probe() = %v, want %v (status %d)", got, tc.want, tc.statusCode)
			}
		})
	}
}

// TestKubeletHealthyProbeConnectionRefusedIsNotHealthy is the case this probe
// exists to detect: a masked/stopped kubelet. Nothing listens on the target
// port, so the dial itself fails -- this must read as "not healthy", never as
// an error surfaced to the caller.
func TestKubeletHealthyProbeConnectionRefusedIsNotHealthy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	// Nothing listens on addr now (the listener above was only used to
	// reserve and immediately release a free loopback port).

	probe := kubeletHealthyProbeAt("http://"+addr+"/healthz", 2*time.Second)
	if got := probe(context.Background()); got {
		t.Fatal("probe() = true, want false (connection refused is the masked/stopped kubelet case)")
	}
}

// TestKubeletHealthyProbeTimeoutIsNotHealthy proves a handler that never
// answers within the bound is reported not healthy, AND that the call itself
// returns promptly at the bound rather than hanging (#4099-1: this runs on
// every reconcile pass and must never be able to stall one).
func TestKubeletHealthyProbeTimeoutIsNotHealthy(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block // held open past the probe's timeout
		w.WriteHeader(http.StatusOK)
	}))
	// httptest.Server.Close blocks until outstanding requests complete, so
	// block MUST be closed (unblocking the handler above) before Close runs --
	// close it explicitly first rather than relying on defer LIFO order.
	defer func() {
		close(block)
		srv.Close()
	}()

	const bound = 100 * time.Millisecond
	probe := kubeletHealthyProbeAt(srv.URL+"/healthz", bound)

	start := time.Now()
	got := probe(context.Background())
	elapsed := time.Since(start)

	if got {
		t.Fatal("probe() = true, want false (handler never answered within the bound)")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("probe() took %v to return, want it bounded near the %v timeout", elapsed, bound)
	}
}

// TestKubeletHealthyProbeUnreadableBodyIsNotHealthy: a response that
// announces more body than it delivers (Content-Length lies, then the
// connection is severed) must read as not healthy -- the body-read error path,
// distinct from a dial error or a non-200 status.
func TestKubeletHealthyProbeUnreadableBodyIsNotHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("short"))
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("ResponseWriter does not support Hijack; cannot simulate a truncated body")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("Hijack: %v", err)
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	probe := kubeletHealthyProbeAt(srv.URL+"/healthz", 2*time.Second)
	if got := probe(context.Background()); got {
		t.Fatal("probe() = true, want false (the promised body never fully arrived)")
	}
}

// TestKubeletHealthyProbeRespectsContextCancellation proves ctx is honored
// independently of the client's own Timeout: a long client timeout must not
// prevent an earlier ctx cancellation from stopping the call promptly.
func TestKubeletHealthyProbeRespectsContextCancellation(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	// httptest.Server.Close blocks until outstanding requests complete, so
	// block MUST be closed (unblocking the handler above) before Close runs --
	// close it explicitly first rather than relying on defer LIFO order.
	defer func() {
		close(block)
		srv.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	probe := kubeletHealthyProbeAt(srv.URL+"/healthz", 30*time.Second) // long client timeout on purpose

	done := make(chan bool, 1)
	go func() { done <- probe(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case got := <-done:
		if got {
			t.Fatal("probe() = true, want false after context cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("probe() did not return promptly after context cancellation")
	}
}

// TestKubeletHealthyProbeIgnoresProxyEnv is D-2's security-relevant test
// (2026-09-18 maintainer decision): HTTP_PROXY/HTTPS_PROXY (and their
// lower-case forms, which Go's http.ProxyFromEnvironment also honors) must
// NEVER decide where this health check lands. A proxied answer would let an
// off-node endpoint decide whether the provider reports success.
//
// The discriminating case is deliberately NOT a loopback target: Go's own
// ProxyFromEnvironment never proxies localhost/127.0.0.1, so a loopback
// target passes whether Transport.Proxy is nil or ProxyFromEnvironment, and
// a test written that way proves nothing about our code (it was written that
// way first, and flipping Proxy to ProxyFromEnvironment did not fail it).
// Pointing the probe at a non-loopback address is what makes the setting
// observable: with Proxy nil the request goes straight at that address and
// fails; with ProxyFromEnvironment it would be answered 200 by the rogue
// proxy instead.
func TestKubeletHealthyProbeIgnoresProxyEnv(t *testing.T) {
	var proxyHit atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()

	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(key, proxy.URL)
	}

	t.Run("non-loopback target is never proxied", func(t *testing.T) {
		// 192.0.2.0/24 is TEST-NET-1 (RFC 5737): reserved for documentation
		// and guaranteed not to be routable, so the direct dial can only
		// fail. Any 200 here could only have come from the rogue proxy.
		probe := kubeletHealthyProbeAt("http://192.0.2.10:10248/healthz", 500*time.Millisecond)
		got := probe(context.Background())
		if proxyHit.Load() {
			t.Error("the request went through the proxy named by HTTP_PROXY; Transport.Proxy must be nil so no off-node endpoint can answer a health check")
		}
		if got {
			t.Errorf("probe() = true for an unroutable address, want false (a true here means something other than the kubelet answered)")
		}
	})

	t.Run("loopback target answers directly", func(t *testing.T) {
		real := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer real.Close()

		probe := kubeletHealthyProbeAt(real.URL+"/healthz", 2*time.Second)
		if got := probe(context.Background()); !got {
			t.Fatal("probe() = false, want true (the real target answers 200 OK)")
		}
		if proxyHit.Load() {
			t.Fatal("a loopback request went through the proxy env's target")
		}
	})
}

// TestKubeletHealthyProbeProductionDefaultUsesLoopback10248 pins the
// production constants against silent drift: kubeletHealthyProbe() must be
// wired to exactly the kubelet's documented healthz defaults.
func TestKubeletHealthyProbeProductionDefaultUsesLoopback10248(t *testing.T) {
	if kubeletHealthzURL != "http://127.0.0.1:10248/healthz" {
		t.Fatalf("kubeletHealthzURL = %q, want the kubelet's documented healthz default", kubeletHealthzURL)
	}
}
