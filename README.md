# provider-kubernetes

> **Found a bug, or want to request a feature?** Open it on
> [kairos-io/kairos](https://github.com/kairos-io/kairos/issues), including
> issues about this repository. Every Kairos issue lives in one place, so you
> never have to work out which repository to file against.

A [Kairos](https://kairos.io) cluster provider that bootstraps **upstream
Kubernetes** using `kubeadm`, while remaining native to the Kairos ecosystem
(the `clusterplugin` / `yip` contract).

## What this is

`provider-kubernetes` gives Kairos users a first-class way to **create and
manage kubeadm-based Kubernetes clusters**, that plugs into the Kairos
cluster lifecycle.

Its design takes the feedback in
[kairos-io/kairos#4099](https://github.com/kairos-io/kairos/issues/4099) as a
starting point, notably:

- **Externally-managed control planes** are a supported topology, not an
  afterthought.
- **Tracks upstream Kubernetes (N, N-1, N-2).** The provider supports the three
  most recent in-support upstream Kubernetes minors (currently 1.34 / 1.35 /
  1.36), rolling the window forward as new minors ship — matching upstream's
  support policy.

## What works today

- **Single-node and multi-node clusters.** A `role: init` node runs
  `kubeadm init`; `worker` and `controlplane` nodes join with CA pinning.
- **Multi-control-plane HA (stacked etcd).** Additional control planes join an
  existing cluster behind a stable endpoint, etcd membership grows correctly,
  and a misconfigured second `role: init` is refused rather than clobbering the
  cluster. See [`samples/ha/`](./samples/ha/).
- **Unattended install.** Booting a Kairos image built from this repo with one
  of the sample cloud-configs installs to disk and bootstraps the cluster with
  no manual steps.
- **`mint-join` helper.** On a control-plane node,
  `agent-provider-kubernetes mint-join --role worker|controlplane` mints
  bounded-TTL join material (for control planes, re-uploading the cluster certs
  under a fresh certificate key), computes the cluster CA SPKI pin, derives the
  API endpoint from `admin.conf`, and prints a ready-to-paste join cloud-config.
- **Cluster upgrades (`kubeadm upgrade`).** Pin a newer `kubernetesVersion` and
  boot the matching image; the provider converges the cluster one minor at a time
  (control plane via `upgrade apply`, followers/workers via `upgrade node`),
  refusing downgrades / skip-level / out-of-window. It auto-repairs the kubelet
  config when an image swap leaves the new kubelet unable to start, and takes a
  best-effort etcd snapshot (only onto encrypted storage). See
  [`docs/upgrades.md`](./docs/upgrades.md).
- **CNI is the operator's choice.** The provider installs no CNI by design (no
  vendor lock-in). Two worked examples ship in `samples/` —
  [`samples/cni-flannel/`](./samples/cni-flannel/) and
  [`samples/cni-calico/`](./samples/cni-calico/) — each showing both ways to
  install one: apply it after the cluster is up, or bundle it in the
  control-plane cloud-config.
- **Built on Hadron (musl).** Images are built on the Kairos **Hadron** minimal,
  musl-based immutable OS. containerd and kubelet are built static from pinned
  source; the remaining binaries are verified static downloads. See
  [`docs/hadron.md`](./docs/hadron.md).
- **CI and releases.** Every push runs build / vet / test / lint, a Kairos image
  build across the supported Kubernetes window, and a real-kubeadm **end-to-end
  suite** (init, worker join, externally-managed-CP join, reset, and the
  refuse-guards) in a privileged node container per minor; tagged releases publish
  per-minor images to ghcr (see "Released images"). See
  [`docs/testing.md`](./docs/testing.md) for the test layers and coverage boundary.

## Building

Requires Go 1.26.4+.

```sh
make build      # produces ./bin/agent-provider-kubernetes
make test
make vet
make lint       # requires golangci-lint
```

The provider binary follows the Kairos naming convention
(`agent-provider-*`) and is intended to be installed under
`/system/providers/` inside a Kairos image.

## Building a Kairos image

The `Dockerfile` at the repo root builds a Kairos image that bundles the
provider plus kubeadm, kubelet, kubectl, containerd, runc and the CNI
plugins. Every external binary download is **checksum-verified** against the
publisher's HTTPS-served `.sha256` file:

```sh
make image KUBERNETES_VERSION=v1.34.0 VERSION=dev
```

The base is the Kairos **[Hadron](./docs/hadron.md)** minimal, musl-based immutable OS,
which `kairos-init` transforms into a bootable Kairos system (mirroring the
canonical Kairos image build). Because Hadron is musl, containerd and kubelet are
built **fully static from pinned source** (the official glibc binaries can't exec
on musl); kubeadm/kubectl/crictl/runc are the verified static downloads. This is
automatic - no build flags. To build a non-default Kubernetes minor, also pass the
matching `KUBERNETES_COMMIT` (see [`docs/hadron.md`](./docs/hadron.md)).

Convert the resulting Docker image into a bootable ISO/raw artifact with
[auroraboot](https://kairos.io/docs/reference/auroraboot/), and provision it
with one of the sample cloud-configs in [`samples/`](./samples/).

## Released images

Tagged releases publish a Kairos image per supported Kubernetes minor to the
GitHub Container Registry, so you can test without building locally:

```sh
# pick the Kubernetes minor you want (1.34 / 1.35 / 1.36):
docker pull ghcr.io/kairos-io/provider-kubernetes:v0.3.0-k8s1.34

# the newest supported minor is also published as the plain tag and :latest:
docker pull ghcr.io/kairos-io/provider-kubernetes:v0.3.0
docker pull ghcr.io/kairos-io/provider-kubernetes:latest
```

Each release also attaches the provider binary (linux/amd64) plus a sha256
checksum. This is an early public release supporting the 1.34 / 1.35 / 1.36
Kubernetes window; see [`docs/testing.md`](./docs/testing.md) for the coverage
boundary. It is not yet certified for production use, and configuration and
behavior may still change between minor releases — pin a released image tag.

Every image and release binary is signed with keyless SLSA build-provenance and
CycloneDX SBOM attestations. To verify them (and for the release process), see
[`CONTRIBUTING.md`](./CONTRIBUTING.md#verifying-a-release).

## Creating a cluster

The [`samples/`](./samples/) directory has cloud-configs for each node role,
and its [README](./samples/README.md) walks through the end-to-end flow:

| File | Role |
|------|------|
| [`samples/master.yaml`](./samples/master.yaml) | first control-plane (`role: init`) |
| [`samples/controlplane.yaml`](./samples/controlplane.yaml) | additional control-plane join |
| [`samples/worker.yaml`](./samples/worker.yaml) | worker join |
| [`samples/cluster.yaml`](./samples/cluster.yaml) | annotated reference covering the full kubeadm v1beta4 surface |
| [`samples/cni-flannel/`](./samples/cni-flannel/) | Flannel CNI (simplest), post-hoc or bundled in the cloud-config |
| [`samples/cni-calico/`](./samples/cni-calico/) | Calico CNI, post-hoc or bundled in the cloud-config |

The flow: boot the first control-plane node from a sample; once it is up, mint
join material with `agent-provider-kubernetes mint-join` and drop it into the
worker/controlplane configs; boot the joiners; install a CNI.

## Documentation

Usage documentation lives in [`docs/`](./docs/):

| Document | Covers |
|----------|--------|
| [Getting started](./docs/getting-started.md) | From nothing to a running single-node control plane. |
| [Configuration reference](./docs/configuration.md) | The full `cluster` cloud-config contract. |
| [Creating a cluster](./docs/creating-a-cluster.md) | Single control plane plus workers. |
| [High availability](./docs/high-availability.md) | Multi-control-plane (stacked etcd). |
| [mint-join](./docs/mint-join.md) | The join-material CLI. |
| [Upgrades](./docs/upgrades.md) | Upgrading between Kubernetes minors with `kubeadm upgrade`. |
| [CNI](./docs/cni.md) | Installing a CNI (Flannel or Calico). |
| [Running on Hadron](./docs/hadron.md) | The Hadron (musl) base: static runtime, supply-chain pinning. |
| [Security model](./docs/security.md) | Tokens, the cert-key blast radius, CA pinning, at-rest encryption. |
| [Lifecycle and reset](./docs/lifecycle.md) | Reconcile, reset, the version window, upgrades. |
| [Node status](./docs/status.md) | Why a node did or did not converge: status file + Node annotations. |
| [Testing](./docs/testing.md) | Test layers, coverage boundary, and release provenance. |
| [Troubleshooting](./docs/troubleshooting.md) | Logs, common failures, filing issues. |

## Contributing

Contributions are welcome. See [`CONTRIBUTING.md`](./CONTRIBUTING.md) for building,
testing, sign-off (DCO), and release/verification details. Please open an issue to
discuss substantial changes before submitting a pull request.

## License

Licensed under the [Apache License 2.0](./LICENSE).
