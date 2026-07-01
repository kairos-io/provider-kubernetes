# provider-kubernetes — build & quality targets.
# The provider binary is named agent-provider-kubernetes (Kairos convention:
# kairos-agent discovers binaries prefixed with "agent-provider-").

BINARY  ?= agent-provider-kubernetes
VERSION ?= dev
GO      ?= go
LDFLAGS := -X github.com/kairos-io/provider-kubernetes/version.Version=$(VERSION) -w -s

# Image build args. Defaults MUST match the Dockerfile ARG defaults so that an
# unset override behaves identically whether the image is built via `make image`
# or a bare `docker build`. The `image` target forwards every one of these, so a
# missing forward can never silently fall back to the Dockerfile default again.
KUBERNETES_VERSION ?= v1.34.0
# Commit SHA the version tags resolve to; pins the static from-source kubelet
# build. Update together with KUBERNETES_VERSION.
KUBERNETES_COMMIT  ?= f28b4c9efbca5c5c0af716d9f2d5702667ee8a45
CRICTL_VERSION     ?= v1.34.0
CONTAINERD_VERSION ?= 2.1.4
# Commit SHA pinning the static from-source containerd build. Update together
# with CONTAINERD_VERSION.
CONTAINERD_COMMIT  ?= 75cb2b7193e4e490e9fbdc236c0e811ccaba3376
RUNC_VERSION       ?= v1.3.0
CNI_PLUGINS_VERSION ?= v1.8.0
# Kairos OS base the image is built FROM. Defaults to the pure upstream Hadron
# (musl) OS, which kairos-init transforms into a Kairos system in the final stage
# (mirroring the canonical Kairos image build). Override to test another base.
# DIGEST-pinned (kairos#4203); must match the Dockerfile ARG defaults. Re-resolve
# with `docker buildx imagetools inspect <image:tag>` when bumping the tag.
KAIROS_BASE_IMAGE  ?= ghcr.io/kairos-io/hadron:v0.4.0@sha256:1e19d9cd5a70dfc6940f58d899e72f6776f4d64934cd6f402f4a4186ccc40d4d
# kairos-init image used in the final stage. Defaults to the Dockerfile's pin
# (the version Kairos itself uses to build Hadron); override for testing.
KAIROS_INIT_IMAGE  ?= quay.io/kairos/kairos-init:v0.14.6@sha256:e53eb7e5ada035e7e192f072f9e041ca5d60440ecf8c766c32e7d95253b293e7
IMAGE              ?= kairos-kubeadm:$(VERSION)

# e2e (ADR-13). The node image FROM-derives the kairos-kubeadm base so it reuses
# the checksum-verified toolchain. BASE_IMAGE defaults to the per-version
# kairos-kubeadm tag; E2E_NODE_IMAGE is the derived systemd-node image the
# harness runs. E2E_TIMEOUT is a never-hang backstop for the whole suite.
BASE_IMAGE       ?= kairos-kubeadm:$(KUBERNETES_VERSION)
E2E_NODE_IMAGE   ?= kairos-kubeadm-e2e-node:$(KUBERNETES_VERSION)
E2E_TIMEOUT      ?= 40m
# Tier-2 nightly suite runs heavier multi-container scenarios (HA, upgrade), so it
# carries a longer wall-clock ceiling than the per-PR Tier-1 suite.
E2E_NIGHTLY_TIMEOUT ?= 90m
# Higher-minor node image + version for the in-place kubeadm upgrade scenario; set
# by the nightly workflow. Empty -> the upgrade scenario self-skips (never faked).
E2E_UPGRADE_TO_NODE_IMAGE ?=
E2E_UPGRADE_TO_VERSION    ?=

.PHONY: all build test vet fmt fmt-check lint tidy verify-pins image clean e2e-node-image e2e e2e-nightly

all: build

## build: compile the provider binary into ./bin
build:
	CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) .

## test: run all tests with coverage
test:
	$(GO) test ./... -coverprofile=coverage.out

## vet: run go vet
vet:
	$(GO) vet ./...

## fmt: format the code in place
fmt:
	gofmt -s -w .

## fmt-check: fail if any file is not gofmt-clean
fmt-check:
	@out="$$(gofmt -s -l .)"; if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi

## lint: run golangci-lint (must be installed)
lint:
	golangci-lint run

## tidy: sync go.mod/go.sum
tidy:
	$(GO) mod tidy

