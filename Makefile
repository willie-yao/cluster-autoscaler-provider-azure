ALL_ARCH = amd64 arm64 s390x
GO_TEST_DEFAULT_ANALYZERS?=all
TAG?=dev
LDFLAGS?=-s
ENVVAR=CGO_ENABLED=0
GOOS?=linux
GOARCH?=$(shell go env GOARCH)
REGISTRY?=localhost
IMAGE=$(REGISTRY)/cluster-autoscaler$(PROVIDER)

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

.PHONY: all build test-azure test-unit test-ci benchmark clean format make-image

all: $(addprefix build-arch-,$(ALL_ARCH))

build: build-arch-$(GOARCH)

build-arch-%: clean-arch-%
	$(ENVVAR) GOOS=$(GOOS) GOARCH=$* go build -o cluster-autoscaler-$* --ldflags="$(LDFLAGS_VALUE)"

test-azure:
	go test -race -vet="${GO_TEST_DEFAULT_ANALYZERS}" ./pkg/cloudprovider/azure/...

test-unit: clean build
	go test -race -vet="${GO_TEST_DEFAULT_ANALYZERS}" ./...

test-ci: test-unit

benchmark:
	go test ./... -bench=. -run='^$$' -vet="${GO_TEST_DEFAULT_ANALYZERS}"

clean: clean-arch-$(GOARCH)

clean-arch-%:
	rm -f cluster-autoscaler-$*

format:
	test -z "$$(find . -path ./vendor -prune -type f -o -name '*.go' -exec gofmt -s -d {} + | tee /dev/stderr)" || \
	test -z "$$(find . -path ./vendor -prune -type f -o -name '*.go' -exec gofmt -s -w {} + | tee /dev/stderr)"

make-image: make-image-arch-$(GOARCH)

make-image-arch-%:
	GOOS=$(GOOS) docker buildx build --pull --platform linux/$* \
		--build-arg "GOARCH=$*" \
		--build-arg 'LDFLAGS=${LDFLAGS_VALUE}' \
		--build-arg 'BUILD_TAGS=${BUILD_TAGS}' \
		-t ${IMAGE}-$*:${TAG} \
		-f Dockerfile .
	@echo "Image ${TAG}${FOR_PROVIDER}-$* completed"
