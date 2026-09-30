REPO_ROOT:=$(shell git rev-parse --show-toplevel)
CAS_ROOT:=$(REPO_ROOT)
DEV_DIR:=$(CAS_ROOT)/cloudprovider/azure/examples/dev

CLUSTER_AUTOSCALER_NAMESPACE?=default
CLUSTER_AUTOSCALER_SERVICEACCOUNT_NAME?=cluster-autoscaler

# TEST_SUITE selects a specific test suite directory (e.g., TEST_SUITE=scaleup).
TEST_SUITE?=scaleup
TEST_TIMEOUT?=3h
PROW_TEST_TIMEOUT?=4h
ENVIRONMENT?=

# Compute the selected suite path.
SUITE_PATH=$(if $(TEST_SUITE),./suites/$(shell echo $(TEST_SUITE) | tr A-Z a-z)/...,./suites/...)
FOCUS?=
LABEL_FILTER?=
ARTIFACTS?=_artifacts
IMAGE?=$(REGISTRY)/cluster-autoscaler-azure
TAG?=

# --------------------------------------------------------------------------
# E2E and legacy AKS development targets
# --------------------------------------------------------------------------
# E2E workflows:
#   Prow: CAPZ creates an AKS cluster from templates/cluster-template-prow-aks-aso-e2e.yaml,
#   then test-e2e deploys CAS and writes the JSON binding.
#   Manual: prepare a cluster as README.md describes, then pass
#   ENVIRONMENT=/absolute/path/to/environment.json to e2etests.
#   The legacy AKS targets don't prepare a cluster for the suite.

.PHONY: help
help: ## Display help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-25s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Legacy AKS development setup (not the E2E setup)

.PHONY: setup-cluster
setup-cluster: ## Legacy: create AKS + ACR + workload identity
	cd $(DEV_DIR) && bash ./aks-dev-deploy.sh

##@ Legacy AKS build and deployment

.PHONY: deploy-local
deploy-local: ## Legacy: build and deploy CAS with the AKS Skaffold configuration
	cd $(CAS_ROOT) && skaffold run --filename cloudprovider/azure/examples/dev/skaffold.yaml

.PHONY: deploy-local-dev
deploy-local-dev: ## Legacy: watch and redeploy CAS with the AKS Skaffold configuration
	cd $(CAS_ROOT) && skaffold dev --filename cloudprovider/azure/examples/dev/skaffold.yaml

##@ E2E workflow

.PHONY: build-e2e
build-e2e: ## Build and push the candidate image for a CAPZ AKS Prow cluster
	IMAGE="$(IMAGE)" TAG="$(TAG)" bash hack/ci-e2e.sh build

.PHONY: test-e2e
test-e2e: build-e2e ## Deploy CAS on the CAPZ AKS cluster and run the specs of one label filter
	IMAGE="$(IMAGE)" TAG="$(TAG)" ARTIFACTS="$(ARTIFACTS)" \
		LABEL_FILTER="$(if $(LABEL_FILTER),$(LABEL_FILTER),smoke)" TEST_TIMEOUT="$(PROW_TEST_TIMEOUT)" \
		CLUSTER_AUTOSCALER_NAMESPACE="$(CLUSTER_AUTOSCALER_NAMESPACE)" \
		CLUSTER_AUTOSCALER_SERVICEACCOUNT_NAME="$(CLUSTER_AUTOSCALER_SERVICEACCOUNT_NAME)" \
		TEST_SUITE="$(TEST_SUITE)" bash hack/ci-e2e.sh test

.PHONY: e2etests
e2etests: ## Run E2Es on a prepared cluster with CAS deployed (ENVIRONMENT=<JSON binding>)
	@test -n "$(ENVIRONMENT)" || (echo "ENVIRONMENT must name an explicit JSON binding"; exit 1)
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
test-local: ## Validate harness logic and E2E registration without cloud credentials
	go test -mod=readonly -race ./pkg/...
	go test -mod=readonly -race -tags=e2e ./suites/... -skip '^TestScaleUp$$'
	go test -mod=readonly -tags=e2e ./suites/... -run '^$$'
	go test -mod=readonly -tags=e2e ./suites/... -run '^TestScaleUp$$' -count=1 -args -ginkgo.dry-run

##@ Current suite utilities

.PHONY: list-suites
list-suites: ## List available test suites
	@find suites -mindepth 1 -maxdepth 1 -type d -printf '%f\n' 2>/dev/null || echo "No suites found."

##@ Legacy AKS environment checks

.PHONY: validate-env
validate-env: ## Legacy: check AKS environment variables, not the E2E JSON binding
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
