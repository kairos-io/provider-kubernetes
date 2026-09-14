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
- **`etcdctl` and `etcdutl`** are not downloaded separately: they are extracted
  from that verified etcd image, so they come from an attested digest and match
  the etcd version kubeadm deploys. CI checks that the shipped `/usr/bin` binaries
  are byte-identical to the ones inside the bundled etcd image.
- Base images and workflow actions are digest/SHA-pinned, and released images and
  binaries carry SLSA build-provenance and SBOM attestations (see
  [Testing](./testing.md#release-artifact-provenance)).

To check the etcd tools on an image yourself, read the etcd entry (digest,
`verified`) from the lockfile, load that tarball, and compare the binaries:

```sh
img=<your image>
docker run --rm --entrypoint cat "$img" /opt/provider-kubernetes/images/images.lock
tar="$(docker run --rm --entrypoint cat "$img" /opt/provider-kubernetes/images/images.lock \
  | jq -r '.images[] | select(.ref | test("/etcd:")) | .tarball')"
etcd_ref="$(docker run --rm --entrypoint cat "$img" "/opt/provider-kubernetes/images/$tar" \
  | docker load | sed -n 's/^Loaded image: //p')"   # registry.k8s.io/etcd:i-was-a-digest
# docker create only materializes the filesystems; neither image is run.
e="$(docker create --pull never "$etcd_ref" x)"; n="$(docker create --pull never "$img" x)"
docker cp "$e:/usr/local/bin/etcdctl" - | tar -xO | sha256sum
docker cp "$n:/usr/bin/etcdctl" - | tar -xO | sha256sum
docker rm "$e" "$n"; docker image rm "$etcd_ref"
```

CI runs this comparison (plus `etcdutl`, file mode/owner, and the version) for every
supported minor.
