# Upgrades

The provider upgrades a running cluster between supported Kubernetes minors by
driving `kubeadm upgrade`. Upgrades are **image-driven and explicitly pinned**:
you boot a newer provider-kubernetes image and bump the version pin; on the next
boot the provider detects the delta and converges the cluster.

> Single-control-plane and multi-node/HA upgrade ordering are implemented (see
> below). HA upgrades are operator-sequenced one node at a time; test them in your
> environment before relying on them. This is an early public release, not yet
> certified for production.

## The model

- The **target version is the bundled `kubeadm` binary** in the image you boot.
  By ADR-3 an operator `kubernetesVersion` pin must equal the bundled minor, so the
  pin and the binary always agree.
- An upgrade runs **only when you pin** `clusterConfiguration.kubernetesVersion` to
  the new minor. A newer image **without** a matching pin bump does **not**
  auto-upgrade - it is a no-op (so a routine image refresh never silently
  re-versions your cluster).
- Only **+1 minor** at a time (kubeadm's rule). Skipping a minor (1.35 -> 1.37),
  downgrades, and out-of-window targets are **refused loudly** before anything
  destructive runs.

## Supported edges

Within the current window: **1.35 -> 1.36** and **1.36 -> 1.37**. To go 1.35 ->
1.37, step through 1.36 (boot the 1.36 image first, let it converge, then the 1.37
image).

A cluster still on **1.34** (dropped from the window) upgrades out the same way:
boot the 1.35 image with the pin bumped to 1.35. Only the upgrade *target* has to
be inside the window.

**1.36 -> 1.37 moves etcd to 3.7.** kubeadm 1.37 upgrades stacked etcd from 3.6.8
(what kubeadm 1.36.x deploys) directly to 3.7.0, while etcd's
[3.7 upgrade guide](https://etcd.io/docs/v3.7/upgrades/upgrade_3_7/) asks for
3.6.11 or later before a rolling upgrade. This edge is not yet validated on
multi-control-plane (stacked etcd) clusters. Take your own off-node etcd backup
before upgrading (do not rely on the provider's snapshot, see
[etcd backups](#etcd-backups)) and upgrade control planes strictly one at a time.

## Upgrading a single control plane

1. Make sure the cluster is healthy (`kubectl get nodes`, control-plane pods
   Running). Take an **etcd backup** first - see [etcd backups](#etcd-backups).
2. Bump the pin in the node's cloud-config (`/oem` on an installed node) to the new
   minor, e.g. `kubernetesVersion: v1.37.0`.
3. Upgrade the OS image to the matching minor and reboot. Either:
   - `kairos-agent upgrade --source oci:<registry>/provider-kubernetes:<tag>-k8s1.37`
     (then reboot), or
   - boot the new image via your provisioning flow.
4. On reboot the provider's reconcile detects that the bundled binary is a minor
   ahead of the cluster and runs `kubeadm upgrade apply <version>`. When the new
   kubelet cannot start against the old node config (a flag removed across the
   minor), the provider first repairs the kubelet config (see below), brings the
   control plane back, then applies. The kubelet is restarted onto the new version.
5. Verify: `kubectl version` (server), `kubectl get nodes` (node at the new
   version, Ready).

## Upgrading multiple control planes / workers (HA)

Upgrade **one node at a time**, control planes first, then workers:

1. Upgrade the first control plane as above; wait until the cluster version has
   flipped and it is Ready.
2. Upgrade each remaining control plane one at a time. A follower runs
   `kubeadm upgrade node` (the provider detects the cluster already advanced and
   does not re-apply); it waits, bounded, for the control plane to be healthy first.
3. Upgrade workers last, one at a time; each runs `kubeadm upgrade node`.

This sequencing is the operator's responsibility (the same one-at-a-time delivery
contract as HA join); the provider adds bounded health gates but builds no
cross-node lock.

## The kubelet-config repair (why an upgrade "just works" after an image swap)

Kubernetes removes/renames kubelet flags and config keys across minors. Because a
Kairos A/B image swap replaces the kubelet binary *before* any upgrade runs, the
new kubelet can crashloop on a flag the old kubeadm wrote (for example
`--pod-infra-container-image`, removed in 1.35), which would leave the control
plane down. The provider handles this automatically: when an upgrade is due and the
local API is unreachable, it runs `kubeadm init phase kubelet-start` to regenerate
the kubelet config with the new kubeadm (no API, no secrets), which lets the
kubelet start and the existing control plane return before `kubeadm upgrade apply`
runs. You do not need to do anything for this.

## etcd backups

`kubeadm upgrade apply` mutates etcd and is largely forward-only. An etcd snapshot
is a full dump of every cluster Secret, so it is as sensitive as the cluster CA.
Three different backups can exist around an upgrade; know which ones you have.

### 1. The provider's pre-upgrade snapshot (encrypted nodes only)

On the control plane that runs `kubeadm upgrade apply`, and only with stacked etcd,
the provider tries to take **one** snapshot with the bundled `/usr/bin/etcdctl`
before applying:

- **Only onto encrypted storage.** It is written to
  `/usr/local/provider-kubernetes/etcd-backup/` (on the persistent partition) and
  only if that filesystem is ext4 or xfs sitting directly on a dm-crypt device,
  for example when `COS_PERSISTENT` is encrypted with
  [`install.encrypted_partitions`](https://kairos.io/docs/advanced/partition_encryption/).
  On anything else - including a default, unencrypted Kairos install - the
  provider **refuses** and writes nothing: it never creates a plaintext
  full-cluster dump.
- **Bounded and best-effort.** At most 2 minutes, skipped if free space is below
  twice the etcd database size plus 1 GiB, and it never blocks the upgrade.
- **Once per cluster and target minor.** A retried apply or a reboot mid-upgrade
  keeps the first snapshot instead of replacing the clean pre-upgrade state with a
  partially upgraded one. The file is named
  `etcd-snapshot-<cluster-id>-to-<minor>-from-<etcd-tag>-<UTC time>.db`, where
  `<cluster-id>` is the first 16 hex characters of the cluster CA public-key hash
  (the same value as `caCertHashes`; not a secret).
- **Retention and custody.** Files are `0600 root:root` in a `0700` directory. The
  snapshot is kept until the next upgrade's snapshot replaces it; a reset does
  **not** remove it. The provider never copies it off the node.

It does not replace your own backup: it lives on the same disk as etcd (no
protection if that disk is lost) and only on the apply node.

Each attempt logs exactly one line in the reconcile log,
`etcd-snapshot outcome=<outcome>`:

| Outcome | Meaning / what to do |
|---------|----------------------|
| `taken` | Snapshot written; the line includes its path. |
| `skipped-already-taken` | A snapshot for this cluster and target already exists (retry or reboot mid-upgrade). |
| `skipped-encryption-unconfirmed` | The snapshot directory is not on dm-crypt (the default install). Take a manual backup. |
| `skipped-external-etcd` | No stacked etcd on this node. Back up your external etcd with its own tooling. |
| `skipped-etcdctl-missing` | The image does not ship `/usr/bin/etcdctl` (built before etcd tools were bundled). Take a manual backup. |
| `skipped-insufficient-space` | Not enough free space on the persistent partition. Free space or take a manual backup. |
| `failed` | The line carries the (sanitized) reason. The upgrade continues; take a manual backup. |

### 2. kubeadm's own etcd data-directory copy (every stacked control plane)

Independently of the provider, `kubeadm upgrade apply` and `kubeadm upgrade node`
copy the etcd data directory to
`/etc/kubernetes/tmp/kubeadm-backup-etcd-<YYYY-MM-DD-HH-MM-SS>` (node local time)
on **every stacked control plane, on every upgrade**, even when the etcd version
does not change. kubeadm uses the copy to roll back a failed etcd upgrade and
deliberately leaves it in place after success. Be aware that:

- It contains every Secret and is **plaintext unless the persistent partition is
  encrypted** - the provider cannot prevent it, and its own refusal above does not
  apply to it. It sits on the same device as the live `/var/lib/etcd`.
- One copy accumulates per upgrade per control plane. Old copies keep Secrets you
  have since deleted or rotated, and they consume space on the device etcd writes
  to (a full disk raises etcd's `NOSPACE` alarm and makes the cluster read-only).
- A provider reset removes them. Otherwise, once an upgrade is verified, delete
  the older copies yourself:

  ```sh
  sudo ls -1d /etc/kubernetes/tmp/kubeadm-backup-etcd-*
  sudo rm -rf /etc/kubernetes/tmp/kubeadm-backup-etcd-<older-timestamp>
  ```

### 3. Your own backup (required on unencrypted nodes; recommended before 1.36 -> 1.37)

Images bundle `etcdctl` and `etcdutl` at `/usr/bin`. They are extracted at build
time from the same signature-verified etcd image the image bundles, so they match
the etcd version that image's kubeadm deploys. (`etcdctl snapshot status` and
`restore` no longer exist since etcd 3.6; use `etcdutl`.) On one healthy control
plane, before booting the new image:

```sh
# Unencrypted node: write to tmpfs (RAM), copy it off the node, then delete it.
# Encrypted node: a directory under /usr/local is fine.
sudo install -d -m 0700 /run/etcd-backup
sudo /usr/bin/etcdctl --endpoints=https://127.0.0.1:2379 \
  --cacert=/etc/kubernetes/pki/etcd/ca.crt \
  --cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt \
  --key=/etc/kubernetes/pki/etcd/healthcheck-client.key \
  --dial-timeout=10s --command-timeout=5m \
  snapshot save /run/etcd-backup/etcd-pre-upgrade.db
sudo /usr/bin/etcdutl snapshot status /run/etcd-backup/etcd-pre-upgrade.db -w table
```

Copy the file off the node over an authenticated, encrypted channel and remove the
local copy. If the running image predates the bundled tools, run the same
`etcdctl ... snapshot save` inside the etcd static pod instead
(`kubectl -n kube-system exec etcd-<node> -- etcdctl ...`), saving under
`/var/lib/etcd/` (the pod's writable host mount), then move the file out of
`/var/lib/etcd` on the host.

### Restoring from a snapshot (outline, not yet validated on a VM)

Follow the upstream [etcd recovery guide](https://etcd.io/docs/v3.7/op-guide/recovery/);
on a kubeadm/Kairos control plane that means:

1. Use the `etcdutl` that matches the etcd version that will run the restored data
   (for a rollback, the one in the previous image).
2. On **every** control plane, stop the static control plane by moving the
   manifests out of `/etc/kubernetes/manifests`, and wait until `crictl ps` shows
   no etcd or kube-apiserver container.
3. On every control plane, move `/var/lib/etcd/member` aside (for example to
   `/var/lib/etcd/member.old`). Do not move `/var/lib/etcd` itself: on Kairos it is
   a bind-mount point.
4. On each member, restore the **same** snapshot with
   `etcdutl snapshot restore <file> --data-dir <new-dir>`, passing that member's
   `--name` and `--initial-advertise-peer-urls` (read them from
   `/etc/kubernetes/manifests/etcd.yaml`), the full `--initial-cluster`, and a new
   `--initial-cluster-token`. Then move `<new-dir>/member` into `/var/lib/etcd/`.
5. Put the manifests back and confirm with `kubectl get nodes`.

## Unit files moved into the image

Since this release the units the provider owns - `containerd.service`,
`kubelet.service`, `provider-kubernetes-image-import.service` and the kubeadm
drop-in `kubelet.service.d/10-kubeadm.conf` - are installed in
`/usr/lib/systemd/system`, which is part of the image you boot. Earlier releases
installed them into `/etc/systemd/system`, which Kairos keeps on the persistent
partition and refreshes from the image with `rsync --update` (update-only, no
delete). A copy there was replaced only when the new build's mtime happened to be
newer, survived a rollback, and could never be removed by an upgrade.

**The one-time cleanup.** The first boot of this release runs
`provider-kubernetes-unit-migrate.service` before containerd, the import unit and
the kubelet, removes the copies it recognizes, and reloads systemd so the same boot
already runs the image's units. It logs one summary line:

```sh
sudo journalctl -b -u provider-kubernetes-unit-migrate.service
# unit-migrate: summary outcome=migrated removed=7 kept=0 overrides=0 reloaded=true
```

`outcome=clean` on a fresh install, a recovery boot or any later boot means there
was nothing to do. Check where a unit now comes from with:

```sh
sudo systemctl show -p FragmentPath,DropInPaths kubelet.service
sudo systemctl cat kubelet.service
```

**Only byte-identical copies are deleted.** The cleanup knows the exact bytes this
project shipped at each of those four paths and removes a file only if it matches
one of them - nothing else, and never anything that is not on that fixed list. **A
copy you edited is kept**, named in the journal
(`unit-migrate: kept "/etc/systemd/system/kubelet.service" reason=modified`), and
the unit **fails**, so `systemctl --failed` shows it. That is deliberate: the edited
copy keeps overriding the image's unit on every boot, and only you can decide what
to do with it. Convert the change into a drop-in and delete the full-unit copy:

```sh
sudo systemctl cat kubelet.service            # see what is in effect and from where
sudo systemd-delta --type=extended /usr/lib/systemd/system
sudo mkdir -p /etc/systemd/system/kubelet.service.d
sudo vi /etc/systemd/system/kubelet.service.d/20-local.conf   # only the directives you change
sudo rm /etc/systemd/system/kubelet.service
sudo systemctl daemon-reload
sudo systemctl restart provider-kubernetes-unit-migrate.service   # optional: clears the failure now
```

A drop-in in `/etc/systemd/system/<unit>.d/` is never touched by the cleanup and
keeps working across upgrades - and it is the upstream-supported way to override a
packaged unit.

**Disabling a unit.** `systemctl disable` is a no-op for these units (they have no
`[Install]` section, and `systemctl is-enabled` reports `static`). Use
`systemctl mask <unit>` instead; unlike before, that now sticks, because no regular
file sits at `/etc/systemd/system/<unit>` for the mask symlink to collide with. A
mask is also left alone by the cleanup.

**Rollback.** Booting an older image re-creates that image's own `/etc` copies and
`.wants` links - immucore copies files that are missing - so the old OS runs exactly
its own units, which is better than before. Coming back to a release with
image-owned units removes them again and reloads. Each round trip costs one cleanup
and one reload; an edited copy keeps being kept, and the unit keeps failing, until
you convert it.

## Rollback

**Bundled images after upgrading from an older release.** Images that bundle the
control-plane images under `/system/provider-kubernetes/images` no longer read the
old copy under `/opt/provider-kubernetes`, which Kairos keeps on the persistent
partition. Once you no longer need to roll back, you can remove it - but **check
first** which import unit the node actually runs:

```sh
sudo systemctl cat provider-kubernetes-image-import.service | grep ConditionPathExists
```

That must print **nothing**. Kairos keeps `/etc/systemd` on the persistent partition
and refreshes it from the image with `rsync --update`, so a node can still be running
the v0.3.0 unit, which has
`ConditionPathExists=/opt/provider-kubernetes/images`. On such a node, removing the
directory makes the import **silently skip on every boot** (the unit reports
`condition failed`, not an error), so the images bundled in the image you boot are
never imported: connected nodes fall back to pulling by tag, and an air-gapped node
fails to converge. If the line is present, leave the directory in place until the
node runs a unit without it.

With no `ConditionPathExists` line, remove it:

```sh
sudo rm -rf /opt/provider-kubernetes
```

Booting an older image puts it back and that image imports from it again. Do not
list `/system/provider-kubernetes` in `kairos-agent upgrade` excluded paths: the
bundle must come from the image you boot.

kubeadm upgrades (especially etcd) are forward-only; there is no automatic
rollback. To recover, restore an etcd snapshot (see above) and boot the previous
image. A node wedged mid-upgrade can be recovered with the [reset](./lifecycle.md)
flow and re-joined. The provider never auto-resets a control plane on upgrade
failure - it fails loud and leaves the node for you to inspect.

## What can go wrong

| Symptom | Cause / fix |
|---------|-------------|
| Reconcile logs `refuse-upgrade` | Skip-level, downgrade, or out-of-window pin. Pin only +1 minor within the window. |
| Upgrade doesn't start | No version pin bump (a newer binary alone won't upgrade). Bump `clusterConfiguration.kubernetesVersion`. |
| Pin/binary mismatch hard error | The pin minor must equal the bundled image's minor. |
| `etcd-snapshot outcome=` anything other than `taken` | See the outcome table under [etcd backups](#etcd-backups); take a manual backup. |
| Disk usage grows under `/etc/kubernetes/tmp` | kubeadm's per-upgrade etcd data-dir copies; remove older ones (see [etcd backups](#etcd-backups)). |

See also [Lifecycle and reset](./lifecycle.md) and [Troubleshooting](./troubleshooting.md).
