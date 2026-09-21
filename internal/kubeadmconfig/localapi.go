package kubeadmconfig

import (
	"net"
	"strconv"
)

// DefaultAPIServerBindPort is kubeadm's own default for
// InitConfiguration.localAPIEndpoint.bindPort (and the control-plane half of
// JoinConfiguration). Named so a future kubeadm default change is greppable
// from here, in the style of kubeletHealthzPort in internal/provider.
const DefaultAPIServerBindPort int32 = 6443

// localAPIHealthzHost is the target for every LOCAL apiserver liveness probe:
// a loopback literal, never a hostname, so DNS cannot redirect the check off
// the node.
const localAPIHealthzHost = "127.0.0.1"

// LocalAPIHealthzURL returns the /healthz URL of the apiserver running on THIS
// node for a given localAPIEndpoint.bindPort.
//
// bindPort is what kubeadm renders as the apiserver's --secure-port, so a
// cluster that pins a non-default port serves /healthz only there. A
// non-positive port means the operator left the field unset, which kubeadm
// fills with its own default.
func LocalAPIHealthzURL(bindPort int32) string {
	port := bindPort
	if port <= 0 {
		port = DefaultAPIServerBindPort
	}
	return "https://" + net.JoinHostPort(localAPIHealthzHost, strconv.Itoa(int(port))) + "/healthz"
}
