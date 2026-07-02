# Contributing

Contributions are welcome. Please open an issue to discuss substantial changes
before submitting a pull request.

## Building and testing

Requires Go 1.26.4+.

```sh
make build      # produces ./bin/agent-provider-kubernetes
make test       # unit / behavior tests
make vet
make lint       # requires golangci-lint
make e2e        # real-kubeadm end-to-end suite (Docker + privileged containers)
```

See [`docs/testing.md`](./docs/testing.md) for the full test layering and the
coverage boundary.

## Sign-off (DCO)

Every commit must be signed off under the Developer Certificate of Origin:

```sh
git commit -s -m "..."
```

## Verifying a release

Every published image and the release binary carry **keyless SLSA build-provenance
and CycloneDX SBOM attestations**, signed via the release workflow's OIDC identity (no
private key). Verify them with the GitHub CLI -- no extra tooling:

```sh
# Image -- verify by digest (resolve it first so you verify the exact bytes):
digest=$(docker buildx imagetools inspect \
  ghcr.io/kairos-io/provider-kubernetes:v0.3.0-k8s1.34 \
  --format '{{.Manifest.Digest}}')
gh attestation verify \
  oci://ghcr.io/kairos-io/provider-kubernetes@${digest} \
  --repo kairos-io/provider-kubernetes

# Binary tarball (downloaded from the GitHub Release):
gh attestation verify \
  agent-provider-kubernetes_v0.3.0_linux_amd64.tar.gz \
  --repo kairos-io/provider-kubernetes
```

A successful verification confirms the artifact was built by this repository's
`Release` workflow from a tagged commit. That command checks the provenance; add
`--predicate-type https://cyclonedx.org/bom` to verify the CycloneDX SBOM
attestation specifically.

Each image additionally carries an attestation of the **pre-bundled control-plane
images** it baked (ADR-16): which images, by digest, and whether each was
upstream-cosign-verified at build vs. digest-pinned only. It uses a custom
predicate type, so select it explicitly:

```sh
gh attestation verify \
  oci://ghcr.io/kairos-io/provider-kubernetes@${digest} \
  --repo kairos-io/provider-kubernetes \
  --predicate-type https://kairos.io/attestations/bundled-control-plane-images/v1
```

The predicate is the image's `/opt/provider-kubernetes/images/images.lock`
(`{ref, digest, verified, verifyReason}` per image). See
[`docs/testing.md`](./docs/testing.md) for the full supply-chain picture.

## Releasing (maintainers)

Maintainers cut a release by pushing a signed semver tag; the `Release` workflow
builds + pushes the per-minor images (with attestations) and creates the GitHub
Release:

```sh
git tag -s v0.3.0 -m "v0.3.0" && git push origin v0.3.0
```
