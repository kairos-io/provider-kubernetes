# Build a Kairos image that bundles provider-kubernetes plus kubeadm, kubelet,
# kubectl, containerd, runc and the CNI plugins. Every external binary download is
# checksum-verified against the publisher's HTTPS-served .sha256 file, fixing the
# unverified-curl supply-chain pitfall of the previous kubeadm provider.
#
# Base is the Kairos Hadron immutable OS (musl). We mirror the canonical Kairos
# image build flow (images/Dockerfile upstream): the base is a pure upstream OS
# that kairos-init transforms into a Kairos system in two phases (install, init).
# Because Hadron is musl, the official glibc-linked containerd and kubelet cannot
# exec; we build both fully static from source (kubeadm/kubectl/crictl/runc are
# already static and run on musl unchanged).
#
# Build:
#   docker build \
#     --build-arg KUBERNETES_VERSION=v1.34.0 \
#     --build-arg PROVIDER_VERSION=$(git describe --always) \
#     -t ghcr.io/<you>/kairos-kubeadm:<tag> .
#
# Convert the resulting Docker image to a bootable ISO/raw with auroraboot
# (https://kairos.io/docs/reference/auroraboot/). See samples/README.md.

# ----------------------------------------------------------------------------
# Versions: pinned by default. Bump together via build args. Base images are
# digest-pinned below (kairos#4203) for a reproducible, tamper-evident supply chain.
# ----------------------------------------------------------------------------
# Pure upstream Hadron base (musl). kairos-init transforms it into a Kairos OS in
# the final stage, exactly as the canonical Kairos image build does.
# All base images are DIGEST-pinned (image:tag@sha256:...) for a reproducible,
# tamper-evident supply chain (kairos#4203). The tag is kept for readability; the
# digest is the multi-arch index digest, so cross-arch builds still resolve the
# right manifest. When bumping a tag, re-resolve the digest
# (docker buildx imagetools inspect <image:tag>) and update both here and in the
# Makefile defaults that override these ARGs.
ARG KAIROS_BASE_IMAGE=ghcr.io/kairos-io/hadron:v0.4.0@sha256:1e19d9cd5a70dfc6940f58d899e72f6776f4d64934cd6f402f4a4186ccc40d4d
# kairos-init that matches the version Kairos itself uses to build Hadron. Older
# pins (e.g. v0.6.0) cannot regenerate Hadron's initramfs (dracut -f ... fails).
ARG KAIROS_INIT_IMAGE=quay.io/kairos/kairos-init:v0.14.6@sha256:e53eb7e5ada035e7e192f072f9e041ca5d60440ecf8c766c32e7d95253b293e7
ARG GO_BUILDER_IMAGE=golang:1.26.4-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648
ARG TARGETARCH=amd64

# containerd and kubelet are ALWAYS built fully static from source: Hadron is
# musl, so the glibc-linked official releases cannot exec. A fully static binary
# runs on musl AND glibc. STATIC_BUILDER is the from-source toolchain (Debian Go,
# matching the validated recipe); patch-pinned (matches GO_BUILDER_IMAGE's pin
# level) so the from-source compiler is reproducible across Go patch releases.
ARG STATIC_BUILDER_IMAGE=golang:1.26.4@sha256:f96cc555eb8db430159a3aa6797cd5bae561945b7b0fe7d0e284c63a3b291609

# Kubernetes (must be within the supported window the provider enforces at
# runtime: 1.34 / 1.35 / 1.36 as of 2026).
ARG KUBERNETES_VERSION=v1.34.0
# Commit SHA the KUBERNETES_VERSION tag must resolve to. Used by the static
# from-source kubelet build to pin the clone to an immutable commit (a git tag is
# mutable; a commit SHA is content-addressed), matching the checksum discipline of
# the binary-download path. MUST be updated together with KUBERNETES_VERSION or
# the build fails loud.
ARG KUBERNETES_COMMIT=f28b4c9efbca5c5c0af716d9f2d5702667ee8a45

# Container runtime stack.
ARG CONTAINERD_VERSION=2.1.4
# Commit SHA for CONTAINERD_VERSION (same rationale as KUBERNETES_COMMIT; used by
# the static from-source containerd build).
ARG CONTAINERD_COMMIT=75cb2b7193e4e490e9fbdc236c0e811ccaba3376
ARG RUNC_VERSION=v1.3.0
ARG CNI_PLUGINS_VERSION=v1.8.0
ARG CRICTL_VERSION=v1.34.0

# Provider build version (injected into the binary via -ldflags).
ARG PROVIDER_VERSION=dev

