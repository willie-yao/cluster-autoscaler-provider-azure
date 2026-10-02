# Architecture

## Runtime

```text
main.go
  -> Kubernetes clients, informers and controller-runtime manager
  -> shared core's autoscaler builder and loop
  -> Azure provider registration
  -> Azure node group operations
```

[`mustBuildAutoscaler`](../main.go) passes the clients, informers and manager to the core's builder. The scheduling simulation and autoscaling loop come from `sigs.k8s.io/cluster-autoscaler`, not a local copy.

[`pkg/cloudprovider/router`](../pkg/cloudprovider/router/router.go) registers only Azure. [`azure_cloud_provider.go`](../pkg/cloudprovider/azure/azure_cloud_provider.go) builds the provider, which discovers groups, creates node templates and requests scale changes. Scale-down deletes instances. AKS compatibility work and deallocate mode are not included.

## Packages and modules

| Path | Purpose |
| --- | --- |
| [go.mod](../go.mod) | Application module `github.com/Azure/cluster-autoscaler-provider-azure` |
| [pkg/cloudprovider/azure](../pkg/cloudprovider/azure) | Provider configuration, caches, node groups and Azure clients |
| [pkg/cloudprovider/router](../pkg/cloudprovider/router) | Azure registration |
| [pkg/version](../pkg/version) | Build version |
| [charts](../charts/README.md) and [deploy](../deploy) | Packaging and deployment examples |
| [test](../test) | Separate E2E module |

The root module pins the shared core to `sigs.k8s.io/cluster-autoscaler v0.0.0-k8s.v1.37.0` and Kubernetes libraries to 1.37.0. Library versions do not establish cluster support. The CapacityBuffer, CapacityQuota and ProvisioningRequest APIs still come from `k8s.io/autoscaler/cluster-autoscaler/apis`, at the version the core requires. The E2E module has its own dependency pins.

## Tests

[`azure_config_test.go`](../pkg/cloudprovider/azure/azure_config_test.go) checks defaults, file settings, legacy fields and environment overrides. [`azure_scale_set_lifecycle_test.go`](../pkg/cloudprovider/azure/azure_scale_set_lifecycle_test.go) checks discovery, empty-pool templates, growth and Delete requests with mocked Azure clients. Those tests do not prove that cloud instances were deleted.

[`azure_compatibility_test.go`](../charts/azure_compatibility_test.go) compares six chart renders with saved upstream resources. It allows the managed identity Secret override fix and the chart version label change. See the [fixture guide](../charts/testdata/azure-compatibility/README.md) for the baseline.

## Build metadata

The root [Makefile](../Makefile) uses the same linker version for binaries and images. It uses an exact Git tag, otherwise the commit SHA, otherwise `dev`. Automatic Git versions add `-dirty` for unstaged changes to tracked files. Staged-only and untracked changes do not add the suffix. An explicit `VERSION` is used unchanged.

[`version_test.go`](../pkg/version/version_test.go) checks version settings in temporary Git repositories.

See [testing](testing.md) for commands, [source provenance](provenance.md) for attribution and the [provider guide](../pkg/cloudprovider/azure/README.md) for configuration.
