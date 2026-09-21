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

### First boot after install (or a state reset) never bootstraps (D-3 / F-UKIBOOT)

**Symptom:** on a UKI (trusted-boot) node, the very first boot after install -
or the boot right after a state reset - never bootstraps: the kubelet
crash-loops, and (before this fix) nothing on the node explained why. A plain
`reboot` used to make it converge; a UKI state reset reproduced the same
non-convergence instead of fixing it.

**Cause:** kairos-sdk's `clusterplugin` writes
`/usr/local/cloud-config/cluster.kairos.yaml` by opening it with
`O_CREATE|O_WRONLY|O_TRUNC` and never creates the parent directory. On a
non-UKI (GRUB) install that directory is created as a side effect of the
install's chroot hook before the first boot; the UKI install and reset paths
have no equivalent chrooted hook, so on those layouts the directory simply
does not exist yet the first time it is needed.

**Fix:** the provider now creates that one directory itself -
`/usr/local/cloud-config`, and only that directory, never its ancestors -
before the SDK writes to it, on every `agent.boot` event. A fresh UKI install
or a UKI state reset should converge normally with no manual step.

**If it still does not converge**, check the status document (`reason:`
field). **Do not go looking for a boot-log line.** On UKI there is no log
channel reachable after boot: this code logs a
`provider-kubernetes: cluster-config-dir: ...` `level=error` line, same as
everything else in this provider, but that line lives only in a pre-pivot,
tmpfs-backed log (immucore's own log), and a VM run (2026-09-21) confirmed it
appears in neither `immucore.log`, `agent.log`, nor the journal afterward --
none of those channels survive the switch-root on a UKI node. The status
document is the only channel that does:

- `/run/provider-kubernetes/status.yaml` (current boot, tmpfs) or
  `/var/log/provider-kubernetes/status.yaml` (persistent mirror). See
  [Node status](./status.md). **On UKI the persistent `/var/log` mirror is
  load-bearing, not a convenience**: it is confirmed to survive the pivot,
  and it is the only copy you can rely on if you did not (or cannot) read
  `/run` before something else rotates or reboots the node.

| `reason:` | Meaning | What to do |
|-----------|---------|------------|
| `ClusterConfigAncestorUnsafe` | `/usr` or `/usr/local` itself is a symlink, not a plain directory, or not owned by root. | The provider withholds `cluster_token` and emits no bootstrap commands this boot: it could not verify where that component actually leads, and the SDK's write would resolve straight through it. Investigate the mount/persistent partition before retrying. |
| `ClusterConfigAncestorMissing` | `/usr` or `/usr/local` itself is simply absent. | Reported only, not withheld: the SDK's own write then fails the identical "not found" error either way, so nothing is gained by withholding. Something is likely wrong with the persistent partition or its mount; reinstall or investigate. |
| `ClusterConfigDirUnsafe` | Something already occupies `/usr/local/cloud-config` and is not a plain, root-owned directory (a symlink, a FIFO, a device, or a regular file). | The provider withholds `cluster_token` and emits no bootstrap commands this boot. Remove or fix the offending entry, then reboot. |
| `ClusterConfigTokenFileUnsafe` | `cluster.kairos.yaml` (or your `cluster_config_path` override's target) already exists and is anything other than absent or an intact, root-owned 0600 regular file - a symlink, a FIFO, a device, a socket, a directory, not owned by root, group/other-writable, or hard-linked. | The provider withholds `cluster_token` and emits no bootstrap commands this boot. Remove the offending entry, then reboot. |
| `ClusterConfigDirWritable` | `/usr/local/cloud-config` exists, is owned by root, but is other-writable. (Group-writable alone is the platform's own expected state from the second boot onward - the platform itself widens the mode to 0770 root:admin - and is deliberately NOT reported.) | Reported only; the boot still converges normally, and any existing reconcile status (for example `Converged`) is left untouched. No action needed. |
| `ClusterConfigOverrideRejected` | `cluster_config_path` is set to somewhere other than directly under `/usr/local/cloud-config`. | The provider creates nothing there. You own pre-creating that directory safely, or drop the override to use the default path. |
| `ClusterConfigNotPersistent` | `/usr/local` is not actually a separate mount on this boot (same device as `/`). | The token is about to be written to ephemeral storage; it will not survive a reboot, so the node will not stay converged. Check that `COS_PERSISTENT` mounted. |
| `ClusterConfigDirCreateFailed` | The directory could not be created for a reason other than already existing (for example a read-only filesystem). | Check `dmesg`/`journalctl` for the underlying mount error. |

These eight reasons split three ways:

- **Withhold** (`ClusterConfigAncestorUnsafe`, `ClusterConfigDirUnsafe`,
  `ClusterConfigTokenFileUnsafe`): `cluster_token` and every bootstrap command
  are withheld for this boot rather than let the SDK write the real secret
  where it could not verify the write would land safely. The status document
  reports `phase: Failed` - accurately, since nothing bootstraps this boot.
- **Failed but not withheld** (`ClusterConfigAncestorMissing`,
  `ClusterConfigDirCreateFailed`): the secret is not at risk (the SDK's own
  write fails the same way regardless of what we do), but the node genuinely
  will not converge this boot, so the status document still reports
  `phase: Failed`.
- **Report-only** (`ClusterConfigDirWritable`, `ClusterConfigOverrideRejected`,
  `ClusterConfigNotPersistent`): logged and recorded, but the boot converges
  normally and **any existing reconcile status is left exactly as it was** -
  these reasons never set `phase: Failed` and never overwrite a `Converged`
  phase with one. (A 2026-09-21 VM run found the pre-fix code doing exactly
  that on a converged GRUB node - reporting `Failed` while the boot converged
  fine - which is corrected here: a diagnostic that downgrades a healthy node
  trains operators to ignore `Failed`, which is its own defect.)

This fix closes the specific missing-parent-directory defect; it is
**not a security boundary** - see
[Security model](./security.md#cluster-config-directory-creation-is-not-a-security-boundary-d-3--f-ukiboot)
for exactly what the directory's mode does and does not protect. Two residual
risks a 2026-09-21 VM run confirmed and this fix does **not** close:

- **A planted non-regular, open-blocking file can still hang the boot before
  our binary ever runs.** kairos-agent's own config scan (`notify agent.boot`)
  reads every file under `/usr/local/cloud-config` (and `/oem`) to build the
  cloud-config it hands to every provider, including ours. A FIFO or similar
  open-blocking file planted there hangs that scan unboundedly, before our
  code gets a chance to run its own preflight, and holds a shutdown inhibitor
  while it does - the result is an unreachable node that will not even
  reboot cleanly. This is present with or without this fix, is not specific
  to us, affects every Kairos cluster provider, and our S-D3-4 preflight only
  ever covers a file planted **after** kairos-agent's scan has already run,
  not one already in place before it. **Do not treat this as something our
  code can prevent.** Recovery is manual: boot recovery media and remove the
  offending file from the persistent partition.
- **A planted `/usr/local/cloud-config` symlink is refused by us, but the
  platform's own chmod still follows it and can corrupt the real target.**
  kairos-init's `10_accounting.yaml` runs an unconditional `chown -R
  root:admin` / `chmod 770` against `/usr/local/cloud-config` in the same
  `initramfs` stage, and that chmod is **not** the O_NOFOLLOW-safe walk this
  fix uses - a symlink there (for example, planted pointing at `/etc`) is
  followed, and its mode/ownership are changed recursively. A VM run
  confirmed this breaks `sshd` and non-root `bash` when the symlink points at
  `/etc`. This provider's withhold **only reduces `cluster_token`
  disclosure**; it does not prevent, and cannot prevent, this denial of
  service, because the corruption happens in a different component's step
  that runs regardless of what we refuse.

### Node is `NotReady`

Expected until you install a CNI. See [CNI](./cni.md).

### Status reports `phase: Degraded`

The node is already a cluster member (`membership: initialized` or `joined`),
but its kubelet failed a liveness check: a plain HTTP GET of
`http://127.0.0.1:10248/healthz` on this node, expecting exactly HTTP 200
within 2 seconds - the same endpoint kubeadm itself waits on. This is **not**
"the kubelet systemd unit is running" or "every control-plane container is
up"; it is specifically the kubelet's own healthz answer. A "connection
refused" result (nothing listening) is exactly what a masked or stopped
kubelet looks like and is reported the same way as a timeout or a non-200
response - all of them mean "not healthy" here.

The provider does **not** automatically re-run `kubeadm init`/`join` in this
state: recovering an established member is a deliberate operator action (an
explicit reset), never an automatic re-bootstrap. `reason: KubeletUnhealthy`
names the signal; `terminal: false` means a later boot may still converge on
its own once the kubelet is healthy again. Check `journalctl -u kubelet`,
`journalctl -u containerd`, and `crictl ps -a` to find out WHY the kubelet's
healthz is failing before deciding whether to reset. See
[Node status](./status.md) and [Lifecycle and reset](./lifecycle.md).

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
install them there. Copies you place in `/usr/local/bin` are ignored - by the
provider and, as of the entry below, by containerd and the kubelet too. See
[Security model](./security.md#exec-hygiene).

### A runtime or helper installed under `/usr/local` or `/opt` is no longer found

containerd and the kubelet run with
`PATH=/usr/sbin:/usr/bin:/sbin:/bin`, set by the image-owned drop-ins
`/usr/lib/systemd/system/{containerd,kubelet}.service.d/50-provider-kubernetes-exec-path.conf`.
`/usr/local/bin`, `/usr/local/sbin` and `/opt/containerd/bin` are **not** searched,
and containerd's image verifier and NRI plugin directories point at
`/usr/lib/containerd/image-verifier/bin` and `/usr/lib/nri/plugins` instead of
`/opt`. So a binary installed the upstream way - containerd release tarballs and
`nerdctl-full` put `containerd-shim-runc-v2` in `/usr/local/bin`, NRI plugins go to
`/opt/nri/plugins` - is ignored, and the symptom is a "not found" or "executable file
not found in $PATH" error from the daemon rather than silence.

Confirm what is in effect:

```sh
sudo systemctl show -p FragmentPath,DropInPaths,Environment containerd.service
sudo tr '\0' '\n' < /proc/$(systemctl show -p MainPID --value containerd.service)/environ | grep '^PATH='
sudo systemd-delta --type=extended /usr/lib/systemd/system
```

Then fix it at the source rather than by widening `PATH`:

- **An extra runtime** (kata, gVisor, a second runc): give its containerd runtime an
  absolute `runtime_path` (and `BinaryName` for a runc-like binary) in
  `/etc/containerd/config.toml`, and install the binaries under `/usr/bin` in a
  derived image. A `PATH` override brings back exactly the shadowing this closes.
- **A helper the kubelet needs** (a `mount.<fs>` helper, a filesystem tool):
  install it in a derived image, not on the node - `/usr/local` is the persistent
  partition, so a copy there is invisible to the image build and survives upgrades.
- **NRI plugins:** copy them into `/usr/lib/nri/plugins` in a derived image, or run
  the plugin as a normal workload that connects to the NRI socket, which is still
  enabled.
- **`pigz`/`igzip` installed but unused:** the containerd drop-in sets
  `CONTAINERD_DISABLE_PIGZ=1` and `CONTAINERD_DISABLE_IGZIP=1`, so containerd always
  uses its built-in Go gzip. A derived image that installs either for faster layer
  decompression must drop those two lines from its own copy of the drop-in.
- **`erofs` snapshotter or differ not available:** both are in `disabled_plugins` in
  `/etc/containerd/config.toml`. A derived image that wants them must remove the two
  URIs and ship `erofs-utils`.
- If you must override on the node, add your own later drop-in (for example
  `/etc/systemd/system/containerd.service.d/60-path.conf`) and accept the risk;
  do not edit the shipped file, which an upgrade replaces.

### The unit-file migration (`unit-migrate`)

`provider-kubernetes-unit-migrate.service` runs once per boot, before containerd,
the import unit and the kubelet, and removes the unit copies that releases before
v0.4.0 installed into the persistent `/etc/systemd/system`. It ends with one
summary line:

```sh
sudo journalctl -b -u provider-kubernetes-unit-migrate.service
```

| `outcome=` | Exit | Meaning |
|------------|------|---------|
| `clean` | 0 | Nothing to remove. A fresh install, a recovery boot, and every boot after the first one. |
| `migrated` | 0 | Removed `removed=N` copies. If a unit file or drop-in was among them it also reloaded systemd (`reloaded=true`), so this boot already runs the image's units; removing only `.wants` links changes no loaded definition and needs no reload (`reloaded=false`). |
| `kept-modified` | 1 | At least one copy is **not** what we shipped, so it was kept - and it is still overriding the image's unit. The unit fails, deliberately. |
| `failed` | 1 | An I/O error, an unsafe path, or a failed reload. Anything already removed stays removed; the run is retried at the next boot. |

Only a file that is **byte-identical** to something this project shipped at that
exact path is deleted. Everything else is kept and named with a reason. A symlink is
not in this table: whatever it points at, it is left silently, is not counted as
kept, does not fail the unit, and shows up only in `overrides=`:

| `reason=` | What it means |
|-----------|---------------|
| `modified` | The bytes differ from every version we shipped there - somebody edited it, or it came from a fork. |
| `not-regular` | A directory, FIFO, device or socket at that path. |
| `owner` | Not owned by root. |
| `size` | Larger than 64 KiB. |
| `link-target` | A `.wants` link pointing somewhere other than the file we installed. |
| `walk-unsafe` | A parent directory is a symlink or not a directory, so the subtree was skipped. |
| `race` | The file changed between being checked and being removed, so it was left alone. These directories are writable only by root and nothing rewrites them this early in boot, so treat it as a sign that something else touched the file. |
| `io` | The file could not be read or removed. |

The migration never logs the content, size or hash of a file it keeps - a unit file
can carry proxy credentials in an `Environment=` line.

Lines like `unit-migrate: override "/etc/systemd/system/kubelet.service.d/20-local.conf"`
are **advisory**: they list overrides of these units that remain in a directory that
outranks `/usr/lib/systemd/system`. Nothing is done about them; drop-ins there are
the supported way to customize the units.

To fix a `kept-modified`:

```sh
sudo systemctl cat kubelet.service        # what is in effect, and from which file
sudo systemd-delta --type=extended /usr/lib/systemd/system
```

then move your change into `/etc/systemd/system/<unit>.d/20-local.conf`, delete the
full-unit copy, `daemon-reload`, and re-run the unit
(`systemctl restart provider-kubernetes-unit-migrate.service`) to clear the failure.
See [Upgrades](./upgrades.md#unit-files-moved-into-the-image).

`systemctl disable` does nothing to these units (they are `static`); use
`systemctl mask`, which now sticks.

### kubeadm pulls control-plane images even though the image bundles them

The image bundles the control-plane images for its Kubernetes version and imports
them into containerd at boot, so `kubeadm init`/`join` should not need a registry.
[`samples/air-gapped/`](../samples/air-gapped/) has the pre-flight checks to run
while a registry is still reachable.
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
  --image-endpoint unix:///run/containerd/containerd.sock \
  inspecti -o go-template --template '{{.status.id}}' registry.k8s.io/pause:<tag>
sudo /usr/bin/tar -xOf /system/provider-kubernetes/images/registry.k8s.io_pause_<tag>.tar manifest.json
```

If the images are missing, check the boot import first (next entry).

### import-images refused an image

At every boot the provider imports the bundled images listed in
`/system/provider-kubernetes/images/images.lock`, and only those. Each run ends with
one summary line:

```sh
sudo journalctl -u provider-kubernetes-image-import --no-pager | grep 'image-import:'
sudo grep 'image-import:' /var/log/provider-kubernetes-image-import.log | tail -n 20
sudo /system/providers/agent-provider-kubernetes import-images --verify-only
```

```text
image-import: summary outcome=success entries=7 imported=7 refused=0 failed=0 unlisted=0 readonly=true dir=/system/provider-kubernetes/images
```

`outcome` is `success`, `partial`, `refused`, `failed` or `not-bundled` (the image
bundles nothing); `--verify-only` runs every check without importing and reports
`verified` or `refused`. A refused image has its own line,
`image-import: refused <tarball> ref=<ref> reason=<reason> ...`. A problem with the
bundle as a whole (a directory, the provider binary used as the reference, or
`images.lock` itself) is logged once as `image-import: bundle refused reason=<reason>
...`, and nothing is imported:

| Reason | Meaning |
|--------|---------|
| `dir-unsafe`, `anchor-unsafe` | A directory on the path, or the provider binary used as the reference, is not a root-owned directory/file without group or other write. Nothing is imported. |
| `lock-missing`, `lock-invalid` | `images.lock` is missing or not exactly what the build writes. Nothing is imported. |
| `missing`, `symlink`, `not-regular`, `owner`, `mode`, `size` | The tarball (or, on a `bundle refused` line, `images.lock`) is absent, a symlink, not a regular file, not owned by root, writable by group or others, or outside the size limits. |
| `device` | The file is not on the same filesystem as the provider binary, for example because something is bind-mounted over the bundle directory (including a `CUSTOM_BIND_MOUNTS` entry). |
| `tar-structure`, `manifest`, `config-digest` | The tarball is not the expected image archive, names a different reference, or its image config does not match its name. |
| `ctr-failed`, `deadline` | containerd rejected the import, or the 5 minute bound ran out. |

Files in the bundle directory that are not in `images.lock` are never opened; they
are reported as `unlisted`. The import never blocks boot. A refused image is not
imported: on a node without registry access kubeadm then fails to find it, and on a
node with registry access kubeadm pulls it from the registry by tag instead. Nothing
on a supported image should be refused; treat a refusal as a sign that the booted
OS image or its mounts were changed.

Nodes upgraded from an image that bundled under `/opt` still have a copy in
`/opt/provider-kubernetes` (see [Upgrades](./upgrades.md#rollback)); it is no longer
read.

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
