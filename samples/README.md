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
2. On that node, mint join material for the additional nodes you want
   (kubeadm is now available on the image):
   ```sh
   # Worker join material:
   kubeadm token create --ttl 1h
   openssl x509 -pubkey -in /etc/kubernetes/pki/ca.crt \
     | openssl rsa -pubin -outform der 2>/dev/null \
     | openssl dgst -sha256 -hex | awk '{print "sha256:"$2}'

   # Additional control-plane: also mint a fresh certificate-encryption key.
   kubeadm init phase upload-certs --upload-certs
   ```
3. Edit `samples/worker.yaml` / `controlplane.yaml` with the freshly minted
   token, CA hash, and (for CP) certificate key. Tokens default to a 1h TTL;
   re-mint and redeliver if a node will boot later than that.
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
