# Cluster Autoscaler Provider Azure

An Azure-only Cluster Autoscaler application for Kubernetes. It connects the
shared autoscaling core to the Azure cloud provider, growing worker pools for
unschedulable workloads and removing unneeded workers through ordinary Azure
Delete-mode operations.

The executable lives at the repository root and registers only Azure.
`--cloud-provider` defaults to `azure`. The autoscaling algorithm comes from
the pinned [extracted core](https://github.com/kubernetes-sigs/cluster-autoscaler);
this repository owns the application wiring, Azure adapter, chart and tests.
It does not implement AKS deallocate-mode parity.

## Build and image configuration

Use Go 1.26 or later, Git and Make:

```sh
make build
```

This writes `cluster-autoscaler-<arch>`, defaulting to Linux and the host Go
architecture. For a native macOS binary, use `make build GOOS=darwin`.
`GOARCH` selects the target architecture.

With Docker available, build a local image without publishing it:

```sh
make image IMAGE=cluster-autoscaler-azure TAG=dev
```

Set `IMAGE` and `TAG` for the image name you intend to use. Automatic build
versions use an exact Git tag, otherwise the commit SHA, with upstream's
`-dirty` suffix for unstaged tracked changes. Explicit `VERSION` overrides are
used verbatim. See [build metadata](docs/architecture.md#build-metadata).

Deployment examples use placeholders. Supply an image built from the source
you intend to run and made available to your cluster.

## Configure and deploy

The [Azure provider guide](cloudprovider/azure/README.md) covers authentication,
VMSS discovery, explicit node groups, node-template tags and deployment examples.
Use one authentication method and scope the controller to the intended worker
pools. Do not run competing autoscalers against the same pools.

The Helm chart is at [charts/cluster-autoscaler](charts/cluster-autoscaler).
Configure authentication, Azure subscription/resource group and either
`autoDiscovery.clusterName` or `autoscalingGroups` in an operator-owned values
file. Set both `image.repository` and `image.tag`; the default repository is
a placeholder, not a runnable published image.

For example, after creating `azure-values.yaml`, render without deploying:

```sh
helm template cluster-autoscaler charts/cluster-autoscaler \
  --namespace kube-system \
  -f azure-values.yaml \
  --set-string image.repository=YOUR_REGISTRY/cluster-autoscaler \
  --set-string image.tag=YOUR_TAG
```

Review [chart values](charts/cluster-autoscaler/values.yaml) and the rendered
resources before deployment. Optional `vpa` settings manage the autoscaler
Pod's resources and require a separately installed VPA controller and CRDs.

## Development

```sh
make test-azure                 # Azure unit and boundary tests
make test-ci                    # All local Go checks, including nested modules
make test-chart                 # Requires Helm
```

On macOS, run `make test-ci GOOS=darwin`. Race-enabled tests need a working C
toolchain. Local E2E checks compile and dry-run the suite without Azure
credentials; live E2Es require a separately authorized disposable fixture.

Start with [CONTRIBUTING.md](CONTRIBUTING.md),
[architecture](docs/architecture.md) and [testing](docs/testing.md).
The [E2E operator guide](cloudprovider/azure/test/README.md) describes fixture
ownership and execution. [Source provenance](docs/provenance.md) records source
pins and compatibility scope.

## Source and license

The application is derived from `kubernetes/autoscaler` and retains its
`k8s.io/autoscaler/cluster-autoscaler` Go module identity and local `./apis`
replacement. Existing upstream history and attribution are retained.
See [LICENSE](LICENSE).
