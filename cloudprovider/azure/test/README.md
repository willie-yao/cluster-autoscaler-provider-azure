# Azure E2E tests

This nested Go module runs Ginkgo tests against an **already prepared,
disposable environment**. The Go runner does not provision a cluster,
deploy Cluster Autoscaler, change cloud resources directly, or destroy
infrastructure. The operator retains responsibility for setup,
single-controller ownership, budget monitoring and cleanup.

The main path is the CAPZ Prow job on AKS. CAPZ creates an AKS
cluster through ASO, as the upstream `kubernetes/autoscaler` Azure job does,
and [hack/ci-e2e.sh](hack/ci-e2e.sh) deploys the autoscaler and runs one shard of
the default specs. See [CAPZ Prow jobs](#capz-prow-jobs).

The default fixture is Linux VMSS Uniform with main min/max `1/2`,
zero min/max `0/1`, and one control plane outside both groups. The CAPZ Prow
jobs use the same two pools on AKS, where a one-node AKS System pool takes the
place of the control plane. The ceiling for both is
four VMs and eight vCPUs, counted using Azure instances, requested capacity and
SKU core counts. This fixture limitation is not a provider support restriction:
standard pools, Flex and VMs-pool need their own qualified fixtures and
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

## Local validation

Use Go 1.26 for the E2E module's Kubernetes 1.37 libraries.

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

## CAPZ Prow jobs

The Prow jobs follow the upstream `pull-cluster-autoscaler-e2e-azure-master`
job. The job runs CAPZ `release-1.27` `scripts/ci-entrypoint.sh`, which
creates an AKS cluster through CAPZ's ASO types from
[templates/cluster-template-prow-aks-aso-e2e.yaml](templates/cluster-template-prow-aks-aso-e2e.yaml).
That template is a copy of the upstream
[templates/cluster-template-prow-aks-aso-cluster-autoscaler.yaml](templates/cluster-template-prow-aks-aso-cluster-autoscaler.yaml),
which stays unchanged, with the node pools changed to the shape the suite
expects:

- `pool0` is the System pool with one node and no autoscaler tags. It runs
  the AKS add-ons and the autoscaler, so it takes the place of the control
  plane VM in the VMSS fixture.
- `main` is a Linux User pool that starts with one node and has `min` and
  `max` tags of `1` and `2`.
- `zero` is a Linux User pool that starts empty and has `min` and `max`
  tags of `0` and `1`.

Both User pools carry the upstream discovery tags, the run tag, the
`acceptance-pool` node label and the node-template tag for that label. AKS's
own autoscaler stays off. Every MachinePool has the upstream
`cluster.x-k8s.io/replicas-managed-by: cluster-autoscaler` annotation, so CAPZ
doesn't reset a pool after the autoscaler scales it. The User pools set
`maxPods: 110`, because the Azure CNI default of 30 Pods per node can't fit
the 100-Pod cases on two nodes. Set `ADDITIONAL_ASO_CRDS` to
`authorization.azure.com/*;managedidentity.azure.com/*` as the upstream job
does, and set `KUBERNETES_VERSION` to a version that AKS offers in the job's
region. Keep one fresh cluster per job and let the CAPZ entrypoint clean it up.

From this directory, the command after CAPZ cluster creation is:

```sh
make test-e2e TAG="$(git rev-parse --short HEAD)" \
  REGISTRY="$REGISTRY" LABEL_FILTER=smoke ARTIFACTS="$ARTIFACTS"
```

`IMAGE=<registry>/<repository>` can replace `REGISTRY`, and it must not
include a tag. `build-e2e` uses the root `image` target with `GOARCH=amd64`
and pushes `IMAGE:TAG`. `test-e2e` then runs [hack/ci-e2e.sh](hack/ci-e2e.sh),
which takes these steps:

1. It reads the node resource group from the ASO `ManagedCluster` and the
   client ID from the ASO `UserAssignedIdentity` in the CAPZ management
   cluster, as the upstream Makefile does.
2. It finds the three VMSS in the node resource group by their
   `aks-managed-poolName` tag and checks their tags and sizes. It fails if
   the AKS autoscaler is on for any pool, or if a cluster autoscaler already
   runs in the cluster.
3. It waits up to 45 minutes for AKS to finish setting up the cluster, as
   described below.
4. It stops AKS from resizing metrics-server when the Node count changes, as
   described below.
5. It drains the User pool nodes once, so that AKS add-on replicas move to
   the System pool, and then it uncordons them.
6. It creates the authorization marker and the run's two PriorityClasses.
7. It installs one controller from this repository's chart with the upstream
   values: the `cluster-autoscaler` release in the `default` namespace,
   workload identity and `autoDiscovery.clusterName`. It pins the Pod to the
   System pool. It also replaces the discovery flag and the Secret references
   for `ARM_SUBSCRIPTION_ID` and `ARM_RESOURCE_GROUP` with the literal values
   that the suite checks.
8. It writes a JSON binding with `resourceGroupMode: aks` in `ARTIFACTS` and
   runs Ginkgo with the selected label filter. Ginkgo writes
   `junit.e2e_suite.1.xml` there.

AKS keeps changing a new cluster for a while after CAPZ reports it Ready.
About 12 to 35 minutes after AKS creates the pools, it rolls out new revisions
of kube-system add-ons, e.g., `konnectivity-agent`, and it adds the
`AKSLinuxExtension` VM extension to each VMSS. It then upgrades the VMSS
instances to that model, so each VMSS is `Updating` for about a minute. If the
tests start before AKS finishes, add-on Pods can land on the drained User pool
nodes, which fails the worker isolation check, and a VMSS in `Updating` fails
the idle case. So `hack/ci-e2e.sh` waits until all of the following hold on three
checks in a row, 30 seconds apart:

- The AKS cluster and its three agent pools are `Succeeded`.
- The three VMSS are `Succeeded` and have `AKSLinuxExtension` in their model,
  and every VMSS instance is `Succeeded` and on the latest model.
- The number of Ready Nodes matches the total VMSS capacity.
- Every kube-system Deployment has observed its latest generation and has all
  of its replicas updated and available.

The cluster and agent pool checks follow the provisioning state checks in
CAPZ's AKS tests. The VMSS state and Node count checks follow upstream's
`AllVMSSStable`, and the Deployment check is similar to the Pod wait in CAPZ's
`ci-entrypoint.sh`. If AKS doesn't finish within 45 minutes, the script fails
and prints the last condition that wasn't met.

AKS runs metrics-server with a `metrics-server-vpa` sidecar that resizes it
when the Node count changes by more than 5%. Each resize rolls out new
metrics-server Pods, and they can land on a drained User pool node when a
test adds or removes a node, which fails the worker isolation check. Before
the drain, `hack/ci-e2e.sh` follows AKS's
[Manually configure Metrics Server resource usage](https://learn.microsoft.com/azure/aks/use-metrics-server-vertical-pod-autoscaler#manually-configure-metrics-server-resource-usage):
it creates the `kube-system/metrics-server-config` ConfigMap with the current
metrics-server CPU and memory requests as `baseCPU` and `baseMemory`, and with
`cpuPerNode: 0m` and `memoryPerNode: 0Mi`, and it restarts metrics-server to
load it. It then checks that the rollout finished, that the Deployment didn't
change during the next minute, and that each new sidecar logged the new
values.

The upstream suite installs the chart from Go in `BeforeSuite`. This suite
keeps the install in `hack/ci-e2e.sh`, because its Go runner only observes a
prepared cluster and never installs or changes the controller. The Prow host
needs Docker, Azure CLI, `kubectl`, `helm`, `jq`, `yq`, Go, Azure credentials,
registry access and the two CAPZ kubeconfig files. `test-e2e` defaults to
`smoke` if no filter is passed. The `e2etests` target still takes an
operator-prepared binding without deploying anything.

In AKS mode, the runner accepts the node resource group that AKS shares with
its load balancer, network security group and other resources. It checks only
the VMSS in that group, which must be exactly the named System, main and zero
VMSS. The System pool must not carry the `cluster-autoscaler-name` tag, and
its VMs count toward the four-VM and eight-vCPU budget in place of the control
plane VM. The main and zero pools must have the run, discovery, `min` and `max`
tags. Any other VMSS, a standalone VM, or a Node outside these three pools fails
the read. The binding names the System VMSS in `systemPool` and omits
`controlPlaneID`. Omit `resourceGroupMode` for the strict operator fixture
described below.

No default case depends on the control plane VM, so no default case skips in
AKS mode. Six of them still need preparation that Prow doesn't do, as the
shard list below shows.

The presubmit runs `smoke` (`AZ-P1-001` to `AZ-P1-003`), three cases. The two
scaling cases can each take up to 50 and 40 minutes, so an hour is a target,
not a guaranteed bound. The periodic jobs run shards A to F on AKS with these
label filters. Shard A is the same three-case smoke filter. Each shard uses a
fresh AKS cluster, `TEST_SUITE=scaleup`, and
the same five-hour Prow timeout. The case timeouts sum to 97 to 180 minutes
per shard, and the remaining time is for cluster setup and cleanup.

| Shard | Label filter | Cases |
| --- | --- | ---: |
| A | `AZ-P1-001 || AZ-P1-002 || AZ-P1-003` | 3 |
| B | `AZ-P1-004 || AZ-001 || CA-001 || CA-002` | 4 |
| C | `CA-004 || CA-005 || CA-006 || CA-007` | 4 |
| D | `CA-008 || CA-009 || CA-010` | 3 |
| E | `CA-011 || CA-012 || CA-013 || CA-014` | 4 |
| F | `CA-015 || CA-016 || CA-017 || CA-018 || CA-019` | 5 |

Prow runs only these six shards, which cover 23 of the 29 default cases. The
other six cases need preparation that the AKS template and `test-e2e` don't
do yet, so the Prow path doesn't run them. Their filters are:

| Shard | Label filter | Cases | Preparation |
| --- | --- | ---: | --- |
| G | `AZ-SUP-ETAG` | 1 | ETag-enabled controller and marker |
| H | `AZ-P1-005` | 1 | Tainted zero pool |
| I | `AZ-P1-006` | 1 | Azure Disk CSI, StorageClass and marker |
| J | `CA-020 || CA-021 || CA-022` | 3 | Synthetic DRA driver and marker |

Together, the ten filters cover all 29 default cases. Do not add G to J to
Prow until their CAPZ preparation exists and each has passed a live check.
The AKS template's zero pool has no taint, because `AZ-P1-003` runs a Pod
without a toleration in that pool. The `Slow`
label marks longer cases, and the upstream `Feature:ClusterSizeAutoscalingScaleUp`
and `Feature:ClusterSizeAutoscalingScaleDown` labels mark the matching `CA-`
cases. The proposed job YAML is
kept outside the repo until the repo owner, Prow org, registry and CI image are
chosen.

## Current and legacy workflows

Outside Prow, prepare the fixture and deploy Cluster Autoscaler
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
workloads on the control plane, or on the System pool in AKS mode. Disable VMSS overprovisioning. Both scale sets
and the control-plane VM must have the `autoscaler-e2e-run=<runID>` Azure tag.
The dedicated worker resource group may contain only the two selected VMSS,
and optionally the named control-plane VM. Other infrastructure is not deleted
or inventoried by these tests. The AKS mode described under
[CAPZ Prow jobs](#capz-prow-jobs) uses the AKS System pool instead of the
control plane VM.

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

The workload image must provide `sh`, `sleep` and `printf`. Pin its digest. The
test Pods exit on SIGTERM, so deleting them does not wait for the grace
period. The tests use
idle containers with scheduling requests rather than consuming the requested
CPU. `demandCPU` is measured against actual worker allocatable CPU and existing
Pod requests: one Pod must fit, two must not. The Kubernetes request
helper accounts for init containers, restartable sidecars and overhead.
The suite rejects Pod-level requests instead of using a guessed
calculation. The helper uses the pinned `k8s.io/component-helpers v0.37.0` dependency.
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
The specs also carry the standard Kubernetes E2E labels. `Slow` marks cases
that run longer than two minutes. `Feature:ClusterSizeAutoscalingScaleUp` and
`Feature:ClusterSizeAutoscalingScaleDown` mark the matching `CA-` cases, as
upstream does. `smoke` selects `AZ-P1-001` to `AZ-P1-003`.
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
