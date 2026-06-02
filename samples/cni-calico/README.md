# Calico CNI example

provider-kubernetes bootstraps the control plane and joins nodes, but it
installs **no CNI** — pod networking is the operator's choice (Calico, Cilium,
Flannel, ...). Until a CNI is installed, nodes stay `NotReady` and CoreDNS stays
`Pending`. This is by design.

This directory installs **Calico v3.32.0** via the Tigera operator. Calico v3.32
is tested against Kubernetes 1.34 / 1.35 / 1.36 — the provider's supported
window. Two ways to apply it:

| File | Approach | When |
|------|----------|------|
| `installation.yaml` | Apply by hand after the cluster is up (`kubectl create` the operator, then `kubectl apply` this CR). | You want explicit, staged control of when the CNI lands, or a GitOps tool owns CNI. |
| `cluster-with-calico.yaml` | Bundled in the control-plane cloud-config — the node installs Calico itself, no follow-up `kubectl`. | You want one declarative file that yields a Ready cluster unattended. |

## Approach A — apply after the cluster is up

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

## Approach B — bundled in the cloud-config (`cluster-with-calico.yaml`)

`cluster-with-calico.yaml` is a complete `role: init` control-plane cloud-config
that ALSO installs Calico, so a single file yields a Ready cluster with no manual
`kubectl`. It works like this:

- The provider runs `kubeadm init` in its `network.after` stage (as always).
- The cloud-config additionally installs a small **systemd one-shot**
  (`provider-kubernetes-cni-install.service` + `/usr/local/bin/provider-kubernetes-install-cni.sh`)
  and a `boot`-stage `systemctl enable --now`.
- That unit **self-waits** for `admin.conf` + a ready API, then server-side-applies
  the Tigera operator and the `Installation` CR, and touches a sentinel so it does
  not re-apply on reboot.

Why a self-waiting unit rather than another yip stage command: it is independent
of stage ordering relative to the provider's reconcile (no deadlock if it would
otherwise run first), it never blocks a boot stage, and it is idempotent across
reboots. On a node that never becomes a control plane (no `admin.conf`) it is a
graceful no-op.

Requirements and caveats:
- The control-plane node needs **outbound internet** (it fetches the pinned
  `tigera-operator.yaml` and Calico images). For airgapped installs, mirror the
  operator manifest + images and point `OPERATOR_URL` at your mirror.
- Edit `cluster_token`, `control_plane_host`, and `kubernetesVersion` as usual.
- Apply only to the **first** control-plane node; workers join with the
  `mint-join`-produced config and need no CNI step.

```sh
# Boot the first control-plane node from an image built with this repo's
# Dockerfile, using cluster-with-calico.yaml as its cloud-config. Once it is up
# the node is Ready on its own (no kubectl apply needed):
kubectl --kubeconfig /etc/kubernetes/admin.conf get nodes
# journalctl -u provider-kubernetes-cni-install.service   # to watch the bundled install
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
