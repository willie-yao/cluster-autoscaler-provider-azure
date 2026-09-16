# Phase 2 public-source E2E matrix

**Phase 2 is not complete.** Runnable tests, required review and live execution
are separate gates. No Phase 2 live pass is recorded in this document yet.
Phase 1 evidence does not substitute for running these new tests.

The accepted inventory source is
[`Azure/autoscaler@d892fba1cf557b26d45540f2f6418b7ae52cca46`](https://github.com/Azure/autoscaler/commit/d892fba1cf557b26d45540f2f6418b7ae52cca46),
`cluster-autoscaler-release-1.35.2-aks`, committed September 1, 2026. This is a
**1.35-line test source**, not a change to the approved upstream application,
extracted core, module identity, dependency baseline or Kubernetes 1.37 target.
Both inventoried test trees are identical at prior commit
`8301ef4ef8e410849b0b95fbbf995660fbcfb696`.

The inspected default `master@efc9b34ee20f0f4645f9d53d47e06b2841cd0bb2`
has an older Azure test layout and 19 generic registrations. The accepted
release contains one Azure smoke and 22 generic registrations, including three
optional DRA cases. No separate public AKS product suite or deallocate E2E was
found. Registration counts are not pass counts and say nothing about private
AKS product test coverage.

## Source-to-test mapping

`B` means eligible baseline workers, excluding the control plane. The prepared
fixture has `B=1`; across its two owned pools it can reach three workers plus
one control plane within four VMs/eight vCPUs. No pool maximum is changed by
tests. Memory-based cases measure worker allocatable memory and existing Pod
requests rather than using control-plane memory or a blind constant.

Every implemented row is independently selectable with `LABEL_FILTER=<ID>`.
Definitions are in `cloudprovider/azure/test/suites/scaleup/`. Implemented does
not mean reviewed, executed, or passed.

| ID and pinned source | Intent and semantic assertions | Test file | Fixture / status |
| --- | --- | --- | --- |
| [AZ-001][az] | 100 Pods at 200m CPU cause actual owned-worker growth, Ready test Pods use all three workers, excess demand remains Pending, deletion restores baseline with VM/Node/NIC evidence. Full 100-Pod readiness is not a source assertion and would exceed this budget. | `public_test.go` | Implemented; unexecuted. Bounded adaptation of original 20-core pressure. |
| [CA-001][ca001] | A 1.1x worker-memory Pod stays scheduler-rejected, emits `NotTriggerScaleUp` for its exact UID and does not grow the eligible pool for five minutes. | `public_test.go` | Implemented; unexecuted. Main-pool selector restricts candidate size. |
| [CA-002][ca002] | 100 small Pods totaling worker allocatable memory cause growth; all 100 are Ready on real workers; cleanup physically shrinks. | `public_test.go` | Implemented; unexecuted. Measured background requests must make the baseline insufficient. |
| [CA-003][ca003] | Avoid duplicate scale-ups while an operation is processing. | None | Explicit source gap: unconditionally disabled as flaky. An idle/status check is not a replacement. No passing empty/skip test added. |
| [CA-004][ca004] | Three Pods reserve the same host port 4321, require three distinct actual Ready workers, and all become Ready. | `public_test.go` | Implemented; unexecuted. `B+2` across both owned pools, host-port admission required. |
| [CA-005][ca005] | Required hostname anti-affinity grows from one to three Pods on three distinct workers. | `public_test.go` | Implemented; unexecuted. `B+2`, namespace-owned constraints only. |
| [CA-006][ca006] | A pending Pod with an EmptyDir and anti-affinity triggers growth and runs on a second worker. | `public_test.go` | Implemented; unexecuted. Local-storage scale-down protection remains enabled. |
| [CA-007][ca007] | Remove three-worker pressure and require physical deletion back to baseline, not just desired-capacity change. | `public_test.go` | Implemented; unexecuted. `B+2`. |
| [CA-008][ca008] | One movable Pod per worker, PDB permits one disruption; drain preserves at least N-1 Ready replicas at each observation and all replicas recover on the surviving worker. | `public_test.go` | Implemented; unexecuted. Preferred spread replaces source-wide taint mutations; actual initial distribution is asserted. |
| [CA-009][ca009] | PDB permits no disruptions; captured instances and Ready workload remain unchanged throughout five minutes. | `public_test.go` | Implemented; unexecuted. Scale-down timings are explicit and at most one minute. |
| [CA-010][ca010] | Two movable Pods per worker, one permitted disruption; multi-Pod drain preserves N-1 Ready replicas at each observation and reschedules all six. | `public_test.go` | Implemented; unexecuted. `B+2`, PDB budget must replenish. |
| [CA-011][ca011] | The multi-Pod PDB drain runs with synthetic test-owned kube-system objects, preserving actual system addons and protections. | `system_test.go` | Implemented; unexecuted. Explicit `allow-kube-system-fixture: CA-011` marker and no overlapping PDB selectors required. |
| [CA-012][ca012] | Real low-priority demand is created in the test body; one Ready and one Pending expendable Pod do not grow the pool. | `priority_test.go` | Implemented; unexecuted. Corrects source bug that creates workload only in deferred cleanup. |
| [CA-013][ca013] | Two high-priority reservations cause growth and become Ready on distinct workers. | `priority_test.go` | Implemented; unexecuted. Operator-owned PriorityClasses/cutoff required. |
| [CA-014][ca014] | High-priority demand actually replaces the captured low-priority Pod; low-priority replacement remains Pending without growth. | `priority_test.go` | Implemented; unexecuted. Real scheduling preemption, not direct test eviction. |
| [CA-015][ca015] | Three expendable memory reservations do not prevent physical shrink; remaining demand is explicitly one Ready/two Pending. | `priority_test.go` | Implemented; unexecuted. `B+2`, source's running expendable workload intent retained. |
| [CA-016][ca016] | Non-expendable memory reservations keep captured workers and all three Pods Ready throughout five minutes. | `priority_test.go` | Implemented; unexecuted. Negative window exceeds the required timing profile. |
| [CA-017][ca017] | Unprocessed bypassed-scheduler demand that cannot fit triggers actual growth while remaining unprocessed. | `scheduler_test.go` | Implemented; unexecuted. Optional bypass profile, not Phase 3. |
| [CA-018][ca018] | Bypassed-scheduler demand measured to fit stays unprocessed without growth. | `scheduler_test.go` | Implemented; unexecuted. Optional bypass profile. |
| [CA-019][ca019] | Unprocessed demand under a unique, unconfigured scheduler name never triggers growth. | `scheduler_test.go` | Implemented; unexecuted. Can run as a baseline control without the bypass flag. |
| [CA-020][ca020] | Twelve one-device DRA Pods cause three-worker growth; all are Ready with unique, Pod-owned claim allocations matching four synthetic devices per worker. | `dra_test.go` | Implemented; unexecuted. Operator-prepared DRA API/driver/future-node simulation required, no GPU SKU. |
| [CA-021][ca021] | A five-device claim cannot fit any four-device worker; exact-Pod no-growth event, persistent Pending behavior and actual unallocated claim. | `dra_test.go` | Implemented; unexecuted. Explicit optional DRA profile, not deallocate. |
| [CA-022][ca022] | DRA anti-affinity grows to two workers, additional two-device Pods occupy both; removing pressure allows physical Delete-mode drain and actual claim reallocation on one worker. | `dra_test.go` | Implemented; unexecuted. Explicit optional DRA profile, not deallocate. |

## Supplemental and excluded coverage

The five `AZ-P1-001` through `AZ-P1-004` and `AZ-SUP-ETAG` cases are described
in the [runner guide](../cloudprovider/azure/test/README.md). They are not extra
registrations in the 23-row public inventory. They add discovery/idle,
min/max, zero-pool identity/deletion, and an explicit PDB release transition.
The retained ETag example is supplemental; the selected source has ETag
unit tests, not an ETag E2E registration.

Standard/availability-set pools, Flex, VMs-pool, Windows, spot, zone balancing,
managed AKS installation and Azure Disk/PVC have no dedicated registration
in this source inventory. They remain provider capabilities or possible
future coverage, not silently prohibited features or established live passes.
The retained CAPZ/ASO AKS/Windows template is setup provenance, not additional
scenario evidence.

Source unit tests for min/max accessors, request/poller failures, ETag
preconditions and mocked deallocate calls are unit-only evidence. Deallocate
dependent unit prior art is explicitly **Phase 3 gated**, not passing Phase 2
E2E. There are zero deallocate-dependent registrations in the 23-row inventory.
DRA's three optional cases have a different feature boundary and are not
assigned to Phase 3.

No restart/recovery E2E was discovered; the DRA runner's autoscaler redeployment
is setup, not a restart test. No live fault-injection or production capability
changes are introduced to fill invented source scenarios. VPA tests, GCE
provisioning wrappers and the Kubernetes dependency's full E2E corpus are
outside this Azure CA inventory.

## Evidence gates

The required actual `rubber-duck` review cycle completed at
`385de9cc6670adf1b3e3ab4cdfe8e17ffd9bbdd8`, using `gpt-6-astra` through the
working CLI. The app-hosted attempt failed its custom-agent prompt callback
and did not perform this review. The same actual reviewer context was retained
across all three rounds.

| Round | Reviewed range | Findings and disposition |
| --- | --- | --- |
| 1 | Phase 1 base through `49f159421` | Three accepted guards fixed: controller ARM scope, per-pool timing/utilization overrides, and peak SKU budget before scaling. Predetermined-victim machinery declined because generic public shrink cases permit choosing any eligible worker. Actual removed VM/Node/NIC identities are still verified. |
| 2 | Full Phase 2 diff through `e2038d649` | Earlier accepted fixes verified and scope decline accepted. One new per-pool bounds gap fixed: neither `main=3/zero=0` nor `main=1/zero=2` may pass through a valid three-worker total. |
| 3 | Focused `e2038d649..385de9cc6` with prior context | Per-pool bounds, exact runtime hostname lease identity and split-form timing argument fixes verified. No new findings. |

Calico init accounting and the live-discovered hostname mismatch were repaired
independently; they were not additional first-round findings. The subsequent
captured-status timestamp repair is an approved small compatibility fix with
focused local regression coverage, not a new broad review cycle.

Local race tests, tagged compilation and Ginkgo registration dry-runs cover
harness logic only. Completed code review does not establish live coverage. The sole live
operator must record exact source/candidate/config identity, selected ID,
JUnit result, cloud and Node observations, and cleanup outcome before any row
changes to passed. A failed or unavailable optional profile remains failed or
unexecuted. The initial `49f159421` demand cases are superseded by repaired
init-container request accounting; they are not assumed to have passed.
`df4ecc31ced963296a6ea4604457267a904be98e` is the repaired standard/public test
checkpoint. The runtime under test remains the separate production candidate
`48f997fcd8d4a93f25d06837e185ea77d4e2c5cd`, unless operator evidence explicitly
records a different approved runtime. Test-suite commits and runtime provenance
must not be conflated.

The first recorded live harness attempt, `AZ-P1-001-retry2`, used frozen tests
`e2038d649400890f960f64d1b9c8e3e7aba2a876` and the unchanged candidate runtime.
Its non-dry-run JSON report records initialization passing, then `BeforeEach`
failing on the lease guard from 09:05:52 to 09:06:07 UTC on September 16.
There were **zero passing scenarios**. The guard incorrectly required a
Pod-name/UUID prefix while the approved host-network runtime's fresh lease
contained its exact control-plane Node hostname. No workload namespace or
scaling workload was created. The operator reported unchanged `main=1/zero=0`
and stopped the controller before the repair.

The operator preserves `AZ-P1-001-retry2/report.e2e_suite.1.json`, SHA-256
`48c7f29a62ee52f296f7ad6ffe791036dab3bc81fd3a93ff5b5e151eebf591f3`,
with its JUnit and runner log. Earlier inherited-credential failures are
setup failures, not application or scenario passes. The hostname guard now
uses the pinned runtime's exact identity forms with freshness and foreign-node
rejection. The next attempt below exercised that repair.

The next attempt, `AZ-P1-001-385de9c-retry1`, passed initialization and the
repaired lease guard, then failed `BeforeEach` on status timestamp parsing at
09:17:59 to 09:18:09 UTC. Its non-dry-run report SHA-256 is
`c3d3cd104e7dae84155c7292216d1624d72d9266349347bd6383e3c7ac031490`.
Again there were no passing scenarios or workload mutations. The runtime's
actual status uses a Go `Time.String()` timestamp, not RFC3339 at the top-level
`time` field. The parser now accepts those two formats while preserving stale,
future and malformed timestamp rejection. The complete captured payload,
original SHA-256
`f3e77a1e309b33dc4dc7750d6d2786296aef2f27bfce9ddc6f89f4bad2921bd8`,
is retained as a unit fixture with only pool names sanitized and a copyright
header added. Its full status/group structure is exercised, not just a timestamp
substring. A new live rerun remains required.

The approved September 16, 2026 live campaign stops starting new cases at
13:30 UTC. Every test process must return or be stopped by 14:00 UTC, and the
sole cloud operator must remove all campaign infrastructure by 15:00 UTC.
Per-command timeouts must fit these absolute deadlines and allow cleanup.
The suite does not schedule the campaign or extend its budget. Cancellation
does not establish a pass; unfinished IDs remain unexecuted or interrupted.

Release ownership, chart `9.59.0` version increment, inherited
`appVersion: 1.35.0`, registry/publication identity and support policy remain
separate unresolved decisions.

[az]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/cloudprovider/azure/test/suites/scaleup/suite_test.go#L125-L212
[ca001]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/e2e/cluster_size_autoscaling.go#L160-L171
[ca002]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/e2e/cluster_size_autoscaling.go#L173-L185
[ca003]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/e2e/cluster_size_autoscaling.go#L187-L226
[ca004]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/e2e/cluster_size_autoscaling.go#L228-L235
[ca005]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/e2e/cluster_size_autoscaling.go#L237-L254
[ca006]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/e2e/cluster_size_autoscaling.go#L256-L275
[ca007]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/e2e/cluster_size_autoscaling.go#L277-L287
[ca008]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/e2e/cluster_size_autoscaling.go#L289-L295
[ca009]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/e2e/cluster_size_autoscaling.go#L297-L305
[ca010]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/e2e/cluster_size_autoscaling.go#L307-L313
[ca011]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/e2e/cluster_size_autoscaling.go#L315-L321
[ca012]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/e2e/cluster_size_autoscaling.go#L323-L332
[ca013]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/e2e/cluster_size_autoscaling.go#L334-L344
[ca014]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/e2e/cluster_size_autoscaling.go#L346-L360
[ca015]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/e2e/cluster_size_autoscaling.go#L362-L376
[ca016]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/e2e/cluster_size_autoscaling.go#L378-L392
[ca017]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/e2e/cluster_size_autoscaling.go#L394-L409
[ca018]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/e2e/cluster_size_autoscaling.go#L426-L431
[ca019]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/e2e/cluster_size_autoscaling.go#L433-L439
[ca020]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/e2e/cluster_size_autoscaling.go#L453-L472
[ca021]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/e2e/cluster_size_autoscaling.go#L474-L486
[ca022]: https://github.com/Azure/autoscaler/blob/d892fba1cf557b26d45540f2f6418b7ae52cca46/cluster-autoscaler/e2e/cluster_size_autoscaling.go#L488-L522
