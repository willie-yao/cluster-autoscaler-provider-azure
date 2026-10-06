# Suspended VMSS nodes

Deallocate mode is opt-in and keeps the Kubernetes Node when a VM is parked. The provider owns VMSS Deallocate and Start operations. The core selects and drains nodes, but does not stop or start VMs.

Correct resume accounting requires the proposed core patch [`willie-yao/cluster-autoscaler@0702a00a6`](https://github.com/willie-yao/cluster-autoscaler/commit/0702a00a61d19c8be292dab260de3df3f77a0477). The released core dependency remains unchanged. See [the local patched-core check](testing.md#patched-core-resume-check) before using this prototype.

## Enable

Set `deallocate: true` in the Azure configuration, or `AZURE_DEALLOCATE=true`, to enable the mode globally. To select one pool instead, leave the global setting disabled and use `--nodes=<min>:<max>:Deallocate:<name>`. Ordinary three-field specs and four-field Delete specs remain available. The AKS fifth field for labels and taints is not supported.

Only non-hosted, regular-priority Uniform VMSS with managed, non-ephemeral OS disks are supported. AKS-managed, Spot, Flex, standard and VMs pools are excluded. An unsupported explicit Deallocate pool is logged and excluded from autodiscovery, not changed to Delete. Do not run another autoscaler against the same pools.

The Kubernetes identity needs `update` on `nodes/status` as well as the existing Node permissions. Pass both startup taints, so cloud-controller taints during resume do not block scheduling simulation:

```text
--startup-taint=node.cloudprovider.kubernetes.io/shutdown
--startup-taint=node.kubernetes.io/out-of-service
```

See the [chart example](../charts/cluster-autoscaler/README.md#suspended-nodes).

## Lifecycle and size contract

After deallocation completes, the provider sets `Suspended=True`, adds a `status-taint.cluster-autoscaler.kubernetes.io/azure-suspended:NoSchedule` taint and the scale-down-disabled annotation. It removes the core's deletion taints and applicable cordon, while retaining Node identity and PodCIDR.

Refresh also repairs suspension metadata from settled Azure instance views, including parks completed before a restart or by an earlier provider. It clears stale suspension metadata on settled running VMs, but skips in-flight power operations and unsettled provisioning states.

IncreaseSize resumes parked VMs before creating new ones. Resume sets `Suspended=False` with a transition time and removes the provider's suspension metadata. The core patch counts an old, unready resumed Node as starting during its startup window.

For Deallocate pools, the provider raises MaxNodeStartupTime to at least the effective MaxNodeProvisionTime plus six minutes and one second. The margin covers the two three-minute API budgets after resume and the Node timestamp's one-second precision, so the startup window ends after the request deadline. Global defaults and per-pool `maxnodeprovisiontime` and `maxnodestartuptime` tags are applied first, and a longer startup time is kept. A failed resume can then trigger the core's `ScaleUpTimedOut` and backoff while the Node still counts as starting. DecreaseTargetSize only refreshes caches and does not stop or delete resuming VMs.

TargetSize counts every VM, including parked VMs. A resume does not change TargetSize, and only newly created VMs increase it. The configured min and max apply to running VMs. With P parked VMs, MinSize returns configured min + P and MaxSize returns configured max + P.

The adjusted bounds let a pool resume after reaching its configured maximum, and stop scale-down at the configured running minimum. Minimum-size enforcement can resume parked VMs to restore the running minimum. Accepted Starts count as running capacity before Azure's instance view catches up.

Core status displays the adjusted total bounds, not the configured running bounds. Similar-group balancing still orders groups by total TargetSize. Synthetic unregistered and failed-creation cleanup uses the existing physical deletion path, because those instances do not have a registered Node to retain.

## Cluster limits

Set `maxActiveNodes` or `AZURE_MAX_ACTIVE_NODES` to add a `nodes` maximum to the provider ResourceLimiter. Zero leaves the additional node cap unset. Suspended Nodes do not consume maximum nodes, cores, memory or GPU quota, while resuming Nodes and upcoming templates do. Minimum-resource tracking keeps its existing semantics.

Use `--max-nodes-total=0` when relying on the active-node cap. The core's `--max-nodes-total` flag counts retained Nodes, and the provider warns rather than overriding it. The cap does not limit the total retained VM inventory.

Until the core minimum-size quota fix lands, `--enforce-node-group-min-size` does not reserve quota for minimum-size growth within the same loop. A later scale-up in that loop can exceed `maxActiveNodes` or other cluster quotas, even with one group growing to minimum. Several groups growing to minimum can also exceed quotas. The provider does not change that core path. Azure VMSS failures, lost Node permissions and delayed registration can still prevent the configured running minimum from being reached.
