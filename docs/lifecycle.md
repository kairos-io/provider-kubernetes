# Lifecycle and reset

## Reconcile on every boot

The provider runs one bounded **reconcile** pass on each boot (from the
`network.after` yip stage). It is desired-vs-actual, not sentinel files:

1. Probe the node's authoritative state (membership from on-disk kubeadm
   artifacts under `cluster_root_path`, kubelet health, control-plane
   reachability).
2. Compute the actions needed to converge the declared `role` with that state.
3. Execute them under a deadline with capped retries.

If the node is already a converged, healthy member, the plan is empty and
reconcile is a fast no-op. The provider does not re-bootstrap or re-join an
already-converged node.

## Bounded, fail-loud, never hang

Every external action runs under a context deadline with bounded retries and a
hard total ceiling. On exhaustion the provider records a loud failure and
returns - it never loops forever and never blocks later Kairos boot stages. A
terminal decision (for example, refusing to `init` because the cluster already
exists) fails fast without burning the retry budget.

## Reset (`EventClusterReset`)

Kairos's cluster-reset event drives the provider's reset path, which:

- runs a bounded `kubeadm reset`,
- removes the authoritative artifacts under `cluster_root_path`
  (`etc/kubernetes` including the PKI, `var/lib/kubelet`, `var/lib/etcd`),
- sweeps `/run` for any leftover transient credential files,

so the next boot re-converges from a clean state. `cluster_root_path` is validated
(absolute, no traversal) and symlinked artifacts are refused rather than followed,
so reset cannot be tricked into wiping a bind-mount target.

On a stacked-etcd control plane, reset also handles etcd membership - see the
removal runbook in [High availability](./high-availability.md#removing-a-control-plane).

## Supported version window

The provider targets the latest three in-support upstream Kubernetes minors,
rolling forward (currently **1.34 / 1.35 / 1.36**). It uses the **v1beta4** kubeadm
config API only (v1beta3 is EOL and deliberately unsupported).

- The target minor can be pinned via `clusterConfiguration.kubernetesVersion`.
- A pin that does not match the `kubeadm` binary bundled in the image is a **hard
  error** (fail fast), as is any version outside the supported window. There is no
  best-effort "close enough" behavior.

Each released image bundles one Kubernetes minor; pick the matching image tag (see
[Getting started](./getting-started.md)).

## Upgrades

Cluster **upgrades** (`kubeadm upgrade`) are **not yet implemented** - this is the
next planned capability. For now the lifecycle covers bootstrap, join, and reset.
Track the roadmap in the project's issues.
