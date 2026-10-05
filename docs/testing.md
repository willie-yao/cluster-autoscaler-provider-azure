# Testing

## Local checks

Use Go 1.26 or later, Git, Make and a C toolchain for race tests. Run commands from the repository root. Local tests do not need Azure credentials or a Kubernetes cluster.

| Command | Checks |
| --- | --- |
| `make test-azure` | Azure provider tests with the race detector |
| `make test-unit` | Application build and root module tests with race detection and vet |
| `make test-ci` | Application build and root module tests |

The build creates a Linux binary by default. Tests still run on the host.

For focused tests:

```sh
go test ./pkg/cloudprovider/azure -run TestBuildAzureConfigPrecedence -count=1
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

With Helm and chart-testing available:

```sh
helm lint --strict charts/cluster-autoscaler
ct lint --chart-dirs charts --target-branch main --validate-maintainers=false
```

The [chart workflow](../.github/workflows/pr.yaml) runs chart-testing lint and kind installation for chart changes. It also checks that the README matches helm-docs output. See the [chart development guide](../charts/README.md#generate-the-readme) for generation. Local Helm lint does not check version increments or install the chart.

## CI

[ca-test.yaml](../.github/workflows/ca-test.yaml) runs `make test-ci`. [verify.yaml](../.github/workflows/verify.yaml) runs formatting, boilerplate, lint and spelling checks.
