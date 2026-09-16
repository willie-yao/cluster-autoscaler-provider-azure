# Phase 2 public-source E2E matrix

**Full Phase 2 live completion is not established.** All 22 active public-source
tests are implemented and reviewed. This campaign records 19 baseline public
passes and one supplemental pass through September 16, 2026 at 13:31 UTC.
The three DRA cases were prepared but not installed or executed within the
approved campaign window. They are unexecuted, not unsupported or Phase 3.
Phase 1 evidence does not substitute for running these new tests.
Campaign infrastructure and campaign-specific credentials were removed before
the deadline; see [campaign closeout](#campaign-closeout).

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

## Campaign accounting

| Coverage | Implemented tests | Live outcome |
| --- | --- | --- |
| Public baseline: `AZ-001`, `CA-001`, `CA-002`, `CA-004` through `CA-019` | 19 | 19 passed on the frozen candidate. |
| Public DRA: `CA-020` through `CA-022` | 3 | Unexecuted. Prepared setup artifacts were retained, but the profile was not installed because the remaining campaign window was insufficient. |
| Source-disabled `CA-003` | 0 | Explicit disabled/flaky source gap, not a passing or skipped proxy spec. |
| Supplemental cases | 5 | `AZ-P1-001` passed; four cases remain unexecuted in this campaign. |

The 22 active public tests plus five supplemental tests account for all 27
registered specs. Registration and local fake/unit coverage are not live passes.
The unexecuted DRA profile requires a later approved live window; it is not a
deallocate dependency and was not found unsupported.

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
| [AZ-001][az] | 100 Pods at 200m CPU cause actual owned-worker growth, Ready test Pods use all three workers, excess demand remains Pending, deletion restores baseline with VM/Node/NIC evidence. Full 100-Pod readiness is not a source assertion and would exceed this budget. | `public_test.go` | Passed on `886f31f8d`; see verified live batches. Bounded adaptation of original 20-core pressure. |
| [CA-001][ca001] | A 1.1x worker-memory Pod stays scheduler-rejected, emits `NotTriggerScaleUp` for its exact UID and does not grow the eligible pool for five minutes. | `public_test.go` | Passed on `886f31f8d`; see verified live batches. Main-pool selector restricts candidate size. |
| [CA-002][ca002] | 100 small Pods totaling worker allocatable memory cause growth; all 100 are Ready on real workers; cleanup physically shrinks. | `public_test.go` | Passed on `886f31f8d`; see verified live batches. Measured background requests make the baseline insufficient. |
| [CA-003][ca003] | Avoid duplicate scale-ups while an operation is processing. | None | Explicit source gap: unconditionally disabled as flaky. An idle/status check is not a replacement. No passing empty/skip test added. |
| [CA-004][ca004] | Three Pods reserve the same host port 4321, require three distinct actual Ready workers, and all become Ready. | `public_test.go` | Passed on `886f31f8d`; see verified live batches. `B+2` across both owned pools, host-port admission required. |
| [CA-005][ca005] | Required hostname anti-affinity grows from one to three Pods on three distinct workers. | `public_test.go` | Passed on `886f31f8d`; see verified live batches. `B+2`, namespace-owned constraints only. |
| [CA-006][ca006] | A pending Pod with an EmptyDir and anti-affinity triggers growth and runs on a second worker. | `public_test.go` | Passed on `886f31f8d`; see verified live batches. Local-storage scale-down protection remains enabled. |
| [CA-007][ca007] | Remove three-worker pressure and require physical deletion back to baseline, not just desired-capacity change. | `public_test.go` | Passed on `886f31f8d`; see verified live batches. `B+2`. |
| [CA-008][ca008] | One movable Pod per worker, PDB permits one disruption; drain preserves at least N-1 Ready replicas at each observation and all replicas recover on the surviving worker. | `public_test.go` | Passed on `886f31f8d`; see verified live batches. Preferred spread replaces source-wide taint mutations; actual initial distribution is asserted. |
| [CA-009][ca009] | PDB permits no disruptions; captured instances and Ready workload remain unchanged throughout five minutes. | `public_test.go` | Passed on `886f31f8d`; see verified live batches. Scale-down timings are explicit and at most one minute. |
| [CA-010][ca010] | Two movable Pods per worker, one permitted disruption; multi-Pod drain preserves N-1 Ready replicas at each observation and reschedules all six. | `public_test.go` | Passed on `886f31f8d`; see verified live batches. `B+2`, PDB budget must replenish. |
| [CA-011][ca011] | The multi-Pod PDB drain runs with synthetic test-owned kube-system objects, preserving actual system addons and protections. | `system_test.go` | Passed on `886f31f8d`; see verified live batches. Explicit `allow-kube-system-fixture: CA-011` marker and no overlapping PDB selectors required. |
| [CA-012][ca012] | Real low-priority demand is created in the test body; one Ready and one Pending expendable Pod do not grow the pool. | `priority_test.go` | Passed on `886f31f8d`; see verified live batches. Corrects source bug that creates workload only in deferred cleanup. |
| [CA-013][ca013] | Two high-priority reservations cause growth and become Ready on distinct workers. | `priority_test.go` | Passed on `886f31f8d`; see verified live batches. Operator-owned PriorityClasses/cutoff required. |
| [CA-014][ca014] | High-priority demand actually replaces the captured low-priority Pod; low-priority replacement remains Pending without growth. | `priority_test.go` | Passed on `886f31f8d`; see verified live batches. Real scheduling preemption, not direct test eviction. |
| [CA-015][ca015] | Three expendable memory reservations do not prevent physical shrink; remaining demand is explicitly one Ready/two Pending. | `priority_test.go` | Passed on `886f31f8d`; see verified live batches. `B+2`, source's running expendable workload intent retained. |
| [CA-016][ca016] | Non-expendable memory reservations keep captured workers and all three Pods Ready throughout five minutes. | `priority_test.go` | Passed on `886f31f8d`; see verified live batches. Negative window exceeds the required timing profile. |
| [CA-017][ca017] | Unprocessed bypassed-scheduler demand that cannot fit triggers actual growth while remaining unprocessed. | `scheduler_test.go` | Passed on `886f31f8d`; see verified live batches. Optional bypass profile, not Phase 3. |
| [CA-018][ca018] | Bypassed-scheduler demand measured to fit stays unprocessed without growth. | `scheduler_test.go` | Passed on `886f31f8d`; see verified live batches. Optional bypass profile. |
| [CA-019][ca019] | Unprocessed demand under a unique, unconfigured scheduler name does not trigger growth for five minutes. | `scheduler_test.go` | Passed on `886f31f8d`; see verified live batches. Can run as a baseline control without the bypass flag. |
| [CA-020][ca020] | Twelve one-device DRA Pods cause three-worker growth; all are Ready with unique, Pod-owned claim allocations matching four synthetic devices per worker. | `dra_test.go` | Implemented; unexecuted. Setup prepared but not installed due campaign window. DRA API/driver/future-node simulation required, no GPU SKU. |
| [CA-021][ca021] | A five-device claim cannot fit any four-device worker; exact-Pod no-growth event, persistent Pending behavior and actual unallocated claim. | `dra_test.go` | Implemented; unexecuted. Setup prepared but not installed due campaign window, not unsupported or Phase 3. |
| [CA-022][ca022] | DRA anti-affinity grows to two workers, additional two-device Pods occupy both; removing pressure allows physical Delete-mode drain and actual claim reallocation on one worker. | `dra_test.go` | Implemented; unexecuted. Setup prepared but not installed due campaign window, not unsupported or Phase 3. |

## Supplemental and excluded coverage

The five `AZ-P1-001` through `AZ-P1-004` and `AZ-SUP-ETAG` cases are described
in the [runner guide](../cloudprovider/azure/test/README.md). They are not extra
registrations in the 23-row public inventory. They add discovery/idle,
min/max, zero-pool identity/deletion, and an explicit PDB release transition.
The retained ETag example is supplemental; the selected source has ETag
unit tests, not an ETag E2E registration.

Only `AZ-P1-001` was executed in this campaign. `AZ-P1-002`, `AZ-P1-003`,
`AZ-P1-004` and `AZ-SUP-ETAG` remain implemented but unexecuted. Similar behavior
observed by other public cases or the separate Phase 1 acceptance does not
turn these four named specs into passes.

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

## Verified live batches

These are actual non-dry-run selected `It` results, not Ginkgo registration
dry-runs, filtered specs or passing setup hooks. All 20 JSON reports declare
the suite and selected scenario passed; their JUnit reports have zero failures
and errors. Each invocation removed its test namespace and returned to actual
main/zero `1/0` before the next case.

Test source was frozen at
`886f31f8dfd0b283c5d0dd128591f4a55861bc42`; the separately frozen runtime source
remained `48f997fcd8d4a93f25d06837e185ea77d4e2c5cd`, image
`localhost/cluster-autoscaler-azure:candidate-48f997f`. This is private local
campaign identity, not an official image publication or release decision.
These are candidate-only checks. The separate Phase 1 upstream/candidate
comparison was not repeated as part of this Phase 2 campaign.

| ID | Recorded interval, UTC | Recorded duration | Observed result |
| --- | --- | --- | --- |
| `AZ-P1-001` | 09:30:24 to 09:35:55 | 5m31s wall | Supplemental discovery/leadership/idle pass. Captured main VM/Node/NIC identities unchanged, actual main/zero `1/0`, two total VMs/four vCPUs. Does not cover public `AZ-001`. |
| `CA-005` | 09:36:40.118 to 09:50:06.878 (suite) | 13m32s wall | Three Ready anti-affinity Pods on distinct workers, actual and desired pool counts `1/0 -> 2/1 -> 1/0`; peak four VMs/eight vCPUs. Suite duration was 13m26.76s including cleanup. This growth case does not assert individual NIC deletion. |
| `CA-001` | 09:50:41 to 09:56:12 | 5m31s wall | Exact-Pod rejection and five-minute no-growth pass. Measured worker allocatable memory 8,165,543,936 bytes, existing requests 52,428,800 bytes, oversized request 8,982,098,329 bytes. Actual main/zero remained `1/0`. |
| `CA-004` | 09:59:38.886 to 10:12:42.774 (suite) | 13m09s wall | Three Ready host-port Pods on distinct workers, actual and desired `1/0 -> 2/1 -> 1/0`; peak four VMs/eight vCPUs. Cleanup restored actual baseline; this growth case does not assert individual NIC deletion. |
| `CA-002` | 10:13:35.221 to 10:26:55.088 (suite) | 13m25s wall | All 100 small-memory Pods became Ready on main workers, actual and desired `1/0 -> 2/0 -> 1/0`; peak three VMs/six vCPUs. Added main instance `3`, its Node and captured NIC were physically deleted; main instance `0` survived. |
| `AZ-001` | 10:27:31.947 to 10:40:50.644 (suite) | 13m23s wall | 100 CPU-requesting Pods caused actual and desired `1/0 -> 2/1 -> 1/0`; Ready test Pods used all three workers and excess demand remained Pending. Peak four VMs/eight vCPUs. Main instance `0` and zero instance `2`, their Nodes and captured NICs were physically deleted; main instance `4` survived. This is not a claim that all 100 Pods became Ready. |
| `CA-006` | 10:41:38.259 to 10:54:47.104 (suite) | 13m14s wall | The EmptyDir/anti-affinity pair became Ready after actual and desired `1/0 -> 2/0` growth; peak three VMs/six vCPUs. Namespace cleanup restored actual `1/0`, retaining main instance `5`. Local-storage protection remained enabled. This growth case does not separately assert individual NIC deletion. |
| `CA-007` | 10:55:57.058 to 11:09:15.652 (suite) | 13m23s wall | Three-worker pressure grew actual and desired `1/0 -> 2/1`; peak four VMs/eight vCPUs. Removing pressure restored `1/0` with physical deletion of main instance `6`, zero instance `3`, their Nodes and captured NICs. Main instance `5` survived. |
| `CA-008` | 11:09:55.826 to 11:23:58.011 (suite) | 14m08s wall | Three movable Pods initially occupied separate workers. The one-disruption PDB drain kept at least two Ready at every observation and all three recovered on main instance `5`. Main instance `7`, zero instance `4`, their Nodes and captured NICs were physically deleted; actual and desired `1/0 -> 2/1 -> 1/0`, peak four VMs/eight vCPUs. |
| `CA-009` | 11:24:31.897 to 11:37:28.277 (suite) | 13m02s wall | Zero permitted disruptions retained captured main instances `5`/`8`, zero instance `5` and Ready movable workload throughout five minutes. Peak four VMs/eight vCPUs. Namespace cleanup restored actual `1/0`, main instance `5` surviving; this blocking case does not separately assert individual NIC deletion after fixture cleanup. |
| `CA-010` | 11:37:59.182 to 11:52:00.949 (suite) | 14m07s wall | Six movable Pods initially occupied three workers, two per worker. The one-disruption drain kept at least five Ready at every observation and all six recovered on main instance `5`. Main instance `9`, zero instance `6`, their Nodes and captured NICs were physically deleted; actual and desired `1/0 -> 2/1 -> 1/0`, peak four VMs/eight vCPUs. |
| `CA-011` | 11:52:40.531 to 12:06:40.655 (suite) | 14m05s wall | Six run-owned synthetic kube-system Pods initially occupied three workers, two per worker. The PDB drain kept at least five Ready per observation and all six recovered on main instance `10`; system-pod protection was not disabled. Main instance `5`, zero instance `7`, their Nodes and captured NICs were physically deleted. Peak four VMs/eight vCPUs; actual `1/0` restored. Operator also confirmed zero remaining run-owned kube-system Deployments/PDBs. |
| `CA-012` | 12:07:48.940 to 12:14:05.286 (suite) | 6m22s wall | Real expendable demand remained one Ready/one Pending without growth throughout five minutes. Each reservation requested 5,715,880,755 bytes against measured allocatable 8,165,543,936 and existing requests 52,428,800 bytes. Main instance `10` remained the sole worker, actual `1/0`, total two VMs/four vCPUs. Namespace cleanup passed. |
| `CA-013` | 12:14:40.658 to 12:27:52.264 (suite) | 13m17s wall | Two non-expendable high-priority reservations became Ready on distinct actual main workers, each using the same measured 5,715,880,755-byte request. Actual and desired `1/0 -> 2/0 -> 1/0`, peak three VMs/six vCPUs. Namespace cleanup retained main instance `11`; this growth case does not separately assert individual NIC deletion. |
| `CA-014` | 12:28:29.080 to 12:35:24.736 (suite) | 7m01s wall | Scheduler preemption replaced the captured expendable Pod with actual high-priority demand. The high-priority Pod remained Ready and the expendable replacement Pending throughout the no-growth window. Main instance `11` and actual `1/0` remained, total two VMs/four vCPUs; namespace cleanup passed. |
| `CA-015` | 12:36:10.234 to 12:50:55.122 (suite) | 14m50s wall | Three running expendable reservations allowed physical shrink, ending with one Ready/two Pending. Main instance `12`, zero instance `8`, their Nodes and captured NICs were physically deleted; main instance `11` survived. Actual and desired `1/0 -> 2/1 -> 1/0`, peak four VMs/eight vCPUs; namespace cleanup passed. |
| `CA-016` | 12:51:30.988 to 13:04:41.811 (suite) | 13m16s wall | Three non-expendable reservations kept captured main instances `11`/`13`, zero instance `9` and all three Pods Ready throughout five minutes. Peak four VMs/eight vCPUs. Namespace cleanup restored actual `1/0`, main instance `11` surviving; no separate individual NIC deletion assertion after cleanup. |
| `CA-017` | 13:05:29.458 to 13:18:47.205 (suite) | 13m23s wall | Bypassed-scheduler demand that could not fit the baseline caused actual `1/0 -> 2/0` growth while the Pods remained genuinely unprocessed/Pending. Peak three VMs/six vCPUs. Namespace cleanup restored actual `1/0`, main instance `14` surviving; this growth case does not separately assert individual NIC deletion. |
| `CA-018` | 13:19:38.071 to 13:25:02.101 (suite) | 5m30s wall | A measured fitting 4,082,771,968-byte request stayed unprocessed/Pending under the bypassed scheduler without growth throughout five minutes. Main instance `14` and actual `1/0` remained, total two VMs/four vCPUs; namespace cleanup passed. |
| `CA-019` | 13:25:53.110 to 13:31:17.872 (suite) | 5m30s wall | Demand under a distinct unconfigured scheduler stayed unprocessed/Pending without growth throughout five minutes. Main instance `14` and actual `1/0` remained, total two VMs/four vCPUs; namespace cleanup passed. The invocation began before the 13:30 UTC launch cutoff and completed before the 14:00 UTC process deadline. |

The sole operator preserves each run's non-secret runner log, JSON and JUnit
under the run name below. Verified SHA-256 digests:

| Run | JSON report | JUnit report | Runner log |
| --- | --- | --- | --- |
| `AZ-P1-001-886f31f` | `29a812fd4bc3094778e64365febbac8a8fea43158102bcd50b38247083215061` | `0e47c4485fb3e33b416122372137c9dae21766479063ac8cb55a87e410b3a4b5` | `a22d6c015f253590402adfdd28cc5fb04d02aca5de455f951d85e715751135ed` |
| `CA-005-886f31f` | `9cda017ac0462653f09c884657deb699e64ab59912dbdd2967ca659ac8605698` | `b5eefe3a4e02207b51f2df9c95a10a3f0a90252827d474a6f0712a4535c3d56e` | `0a845290a45a89099df47818e9ee5e5f1132dd413aae7aad812ac676cebe7ca7` |
| `CA-001-886f31f` | `4211b62753d66e627ccb9393dd7babd16eaa1664321b849c962a8d61ef54a996` | `1a2207e002c814a8840146faef5c88e962c3bce1816948893c5d371c8ef974fe` | `6b0efa724c4aeaa18653059697cc6b22459b859482179dec68df95a6d61ab229` |
| `CA-004-886f31f` | `049bdd8eef70aaad59c4d4458f2d4a2c8a0e88fe2f5c23d02824a8cb53f7369c` | `84c8cbece41e24e486ea7e0a5a89e42c3b12b5a19e454514b1570cbb430cf3ca` | `c070863d787560b1109220ad69ac91fa7e4beda3818c58ecb35cd38b6ff3036e` |
| `CA-002-886f31f` | `2608133d8109ff14c7664a2bd74307f36895280ba18d0eb395ac8a31d456a23f` | `26ab860cb1ddf656e0e1dab6e9420af79ad74edfcc9af057a6625f1f498e1e4c` | `9f72c43a05b7671a68b80f2c007d222cc468d91ff35b23b016b6854880df1d4a` |
| `AZ-001-886f31f` | `de8c6773d711170abf120a31c1ef09053db3f4dcfd512627bc4322c8bd7faa88` | `18455b5c53988a67f98a75dca551ed076e2dee6032ee1f286df503331a7dc34e` | `3fa7957f76ebc89d500e5e736852f176dad876df947f61c8c70205f9d0c800be` |
| `CA-006-886f31f` | `259f28769ee83488adfda1b66001d5792ddd940fcdf46dbf5b31552d28592de5` | `a0a4150c420bf5984266cfb0ef36919c561a689e285c48156eafedebc6c2d777` | `7d6080e8c6e89e26039923f42f531488a1f8f0148f199af6cb8aeb5f34160f1a` |
| `CA-007-886f31f` | `77f46f2d166555b204818555e790acc0b327c6aef0ab6fb7fb88b6e72d145665` | `8daa7e1472369d380907bb671c065eb56394c6879e85b4090a86f1138db4bc9b` | `86bb633ff57d33e2d59281b83306957c6da916068778d87e038bc7b2f0dcf864` |
| `CA-008-886f31f` | `63abff5dd6410b6fadf15822822649d2db85301204ce550a434d23c733399c89` | `0d0f6b21d8609023109b655ec2feb87d8760069bd5f7e8991d1eadb8ab1efaed` | `f7835a1d0c31a1f14da1a1c02b98c6853ab8526c514c4e5199f975cb79e03c4b` |
| `CA-009-886f31f` | `f4ef3c493011f3c8a2499e034170e7b66b46f4e3d77a07f900a05c81bdc8f19a` | `3f62073d94a5d14cbbc90e25ac76f14454bdcf702c6d6e919de6d2263aa87e62` | `3de87bf316c9b3b08ccd58fc8a3df2bf6b2b4e29a8d24a7c1762acd0f626bae3` |
| `CA-010-886f31f` | `b249a66f999609c3622ec0d8ed000f1916dd2f244890deee73acf90a39c938a5` | `a5294fcddc60004d79405f5177b1e464682360f6fac23762668f23d6b1ed9c6d` | `88660148940ec229f78215754f0b1f2e3122ebefe613dbe93b9fcf575e814e90` |
| `CA-011-886f31f` | `8f0b0de24bbb180d260210b76564b1270f233fdf7f1b6f3d6e16507ea48d7c35` | `977e658862607e2da7fca7606ff51a470524c4b99d11ff59b6bcd4fe561e8c64` | `390cba57f2d480eaa88783e9a708fa0686b16a9634b12a88d207dfdd35e112a9` |
| `CA-012-886f31f` | `9c7b13266a00b7e91e55d7f334485e55bde83e1b0944318b939ce58a7c46357c` | `344291d512d9226f12ef5f0a82e849c12386c901f0e71b75d5df78e2b8bf53d5` | `6246f661d6278a7fd5d6400b4614ab71ea4f9feed44b1710d2e5dd6e312ffc58` |
| `CA-013-886f31f` | `8899ba7a8d894485efaa046f90e0b6aeb02740d6d371c12bad871bfd6d7e9b89` | `9a818cdbbbaae0578a4dad76a3ce8dae49732f75eb16d34ebaec63152ab47540` | `3455e545a0dcef78d7948300f579b0e9805641f753924d86c88f2fe6daaaacd9` |
| `CA-014-886f31f` | `5031529b9a68c6273c0264fec33f69712f8057ece0fca99e31955ce8ca109321` | `660101b66c07eb5e61f68123226eb97fba0957b7a21f801954e43db73af173c7` | `6702a3f7da6fa72a299517757f92a9e912eae6874f173394299fcea2fb5a2897` |
| `CA-015-886f31f` | `be6396dca1d2d1c15c4aba6aab015c137e009b485fdc9cedad9090d649bfce85` | `29a0b2f2a89b9ee8d92c0d5c252c7f69b9dd407fc574b86d871bd889c61a9630` | `ccfc5949aca565349d31668172c9908246086823519cc7b5f637a7782a4a70b5` |
| `CA-016-886f31f` | `852c47af9e1b379da2ab730471b5143f26ff20fb55fe3888e7e3043689cc8b8e` | `b503ca83ecffae5e65e2f702e40afc006fb4f12448cc68db7938c22417b62e82` | `a41766800fad2e5dbade8a0bfa4b4028e3c47b201859f7d10a8d563886845442` |
| `CA-017-886f31f` | `d718ca1e05c73451095198b631195c23f2b556411bcdd7585143bbb6adbce7ef` | `73c82d791a9cad10591eb80cc6e794f0f9b0ed15607cb5df9d5f483ac4723f86` | `db61d1db593a825753685ca5687b8c400c689ffca49b6cf6b45970ca981bf344` |
| `CA-018-886f31f` | `5de37587f7d0017f4ebdc53ac76b7da496da26df7dde9c53c1af591cef67b1bb` | `7a6b5b0fd84e23a46796226e163c27e719f89ba9ad4f055bb18a4c53471f8f95` | `53887046801f3772c982523041cda0e02cedc5e0dcac5fecd0e622d10b488bfd` |
| `CA-019-886f31f` | `81ad26d6b60aaad82d7505bcd9539675ec5c683d296c87488f7c0e943310407b` | `29287487c4954d5db4750f1c85f8f99036aaf795a9b9ac37182f9b1042ff67b2` | `310498ca3d8f0e670aee9db0d281d0a9fd00968b9568fc419e608a7376691460` |

## Campaign closeout

The sole cloud operator stopped the autoscaler at 13:32:11 UTC and confirmed
settled state at 13:32:39 and 13:33:33. Run-owned workloads, PriorityClasses and
the authorization marker were removed. No DRA fixture had been installed.
All kubeadm bootstrap tokens were revoked, with a final count of zero, and the
control-plane join template was removed.

Three campaign-created scoped role assignments were deleted and verified
absent. The worker resource group was deleted by 13:36:43; the infrastructure
resource group was deleted by 13:42:49. At 13:43:22 the operator verified both
groups absent, zero run-tagged resources and zero remaining created role
assignments. Nine campaign credential files and their directory were removed
and verified absent at 13:43:42, before the 15:00 deadline. The existing external
policy identity was left untouched. An independent coordinator check also
confirmed both groups and the campaign credential directory absent.

**Current campaign resource inventory is zero.** No infrastructure deletion was
performed by the test suite. The operator cleared its deadline automation.
The operator's estimate of approximately $2.25 covers identifiable usage only;
traffic, operations and external policy telemetry may add charges. This is not
a final invoice or a claim of precisely measured total spend.

The operator retains the detailed private ledger and non-secret artifacts.
Their sanitized references and verified SHA-256 digests are below. The two
resource inventories were captured before deletion, not mistaken for empty
post-cleanup inventories. The corrected ledger explicitly distinguishes its
historical two-VM state from the final zero-resource state.

| Evidence | SHA-256 |
| --- | --- |
| Worker-resource inventory before deletion | `a6755ac4edf5893e45f73189f43d6f2f12813921557ee57a7b4d003438453a66` |
| Infrastructure-resource inventory before deletion | `89fc31a97d6acacdc97c4e6f8eb499ae2479b1d7201d7f852af32086eae32985` |
| Corrected final operator ledger | `e4ae9902fd4d7d0f6cab202d57c4aa247bb7dbab788a9e16257f6222a4c5b8c7` |
| Final 102-file non-secret evidence manifest | `d746fa430796cc287a5cd279b10db179edfdad561f78a4a5fbaf6a97ca22e022` |

All 102 manifest entries were verified against retained artifacts. The 60
runner/JSON/JUnit digests for the 20 passing invocations were unchanged by the
ledger correction. Cloud-resource, identity, network and credential details
are intentionally omitted here. Successful campaign cleanup does not establish
the unexecuted DRA coverage or full Phase 2 live completion.

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
substring. The subsequent `AZ-P1-001` pass in the initial batch exercised the
repair; the historical failed attempts do not become passes.

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
