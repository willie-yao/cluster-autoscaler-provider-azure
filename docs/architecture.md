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
add stopped-VM reuse or the AKS deallocate mode.

## AKS settings file

When `--config-path` is set, the application reads `nodeGroups` from the
mounted settings file before building the autoscaler. For example, AKS mounts
`/opt/conf/autoscaler/settings.json` with this content:

```json
{
  "nodeGroups": [
    {"name": "pool-vmss", "minSize": 0, "maxSize": 10, "scaleDownPolicy": "Delete"}
  ]
}
```

The settings file replaces `--nodes` while `--config-path` is set. If the file
is missing or invalid at startup, the application logs the error and starts
with no explicit node groups. It checks the file before each autoscaler loop
and exits with status 0 when the node groups change, so the pod can restart
with the new values. Other settings in the file do not change the node groups.
If `--config-path` is empty, `--nodes` and auto-discovery work as before.
Auto-discovery also remains active when it is set alongside `--config-path`.
The file path comes from the flag. The AKS fork instead always reads
`/opt/conf/autoscaler/settings.json`, so the two agree when AKS passes that
standard path.

The flags `--enable-force-delete`, `--enable-dynamic-instance-list` and
`--enable-detailed-cse-message` are accepted for AKS argument compatibility.
They have no effect.

## AKS node group specs

The Azure provider accepts `min:max:name` as before. It also accepts
`min:max:Delete:name:labels|taints`, where labels are a JSON object with string
values and taints use `key=value:effect`, separated by commas. The policy-only form
`min:max:Delete:name` also works. The settings file uses the same fields as
separate JSON properties and defaults the policy to `Delete`.

The provider rejects `Deallocate` in both the settings file and extended
specs. Deallocate would keep a stopped VM and its Node object, but the
unchanged autoscaler core does not support that flow. Silently treating it as
Delete would remove Nodes and VMs instead.

The reference AKS provider applies labels and taints from extended specs to
both VMs pools and VMSS templates. For VMSS pools, nonempty spec labels replace
labels from node template tags, and spec taints replace tag taints. Without
these fields, VMSS tags work as before. For VMs pools, spec labels override
matching agent pool labels, and spec taints are added to agent pool taints.

## Module and package boundaries

| Location | Responsibility |
| --- | --- |
| Root [go.mod](../go.mod) | Application module `k8s.io/autoscaler/cluster-autoscaler` and Azure adapter dependencies |
| [cloudprovider/azure](../cloudprovider/azure) | Azure provider, configuration, caches, node groups and Azure-client boundaries |
| [charts](../charts) | Deployment packaging and frozen render compatibility tests |
| [cloudprovider/azure/test](../cloudprovider/azure/test) | Separate Go module for the maintained E2E harness and scenarios |

The root module pins extracted core to
`sigs.k8s.io/cluster-autoscaler v0.0.0-k8s.v1.37.0`, sourced
from `kubernetes-sigs/cluster-autoscaler`, not the old autoscaler monorepo.
The CapacityBuffer, CapacityQuota and ProvisioningRequest APIs come from the
published `k8s.io/autoscaler/cluster-autoscaler/apis` module at the version
that core requires. Repository location and Go import identity are distinct.

Kubernetes dependencies are pinned to `1.37.0`. This source dependency pin
does not establish cluster-version support. The E2E module retains its own
dependency pins.

## Compatibility contracts

[`azure_config_test.go`](../cloudprovider/azure/azure_config_test.go) covers
default, file, legacy-field and environment precedence, including conflicting
authentication choices. The provider's runtime configuration is not derived
from the E2E JSON binding.

[`azure_scale_set_lifecycle_test.go`](../cloudprovider/azure/azure_scale_set_lifecycle_test.go)
exercises real provider methods with mocked Azure clients. It checks tagged
discovery, zero-size templates, bounded growth and the selected non-force
Delete request. Mock success is not proof of physical deletion.

[`azure_compatibility_test.go`](../charts/azure_compatibility_test.go) compares
six complete resource sets with frozen upstream renders. An empty render
cannot pass because a Deployment is required. The VPA-enabled fixture must
also contain a VerticalPodAutoscaler. The test allows two differences, which
are the managed identity existing Secret correction and the top-level
`metadata.labels["helm.sh/chart"]` label change from
`cluster-autoscaler-9.59.0` to `cluster-autoscaler-9.59.1`. Selectors, pod
labels, other labels and functional fields are still compared, as described
in the [saved renders README](../charts/testdata/azure-compatibility/README.md).

## E2E workload and observation path

```text
a disposable cluster runs one autoscaler
  -> test checks the controller and the two pools
  -> test creates Kubernetes workload demand
  -> real autoscaler scales workers
  -> test observes Azure instances, Nodes and workload state
  -> test removes its workloads and waits for baseline
  -> the cluster is deleted after the run
```

[`Config`](../cloudprovider/azure/test/pkg/environment/config.go) binds the
suite to an explicit kubeconfig, run ID, controller image, Azure scope and two
pools.
[`Environment`](../cloudprovider/azure/test/pkg/environment/environment.go)
checks that one autoscaler runs the expected image and reports exactly those
pools.

The [`Cloud` interface](../cloudprovider/azure/test/pkg/environment/azure.go)
has only reads. The harness cannot make a scale-up test pass by directly
resizing a VMSS. For example, the hostname anti-affinity spec in
[`public_test.go`](../cloudprovider/azure/test/suites/scaleup/public_test.go)
grows an anti-affinity workload from one to three replicas and requires three
distinct real workers across the two pools.

Desired capacity, cloud instances and Ready Nodes are separate observations.
[`observations.go`](../cloudprovider/azure/test/pkg/environment/observations.go)
requires identity and per-pool consistency for stable state. A pool above its
`max` tag becomes a terminal failure in
[`readSnapshot`](../cloudprovider/azure/test/suites/scaleup/suite_test.go);
ordinary read failures can retry while waiting for convergence. Negative
windows also require a fresh healthy controller. Read the
[E2E guide](../cloudprovider/azure/test/README.md) before any live run.

## Build metadata

The root [Makefile](../Makefile) shares one linker version setting between
binary and image builds: exact tag, otherwise SHA, otherwise `dev`. Automatic
Git versions add `-dirty` for unstaged tracked changes. Staged-only and untracked
changes do not add it, which matches upstream. Explicit `VERSION` is used
verbatim.
[`version_test.go`](../version/version_test.go) checks this in isolated Git
repositories.

For change-specific checks, see [testing](testing.md). For source attribution
and compatibility scope, see [provenance](provenance.md).
