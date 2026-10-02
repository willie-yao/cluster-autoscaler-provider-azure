ALL_ARCH = amd64 arm64 s390x
GO_TEST_DEFAULT_ANALYZERS?=all
TAG?=dev
LDFLAGS?=-s
ENVVAR=CGO_ENABLED=0
GOOS?=linux
GOARCH?=$(shell go env GOARCH)
IMAGE?=cluster-autoscaler-azure

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

VERSION_PKG := github.com/Azure/cluster-autoscaler-provider-azure/pkg/version
VERSION_LDFLAG := -X $(VERSION_PKG).ClusterAutoscalerVersion=$(VERSION)
LDFLAGS_VALUE := $(strip $(LDFLAGS) $(VERSION_LDFLAG))

.PHONY: all build test-azure test-unit test-ci test-chart test-core-integration benchmark clean format image

all: $(addprefix build-arch-,$(ALL_ARCH))

build: build-arch-$(GOARCH)

build-arch-%:
	$(ENVVAR) GOOS=$(GOOS) GOARCH=$* go build -o cluster-autoscaler-$* --ldflags="$(LDFLAGS_VALUE)"

test-azure:
	go test -race -vet="${GO_TEST_DEFAULT_ANALYZERS}" ./pkg/cloudprovider/azure/...

test-unit: build
	go test -race -vet="${GO_TEST_DEFAULT_ANALYZERS}" ./...

test-chart:
	go test -tags helm ./charts -count=1

test-core-integration:
	go test -race sigs.k8s.io/cluster-autoscaler/pkg/test/integration/inmemory -count=1

test-ci: test-unit test-core-integration

benchmark:
	go test ./... -bench=. -run='^$$' -vet="${GO_TEST_DEFAULT_ANALYZERS}"

clean: clean-arch-$(GOARCH)

clean-arch-%:
	rm -f cluster-autoscaler-$*

format:
	test -z "$$(find . -path ./vendor -prune -type f -o -name '*.go' -exec gofmt -s -d {} + | tee /dev/stderr)" || \
	test -z "$$(find . -path ./vendor -prune -type f -o -name '*.go' -exec gofmt -s -w {} + | tee /dev/stderr)"

image:
	docker build --pull \
		--platform=linux/$(GOARCH) \
		--build-arg "GOARCH=$(GOARCH)" \
		--build-arg 'LDFLAGS=$(LDFLAGS_VALUE)' \
		-t $(IMAGE):$(TAG) \
		-f Dockerfile .
