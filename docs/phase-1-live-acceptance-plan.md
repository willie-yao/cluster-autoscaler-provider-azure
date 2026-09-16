# Kubernetes v1.37.0 live acceptance plan

> Historical execution plan. The run was later authorized, completed and cleaned
> up. See the [Phase 1 acceptance evidence](phase-1-acceptance-evidence.md).
> This plan does not authorize future cloud actions.

The proposed profile used one dedicated, self-managed Kubernetes **v1.37.0**
cluster, Linux x86-64 VMSS **Uniform** workers and ordinary **Delete** mode. HA,
AKS, Flex, VMs-pool, deallocate and broad E2E were excluded.

The bootstrap prerequisite was blocked when this proposal was written. The
[prerequisite record](phase-1-bootstrap-prerequisite.md) now documents its
resolution for the tested profile.

## Fixed references

| Item | Pin |
| --- | --- |
| Upstream Azure executable | `kubernetes/autoscaler@c85f5afed954f7ecdbfb9091da2202426806d8d8`, application directory, `-tags azure` |
| Candidate | `48f997fcd8d4a93f25d06837e185ea77d4e2c5cd`, unsigned local commit, not pushed |
| Extracted core | `sigs.k8s.io/cluster-autoscaler v0.0.0-20260903143621-3d1c7137cdac` |
| Dependencies | Unchanged, including Kubernetes `v1.37.0-rc.1` and `replace k8s.io/autoscaler/cluster-autoscaler/apis => ./apis` |

A demonstrated incompatibility requires evidence and a separate change proposal,
not an automatic dependency update.

## Resources and ownership

Proposed region: **West US 3**, one available zone. Confirm subscription, SKU
availability and eight DSv5-family vCPUs before creation.

| Resource | Limit |
| --- | --- |
| Resource groups | Two unique groups: infrastructure and workers; run ID, owner and expiry tags |
| Control plane | One `Standard_D2s_v5`, outside autoscaled groups; API/etcd, CCM, CNI and autoscaler |
| Main worker VMSS | Uniform, same SKU, initial/min/max **1/1/2** |
| Zero worker VMSS | Uniform, same SKU/image, initial/min/max **0/0/1** |
| Peak capacity | **Three workers plus control plane: four VMs, eight vCPUs, 32 GiB RAM** |
| Storage | At most four 128-GiB Standard SSD LRS OS disks, worker disk deletion enabled; no workload PVs/snapshots |
| Network | One VNet, control-plane/worker subnets, NSGs, one Standard NAT gateway, two Standard IPv4 addresses for NAT and control-plane access |
| Identity | Autoscaler UAMI and separate CCM UAMI, attached only to the control plane |
| Not provisioned | AKS, ACR, Bastion, paid monitoring, application load balancer, image-builder VM or image gallery |

Restrict public SSH/API access to the approved operator IP. Workers use the
private API endpoint and explicit NAT egress. No production network peering.

**Credentials:** autoscaler user-assigned managed identity, with Contributor only
on the disposable worker resource group. Do not use the README's subscription-wide
service-principal example. Scope the separate CCM identity's compute/network
permissions to the two disposable groups. The operator uses interactive Azure
authentication and an explicit temporary kubeconfig; setup/cleanup requires scoped
role-assignment authority. No worker receives a cloud-management identity.

**Cleanup owner:** a named accountable human accepted before creation. The agent
executes approved, attended steps only. On session loss, the owner takes over
from the resource ledger; no background cleanup promise.

## Bootstrap and image delivery

Freeze an exact Ubuntu 24.04 Gen2 image, kubeadm v1beta4 bootstrap and v1.37.0
binaries, containerd/runc, CNI, Azure CCM, kube-proxy and CoreDNS artifacts. Their
versions/checksums are required approval inputs. The repository does not provide
a verified self-managed v1.37.0 bootstrap/image artifact. Missing inputs block
provisioning; do not substitute an RC image or start an image-building project.
Static validation is not boot proof; step 2 qualifies the first worker live.