# ----------------------------------------------------------------------------
# Stage: bring in the kairos-init binary. kairos-init is run as the very last
# step of the final image to transform the pure Hadron base into a Kairos system
# (install + init phases), mirroring the canonical Kairos image build.
# ----------------------------------------------------------------------------
FROM ${KAIROS_INIT_IMAGE} AS kairos-init

# ----------------------------------------------------------------------------
# Stage: build the provider binary.
# ----------------------------------------------------------------------------
FROM ${GO_BUILDER_IMAGE} AS provider-builder
ARG PROVIDER_VERSION
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build \
      -ldflags "-X github.com/kairos-io/provider-kubernetes/version.Version=${PROVIDER_VERSION} -w -s" \
      -o /out/agent-provider-kubernetes .

# ----------------------------------------------------------------------------
# Stage: download and verify Kubernetes binaries (kubeadm, kubectl, crictl).
#
# dl.k8s.io publishes a per-binary .sha256 file alongside each binary, served
# over HTTPS by the same origin. Verifying with sha256sum -c (against that
# canonical .sha256) gives us integrity from the publisher without hardcoding
# hashes here, while ensuring an unverified or tampered download fails the build.
# kubeadm/kubectl/crictl are already static and run on musl unchanged. kubelet is
# NOT downloaded here -- it is built static from source (see kubelet-build).
# ----------------------------------------------------------------------------
FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d AS k8s-binaries
ARG KUBERNETES_VERSION
ARG CRICTL_VERSION
ARG TARGETARCH
RUN apk add --no-cache curl ca-certificates
WORKDIR /bin

# Helper: download <name> and its <name>.sha256, then verify.
RUN set -eux; \
    for bin in kubeadm kubectl; do \
      base="https://dl.k8s.io/${KUBERNETES_VERSION}/bin/linux/${TARGETARCH}"; \
      curl -fsSL -o "${bin}" "${base}/${bin}"; \
      curl -fsSL -o "${bin}.sha256" "${base}/${bin}.sha256"; \
      echo "$(cat ${bin}.sha256)  ${bin}" | sha256sum -c -; \
      rm "${bin}.sha256"; \
      chmod +x "${bin}"; \
    done

# crictl: published with a SHA256SUMS file alongside the release tarballs.
RUN set -eux; \
    base="https://github.com/kubernetes-sigs/cri-tools/releases/download/${CRICTL_VERSION}"; \
    file="crictl-${CRICTL_VERSION}-linux-${TARGETARCH}.tar.gz"; \
    curl -fsSL -o "${file}" "${base}/${file}"; \
    curl -fsSL -o "${file}.sha256" "${base}/${file}.sha256"; \
    echo "$(cat ${file}.sha256)  ${file}" | sha256sum -c -; \
    tar -xzf "${file}"; \
    rm "${file}" "${file}.sha256"; \
    chmod +x crictl

# ----------------------------------------------------------------------------
# Stage: download and verify the runtime stack we DON'T build from source
# (runc, CNI). containerd is built static from source (see containerd-build);
# runc and the CNI plugins are already static and run on musl unchanged.
# ----------------------------------------------------------------------------
FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d AS runtime-binaries
ARG RUNC_VERSION
ARG CNI_PLUGINS_VERSION
ARG TARGETARCH
RUN apk add --no-cache curl ca-certificates
WORKDIR /bin

# runc: ships a PGP-signed runc.sha256sum that names files like "runc.amd64",
# so we download under that name (matching the sha256sum line), verify, then
# rename to plain "runc" for the final image.
RUN set -eux; \
    base="https://github.com/opencontainers/runc/releases/download/${RUNC_VERSION}"; \
    curl -fsSL -o "runc.${TARGETARCH}"      "${base}/runc.${TARGETARCH}"; \
    curl -fsSL -o runc.sha256sum            "${base}/runc.sha256sum"; \
    grep "  runc.${TARGETARCH}$" runc.sha256sum | sha256sum -c -; \
    rm runc.sha256sum; \
    mv "runc.${TARGETARCH}" runc; \
    chmod +x runc

# CNI plugins: each release tarball has a matching <file>.sha256 sibling.
RUN set -eux; \
    base="https://github.com/containernetworking/plugins/releases/download/${CNI_PLUGINS_VERSION}"; \
    file="cni-plugins-linux-${TARGETARCH}-${CNI_PLUGINS_VERSION}.tgz"; \
    curl -fsSL -o "${file}" "${base}/${file}"; \
    curl -fsSL -o "${file}.sha256" "${base}/${file}.sha256"; \
    echo "$(awk '{print $1}' ${file}.sha256)  ${file}" | sha256sum -c -; \
    mkdir -p cni && tar -xzf "${file}" -C cni; \
    rm "${file}" "${file}.sha256"

