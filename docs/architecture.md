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

Ordinary scale-down uses Azure Delete operations by default. The experimental
[provider-only deallocate mode](#provider-only-deallocate) parks and reuses
supported self-managed Uniform VMSS instances. It doesn't match the AKS
deallocate mode.

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
`min:max:policy:name:labels|taints`, where policy is `Delete` or `Deallocate`,
labels are a JSON object with string values, and taints use
`key=value:effect`, separated by commas. The policy-only form
`min:max:policy:name` also works. The settings file uses the same fields as
separate JSON properties and defaults the policy to `Delete`.

The provider uses `Deallocate` in either input to opt a supported VMSS group
into [provider-only deallocation](#provider-only-deallocate). It logs the
eligibility error and skips an unsupported group, including in auto-discovery,
while other groups continue to autoscale. A malformed `--nodes` spec still
causes a startup error. An invalid settings file follows the startup behavior
described above.

The reference AKS provider applies labels and taints from extended specs to
both VMs pools and VMSS templates. For VMSS pools, nonempty spec labels replace
labels from node template tags, and spec taints replace tag taints. Without
these fields, VMSS tags work as before. For VMs pools, spec labels override
matching agent pool labels, and spec taints are added to agent pool taints.

## Provider-only deallocate

This optional mode is in
[`azure_provider_only_deallocate.go`](../cloudprovider/azure/azure_provider_only_deallocate.go).
It changes only the Azure provider. The core still picks, drains and cleans up
nodes as it does in Delete mode. The
[provider guide](../cloudprovider/azure/README.md#provider-only-deallocate-experimental)
lists the settings and requirements.

A group uses the mode when its spec has the `Deallocate` policy or when the
global `providerOnlyDeallocate` setting is on. The global setting applies to
every VMSS group, even one whose spec says `Delete`. A `Deallocate` spec for a
group that can't be parked skips the group. Only self-managed Uniform VMSS
groups with regular-priority VMs and managed OS disks that aren't ephemeral
can be parked.

**Park.** When the core scales down a registered node, the provider checks the
Node and its VM, writes a deletion receipt to the Node as an annotation, and
sends one Deallocate request. The receipt holds the Node name and UID, the
provider ID and the VM ID. The provider holds the group's lock only while it
checks the nodes and sends the requests, so a scale-up of the same group
doesn't wait for a deallocation. While the VM deallocates, the provider reports
it as deleting and leaves it out of the target size. When Azure reports the VM
deallocated, the provider deletes the Node with a UID precondition. The parked
VM stays in the scale set with its OS disk. The provider doesn't report it to
the core, and the target size is the VMSS capacity minus the parked and
deallocating VMs.

If Azure rejects the request, including when it throttles it, the provider
removes the receipt and the scale-down fails. If the result is unclear, the
receipt stays. Before each loop, the provider looks for Nodes with a receipt
and deletes each one once its VM is deallocated with the same VM ID. It never
sends Deallocate again, uses a different Node UID or removes finalizers. A
receipt on a running VM blocks that node's scale-down until an operator
removes the annotation. Errors in this step are logged and don't stop the
loop.

**Reuse.** `IncreaseSize` starts parked VMs before it adds capacity. It skips a
VM that is still being parked, and refuses a parked VM that still has a Node
with the same provider ID or name. It returns once Azure accepts each Start,
and watches each Start in the background. An accepted Start counts toward the
target size even when its result is unclear. The restarted VM registers a new
Node with a new UID, and the core treats it as a new node.

**Failed retention.** If a started or new VM fails, or never registers, the
core's cleanup calls `DeleteNodes` or `ForceDeleteNodes` with a node that has no
UID. The provider deallocates that VM instead of deleting it, so the VM and its
disk stay for a later Start. The VM counts as deleting until the deallocation
finishes. `ForceDeleteNodes` skips the minimum size check but still
deallocates.

**Inventory.** `Nodes`, `TargetSize` and `HasInstance` read power states from
the scale set's instance cache. The cache is read again after its TTL, when its
size differs from the VMSS capacity, and after each finished power operation.
A park, Start, cleanup or receipt recovery lists the VMs again first. Until
Azure shows an accepted operation, the provider keeps its expected power state
by VM ID.

**Node deletion.** Deleting the old Node lets the unchanged core see the
returning VM as a new registration, with the usual node startup and readiness
checks. The cost is that labels, annotations, taints and cordons set only on
the old Node are lost, and the VM must handle a new PodCIDR. The autoscaler
also needs `delete` permission on Nodes.

The provider keeps its expected power states and its running parks, Starts
and cleanups in memory only. After a restart it reads the power states from
Azure again and finishes Node deletes from receipts. An operation that Azure
accepted but doesn't show yet counts by its last shown power state until the
instance view catches up.

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
changes do not add it, which matches upstream. Explicit `VERSION` is used
verbatim.
[`version_test.go`](../version/version_test.go) checks this in isolated Git
repositories.

For change-specific checks, see [testing](testing.md). For source attribution
and compatibility scope, see [provenance](provenance.md).
