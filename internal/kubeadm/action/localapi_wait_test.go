package action

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadmconfig"
	"github.com/kairos-io/provider-kubernetes/internal/reconcile"
	"github.com/kairos-io/provider-kubernetes/internal/reconcile/actualstate"
)

// healthzServerPort starts a TLS /healthz server on loopback and returns its port.
func healthzServerPort(t *testing.T) int32 {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse %q: %v", srv.URL, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port of %q: %v", srv.URL, err)
	}
	return int32(p)
}

// The post-repair wait is the built-in probe (LocalAPIReachable nil). It must
// poll the port the cluster pinned in localAPIEndpoint.bindPort; polling 6443
// on a cluster that binds elsewhere never sees the control plane return and
// fails the upgrade with a timeout.
func TestWaitForLocalAPIHealthy_UsesBindPort(t *testing.T) {
	port := healthzServerPort(t)
	if port == 6443 {
		t.Skip("httptest picked the default port; nothing to distinguish")
	}
	e := &KubeadmExecutor{
		Runner:   &fakeRunner{},
		RunDir:   t.TempDir(),
		Role:     actualstate.RoleControlPlane,
		Input:    kubeadmconfig.Input{ControlPlaneEndpoint: "10.0.0.1:6443", BindPort: port},
		RootPath: "/",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.Execute(ctx, reconcile.ActionRepairKubeletConfig); err != nil {
		t.Fatalf("repair with bindPort %d: %v", port, err)
	}
}

// With no server on the configured port the wait still ends within ctx.
func TestWaitForLocalAPIHealthy_BoundedOnWrongPort(t *testing.T) {
	e := &KubeadmExecutor{
		Runner: &fakeRunner{},
		RunDir: t.TempDir(),
		Role:   actualstate.RoleControlPlane,
		Input:  kubeadmconfig.Input{ControlPlaneEndpoint: "10.0.0.1:6443", BindPort: 6444},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := e.Execute(ctx, reconcile.ActionRepairKubeletConfig); err == nil {
		t.Fatal("expected a bounded error when nothing answers on the configured port")
	}
}
