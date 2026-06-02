# mint-join reference

`mint-join` produces join material on an existing control-plane node and prints a
ready-to-paste cloud-config for a joining node. It embodies the operator-delivery
model: the control plane mints, you deliver, the joining node consumes. Joining
nodes never mint their own credentials.

## Usage

```sh
agent-provider-kubernetes mint-join [flags]
```

Run it **on a healthy control-plane node**, as root (it needs the local admin
credentials and the cluster CA under `--root-path`).

## Flags

| Flag | Default | Meaning |
|------|---------|---------|
| `--role` | `worker` | `worker` or `controlplane`. |
| `--ttl` | `1h` | Bootstrap token TTL (must be > 0; never unbounded). |
| `--endpoint` | derived from `admin.conf` | API server endpoint (`host:port`) the joiner targets. For HA, the stable VIP/LB/DNS endpoint. |
| `--advertise-address` | empty | `controlplane` only: the joining node's own routable IP. Left blank, the rendered config carries a fill-in placeholder and kubeadm falls back to the default-route interface (fine for single-homed nodes). |
| `--root-path` | `/` | `cluster_root_path`; locates `admin.conf` and `ca.crt`. |
| `--cluster-token` | empty | The cluster's `cluster_token` to embed in the rendered config (must match the control plane). Left blank, a clearly-marked placeholder is emitted. |

## What it does

1. Creates a fresh, bounded-TTL bootstrap token (`kubeadm token create`).
2. Computes the cluster CA SPKI pin (`sha256:...`) from `ca.crt`.
3. For `--role controlplane`: generates a fresh certificate key and **re-uploads
   the cluster certs encrypted under it** (so the key matches the `kubeadm-certs`
   Secret), preserving the upstream 2h expiry.
4. Renders and prints a cloud-config to stdout.

Secret values are written only to stdout (by design) and never to a logger. The
certificate key never appears on a command line.

## Examples

Worker:

```sh
sudo agent-provider-kubernetes mint-join \
  --role worker --ttl 1h \
  --cluster-token "the-clusters-cluster-token"
```

Additional control plane (HA), telling it the new node's IP:

```sh
sudo agent-provider-kubernetes mint-join \
  --role controlplane --ttl 1h \
  --endpoint k8s-api.example.test:6443 \
  --advertise-address 172.16.56.241 \
  --cluster-token "the-clusters-cluster-token"
```

Take the printed `cluster:` block into the joining node's cloud-config. Deliver
it promptly over a confidential channel - the token is short-lived, and a
control-plane bundle is root-equivalent ([Security model](./security.md)).

## Lifetime

Tokens and the certificate-upload both expire deliberately (token per `--ttl`,
default 1h; the `kubeadm-certs` upload after the upstream 2h). If material
expires before the node boots, just mint again. Mint **fresh** material per node;
do not reuse a control-plane key across joins.
