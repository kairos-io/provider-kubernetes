package provider

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kairos-io/kairos-sdk/clusterplugin"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
	"github.com/kairos-io/provider-kubernetes/internal/reconcile"
	"github.com/kairos-io/provider-kubernetes/internal/status"
)

// shortCPProbeTimeout shortens cpProbeTimeout for one test. Not for parallel
// tests.
func shortCPProbeTimeout(t *testing.T) {
	t.Helper()
	prev := cpProbeTimeout
	cpProbeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { cpProbeTimeout = prev })
}

// lbWithoutBackend emulates a load balancer in TCP mode with no backend up,
// HAProxy's "mode tcp" for example: it accepts the connection, reads what the
// client sends while it looks for a backend, finds none, and closes. Returns
// its "host:port".
func lbWithoutBackend(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				_ = c.SetReadDeadline(time.Now().Add(time.Second))
				buf := make([]byte, 1024)
				_, _ = c.Read(buf)
				_ = c.Close()
			}(c)
		}
	}()
	return ln.Addr().String()
}

// silentListener accepts connections and never sends a byte, until the test
// ends. Returns its "host:port".
func silentListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done); _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				<-done
				_ = c.Close()
			}(c)
		}
	}()
	return ln.Addr().String()
}

// silentTLSServer completes TLS handshakes and never answers a request, until
// the test ends. Returns its "host:port".
func silentTLSServer(t *testing.T) string {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	srv.StartTLS()
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs first: lets the handlers return
	return srv.Listener.Addr().String()
}

// returnsWithin runs probe and fails the test if it has not returned within
// limit, so a probe that ignores its bound fails here rather than hanging the
// suite.
func returnsWithin(t *testing.T, limit time.Duration, probe func() bool) bool {
	t.Helper()
	got := make(chan bool, 1)
	go func() { got <- probe() }()
	select {
	case v := <-got:
		return v
	case <-time.After(limit):
		t.Fatalf("probe still running after %v; it must return within its bound", limit)
		return false
	}
}

func tlsServerAnswering(t *testing.T, code int) string {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func TestCPReachableProbeEmptyEndpointIsNil(t *testing.T) {
	if makeCPReachableProbe("") != nil {
		t.Fatal("an empty endpoint must give a nil probe, which callers treat as unreachable")
	}
}

// The regression: the old probe was a bare TCP dial, so a load balancer that
// accepts before any backend exists read as a control plane and the first
// role: init was refused for good.
func TestCPReachableProbeLoadBalancerWithoutBackendIsNotServing(t *testing.T) {
	ep := lbWithoutBackend(t)
	if makeCPReachableProbe(ep)(context.Background()) {
		t.Fatalf("a TCP load balancer without a backend at %s counted as a control plane", ep)
	}
}

// A real apiserver answers /version with 200, or 401/403 when anonymous
// requests are off; any answer other than a gateway error counts, so the guard
// errs towards refusing an init.
func TestCPReachableProbeAnswers(t *testing.T) {
	for _, tc := range []struct {
		code int
		want bool
	}{
		{http.StatusOK, true},
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, true},
		{http.StatusNotFound, true},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, false},
		{http.StatusServiceUnavailable, false},
		{http.StatusGatewayTimeout, false},
	} {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			ep := tlsServerAnswering(t, tc.code)
			if got := makeCPReachableProbe(ep)(context.Background()); got != tc.want {
				t.Fatalf("GET /version answered %d: serving=%v, want %v", tc.code, got, tc.want)
			}
		})
	}
}

