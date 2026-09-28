TAG ?= dev
LDFLAGS ?= -s
GOOS ?= linux
GOARCH ?= $(shell go env GOARCH)
IMAGE ?= cluster-autoscaler-azure

# VERSION is the cluster-autoscaler version baked into the binary via -ldflags.
#   1. Exact git tag pointing at HEAD, with the "cluster-autoscaler-" prefix stripped.
#   2. The current commit SHA.
#   3. The literal string "dev" (for builds outside a git checkout).
# A "-dirty" suffix is appended when the git working tree has uncommitted
# changes (cases 1 and 2 only).
# Override with `make VERSION=<value> ...` to force a specific value; an
# externally provided VERSION is used verbatim, with no "-dirty" suffix.
ifeq ($(origin VERSION),undefined)
  VERSION := $(shell git describe --exact-match --tags 2>/dev/null | sed -e 's|^cluster-autoscaler-||')
  ifeq ($(strip $(VERSION)),)
    VERSION := $(shell git rev-parse HEAD 2>/dev/null)
  endif
  ifeq ($(strip $(VERSION)),)
    VERSION := dev
  else
    VERSION := $(VERSION)$(shell git diff --no-ext-diff --quiet --exit-code 2>/dev/null || echo -dirty)
  endif
endif

VERSION_PKG := k8s.io/autoscaler/cluster-autoscaler/version
VERSION_LDFLAG := -X $(VERSION_PKG).ClusterAutoscalerVersion=$(VERSION)
LDFLAGS_VALUE := $(strip $(LDFLAGS) $(VERSION_LDFLAG))

.PHONY: all build test-azure test-unit test-ci test-chart test-core-integration test-e2e-local verify-instance-types clean format image

all: build

build: build-arch-$(GOARCH)

build-arch-%:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$* go build -o cluster-autoscaler-$* --ldflags="$(LDFLAGS_VALUE)"

test-azure:
	go test -race -vet=all ./cloudprovider/azure/...

test-unit: build
	go test -race -vet=all ./...

test-chart:
	go test -tags helm ./charts -count=1

test-core-integration:
	go test -race sigs.k8s.io/cluster-autoscaler/pkg/test/integration/inmemory -count=1

test-e2e-local:
	$(MAKE) -C cloudprovider/azure/test test-local

test-ci: test-unit test-core-integration test-e2e-local

verify-instance-types:
	go run ./hack/verify-instance-types

clean:
	rm -f cluster-autoscaler-*

format:
	bash hack/update-gofmt.sh

image:
	docker build --pull \
		--platform=linux/$(GOARCH) \
		--build-arg "GOARCH=$(GOARCH)" \
		--build-arg 'LDFLAGS=$(LDFLAGS_VALUE)' \
		-t $(IMAGE):$(TAG) \
		-f Dockerfile .
