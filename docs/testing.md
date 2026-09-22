# Testing

## Local checks

Use Go 1.26 or later, Git and Make, plus a C toolchain for race-enabled tests.
These checks do not require Azure credentials or a Kubernetes cluster:

| Command from the root | What it checks |
| --- | --- |
| `make test-azure` | Race-enabled Azure provider unit and operation-boundary tests |
| `make test-unit` | Application build, Azure tests and race-enabled root-module tests |
| `make test-apis` | Tests in the separate local API module, including six-version informer integration |
| `make test-core-integration` | Pinned core's `TestStaticAutoscaler_FullLifecycle` and `TestScaleUp_ResourceLimits` with the race detector |
| `make test-ci` | All of the above, except the separately tagged Helm checks |
| `make test-chart` | Strict Helm lint and six frozen whole-resource comparisons; requires Helm on PATH |

On macOS, pass `GOOS=darwin` to `test-unit` or `test-ci`, because their build
prerequisite otherwise targets Linux. The root `go test ./...` does not enter
nested Go modules. `make test-ci` explicitly enters `apis` and tests the
pinned core dependency, but does not enter the E2E module.

For quick regression work:

```sh
go test ./cloudprovider/azure -run TestBuildAzureConfigMigrationPrecedence -count=1
go test ./cloudprovider/azure -run TestVMSSMigrationBoundaries -count=1
go test ./version -count=1
```

The informer checks use simple fake-client trackers. They cover shared caches
and typed event delivery, not API-server admission or the inherited apply-aware
fake-client path.

## Chart checks

The [oracle directory](../charts/testdata/azure-compatibility/README.md)
documents the pinned upstream source, Helm version, six cases, hashes and two
narrowly asserted expected-side exceptions: the managed-identity
existing-Secret reference correction and the top-level resource
`metadata.labels["helm.sh/chart"]` transition from
`cluster-autoscaler-9.59.0` to `cluster-autoscaler-9.59.1`.
Selectors, pod labels, other labels and functional fields remain compared;
there is no broader normalization.
Keep the captured YAML and its synthetic values under version control.
Do not regenerate the oracle from the chart under test.

[`pr.yaml`](../.github/workflows/pr.yaml) runs the compatibility comparison
before chart-testing lint/install for relevant changes. Local `make test-chart`
does not run chart-testing's version-increment check or its kind installation.
Chart changes must satisfy the existing version-increment gate. A local render
compatibility pass does not waive that gate or establish live qualification.

[`ca-test.yaml`](../.github/workflows/ca-test.yaml) runs `make test-ci`.
[`verify.yaml`](../.github/workflows/verify.yaml) runs the existing Go formatting,
boilerplate, lint and spelling scripts. None of these workflows provisions an
Azure acceptance environment.

Live execution is separate from developer checks. Record the test-source
commit, runtime image's source commit and digest, environment and scope for
each run. Results apply only to those identities and conditions, not to other
builds or configurations. For source attribution and compatibility scope,
see [provenance](provenance.md).
