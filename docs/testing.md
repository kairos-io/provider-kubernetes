# Testing

How provider-kubernetes is tested, what each layer proves, how to run the suites
locally, and -- just as important -- the explicit boundary of what is **not**
covered automatically and must be validated by hand.

## Philosophy

Two design principles drive the test strategy:

- **Hardware-free testability (principle 6).** Config generation is pure functions
  over typed inputs; the reconcile is a pure `Plan` plus a bounded executor. We
  test the *behavior* (the produced config/objects, the planned actions, the
  on-disk status), never generated strings.
- **Fail fast, never hang (principle 4 / #4099-1).** This applies to the tests too:
  every wait is bounded, and the e2e harness always tears its containers down.

The result is a layered pyramid: fast pure tests on every change, real-kubeadm
end-to-end tests on every PR, heavier multi-node scenarios nightly, and a set of
full-VM scenarios that require real hardware and fall outside per-PR CI.

## The layers at a glance

| Layer | Command | Runs | What it proves | Hardware |
|-------|---------|------|----------------|----------|
| Unit / behavior | `make test` | every PR (gates job) | config generation, pure `Plan`, version skew, prober parsing, status `BuildStatus`, credential/PKI math | none |
| End-to-end (e2e) | `make e2e` | every PR (e2e matrix, 1.34/1.35/1.36) | real `kubeadm`/`kubelet`/`containerd` accept our config and converge: init, join, reset, the refuse-guards, status, externally-managed join | Docker + privileged container |
| Nightly e2e | `make e2e-nightly` | nightly + on demand | heavier multi-container scenarios: multi-control-plane stacked-etcd HA and the pre-membership failure-status path | Docker + privileged container |
| Full-VM scenarios | KVM/libvirt | boundary (see below) | full Kairos ISO boot, A/B reboot upgrade, HA failover, kube-vip, TPM2/kcrypt | KVM/libvirt host |

## Unit and behavior tests

```sh
make test     # go test ./... with coverage
make vet
make lint     # golangci-lint (v2 config)
make fmt-check
```

These cover the bulk of the logic and run in seconds. The e2e files are behind a
`//go:build e2e` tag, so `make test` / `go test ./...` never compile or run them --
the fast gate stays fast.

## End-to-end tests

The e2e suite (`test/e2e/`, behind `//go:build e2e`) proves the layer unit tests
cannot: that a real `kubeadm` accepts our generated v1beta4 config and that a real
node actually converges, joins, and resets.

### Mechanism

Each scenario runs the provider against a **real** kubeadm/kubelet/containerd
inside a kind-style **privileged systemd "node container"**:

- The node image (`Dockerfile.e2e-node`) is `FROM`-derived from a built
  `kairos-kubeadm` image, so it reuses the exact checksum-verified toolchain we
  ship -- nothing is re-downloaded. It only adds the kind-blessed tweaks needed to
  run systemd as PID 1 with the systemd cgroup driver on cgroup v2 (masking
  container-hostile units, `--fail-swap-on=false`, stateful-dir volumes).
- The harness (`test/e2e/nodecontainer.go`) starts the container `--privileged
  --cgroupns=private` with tmpfs `/run` + `/tmp`, anonymous volumes for
  `/var/lib/{containerd,kubelet,etcd}`, and read-only `/lib/modules` + `/boot`
  (the latter lets kubeadm's `SystemVerification` preflight read the kernel config
  **without** weakening the provider's preflight).
- It drives the **real** entrypoints, exactly as production does: it serializes a
  `clusterplugin.Cluster` to the same `0600` tmpfs path the yip stage uses and runs
  `agent-provider-kubernetes reconcile --cluster-file=...` (and `reset`,
  `mint-join`) via `docker exec`. There is **no parallel code path** -- the test
  exercises the shipped binary and the real on-disk contract.
- Assertions read back the typed `status.yaml`, the Node annotations, and
  `kubectl get nodes` -- behavior, not strings. Teardown (`docker rm -fv`) is
  guaranteed via `t.Cleanup`, reaping the anonymous volumes.

No VM and no `/dev/kvm` are required -- only Docker with privileged containers.

### Scenarios (Tier 1, per-PR)

| Test | Sets up | Proves |
|------|---------|--------|
| `TestSingleNodeInitConverges` | one node, `role: init` | `kubeadm init` converges: apiserver healthy, node registered, `status.yaml` `phase=Converged` (mode `0640`), all seven `provider-kubernetes.kairos.io/*` annotations present |
| `TestWorkerJoin` | CP + worker (2 containers) | a real CA-pinned worker join: `mint-join` mints material on the CP, the worker joins with the SPKI pin; both nodes register; `UnsafeSkipCAVerification` never set |
| `TestExternallyManagedControlPlaneJoin` | external CP via plain `kubeadm init` + worker | joining a CP the provider did **not** bootstrap (#4099-5): worker joins with only the operator's CA **PEM** (`ca_certs`); the provider derives the pin itself |
| `TestResetCleansArtifacts` | init, then `reset` | the reset path removes the authoritative artifacts (PKI, `admin.conf`, etcd member data, kubelet join marker) |
| `TestInitClobberRefusal` | live CP + a second `role: init` node | the init-clobber guard (#4099-5) fires: `refuse-init`, terminal, `status.reason=InitRefused`, the existing cluster untouched |
| `TestUpgradeSkewRefusal` | converged node, skip-level pin | the upgrade skew guard fires: `refuse-upgrade`, terminal, `status.reason=UpgradeRefused`, nothing destructive runs |

### Running locally

Prerequisites:

- Docker with the ability to run `--privileged` containers (cgroup v2 host).
- `/boot/config-$(uname -r)` and `/lib/modules` present (standard on Ubuntu) for
  the kubeadm preflight.
- **No** KVM / nested virtualization.

The e2e node image is `FROM`-derived from a `kairos-kubeadm` base image, so build
that base once for the version you want, then run the suite:

```sh
# 1. Build the base image (a few minutes; bundles the toolchain).
make image KUBERNETES_VERSION=v1.34.0 VERSION=v1.34.0 IMAGE=kairos-kubeadm:v1.34.0

# 2. Build the node image and run the suite (make e2e builds the node image for you).
make e2e KUBERNETES_VERSION=v1.34.0
```

Notes:

- `make e2e` depends on `e2e-node-image`; if the base image is missing it prints
  the exact `make image` command to run first.
- Each scenario creates its own container(s) and cleans them up. To run one:
  `E2E_NODE_IMAGE=kairos-kubeadm-e2e-node:v1.34.0 E2E_KUBERNETES_VERSION=v1.34.0
  go test -tags e2e -count=1 -run TestWorkerJoin -v ./test/e2e/...`.
- Each scenario pre-pulls the control-plane images before reconcile so a cold
  containerd pull does not race the reconcile budget (a runtime nuance, not a
  production change).
- A converged node shows `NotReady` because the provider installs no CNI by
  design; the scenarios assert on convergence + registration, not `Ready`.
- Watch disk: the base + node images are ~3 GB each, and anonymous volumes
  accumulate if a run is killed mid-flight. `docker volume prune` and
  `docker image prune` clean up.

### In CI

The `e2e` job in `.github/workflows/ci.yml` runs on every PR, as a matrix over the
full supported window (1.34 / 1.35 / 1.36) in parallel. For each minor it resolves
the latest patch, builds the base + node image, and runs the whole suite. A
`timeout-minutes` backstop guarantees it never hangs.

Security posture (this job runs a `--privileged` container on
attacker-influenceable PR code, so the controls matter):

- **GitHub-hosted ephemeral runners only** -- never self-hosted.
- **`pull_request` trigger** (not `pull_request_target`), so fork PRs get no
  secrets.
- **Explicit read-only token** (`permissions: contents: read`); no secrets, no
  `packages: write`.

These conditions -- not the container flags -- are what contain a hostile PR.

## Nightly tier

A heavier `nightly` workflow (`make e2e-nightly`, cron + on demand) runs the
multi-container scenarios that are too slow for per-PR: multi-control-plane
stacked-etcd HA bring-up and the pre-membership failure-status path under a real
unreachable endpoint. The kubeadm-layer in-place upgrade is exercised on the
full-VM boundary rather than in a container (its preflight requires a functional
pod-scheduling cluster; see the boundary below).

## Coverage boundary -- what e2e does NOT cover

The container-based e2e cannot reproduce events that need a real machine, a reboot,
or hardware. These require a KVM/libvirt host and are an explicit, documented
boundary -- not a gap to be silently closed by per-PR CI:

- **Full Kairos image boot.** The e2e runs the provider binary in a node
  container; it does not boot a Kairos ISO, install to disk, or exercise the
  `network.after` yip stage end to end.
- **A/B image upgrade across a real reboot.** The kubeadm-layer upgrade is
  testable in a container; the Kairos A/B swap + reboot (and the kubelet-config
  repair that the swap-then-reboot ordering triggers) is not.
- **HA failover.** Powering off a control plane and proving the cluster survives on
  remaining quorum needs real VMs.
- **kube-vip / cross-host networking.** A real VIP and a multi-host LAN.
- **TPM2 / kcrypt at-rest encryption.** Real hardware/firmware.

These scenarios are exercised on a KVM/libvirt host with ISOs built from this
repo's image. When you change anything in these areas, run the relevant scenario
before release.

## Release artifact provenance

Testing proves the software behaves; provenance proves the artifact a tester pulls
is the one CI built. The two halves of the supply chain:

- **Build-time (inputs):** the `Dockerfile` checksum-verifies every binary it
  downloads (kubeadm/kubelet/kubectl/containerd/runc/CNI) against the publisher's
  HTTPS-served `.sha256`.
- **Publish-time (outputs):** the `Release` workflow attaches, to every published
  image (by digest) and to the release binary, a **keyless SLSA build-provenance
  attestation** and a **CycloneDX SBOM attestation**, signed via the workflow's OIDC
  identity (no private key, ADR-15). The release job self-verifies its own
  attestations before publishing, so a broken signing step fails the release rather
  than reaching a tester.

Verify with the GitHub CLI (no extra tooling):

```sh
# image, by digest:
gh attestation verify oci://ghcr.io/kairos-io/provider-kubernetes@sha256:<digest> \
  --repo kairos-io/provider-kubernetes
# binary:
gh attestation verify agent-provider-kubernetes_<tag>_linux_amd64.tar.gz \
  --repo kairos-io/provider-kubernetes
```

The command above checks the build-provenance attestation. The SBOM is attested
separately as CycloneDX; to verify it specifically add
`--predicate-type https://cyclonedx.org/bom`. See the [README](../README.md#verifying-a-release) for the
digest-resolution one-liner.

## See also

- [Lifecycle and reset](./lifecycle.md) -- the reconcile model the e2e exercises.
- [Configuration reference](./configuration.md) -- the `cluster` contract the e2e
  serializes, including externally-managed control planes.
- [Node status](./status.md) -- the `status.yaml` + annotations the e2e asserts on.
