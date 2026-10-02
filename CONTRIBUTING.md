# Contributing

The project welcomes contributions and suggestions. Most contributions require a Contributor License Agreement (CLA) that gives us the right to use your contribution. See [Microsoft's CLA](https://cla.microsoft.com) for details.

For pull requests to the Azure organization, the CLA bot checks whether you need to sign and provides instructions. You only need to sign once across repositories that use the Microsoft CLA.

The project follows the [Microsoft Open Source Code of Conduct](CODE_OF_CONDUCT.md). See the [FAQ](https://opensource.microsoft.com/codeofconduct/faq/) or contact [opencode@microsoft.com](mailto:opencode@microsoft.com) with questions. Report vulnerabilities through the [security policy](SECURITY.md), not public issues.

## Development

Use Go 1.26 or later, Git, Make and a C toolchain for race tests. Chart checks need Helm, and image builds need Docker.

Start from current `main` and keep changes focused. Reproduce the problem and add a test near the affected code. Follow nearby Go conventions and the [Go review guidance](https://go.dev/wiki/CodeReviewComments). Preserve license headers and source attribution.

Read the [architecture guide](docs/architecture.md) before changing discovery, node templates, scaling or module boundaries. Keep dependency pins unless the change needs a dependency update. The autoscaling core belongs in [kubernetes-sigs/cluster-autoscaler](https://github.com/kubernetes-sigs/cluster-autoscaler). The published API module remains in [kubernetes/autoscaler](https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler/apis).

From the repository root:

```sh
make format
hack/verify-boilerplate.sh
hack/verify-spelling.sh
make test-ci
make test-chart
git diff --check
```

Start with a focused test, then run the checks that apply before requesting review. The [testing guide](docs/testing.md) explains the targets and spelling tool. A root `go test ./...` does not enter the separate E2E module.

## Chart and E2E changes

Chart changes must pass `make test-chart`. Keep the [saved upstream renders](charts/testdata/azure-compatibility/README.md) as the comparison baseline. Do not regenerate them from the chart under test. If you change chart values or the README template, regenerate the chart README as described in the [chart development guide](charts/README.md).

The chart workflow also checks version increments and installation in kind. A local render check does not replace either check. Chart and application versions are separate release decisions.

Use `make test-e2e-local` without cloud credentials. Live E2Es need permission to use Azure, a disposable cluster and a cleanup plan. Follow the `test/README.md`, not the legacy AKS development targets. Keep credentials, kubeconfigs and private test artifacts out of commits.

## Pull requests

Use the issue and pull request templates. Describe the problem, the change and any compatibility impact. Include checks and results, and say what was not tested. Update affected docs and tests.

For live E2E results, record the test commit, the image's source commit and its digest. Results from another commit do not validate new code.
