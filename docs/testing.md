# Testing

## Local checks

Use Go 1.26 or later, Git and Make, plus a C toolchain for race-enabled tests.
These checks do not require Azure credentials or a Kubernetes cluster:

| Command from the root | What it checks |
| --- | --- |
| `make test-azure` | Race-enabled Azure provider tests |
| `make test-unit` | Application build, Azure tests and race-enabled root-module tests |
| `make test-core-integration` | Pinned core's in-memory integration tests with the race detector |
| `make test-ci` | All of the above, except the separately tagged Helm checks |
| `make test-chart` | Strict Helm lint and six frozen whole-resource comparisons; requires Helm on PATH |

The build step of `test-unit` makes a Linux binary by default, and the tests
still run on the host platform. The root `go test ./...` does not enter
nested Go modules. `make test-ci` also tests the pinned core dependency, but
does not enter the E2E module.

For quick regression work:

```sh
go test ./cloudprovider/azure -run TestBuildAzureConfigPrecedence -count=1
go test ./cloudprovider/azure -run TestScaleSetDiscoveryScaleUpAndDelete -count=1
go test ./version -count=1
```

## Chart checks

The [saved renders README](../charts/testdata/azure-compatibility/README.md)
documents the pinned upstream source, Helm version, six cases and hashes. The
test allows two differences from the saved upstream renders. First, the
managed identity setting reads the existing Secret named by
`secretKeyRefNameOverride`. Second, the top-level
`metadata.labels["helm.sh/chart"]` label changes from
`cluster-autoscaler-9.59.0` to `cluster-autoscaler-9.59.1`. Selectors, pod
labels, other labels and functional fields are still compared.
Keep the saved YAML and its synthetic values under version control.
Do not regenerate the saved renders from the chart under test.

[`pr.yaml`](../.github/workflows/pr.yaml) runs the compatibility comparison
before chart-testing lint/install for relevant changes, and it checks that the
chart README matches the helm-docs output. Local `make test-chart`
does not run chart-testing's version-increment check or its kind installation.
Chart changes must satisfy the existing version-increment gate. A passing local
render comparison does not waive that gate, and it does not show that the chart
works in a live cluster.

[`ca-test.yaml`](../.github/workflows/ca-test.yaml) runs `make test-ci`.
[`verify.yaml`](../.github/workflows/verify.yaml) runs the existing Go formatting,
boilerplate, lint and spelling scripts. None of these workflows provisions an
Azure acceptance environment.

Live execution is separate from developer checks. Record the test-source
commit, runtime image's source commit and digest, environment and scope for
each run. Results apply only to those identities and conditions, not to other
builds or configurations. For source attribution and compatibility scope,
see [provenance](provenance.md).
