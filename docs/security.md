# Security model

The provider is secure-by-default. This page explains the trust model so you can
operate it safely and understand the blast radius of each secret.

## cluster_token is not key material

`cluster_token` is a low-trust **correlation value**. The provider never derives a
credential from it (no `SHA-256(cluster_token)` anywhere); kubeadm's own CSPRNG
generators produce all tokens and keys.

- Validated at ingest: rejected if empty or under 16 characters; a loud one-time
  warning below roughly 128 bits of estimated entropy.
- Never logged. A leaked `cluster_token` does not, by itself, grant cluster
  access. Still, keep it confidential.

## Join material: bootstrap token + CA pin

Worker and control-plane joins use a **bounded-TTL bootstrap token** (default 1h)
plus a **CA SPKI pin** (`caCertHashes`). CA pinning is **mandatory and enforced by
construction**: the provider refuses to emit a token-discovery join config without
a CA hash and never sets `unsafeSkipCAVerification`. For externally-managed control
planes you supply the anchor (a CA hash, `CACerts`, or a CA-embedded discovery
file); supplying both a hash and `CACerts` cross-validates them and fails loud on
mismatch.

A leaked **worker** token grants, within its TTL, the ability to join one node as
a `system:node` identity. Bounded and comparatively low-value.

## The control-plane certificate key is root-equivalent

A control-plane join additionally needs a `certificateKey` that decrypts the
cluster PKI - **including the CA private key** - uploaded to the `kubeadm-certs`
Secret. Anyone holding the cert key (plus a live token and API reachability) can
obtain the CA key and mint a credential for any identity. **Treat a control-plane
join bundle as a cluster root credential.**

Controls the provider enforces:

- The cert key is minted **fresh per control-plane join**, never reused or
  persisted by the provider.
- It flows only through a `0600` file on tmpfs (`/run`), consumed via kubeadm's
  `--config`, and shredded immediately after the kubeadm process returns. It never
  appears on a command line and is never logged (kubeadm stderr is sanitized at
  the error boundary; structs that carry it self-redact).
- The upstream **2h expiry** on the `kubeadm-certs` Secret is preserved (never
  stripped, never `TTL: 0`). A leaked cert key is worthless once the Secret
  self-deletes.

Operator responsibilities:

- Deliver control-plane material only over a **confidential, integrity-protected**
  channel, just before the node boots, fresh per node.
- Do not store, log, or commit a rendered control-plane cloud-config; do not leave
  it on the joined node's persistent storage after first boot.

## The trust boundary in v1

Under the operator-delivered model there is **no node attestation in v1**: the
provider cannot verify that the node receiving join material is the node you
intended. **The delivery channel is the trust boundary.** For control-plane
material the consequence of a compromised channel is full cluster takeover, not
one rogue worker. The pre-registered future hardening is **TPM2 node attestation**.

## Secrets at rest

The provider persists no bootstrap secrets of its own. A control-plane node still
holds full-cluster secrets at rest, all on the persistent partition:

| Artifact | Location | Notes |
|----------|----------|-------|
| kubeadm PKI, including the CA private keys | `/etc/kubernetes/pki` | `0600 root:root`. |
| kubeconfigs with embedded client keys | `/etc/kubernetes/*.conf` | `admin.conf` is cluster-admin. |
| Live etcd data (every Secret) | `/var/lib/etcd` | |
| kubeadm's etcd data-dir copies | `/etc/kubernetes/tmp/kubeadm-backup-etcd-*` | Written by `kubeadm upgrade` on every stacked control plane, regardless of encryption, and kept until you delete them or reset. |
| Provider pre-upgrade etcd snapshot | `/usr/local/provider-kubernetes/etcd-backup/` | Written only onto dm-crypt; kept until the next upgrade's snapshot; **not** removed by reset. |