func TestCPReachableProbeSendsAPlainVersionRequest(t *testing.T) {
	type seen struct {
		method, path, auth, agent string
		hasCert                   bool
	}
	got := make(chan seen, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.UserAgent(), r.TLS != nil && len(r.TLS.PeerCertificates) > 0}
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	if !makeCPReachableProbe(srv.Listener.Addr().String())(context.Background()) {
		t.Fatal("a 403 from an apiserver must count as serving")
	}
	var s seen
	select {
	case s = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the probe counted a control plane without sending a request")
	}
	if s.method != http.MethodGet || s.path != "/version" {
		t.Fatalf("probe sent %s %s, want GET /version", s.method, s.path)
	}
	if s.auth != "" || s.hasCert {
		t.Fatalf("probe sent credentials (Authorization=%q, client cert=%v); it must send none", s.auth, s.hasCert)
	}
	if s.agent != cpProbeUserAgent {
		t.Fatalf("probe sent User-Agent %q, want %q so audit logs can name it", s.agent, cpProbeUserAgent)
	}
}

// A gateway error whose body is a Kubernetes Status object comes from an
// apiserver, not from a load balancer without a backend: still serving.
func TestCPReachableProbeAPIServerGatewayErrorCountsAsServing(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"apiserver is shutting down","reason":"ServiceUnavailable","code":503}`))
	}))
	t.Cleanup(srv.Close)
	if !makeCPReachableProbe(srv.Listener.Addr().String())(context.Background()) {
		t.Fatal("an apiserver's own 503 was taken for a load balancer without a backend")
	}
}

// Whatever answers is untrusted (the certificate is not verified), so the
// probe reads a bounded amount: an answer whose headers never end is cut off
// at cpProbeReadCap, well inside the time bound, and counts as serving.
func TestCPReachableProbeBoundsWhatItReads(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nX-Flood: ")
		chunk := []byte(strings.Repeat("a", 32<<10))
		for {
			select {
			case <-release:
				return
			default:
			}
			if _, err := buf.Write(chunk); err != nil {
				return
			}
			if err := buf.Flush(); err != nil {
				return
			}
		}
	}))
	srv.StartTLS()
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	probe := makeCPReachableProbe(srv.Listener.Addr().String())
	// cpProbeTimeout is not shortened: returning well inside it shows the read
	// stopped at the cap, not at the time bound.
	if !returnsWithin(t, 2*time.Second, func() bool { return probe(context.Background()) }) {
		t.Fatal("a TLS server flooding its answer was counted as no control plane")
	}
}

func TestCPReachableProbeIPv6Endpoint(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.StartTLS()
	t.Cleanup(srv.Close)
	ep := ln.Addr().String()
	if !strings.HasPrefix(ep, "[::1]:") {
		t.Fatalf("listener address %q, want [::1]:port", ep)
	}
	if !makeCPReachableProbe(ep)(context.Background()) {
		t.Fatalf("an apiserver at the IPv6 endpoint %s was not counted", ep)
	}
}

func TestCPReachableProbeNothingListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ep := ln.Addr().String()
	_ = ln.Close()
	if makeCPReachableProbe(ep)(context.Background()) {
		t.Fatalf("nothing listens at %s, yet the probe reported a control plane", ep)
	}
}

// An apiserver always serves TLS; a plain-HTTP server is not one.
func TestCPReachableProbePlainHTTPIsNotServing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	if makeCPReachableProbe(srv.Listener.Addr().String())(context.Background()) {
		t.Fatal("a plain-HTTP server counted as a control plane")
	}
}

// A load balancer that accepts and holds the connection (queueing it for a
// backend that never comes) never completes a handshake: not serving, and the
// probe returns within its bound.
func TestCPReachableProbeNoHandshakeIsNotServingAndBounded(t *testing.T) {
	shortCPProbeTimeout(t)
	probe := makeCPReachableProbe(silentListener(t))
	if returnsWithin(t, 5*cpProbeTimeout, func() bool { return probe(context.Background()) }) {
		t.Fatal("an endpoint that never completes a TLS handshake counted as a control plane")
	}
}

// Once a TLS handshake completes, something terminates TLS for the endpoint;
// an apiserver too slow to answer still counts, so a busy control plane is
// never mistaken for none.
func TestCPReachableProbeSilentTLSServerCountsAsServing(t *testing.T) {
	shortCPProbeTimeout(t)
	probe := makeCPReachableProbe(silentTLSServer(t))
	if !returnsWithin(t, 5*cpProbeTimeout, func() bool { return probe(context.Background()) }) {
		t.Fatal("a TLS server that completed the handshake but did not answer in time was counted as no control plane")
	}
}

// A canceled caller gets false, as a failed dial did, never a "serving" that
// would refuse an init on the way down.
func TestCPReachableProbeCanceledCallerIsNotServing(t *testing.T) {
	probe := makeCPReachableProbe(silentTLSServer(t))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	// cpProbeTimeout is not shortened here: returning well inside it shows the
	// probe noticed the cancellation rather than waiting out its own bound.
	if returnsWithin(t, 2*time.Second, func() bool { return probe(ctx) }) {
		t.Fatal("a canceled caller was told a control plane answers")
	}
}

// End to end through Run with the production probe: role: init behind a
// load balancer that has no backend yet initializes, where the TCP probe
// refused it terminally.
func TestRunInitBehindLoadBalancerWithoutBackend(t *testing.T) {
	ep := lbWithoutBackend(t)
	fr := &fakeRunner{respond: func(args []string) (kubeadm.Result, error) {
		switch {
		case args[0] == "version":
			return kubeadm.Result{Stdout: "v1.37.0\n"}, nil
		case args[0] == "token" && args[1] == "generate":
			return kubeadm.Result{Stdout: "abcdef.0123456789abcdef\n"}, nil
		case args[0] == "certs":
			return kubeadm.Result{Stdout: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"}, nil
		}
		return kubeadm.Result{}, nil
	}}
	cluster := clusterplugin.Cluster{
		Role:             clusterplugin.RoleInit,
		ClusterToken:     validToken(),
		ControlPlaneHost: ep,
		ProviderOptions:  map[string]string{"cluster_root_path": t.TempDir()},
	}
	opts, sink := hermeticRunOptions(t, fr)
	opts.CPReachableProbe = nil // the production probe, against ep
	if err := Run(context.Background(), cluster, opts); err != nil {
		t.Fatalf("role: init behind a load balancer without a backend failed: %v", err)
	}
	if !fr.called("init") {
		t.Fatalf("expected kubeadm init; calls=%v", fr.calls)
	}
	if got := sink.only(t); got.Phase != status.PhaseConverged || got.LastAction != string(reconcile.ActionRunInit) {
		t.Fatalf("recorded phase=%q reason=%q lastAction=%q, want %q after %q", got.Phase, got.Reason, got.LastAction, status.PhaseConverged, reconcile.ActionRunInit)
	}
}

// And the guard still holds through Run: an apiserver at the endpoint, even
// one refusing anonymous requests, refuses the init terminally.
func TestRunInitRefusedWhenAnAPIServerAnswers(t *testing.T) {
	ep := tlsServerAnswering(t, http.StatusUnauthorized)
	fr := &fakeRunner{respond: func(args []string) (kubeadm.Result, error) {
		if args[0] == "version" {
			return kubeadm.Result{Stdout: "v1.37.0\n"}, nil
		}
		return kubeadm.Result{}, nil
	}}
	cluster := clusterplugin.Cluster{
		Role:             clusterplugin.RoleInit,
		ClusterToken:     validToken(),
		ControlPlaneHost: ep,
		ProviderOptions:  map[string]string{"cluster_root_path": t.TempDir()},
	}
	opts, sink := hermeticRunOptions(t, fr)
	opts.CPReachableProbe = nil
	err := Run(context.Background(), cluster, opts)
	if !errors.Is(err, reconcile.ErrTerminal) {
		t.Fatalf("Run error = %v, want a terminal refusal", err)
	}
	if fr.called("init") {
		t.Fatal("kubeadm init ran against an endpoint where an apiserver answers")
	}
	if got := sink.only(t); got.Reason != status.ReasonInitRefused {
		t.Fatalf("recorded reason=%q, want %q", got.Reason, status.ReasonInitRefused)
	}
}
