# Build a Kairos image that bundles provider-kubernetes plus kubeadm, kubelet,
# kubectl, containerd, runc and the CNI plugins. Every external binary download is
# checksum-verified against the publisher's HTTPS-served .sha256 file, fixing the
# unverified-curl supply-chain pitfall of the previous kubeadm provider.
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
# Versions: pinned by default. Bump together via build args; CI should pin via
# image digest as well once a release pipeline lands.
# ----------------------------------------------------------------------------
ARG KAIROS_BASE_IMAGE=quay.io/kairos/ubuntu:24.04-core-amd64-generic-v3.5.1
ARG KAIROS_INIT_IMAGE=quay.io/kairos/kairos-init:v0.6.0
ARG GO_BUILDER_IMAGE=golang:1.26.3-alpine
ARG TARGETARCH=amd64

# Kubernetes (must be within the supported window the provider enforces at
# runtime: 1.34 / 1.35 / 1.36 as of 2026).
ARG KUBERNETES_VERSION=v1.34.0

# Container runtime stack.
ARG CONTAINERD_VERSION=2.1.4
ARG RUNC_VERSION=v1.3.0
ARG CNI_PLUGINS_VERSION=v1.8.0
ARG CRICTL_VERSION=v1.34.0

# Provider build version (injected into the binary via -ldflags).
ARG PROVIDER_VERSION=dev

# ----------------------------------------------------------------------------
# Stage: bring in the kairos-init binary. kairos-init is run as the very last
# step of the final image to regenerate the initramfs with our bundled binaries
# in place and to wire the installer machinery for our custom image. Without
# this step, kairos-installer.service exits 1 and auto-install does not fire on
# the LiveCD (observed empirically; see build/vmtest/RESULTS.md).
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
# Stage: download and verify Kubernetes binaries.
#
# dl.k8s.io publishes a per-binary .sha256 file alongside each binary, served
# over HTTPS by the same origin. Verifying with sha256sum -c (against that
# canonical .sha256) gives us integrity from the publisher without hardcoding
# hashes here, while ensuring an unverified or tampered download fails the build.
# ----------------------------------------------------------------------------
FROM alpine:3.21 AS k8s-binaries
ARG KUBERNETES_VERSION
ARG CRICTL_VERSION
ARG TARGETARCH
RUN apk add --no-cache curl ca-certificates
WORKDIR /bin

# Helper: download <name> and its <name>.sha256, then verify.
RUN set -eux; \
    for bin in kubeadm kubelet kubectl; do \
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
# Stage: download and verify the container-runtime stack (containerd, runc, CNI).
# ----------------------------------------------------------------------------
FROM alpine:3.21 AS runtime-binaries
ARG CONTAINERD_VERSION
ARG RUNC_VERSION
ARG CNI_PLUGINS_VERSION
ARG TARGETARCH
RUN apk add --no-cache curl ca-certificates
WORKDIR /bin

# containerd publishes a SHA256SUMS file per release. We pull only the line for
# our tarball and verify just that, keeping the verification scope tight.
RUN set -eux; \
    base="https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}"; \
    file="containerd-${CONTAINERD_VERSION}-linux-${TARGETARCH}.tar.gz"; \
    curl -fsSL -o "${file}" "${base}/${file}"; \
    curl -fsSL -o sha256sums "${base}/${file}.sha256sum"; \
    echo "$(awk '{print $1}' sha256sums)  ${file}" | sha256sum -c -; \
    mkdir containerd && tar -xzf "${file}" -C containerd; \
    rm "${file}" sha256sums

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
# Final stage: a Kairos image with everything wired up.
# ----------------------------------------------------------------------------
FROM ${KAIROS_BASE_IMAGE}
ARG KUBERNETES_VERSION

# --- Kubernetes binaries (verified) -----------------------------------------
COPY --from=k8s-binaries /bin/kubeadm  /usr/bin/kubeadm
COPY --from=k8s-binaries /bin/kubelet  /usr/bin/kubelet
COPY --from=k8s-binaries /bin/kubectl  /usr/bin/kubectl
COPY --from=k8s-binaries /bin/crictl   /usr/bin/crictl

# --- Container runtime (verified) -------------------------------------------
COPY --from=runtime-binaries /bin/containerd/bin/ /usr/bin/
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

# --- Boot-time setup: enable services; modules and sysctls load via /etc -----
RUN systemctl enable containerd.service kubelet.service

# Record the bundled Kubernetes version on the image (OS_VERSION style banner
# kept short; the provider also detects/enforces the version at runtime).
RUN echo "BUNDLED_KUBERNETES_VERSION=${KUBERNETES_VERSION}" >> /etc/os-release

# --- kairos-init finalize ---------------------------------------------------
# Regenerates the initramfs and wires the installer machinery for our custom
# image. MUST be the last layer touched before the build artifact is finalized.
#
# kairos-init's --version is strict semver. PROVIDER_VERSION can be a git SHA
# or 'dev', so we lift it into a synthesized semver (the SHA/string is preserved
# as semver build metadata after '+').
ARG PROVIDER_VERSION
COPY --from=kairos-init /kairos-init /kairos-init
RUN sanitized=$(printf '%s' "${PROVIDER_VERSION}" | tr -c 'A-Za-z0-9-' '-' | sed 's/^-*//; s/-*$//') \
 && image_version="v0.0.0-dev+${sanitized:-unset}" \
 && /kairos-init -l info -m generic --version "${image_version}" \
 && /kairos-init validate \
 && rm /kairos-init
