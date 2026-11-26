package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/kairos-io/kairos-sdk/clusterplugin"
	"github.com/sirupsen/logrus"

	"github.com/kairos-io/provider-kubernetes/internal/kubeadm"
)

// A pin whose patch differs from the bundled kubeadm is accepted (only the minor
// is enforced), but Run must say so instead of silently targeting the binary.
func TestRunWarnsWhenAPinnedPatchIsReplaced(t *testing.T) {
	for _, tt := range []struct {
		name     string
		pin      string
		wantWarn bool
	}{
		{name: "pin with another patch", pin: "v1.37.0", wantWarn: true},
		{name: "pin equals binary", pin: "v1.37.2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hook := captureLogs(t)
			fr := &fakeRunner{respond: func(args []string) (kubeadm.Result, error) {
				if args[0] == "version" {
					return kubeadm.Result{Stdout: "v1.37.2\n"}, nil
				}
				return kubeadm.Result{}, nil
			}}
			cluster := clusterplugin.Cluster{
				Role:             clusterplugin.RoleWorker,
				ClusterToken:     validToken(),
				ControlPlaneHost: "10.0.0.1",
				ProviderOptions:  map[string]string{"cluster_root_path": t.TempDir()},
				Options: "clusterConfiguration:\n  kubernetesVersion: " + tt.pin + "\n" +
					"joinConfiguration:\n  discovery:\n    bootstrapToken:\n" +
					"      token: abcdef.0123456789abcdef\n      caCertHashes:\n      - sha256:deadbeef\n",
			}
			opts, _ := hermeticRunOptions(t, fr)
			opts.CPReachableProbe = func(_ context.Context) bool { return true }
			// A fresh node has no running kubelet and sees no cluster version yet.
			opts.ClusterVersionProbe = func(context.Context) string { return "" }
			opts.RunningKubeletVersionProbe = func(context.Context) string { return "" }
			if err := Run(context.Background(), cluster, opts); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			warned := false
			for _, e := range hook.AllEntries() {
				if e.Level == logrus.WarnLevel && strings.Contains(e.Message, "pinned kubernetesVersion "+tt.pin+" is replaced") {
					warned = true
				}
			}
			if warned != tt.wantWarn {
				t.Fatalf("pin-replaced warning logged=%v, want %v", warned, tt.wantWarn)
			}
		})
	}
}
