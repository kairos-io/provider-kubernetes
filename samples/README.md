# Sample cloud-configs and image build

This directory contains example Kairos cloud-configs for the three node roles
the provider supports, and points at the `Dockerfile` you build the image with.

**The project is under active development and is NOT field-ready.** Treat these
samples as design demos, not as production templates.

## Files

| File | Role | Notes |
|------|------|-------|
| `master.yaml`       | `init`         | First control-plane node; runs `kubeadm init`. |
| `controlplane.yaml` | `controlplane` | Additional control-plane node; joins via token + cert key. |
| `worker.yaml`       | `worker`       | Worker node; joins via token + CA hash. |
| `ha/`               | HA             | Multi-control-plane (stacked-etcd) walkthrough: stable endpoint, one-at-a-time bring-up, reset/etcd-orphan runbook. |
| `cni-calico/`       | CNI            | Calico CNI examples (post-hoc apply and bundled-in-cloud-config). |

## Build the image

The root `Dockerfile` builds a Kairos image that bundles the provider plus
kubeadm, kubelet, kubectl, containerd, runc and the CNI plugins. **Every
external binary download is checksum-verified** against the publisher's
HTTPS-served `.sha256` file — a deliberate fix for the unverified-`curl`
supply-chain pitfall of the previous kubeadm provider.

```sh
docker build \
  --build-arg KUBERNETES_VERSION=v1.34.0 \
  --build-arg PROVIDER_VERSION=$(git describe --always) \
  -t ghcr.io/<you>/kairos-kubeadm:dev .
```

Supported `KUBERNETES_VERSION` values track the provider's runtime
supported-window (currently `v1.34.x` / `v1.35.x` / `v1.36.x`). A mismatch
between the pinned version and the bundled `kubeadm` binary is a hard error at
runtime (no best-effort "close enough" behavior).

## Convert to a bootable artifact

The Dockerfile produces a Kairos OCI image. Convert it into a bootable ISO/raw
image with [auroraboot](https://kairos.io/docs/reference/auroraboot/):

```sh
mkdir -p build
docker run --rm -it \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD/build:/build" \
  quay.io/kairos/auroraboot:latest \
    --debug build-iso \
    --output /build \
    docker://ghcr.io/<you>/kairos-kubeadm:dev
```

The resulting ISO can be booted with a cloud-config (`samples/master.yaml` etc.)
injected via your usual Kairos provisioning channel (ISO+config, PXE, the
config_url field, etc.).

## Cluster creation flow

1. Boot a node from the ISO with `samples/master.yaml` (edit
   `cluster_token`, `control_plane_host`, and `kubernetesVersion`).
   With `install.auto: true` and a user in the `admin` group, the install
   runs unattended; on reboot the provider runs `kubeadm init` automatically.
2. On the control-plane node, mint join material with the provider's
   `mint-join` helper. It mints a bounded-TTL bootstrap token, computes the
   cluster CA SPKI pin, derives the API endpoint from `admin.conf`, and prints
   a ready-to-paste cloud-config — no manual `openssl`/`kubeadm token` dance:
   ```sh
   # Worker join cloud-config (token + CA pin):
   sudo agent-provider-kubernetes mint-join \
     --role worker --ttl 1h \
     --cluster-token "<the cluster's cluster_token>" > worker-join.yaml

   # Additional control-plane: --role controlplane also mints a fresh
   # certificate-encryption key (upstream kubeadm applies a 2h expiry on it).
   sudo agent-provider-kubernetes mint-join \
     --role controlplane --ttl 2h \
     --cluster-token "<the cluster's cluster_token>" > cp-join.yaml
   ```
   Flags: `--endpoint host:port` overrides the auto-derived endpoint (e.g. a
   load-balanced `controlPlaneEndpoint`); `--root-path` locates `admin.conf`
   and `ca.crt` when the cluster root is not `/`. The printed material is the
   only copy — the provider persists none of it.
3. Deliver the printed cloud-config to each joining node (the provider does not
   pull join material; the operator delivers it out-of-band per ADR-10). Tokens
   default to a 1h TTL; re-mint and redeliver if a node will boot later. The
   `samples/worker.yaml` / `controlplane.yaml` files show the same shape by hand
   if you prefer to assemble it yourself.
4. Boot the joiners. The provider runs `kubeadm join` with CA pinning enforced
   by construction; joins refuse to proceed without a CA anchor.
5. Install a CNI plugin (Flannel, Calico, Cilium, ...). The provider does not
   install one.

## Why each design choice

- **Pinned + checksum-verified downloads:** integrity from the canonical
  publisher's HTTPS-served `.sha256` files; an unverified or tampered binary
  fails the image build, not a runtime cluster.
- **`registry.k8s.io`, not `k8s.gcr.io`:** the latter was frozen in 2023 and is
  deprecated.
- **Bounded-TTL tokens, never persisted on joiners:** the provider mints
  CSPRNG kubeadm-native tokens with a 1h default TTL; joiners consume them once
  and store nothing. A reboot mid-bootstrap is recovered by re-minting and
  redelivering, never by re-deriving a permanent secret.
- **CA pinning is mandatory:** `caCertHashes` is required for token discovery;
  `unsafeSkipCAVerification` is structurally never set true.
- **`cluster_token` is a correlation value, not key material:** the provider
  does not derive any kubeadm credential from it.