The VMSS model must autonomously join each new instance, set unique hostnames,
correct Azure `providerID`s and the pool labels used below. Use a 12-hour kubeadm
join credential in Custom Script Extension **protected settings**, never public
custom data. Use root-only temporary files, no shell tracing or credential output,
and revoke the token during cleanup. No per-instance manual join/label repair.

Build both Linux/amd64 executables with Go 1.26.0, `CGO_ENABLED=0`, `-mod=readonly`,
`-trimpath` and `-tags azure`. Package them on the same runtime-image digest.
Record source, binary and OCI hashes. After approval, transfer OCI archives by SSH
to the control plane and import into containerd's `k8s.io` namespace. Verify CRI
image identities, use immutable references and `imagePullPolicy: Never`, and keep
both images available for rollback. **No registry creation/login or image push.**
System and workload images are separately pinned.

## System placement and first-worker gate

Apply required placement before creating any worker. Every non-DaemonSet system
workload stays on the sole control-plane VM, including **both CoreDNS replicas**,
the autoscaler, any Deployment-form CCM, Calico kube-controllers, and any selected
CNI operator/Typha or other system controller. Use a required selector, not only
preferred affinity:

```yaml
nodeSelector:
  node-role.kubernetes.io/control-plane: ""
tolerations:
  - key: node-role.kubernetes.io/control-plane
    operator: Exists
    effect: NoSchedule
```

Retain the control-plane taint. Its static control-plane Pods stay there naturally.
Ensure CoreDNS replicas can co-locate on this one node without required
anti-affinity. Any additional non-DaemonSet addon must meet the same rule or stay
uninstalled. Confirm their requests fit the existing control-plane SKU; otherwise
stop rather than spill onto workers or enlarge the envelope. Before autoscaler
testing, workers may host only required per-node DaemonSets, such as the CNI node
agent, kube-proxy and cloud-node-manager, plus the temporary diagnostic Pod below.

**Hard gate before either autoscaler starts:** with both autoscalers absent or
stopped, create the first main-pool instance through the frozen VMSS model and
observe, within 20 minutes:

1. Extension/cloud-init completes and kubeadm joins without interactive SSH,
   manual join, node-label repair or service restarts.
2. The node has the expected VMSS instance/provider ID, pool label, kubelet
   `v1.37.0`, pinned runtime, expected allocatable resources and sustained Ready
   status in two observations at least 60 seconds apart.
3. Required node DaemonSets are Ready; CNI networking and DNS work from a temporary
   pinned diagnostic Pod in a dedicated acceptance namespace on that worker.
4. CoreDNS and every other non-DaemonSet system Pod remain on the control plane.

Remove the diagnostic Pod and record the evidence. VM provisioning success,
installed binaries or a bootstrap sentinel alone do not satisfy the gate. Any
failure stops before autoscaler testing and is classified as environment/bootstrap
failure. Do not repair the guest and silently proceed.

## Reuse and equivalent configuration

The retained `cloudprovider/azure/test` setup is not runnable unchanged:

| Existing component | Treatment |
| --- | --- |
| `Makefile:setup-cluster` and dev deployment script | AKS/ACR/workload-identity provisioning; do not run |
| `pkg/environment/environment.go` | Reuse observation patterns, but match workers to VMSS instance/provider IDs and require every worker Ready; its current count includes a standalone control-plane node |
| `suites/scaleup/suite_test.go` | Reuse CPU-demand/polling patterns; replace the 100-Pod flow with bounded CLI-driven manifests below |
| Automatic Helm upgrade and fast scale-down values | Do not use `ResetValues` or settings that disable system-pod/local-storage safeguards |

Leave the older nested E2E-module pins unchanged. Use direct CLI observations for
this run rather than introducing a new framework.

Freeze one candidate-chart render for both executables; only the image reference
changes. This isolates runtime comparison from the separately tested upstream
chart compatibility and preserves managed-identity Secret handling. Keep identical:

- Namespace, service account, lease name, RBAC, existing configuration Secret
  references, arguments, volumes, selectors and limits.
