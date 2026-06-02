# Upgrades

The provider upgrades a running cluster between supported Kubernetes minors by
driving `kubeadm upgrade`. Upgrades are **image-driven and explicitly pinned**:
you boot a newer provider-kubernetes image and bump the version pin; on the next
boot the provider detects the delta and converges the cluster.

> Validated live for 1.34 -> 1.35 on a single control plane. Multi-node/HA upgrade
> ordering is implemented (see below) but exercise it carefully. As always, this
> provider is under active development and not field-ready.

## The model

- The **target version is the bundled `kubeadm` binary** in the image you boot.
  By ADR-3 an operator `kubernetesVersion` pin must equal the bundled minor, so the
  pin and the binary always agree.
- An upgrade runs **only when you pin** `clusterConfiguration.kubernetesVersion` to
  the new minor. A newer image **without** a matching pin bump does **not**
  auto-upgrade - it is a no-op (so a routine image refresh never silently
  re-versions your cluster).
- Only **+1 minor** at a time (kubeadm's rule). Skipping a minor (1.34 -> 1.36),
  downgrades, and out-of-window targets are **refused loudly** before anything
  destructive runs.

## Supported edges

Within the current window: **1.34 -> 1.35** and **1.35 -> 1.36**. To go 1.34 ->
1.36, step through 1.35 (boot the 1.35 image first, let it converge, then the 1.36
image).

## Upgrading a single control plane

1. Make sure the cluster is healthy (`kubectl get nodes`, control-plane pods
   Running). Take an **etcd backup** first - see "etcd snapshots" below.
2. Bump the pin in the node's cloud-config (`/oem` on an installed node) to the new
   minor, e.g. `kubernetesVersion: v1.35.5`.
3. Upgrade the OS image to the matching minor and reboot. Either:
   - `kairos-agent upgrade --source oci:<registry>/provider-kubernetes:<tag>-k8s1.35`
     (then reboot), or
   - boot the new image via your provisioning flow.
4. On reboot the provider's reconcile detects that the bundled binary is a minor
   ahead of the cluster and runs `kubeadm upgrade apply <version>`. When the new
   kubelet cannot start against the old node config (a flag removed across the
   minor), the provider first repairs the kubelet config (see below), brings the
   control plane back, then applies. The kubelet is restarted onto the new version.
5. Verify: `kubectl version` (server), `kubectl get nodes` (node at the new
   version, Ready).

## Upgrading multiple control planes / workers (HA)

Upgrade **one node at a time**, control planes first, then workers:

1. Upgrade the first control plane as above; wait until the cluster version has
   flipped and it is Ready.
2. Upgrade each remaining control plane one at a time. A follower runs
   `kubeadm upgrade node` (the provider detects the cluster already advanced and
   does not re-apply); it waits, bounded, for the control plane to be healthy first.
3. Upgrade workers last, one at a time; each runs `kubeadm upgrade node`.

This sequencing is the operator's responsibility (the same one-at-a-time delivery
contract as HA join); the provider adds bounded health gates but builds no
cross-node lock.

## The kubelet-config repair (why an upgrade "just works" after an image swap)

Kubernetes removes/renames kubelet flags and config keys across minors. Because a
Kairos A/B image swap replaces the kubelet binary *before* any upgrade runs, the
new kubelet can crashloop on a flag the old kubeadm wrote (for example
`--pod-infra-container-image`, removed in 1.35), which would leave the control
plane down. The provider handles this automatically: when an upgrade is due and the
local API is unreachable, it runs `kubeadm init phase kubelet-start` to regenerate
the kubelet config with the new kubeadm (no API, no secrets), which lets the
kubelet start and the existing control plane return before `kubeadm upgrade apply`
runs. You do not need to do anything for this.

## etcd snapshots

`kubeadm upgrade apply` mutates etcd and is largely forward-only. The provider
takes a **best-effort** etcd snapshot before applying, but **only** onto an
encrypted persistent partition (an etcd snapshot is a full plaintext dump of every
cluster secret). If it cannot confirm the partition is encrypted, it **refuses to
write a plaintext snapshot** and logs a warning - so on an unencrypted node you
must take your own snapshot before upgrading. The provider never copies the
snapshot off the node; backup custody is yours.

## Rollback

kubeadm upgrades (especially etcd) are forward-only; there is no automatic
rollback. To recover, restore your etcd snapshot and boot the previous image. A
node wedged mid-upgrade can be recovered with the [reset](./lifecycle.md) flow and
re-joined. The provider never auto-resets a control plane on upgrade failure - it
fails loud and leaves the node for you to inspect.

## What can go wrong

| Symptom | Cause / fix |
|---------|-------------|
| Reconcile logs `refuse-upgrade` | Skip-level, downgrade, or out-of-window pin. Pin only +1 minor within the window. |
| Upgrade doesn't start | No version pin bump (a newer binary alone won't upgrade). Bump `clusterConfiguration.kubernetesVersion`. |
| Pin/binary mismatch hard error | The pin minor must equal the bundled image's minor. |
| Snapshot skipped warning | Persistent partition encryption unconfirmed - take a manual etcd backup. |

See also [Lifecycle and reset](./lifecycle.md) and [Troubleshooting](./troubleshooting.md).
