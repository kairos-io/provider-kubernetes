package kubeadmconfig

import "testing"

func TestDeriveDNSIP(t *testing.T) {
	tests := []struct {
		name    string
		subnet  string
		want    string
		wantErr bool
	}{
		// kubeadm default-equivalent and custom IPv4 subnets -> 10th host IP.
		{"default /12", "10.96.0.0/12", "10.96.0.10", false},
		{"custom /16", "10.0.0.0/16", "10.0.0.10", false},
		{"custom non-default base /16", "172.20.0.0/16", "172.20.0.10", false},
		{"custom /24", "10.96.0.0/24", "10.96.0.10", false},
		// Boundary: /28 (16 hosts) -- index 10 is the last comfortably-usable host.
		{"boundary /28", "10.96.0.0/28", "10.96.0.10", false},
		// Non-zero-aligned base: offset adds to the network address (regression
		// guard for byte-wise addition vs. naive last-octet set).
		{"non-zero-aligned /20", "192.168.128.0/20", "192.168.128.10", false},
		{"non-zero-aligned /26", "10.96.0.64/26", "10.96.0.74", false},
		// IPv6.
		{"ipv6 /108", "fd00::/108", "fd00::a", false},
		// Dual-stack: derive from the primary (first) CIDR.
		{"dual-stack v4 primary", "10.96.0.0/12,fd00::/108", "10.96.0.10", false},
		{"dual-stack v6 primary", "fd00::/108,10.96.0.0/12", "fd00::a", false},
		// Tolerate whitespace around the dual-stack split.
		{"dual-stack with spaces", "10.96.0.0/12 , fd00::/108", "10.96.0.10", false},
		// Errors: fail fast, never guess.
		{"empty", "", "", true},
		{"whitespace only", "   ", "", true},
		{"garbage", "not-a-cidr", "", true},
		{"bare ip no mask", "10.96.0.0", "", true},
		{"too small ipv4 /29", "10.96.0.0/29", "", true},
		{"too small ipv6 /125", "fd00::/125", "", true},
		{"three cidrs", "10.96.0.0/12,fd00::/108,10.1.0.0/16", "", true},
		{"empty primary", ",fd00::/108", "", true},
		{"trailing comma empty secondary", "10.96.0.0/12,", "", true},
		{"empty secondary with space", "10.96.0.0/12, ", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DeriveDNSIP(tt.subnet)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("DeriveDNSIP(%q) = %q, want error", tt.subnet, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("DeriveDNSIP(%q) unexpected error: %v", tt.subnet, err)
			}
			if got != tt.want {
				t.Fatalf("DeriveDNSIP(%q) = %q, want %q", tt.subnet, got, tt.want)
			}
		})
	}
}
