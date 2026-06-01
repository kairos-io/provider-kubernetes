# provider-kubernetes

A [Kairos](https://kairos.io) cluster provider that bootstraps **upstream
Kubernetes** using `kubeadm`, while remaining native to the Kairos ecosystem
(the `clusterplugin` / `yip` contract).

> ## Under active development — NOT ready for field testing
>
> This project is in **early, pre-alpha development**. It is **not** ready for
> field testing, staging, or production use. Interfaces, configuration, and
> behavior **will** change without notice, and the provider currently does not
> bootstrap a cluster. Do not deploy it on machines you care about.

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
  verification, least privilege, and a verified supply chain.
- **Externally-managed control planes** are a supported topology, not an
  afterthought.

## Status

| Area | State |
|------|-------|
| Project bootstrap | in place |
| Architecture / design | in progress |
| Cluster bootstrap logic | not implemented |
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
with one of the sample cloud-configs in [`samples/`](./samples/) (`master.yaml`,
`controlplane.yaml`, `worker.yaml`). The samples directory's
[README](./samples/README.md) walks through the end-to-end cluster-creation
flow.

## Contributing

This project is moving quickly and its architecture is still being defined.
Please open an issue to discuss substantial changes before submitting a pull
request.

## License

Licensed under the [Apache License 2.0](./LICENSE).
