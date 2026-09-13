# Security Policy

provider-kubernetes is part of the [Kairos](https://github.com/kairos-io/kairos)
project and follows the Kairos security process.

## Reporting a vulnerability

Please do **not** open a public issue or pull request for a security problem.

Report it privately through a
[GitHub Security Draft Advisory on kairos-io/kairos](https://github.com/kairos-io/kairos/security/advisories/new),
and mention that it affects `provider-kubernetes`. If you do not have a GitHub
account, email **security@kairos.io**.

Kairos is a community-driven project without a bug bounty. We aim to acknowledge
reports promptly, credit reporters, and fix issues as fast as possible. Patches
are welcome once a fix has been coordinated.

## Supported versions

This project is in early public release. Security fixes land on `main` and ship in
the next tagged release; only the latest release is supported. Each release
supports the Kubernetes minors listed in the [README](./README.md) (currently
1.35 / 1.36 / 1.37).

## What we do by default

The provider's trust model (join-time CA pinning, bounded bootstrap credentials,
no persisted bootstrap secrets) is described in [docs/security.md](./docs/security.md).
Supply-chain controls include digest-pinned base images and SHA-pinned workflow
actions, checksum-verified binary downloads, cosign-verified control-plane images,
signed release provenance and SBOM attestations, and continuous dependency
scanning with OSV-Scanner.
