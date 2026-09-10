# Opt-in Deallocate for Uniform VMSS

This first delivery retains a VM and its managed disks when autoscaling down.
Scale-up starts retained VMs before increasing physical VMSS capacity.
It requires the matching shared-core accounting and deletion-taint interfaces.
No released dependency pin or published image includes this integration yet.
The default chart image is unchanged and must not be assumed to support it.

## Configuration

Merge [cloud-config.json](cloud-config.json) into your existing Azure cloud
configuration without replacing authentication, subscription or resource-group
settings. Policy keys are node group names, matched case-insensitively. Values
must be exactly `Delete` or `Deallocate`; omitted groups remain `Delete`.
Duplicate keys, including names differing only by case, are rejected.

The same map controls explicit `--nodes` and tag-discovered VMSS groups. It
does not select groups or replace their min/max configuration. Keep your
existing explicit group flags or discovery selector.

Set `--cordon-node-before-terminating=false`. The CLI default is true, which
is rejected for Deallocate before mutation. This is deliberately taint-only:
the provider does not cordon or uncordon Nodes, including Nodes already cordoned
by an administrator.

Supported groups are non-hosted Uniform VMSS with regular-priority VMs and
managed, non-ephemeral OS disks. Set `strictCacheUpdates` to `false`.
Flexible orchestration, Spot/low-priority VMs, dedicated hosts/HostGroup,
AKS-managed VMSS, standard AgentPool and native AKS VMPool backends are unsupported.
Atomic provisioning and `ZeroOrMaxNodeScaling` are unsupported.

VM power state is not a scale-down policy. This configuration does not discover
or alter a built-in compute VMSS Delete/Deallocate setting.

## Kubernetes permissions and Helm

Azure CA writes only its owned `Suspended` condition using `nodes/status` updates.
Its service account needs `update` on `nodes/status` in addition to existing
Node get/list/watch/update permissions.

The [values.yaml](values.yaml) overlay uses existing chart `extraArgs`,
`extraVolumes`, `extraVolumeMounts` and `rbac.additionalRules`. Prepare the
existing `cluster-autoscaler-azure-config` Secret with an `azure.json` key
containing your merged cloud config, or adapt the overlay to an existing mount.
Preserve your existing authentication values and adapt the example's explicit
group selection, or remove `autoscalingGroups` when using autodiscovery. Helm lists
replace, rather than merge, any pre-existing additional rules or mounts, so
carry those entries into your combined values.

Local rendering from the repository root:

```sh
helm lint charts/cluster-autoscaler \
  -f cloudprovider/azure/examples/deallocate/values.yaml
helm template cluster-autoscaler charts/cluster-autoscaler \
  -f cloudprovider/azure/examples/deallocate/values.yaml
```

This example does not change chart defaults, authentication or image versions.
Do not deploy the rendered example with an image lacking the matching core
and provider changes.

## Accounting and safety

Min/max constrain active target, not physical VMSS capacity. Parked VMs and
retiring VMs remain members but do not count as active capacity. An accepted
restart consumes active target while unavailable. Retained managed disks
continue to incur storage charges.

For example, physical capacity 10 with four retiring VMs has active target 6.
One more unit of demand while those stops are still in flight requests physical
capacity 11, not 7. Only successfully parked instances can be restarted.
Accepted Starts survive a later capacity-update rejection. Decreasing target
after a timeout performs a read-only inventory reconciliation, never a physical
shrink, compensating stop or invented cancellation.

The normal core drain, eviction and PDB checks run before the provider accepts a
stop. Azure records the exact Node UID and deletion taint it wrote. The
NoSchedule taint remains during stopping, parking and restart. The provider
reports `Suspended=True` and excludes inactive inventory from scheduling,
health and unregistered-instance cleanup, including retained VMs with no Node.

Reuse requires backend Start success and a Ready heartbeat newer than that
specific Start's acceptance time. The provider publishes active availability
only after owned status and taint cleanup succeed. Kubernetes status and spec
cannot be updated atomically: status release is performed while NoSchedule
protection remains, and the coherent provider view continues reporting
inactivity until both updates succeed. Unrelated conditions, taints, annotations,
UIDs and administrative cordons are preserved. Later ordinary Unready or
administrative cordoning does not replay activation.

Each accepted attempt keeps its own deadline from the node group's maximum
provision time. New requests do not extend older deadlines. Failed or expired
accepted capacity is withheld from upcoming placeholders but still consumes
upper capacity limits. Azure reports each failure through the existing
scale-state notifier outside provider locks.

Required read/write failures, changed identities, unowned restrictions and
ambiguous cloud submissions fail closed. A `403` writing status after an
accepted stop does not cancel that stop or authorize blanket taint removal.
Restore the missing permission or resolve the conflicting ownership before
expecting scaling to continue.

## Restart and migration limitations

Accepted-operation evidence and ownership receipts are process-local, not a
durable recovery journal. A persisted condition, VM power state or Node age
cannot reconstruct an acceptance time or prove ownership after a crash or
leadership change. Ambiguous retained inventory or unfinished operations block
safe accounting and mutations and may block an autoscaler iteration.

There is no autonomous adoption or crash recovery in this delivery. Preserve
diagnostics, inspect the cloud operation and Kubernetes state, and explicitly
reconcile uncertain work before restarting normal scaling. Removing a policy
entry is not recovery and does not purge retained instances.

Before disabling Deallocate or migrating a group to Delete, resolve all retained,
in-flight and uncertain work and restore ordinary active infrastructure through
an operator-controlled procedure. A live group with retention history cannot
silently be replaced by a Delete group.

Local tests and rendering are not live Azure validation, AKS parity, an accepted
upstream API, or a deployable release claim.
