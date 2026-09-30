# Azure E2E tests

This nested Go module runs Ginkgo tests against a prepared, disposable
cluster that runs one Cluster Autoscaler. The specs create and remove
Kubernetes test workloads, and their Azure calls only read. The runner doesn't
create infrastructure, deploy the autoscaler or resize a VMSS, so a scale-up
can pass only if the autoscaler did the work.

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

The specs rely on these autoscaler settings:

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
  four devices, and no other Node may publish any. The DRA scale-up spec
  needs the main worker to publish its devices before it grows, so it doesn't
  cover DRA scale-from-zero.
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
make -C test e2etests \
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

See [source provenance](../docs/provenance.md) for upstream source
attribution and compatibility scope.
