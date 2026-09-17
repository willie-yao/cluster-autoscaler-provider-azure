# Source provenance and validation boundaries

## Source identity

| Component | Source |
| --- | --- |
| Upstream application and chart | [`kubernetes/autoscaler@c85f5afed954f7ecdbfb9091da2202426806d8d8`](https://github.com/kubernetes/autoscaler/tree/c85f5afed954f7ecdbfb9091da2202426806d8d8) |
| Azure-only repository baseline | [`c97a283f52a12939edeff8fe1d38d69f4a9ce789`](https://github.com/willie-yao/cluster-autoscaler-provider-azure/commit/c97a283f52a12939edeff8fe1d38d69f4a9ce789) |
| Frozen compatibility and E2E source | [`6275fd3d39efa817b3e6c401a97bb3c4af285be5`](https://github.com/willie-yao/cluster-autoscaler-provider-azure/tree/6275fd3d39efa817b3e6c401a97bb3c4af285be5) |
| Extracted autoscaling core | [`kubernetes-sigs/cluster-autoscaler@3d1c7137cdac`](https://github.com/kubernetes-sigs/cluster-autoscaler/commit/3d1c7137cdac), module `sigs.k8s.io/cluster-autoscaler v0.0.0-20260903143621-3d1c7137cdac` |
| Public Azure E2E inventory | [`Azure/autoscaler@d892fba1cf557b26d45540f2f6418b7ae52cca46`](https://github.com/Azure/autoscaler/tree/d892fba1cf557b26d45540f2f6418b7ae52cca46), public 1.35-line test source |

The repository retains upstream Git ancestry through the Azure-only baseline.
The source candidate curates the frozen implementation into reviewable commits
on that baseline rather than replacing history with a new root or importing
the development-log commits.

Production and test Go files, dependency pins, Dockerfile, environment example,
fixtures and Makefile recipes are retained from the frozen source. Project
documentation and the contributor tour replace development chronology.
Campaign plans and detailed run records remain available at the immutable
source checkpoint, not as maintained instructions in this tree.

The application keeps `k8s.io/autoscaler/cluster-autoscaler` and its local
`./apis` replacement. Azure-only registration, image placeholders and the
managed-identity existing-Secret correction are intentional differences from
the old monorepo. The [chart oracle](../charts/testdata/azure-compatibility/README.md)
records the precise packaging comparison and attribution. Source licenses and
copyrights remain intact.

## Historical live evidence

These results describe earlier executions, not a cloud run of the curated
commits:

| Coverage | Test source | Runtime source |
| --- | --- | --- |
| Paired upstream Azure Delete-mode compatibility, image-only cutover and rollback | `48f997fcd8d4a93f25d06837e185ea77d4e2c5cd` compared with upstream `c85f5afed954f7ecdbfb9091da2202426806d8d8` | Candidate `48f997fcd8d4a93f25d06837e185ea77d4e2c5cd` and pinned upstream, run sequentially |
| 19 baseline public intents plus supplemental `AZ-P1-001` | `886f31f8dfd0b283c5d0dd128591f4a55861bc42` | `48f997fcd8d4a93f25d06837e185ea77d4e2c5cd` |
| Three public DRA intents, `CA-020` through `CA-022` | `1ac42b41f848d0e8b2f590d131d4a86541641d10` | `48f997fcd8d4a93f25d06837e185ea77d4e2c5cd` |

The paired profile used self-managed Kubernetes `1.37.0`, Linux and VMSS
Uniform. It passed authentication/discovery, idle stability, growth, maximum
refusal, scale-from-zero, PDB protection and safe physical deletion, followed
by cutover and upstream rollback without overlapping controllers.

The public inventory has 22 active intents with live passes across the two
test checkpoints above. It is not a run of all 27 local specs at one head.
`CA-003` remains source-disabled. Four supplements remain unexecuted:
`AZ-P1-002`, `AZ-P1-003`, `AZ-P1-004` and `AZ-SUP-ETAG`.
No internal AKS parity, deallocate behavior, broad backend/CNI coverage or
Windows acceptance is established.

The frozen source's later instance-page terminal-bound and automatic
dirty-version corrections have local regression/review coverage, not
retroactive credit from those live runs. Current
[scenario adaptations](../cloudprovider/azure/test/README.md#coverage-inventory)
must likewise be distinguished from claims about the original public tests.

The archived
[paired acceptance evidence](https://github.com/willie-yao/cluster-autoscaler-provider-azure/blob/6275fd3d39efa817b3e6c401a97bb3c4af285be5/docs/phase-1-acceptance-evidence.md)
and
[source-to-test matrix](https://github.com/willie-yao/cluster-autoscaler-provider-azure/blob/6275fd3d39efa817b3e6c401a97bb3c4af285be5/docs/phase-2-e2e-matrix.md)
contain full scope, artifact and cleanup records. They are historical evidence,
not authorization to repeat a run.

## Local and hosted checks

The frozen source's
[Verify Go](https://github.com/willie-yao/cluster-autoscaler-provider-azure/actions/runs/35147856163)
and
[Cluster Autoscaler](https://github.com/willie-yao/cluster-autoscaler-provider-azure/actions/runs/35147856169)
push workflows passed at `6275fd3d39efa817b3e6c401a97bb3c4af285be5`.
Those results do not establish the PR chart-version gate or new live coverage.

For a new candidate, run the [local checks](testing.md#local-checks) and record
its own results. Source equivalence preserves implementation, not the identity
or live qualification of a rebuilt binary. The curated source does not create
an official Azure repository, release, registry, contribution policy or
publication authority.
