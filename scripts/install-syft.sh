#!/usr/bin/env bash
# Install syft, the SBOM generator the release workflow runs, into DIR.
#
# The release jobs that generate SBOMs can push images, write the GitHub
# Release and sign attestations with the workflow's OIDC identity (ADR-15), so
# nothing fetched at run time may execute unless it matches what is pinned
# here: the release tarball is checked against the sha256 below before it is
# unpacked, and no installer script runs. anchore/sbom-action used to run
# syft's install.sh from the main branch of anchore/syft in those jobs.
#
# To bump, change both lines together. Take the sha256 from the release's
# syft_<version>_checksums.txt after verifying that file's signature:
#   cosign verify-blob syft_<version>_checksums.txt \
#     --bundle syft_<version>_checksums.txt.sigstore.json \
#     --certificate-identity https://github.com/anchore/syft/.github/workflows/release.yaml@refs/heads/main \
#     --certificate-oidc-issuer https://token.actions.githubusercontent.com
# (Releases before the .sigstore.json bundle ship .pem and .sig instead: pass
# --certificate syft_<version>_checksums.txt.pem --signature
# syft_<version>_checksums.txt.sig in place of --bundle.) Renovate proposes
# both lines in one PR (customManagers in renovate.json), and CI installs and
# runs the pinned syft on every pull request.
#
# Usage: scripts/install-syft.sh DIR
set -euo pipefail

# renovate: datasource=github-release-attachments depName=anchore/syft
SYFT_VERSION=v1.42.3
SYFT_LINUX_AMD64_SHA256=0d6be741479eddd2c8644a288990c04f3df0d609bbc1599a005532a9dff63509

dir="${1:?usage: $0 DIR}"

case "$(uname -s)/$(uname -m)" in
  Linux/x86_64) ;;
  *)
    echo "install-syft: only linux/amd64 is pinned, not $(uname -s)/$(uname -m)" >&2
    exit 1
    ;;
esac

asset="syft_${SYFT_VERSION#v}_linux_amd64.tar.gz"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

curl -fsSL --proto '=https' --proto-redir '=https' --tlsv1.2 -o "${tmp}/${asset}" \
  "https://github.com/anchore/syft/releases/download/${SYFT_VERSION}/${asset}"
echo "${SYFT_LINUX_AMD64_SHA256}  ${tmp}/${asset}" | sha256sum --check --strict --quiet -

mkdir -p "${dir}"
tar -xzf "${tmp}/${asset}" -C "${dir}" syft
echo "install-syft: syft ${SYFT_VERSION} (sha256 ${SYFT_LINUX_AMD64_SHA256}) in ${dir}"
