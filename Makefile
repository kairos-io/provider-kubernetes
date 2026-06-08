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
CRICTL_VERSION     ?= v1.34.0
CONTAINERD_VERSION ?= 2.1.4
RUNC_VERSION       ?= v1.3.0
CNI_PLUGINS_VERSION ?= v1.8.0
IMAGE              ?= kairos-kubeadm:$(VERSION)

# e2e (ADR-13). The node image FROM-derives the kairos-kubeadm base so it reuses
# the checksum-verified toolchain. BASE_IMAGE defaults to the per-version
# kairos-kubeadm tag; E2E_NODE_IMAGE is the derived systemd-node image the
# harness runs. E2E_TIMEOUT is a never-hang backstop for the whole suite.
BASE_IMAGE       ?= kairos-kubeadm:$(KUBERNETES_VERSION)
E2E_NODE_IMAGE   ?= kairos-kubeadm-e2e-node:$(KUBERNETES_VERSION)
E2E_TIMEOUT      ?= 40m

.PHONY: all build test vet fmt fmt-check lint tidy image clean e2e-node-image e2e

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

## image: build the Kairos image bundling provider + kubeadm + containerd
## (requires Docker; supply-chain-verified downloads inside the Dockerfile).
image:
	docker build \
	  --build-arg KUBERNETES_VERSION=$(KUBERNETES_VERSION) \
	  --build-arg CRICTL_VERSION=$(CRICTL_VERSION) \
	  --build-arg CONTAINERD_VERSION=$(CONTAINERD_VERSION) \
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

## clean: remove build artifacts
clean:
	rm -rf bin coverage.out
