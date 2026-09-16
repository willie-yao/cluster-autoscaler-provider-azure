# Azure E2E tests

This nested Go module runs Ginkgo tests against an **already prepared,
disposable environment**. It does not provision a cluster, deploy/reconfigure
Cluster Autoscaler, change cloud resources directly, or destroy infrastructure.
The operator retains responsibility for setup, single-controller ownership,
budget monitoring and infrastructure cleanup.

The initial runnable fixture is Linux VMSS Uniform with main min/max `1/2`,
zero min/max `0/1`, and one control plane outside both groups. Its ceiling is
four VMs and eight vCPUs, counted using Azure instances, requested capacity and
SKU core counts. This fixture limitation is not a provider support restriction:
standard pools, Flex, VMs-pool, AKS and optional scheduler/DRA configurations
need their own qualified fixtures and execution evidence.

## Local validation

```sh
make test-e2e-local # from the repository root
# Or, from this directory:
make test-local
```

These run race-enabled fake/unit checks and compile the `e2e`-tagged suite.
They do not authenticate to Azure or contact Kubernetes. Ordinary PR CI runs
the same target through `make test-ci`. The nested module's existing dependency
and toolchain pins are unchanged. Compilation is not live E2E evidence.

## Operator contract

Use a fresh cluster, explicit kubeconfig/context, one controller managing only
the intended pools, and an exact candidate image. Keep non-DaemonSet system
workloads on the control plane. Disable VMSS overprovisioning. Both scale sets
and the control-plane VM must have the `autoscaler-e2e-run=<runID>` Azure tag.
The dedicated worker resource group may contain only the two selected VMSS,
and optionally the named control-plane VM. Other infrastructure is not deleted
or inventoried by these tests.

Both VMSS must have `cluster-autoscaler-name=<discoveryValue>`, and `min`/`max`
tags matching `1`/`2` and `0`/`1`. The autoscaler must use exactly:

```text
--node-group-auto-discovery=label:cluster-autoscaler-name=<discoveryValue>
```

The test runner verifies the controller's fresh status ConfigMap contains
exactly these two groups and its fresh lease belongs to the single Ready
controller Pod. It does not read cloud Secrets or claim to detect every
possible external controller. The operator must disable any managed or other
autoscaler that could share these pools.

Use ordinary Delete mode, preserve PDB, system-pod and local-storage
protections, and configure scale-down delays externally to fit the test:
`scale-down-delay-after-add`, `scale-down-unneeded-time` and
`unremovable-node-recheck-timeout` must be explicit and at most one minute. Leave
`scale-down-utilization-threshold` at its default `0.5`. Five-minute negative
observations are not meaningful with longer delays. Nothing in the runner
changes these settings or installs a Helm release.

Create `kube-system/autoscaler-e2e-authorization` yourself. Its data must contain
exact matching `run-id`, `subscription-id`, `resource-group` and `cluster-uid`
values. `cluster-uid` is the UID of the **kube-system Namespace**. Tests cannot
self-authorize by creating this marker. A missing or mismatched marker fails
before any workload mutation.

Supply a non-secret JSON file based on [environment.example.json](environment.example.json).
Every field is required, unknown fields fail, and kubeconfig must be absolute.
Do not commit the real kubeconfig, credentials, bootstrap material or private
keys. Azure observations use the existing SDK's `DefaultAzureCredential`.
Azure errors retain HTTP status/code but omit response bodies that might
contain bootstrap data.

The workload image must provide `sh` and `sleep`; pin its digest. The tests use
idle containers with scheduling requests rather than consuming the requested
CPU. `demandCPU` is measured against actual worker allocatable CPU and existing
Pod requests: one Pod must fit, two must not. Unknown init-container or
Pod-level request accounting fails instead of making a guessed calculation.

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

The original `FOCUS` selector is also supported. Ginkgo parallel execution is
rejected. JUnit and report entries record outcomes and redacted count/deletion
observations. Do not treat a filtered-out or unexecuted spec as passing.

| Test ID | Assertions |
| --- | --- |
| `AZ-P1-001` | Exact fresh discovery, one leader, Azure ownership/bounds, five-minute `1/0` idle stability |
| `AZ-P1-002` | CPU geometry, main `1 -> 2`, two Ready Pods on distinct real workers, third scheduler-rejected Pod stays Pending for two minutes at max, physical return to `1` |
| `AZ-P1-003` | Zero-pool `0 -> 1`, predicted label matches actual Ready Node/workload, physical `1 -> 0` |
| `AZ-P1-004` | Two protected Pods on separate workers, zero allowed disruptions blocks deletion for five minutes, one allowed disruption permits physical deletion and rescheduling while at least one replica remains Ready at each observation |
| `AZ-SUP-ETAG` | `AZ-P1-002` semantics with operator-enabled `AZURE_ENABLE_VMSS_ETAG=true`; supplemental retained example, not a scenario at the selected public inventory pin |

Physical deletion requires captured Azure VM instance IDs, their Kubernetes
Nodes and their NIC resource IDs to disappear. A successful capacity update,
authentication failure or unreadable NIC is not deletion evidence.

Each spec creates a generated namespace marked with its run ID. Cleanup checks
the original Namespace UID and marker before deletion, waits for removal, and
waits for pool baseline. A failure is reported and evidence preserved; there is
no `AfterSuite` cluster deletion and no forced finalizer, PDB or eviction bypass.
Infrastructure cleanup remains operator-owned even after a test failure.

## Coverage and release boundaries

These Phase 1-derived tests are supplemental acceptance cases. The Phase 2
public-source inventory is pinned separately to
`Azure/autoscaler@d892fba1cf557b26d45540f2f6418b7ae52cca46`, a 1.35-line test
source, not a runtime or Kubernetes support baseline. Mapping all 23 discovered
registrations and their feature/resource requirements is an ongoing Phase 2
gate. No claim covers private/internal AKS product tests. DRA is not deallocate;
deallocate implementation remains Phase 3.

Chart version `9.59.0`, inherited `appVersion: 1.35.0`, official registry and
publication identity remain separate unresolved decisions. This suite does
not suppress the chart version-increment gate or approve a release.
