# provider-kubernetes

A [Kairos](https://kairos.io) cluster provider that bootstraps **upstream
Kubernetes** using `kubeadm`, while remaining native to the Kairos ecosystem
(the `clusterplugin` / `yip` contract).

> ## Under active development — NOT ready for field testing
>
> This project is in **early development**. It is **not** production-hardened
> and is **not** ready for field testing, staging, or production use. It can
> bootstrap clusters in validation (see "What works today"), but interfaces,
> configuration, and behavior **will** change without notice. Do not deploy it
> on machines you care about.

## What this is

`provider-kubernetes` gives Kairos users a first-class way to **create and
manage kubeadm-based Kubernetes clusters**. It is a new, **fully Go-native**
provider — no shell scripts driving the bootstrap — that plugs into the Kairos
cluster lifecycle.

Its design takes the feedback in
[kairos-io/kairos#4099](https://github.com/kairos-io/kairos/issues/4099) as a
starting point, notably:

- **No more bash-driven orchestration** — kubeadm is driven through Go, so
  behavior is unit-testable rather than only verifiable end-to-end.
- **Fail fast, never hang** — failures surface as errors and never block
  subsequent Kairos boot stages.
- **No vendor lock-in** — upstream kubeadm first; FIPS and downstream
  distributions are optional build variants, not core assumptions.
- **Secure by default** — sound credential handling, join-time CA
  verification (mandatory CA pinning, never `unsafeSkipCAVerification`), least
  privilege, and a checksum-verified supply chain.
- **Externally-managed control planes** are a supported topology, not an
  afterthought.

## What works today

Validated end-to-end on KVM/QEMU and libvirt (Kubernetes v1.34.0):

- **Single-node and multi-node clusters.** A `role: init` node runs
  `kubeadm init`; `worker` and `controlplane` nodes join with CA pinning.
- **Unattended install.** Booting a Kairos image built from this repo with one
  of the sample cloud-configs installs to disk and bootstraps the cluster with
  no manual steps.
- **`mint-join` helper.** On a control-plane node,
  `agent-provider-kubernetes mint-join --role worker|controlplane` mints
  bounded-TTL join material, computes the cluster CA SPKI pin, derives the API
  endpoint from `admin.conf`, and prints a ready-to-paste join cloud-config.
- **CNI is the operator's choice.** The provider installs no CNI;
  [`samples/cni-calico/`](./samples/cni-calico/) shows two ways to add Calico
  (apply after the cluster is up, or bundle it in the control-plane
  cloud-config).

The cluster-bootstrap logic, credential handling, reset handler, and image
build are implemented; multi-control-plane HA, broader version-window testing,
and CI are in progress. See the roadmap discussion in the issue above.

## Status

| Area | State |
|------|-------|
| Project bootstrap | in place |
| Architecture / design | foundational decisions made |
| Cluster bootstrap (init / worker / controlplane) | implemented, validated on KVM/libvirt |
| `mint-join` join-material helper | implemented, validated |
| Kairos image build (pinned, checksum-verified) | implemented |
| CI / release automation | not yet |
| Multi-control-plane HA | not yet |
| Field readiness | not ready |

## Building

Requires Go 1.26.3+.

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
# or:
docker build \
  --build-arg KUBERNETES_VERSION=v1.34.0 \
  --build-arg PROVIDER_VERSION=$(git describe --always) \
  -t kairos-kubeadm:dev .
```

Convert the resulting Docker image into a bootable ISO/raw artifact with
[auroraboot](https://kairos.io/docs/reference/auroraboot/), and provision it
with one of the sample cloud-configs in [`samples/`](./samples/).

## Creating a cluster

The [`samples/`](./samples/) directory has cloud-configs for each node role,
and its [README](./samples/README.md) walks through the end-to-end flow:

| File | Role |
|------|------|
| [`samples/master.yaml`](./samples/master.yaml) | first control-plane (`role: init`) |
| [`samples/controlplane.yaml`](./samples/controlplane.yaml) | additional control-plane join |
| [`samples/worker.yaml`](./samples/worker.yaml) | worker join |
| [`samples/cluster.yaml`](./samples/cluster.yaml) | annotated reference covering the full kubeadm v1beta4 surface |
| [`samples/cni-calico/`](./samples/cni-calico/) | Calico CNI, post-hoc or bundled in the cloud-config |

The flow: boot the first control-plane node from a sample; once it is up, mint
join material with `agent-provider-kubernetes mint-join` and drop it into the
worker/controlplane configs; boot the joiners; install a CNI.

## Contributing

This project is moving quickly and its architecture is still being defined.
Please open an issue to discuss substantial changes before submitting a pull
request.

## License

Licensed under the [Apache License 2.0](./LICENSE).
