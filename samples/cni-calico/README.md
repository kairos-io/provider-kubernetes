# Calico CNI example

provider-kubernetes bootstraps the control plane and joins nodes, but it
installs **no CNI** — pod networking is the operator's choice (Calico, Cilium,
Flannel, ...). Until a CNI is installed, nodes stay `NotReady` and CoreDNS stays
`Pending`. This is by design.

This example installs **Calico v3.32.0** via the Tigera operator. Calico v3.32
is tested against Kubernetes 1.34 / 1.35 / 1.36 — the provider's supported
window.

## Install

Run on the control-plane node (or anywhere with the cluster's admin kubeconfig):

```sh
KUBECONFIG=/etc/kubernetes/admin.conf

# 1. Install the Tigera operator (pinned).
kubectl create -f https://raw.githubusercontent.com/projectcalico/calico/v3.32.0/manifests/tigera-operator.yaml

# 2. Apply the Installation matched to this repo's sample podSubnet.
kubectl create -f installation.yaml

# 3. Wait for Calico to come up and the nodes to go Ready.
kubectl -n calico-system wait --for=condition=Ready pod --all --timeout=300s
kubectl wait --for=condition=Ready node --all --timeout=300s
```

## Why these settings

- **`cidr: 10.244.0.0/16`** matches `networking.podSubnet` in
  `samples/master.yaml` / `samples/cluster.yaml`. Calico's IP pool must fall
  within the cluster pod CIDR that kubeadm was initialized with. If you change
  one, change the other.
- **`encapsulation: VXLANCrossSubnet`** tunnels pod traffic only between nodes
  on different subnets and routes natively within a subnet. VXLAN needs neither
  BGP nor any fabric cooperation, so it works on a plain bridged or NAT'd LAN
  (the libvirt host-bridge the repo's multi-node walkthrough uses). Switch to
  BGP/IPIP if your environment calls for it.
- **Operator install, not the flat manifest.** The Tigera operator is Calico's
  recommended install path; the `Installation` CR is the supported way to set
  the pod CIDR declaratively rather than editing a generated DaemonSet env var.

## Pinning

`v3.32.0` is pinned deliberately (the same supply-chain discipline as the image
build). Bump it consciously and re-check the Calico/Kubernetes compatibility
matrix at https://docs.tigera.io/calico/latest/getting-started/kubernetes/requirements
before moving to a newer Kubernetes minor.
