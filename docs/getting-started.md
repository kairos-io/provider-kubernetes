# Getting started

This walks you from nothing to a running single-node Kubernetes control plane.

## Prerequisites

- A machine or VM that can boot a Kairos image (UEFI; bare metal, KVM/QEMU,
  libvirt, or a cloud instance).
- A way to deliver a cloud-config to the node (ISO + embedded config, PXE, the
  Kairos `config_url`, or your provisioning pipeline).
- For multi-node or HA: network reachability between nodes and a stable
  control-plane endpoint (see [High availability](./high-availability.md)).

The provider targets a rolling window of upstream Kubernetes minors (currently
**1.34 / 1.35 / 1.36**). Pick one; the bundled `kubeadm` binary must match
(a mismatch is a hard error - see [Lifecycle](./lifecycle.md)).

## 1. Get an image

A provider-kubernetes image is a Kairos OS image with the provider installed at
`/system/providers/agent-provider-kubernetes` plus the kubeadm toolchain
(kubeadm, kubelet, kubectl, containerd, runc, CNI plugins), each pinned and
checksum-verified.

### Option A - pull a released image (no build)

Tagged releases publish one image per supported Kubernetes minor:

```sh
# choose the Kubernetes minor (1.34 / 1.35 / 1.36):
docker pull ghcr.io/kairos-io/provider-kubernetes:v0.1.0-k8s1.34
# the newest supported minor is also tagged plainly and as :latest
docker pull ghcr.io/kairos-io/provider-kubernetes:latest
```

### Option B - build it yourself

```sh
make image KUBERNETES_VERSION=v1.34.0 VERSION=dev
# equivalently:
docker build \
  --build-arg KUBERNETES_VERSION=v1.34.0 \
  --build-arg PROVIDER_VERSION="$(git describe --always)" \
  -t kairos-kubeadm:dev .
```

## 2. Turn it into a bootable artifact

Convert the OCI image into a bootable ISO/raw image with
[auroraboot](https://kairos.io/docs/reference/auroraboot/), embedding your
cloud-config:

```sh
docker run --rm --privileged \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD:/work" \
  quay.io/kairos/auroraboot:latest \
  build-iso --output /work --cloud-config /work/master.yaml \
  docker:kairos-kubeadm:dev
```

## 3. Write the first node's cloud-config

Start from [`samples/master.yaml`](../samples/master.yaml). The minimum you must
set: a high-entropy `cluster_token`, the node's `control_plane_host`, and a
`kubernetesVersion` within the supported window.

```yaml
#cloud-config
install:
  device: "auto"
  auto: true
  reboot: true
users:
  - name: kairos
    groups: ["admin", "sudo"]   # Kairos requires an admin user for auto-install
    passwd: kairos
cluster:
  cluster_token: "a-long-high-entropy-correlation-string-change-me"
  control_plane_host: "192.168.1.10"
  role: init
  providerConfig:
    cluster_root_path: "/"
  config: |
    clusterConfiguration:
      kubernetesVersion: v1.34.0
      controlPlaneEndpoint: "192.168.1.10:6443"
      networking:
        podSubnet: 10.244.0.0/16
        serviceSubnet: 10.96.0.0/12
```

See the [configuration reference](./configuration.md) for every field.

## 4. Boot and verify

Boot the node from the artifact. On first boot Kairos installs to disk; on the
next boot the provider's `network.after` stage runs `kubeadm init`. After about a
minute:

```sh
# on the node:
sudo kubectl --kubeconfig /etc/kubernetes/admin.conf get nodes
sudo kubectl --kubeconfig /etc/kubernetes/admin.conf get pods -n kube-system
```

The node will be `NotReady` until you install a CNI - that is expected, and it is
your choice. See [CNI](./cni.md).

If something goes wrong, the provider writes a reconcile log and fails loudly
rather than hanging - see [Troubleshooting](./troubleshooting.md).

## Next steps

- [Add workers / more control planes](./creating-a-cluster.md)
- [Stand up a multi-control-plane HA cluster](./high-availability.md)
- [Install a CNI](./cni.md)
