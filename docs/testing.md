# Testing

## Local checks

Use Go 1.26 or later, Git, Make and a C toolchain for race tests. Run commands from the repository root. Local tests do not need Azure credentials or a Kubernetes cluster.

| Command | Checks |
| --- | --- |
| `make test-azure` | Azure provider tests with the race detector |
| `make test-unit` | Application build and root module tests with race detection and vet |
| `make test-core-integration` | Pinned core's in-memory integration tests with race detection |
| `make test-ci` | Root and core integration checks |
| `make test-chart` | Strict Helm lint and six saved render comparisons, with Helm on PATH |

The build creates a Linux binary by default. Tests still run on the host.

For focused tests:

```sh
go test ./pkg/cloudprovider/azure -run TestBuildAzureConfigPrecedence -count=1
go test ./pkg/cloudprovider/azure -run TestScaleSetDiscoveryScaleUpAndDelete -count=1
go test ./pkg/version -count=1
```

For boilerplate and spelling:

```sh
hack/verify-boilerplate.sh
hack/verify-spelling.sh
```

Spelling checks need `misspell` on PATH. If it is missing, install the tool used by [CI](../hack/install-verify-tools.sh):

```sh
go install github.com/golangci/misspell/cmd/misspell@latest
export PATH="$(go env GOPATH)/bin:$PATH"
```

## Chart checks

The [fixture guide](../charts/testdata/azure-compatibility/README.md) records the upstream source and six saved renders. The test allows the managed identity Secret override fix and the top-level chart version label change from 9.59.0 to 9.59.1. Other resource fields are compared. Do not regenerate the saved renders from the chart under test.

The [chart workflow](../.github/workflows/pr.yaml) runs `make test-chart`, chart-testing lint and kind installation for chart changes. It also checks that the README matches helm-docs output. See the [chart development guide](../charts/README.md#generate-the-readme) for generation. Local render checks do not check version increments or install the chart.

## CI

[ca-test.yaml](../.github/workflows/ca-test.yaml) runs `make test-ci`. [verify.yaml](../.github/workflows/verify.yaml) runs formatting, boilerplate, lint and spelling checks.