- One autoscaler replica pinned to the tainted control plane, with the required
  toleration, `hostNetwork: true` and `ClusterFirstWithHostNet`.
- VM type `vmss`, managed identity enabled, force-delete/ETag/Flex/VMs-pool disabled.
  No deallocate-specific option.
- Discovery tags `cluster-autoscaler-enabled=true`,
  `cluster-autoscaler-name=<run-id>` and the table's `min`/`max` values.
- Actual node label `acceptance-pool=main|zero` and matching VMSS template tag
  `k8s.io_cluster-autoscaler_node-template_label_acceptance-pool`.
- Test timings: scan 10s; scale-down delay after add 2m; unneeded time 2m;
  unremovable-node recheck 30s; maximum provisioning 20m; graceful termination 120s.
  Retain system-pod/local-storage protections.

VMSS upgrades are manual; Azure autoscale and automatic repairs are disabled.
No AKS, CAPZ, GitOps, HPA or other controller may compete for worker capacity or
restart a stopped autoscaler.

## Step-by-step execution after approval

Use explicit subscription, group and kubeconfig arguments. Run one CLI action,
inspect the result, then proceed. No unattended setup/test/cleanup orchestration.
Stage ceilings: provisioning/qualification 60m, image/start checks 15m,
cutover/quiescence 10m, rollback smoke 45m; workload bounds are below.

1. **Read-only preflight.** Confirm the approval inputs below, provider registration,
   permissions, quota, network isolation and unused resource names. Verify artifact
   checksums and freeze both manifests. Missing prerequisites stop the run without
   registering providers, expanding permissions or changing dependencies.
2. **Provision and qualify.** Create groups, network, identities and control plane
   one operation at a time. Initialize v1.37.0 and install frozen CCM/CNI. Create
   both VMSS models at zero, then manually set main to one before starting an
   autoscaler. Require exact server/kubelet v1.37.0, healthy runtime/DNS, correct
   provider IDs and the complete first-worker gate above. Record initial sizes
   **1/0** only after that gate passes.
3. **Start upstream.** Preload/verify both images, then apply the common manifest
   with the upstream image. Confirm one leader, correct UAMI, working Azure and
   Kubernetes access, exactly the intended groups/bounds and five idle minutes
   without capacity changes. Record CPU request `R`: start with 1200m, but require
   one demand Pod to fit a worker and two not to fit after daemonset reservations.
4. **Run upstream cases.** Execute the table below, retain evidence, remove test
   workloads and restore **1/0**. An upstream failure blocks the comparison.
5. **Cut over.** Save the upstream manifest/image. Disable any restart/reconciliation
   path; scale its Deployment to zero and wait for every old Pod to disappear.
   Require terminal Azure operations and stable instance/Ready counts in two
   observations 30s apart. While replicas remain zero, change only the image;
   verify configuration equivalence, then start one candidate replica.
6. **Run candidate cases.** Repeat the same table from **1/0**, using the same `R`,
   manifests, timings and environment. Do not change VMSS models, identities or
   permissions to rescue one arm. Record results and reset to **1/0**.
7. **Rollback rehearsal.** Stop candidate first and repeat quiescence checks. Restore
   the saved upstream image/configuration while replicas are zero, verify scope,
   limits and credential references, then start one replica. Require healthy
   discovery and a main-pool **1 -> 2 -> 1** growth/Delete smoke with scheduled Pods,
   no zero-pool/control-plane changes and terminal cloud operations. No blind
   `rollout undo`, forced lease deletion or overlapping controllers.
8. **Cleanup.** Stop the remaining controller, settle operations, save redacted
   evidence and revoke the join token. Verify ownership, delete the worker group
   and wait, then delete infrastructure. Verify no run-owned VMSS, VMs, disks,
   NICs, IPs, NAT gateway, identities or explicit role assignments remain. Remove
   temporary credentials; record the owner and next action for any failed cleanup.

Different leader-election locks do not prevent two controllers managing the same
pool. Stopping the old controller and settling operations are mandatory.

