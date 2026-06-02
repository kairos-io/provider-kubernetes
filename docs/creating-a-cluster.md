# Creating a cluster

This covers a single control plane plus worker nodes. For multiple control
planes, see [High availability](./high-availability.md).

## The model

Node roles are declared in each node's cloud-config; there is no central
controller. The provider on each node converges that node toward its declared
role. Join material (bootstrap token + CA pin, and for control planes a
certificate key) is **minted on an existing control plane and delivered by you**
to the joining node's cloud-config - the joining node never mints its own
credentials.

## 1. The first control plane

Boot one node with `role: init` (see [Getting started](./getting-started.md)).
When it is up you have a working single-node control plane and an admin
kubeconfig at `/etc/kubernetes/admin.conf`.

## 2. Mint join material

On the running control plane, mint material for the node you want to add:

```sh
# a worker:
sudo agent-provider-kubernetes mint-join --role worker --ttl 1h \
  --cluster-token "<the cluster's cluster_token>"

# an additional control plane:
sudo agent-provider-kubernetes mint-join --role controlplane --ttl 1h \
  --advertise-address <the-new-node-ip> \
  --cluster-token "<the cluster's cluster_token>"
```

`mint-join` prints a ready-to-edit cloud-config containing a bounded-TTL bootstrap
token, the cluster CA SPKI pin, and (for control planes) a fresh certificate key
with the cluster certs re-uploaded under it. See [mint-join](./mint-join.md).

Deliver the printed config to the joining node **soon** - the token is
short-lived by design. The provider persists none of this material.

## 3. Boot the joiner

Take the `cluster:` block from the minted output, drop it into the joining node's
cloud-config (adjusting install device, hostname, networking as needed), and
boot. The provider's `network.after` stage runs `kubeadm join` against the
delivered material with CA pinning.

Worker example (the `config:` comes from `mint-join --role worker`):

```yaml
cluster:
  cluster_token: "<same cluster_token>"
  control_plane_host: "192.168.1.10:6443"
  role: worker
  providerConfig:
    cluster_root_path: "/"
  config: |
    joinConfiguration:
      discovery:
        bootstrapToken:
          token: "abcdef.0123456789abcdef"
          apiServerEndpoint: "192.168.1.10:6443"
          caCertHashes:
            - "sha256:<pin>"
```

See [`samples/worker.yaml`](../samples/worker.yaml) and
[`samples/controlplane.yaml`](../samples/controlplane.yaml).

## 4. Verify

```sh
sudo kubectl --kubeconfig /etc/kubernetes/admin.conf get nodes -o wide
```

New nodes appear and go `Ready` once a CNI is installed ([CNI](./cni.md)).

## Idempotency and reboots

Reconcile is desired-vs-actual: on every boot the provider probes the node's real
state and only acts on the difference. A node that is already a healthy member is
a no-op on reboot. The provider does not re-bootstrap or re-join an
already-converged node; recovery is the explicit [reset](./lifecycle.md) flow.
