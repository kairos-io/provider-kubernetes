# Flannel CNI example

provider-kubernetes bootstraps the control plane and joins nodes, but it installs
no CNI: pod networking is the operator's choice (Flannel, Calico, Cilium, ...).
Until a CNI is installed, nodes stay `NotReady` and CoreDNS stays `Pending`. That
is by design, not a bug. kubeadm ships no CNI either, and staying CNI-agnostic is
how the provider avoids vendor lock-in.

This directory installs **Flannel v0.28.5**: a single upstream manifest, no
operator, no CRDs. Flannel's default pod network is `10.244.0.0/16`, which
already matches the `podSubnet` used across these samples, so for a lab or
single-node cluster it is the shortest path to a `Ready` node. For network
policy, eBPF, or HA-oriented deployments, see
[`../cni-calico`](../cni-calico).

Two ways to apply it:

| File | Approach | When |
|------|----------|------|
| (this README, "apply by hand") | `kubectl apply` the pinned manifest after the cluster is up. | Explicit/staged control of when the CNI lands, or a GitOps tool owns CNI. |
| `cluster-with-flannel.yaml` | Bundled in the control-plane cloud-config: the node installs Flannel itself, no follow-up `kubectl`. | One declarative file that yields a `Ready` cluster unattended. |

## Prerequisites

- `clusterConfiguration.networking.podSubnet` must be `10.244.0.0/16` (Flannel's
  default). It already is in these samples. If you change it, you must also edit
  Flannel's `net-conf.json` `Network` field (the `kube-flannel-cfg` ConfigMap in
  the manifest) to match, or pods won't get routable IPs.
- The provider image already loads `br_netfilter` and sets the required sysctls
  (`net.bridge.bridge-nf-call-iptables=1`, `net.ipv4.ip_forward=1`), which Flannel
  needs, so there is no extra host setup.

## Approach A: apply after the cluster is up

Run on the control-plane node (or anywhere with the cluster's admin kubeconfig):

```sh
export KUBECONFIG=/etc/kubernetes/admin.conf

# Pinned Flannel release (verify the current one at
# https://github.com/flannel-io/flannel/releases).
kubectl apply -f https://github.com/flannel-io/flannel/releases/download/v0.28.5/kube-flannel.yml
```

Within a minute the node should flip to `Ready` and CoreDNS should schedule:

```sh
kubectl get nodes            # STATUS: Ready
kubectl get pods -A          # coredns Running; kube-flannel-ds Running
```

## Approach B: bundled in the cloud-config

Boot the control-plane node with [`cluster-with-flannel.yaml`](./cluster-with-flannel.yaml).
The provider runs `kubeadm init`, and a self-contained systemd one-shot waits for
the local API to be healthy, then applies the pinned Flannel manifest. It is
idempotent (a sentinel skips re-apply on reboot) and a graceful no-op on
non-control-plane nodes (no `admin.conf` there). Edit `control_plane_host`, the
endpoint, and `cluster_token` before use.

## Air-gap

The manifest URL and the Flannel images need outbound internet. The control-plane
images bundled in the OS image do not cover a CNI, so on an air-gapped cluster
mirror `kube-flannel.yml` and the Flannel images internally and point the apply at
your local copy. See [`../air-gapped/`](../air-gapped/).
