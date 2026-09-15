# Troubleshooting

## Where to look first

- **Status file (start here):** `/run/provider-kubernetes/status.yaml` (current
  boot) or `/var/log/provider-kubernetes/status.yaml` (persists across reboot). A
  one-glance `phase` / `reason` / `message` summary of why the node did or did not
  converge, plus whether the failure is `terminal`. On a cluster member the same
  outcome is also on the Node as `provider-kubernetes.kairos.io/*` annotations. See
  [Node status](./status.md).
- **Reconcile log:** `/var/log/provider-kubernetes-reconcile.log` on the node. It
  records the role, observed membership, the planned actions, and any bounded
  failure (with kubeadm output, secret-sanitized).
- **Serialized input:** `/run/provider-kubernetes/cluster.json` (tmpfs, `0600`) -
  the `Cluster` the reconcile pass consumed this boot.
- **kubeadm artifacts:** `/etc/kubernetes/` (confs, manifests, pki; upgrade
  backups under `/etc/kubernetes/tmp/`), `/var/lib/etcd`, `/var/lib/kubelet`.
- **Pre-upgrade etcd snapshot (encrypted control planes only):**
  `/usr/local/provider-kubernetes/etcd-backup/` - see
  [Upgrades](./upgrades.md#etcd-backups).
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

### `fork/exec /usr/bin/<tool>: no such file or directory`

The provider runs `kubeadm`, `kubectl`, `ctr`, `systemctl` and `etcdctl` only from
`/usr/bin` in the image and never searches `PATH`. A custom or derived image must
install them there. Copies you place in `/usr/local/bin` are ignored by the
provider (containerd and the kubelet may still pick up tools there, so avoid putting
Kubernetes binaries in that directory). See
[Security model](./security.md#exec-hygiene).

### kubeadm pulls control-plane images even though the image bundles them

The image bundles the control-plane images for its Kubernetes version and imports
them into containerd at boot, so `kubeadm init`/`join` should not need a registry.
Check that containerd has an image under the exact reference kubeadm looks for
(this is the same lookup kubeadm does):

```sh
sudo /usr/bin/crictl --runtime-endpoint unix:///run/containerd/containerd.sock \
  --image-endpoint unix:///run/containerd/containerd.sock inspecti registry.k8s.io/pause:<tag>
sudo /usr/bin/kubeadm config images list --kubernetes-version <version>
```

`crictl inspecti` also succeeds if the image was pulled from the registry. To see
that containerd holds the bundled image, compare its image ID with the `Config`
digest in the matching tarball's manifest; they must be equal:

```sh
sudo /usr/bin/crictl --runtime-endpoint unix:///run/containerd/containerd.sock \
  --image-endpoint unix:///run/containerd/containerd.sock inspecti -o json registry.k8s.io/pause:<tag> | grep -m1 '"id"'
sudo /usr/bin/tar -xOf /opt/provider-kubernetes/images/registry.k8s.io_pause_<tag>.tar manifest.json
```

- The bundle covers the default `imageRepository` (`registry.k8s.io`) only. With
  a custom `imageRepository`, kubeadm looks for other references and pulls.
- **v0.3.0 images** imported every bundled image under a placeholder name
  (`registry.k8s.io/<image>:i-was-a-digest`), so kubeadm could not find them: nodes
  without registry access failed to bootstrap, and connected nodes pulled the
  images by tag. Use a fixed image. Leftover `:i-was-a-digest` names are harmless
  and can be removed with `sudo /usr/bin/ctr -n k8s.io images rm <name>`; an
  air-gapped node that failed to bootstrap should be reinstalled from a fixed image.

### Upgrade didn't run, or was refused

- **Nothing happened after booting a newer image.** An upgrade runs only when you
  bump `clusterConfiguration.kubernetesVersion` to the new minor; a newer binary
  alone is a no-op by design.
- **Reconcile logs `refuse-upgrade`.** The pin is a downgrade, skip-level
  (e.g. 1.35 -> 1.37), or out-of-window. Upgrade one minor at a time within the
  window.

### The pre-upgrade etcd snapshot was not taken

Before `kubeadm upgrade apply` the reconcile log has exactly one
`etcd-snapshot outcome=<outcome>` line. Anything other than `taken` or
`skipped-already-taken` means the provider wrote no snapshot; the upgrade still
continues. Common causes:

- `skipped-encryption-unconfirmed` - expected on a default install. The provider
  only writes a snapshot onto ext4/xfs directly on dm-crypt (an encrypted
  `COS_PERSISTENT`) and never writes a plaintext full-cluster dump.
- `skipped-etcdctl-missing` - the image predates the bundled `/usr/bin/etcdctl`.
- `skipped-insufficient-space` - the persistent partition has less free space than
  twice the etcd database plus 1 GiB.
- `failed` - the line carries the sanitized reason (for example etcd unhealthy or
  the 2 minute bound exceeded).

In every case take a manual, off-node backup with the bundled `etcdctl`/`etcdutl`
before upgrading; see [Upgrades](./upgrades.md#etcd-backups) for the commands.

### Disk usage grows under `/etc/kubernetes/tmp`

kubeadm copies the etcd data directory to
`/etc/kubernetes/tmp/kubeadm-backup-etcd-<timestamp>` on every stacked control
plane on every upgrade and keeps it (it is kubeadm's rollback artifact). The copies
hold every Secret, are plaintext unless the persistent partition is encrypted, and
share a device with etcd. Once an upgrade is verified, delete the older copies; see
[Upgrades](./upgrades.md#etcd-backups).

### A reset left a stale etcd member (HA)

If a control plane was reset while the cluster was unreachable, its etcd member is
orphaned. Deregister it from a surviving control plane with `kubectl delete node`
and the bundled `/usr/bin/etcdctl` (`member list`, then `member remove <id>`,
passing the etcd client certificate flags shown in
[Upgrades](./upgrades.md#etcd-backups)). See
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
and the image tag / Kubernetes version. Mention the release (or commit) you are
on so the report is reproducible.
