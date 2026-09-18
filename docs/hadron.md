# The Hadron base

provider-kubernetes images are built on **[Hadron](https://github.com/kairos-io/hadron)**,
the Kairos maintainers' minimal, from-scratch, **musl-based** immutable OS. This
page explains what that means for building and operating the provider; for the
build command see [Building a Kairos image](../README.md#building-a-kairos-image).

> The provider images are built on Hadron; a single-node control plane converges
> end to end on the Hadron base. As with the rest of the project, this is an early
> public release and is not yet certified for production.

## How the image is built

The build mirrors the **canonical Kairos image flow**: the base is the pure
upstream Hadron OS (`ghcr.io/kairos-io/hadron`), and `kairos-init` transforms it
into a bootable Kairos system in two phases (`install`, then `init`) as the very
last step - exactly as Kairos itself builds Hadron. We pin `kairos-init` to the
version Kairos uses for Hadron (older versions cannot regenerate Hadron's
initramfs).

Because Hadron is **musl** (not glibc), two of the bundled binaries get special
handling:

- **containerd** and **kubelet** ship from upstream as **glibc-linked** binaries
  that cannot exec on musl, so we build them **fully static from source**
  (containerd `make STATIC=1`, kubelet `CGO_ENABLED=0`). A fully static binary
  runs on musl.
- **kubeadm, kubectl, crictl, runc** are already static and run on musl unchanged,
  so they stay the checksum-verified official downloads.
- **etcdctl, etcdutl** are the static upstream binaries extracted from the
  signature-verified etcd control-plane image bundled for the minor (the build
  fails if they do not run on musl or do not match that etcd version).

This is all automatic - there are no build flags to set. `make image` produces a
Hadron image.

The provider executes `kubeadm`, `kubectl`, `ctr`, `systemctl` and `etcdctl` only
by their `/usr/bin` paths (see [Security model](./security.md#exec-hygiene)), so an
image derived from this one must keep them there.

The systemd units the provider owns - `containerd.service`, `kubelet.service`,
`provider-kubernetes-image-import.service`, `provider-kubernetes-unit-migrate.service`
and `kubelet.service.d/10-kubeadm.conf` - are installed in
`/usr/lib/systemd/system`, with no `[Install]` section and a relative
`../<unit>` symlink in `/usr/lib/systemd/system/multi-user.target.wants/`. A derived
image must keep them there and must **not** `systemctl enable` them: that needs an
`[Install]` section and writes absolute links into `/etc/systemd/system`, which is
persistent on Kairos.

Which matters because these directories are **persistent** on a Kairos node, all
outrank `/usr/lib/systemd/system` in systemd's unit search path, and none of them is
refreshed from the image the way `/usr` is:

| Path | What it holds | Kairos behavior |
|------|---------------|-----------------|
| `/etc/systemd/system`, `.control`, `.attached` | unit fragments, `<unit>.d/` drop-ins, `.wants` links | bind mount from the persistent partition, refreshed from the image with `rsync --update` (update-only, never deletes) |
| `/usr/local/lib/systemd/system` | the same | on `COS_PERSISTENT`; never refreshed from the image at all |
| `/var/lib/kubelet/kubeadm-flags.env`, `/etc/default/kubelet` | `EnvironmentFile=` content, which outranks `Environment=` | the first is persistent; the second is rebuilt from the image each boot |
| `/var/lib/extensions`, `/var/lib/confexts` | sysext/confext images that can overlay `/usr/lib` or `/etc` | persistent |

So anything an operator puts in one of them keeps overriding the image's units
across every upgrade and rollback. Put local changes in a drop-in under
`/etc/systemd/system/<unit>.d/` (supported, never touched by the provider) rather
than editing a full unit, and see
[Upgrades](./upgrades.md#unit-files-moved-into-the-image) for the one-time cleanup of
the copies earlier releases left there.

The pre-bundled control-plane images live in `/system/provider-kubernetes/images`
(with `images.lock`), beside the provider binary in `/system/providers`. `/system`
is part of the read-only OS image and is not a persistent or overlaid path on
Kairos. A derived image must keep the bundle there, owned by root and not writable
by group or others, and must not bind-mount anything over it: the import refuses
tarballs that are not on the same filesystem as the provider binary.

## Supply-chain pinning

The static builds clone the upstream **version tag** and then verify it resolves
to a **pinned commit SHA** (`KUBERNETES_COMMIT` / `CONTAINERD_COMMIT`), because a
git tag is mutable while a commit is content-addressed - giving the from-source
path the same integrity guarantee as the checksum-verified download path. The
defaults match the default `KUBERNETES_VERSION` (v1.37.0) and `CONTAINERD_VERSION`.
Building a **different** Kubernetes minor requires the matching commit (and the
matching `CRICTL_VERSION`), or the build fails loud:

```sh
make image \
  KUBERNETES_VERSION=v1.35.8 \
  KUBERNETES_COMMIT=$(git ls-remote https://github.com/kubernetes/kubernetes refs/tags/v1.35.8^{} | cut -f1) \
  CRICTL_VERSION=v1.35.0
```

## Verify

A Hadron node is a normal provider-kubernetes node: on boot the reconcile runs and
writes `/run/provider-kubernetes/status.yaml` (`phase: Converged` on success) and
the `provider-kubernetes.kairos.io/*` Node annotations - see
[Node status](./status.md). A converged control plane shows the node at the bundled
Kubernetes version with `containerd://2.3.5`, OS-IMAGE `Hadron Linux`, and a
`...-hadron` kernel. The node is `NotReady` until you install a CNI ([CNI](./cni.md)).

## Caveats

- **Static-binary capability loss:** a fully static binary cannot `dlopen`, so
  containerd/kubelet do **not** support PKCS#11 / HSM-backed image-signing keys or
  externally `dlopen`-ed `.so` plugins, and glibc **NSS** modules (LDAP/SSSD-style
  lookup) are bypassed in favour of Go's built-in resolver
  (`/etc/resolv.conf` + `/etc/hosts`). **Unaffected:** seccomp, AppArmor, cgroups,
  and per-container enforcement (runc is the official static binary). SELinux-
  enforcing mode is not covered on the static kubelet - confirm it before relying
  on it.
- **Build-time module fetch:** the static **containerd** build resolves Go modules
  at build time (checksum-enforced via the pinned `go.sum`); the static **kubelet**
  build is fully vendored, so it pulls nothing beyond the pinned clone.
- **Per-minor pinning:** building a non-default Kubernetes minor from source
  requires the matching `KUBERNETES_COMMIT` (above).
- **arch:** amd64 (arm64 not yet supported).

See also [Getting started](./getting-started.md),
[Configuration reference](./configuration.md), and [Testing](./testing.md).