# ----------------------------------------------------------------------------
# Stage: build kubelet fully static from source.
#
# Hadron is musl; the glibc-linked official kubelet cannot exec. CGO_ENABLED=0
# produces a fully static binary that runs on musl AND glibc. The clone is pinned
# to an immutable commit (KUBERNETES_COMMIT); a mismatch fails the build loud.
# ----------------------------------------------------------------------------
FROM ${STATIC_BUILDER_IMAGE} AS kubelet-build
ARG KUBERNETES_VERSION
ARG KUBERNETES_COMMIT
ARG TARGETARCH
# Clone the version tag, then PIN to the expected commit (a tag is mutable; the
# commit SHA is immutable). Mismatch fails loud -- update KUBERNETES_COMMIT when
# bumping KUBERNETES_VERSION.
RUN git clone --depth 1 --branch "${KUBERNETES_VERSION}" \
      https://github.com/kubernetes/kubernetes /src \
 && got="$(git -C /src rev-parse HEAD)" \
 && if [ "${got}" != "${KUBERNETES_COMMIT}" ]; then \
      echo "kubernetes ${KUBERNETES_VERSION} resolved to ${got}, expected ${KUBERNETES_COMMIT}; update KUBERNETES_COMMIT to match KUBERNETES_VERSION" >&2; exit 1; \
    fi
WORKDIR /src
RUN set -eux; \
    V="${KUBERNETES_VERSION}"; \
    maj="$(echo "$V" | sed -E 's/^v([0-9]+)\..*/\1/')"; \
    min="$(echo "$V" | sed -E 's/^v[0-9]+\.([0-9]+)\..*/\1/')"; \
    LD="-s -w \
      -X k8s.io/client-go/pkg/version.gitVersion=${V} \
      -X k8s.io/component-base/version.gitVersion=${V} \
      -X k8s.io/client-go/pkg/version.gitMajor=${maj} \
      -X k8s.io/client-go/pkg/version.gitMinor=${min} \
      -X k8s.io/component-base/version.gitMajor=${maj} \
      -X k8s.io/component-base/version.gitMinor=${min} \
      -X k8s.io/client-go/pkg/version.gitTreeState=clean \
      -X k8s.io/component-base/version.gitTreeState=clean"; \
    CGO_ENABLED=0 GOOS=linux GOARCH="${TARGETARCH}" GOFLAGS=-mod=vendor \
      go build -ldflags "${LD}" -o /bin/kubelet ./cmd/kubelet; \
    [ "$(/bin/kubelet --version)" = "Kubernetes ${V}" ] || { echo "kubelet version ldflags wrong: $(/bin/kubelet --version)" >&2; exit 1; }

