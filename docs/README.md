# provider-kubernetes documentation

Usage documentation for **provider-kubernetes**, the Go-native
[Kairos](https://kairos.io) cluster provider that creates and manages
**kubeadm-based Kubernetes clusters**. The provider binary is
`agent-provider-kubernetes` (Kairos discovers `agent-provider-*` binaries).

> This project is under active development and is **not** field-ready. Interfaces,
> configuration, and behavior change without notice. Do not deploy it on machines
> you care about.
>
> Not to be confused with `provider-kairos` (the k3s/k0s provider); this is the
> kubeadm provider.

## Contents

| Document | What it covers |
|----------|----------------|
| [Getting started](./getting-started.md) | Prerequisites, getting an image (pull or build), booting the first node, verifying. |
| [Configuration reference](./configuration.md) | The `cluster` cloud-config contract: roles, `cluster_token`, `control_plane_host`, `providerConfig`, the `config:` (kubeadm v1beta4) surface, proxy env, CA anchors. |
| [Creating a cluster](./creating-a-cluster.md) | Single control plane, adding workers, the join-material flow. |
| [High availability](./high-availability.md) | Multi-control-plane (stacked etcd): stable endpoint, one-at-a-time bring-up, failover. |
| [mint-join reference](./mint-join.md) | The `agent-provider-kubernetes mint-join` subcommand. |
| [CNI](./cni.md) | Installing a CNI (the provider installs none). |
| [Security model](./security.md) | `cluster_token`, certificate-key blast radius, CA pinning, at-rest encryption, the trust boundary. |
| [Lifecycle and reset](./lifecycle.md) | Reboot idempotency, reset / `EventClusterReset`, the supported version window, upgrades. |
| [Troubleshooting](./troubleshooting.md) | Where logs are, common failures, and what "fail loud, never hang" means in practice. |

## How it works in one paragraph

You boot a Kairos node from an image that bundles this provider plus the kubeadm
toolchain. Kairos passes your `cluster` cloud-config to the provider, which emits
a single boot-time stage (`network.after`) that runs one bounded reconcile pass:
it reads the node's desired role, probes the node's actual state, and converges
the difference by driving the `kubeadm` binary over typed `os/exec` argv (never a
shell). A `role: init` node runs `kubeadm init`; `worker` / `controlplane` nodes
run `kubeadm join` against operator-delivered join material with mandatory CA
pinning. Every external action is bounded by a deadline with capped retries, so a
failure surfaces loudly and never blocks later Kairos boot stages.

## See also

- [`samples/`](../samples/) - ready-to-edit cloud-configs and an end-to-end walkthrough.
- [`samples/ha/`](../samples/ha/) - the multi-control-plane walkthrough.
- [`samples/cni-calico/`](../samples/cni-calico/) - Calico CNI examples.
- Root [`README.md`](../README.md) - project overview and status.
