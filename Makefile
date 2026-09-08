ALL_ARCH ?= amd64 arm64 s390x
TAG ?= dev
LDFLAGS ?= -s
GOOS ?= linux
GOARCH ?= $(shell go env GOARCH)
IMAGE ?= cluster-autoscaler-azure

VERSION_PKG := k8s.io/autoscaler/cluster-autoscaler/version
VERSION ?= $(shell git describe --exact-match --tags 2>/dev/null | sed -e 's|^cluster-autoscaler-||')
ifeq ($(strip $(VERSION)),)
  VERSION := $(shell git rev-parse HEAD 2>/dev/null || echo dev)
endif
VERSION_LDFLAG := -X $(VERSION_PKG).ClusterAutoscalerVersion=$(VERSION)
LDFLAGS_VALUE := $(strip $(LDFLAGS) $(VERSION_LDFLAG))

.PHONY: all build build-arch test-azure test-unit test-ci test-apis clean format image

all: build

build: build-arch-$(GOARCH)

build-arch-%:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$* go build -o cluster-autoscaler-$* --ldflags="$(LDFLAGS_VALUE)"

test-azure:
	go test -race ./cloudprovider/azure/...

test-unit: build test-azure
	go test -race ./...

test-apis:
	cd apis && go test ./...

test-ci: test-unit test-apis

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