# ----------------------------------------------------------------------------
# Stage: build containerd + shim fully static from source.
#
# make STATIC=1 produces fully static containerd + shim (verified: "not a dynamic
# executable"); a fully static binary runs on musl AND glibc. The clone is pinned
# to an immutable commit (CONTAINERD_COMMIT); a mismatch fails the build loud.
# ----------------------------------------------------------------------------
FROM ${STATIC_BUILDER_IMAGE} AS containerd-build
ARG CONTAINERD_VERSION
ARG CONTAINERD_COMMIT
RUN apt-get update \
 && apt-get install -y --no-install-recommends make git \
 && rm -rf /var/lib/apt/lists/*
# Clone the version tag, then PIN to the expected commit (see KUBERNETES_COMMIT).
RUN git clone --depth 1 --branch "v${CONTAINERD_VERSION}" \
      https://github.com/containerd/containerd /src \
 && got="$(git -C /src rev-parse HEAD)" \
 && if [ "${got}" != "${CONTAINERD_COMMIT}" ]; then \
      echo "containerd v${CONTAINERD_VERSION} resolved to ${got}, expected ${CONTAINERD_COMMIT}; update CONTAINERD_COMMIT to match CONTAINERD_VERSION" >&2; exit 1; \
    fi
WORKDIR /src
# Lay the static binaries out under /bin/containerd/bin so the final stage can
# COPY the whole bin/ tree in one shot. ctr is built too: the boot-time image
# import (ADR-16) uses `ctr -n k8s.io images import`, and it must be static (musl).
RUN set -eux; \
    make STATIC=1 bin/containerd bin/containerd-shim-runc-v2 bin/ctr; \
    mkdir -p /bin/containerd/bin; \
    cp bin/containerd bin/containerd-shim-runc-v2 bin/ctr /bin/containerd/bin/

# ----------------------------------------------------------------------------
# Stage: pre-bundle the control-plane container images (ADR-16). Resolve the EXACT
# set from the bundled kubeadm (same source as the pause pin, so refs never drift),
# cosign-VERIFY each against the Kubernetes release identity, and fetch each as a
# ctr-importable tarball with crane (daemonless). The tarballs + a digest lockfile
# are embedded read-only in the final image and imported into containerd at boot,
# so a first boot converges with NO registry access (air-gap). Because the images
# land in the immutable OS with no later admission check, signature verification is
# done here at BUILD time (P5). CNI is intentionally NOT bundled (operator choice).
# ----------------------------------------------------------------------------
FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d AS image-bundler
ARG KUBERNETES_VERSION
ARG TARGETARCH
# crane + cosign are static Go binaries that run on musl; both are version-pinned
# and checksum-verified at install, matching the rest of the binary supply chain.
ARG CRANE_VERSION=v0.20.3
ARG COSIGN_VERSION=v2.4.3
RUN apk add --no-cache curl ca-certificates
COPY --from=k8s-binaries /bin/kubeadm /usr/bin/kubeadm
# Install crane (checksum-verified against the release checksums.txt).
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) arch="x86_64" ;; \
      arm64) arch="arm64" ;; \
      *) echo "unsupported TARGETARCH ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    base="https://github.com/google/go-containerregistry/releases/download/${CRANE_VERSION}"; \
    asset="go-containerregistry_Linux_${arch}.tar.gz"; \
    curl -fsSL -o "${asset}" "${base}/${asset}"; \
    curl -fsSL -o checksums.txt "${base}/checksums.txt"; \
    grep " ${asset}\$" checksums.txt | sha256sum -c -; \
    tar -xzf "${asset}" crane; install -m0755 crane /usr/bin/crane; \
    rm -f "${asset}" checksums.txt crane
# Install cosign (checksum-verified against the release cosign_checksums.txt).
RUN set -eux; \
    case "${TARGETARCH}" in amd64|arm64) : ;; *) echo "unsupported TARGETARCH ${TARGETARCH}" >&2; exit 1 ;; esac; \
    base="https://github.com/sigstore/cosign/releases/download/${COSIGN_VERSION}"; \
    asset="cosign-linux-${TARGETARCH}"; \
    curl -fsSL -o "${asset}" "${base}/${asset}"; \
    curl -fsSL -o cosign_checksums.txt "${base}/cosign_checksums.txt"; \
    grep " ${asset}\$" cosign_checksums.txt | sha256sum -c -; \
    install -m0755 "${asset}" /usr/bin/cosign; \
    rm -f "${asset}" cosign_checksums.txt
# Resolve -> verify -> pull each control-plane image, and write the digest lockfile.
COPY build/bundle-images.sh /usr/local/bin/bundle-images.sh
RUN KUBERNETES_VERSION="${KUBERNETES_VERSION}" OUT_DIR=/images \
      sh /usr/local/bin/bundle-images.sh

# ----------------------------------------------------------------------------
# Final stage: a Kairos image with everything wired up.
#
# Order matters: we lay down every bundled binary and config FIRST, then run
# kairos-init LAST so the install/init phases (which regenerate the kernel/initrd
# and wire the installer machinery) see the final on-disk layout.
# ----------------------------------------------------------------------------
FROM ${KAIROS_BASE_IMAGE}
ARG KUBERNETES_VERSION

# --- Kubernetes binaries -----------------------------------------------------
# kubeadm/kubectl/crictl are the verified official static binaries; kubelet is
# built static from source (musl base).
COPY --from=k8s-binaries  /bin/kubeadm  /usr/bin/kubeadm
COPY --from=kubelet-build  /bin/kubelet  /usr/bin/kubelet
COPY --from=k8s-binaries  /bin/kubectl  /usr/bin/kubectl
COPY --from=k8s-binaries  /bin/crictl   /usr/bin/crictl

# --- Container runtime -------------------------------------------------------
# containerd + shim are built static from source; runc and CNI are verified
# official static binaries.
COPY --from=containerd-build /bin/containerd/bin/ /usr/bin/
COPY --from=runtime-binaries /bin/runc            /usr/bin/runc
RUN mkdir -p /opt/cni/bin
COPY --from=runtime-binaries /bin/cni/            /opt/cni/bin/

# --- Provider binary --------------------------------------------------------
# kairos-agent discovers binaries named agent-provider-* under /system/providers.
COPY --from=provider-builder /out/agent-provider-kubernetes /system/providers/agent-provider-kubernetes

# --- Static configuration ---------------------------------------------------
COPY containerd/config.toml                 /etc/containerd/config.toml
COPY systemd/containerd.service             /etc/systemd/system/containerd.service
COPY systemd/kubelet.service                /etc/systemd/system/kubelet.service
COPY systemd/kubelet.service.d/10-kubeadm.conf /etc/systemd/system/kubelet.service.d/10-kubeadm.conf
COPY sysctl/k8s.conf                        /etc/sysctl.d/k8s.conf
COPY modules-load/k8s.conf                  /etc/modules-load.d/k8s.conf
COPY systemd/provider-kubernetes-image-import.service /etc/systemd/system/provider-kubernetes-image-import.service

# --- Pre-bundled control-plane images (ADR-16) ------------------------------
# Embed the control-plane image tarballs (fetched by the image-bundler stage)
# read-only in the OS image; the import oneshot loads them into containerd at boot
# so a first boot converges with no registry access (air-gap).
COPY --from=image-bundler /images /opt/provider-kubernetes/images

# Pin containerd's pod-sandbox (pause) image to the EXACT version the bundled
# kubeadm expects for this Kubernetes minor, instead of a hardcoded tag. kubeadm
# bumps the pause version per release (e.g. 3.10.1 in 1.34/1.35 -> 3.10.2 in 1.36);
# a stale tag means containerd pulls a different pause than kubeadm pre-pulled
# (duplicate image / drift -- pitfall C4). Resolved from kubeadm at build time so
# it always matches the bundled toolchain. kubeadm is static and runs here on musl.
RUN set -eux; \
    pause="$(/usr/bin/kubeadm config images list \
      --kubernetes-version "${KUBERNETES_VERSION}" \
      --image-repository registry.k8s.io | grep -E '/pause:[0-9]')"; \
    test -n "${pause}"; \
    sed -i "s#^\([[:space:]]*sandbox_image[[:space:]]*=\).*#\1 \"${pause}\"#" /etc/containerd/config.toml; \
    grep -q "sandbox_image = \"${pause}\"" /etc/containerd/config.toml; \
    echo "pinned containerd sandbox_image to ${pause}"

# --- Boot-time setup: enable services; modules and sysctls load via /etc -----
RUN systemctl enable containerd.service kubelet.service provider-kubernetes-image-import.service

# Record the bundled Kubernetes version on the image (OS_VERSION style banner
# kept short; the provider also detects/enforces the version at runtime).
RUN echo "BUNDLED_KUBERNETES_VERSION=${KUBERNETES_VERSION}" >> /etc/os-release

# --- kairos-init: transform the pure Hadron base into a Kairos system --------
# Mirrors the canonical Kairos image build (images/Dockerfile): two phases in one
# RUN -- "install" then "init" -- no "validate". This runs LAST so it sees every
# bundled binary/config above. kairos-init regenerates the kernel/initramfs and
# wires the installer machinery; without it kairos-installer.service exits 1 and
# auto-install does not fire on the LiveCD.
#
# kairos-init's --version is strict semver. PROVIDER_VERSION can be a git SHA or
# 'dev', so we lift it into a synthesized semver (the SHA/string is preserved as
# semver build metadata after '+').
ARG PROVIDER_VERSION
# OCI provenance: links the published package back to the repo + records the
# provider/Kubernetes versions baked in. Applied to every build (local, CI, release).
LABEL org.opencontainers.image.source="https://github.com/kairos-io/provider-kubernetes" \
      org.opencontainers.image.description="Kairos provider-kubernetes: kubeadm + provider, Kubernetes ${KUBERNETES_VERSION}" \
      org.opencontainers.image.version="${PROVIDER_VERSION}" \
      org.opencontainers.image.licenses="Apache-2.0"
RUN --mount=type=bind,from=kairos-init,src=/kairos-init,dst=/kairos-init \
    set -eux; \
    sanitized=$(printf '%s' "${PROVIDER_VERSION}" | tr -c 'A-Za-z0-9-' '-' | sed 's/^-*//; s/-*$//'); \
    image_version="v0.0.0-dev+${sanitized:-unset}"; \
    /kairos-init -l debug -s install -m generic -t false --version "${image_version}"; \
    /kairos-init -l debug -s init    -m generic -t false --version "${image_version}"
