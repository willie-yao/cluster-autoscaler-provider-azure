# Azure E2E tests

This nested Go module runs Ginkgo tests against an **already prepared,
disposable environment**. It does not provision a cluster, deploy/reconfigure
Cluster Autoscaler, change cloud resources directly, or destroy infrastructure.
The operator retains responsibility for setup, single-controller ownership,
budget monitoring and infrastructure cleanup.

The default fixture is Linux VMSS Uniform with main min/max `1/2`,
zero min/max `0/1`, and one control plane outside both groups. Its ceiling is
four VMs and eight vCPUs, counted using Azure instances, requested capacity and
SKU core counts. This fixture limitation is not a provider support restriction:
standard pools, Flex, VMs-pool and AKS need their own qualified fixtures and
execution evidence. Priority, scheduler, system-namespace, DRA, taint and disk cases require
the additional operator preparation described below.
Before any workload, the configured maximum of two main workers, one zero-pool
worker and the control plane must also fit eight vCPUs. A zero-capacity pool
with an oversized SKU is rejected before it can scale.
Positively observed envelope or per-pool capacity/instance-count breaches stop
the observation immediately, even if a later read would converge within bounds.
Received instance pages are counted by distinct normalized VM identity before
NIC validation or another page request. A partial list can establish an upper
bound breach, but the lower bound is checked only after the list is complete.
Ordinary read failures and convergence remain retryable in positive waits.
These sampled guards are not a hard spending interlock: the operator still
owns independent budget monitoring and cleanup.

Three additional cases run in `suites/scalephase` with `TEST_SUITE=scalephase`.
They use separate operator phases under the same four-VM and eight-vCPU
limits. The 29 default `scaleup` cases keep their two-pool fixture.
Do not select a default case while the pools or controller flags are set
for a phase.

## Local validation

```sh
make test-e2e-local # from the repository root
# Or, from this directory:
make test-local
```

These run race-enabled fake/unit checks, including the tagged observation and
DRA image-guard regressions, compile the `e2e`-tagged suite and
dry-run Ginkgo registration without running setup hooks or test bodies.
They do not authenticate to Azure or contact Kubernetes. Ordinary PR CI runs
the same target through `make test-ci`. Compilation is not live E2E evidence.

## Current and legacy workflows

