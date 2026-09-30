# Azure E2E tests

This nested Go module runs Ginkgo tests against a prepared, disposable
cluster that runs one Cluster Autoscaler. The specs create and remove
Kubernetes test workloads, and their Azure calls only read. The runner doesn't
create infrastructure, deploy the autoscaler or resize a VMSS, so a scale-up
can pass only if the autoscaler did the work.

The main path is the CAPZ Prow job on AKS. CAPZ creates an AKS cluster
through ASO, as the upstream `kubernetes/autoscaler` Azure job does, and
[hack/ci-e2e.sh](hack/ci-e2e.sh) deploys the autoscaler and runs one group of
the default specs. See [CAPZ Prow jobs](#capz-prow-jobs).

The suite expects two Linux VMSS Uniform pools, `main` with min/max `1/2`
and `zero` with min/max `0/1`, and runs the autoscaler outside them. Other
pool types, such as Flex and VMs pools, need their own setup and aren't
covered. Priority, scheduler, system-namespace, DRA, taint and disk cases
need the extra setup described below.

## Local validation

Use Go 1.26 for the E2E module's Kubernetes 1.37 libraries.

```sh
make test-e2e-local # from the repository root
# Or, from this directory:
make test-local
```

These run the race-enabled unit tests with fake clients, compile the
`e2e`-tagged suite and dry-run the Ginkgo registration without running
setup hooks or test bodies. They don't contact Azure or Kubernetes. Ordinary
PR CI runs the same target through `make test-ci`. Compilation is not live
E2E evidence.

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
  the AKS add-ons and the autoscaler.
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

The `taint` Prow job uses
[templates/cluster-template-prow-aks-aso-e2e-taint.yaml](templates/cluster-template-prow-aks-aso-e2e-taint.yaml)
instead, through CAPZ's `CLUSTER_TEMPLATE`. It differs only in the zero pool,
which has the `autoscaler-e2e-run=<CLUSTER_NAME>:NoSchedule` node taint and
the matching `k8s.io_cluster-autoscaler_node-template_taint_autoscaler-e2e-run`
tag. The other Prow jobs need the untainted zero pool.

From this directory, the command after CAPZ cluster creation is:

```sh
make test-e2e TAG="$(git rev-parse --short HEAD)" \
  REGISTRY="$REGISTRY" LABEL_FILTER=smoke ARTIFACTS="$ARTIFACTS"
```

Some Prow jobs also set `E2E_PREPARE`, as the table below shows.

`IMAGE=<registry>/<repository>` can replace `REGISTRY`, and it must not
include a tag. `build-e2e` uses the root `image` target with `GOARCH=amd64`
and pushes `IMAGE:TAG`. `test-e2e` then runs [hack/ci-e2e.sh](hack/ci-e2e.sh),
which takes these steps:

1. It reads the node resource group from the ASO `ManagedCluster` and the
   client ID from the ASO `UserAssignedIdentity` in the CAPZ management
   cluster, as the upstream Makefile does.
2. It finds the three VMSS in the node resource group by their
   `aks-managed-poolName` tag and checks their tags and sizes, and the zero
   pool's taint for the `taint` job. It fails if the AKS autoscaler is on for
   any pool.
3. It waits up to 45 minutes for AKS to finish setting up the cluster, as
   described below, and fails if a cluster autoscaler already runs in the
   cluster.
4. It stops AKS from resizing metrics-server when the Node count changes, as
   described below.
5. It drains the User pool nodes once, so that AKS add-on replicas move to
   the System pool, and then it uncordons them.
6. It prepares what `E2E_PREPARE` selects, then creates the run's two
   PriorityClasses.
7. It installs one controller from this repository's chart with
   `helm upgrade --install --wait` and the upstream values: the
   `cluster-autoscaler` release in the `default` namespace, workload identity
   and `autoDiscovery.clusterName`. It pins the Pod to the System pool and
   sets the controller flags that the specs rely on, listed under
   [Running the suite](#running-the-suite).
8. It waits up to 5 minutes for the autoscaler's status ConfigMap to report
   `Running`, writes the JSON binding in `ARTIFACTS` and runs Ginkgo with the
   selected label filter. Ginkgo writes `junit.e2e_suite.1.xml` there.

AKS keeps changing a new cluster for a while after CAPZ reports it Ready.
About 12 to 35 minutes after AKS creates the pools, it rolls out new revisions
of kube-system add-ons, e.g., `konnectivity-agent`, and it adds the
`AKSLinuxExtension` VM extension to each VMSS. It then upgrades the VMSS
instances to that model, so each VMSS is `Updating` for about a minute. If the
tests start before AKS finishes, add-on Pods can land on the drained User pool
nodes, which fails the worker isolation check, and a VMSS in `Updating` fails
the stable-pool check. So `hack/ci-e2e.sh` waits until all of the following
hold on three checks in a row, 30 seconds apart:

- The AKS cluster and its three agent pools are `Succeeded`.
- The three VMSS are `Succeeded` and have `AKSLinuxExtension` in their model,
  and every VMSS instance is `Succeeded` and on the latest model.
- The number of Ready Nodes matches the total VMSS capacity.
- Every kube-system Deployment has observed its latest generation and has all
  of its replicas updated and available.
- The `konnectivity-agent` Deployment's Pod template has the
  `checksum/service-account-key` annotation, and that revision has rolled
  out. In live runs, AKS added it about 25 minutes after creating the pools,
  which could be after the other checks passed.

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
keeps the install in `hack/ci-e2e.sh`, so that the Go runner never installs
or changes the controller. The Prow host needs Docker, Azure CLI, `kubectl`,
`helm`, `jq`, Go, Azure credentials, registry access and the two CAPZ
kubeconfig files. `test-e2e` defaults to `smoke` if no filter is passed.

The presubmit runs the three `smoke` specs: idle discovery, scale-up to the
node group maximum with deletion, and scale from zero. The two scaling specs
can each take up to 50 and 40 minutes, so an hour is a target, not a
guaranteed bound.

The periodic Prow jobs run these label filters on AKS. The `smoke` job is the
presubmit filter. Each job uses a fresh AKS cluster, `TEST_SUITE=scaleup` and
the same five-hour Prow timeout. The spec timeouts sum to 45 to 180 minutes
per job, and the remaining time is for cluster setup and cleanup.

| Prow job | Label filter | Specs | Preparation |
| --- | --- | ---: | --- |
| smoke | `smoke` | 3 | None |
| pdb-demand | `pdb \|\| cpu \|\| memory` | 4 | None |
| placement | `host-port \|\| anti-affinity \|\| emptydir \|\| scale-down` | 4 | None |
| drain | `drain` | 3 | None |
| system-priority | `system-pods \|\| (priority && Feature:ClusterSizeAutoscalingScaleUp)` | 4 | None |
| priority-scheduler | `scheduler \|\| (priority && Feature:ClusterSizeAutoscalingScaleDown)` | 5 | None |
| etag | `etag` | 1 | `E2E_PREPARE=etag` |
| taint | `taint` | 1 | `E2E_PREPARE=taint` and the taint template |
| disk | `disk` | 1 | `E2E_PREPARE=disk` |
| dra | `dra` | 3 | `E2E_PREPARE=dra` |

The ten Prow jobs cover all 29 default specs. Each preparation adds only what
its job's specs check:

- `etag` sets the chart's `azureEnableVMSSEtag: true`, so the controller runs
  with `AZURE_ENABLE_VMSS_ETAG=true`.
- `taint` checks that the zero pool has the run taint and tag from the taint
  template.
- `disk` checks AKS's built-in Azure Disk CSI driver, creates the
  `<CLUSTER_NAME>-disk` StorageClass and writes `diskStorageClass` in the
  binding.
- `dra` installs [hack/dra-driver.yaml.in](hack/dra-driver.yaml.in) on the
  main pool and waits for its four devices on the main worker.

The proposed job YAML is kept outside the repo until the repo owner, Prow
org, registry and CI image are chosen.

## Running the suite

To run the suite on your own cluster, prepare a fresh cluster with the two
pools and one autoscaler, then run [focused cases](#focused-execution) with
`ENVIRONMENT` pointing to a JSON binding like
[environment.example.json](environment.example.json). Unknown fields fail,
and `kubeconfig` must be an absolute path. `resourceGroup` is
the group of the two VMSS, `runID` names the run's labels, taint and
PriorityClasses, and `diskStorageClass` is needed only for the `disk` spec.
Azure reads use the SDK's `DefaultAzureCredential`, and errors keep the HTTP
status and code but not the response body.

The runner checks the following before each spec and during each
"no growth" window:

- The autoscaler Deployment has one replica and one Ready Pod that runs
  `expectedImage`, and its status ConfigMap is less than two minutes old,
  `Running` and lists exactly the main and zero VMSS.
- The main and zero VMSS are `Succeeded`, and their capacity, VMs and Ready,
  schedulable Nodes with the pool label match. A pool above its `max` tag
  stops the spec at once.
- At the start of each spec, only DaemonSet Pods run on the main and zero
  workers.

Deletion counts only when the captured VM, its Node and its NICs are all
gone. A capacity change or an unreadable NIC is not deletion evidence.

The specs rely on these autoscaler settings, which `hack/ci-e2e.sh` also
uses:

- Label discovery that selects only the two pools, ordinary Delete mode and
  leader election with the status ConfigMap.
- `scale-down-delay-after-add`, `scale-down-unneeded-time` and
  `unremovable-node-recheck-timeout` of at most one minute, and the default
  `scale-down-utilization-threshold` of `0.5`. The five-minute "no growth"
  windows mean nothing with longer delays.
- `skip-nodes-with-local-storage` and `skip-nodes-with-system-pods` left on.
- `--expendable-pods-priority-cutoff=-10` for the priority cases and
  `--bypassed-scheduler-names=non-existing-bypassed-scheduler` for the
  bypassed-scheduler specs.

Some cases need extra setup:

- The `priority` specs need the `<runID>-expendable` and `<runID>-high`
  PriorityClasses with values `-15` and `1000`.
- The `system-pods` spec creates a run-labeled Deployment and PDB in
  `kube-system` and deletes them afterwards.
- The `dra` specs need the DRA example driver as the
  `dra-example-driver-kubeletplugin` DaemonSet in `kube-system`, with
  `DRIVER_NAME=gpu.example.com` and `NUM_DEVICES=4`, and a `gpu` DeviceClass
  that selects `device.driver == 'gpu.example.com'`. Each worker must publish
  four devices, and no other Node may publish any.
  [hack/dra-driver.yaml.in](hack/dra-driver.yaml.in) installs both on the
  main pool. The DRA scale-up spec needs the main worker to publish its
  devices before it grows, so it doesn't cover DRA scale-from-zero.
- The `taint` spec needs the zero pool tag
  `k8s.io_cluster-autoscaler_node-template_taint_autoscaler-e2e-run=<runID>:NoSchedule`
  and the same taint on the pool's Nodes. Run the other zero-pool specs on an
  untainted zero pool.
- The `disk` spec needs the Azure Disk CSI driver and a StorageClass with
  `volumeBindingMode: WaitForFirstConsumer`. Both main workers must be in the
  same zone. The runner also needs permission to exec into its own Pods.

The workload image must provide `sh`, `sleep` and `printf`. Pin its digest.
The test Pods request CPU or memory and sleep. `demandCPU` must be a CPU
request that fits once on a worker, but not twice. The runner measures this
against the worker's allocatable CPU and existing Pod requests, and rejects
Pods with Pod-level requests. Use the same worker SKU in both pools.

With an Azure CLI login, unset stale `AZURE_CLIENT_ID`, `AZURE_CLIENT_SECRET`
and `AZURE_TENANT_ID` variables for the runner process, because they take
precedence over the login.

The legacy `setup-cluster`, `deploy-local`, `deploy-local-dev` and
`validate-env` targets belong to a separate AKS development workflow. They
don't prepare a cluster for this suite.

## Focused execution

Run one scenario at a time:

```sh
make -C cloudprovider/azure/test e2etests \
  TEST_SUITE=scaleup \
  ENVIRONMENT=/absolute/path/to/environment.json \
  ARTIFACTS=/absolute/path/to/artifacts \
  LABEL_FILTER=max-size \
  TEST_TIMEOUT=90m
```

`TEST_TIMEOUT` is the Ginkgo suite timeout. The Make target runs Ginkgo, not
`go test` with its default ten-minute timeout. If you run `go test`
directly, set both `-timeout` and `-ginkgo.timeout`. `FOCUS` also selects
specs. Ginkgo parallel runs are rejected, an empty selection fails and the
first failure stops the run. The JUnit and JSON reports record the runtime
image, the pools and the captured VM, Node and NIC IDs. Use a separate
artifact directory for each run.

Each spec creates a run-labeled namespace. Cleanup deletes it, waits for it
to go and waits for the pools to return to `1/0`. There is no forced
finalizer, PDB or eviction bypass, and the suite doesn't delete
infrastructure.

Each spec has a topic label, such as `pdb`, `priority` or `dra`, to select it
in label filters. `Slow` marks specs that run longer than two minutes,
and the upstream `Feature:ClusterSizeAutoscalingScaleUp` and
`Feature:ClusterSizeAutoscalingScaleDown` labels mark the specs from the
public Azure inventory. Specs that need three workers use both pools, so
main and zero grow to `2/1`.

The Azure-specific specs check these results:

| Label | Assertions |
| --- | --- |
| `idle` | Exact discovery, one controller, five-minute `1/0` idle stability |
| `max-size` | CPU geometry, main `1 -> 2`, two Ready Pods on distinct real workers, third scheduler-rejected Pod stays Pending for two minutes at max, physical return to `1` |
| `scale-from-zero` (smoke) | Zero-pool `0 -> 1`, predicted label matches actual Ready Node/workload, physical `1 -> 0` |
| `pdb` | Two protected Pods on separate workers, zero allowed disruptions blocks deletion for five minutes, one allowed disruption permits physical deletion and rescheduling while at least one replica remains Ready at each observation |
| `taint` | A zero-pool VMSS tag blocks demand without a matching taint toleration for five minutes, then tolerated demand grows the pool and the tainted Node runs the Pod; physical return to zero |
| `disk` | Main `1 -> 2 -> 1`, two Ready StatefulSet Pods use distinct Azure Disks, one Pod and its disk move to the survivor without losing the file, and VM/Node/NIC deletion is verified |
| `etag` | The `max-size` checks with `AZURE_ENABLE_VMSS_ETAG=true`, kept from the upstream suite |

## Coverage inventory

The public scenario source is
`Azure/autoscaler@d892fba1cf557b26d45540f2f6418b7ae52cca46`, a public 1.35-line
test source, not the runtime or Kubernetes support baseline. The suite
implements its 22 active scenarios, and one scenario that the source disables
as flaky is left out. One disk spec adapts a separate public source, and six
Azure-specific specs bring the default suite to 29 registered specs.
Registration is not execution.

| Labels | Maintained intent | Source file |
| --- | --- | --- |
| `cpu`, `memory` | CPU demand, oversized request refusal, memory demand | [public_test.go](suites/scaleup/public_test.go) |
| `host-port`, `anti-affinity`, `emptydir` | Host-port conflict, hostname anti-affinity, pending EmptyDir workload | [public_test.go](suites/scaleup/public_test.go) |
| `scale-down` | Physical worker deletion after demand disappears | [public_test.go](suites/scaleup/public_test.go) |
| `drain` | PDB-permitted drain, blocked deletion, sequential disruption | [public_test.go](suites/scaleup/public_test.go) |
| `system-pods` | Run-labeled synthetic system-namespace workload and PDB | [system_test.go](suites/scaleup/system_test.go) |
| `priority` | Expendable and non-expendable demand, preemption and retention | [priority_test.go](suites/scaleup/priority_test.go) |
| `scheduler` | Bypassed and unconfigured scheduler demand | [scheduler_test.go](suites/scaleup/scheduler_test.go) |
| `dra` | Synthetic DRA growth, oversized claim refusal, deletion and reallocation | [dra_test.go](suites/scaleup/dra_test.go) |
| `taint` | Zero-pool template taint blocks demand without a toleration and permits tolerated demand | [zero_taint_test.go](suites/scaleup/zero_taint_test.go) |
| `disk` | Azure Disk StatefulSet Pods retain claims and data when a worker is deleted | [disk_test.go](suites/scaleup/disk_test.go) |

Scope adaptations include real expendable demand in the expendable Pod spec,
sampled readiness in the PDB and priority windows, and eight-device,
main-only DRA growth in the DRA scale-up spec. The `pdb` spec sets
`nodeTaintsPolicy: Honor` on its hard hostname spread constraint so a
NoSchedule drain candidate does not remain an empty topology domain, and it
records the PDB state before and after relaxation. The anti-affinity and DRA
scale-up specs restore the baseline without checking every removed NIC;
specs that claim physical deletion check the VM, Node and NICs.

The suite covers public Delete-mode scenarios and the specified taint spec.
It does not cover all AKS behavior, deallocate mode or every Azure backend.
Read older live outcomes at their recorded test and runtime commits. New
source changes do not inherit live credit from an earlier run.

See [source provenance](../../../docs/provenance.md) for upstream source
attribution and compatibility scope.
