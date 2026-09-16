# Phase 1 migration acceptance

**Phase 1 functional migration acceptance passed for the tested profile.** The
[live evidence](phase-1-acceptance-evidence.md) covers stable Kubernetes
`v1.37.0`, paired upstream/candidate cases, cutover, rollback and cleanup. It
does not establish release readiness or broader provider support.

## Baseline and scope

| Item | Pin or decision |
| --- | --- |
| Upstream application and Azure chart | `kubernetes/autoscaler@c85f5afed954f7ecdbfb9091da2202426806d8d8` |
| Provider bootstrap | `c97a283f52a12939edeff8fe1d38d69f4a9ce789` |
| Extracted core repository | `kubernetes-sigs/cluster-autoscaler` |
| Core dependency | `sigs.k8s.io/cluster-autoscaler v0.0.0-20260903143621-3d1c7137cdac` |
| Application module | `k8s.io/autoscaler/cluster-autoscaler` |
| API replacement | `replace k8s.io/autoscaler/cluster-autoscaler/apis => ./apis` |
| Kubernetes dependencies | `v1.37.0-rc.1`, with staging modules at `v0.37.0-rc.1` |
| Exact live acceptance target | Stable Kubernetes `v1.37.0`; the qualified self-managed profile is recorded in the [acceptance evidence](phase-1-acceptance-evidence.md). |

Phase 1 preserves upstream Azure functionality: standard pools, explicit VMSS
groups, discovery, scale-from-zero, existing authentication/configuration paths,
and ordinary Delete-mode scale-down. Flex and VMs-pool functionality retain
upstream gates and requirements. Neither is prohibited by deallocate-branch
restrictions. AKS-fork deallocate parity is excluded. Phase 2 covers the active
public-source E2E inventory, not comprehensive AKS product compatibility;
deallocate belongs to Phase 3.

Intentional migration differences:

- Azure-only provider registration and chart selection, plus root application
  layout and removed non-Azure/monorepo publication wiring.
- Placeholder image repository and no official publication identity.
- Managed-identity Secret references honor `secretKeyRefNameOverride`.
- Regenerated API informers have typed wrappers. CapacityQuota scheme aliases
  preserve generated-client compatibility. CRD schemas are unchanged.
- VPA packaging is restored from upstream, not excluded as another provider.

## Local and live evidence

The local checks use Go 1.26.0 and unchanged dependency pins. Fake-client and
local-process limits remain explicit. The final row records the separately
authorized live profile.

| Evidence | Status | Limit |
| --- | --- | --- |
| Source continuity | Pass | `main.go` and retained Azure runtime code match upstream. Bootstrap Go differences are generator paths and an E2E comment; CRD schemas are identical. |
| Azure chart compatibility, `make test-chart` | Pass | Six populated fixtures, complete resources, required Deployment, VPA regression fails before restoration. See the [oracle record](../charts/testdata/azure-compatibility/README.md). |
| Azure-only executable comparison | Pass | Both exact upstream `-tags azure` and local builds succeed. Same sanitized environment, arguments and `argv[0]` produce identical 383-line help for default and explicit Azure selection. Time-derived Ginkgo seeds matched in paired launches. |
| Resolved executable dependencies | Pass | `go list -mod=readonly -deps -json` resolves the same 1,810 package paths and module/replacement versions for both applications. Local API source changes are tested separately. |
| Configuration precedence | Pass | `TestBuildAzureConfigMigrationPrecedence` covers file/defaults, legacy fields, environment aliases, empty values and rejected auth conflicts. Existing standard-pool and discovery tests remain. |
| Generated API integration | Pass | All six API versions exercise scheme serialization, typed add/update/delete handlers, namespace indexing, and shared legacy caches. Uses simple fake trackers, not server-side apply or CRD admission. |
| VMSS discovery and operation boundaries | Pass | `TestVMSSMigrationBoundaries` checks tagged pool scope, zero-size template/growth, max limits, selected non-force Delete request and already-minimum rejection. Fake cloud responses do not prove cloud effects. |
| Core demand-driven growth and safe scale-down | Pass | `make test-core-integration` runs the pinned core's existing fake-provider lifecycle and resource-limit tests. This is not an integrated Azure-cloud E2E test. |
| Full local race and API suites | Pass | `make test-ci GOOS=darwin`, API informer race tests, and repeated migration tests pass. API and focused migration tests were repeated ten times. |
| Hosted CI for this change | Missing | No push or workflow run requested. Bootstrap run `34291127550` passed at the bootstrap SHA only. The existing `ct lint` version-increment gate still needs an approved chart version beyond inherited `9.59.0`. |
| Real auth, registration, scheduling, deletion, cutover and rollback | Pass | Passed for the exact bounded profile in the [Phase 1 acceptance evidence](phase-1-acceptance-evidence.md). This is not a general support claim. |

The generated apply-aware fake client has a separate unresolved limitation:
updating a seeded CapacityBuffer fails with a structured-merge schema type lookup
error on both the exact upstream baseline and this checkout. Do not treat the
simple-tracker informer tests as server-side apply proof. Live apply compatibility
remains unknown. This change does not redesign clients or change dependency versions.

## Installation, upgrade and cutover gates

1. Resolve release ownership and the decisions below. Pin the exact image digest,
   chart/version, Kubernetes acceptance target and retained previous deployment.
   The inherited chart `appVersion: 1.35.0` and default image tag `v1.35.0` are not
   Kubernetes 1.37 compatibility claims or an approved first-release identity.
2. Render the actual installation or upgrade values. Require a Deployment and
   preserve pool scope, min/max limits, discovery tags, cloud configuration,
   identity/credential references, namespaces, selectors and RBAC. Confirm any
   enabled optional feature's permissions. VPA also needs its CRD and controller.
3. Confirm a single controller owner for every managed pool. Disable the old
   controller's restart/reconciliation path, stop it, and settle outstanding
   operations before enabling the replacement. Different leader-election locks
   do not prevent controllers from managing the same pool.
4. On stable Kubernetes `v1.37.0`, verify authentication and authorization,
   discovery includes only intended pools, and no unexpected capacity changes
   occur.
5. Demonstrate bounded demand-driven growth, node registration/readiness and
   workload scheduling; repeat from zero for a VMSS pool. Demonstrate ordinary
   safe Delete-mode scale-down with eviction/PDB protections and min limits.
   Verify selected cloud instances are deleted and remaining workloads recover.
6. Perform an authorized cutover and rollback rehearsal. Any failed gate blocks
   release acceptance. Use direct, recoverable CLI steps, not an unattended
   resource-creation or cleanup script.

For rollback, **stop the replacement first**, settle its operations, then restore
the previous pinned image and configuration. Verify single-controller ownership,
unchanged pool scope/limits/credentials, recovered discovery, node health and
workload scheduling before declaring recovery. Do not run both controllers to
speed up rollback.

## Unresolved release decisions

| Decision | Required before release |
| --- | --- |
| Ownership | Maintainer organization, operational owner and approval authority |
| Support and security | Public support path and private vulnerability contact |
| Image registry | Registry owner, image naming, signing/provenance and publishing authority |
| Module and APIs | Downstream publication policy while retaining current identities until approved |
| Versioning | First application/chart release, tag policy, Kubernetes support policy and upgrade guarantees |
| Kubernetes 1.37 acceptance | Tested profile is qualified; broader support policy and future environment coverage remain release decisions |

Changing these decisions, publishing artifacts, and performing cloud or cluster
mutations require separate authorization.
