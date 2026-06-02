# Multi-control-plane (HA, stacked-etcd) walkthrough

How to stand up a 3-control-plane, stacked-etcd kubeadm cluster with
provider-kubernetes. **This provider is under active development and is NOT
field-ready** -- treat this as a design demo.

| File | Role | Notes |
|------|------|-------|
| `init-cp1.yaml`        | `init`         | First control plane; runs `kubeadm init` with a STABLE endpoint. |
| `controlplane-cp2.yaml`| `controlplane` | Additional control plane; joins via token + CA pin + cert key + own advertiseAddress. |

cp3 (and beyond) is `controlplane-cp2.yaml` with its own `control_plane_host`
unchanged (the shared VIP) but its own `localAPIEndpoint.advertiseAddress`.

## The one hard prerequisite: a stable control-plane endpoint

HA is only possible if the cluster is initialized with a
`controlPlaneEndpoint` that is **stable and outlives any single node** -- a VIP,
an external L4 load balancer, or a health-checked DNS name fronting all control
planes. Every control plane and worker talks to the API server through this
address, and it is baked into the API server serving certificate.

The provider **does not ship a load balancer** (no vendor lock-in; ADR-11).
Provisioning the endpoint is the operator's job. Common options:

- **kube-vip** (in-cluster static pod advertising a VIP) -- a popular choice; run
  it as a static pod manifest delivered via cloud-config. Doc-only here; not a
  dependency of this provider.
- **keepalived** for a VRRP VIP across the control-plane nodes.
- An external/cloud **L4 load balancer** targeting all control-plane node IPs on
  6443.
- A health-checked **DNS** record.

If you initialize without a stable endpoint, the provider warns at `role: init`
and additional control planes cannot join later. You cannot retrofit an endpoint
into a live cluster (it requires re-issuing all API server certs) -- start over
with a stable endpoint.

## Bring-up: ONE control plane at a time

etcd membership changes must be sequential. Adding two control planes at once
can break quorum, and concurrent joins race the shared `kubeadm-certs` secret.
The provider does NOT implement a distributed lock (that risks hangs); instead
sequential bring-up is the supported operator procedure, backed by a bounded
node-local `/readyz` health gate before each control-plane join.

1. **Boot cp1** from `init-cp1.yaml`. Wait until it is fully Ready
   (`kubectl get nodes`, all control-plane pods Running, etcd healthy).
2. **Mint cp2's join material on cp1, just before booting cp2:**
   ```sh
   sudo agent-provider-kubernetes mint-join --role controlplane --ttl 1h \
     --advertise-address 192.168.1.12 \
     --cluster-token "<the cluster's cluster_token>"
   ```
   This mints a bootstrap token + CA SPKI pin AND re-uploads the cluster certs
   under a FRESH certificate key (so the key matches etcd), then prints a
   cloud-config. Paste its values into `controlplane-cp2.yaml`.
3. **Boot cp2.** Wait until it is Ready and shows as a second control plane
   (`kubectl get nodes`, `kubectl -n kube-system get pods -o wide`, etcd shows 2
   members and is healthy).
4. **Repeat steps 2-3 for cp3** (re-mint fresh material; set cp3's own
   `--advertise-address`). End state: 3 control planes, 3 healthy etcd members,
   odd-sized quorum.

Re-mint fresh material for each node near its boot time: the token (default 1h)
and the kubeadm-certs upload (upstream 2h expiry, deliberately not stripped) are
intentionally short-lived. An expired bundle simply means "mint again."

## Why the certificate key is special

A worker join token lets a node join as a `system:node`, bounded by a TTL. A
control-plane join bundle is far more dangerous: its `certificateKey` decrypts
the uploaded cluster PKI **including the CA private key**, and joining grants the
node full etcd membership (read/write of every Secret). A leaked control-plane
bundle is full, persistent cluster takeover. Handle it as a root credential:
confidential + integrity-protected delivery, fresh per join, never persisted,
never logged, never committed. The provider performs **no node attestation in
v1** -- the delivery channel is the trust boundary.

The transient config the provider writes (which embeds the key) is `0600` on
tmpfs (`/run`) and shredded immediately after the kubeadm exec returns; the key
never appears on the process command line. `/run` MUST be tmpfs (it is, on every
supported Kairos image) -- this is a load-bearing precondition for a
control-plane node, because that file decrypts the CA key.

## Removing a control plane / reset

Removing a control plane is a TWO-part operation:

1. **Local reset** on the node (the provider's `EventClusterReset`, or
   `agent-provider-kubernetes` reset path) runs a bounded `kubeadm reset` and
   wipes the local kubeadm artifacts (including `/var/lib/etcd` and the local
   PKI).
2. **etcd member deregistration.** When the cluster is reachable at reset time,
   `kubeadm reset` removes this node's etcd member for you. When the node is
   being reset *because* it is broken/partitioned (cluster unreachable), the
   member cannot be removed and is left orphaned -- a stale member erodes quorum.
   The provider does NOT run `etcdctl` from a dying node; it emits a loud warning
   and proceeds with local cleanup. **You must then deregister the member from a
   surviving control plane:**
   ```sh
   kubectl delete node <name>
   # on a surviving CP:
   etcdctl member list                 # find this node's member id
   etcdctl member remove <member-id>
   ```

Always keep an odd number of healthy control planes (3 or 5). Removing one of
three uncleanly leaves a 3-member list with 2 reachable (quorum still 2);
removing a second uncleanly drops you below quorum and the cluster wedges.
