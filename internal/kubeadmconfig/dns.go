package kubeadmconfig

import (
	"fmt"
	"net"
	"strings"
)

// dnsIPOffset is the host index within the service CIDR that kubeadm assigns to
// the cluster DNS (kube-dns/coredns) Service, mirroring kubeadm's GetDNSIP, which
// is GetIndexedIP(serviceCIDR, 10). We compute it one layer earlier so clusterDNS
// becomes a pure, hardware-free-testable function of the configured serviceSubnet
// (pitfall C2) and so the provider explicitly OWNS the value rather than relying
// on kubeadm's implicit derivation (which a future kubeadm change could shift).
const dnsIPOffset = 10

// DeriveDNSIP returns the cluster DNS service IP for the given serviceSubnet,
// matching kubeadm's derivation: the dnsIPOffset-th (10th) host IP of the service
// CIDR. For a dual-stack serviceSubnet ("v4cidr,v6cidr"), the DNS IP is derived
// from the FIRST (primary) CIDR, as kubeadm does. It fails fast on an empty,
// malformed, or too-small subnet rather than guessing or wrapping around.
func DeriveDNSIP(serviceSubnet string) (string, error) {
	s := strings.TrimSpace(serviceSubnet)
	if s == "" {
		return "", fmt.Errorf("serviceSubnet is empty")
	}
	cidrs := strings.Split(s, ",")
	if len(cidrs) > 2 {
		return "", fmt.Errorf("serviceSubnet %q has more than two CIDRs", serviceSubnet)
	}
	primary := strings.TrimSpace(cidrs[0])
	if primary == "" {
		return "", fmt.Errorf("serviceSubnet %q has an empty primary CIDR", serviceSubnet)
	}
	// Reject a dual-stack value with an empty secondary element (e.g. "cidr," or
	// "cidr, "): kubeadm validation rejects it, so fail fast here rather than
	// emit a config that kubeadm later refuses with a less obvious error.
	if len(cidrs) == 2 && strings.TrimSpace(cidrs[1]) == "" {
		return "", fmt.Errorf("serviceSubnet %q has an empty secondary CIDR", serviceSubnet)
	}
	_, ipnet, err := net.ParseCIDR(primary)
	if err != nil {
		return "", fmt.Errorf("parse serviceSubnet %q: %w", primary, err)
	}
	ip, err := indexedIP(ipnet, dnsIPOffset)
	if err != nil {
		return "", fmt.Errorf("serviceSubnet %q: %w", primary, err)
	}
	return ip.String(), nil
}

// indexedIP returns the IP at the given offset within ipnet, computed by
// big-endian addition on the network base address so it is correct for both IPv4
// (4-byte) and IPv6 (16-byte) and for non-zero-aligned bases (e.g. 10.96.0.64/26
// -> +10 -> 10.96.0.74). It errors when the offset falls outside the network,
// i.e. the CIDR is too small to contain that host.
func indexedIP(ipnet *net.IPNet, offset int) (net.IP, error) {
	base := ipnet.IP
	if v4 := base.To4(); v4 != nil {
		base = v4
	} else {
		base = base.To16()
	}
	if base == nil {
		return nil, fmt.Errorf("unsupported IP form")
	}
	out := make(net.IP, len(base))
	copy(out, base)
	for i, carry := len(out)-1, offset; i >= 0 && carry > 0; i-- {
		sum := int(out[i]) + carry
		out[i] = byte(sum & 0xff)
		carry = sum >> 8
		if i == 0 && carry > 0 {
			return nil, fmt.Errorf("offset %d overflows the address space", offset)
		}
	}
	if !ipnet.Contains(out) {
		return nil, fmt.Errorf("offset %d is outside the subnet (CIDR too small for the DNS IP)", offset)
	}
	return out, nil
}
