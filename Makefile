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

.PHONY: all build test vet fmt fmt-check lint tidy image clean

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

## clean: remove build artifacts
clean:
	rm -rf bin coverage.out
