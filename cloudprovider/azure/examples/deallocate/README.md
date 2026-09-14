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

The standard `--cordon-node-before-terminating` setting is supported, including
its default value of true. Azure CA records whether it changed a Node from
schedulable to cordoned and only undoes that provider-owned cordon. Nodes already
cordoned by an administrator remain cordoned.

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
stop. Azure records a versioned Node receipt containing the exact Node UID,
provider ID, deletion taint and provider-owned cordon state. The NoSchedule
taint remains during stopping, parking and restart. The provider reports
`Suspended=True` and excludes inactive inventory from scheduling, health and
unregistered-instance cleanup.

Reuse requires backend Start success and a Ready heartbeat newer than that
specific Start's acceptance time. The provider publishes active availability
only after owned status and taint cleanup succeed. Kubernetes status and spec
cannot be updated atomically: status release is performed while NoSchedule
protection remains, and the coherent provider view continues reporting
inactivity until both updates succeed. Unrelated conditions, taints and
annotations are preserved. Receipt-based cycles preserve administrative cordons.
The legacy cordon exception described below applies only during receiptless
restart adoption. Later ordinary Unready or administrative cordoning does not
replay activation.

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

Completed parks created by this provider are rediscovered after process
recreation when the VM is exactly `deallocated` with successful provisioning and
the same live Node still carries the provider's `parked` receipt, exact deletion
taint and `Suspended=True` condition. Scale-up reuses that capacity before
growing the VMSS. A recovered Node remains inactive until the new Start
completes and its Ready heartbeat advances beyond both the pre-Start heartbeat
and the new acceptance time.

Settled parks created by the legacy Azure implementation or an earlier
receiptless prototype are also adopted when the group is explicitly configured
for Deallocate, the VM is exactly `deallocated` with successful provisioning,
and its matching Kubernetes Node still exists with a stable UID and provider ID.
The Node may already have the exact legacy
`ToBeDeletedByClusterAutoscaler=<unix timestamp>:NoSchedule` taint, or recovery
adds a new owned deletion taint. It may be schedulable or cordoned. Recovery
establishes the current receipt and `Suspended=True` protection before making the
park available for reuse.

Legacy AKS did not record whether it introduced `spec.unschedulable`. When a
receiptless Node has both the exact legacy deletion taint and an existing cordon,
compatibility recovery treats that cordon as legacy autoscaler protection and
may remove it after Start completes and fresh Ready evidence arrives. This can
remove a cordon that an administrator set before the legacy scale-down. The
exception does not apply to normal receipt-based recovery or to a receiptless
legacy Node that is cordoned without the legacy deletion taint; those existing
cordons remain in place. Unrelated taints, annotations and conditions are never
claimed by this compatibility rule.

This is settled-state adoption, not a durable cloud-operation journal. A
`pending` receipt, deallocating or otherwise ambiguous VM state, missing Node,
identity changes during adoption, conflicting receipt or `Suspended` metadata,
or malformed or duplicate legacy deletion taints block accounting and mutation.
A crash before the provider publishes `parked` for a new scale-down can therefore
strand an otherwise deallocated VM for operator reconciliation. Node-less legacy
parks are not adopted. Removing a policy entry is not recovery and does not
purge retained instances.

A later operator request to keep a Node cordoned while it is already under a
receipt-based provider-owned cordon cannot be distinguished from no change.
Outside the explicit legacy-taint exception above, preexisting operator cordons
and cordons applied when provider cordoning is disabled are preserved.

Before disabling Deallocate or migrating a group to Delete, resolve all retained,
in-flight and uncertain work and restore ordinary active infrastructure through
an operator-controlled procedure. A live group with retention history cannot
silently be replaced by a Delete group or unregistered through discovery-selector
loss. Invalid discovery sizing or selector removal fails refresh and preserves
the existing registration; correct the metadata before expecting scaling to resume.

Local tests and rendering are not live Azure validation, AKS parity, an accepted
upstream API, or a deployable release claim.
