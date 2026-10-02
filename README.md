# Cluster Autoscaler provider for Azure

The Azure provider runs [Cluster Autoscaler](https://github.com/kubernetes-sigs/cluster-autoscaler) on Kubernetes. It adds workers when Pods cannot be scheduled and deletes workers that are no longer needed.

## Status

The project is pre-release. There is no official container image yet. Build your own image and set the chart's `image.repository` and `image.tag`. The default image is a placeholder.

AKS compatibility work and deallocate mode are not included yet. The E2E suite uses a limited AKS test setup with Linux VMSS Uniform pools. It does not establish general AKS or Kubernetes version support. Scale-down uses Azure Delete operations. Do not run another autoscaler against the same pools.

## Build and test

Use Go 1.26 or later, Git and Make. Race tests also need a C toolchain. Helm is needed for chart tests.

```sh
make build
make test-ci
make test-chart
```

The build writes `cluster-autoscaler-<arch>` for Linux by default. Use `make build GOOS=darwin` for macOS or set `GOARCH` for another architecture. Local tests do not need Azure credentials. See [testing](docs/testing.md) for focused checks and [build metadata](docs/architecture.md#build-metadata) for version settings.

With Docker running, build an image locally:

```sh
make image IMAGE=cluster-autoscaler-azure TAG=dev
```

The command does not publish the image. Make your image available to the cluster before installing the chart.

## Install

Installation needs an Azure Kubernetes cluster, permission to manage its worker pools and a configured Azure identity. Read the [provider guide](pkg/cloudprovider/azure/README.md) and [chart guide](charts/cluster-autoscaler/README.md) first.

Create an `azure-values.yaml` file with your subscription, the resource group that contains the workers, authentication and either `autoDiscovery.clusterName` or `autoscalingGroups`. Set the image to one you built and made available to the cluster. From the repository root, render the chart before deploying:

```sh
helm template cluster-autoscaler charts/cluster-autoscaler \
  --namespace kube-system \
  -f azure-values.yaml \
  --set-string image.repository=YOUR_REGISTRY/cluster-autoscaler-azure \
  --set-string image.tag=YOUR_TAG
```

Review the rendered resources, then install on your Azure cluster:

```sh
helm upgrade --install cluster-autoscaler charts/cluster-autoscaler \
  --namespace kube-system \
  -f azure-values.yaml \
  --set-string image.repository=YOUR_REGISTRY/cluster-autoscaler-azure \
  --set-string image.tag=YOUR_TAG \
  --wait
```

Replace `YOUR_REGISTRY` and `YOUR_TAG` in both commands. Optional VPA settings need a separately installed Vertical Pod Autoscaler controller and CRDs.

## Code and docs

The shared core moved from [kubernetes/autoscaler](https://github.com/kubernetes/autoscaler) to [kubernetes-sigs/cluster-autoscaler](https://github.com/kubernetes-sigs/cluster-autoscaler). The Azure provider now lives in a separate repository, with the Go module path `github.com/Azure/cluster-autoscaler-provider-azure`. The application uses `sigs.k8s.io/cluster-autoscaler v0.0.0-k8s.v1.37.0` and Kubernetes 1.37.0 libraries. The library versions are not a cluster support statement.

| Path | Contents |
| --- | --- |
| [main.go](main.go) | Application entry point |
| [pkg/cloudprovider/azure](pkg/cloudprovider/azure) | Azure provider |
| [pkg/cloudprovider/router](pkg/cloudprovider/router) | Azure registration |
| [pkg/version](pkg/version) | Build version |
| [charts](charts/README.md) and [deploy](deploy) | Helm chart and deployment examples |
| [test](test) | Separate E2E module |
| [hack](hack) and [docs](docs) | Development scripts and guides |

Read [architecture](docs/architecture.md), [testing](docs/testing.md) and [source provenance](docs/provenance.md) for details. The `test/README.md` covers live tests. The [legacy development guide](deploy/dev/README.md) describes the limits of the older AKS setup.

## Contribute and get help

See [contributing](CONTRIBUTING.md), [support](SUPPORT.md) and the [security policy](SECURITY.md). The project follows the [Microsoft Open Source Code of Conduct](CODE_OF_CONDUCT.md). The code is licensed under [Apache 2.0](LICENSE).