For the current suite, prepare the fixture and deploy Cluster Autoscaler
separately according to the [operator contract](#operator-contract), then run
[focused cases](#focused-execution) with `ENVIRONMENT` pointing to the
[JSON binding](environment.example.json).

The following Make targets are retained for a separate legacy AKS development
workflow. They are not preparation or validation steps for the current
standalone-control-plane VMSS Uniform fixture:

| Target | Legacy action |
| --- | --- |
| `setup-cluster` | Creates AKS, ACR and workload identity. |
| `deploy-local` | Builds and deploys CAS with the existing AKS Skaffold configuration. |
| `deploy-local-dev` | Watches and redeploys CAS with that configuration. |
| `validate-env` | Checks only the presence of `AZURE_SUBSCRIPTION_ID` and `AZURE_RESOURCE_GROUP`; it does not inspect the JSON binding or cluster. |

Neither `setup-cluster` nor `validate-env` is a prerequisite for `e2etests`.
The current runner takes its subscription and resource group from the JSON
binding, not from those legacy environment-variable checks.

## Operator contract

Use a fresh cluster, explicit kubeconfig/context, one controller managing only
the intended pools, and an exact candidate image. Keep non-DaemonSet system
workloads on the control plane. Disable VMSS overprovisioning. Both scale sets
and the control-plane VM must have the `autoscaler-e2e-run=<runID>` Azure tag.
The dedicated worker resource group may contain only the two selected VMSS,
and optionally the named control-plane VM. Other infrastructure is not deleted
or inventoried by these tests.
The balance phase is the one exception. It requires exactly the main,
zero and two named balance VMSS in that group.

Both VMSS must have `cluster-autoscaler-name=<discoveryValue>`, and `min`/`max`
tags matching `1`/`2` and `0`/`1`. The autoscaler must use exactly:

```text
--node-group-auto-discovery=label:cluster-autoscaler-name=<discoveryValue>
```

The test runner verifies the controller's fresh status ConfigMap contains
exactly these two groups and its fresh lease belongs to the single Ready
controller Pod. The pinned runtime uses `os.Hostname()` as its lease identity:
the exact Pod name normally, or the exact scheduled Node name when that Pod
uses host networking. Arbitrary prefixes and foreign Node identities fail.
It does not read cloud Secrets or claim to detect every
possible external controller. The operator must disable any managed or other
autoscaler that could share these pools.
Every negative observation of no-growth, PDB blocking, workload retention,
scheduler suppression or the maximum-capacity refusal rechecks the same
controller Pod, lease, status, image and scope contract. Losing the controller
or observing stale leadership/status fails that negative window.
In the balance phase, the controller status must list exactly four groups.

Both the Deployment template and its running autoscaler container must contain
exactly one literal `ARM_SUBSCRIPTION_ID` and `ARM_RESOURCE_GROUP` matching the
environment binding. These non-secret values override the provider's cloud
configuration file. Secret/ConfigMap references for these two values are not
resolved by the runner: replace those entries with explicit literals when
preparing the fixture. Credential references and identity configuration remain
operator-owned and unchanged.

Use ordinary Delete mode, preserve PDB, system-pod and local-storage
protections, and configure scale-down delays externally to fit the test:
`scale-down-delay-after-add`, `scale-down-unneeded-time` and
`unremovable-node-recheck-timeout` must be explicit and at most one minute. Leave
`scale-down-utilization-threshold` at its default `0.5`. Five-minute negative
observations are not meaningful with longer delays. Nothing in the runner
changes these settings or installs a Helm release.
VMSS tags under
`k8s.io_cluster-autoscaler_node-template_autoscaling-options_` can override
global flags. If present, `scaledownunneededtime` must remain between zero and
one minute and `scaledownutilizationthreshold` must equal `0.5`. Conflicting
overrides fail preflight and subsequent observations.

Create `kube-system/autoscaler-e2e-authorization` yourself. Its data must contain
exact matching `run-id`, `subscription-id`, `resource-group` and `cluster-uid`
values. `cluster-uid` is the UID of the **kube-system Namespace**. Tests cannot
self-authorize by creating this marker. A missing or mismatched marker fails
before any workload mutation. This marker is explicit opt-in and consistency
checking, not independent security authorization.

Supply a non-secret JSON file based on [environment.example.json](environment.example.json).
The base fields are required, unknown fields fail, and kubeconfig must be absolute.
Set `diskStorageClass` to the name of the prepared class for `AZ-P1-006`.
It may be empty for other cases.
For a phased case, use a separate copy of the binding and set `phase` to
`balance`, `no-join`, or `minimum`. The balance phase also requires
`balancePoolA`, `balancePoolB`, and `balanceLabel`. Omit these three fields
in the other phases. A missing phase skips the new cases. A wrong marker
or controller setting fails the selected case.
Do not commit the real kubeconfig, credentials, bootstrap material or private
keys. Azure observations use the existing SDK's `DefaultAzureCredential`.
Azure errors retain HTTP status/code but omit response bodies that might
contain bootstrap data.

For an operator intentionally authenticating the runner through an existing
Azure CLI login, stale inherited service-principal variables can take
precedence. Scope their removal to that runner process only:

```sh
env -u AZURE_CLIENT_ID -u AZURE_CLIENT_SECRET -u AZURE_TENANT_ID \
  make -C cloudprovider/azure/test e2etests \
  ENVIRONMENT=/absolute/path/to/environment.json \
  ARTIFACTS=/absolute/path/to/non-secret-artifacts \
  LABEL_FILTER=CA-005 TEST_TIMEOUT=45m
```

Do not print credential values or change global shell/CI authentication.
This is an operator-process option, not a requirement to use CLI credentials
in all environments. It does not change the application's managed identity.

### Phased fixtures

Run the three phased cases separately on one fresh, owned fixture. Stop the
single autoscaler before changing pool tags or its Deployment. Wait until
the old Pod is gone, then check the next phase's pools and flags. The runner
does not change Azure pools or restart the controller. It creates only
namespaced test objects and reads Azure and Kubernetes state. Keep one
control plane and use `Standard_D2s_v5` workers in zone 1. Stop the run
if it exceeds four actual VMs or eight vCPUs.

For `AZ-P1-007`, set main to `1/1` and zero to `0/0`. Add two run-tagged
VMSS named by `balancePoolA` and `balancePoolB` in the worker resource
group. Give both `min=0`, `max=2`, and matching SKU, image, zone,
join setup and node-template scheduling tags. Both future Nodes must get `<poolLabel>=<balanceLabel>` from
their kubelet setup. Set the same value on each VMSS tag
`k8s.io_cluster-autoscaler_node-template_label_<poolLabel>`.
The controller must discover all four groups and use
`--balance-similar-node-groups=true`, `--balancing-label=<poolLabel>`,
`--max-nodes-total=4`, `--parallel-scale-up=false`, `--salvo-scale-up=false`,
and `--v=1` with text logs. Keep ordinary bounded scale-down settings.
The configured group maxima exceed four, so also monitor actual VM
counts independently. Set `allow-balance-fixture: AZ-P1-007` in the
operator marker. Prepare the Deployment with zero replicas. The case
creates two unschedulable Pods and then creates `start-controller` in
its test namespace. Start one controller replica only after that signal.
The case checks one plan that adds one node to each pool, Ready Pods
on both Nodes, and physical deletion back to zero. The runner compares
returned image, zone, SKU and template tags. It cannot compare
bootstrap data, so both workers joining and becoming Ready provides
that check. Remove the extra
VMSS and verify their deletion before the next phase.

For `AZ-P1-008`, set main to `1/2` and zero to `0/1`. The zero VMSS
must not include the join bootstrap. Give it the operator tag
`autoscaler-e2e-no-join=<runID>`. Azure GET redacts `customData`,
so the runner cannot read or verify the VMSS bootstrap content.
The operator must check the template before starting the case.
Do not use guest fault injection or RunCommand.
Set `--max-node-provision-time=3m` and
`--initial-node-group-backoff-duration=5m` on the running controller.
Set `allow-no-join-fixture: AZ-P1-008` in the marker. The runner
checks the operator tag, observes the VM in Running state without
a Kubernetes Node during the provision window, checks the timeout
event and group backoff, and verifies deletion of the VM and NIC
when capacity returns to zero.

For `AZ-P1-009`, set main's discovery tags to `min=2`, `max=2`,
but leave its Azure capacity at one with one Ready worker. Keep zero
at `0/1`. Prepare `--enforce-node-group-min-size=true` and `--v=1`,
then stop the controller before the case starts. Set
`allow-minimum-fixture: AZ-P1-009` in the marker. The case reads
the below-minimum Azure capacity before it creates `start-controller`
in its test namespace. Start one controller replica only after that
signal. The case rejects unrelated Pending Pods and checks the
controller's minimum-size scale-up plan. It then checks that the first
worker remains, a second joins and both stay at the tagged minimum.
Leave both workers in place until the operator's workers-first cleanup.

The workload image must provide `sh` and `sleep`; pin its digest. The tests use
idle containers with scheduling requests rather than consuming the requested
CPU. `demandCPU` is measured against actual worker allocatable CPU and existing
Pod requests: one Pod must fit, two must not. The retained kubectl request
helper accounts for init containers, restartable sidecars and overhead.
Pod-level requests, which this helper predates, fail instead of using a guessed
calculation. The helper uses the pinned `k8s.io/kubectl v0.33.1` dependency.
Use homogeneous worker SKUs, allocatable CPU/memory and background Pod request
profiles across both pools, including newly joined workers. CPU and memory
geometry is measured on current workers and assumes future eligible workers
match; the memory-based cross-pool cases use the baseline main worker's
measurement. Qualifying those assumptions after installing addons is an
operator prerequisite, not heterogeneous packing support.

## Focused execution

Freeze a local source commit before the operator starts a case. Run one
scenario at a time:

```sh
make -C cloudprovider/azure/test e2etests \
  TEST_SUITE=scaleup \
  ENVIRONMENT=/absolute/path/to/environment.json \
  ARTIFACTS=/absolute/path/to/non-secret-artifacts \
  LABEL_FILTER=AZ-P1-002 \
  TEST_TIMEOUT=90m
```

`TEST_TIMEOUT` is the Ginkgo suite timeout, not a cloud-operation deadline.
Choose it to fit the operator's remaining execution window, including the
bounded namespace/baseline cleanup. The Make target runs Ginkgo's compiled test
binary, not a direct `go test` with its default ten-minute timeout. If invoking
`go test` directly, set both its `-timeout` and `-ginkgo.timeout` explicitly.
The operator must also enforce an absolute process deadline: cancellation and
cleanup grace periods must not delay infrastructure teardown. Interrupted,
timed-out or cleanup-failed cases are unproven/failed, never passing.

The original `FOCUS` selector is also supported. Ginkgo parallel execution is
rejected; an empty selection fails and the first failure stops further cases.
JUnit and JSON reports record outcomes, runtime image, resource counts,
captured VM/Node/NIC identities and physical deletion assertions, not raw API
objects or credentials. Use a separate artifact directory for each invocation.
Record the frozen test-suite commit separately from the runtime image's source
commit and digest. Test-only changes do not require rebuilding the unchanged
runtime. Do not treat a filtered-out or unexecuted spec as passing.

Public-source cases use their own `AZ-001` or `CA-NNN` label. See the
[coverage inventory](#coverage-inventory) for the maintained scenario groups.
Public tests spanning three workers
select only the two owned pools, so `B=1` grows to main/zero `2/1`, still within
four VMs including the control plane.

Priority cases require operator-created `<runID>-expendable` and `<runID>-high`
PriorityClasses with values `-15` and `1000`, `globalDefault: false`, ordinary
preemption, and the `autoscaler-e2e-run=<runID>` label. The candidate must use
`--expendable-pods-priority-cutoff=-10`. The suite never creates or deletes
these cluster-scoped classes. `CA-012` creates actual expendable demand in the
test body rather than deferring it until cleanup, as the public source did.
`CA-014` checks high-priority readiness before and after its observation window,
not continuously throughout it.

`CA-017` and `CA-018` require
`--bypassed-scheduler-names=non-existing-bypassed-scheduler` with no scheduler
installed under that name. `CA-019` uses a distinct unconfigured name and can
run without the optional flag. These Pods intentionally remain unprocessed;
Ready is not the expected result.

`CA-011` is the only exception to generated-namespace-only workloads. It
requires `allow-kube-system-fixture: CA-011` in the operator marker and creates
only uniquely named, run-labeled synthetic Deployment/PDB objects in
`kube-system`. It rejects existing PDB selectors overlapping its labels and
cleans up only the captured object names/UIDs. It never modifies actual system
addons or global system-pod protections.

`CA-020` through `CA-022` require an operator-prepared `resource.k8s.io/v1`
synthetic DRA profile. The suite never installs its privileged addon, RBAC or
DeviceClass. Prepare the public source's `dra-example-driver-kubeletplugin`
DaemonSet in `kube-system`, container `plugin`, image
`registry.k8s.io/dra-example-driver/dra-example-driver@sha256:728fbb69b99e335cfef2d1b9a3d695d2f502c58dd04f7f81143089a72e4044e3`, with
`DRIVER_NAME=gpu.example.com` and `NUM_DEVICES=4`. DeviceClass `gpu` must select
`device.driver == 'gpu.example.com'`. Both objects must carry the run label.
Set marker data `allow-dra-fixture: CA-020,CA-021,CA-022` only after separately
qualifying API, kubelet and autoscaler DRA support. ResourceSlices must publish
four devices per owned worker, with none on the control plane. This does not
require a GPU SKU or authorize changing runtime feature gates or dependencies.
The tests create only namespace-owned claim templates and workloads, and check
generated claim ownership, actual allocations, device uniqueness and matching
worker ResourceSlices. Missing future-node simulation or an incompatible driver
is an execution failure/gap, never a reason to mark the scenarios passing.
`CA-020` adapts the source's twelve one-device Pods to eight, restricted to the
main pool. It requires actual main/zero `1/0 -> 2/0`, eight Ready Pods and eight
unique allocations matching four devices per worker. The initial main worker
must already publish DRA ResourceSlices so the autoscaler can simulate another
worker in that same group. A fresh zero pool has no such live/cached template,
and the frozen Azure provider template supplies no ResourceSlices. This case
does not prove DRA scale-from-zero or three-worker DRA growth and does not warm
the zero-pool cache or change pool maxima.

`AZ-P1-005` requires the zero VMSS tag
`k8s.io_cluster-autoscaler_node-template_taint_autoscaler-e2e-run=<runID>:NoSchedule`.
Configure that pool's kubelet to register with the same
`autoscaler-e2e-run=<runID>:NoSchedule` taint. The suite checks the VMSS tag
before and during the five-minute blocked-demand window, then checks the taint
on the new Node. The baseline main worker must remain untainted by this key.
Run other zero-pool cases on an untainted fixture because their Pods do not
tolerate this taint. The runner does not change VMSS tags or kubelet settings.

`AZ-P1-006` requires the Azure Disk CSI driver and a StorageClass named by
`diskStorageClass`. Install the driver before the run, and verify that
`disk.csi.azure.com` is registered on both main workers. Put both workers in
the same zone so either worker can mount the other's disk. Create a
run-labeled StorageClass with provisioner `disk.csi.azure.com`,
`volumeBindingMode: WaitForFirstConsumer`, `reclaimPolicy: Delete`, and
parameters `skuName: StandardSSD_LRS`, `subscriptionID: <subscriptionID>`,
`resourceGroup: <resourceGroup>`, and `tags: autoscaler-e2e-run=<runID>`.
Set `allow-disk-fixture: AZ-P1-006` in the operator
marker after checking the driver, class and disk permissions. The runner
checks the class, marker, CSIDriver and baseline CSINode before growing main.
It checks that both main workers share a zone and waits up to three minutes
for driver registration before creating the StatefulSet.
Its client needs read access to StorageClasses, CSIDrivers, CSINodes, PVs and
VolumeAttachments, plus permission to exec into its own Pods. The class
provisions two one-GiB claims in the authorized worker resource group. Check
that both disks are removed after namespace cleanup, since a namespace can
finish deleting before the CSI driver finishes deleting its disks.

| Test ID | Assertions |
| --- | --- |
| `AZ-P1-001` | Exact fresh discovery, one leader, Azure ownership/bounds, five-minute `1/0` idle stability |
| `AZ-P1-002` | CPU geometry, main `1 -> 2`, two Ready Pods on distinct real workers, third scheduler-rejected Pod stays Pending for two minutes at max, physical return to `1` |
| `AZ-P1-003` | Zero-pool `0 -> 1`, predicted label matches actual Ready Node/workload, physical `1 -> 0` |
| `AZ-P1-004` | Two protected Pods on separate workers, zero allowed disruptions blocks deletion for five minutes, one allowed disruption permits physical deletion and rescheduling while at least one replica remains Ready at each observation |
| `AZ-P1-005` | A zero-pool VMSS tag blocks demand without a matching taint toleration for five minutes, then tolerated demand grows the pool and the tainted Node runs the Pod; physical return to zero |
| `AZ-P1-006` | Main `1 -> 2 -> 1`, two Ready StatefulSet Pods use distinct Azure Disks, one Pod and its disk move to the survivor without losing the file, and VM/Node/NIC deletion is verified |
| `AZ-P1-007` | One two-node plan splits across two similar zero pools, each grows to one Ready Node and runs a Pod, then both return to zero with physical deletion |
| `AZ-P1-008` | A VM that never registers times out, its group enters backoff, and its VM and NIC are physically deleted |
| `AZ-P1-009` | Main starts below its tagged minimum and grows from one to two without Pod demand |
| `AZ-SUP-ETAG` | `AZ-P1-002` semantics with operator-enabled `AZURE_ENABLE_VMSS_ETAG=true`; supplemental retained example, not a scenario at the selected public inventory pin |

Physical deletion requires captured Azure VM instance IDs, their Kubernetes
Nodes and their NIC resource IDs to disappear. A successful capacity update,
authentication failure or unreadable NIC is not deletion evidence.

Each spec creates a generated namespace marked with its run ID. Cleanup checks
the original Namespace UID and marker before deletion, waits for removal, and
waits for pool baseline. A failure is reported and evidence preserved; there is
no `AfterSuite` cluster deletion and no forced finalizer, PDB or eviction bypass.
Infrastructure cleanup remains operator-owned even after a test failure.

## Coverage inventory

The public scenario source is
`Azure/autoscaler@d892fba1cf557b26d45540f2f6418b7ae52cca46`, a public 1.35-line
test source, not the runtime or Kubernetes support baseline. Of its 23
registrations, 22 active intents are implemented here. `CA-003` remains
source-disabled/flaky and is not implemented. One disk case adapts a separate
public source, and six supplemental cases bring the default suite to 29
registered specs. Three phased cases bring the full module to 32
registered specs. Registration is not execution.

| Cases | Maintained intent | Source file |
| --- | --- | --- |
| `AZ-001`, `CA-001`, `CA-002` | CPU demand, oversized request refusal, memory demand | [public_test.go](suites/scaleup/public_test.go) |
| `CA-004`, `CA-005`, `CA-006` | Host-port conflict, hostname anti-affinity, pending EmptyDir workload | [public_test.go](suites/scaleup/public_test.go) |
| `CA-007` | Physical worker deletion after demand disappears | [public_test.go](suites/scaleup/public_test.go) |
| `CA-008`, `CA-009`, `CA-010` | PDB-permitted drain, blocked deletion, sequential disruption | [public_test.go](suites/scaleup/public_test.go) |
| `CA-011` | Run-owned synthetic system-namespace workload and PDB | [system_test.go](suites/scaleup/system_test.go) |
| `CA-012` through `CA-016` | Expendable and non-expendable demand, preemption and retention | [priority_test.go](suites/scaleup/priority_test.go) |
| `CA-017` through `CA-019` | Bypassed and unconfigured scheduler demand | [scheduler_test.go](suites/scaleup/scheduler_test.go) |
| `CA-020` through `CA-022` | Synthetic DRA growth, oversized claim refusal, deletion and reallocation | [dra_test.go](suites/scaleup/dra_test.go) |
| `AZ-P1-005` | Zero-pool template taint blocks demand without a toleration and permits tolerated demand | [zero_taint_test.go](suites/scaleup/zero_taint_test.go) |
| `AZ-P1-006` | Azure Disk StatefulSet Pods retain claims and data when a worker is deleted | [disk_test.go](suites/scaleup/disk_test.go) |
| `AZ-P1-007` | Balance two similar zero pools with one two-node plan | [phase_test.go](suites/scalephase/phase_test.go) |
| `AZ-P1-008` | Delete an unregistered zero-pool VM after a provision timeout | [phase_test.go](suites/scalephase/phase_test.go) |
| `AZ-P1-009` | Grow a main pool that starts below its tagged minimum | [phase_test.go](suites/scalephase/phase_test.go) |

PDB cases establish placement and disruption allowance before their observation
windows. Readiness is sampled, not an uninterrupted-availability guarantee.
`AZ-P1-004` sets `nodeTaintsPolicy: Honor` on its hard hostname spread
constraint so a NoSchedule drain candidate does not remain an empty topology
domain. Structured report entries capture the PDB generation, disruption
allowance and run-owned Pod placement before and after relaxation, plus a final
best-effort capture before namespace cleanup.
`CA-005` and `CA-020` cleanup restores baseline without independently checking
every removed NIC; cases claiming physical deletion use explicit VM/Node/NIC
assertions. DRA growth and the Azure Disk case are main-only, not
scale-from-zero.

The suite covers public Delete-mode cases and the specified taint case. It does
not cover all AKS behavior, deallocate mode or every Azure backend. Read older
live outcomes at their recorded test and runtime commits. New source changes
do not inherit live credit from an earlier run.

See [source provenance](../../../docs/provenance.md) for upstream source
attribution and compatibility scope.
