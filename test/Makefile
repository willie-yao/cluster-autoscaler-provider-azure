REPO_ROOT:=$(shell git rev-parse --show-toplevel)
CAS_ROOT:=$(REPO_ROOT)
DEV_DIR:=$(CAS_ROOT)/deploy/dev
REGISTRY?=localhost
IMAGE?=$(REGISTRY)/cluster-autoscaler
TAG?=dev
GOARCH?=$(shell go env GOARCH)

CLUSTER_AUTOSCALER_NAMESPACE?=default
CLUSTER_AUTOSCALER_SERVICEACCOUNT_NAME?=cluster-autoscaler

# TEST_SUITE selects a specific test suite directory (e.g., TEST_SUITE=scaleup).
# When unset, all suites under suites/ are run.
TEST_SUITE?=
TEST_TIMEOUT?=75m
ENVIRONMENT?=

# Compute ginkgo suite path: ./suites/... (all) or ./suites/<name>/... (single)
SUITE_PATH=$(if $(TEST_SUITE),./suites/$(shell echo $(TEST_SUITE) | tr A-Z a-z)/...,./suites/...)
FOCUS?=
LABEL_FILTER?=
ARTIFACTS?=_artifacts

# --------------------------------------------------------------------------
# CI targets (build image and run against a CAPZ AKS cluster)
# --------------------------------------------------------------------------

.PHONY: build-e2e
build-e2e:
	$(MAKE) -C $(CAS_ROOT) make-image-arch-$(GOARCH) IMAGE=$(IMAGE) TAG=$(TAG)
	docker push $(IMAGE)-$(GOARCH):$(TAG)

.PHONY: test-e2e
test-e2e: build-e2e ## Build, push and deploy CAS on the prepared CAPZ cluster
	IMAGE="$(IMAGE)-$(GOARCH)" TAG="$(TAG)" ARTIFACTS="$(ARTIFACTS)" \
		TEST_SUITE="$(TEST_SUITE)" TEST_TIMEOUT="$(TEST_TIMEOUT)" bash hack/ci-e2e.sh test

# --------------------------------------------------------------------------
# Local development helpers
# --------------------------------------------------------------------------
# These helpers do not create the main/zero fixture for the E2E suite.
# See README.md for CAPZ setup or pass ENVIRONMENT=<JSON binding> to e2etests.

.PHONY: help
help: ## Display help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-25s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Cluster Setup (one-time)

.PHONY: setup-cluster
setup-cluster: ## Create AKS + ACR + workload identity for development
	cd $(DEV_DIR) && bash ./aks-dev-deploy.sh

##@ Build & Deploy

.PHONY: deploy-local
deploy-local: ## Build CAS and deploy to cluster via skaffold
	cd $(CAS_ROOT) && skaffold run --filename deploy/dev/skaffold.yaml

.PHONY: deploy-local-dev
deploy-local-dev: ## Build + deploy CAS in watch mode (auto-redeploy on changes)
	cd $(CAS_ROOT) && skaffold dev --filename deploy/dev/skaffold.yaml

##@ E2E Testing

.PHONY: e2etests
e2etests: ## Run E2Es with an explicit environment JSON binding
	@test -n "$(ENVIRONMENT)" || (echo "ENVIRONMENT is required"; exit 1)
	go run -mod=readonly github.com/onsi/ginkgo/v2/ginkgo \
		--tags e2e \
		-v --trace --fail-fast --fail-on-empty \
		--timeout $(TEST_TIMEOUT) \
		--output-dir "$(ARTIFACTS)" \
		--junit-report="junit.e2e_suite.1.xml" \
		--json-report="report.e2e_suite.1.json" \
		$(if $(FOCUS),--focus="$(FOCUS)",) \
		$(if $(LABEL_FILTER),--label-filter="$(LABEL_FILTER)",) \
		$(SUITE_PATH) -- \
		-environment="$(ENVIRONMENT)"

.PHONY: test-local
test-local: ## Check the library and spec registration without Azure
	go test -mod=readonly -race ./pkg/...
	go test -mod=readonly -tags=e2e ./suites/... -run '^TestScaleUp$$' -count=1 -args -ginkgo.dry-run

##@ Utilities

.PHONY: list-suites
list-suites: ## List available test suites
	@find suites -mindepth 1 -maxdepth 1 -type d -printf '%f\n' 2>/dev/null || echo "No suites found."

.PHONY: validate-env
validate-env: ## Check required environment variables
	@missing=""; \
	for var in AZURE_SUBSCRIPTION_ID AZURE_RESOURCE_GROUP; do \
		eval val=\$$$$var; \
		if [ -z "$$val" ]; then missing="$$missing $$var"; fi; \
	done; \
	if [ -n "$$missing" ]; then \
		echo "ERROR: Missing required environment variables:$$missing"; \
		exit 1; \
	fi; \
	echo "All required environment variables are set."
