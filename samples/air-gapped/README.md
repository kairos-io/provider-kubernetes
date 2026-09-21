# Air-gapped install

A node with no registry access can still run `kubeadm init` or `kubeadm join`,
because every provider-kubernetes image carries the control-plane container
images for its own Kubernetes minor and imports them into containerd at boot.

[`cluster-airgapped.yaml`](./cluster-airgapped.yaml) is a `role: init`
cloud-config for that case. It is the ordinary init config; what matters is the
two things it does not do (set a custom `imageRepository`, pin a
`kubernetesVersion` the image does not ship) and the verification below.

## What the image carries, and what it does not

Bundled, verified at build time, imported at every boot:

- `kube-apiserver`, `kube-controller-manager`, `kube-scheduler`, `kube-proxy`,
  `etcd`, `coredns`, `pause`, at the exact references the bundled kubeadm asks
  for. The build's signature floor is `pause`, `etcd` and `coredns` plus at
  least one `kube-*` component: each of those must pass cosign verification
  against the Kubernetes release signing identity, or the build fails.
  `images.lock` records, per image, whether it verified.
- The bundle lives in `/system/provider-kubernetes/images`, which is read-only
  OS-image content, with an `images.lock` recording each reference, digest and
  tarball name.

Not bundled, and your problem on an air-gapped network:

- **A CNI.** The provider installs none. Mirror the manifest and its images
  internally and apply from your local copy. See [`../cni-flannel/`](../cni-flannel/)
  for the manifest-based path.
- **Anything your workloads pull.** Point them at an internal mirror.
- **Images for a custom `imageRepository`.** The bundle covers
  `registry.k8s.io` only.

## Verify before you cut the network

Boot one node from the image with this config while a registry is still
reachable, and confirm the import is doing the work rather than a silent pull.

```sh
# The import runs before reconcile and logs one summary line.
sudo grep 'image-import:' /var/log/provider-kubernetes-image-import.log | tail -n 5
```

```text
image-import: summary outcome=success entries=7 imported=7 refused=0 failed=0 unlisted=0 readonly=true dir=/system/provider-kubernetes/images
```

`outcome=success` with `refused=0 failed=0` is what you want. You can run every
check without importing anything:

```sh
sudo /system/providers/agent-provider-kubernetes import-images --verify-only
```

Then confirm containerd holds each image under the reference kubeadm looks up,
which is the lookup that decides whether kubeadm pulls:

```sh
sudo /usr/bin/kubeadm config images list --kubernetes-version v1.37.0
sudo /usr/bin/crictl --runtime-endpoint unix:///run/containerd/containerd.sock \
  --image-endpoint unix:///run/containerd/containerd.sock images
```

Every reference in the first command's output must appear in the second.
[Troubleshooting](../../docs/troubleshooting.md) covers the refusal reasons and
how to tell a bundled image apart from a pulled one.

## Upgrading an air-gapped cluster

An upgrade is an image swap plus a pin bump, so the new minor's images come in
with the new OS image. Do not remove `/opt/provider-kubernetes` on a node that
still runs a v0.3.0 import unit: that unit skips the import when the directory
is gone, and an air-gapped node then fails to converge. The check and the
sequence are in [Upgrades](../../docs/upgrades.md#rollback).