All of these are plaintext on disk unless the persistent partition is encrypted.
For production, **kcrypt (TPM2) at-rest encryption of the persistent partition on
control-plane nodes is a documented requirement**. The provider never hard-fails on
missing encryption, and it does not yet emit a general runtime warning when it
cannot confirm encryption (tracked). The one place it checks is the pre-upgrade
etcd snapshot, which it refuses to write unless the snapshot directory is on
dm-crypt (see [Upgrades](./upgrades.md#etcd-backups)).

When you **decommission** a control plane, or **rotate credentials after an
incident**, delete the provider snapshot directory and any
`/etc/kubernetes/tmp/kubeadm-backup-etcd-*` copies (or wipe the disk): they keep
Secrets and keys that you have since deleted or rotated in the live cluster.

`/run` must be tmpfs (it is, on every supported Kairos image) - this is
load-bearing for control-plane joins, because the transient config there decrypts
the CA key.

## Never clobber an existing cluster

A node configured `role: init` against an endpoint where a control plane already
answers is **refused** (a loud, terminal error directing you to `role:
controlplane`), rather than running `kubeadm init` and destroying the existing
cluster. This protects both Kairos-bootstrapped and externally-managed control
planes.

## Exec hygiene

Every tool the provider runs is an absolute path in the booted image, started with
an environment the provider builds itself. Nothing is looked up on `PATH`, and
nothing else is inherited from the provider's own environment.

| Tool | Path | Environment |
|------|------|-------------|
| kubeadm | `/usr/bin/kubeadm` | `PATH=/usr/sbin:/usr/bin:/sbin:/bin` plus the proxy variables |
| kubectl | `/usr/bin/kubectl` | a tmpfs discovery cache (`/run/provider-kubernetes/kubectl-cache`), kuberc disabled, plus the proxy variables |
| ctr | `/usr/bin/ctr` | empty |
| systemctl | `/usr/bin/systemctl` | empty |
| etcdctl | `/usr/bin/etcdctl` | empty |

Why it matters on Kairos: `/usr/local` is the persistent partition and comes before
`/usr/bin` in the default `PATH`. A binary placed in `/usr/local/bin` - by an
operator, a sample script, or anyone with a one-time root write - would otherwise
run instead of the bundled, verified tool and survive image upgrades. Inherited
variables could also change what a tool does: `SYSTEMD_OFFLINE=1` turns
`systemctl restart kubelet` into a successful no-op, `CONTAINERD_ADDRESS` sends the
image import to another socket, a kuberc file injects kubectl flags such as
`--server`, and `GODEBUG` or `SSL_CERT_FILE` change TLS behavior. The helpers
kubeadm looks up itself (systemctl, kubelet, cp, mount, losetup, modprobe) use the
fixed `PATH` above, so they never come from `/usr/local` either.

Consequences to be aware of:

- **Proxy variables.** Only `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` (and their
  lowercase forms) are passed on. kubeadm copies them verbatim into the
  control-plane static pods and the kube-proxy DaemonSet, so credentials in a proxy
  URL are readable by anyone who can read those objects. `ALL_PROXY` and other
  `*_proxy` names are no longer copied. See
  [Configuration](./configuration.md#proxy-environment).
- **Not passed:** `KUBECONFIG`, `HOME`, `GODEBUG`, `SSL_CERT_FILE`/`SSL_CERT_DIR`,
  and any `KUBEADM_*`, `KUBERC`, `SYSTEMD_*` or `CONTAINERD_*` variable. Go runtime
  modes such as FIPS must come from how the tools were built, not from the
  environment.
- **Custom images** must install these tools at `/usr/bin`; otherwise the provider
  fails loudly, naming the tool and path.

### Daemons

containerd and the kubelet are started by systemd, not by the provider, and they
resolve a number of helpers by name. The image ships settings that keep those
lookups inside the read-only OS image:

- **`PATH` drop-ins.** `/usr/lib/systemd/system/containerd.service.d/50-provider-kubernetes-exec-path.conf`
  and the matching `kubelet.service.d` file set
  `Environment=PATH=/usr/sbin:/usr/bin:/sbin:/bin` - the same value the provider
  gives kubeadm. This covers the kubelet's `mount`, `umount`, `systemd-run`,
  `blkid`, `losetup` and `iptables` calls, containerd's own lookups, and everything
  they start, including CNI plugins (which inherit containerd's environment). The
  files live under `/usr/lib`, so an upgrade always replaces them and they also
  apply to the unit copies Kairos keeps on the persistent `/etc/systemd`.
- **Two lookups closed at the source.** The containerd drop-in also sets
  `CONTAINERD_DISABLE_IGZIP=1` and `CONTAINERD_DISABLE_PIGZ=1`, and
  `disabled_plugins` disables `io.containerd.differ.v1.erofs` and
  `io.containerd.snapshotter.v1.erofs`. Those are the three names containerd would
  look up and *not* find anywhere in the image - `igzip`, `unpigz` and `mkfs.erofs`
  - which is the one thing an appended `PATH` element could still answer (see
  below). containerd now skips those lookups entirely and uses its built-in Go gzip.
  The cost is zero on this image, which ships none of the three. A derived image
  that installs `pigz`/`igzip` for faster decompression must drop those two
  variables; one that wants erofs must remove the two plugin URIs and ship
  `erofs-utils`.
- **`runtime_path = "/usr/bin/containerd-shim-runc-v2"`** and
  **`BinaryName = "/usr/bin/runc"`** in containerd's config. The shim and runc are
  taken by absolute path, so neither the working directory, nor `PATH`, nor a
  directory next to containerd is ever searched - this holds even if the `PATH`
  drop-in is overridden.
- **Three `/opt` directories closed.** `/opt` is on the persistent partition.
  containerd's `opt` internal plugin is disabled, so it no longer prepends
  `/opt/containerd/bin` to the daemon's own `PATH` and `/opt/containerd/lib` to its
  `LD_LIBRARY_PATH`; the image verifier reads `/usr/lib/containerd/image-verifier/bin`
  instead of `/opt/containerd/image-verifier/bin` (it runs binaries from there on
  every image pull); and NRI launches plugins from `/usr/lib/nri/plugins` instead of
  `/opt/nri/plugins` (it launches every executable found there at each containerd
  start, and an NRI plugin can rewrite every container's OCI spec). Both image-only
  directories ship empty. NRI itself stays enabled, so plugins that connect over the
  NRI socket keep working.

One `/opt` path is **not** closed, because containerd offers no setting for it: its
CRI pod-sandbox code calls the deprecated NRI v0.1 client on every sandbox
create/delete, and that client appends `/opt/nri/bin` to containerd's own `PATH`, at
the first sandbox create and once per daemon, whether or not any such plugin exists.
Because it is appended and never prepended, it cannot shadow a binary the image
ships - it can only supply a name the image does not have. The three unconditional
such names are closed by the settings above; what remains depends on features this
image does not configure (FUSE mounts, checkpoint/`criu`, devmapper). Treat
`/opt/nri/bin` as a root-exec input on the node, and expect this to be re-checked at
every containerd bump.

This is an integrity measure, **not a privilege boundary**. Kairos keeps all
persistent state as bind mounts out of `/usr/local/.state`, so anyone who can write
`/usr/local` can already write `/etc/systemd`, `/opt`, `/var/lib/kubelet` and
`/etc/kubernetes`. What it does fix is the accident: a runtime or helper installed
under `/usr/local` by following upstream instructions silently replaces a root
component of the node and survives every A/B upgrade, which means a fix delivered in
a new image never takes effect. It also makes such drift visible.

What this does **not** cover:

- It stops name-based shadowing for the provider's own commands. It trusts the
  booted `/usr`: anyone who can change its contents (a tampered image, or an
  activated systemd-sysext overlay) controls these binaries.
- **The daemon settings are overridable by anyone with root.** systemd reads unit
  fragments and `<unit>.d/` drop-ins from `/etc/systemd/system` (persistent),
  `/etc/systemd/system.control`, `/run/systemd/system`,
  `/usr/local/lib/systemd/system` (persistent) and the top-level `service.d/`
  directories, all of which outrank `/usr/lib`; an activated systemd-sysext can
  overlay `/usr/lib` itself; and an `EnvironmentFile=` outranks `Environment=`, so
  a `PATH` line in `/var/lib/kubelet/kubeadm-flags.env` (persistent) or
  `/etc/default/kubelet` (rebuilt from the image each boot) wins over the drop-in.
- Kairos itself runs yip stage commands through a `sh` found on `PATH`, and plugin
  discovery scans `PATH`. The provider only removes this for its own execs.
- **Persistent inputs that run as root by design** are unchanged and out of scope:
  `/etc/kubernetes/manifests` (static pods), `/var/lib/kubelet` (`config.yaml`,
  `kubeadm-flags.env`), `/etc/cni/net.d` and `/opt/cni/bin`, `/etc/modprobe.d`
  (install directives), the FlexVolume directory
  `/usr/libexec/kubernetes/kubelet-plugins/volume/exec` (the kubelet runs
  `<driver> init` from it), `/var/lib/extensions` and `/var/lib/confexts` (sysext
  and confext images), and `/etc/ssl/certs`.
- Booting a new image therefore does not make a tampered node known-good: it
  replaces `/usr` and the image-owned units, and leaves every persistent path above
  exactly as it was.
- When a kubeadm run hits its deadline and is killed, a helper it started (for
  example the `cp -r` of the etcd data directory during an upgrade) can keep
  running; the provider stops waiting for it after 5 seconds.
- Commands run by linked Kairos libraries themselves are outside these checks; the
  provider's code paths are not known to reach them.

## Supply chain

- **Downloaded binaries** (kubeadm, kubectl, crictl, runc, CNI plugins) are pinned
  and **checksum-verified** against the publisher's HTTPS-served checksums during
  the image build.
- **kubelet and containerd** are built fully static from source (Hadron is musl),
  cloned at the version tag and pinned to the expected commit SHA.
- **Control-plane images** are resolved to digests from the bundled kubeadm's own
  image list, **cosign-verified** against the Kubernetes release signing identity
  where upstream signs them (pause, etcd and coredns must verify or the build
  fails), and pulled by that digest. `images.lock` in the image records each
  digest and whether it verified. The sandbox image is the version-matched
  `registry.k8s.io/pause`.
  Each image tarball records the exact reference kubeadm uses, for example
  `registry.k8s.io/pause:3.10.2`. Pulling by digest makes crane store a placeholder
  tag, so the build replaces that one name after the verified pull. It checks that
  the image config and layers are byte-for-byte unchanged, and CI and the
  end-to-end tests check that containerd lists each image under its exact
  reference.
  The bundle lives in `/system/provider-kubernetes/images`, part of the read-only
  OS image and not under the persistent `/opt`. At every boot the provider imports
  only the images listed in `images.lock`. It first checks that the directories,
  the lock file and each tarball are owned by root, not writable by group or
  others, not symlinks, and on the same filesystem as the provider binary, and that
  each tarball names exactly its lock reference. Image layers are not re-hashed at
  boot: containerd checks layer contents against the image configuration when it
  unpacks them. The bundle is trusted exactly as much as the provider binary beside
  it. Anyone who can change the booted OS image, or who boots with a writable root,
  can change both. On nodes with registry access, an image that fails these checks
  is pulled from the registry by tag instead.
  Images already in containerd's persistent store are not re-verified; a layer that
  is already unpacked is reused as is. A node booted from an older image restores
  and imports the old `/opt` copy.
- **`etcdctl` and `etcdutl`** are not downloaded separately: they are extracted
  from that verified etcd image, so they come from an attested digest and match
  the etcd version kubeadm deploys. CI checks that the shipped `/usr/bin` binaries
  are byte-identical to the ones inside the bundled etcd image.
- Base images and workflow actions are digest/SHA-pinned, and released images and
  binaries carry SLSA build-provenance and SBOM attestations (see
  [Testing](./testing.md#release-artifact-provenance)).

To check the etcd tools on an image yourself, compare three copies of `etcdctl`
with [crane](https://github.com/google/go-containerregistry), which reads images
without loading them into your local image store (a `docker load` would create or
overwrite your own `registry.k8s.io/etcd:<tag>`). All three hashes must match:

```sh
img=<your image>
dir=/system/provider-kubernetes/images
work="$(mktemp -d)"
docker run --rm --entrypoint cat "$img" "$dir/images.lock" > "$work/images.lock"
sel='.images[] | select(.ref | test("/etcd:"))'
ref="$(jq -r "$sel | .ref" "$work/images.lock")"
digest="$(jq -r "$sel | .digest" "$work/images.lock")"
tarball="$(jq -r "$sel | .tarball" "$work/images.lock")"

# 1. The shipped binary (docker create only materializes the filesystem; nothing runs).
n="$(docker create --pull never "$img" x)"
docker cp "$n:/usr/bin/etcdctl" - | tar -xOf - | sha256sum
docker rm "$n" >/dev/null

# 2. The binary inside the bundled etcd tarball (offline). Matching 1 proves the
#    image is internally consistent, not that it matches the attested digest.
docker run --rm --entrypoint cat "$img" "$dir/$tarball" > "$work/etcd.tar"
crane export - - < "$work/etcd.tar" | tar -xOf - usr/local/bin/etcdctl | sha256sum

# 3. The binary in the upstream etcd image at the digest images.lock records (needs
#    registry access). Matching 1 ties the shipped binary to that digest.
crane export --platform linux/amd64 "${ref}@${digest}" - | tar -xOf - usr/local/bin/etcdctl | sha256sum

rm -rf "$work"
```

Use `--platform linux/arm64` for an arm64 image.

CI runs comparisons 1 and 2 (plus `etcdutl`, file mode/owner, and the version) for every
supported minor.
