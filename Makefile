ALL_ARCH ?= amd64 arm64 s390x
TAG ?= dev
LDFLAGS ?= -s
GOOS ?= linux
GOARCH ?= $(shell go env GOARCH)
IMAGE ?= cluster-autoscaler-azure

VERSION_PKG := k8s.io/autoscaler/cluster-autoscaler/version
# Explicit VERSION overrides are used verbatim.
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
VERSION_LDFLAG := -X $(VERSION_PKG).ClusterAutoscalerVersion=$(VERSION)
LDFLAGS_VALUE := $(strip $(LDFLAGS) $(VERSION_LDFLAG))

.PHONY: all build build-arch test-azure test-unit test-ci test-chart test-core-integration test-e2e-local clean format image

all: build

build: build-arch-$(GOARCH)

build-arch-%:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$* go build -o cluster-autoscaler-$* --ldflags="$(LDFLAGS_VALUE)"

test-azure:
	go test -race ./cloudprovider/azure/...

test-unit: build test-azure
	go test -race ./...

test-chart:
	go test -mod=readonly -tags helm ./charts -count=1

test-core-integration:
	go test -mod=readonly -race sigs.k8s.io/cluster-autoscaler/pkg/test/integration/inmemory -run 'TestStaticAutoscaler_FullLifecycle|TestScaleUp_ResourceLimits' -count=1

test-e2e-local:
	$(MAKE) -C cloudprovider/azure/test test-local

test-ci: test-unit test-core-integration test-e2e-local

clean:
	rm -f cluster-autoscaler-*

format:
	bash hack/update-gofmt.sh

image:
	docker build \
		--platform=linux/$(GOARCH) \
		--build-arg "GOARCH=$(GOARCH)" \
		--build-arg 'LDFLAGS=$(LDFLAGS_VALUE)' \
		-t $(IMAGE):$(TAG) \
		-f Dockerfile .
