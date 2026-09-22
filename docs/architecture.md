# Architecture and development

## Runtime path

```text
main.go
  -> Kubernetes clients, shared informers and controller-runtime manager
  -> extracted-core autoscaler builder and loop
  -> registered Azure provider
  -> Azure node-group operations
```

[`mustBuildAutoscaler`](../main.go) supplies the Kubernetes client, informer
factory and manager to `autoscalerbuilder.New`. The builder, scheduling
simulation and autoscaler loop come from `sigs.k8s.io/cluster-autoscaler`.
This application does not maintain a second copy of the core algorithm.

[`cloudprovider/router`](../cloudprovider/router/router.go) imports only Azure
for registration. [`azure_cloud_provider.go`](../cloudprovider/azure/azure_cloud_provider.go)
connects that registration to `BuildAzure`. The Azure adapter discovers groups,
builds node templates and issues scaling operations; configuration and operator
examples are described in the [provider guide](../cloudprovider/azure/README.md).

Ordinary scale-down uses Azure Delete operations. This source tree does not
add stopped-VM reuse or AKS deallocate-mode behavior.

## Module and package boundaries

| Location | Responsibility |
| --- | --- |
| Root [go.mod](../go.mod) | Application module `k8s.io/autoscaler/cluster-autoscaler` and Azure adapter dependencies |
| [apis](../apis) | Local module for CapacityBuffer, CapacityQuota and ProvisioningRequest APIs, generated clients and informers |
| [cloudprovider/azure](../cloudprovider/azure) | Azure provider, configuration, caches, node groups and Azure-client boundaries |
| [charts](../charts) | Deployment packaging and frozen render compatibility tests |
| [cloudprovider/azure/test](../cloudprovider/azure/test) | Separate Go module for the maintained E2E harness and scenarios |

The root module replaces `k8s.io/autoscaler/cluster-autoscaler/apis` with
`./apis`. It pins extracted core to
`sigs.k8s.io/cluster-autoscaler v0.0.0-20260903143621-3d1c7137cdac`, sourced
from `kubernetes-sigs/cluster-autoscaler`, not the old autoscaler monorepo.
Repository location and Go import identity are distinct.

Kubernetes dependencies are pinned to `1.37.0-rc.1`. This source dependency pin
does not establish cluster-version support. Nested modules retain their own
dependency pins.

## Compatibility contracts

[`azure_config_test.go`](../cloudprovider/azure/azure_config_test.go) covers
default, file, legacy-field and environment precedence, including conflicting
authentication choices. The provider's runtime configuration is not derived
from the E2E JSON binding.

[`informers_test.go`](../apis/integration/informers_test.go) checks six API
versions: scheme round trips, typed events, namespace indexing and shared
typed/legacy caches. These fake-client checks do not establish API-server
admission or server-side apply support.

[`azure_migration_test.go`](../cloudprovider/azure/azure_migration_test.go)
exercises real provider methods with mocked Azure clients. It checks tagged
discovery, zero-size templates, bounded growth and the selected non-force
Delete request. Mock success is not proof of physical deletion.

[`azure_compatibility_test.go`](../charts/azure_compatibility_test.go) compares
six complete resource sets with frozen upstream renders. An empty render
cannot pass because a Deployment is required. The VPA-enabled fixture must
also contain a VerticalPodAutoscaler. The sole normalized field is the
managed-identity existing-Secret reference described in the
[oracle provenance](../charts/testdata/azure-compatibility/README.md).

## E2E workload and observation path

```text
operator prepares and authorizes fixture
  -> test checks JSON binding, ownership, controller and capacity bounds
  -> test creates Kubernetes workload demand
  -> real autoscaler scales workers
  -> test observes Azure instances, Nodes and workload state
  -> test removes owned workloads and waits for baseline
  -> operator tears down infrastructure
```

[`Config`](../cloudprovider/azure/test/pkg/environment/config.go) binds the
suite to an explicit kubeconfig/context, `kube-system` Namespace UID, run ID,
controller/image, Azure scope and two pools.
[`Environment`](../cloudprovider/azure/test/pkg/environment/environment.go)
checks this binding and the operator-created marker. The marker expresses
opt-in and consistency, not independent security authorization.

The [`Cloud` interface](../cloudprovider/azure/test/pkg/environment/azure.go)
has only reads. The harness cannot make a scale-up test pass by directly
resizing a VMSS. For example, `CA-005` in
[`public_test.go`](../cloudprovider/azure/test/suites/scaleup/public_test.go)
grows an anti-affinity workload from one to three replicas and requires three
distinct real workers across the owned pools.

Desired capacity, cloud instances and Ready Nodes are separate observations.
[`observations.go`](../cloudprovider/azure/test/pkg/environment/observations.go)
requires identity and per-pool consistency for stable state. Known bound
breaches become terminal failures in
[`readSnapshot`](../cloudprovider/azure/test/suites/scaleup/suite_test.go);
ordinary read failures can retry while waiting for convergence. A partial
instance list can prove an upper-bound breach, not a lower-bound violation.
Negative windows also require a fresh healthy controller.

The fixture's four-VM/eight-vCPU checks are sampled guards, not a spending
interlock or a statement of all provider capabilities. Read the
[operator guide](../cloudprovider/azure/test/README.md) before any live run.

## Build metadata

The root [Makefile](../Makefile) shares one linker version setting between
binary and image builds: exact tag, otherwise SHA, otherwise `dev`. Automatic
Git versions add `-dirty` for unstaged tracked changes. Staged-only and untracked
changes do not add it, matching upstream behavior rather than defining a
stronger source-attestation policy. Explicit `VERSION` is used verbatim.
[`version_test.go`](../version/version_test.go) checks this in isolated Git
repositories.

For change-specific checks, see [testing](testing.md). For source attribution
and compatibility scope, see [provenance](provenance.md).
