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

The only at-rest secret is kubeadm's own PKI under `/etc/kubernetes/pki` on
control-plane nodes (`0600 root:root`), persisted by Kairos's default
`/etc/kubernetes` bind-mount to the persistent partition. For production,
**kcrypt (TPM2) at-rest encryption of the persistent partition on control-plane
nodes is a documented requirement**. The provider emits a one-time runtime warning
if it cannot confirm the persistent partition is encrypted; it never hard-fails on
missing encryption.

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

Every external binary baked into the image (kubeadm, kubelet, kubectl,
containerd, runc, CNI plugins, crictl) is pinned and **checksum-verified** against
the publisher's HTTPS-served checksum during the image build. The container
sandbox image is pinned to `registry.k8s.io/pause` (not the dead `k8s.gcr.io`).
Signature/provenance verification is a tracked future enhancement.
