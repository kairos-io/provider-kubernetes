package kubeadmconfig

import "testing"

func TestLocalAPIHealthzURL(t *testing.T) {
	cases := []struct {
		name     string
		bindPort int32
		want     string
	}{
		{"unset falls back to the kubeadm default", 0, "https://127.0.0.1:6443/healthz"},
		{"explicit default", 6443, "https://127.0.0.1:6443/healthz"},
		{"custom bindPort", 6444, "https://127.0.0.1:6444/healthz"},
		{"high port", 16443, "https://127.0.0.1:16443/healthz"},
		{"negative is treated as unset", -1, "https://127.0.0.1:6443/healthz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LocalAPIHealthzURL(tc.bindPort); got != tc.want {
				t.Fatalf("LocalAPIHealthzURL(%d) = %q, want %q", tc.bindPort, got, tc.want)
			}
		})
	}
}

// The host is a loopback literal, never a name, so DNS cannot redirect the
// probe off the node.
func TestLocalAPIHealthzURL_IsLoopbackLiteral(t *testing.T) {
	if got := LocalAPIHealthzURL(6444); got[:len("https://127.0.0.1:")] != "https://127.0.0.1:" {
		t.Fatalf("probe target is not a loopback literal: %q", got)
	}
}
