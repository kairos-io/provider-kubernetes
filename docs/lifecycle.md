# Lifecycle and reset

## Reconcile on every boot

The provider runs one bounded **reconcile** pass on each boot (from the
`network.after` yip stage). It is desired-vs-actual, not sentinel files:

1. Probe the node's authoritative state (membership from on-disk kubeadm
   artifacts under `cluster_root_path`, kubelet health, control-plane
   reachability, and on a control plane whether `kubeadm init` finished and
   whether its own apiserver answers).
2. Compute the actions needed to converge the declared `role` with that state.
3. Execute them under a deadline with capped retries.

If the node is already a converged, healthy member, the plan is empty and
reconcile is a fast no-op. The provider does not re-bootstrap or re-join an
existing member, healthy or not: the action stays a no-op either way, but the
status the node reports differs. A healthy member reports `phase: Converged`.
One whose kubelet is down, whose `kubeadm init` never finished, or whose
control plane is not serving reports `phase: Degraded`, so the outage is never
silently hidden as a success. On a control plane whose apiserver is not
answering yet, for example because the static pods are still starting after a
reboot, the pass waits up to 3 minutes for it before reporting. See
[Node status](./status.md).

## Bounded, fail-loud, never hang

Every external action runs under a context deadline with bounded retries and a
hard total ceiling. On exhaustion the provider records a loud failure and
returns - it never loops forever and never blocks later Kairos boot stages. A
terminal decision (for example, refusing to `init` because the cluster already
exists) fails fast without burning the retry budget.

## Reset (`EventClusterReset`)

Kairos's cluster-reset event, and the `reset` subcommand for a reset by hand,

```sh
sudo /system/providers/agent-provider-kubernetes reset --cluster-file=/run/provider-kubernetes/cluster.json
```

both:

- run a bounded `kubeadm reset`,
- empty the authoritative artifact directories under `cluster_root_path`
  (`etc/kubernetes` including the PKI, `var/lib/kubelet`, `var/lib/etcd`) and keep
  the directories themselves, which on Kairos are mount points,
- sweep `/run` for any leftover transient credential files,
- record `phase: Reset` in the node status, with `reason: ResetOK` or `ResetFailed`,

so the next boot re-converges from a clean state. `cluster_root_path` is validated
(absolute, no traversal), and a symlinked artifact is removed, never followed.

`kubeadm reset` runs first and unmounts the kubelet's volumes itself, within the
reset's 5 minute bound. The provider's own cleanup then never enters or removes
anything below those directories that is still a mount point: when a volume could
not be unmounted, for example because a container still uses it, the reset leaves
it in place with its data, empties everything around it, and fails with
`reason: ResetFailed`. The status says how many mounts were kept and the reset log
names them; unmount them and run the reset again (see
[Troubleshooting](./troubleshooting.md#a-reset-failed-mount-points-left-in-place)). In that case `kubeadm reset`
stopped before removing the containers: the kubelet is stopped, but some
control-plane containers, such as the apiserver, can keep running until the
second reset removes them. The cleanup does not read a
mounted filesystem below those directories, with one exception on kernels older
than 6.18: an automount point that has not been mounted yet may still be triggered
when it is checked.

On a stacked-etcd control plane, reset also handles etcd membership - see the
removal runbook in [High availability](./high-availability.md#removing-a-control-plane).

## Supported version window

The provider targets the latest three in-support upstream Kubernetes minors,
rolling forward (currently **1.35 / 1.36 / 1.37**; releases up to v0.3.0 shipped
1.34 / 1.35 / 1.36). It uses the **v1beta4** kubeadm config API only (v1beta3 is
EOL and deliberately unsupported).

- The target minor can be pinned via `clusterConfiguration.kubernetesVersion`.
- A pin that does not match the `kubeadm` binary bundled in the image is a **hard
  error** (fail fast), as is any version outside the supported window. There is no
  best-effort "close enough" behavior.

Each released image bundles one Kubernetes minor; pick the matching image tag (see
[Getting started](./getting-started.md)).

## Upgrades

Cluster **upgrades** (`kubeadm upgrade`) are **implemented**: pin a newer
`kubernetesVersion`, boot the matching image, and the provider converges the
cluster on the next reconcile (control plane via `kubeadm upgrade apply`, followers
and workers via `kubeadm upgrade node`, one minor at a time). A newer image without
a pin bump does not auto-upgrade, and downgrade / skip-level / out-of-window
targets are refused. See [Upgrades](./upgrades.md) for the full operator runbook
(single-node and HA), the automatic kubelet-config repair, etcd backups, and
rollback.
