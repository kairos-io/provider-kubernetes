# Troubleshooting

## Where to look first

- **Reconcile log:** `/var/log/provider-kubernetes-reconcile.log` on the node. It
  records the role, observed membership, the planned actions, and any bounded
  failure (with kubeadm output, secret-sanitized).
- **Serialized input:** `/run/provider-kubernetes/cluster.json` (tmpfs, `0600`) -
  the `Cluster` the reconcile pass consumed this boot.
- **kubeadm artifacts:** `/etc/kubernetes/` (confs, manifests, pki),
  `/var/lib/etcd`, `/var/lib/kubelet`.
- **Kubelet / containerd:** `journalctl -u kubelet`, `journalctl -u containerd`,
  `crictl ps -a`.

The provider **fails loud and never hangs**: if bootstrap cannot proceed, you get
an error in the log, not a stuck boot. If a boot seems stuck, suspect the
environment (network, disk, the kubelet/containerd) rather than the provider
looping.

## Common situations

### Node is `NotReady`

Expected until you install a CNI. See [CNI](./cni.md).

### `role: init` refused: "a control plane already answers at ..."

Working as intended. The node is configured `role: init` but a control plane
already exists at the endpoint, so the provider refuses to clobber it. Use `role:
controlplane` (with minted control-plane material) to join instead, or `role:
worker`. See [Security model](./security.md#never-clobber-an-existing-cluster).

### Join fails: missing CA anchor / refuses to build join config

Token discovery requires `caCertHashes`. Re-mint with `mint-join` (which computes
the pin) or supply a CA anchor explicitly. The provider never joins without
pinning the CA. See [Configuration](./configuration.md).

### Control-plane join fails to decrypt certs / cert key mismatch

The certificate key must match the certs uploaded to the `kubeadm-certs` Secret,
and that upload expires after ~2h. Always mint control-plane material with
`mint-join --role controlplane` (it re-uploads under a fresh key) **just before**
booting the node, and never reuse a key across joins. See
[mint-join](./mint-join.md).

### Pin/binary mismatch: version is a hard error

`clusterConfiguration.kubernetesVersion` must be within the supported window and
match the `kubeadm` binary in the image. A mismatch fails fast by design. Use the
image tag for the minor you want, or adjust the pin. See
[Lifecycle](./lifecycle.md#supported-version-window).

### A reset left a stale etcd member (HA)

If a control plane was reset while the cluster was unreachable, its etcd member is
orphaned. Deregister it from a surviving control plane (`kubectl delete node`,
`etcdctl member remove`). See
[High availability](./high-availability.md#removing-a-control-plane).

### HA: endpoint advisory at `role: init`

A warning that the endpoint looks like the node's own IP means you have not set a
stable VIP/LB/DNS endpoint. Fine for a single control plane; required before you
add more. See [High availability](./high-availability.md).

## Releasing / CI (maintainers)

- The release workflow needs repo **Settings -> Actions -> Workflow permissions =
  Read and write** for `GITHUB_TOKEN` to push images and create releases.
- The published ghcr package inherits repo visibility; make it **public** for
  external testers.

## Filing an issue

Include: the role and `config:` of the affected node (redact `cluster_token` and
any token/cert key), the tail of the reconcile log, `kubectl get nodes -o wide`,
and the image tag / Kubernetes version. The project is moving quickly - mention
the commit or release you are on.