## verify-pins: fail if a digest-pinned base image drifts between Dockerfile and
## Makefile, or is not digest-pinned at all (kairos#4203 supply-chain guard).
verify-pins:
	@fail=0; \
	for var in KAIROS_BASE_IMAGE KAIROS_INIT_IMAGE; do \
	  df="$$(sed -n "s/^ARG $$var=//p" Dockerfile | head -1)"; \
	  mk="$$(sed -n "s/^$$var[[:space:]]*?=[[:space:]]*//p" Makefile | head -1)"; \
	  case "$$df" in *@sha256:*) : ;; *) echo "FAIL: Dockerfile $$var is not digest-pinned: '$$df'"; fail=1 ;; esac; \
	  if [ "$$df" != "$$mk" ]; then \
	    echo "FAIL: $$var drift:"; echo "  Dockerfile: $$df"; echo "  Makefile:   $$mk"; fail=1; \
	  else \
	    echo "ok: $$var = $$df"; \
	  fi; \
	done; \
	if [ "$$fail" != 0 ]; then echo "re-resolve with: docker buildx imagetools inspect <image:tag>"; exit 1; fi

## image: build the Kairos image bundling provider + kubeadm + containerd
## (requires Docker; supply-chain-verified downloads inside the Dockerfile).
image:
	docker build \
	  --build-arg KAIROS_BASE_IMAGE=$(KAIROS_BASE_IMAGE) \
	  --build-arg KAIROS_INIT_IMAGE=$(KAIROS_INIT_IMAGE) \
	  --build-arg KUBERNETES_VERSION=$(KUBERNETES_VERSION) \
	  --build-arg KUBERNETES_COMMIT=$(KUBERNETES_COMMIT) \
	  --build-arg CRICTL_VERSION=$(CRICTL_VERSION) \
	  --build-arg CONTAINERD_VERSION=$(CONTAINERD_VERSION) \
	  --build-arg CONTAINERD_COMMIT=$(CONTAINERD_COMMIT) \
	  --build-arg RUNC_VERSION=$(RUNC_VERSION) \
	  --build-arg CNI_PLUGINS_VERSION=$(CNI_PLUGINS_VERSION) \
	  --build-arg PROVIDER_VERSION=$(VERSION) \
	  -t $(IMAGE) .

## e2e-node-image: build the systemd-as-PID1 node container image (ADR-13 E2),
## FROM-deriving the kairos-kubeadm base. If the base image is absent, build it
## first with: make image KUBERNETES_VERSION=$(KUBERNETES_VERSION) VERSION=$(KUBERNETES_VERSION) IMAGE=$(BASE_IMAGE)
e2e-node-image:
	@if ! docker image inspect $(BASE_IMAGE) >/dev/null 2>&1; then \
	  echo "ERROR: base image $(BASE_IMAGE) not found."; \
	  echo "Build it first:"; \
	  echo "  make image KUBERNETES_VERSION=$(KUBERNETES_VERSION) VERSION=$(KUBERNETES_VERSION) IMAGE=$(BASE_IMAGE)"; \
	  exit 1; \
	fi
	docker build \
	  --build-arg BASE_IMAGE=$(BASE_IMAGE) \
	  -f Dockerfile.e2e-node \
	  -t $(E2E_NODE_IMAGE) .

## e2e: run the tag-gated e2e suite (ADR-13 E1) against a real kubeadm node
## container. Builds the node image first. Requires Docker with privileged
## containers. The e2e files are behind //go:build e2e, so `make test` is
## unaffected.
e2e: e2e-node-image
	E2E_NODE_IMAGE=$(E2E_NODE_IMAGE) \
	E2E_KUBERNETES_VERSION=$(KUBERNETES_VERSION) \
	$(GO) test -tags e2e -timeout $(E2E_TIMEOUT) -v ./test/e2e/...

## e2e-nightly: run the heavier Tier-2 (ADR-13 E4) scenarios that are too slow for
## per-PR (CP-join/HA, pre-membership failure-status, kubeadm-layer upgrade). It
## mirrors `e2e` but compiles the nightly-gated files too (-tags "e2e nightly", so
## //go:build e2e && nightly files are included) and allows a longer timeout. The
## upgrade scenario also consumes E2E_UPGRADE_TO_NODE_IMAGE / E2E_UPGRADE_TO_VERSION
## (the higher-minor node image); when unset that one scenario self-skips.
e2e-nightly: e2e-node-image
	E2E_NODE_IMAGE=$(E2E_NODE_IMAGE) \
	E2E_KUBERNETES_VERSION=$(KUBERNETES_VERSION) \
	E2E_UPGRADE_TO_NODE_IMAGE=$(E2E_UPGRADE_TO_NODE_IMAGE) \
	E2E_UPGRADE_TO_VERSION=$(E2E_UPGRADE_TO_VERSION) \
	$(GO) test -tags "e2e nightly" -count=1 -timeout $(E2E_NIGHTLY_TIMEOUT) -v ./test/e2e/...

## clean: remove build artifacts
clean:
	rm -rf bin coverage.out
