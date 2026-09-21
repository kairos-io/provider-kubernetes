# Changelog

All notable changes to provider-kubernetes are recorded here. Dates are the tag
dates. Each release ships one Kairos image per supported Kubernetes minor, plus
the provider binary, with keyless SLSA provenance and a CycloneDX SBOM.

## v0.4.0 - 2026-09-21

A security and correctness release. Two of the fixes are the reason to upgrade
rather than stay: an air-gapped node could not bootstrap at all on v0.3.0, and a
trusted-boot (UKI) node could not bootstrap on its first boot on any earlier
version.

### Breaking

- The supported Kubernetes window is now 1.35, 1.36 and 1.37. Kubernetes 1.34 is
  no longer built or tested. Move to a supported minor before upgrading the
  provider.
- `import-images` no longer accepts `--dir`. It reads the bundle only from the
  image, and takes no arguments other than `--verify-only`.
- The systemd units the image installs now live in `/usr/lib/systemd/system` and
  have no `[Install]` section. `systemctl is-enabled` reports `static`, and
  `systemctl disable` no longer persists against them. Use `systemctl mask` to
  turn one off, which now works where it previously could not.
- A node whose kubelet is not healthy reports the new phase `Degraded` instead of
  `Converged`. Anything parsing `status.yaml` or the node annotations must treat
  `Degraded` as a value it can see.
- containerd's `opt` plugin and both erofs plugins are disabled, and its optional
  gzip helpers are switched off. A derived image that wants erofs or a faster
  decompressor must re-enable them.

### Fixed

- Air-gapped bootstrap. v0.3.0 imported every bundled control-plane image under a
  placeholder name, so kubeadm could not find them: a node without registry
  access failed to bootstrap, and a connected node silently pulled the images
  instead. Images now import under their exact references. There is no v0.3.x
  backport; move to this release. Leftover `:i-was-a-digest` names on an existing
  node are harmless and can be removed with `ctr -n k8s.io images rm`.
- First boot on trusted-boot (UKI) nodes. The cluster configuration was read
  before the directory holding it existed, so nothing was written, the kubelet
  crash-looped, and the failure was silent in every channel. It affected every
  UKI install, and a state reset reproduced it. Validated on a secure-boot VM
  with an emulated TPM: a fresh install and a reset now converge unaided.
- The bundled image import on trusted-boot nodes, where the read-only root
  filesystem is world-writable by mode and was refused outright.
- The local apiserver probes polled a hardcoded port, so a cluster that pinned
  `localAPIEndpoint.bindPort` elsewhere had its kubelet config repaired on every
  pass and its upgrade never ran. Both probes now follow the configured port.
- The pre-upgrade etcd snapshot, which could not run because the image shipped no
  `etcdctl`. `etcdctl` and `etcdutl` now ship, taken from the etcd image kubeadm
  pins for the release and verified with cosign.
- `crictl` shipped owned by a non-root user, inherited from its release tarball.
- Configuration files and directories in the image took their mode from the build
  host's umask, so a local build could ship group-writable units and a 0644
  directory that only root could traverse.

### Security

- Host tools run by absolute path with a closed environment, so a binary planted
  in a writable directory on `PATH` cannot be run in place of `kubeadm`,
  `kubectl`, `ctr`, `systemctl` or `etcdctl`.
- containerd and the kubelet, and everything they start, resolve helpers inside
  the read-only image. containerd's three directories that default under the
  persistent `/opt` and whose contents it executes as root are closed.
- Bundled images are imported only from the read-only image, listed in a lock
  file, with each tarball checked for ownership, mode, file type, filesystem and
  archive structure before import, and passed to containerd by descriptor rather
  than by path.
- The systemd units are owned by the image rather than by the persistent
  partition, so what a node runs is decided by the image it booted. A migration
  removes superseded copies from `/etc`, but only when their bytes match
  something this project shipped; anything edited is kept, and the migration unit
  fails so the state is visible.
- On a trusted-boot node the provider now creates the cluster-config directory
  itself, and withholds the cluster token when the target looks unsafe: the write
  that Kairos performs regardless then carries no token and no commands.

None of the above is a boundary against an actor who already has root on the
node. Kairos keeps every persistent path under `/usr/local`, so whoever can write
there can write the units too. What these changes remove is name-based and
accidental substitution, and state that outlives the image it came from.

### Added

- `import-images --verify-only` runs every check without importing.
- `migrate-units` removes superseded unit copies, and runs from its own unit
  before containerd, the kubelet and the image import.
- OSV-Scanner in CI, and Renovate for dependency updates.
- Go 1.27.1, with runtime CVE fixes in the image stack.

### Known issues

- A non-regular file placed in the cloud-config directory, such as a FIFO, hangs
  the Kairos agent's own configuration scan before this provider runs, leaving a
  node that cannot be reached or rebooted. It is not specific to this provider
  and cannot be prevented from here. Recovery is to remove the file from recovery
  media.
- A symlink planted at the cluster-config directory causes a Kairos stage to
  change the mode of whatever it points at, which can break shell and SSH access
  for non-root users. The provider refuses to follow it and withholds the token,
  which limits disclosure but does not prevent that.
- On trusted-boot nodes no log channel survives from before the pivot. The status
  document, and its `/var/log` mirror, are the only record of an early refusal.
- Only amd64 images are published. The Dockerfile handles arm64, but the release
  workflow builds amd64 only, so there is no arm64 image for this release.

## v0.3.0 - 2026-07-01

- Pre-bundled control-plane images for an air-gapped first boot, cosign-verified
  and digest-pinned at build time, attested as a custom predicate.
- Images built on the Kairos Hadron (musl) base.
- kubelet `clusterDNS` derived correctly for a custom service subnet.
- The containerd pause image version-matched to kubeadm.
- Base images digest-pinned and workflow actions SHA-pinned.
- A nightly e2e workflow for the heavier scenarios, and the in-container kubeadm
  upgrade scenario retired in favour of the VM boundary.

See the note above: air-gapped bootstrap is broken in this release.

## v0.2.2 - 2026-06-09

- CycloneDX SBOM emission so the attestation fits the size cap.
- Released images and the binary signed and attested with provenance and SBOM.

## v0.2.0 - 2026-06-02

- Reconcile status surfaced on the node.
- A tiered e2e suite running kubeadm in containers, with a per-PR CI job.
- Externally-managed control-plane join proven end to end.

## v0.1.0 - 2026-06-02

First tagged release: the Kairos provider contract, kubeadm configuration
generation, and cluster bring-up for init, control-plane and worker roles.