### Identical cases for each arm

| Case | Action and pass condition | Bound |
| --- | --- | --- |
| Growth | Two main-pool Pods requesting `R`: CPU-unschedulable demand causes **1 -> 2**, Azure update completes, new worker joins unaided, both Pods Ready | 20m |
| Maximum | Request a third main replica briefly, then return to two: third stays Pending, main remains **2**, zero **0**, no out-of-scope scale operation | Observe >=2m |
| From zero | One Pod selected only onto `acceptance-pool=zero`: **0 -> 1**, correct predicted/actual label and provider ID, Pod Ready without manual repair | 20m |
| To zero | Remove only that workload: **1 -> 0**, selected Azure instance and Kubernetes Node disappear, not merely deallocate | 15m |
| PDB protection | While main is two, place two low-request replicas with preferred anti-affinity, one on each main worker, and `minAvailable: 2`; remove CPU-demand Pods. Require both workers underutilized, feasible consolidation and an explicit PDB-blocked decision with no deletion | Observe >=5m |
| Safe Delete | Change PDB to `minAvailable: 1`: main **2 -> 1** through eviction/rescheduling, replicas recover, selected instance and Node disappear, survivor healthy, no force-delete or deallocate call | 15m |

If PDB workload distribution or a blocked decision is absent, the test is unproven.
Capture image/config hashes, group limits, provider/instance mappings, Pod events
and placement, all-worker readiness, autoscaler logs/status, Azure operation IDs
and before/after inventories. Desired capacity alone is not deletion proof.
Do not collect Secret data, join credentials or kubeconfig contents.

## Cost, deadline and approval

USD public retail queried **September 15, 2026**, West US 3, excluding tax and
discounts. Conservative assumption: all four VMs for all eight hours; monthly
disks prorated using 730 hours.

| Item | Rate | Eight-hour allowance |
| --- | --- | --- |
| Four Linux D2s_v5 VMs | $0.096/VM-hour | $3.07 |
| Four E10 LRS OS disks | $9.60/disk-month | $0.42 plus operations |
| Standard NAT | $0.045/hour | $0.36 |
| Two Standard IPv4 addresses | $0.005/address-hour | $0.08 |
| NAT traffic | 20 GB at $0.045/GB | $0.90 |
| Bandwidth and disk operations | Usage contingency | $1-3 |

**Expected $6-10; proposed ceiling $20.** Stop new tests at hour six and finish
rollback/evidence/cleanup by hour eight. Limits and budget alerts are not hard
billing caps. Extensions, extra resources, another region or failed cleanup
require renewed approval and an updated estimate.

Approval must confirm: subscription; West US 3/zone/SKU; operator IP; resource
names; exact node/bootstrap/component pins; scoped roles; accountable cleanup
ownership; eight-hour/$20 bounds; and permission for provisioning, protected
bootstrap credentials, SSH image import, workload tests, cutover/rollback and
cleanup. Repository pushes and image publication remain excluded.

Stop on scope/capacity violations, unstable control plane, wrong image/version,
missing evidence, overlapping controllers, access failures or case timeouts.
Freeze workloads, stop the active autoscaler when safe, capture evidence and
report the failing layer. No automatic retries, node repair, safeguard disabling
or dependency changes. Classify each case separately as pass, fail or unproven;
success supports this exact configuration, not every remaining Phase 1 gate.

## References

- [Kubeadm setup](https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/create-cluster-kubeadm/) and [runtime requirements](https://kubernetes.io/docs/setup/production-environment/container-runtimes/)
- [Managed identities](https://learn.microsoft.com/en-us/entra/identity/managed-identities-azure-resources/how-to-configure-managed-identities) and [Custom Script protected settings](https://learn.microsoft.com/en-us/azure/virtual-machines/extensions/custom-script-linux)
- [Pre-pulled images](https://kubernetes.io/docs/concepts/containers/images/) and [Azure Retail Prices API](https://learn.microsoft.com/en-us/rest/api/cost-management/retail-prices/azure-retail-prices)
