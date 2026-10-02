# Helm chart

The [cluster-autoscaler chart](cluster-autoscaler/README.md) installs the Azure provider from this repository. It is based on the upstream Cluster Autoscaler chart and supports only Azure. It is not a published chart repository.

There is no official image yet. Set `image.repository` and `image.tag`, configure Azure authentication and select worker pools. See the [installation guide](../README.md#install) and [chart values](cluster-autoscaler/values.yaml).

## Development checks

From the repository root, with Helm available:

```sh
make test-chart
```

The target runs strict Helm lint and compares resources with [saved upstream renders](testdata/azure-compatibility/README.md). Do not regenerate those renders from the chart under test.

The [chart workflow](../.github/workflows/pr.yaml) also runs chart-testing lint, checks version increments and installs changed charts in kind.

## Generate the README

Edit [README.md.gotmpl](cluster-autoscaler/README.md.gotmpl) or the comments in `values.yaml`, not the generated chart README.

The chart workflow uses helm-docs v1.14.2 and fails if generation changes tracked files. From the repository root:

```sh
go run github.com/norwoodj/helm-docs/cmd/helm-docs@v1.14.2
git diff -- charts/cluster-autoscaler/README.md
```

Commit the generated README with its source changes. The legacy pre-commit configuration pins an older helm-docs version, so use the CI version above.
