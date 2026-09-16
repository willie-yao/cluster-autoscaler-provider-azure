# Phase 1 acceptance evidence

Phase 1 functional migration acceptance passed on September 16, 2026 for the
exact self-managed Azure profile below. The upstream baseline and extracted
candidate completed the same cases sequentially, the upstream rollback smoke
passed, and all test resources were removed.

This is functional evidence for one bounded profile. It is not an official
release, support-policy, AKS, deallocate, Flex, VMs-pool, or broad provider
compatibility claim.

## Exact references

| Item | Reference |
| --- | --- |
| Upstream application | `kubernetes/autoscaler@c85f5afed954f7ecdbfb9091da2202426806d8d8`, `cluster-autoscaler/`, Azure build tag |
| Candidate | `48f997fcd8d4a93f25d06837e185ea77d4e2c5cd` |
| Provider bootstrap | `c97a283f52a12939edeff8fe1d38d69f4a9ce789` |
| Extracted core | `sigs.k8s.io/cluster-autoscaler v0.0.0-20260903143621-3d1c7137cdac` |
| Application module | `k8s.io/autoscaler/cluster-autoscaler` |
| API replacement | `k8s.io/autoscaler/cluster-autoscaler/apis => ./apis` |
| Live Kubernetes | `v1.37.0` |
| Application dependencies | Unchanged Kubernetes `v1.37.0-rc.1` and staging `v0.37.0-rc.1` pins |
| Build | Go `1.26.0`, Linux/amd64, `CGO_ENABLED=0`, readonly modules, trimpath, Azure tag |
| Upstream binary SHA-256 | `5ef4c8bf2d42f0b2a8c2bb1fe2eb6403d811b7af87ff1e89f977bb27e0498eee` |
| Candidate binary SHA-256 | `30e4401e2ddbe6fd15ff3533b177e8ade9a4d3f569dc4230bf214ff22a7070b1` |
| Shared runtime | `gcr.io/distroless/static@sha256:2293b36c7c9082bf4115aab724b4d2cddec82c8eba39bf27ac0517e159acf150` |
| Upstream local image | `localhost/cluster-autoscaler-azure:upstream-c85f5af`, CRI image ID `sha256:c8df9bfa2287c6273f3ec67f607d9cccaceafbb17eb19fb319b2779fa3d911fa` |
| Candidate local image | `localhost/cluster-autoscaler-azure:candidate-48f997f`, CRI image ID `sha256:f066e9429fe7aa8dc152a014f1efbbcc0402043a64d895bbcf9bb19796d56d71` |
| Candidate chart | Exact chart from candidate commit, inherited chart version `9.59.0` |
| Common values SHA-256 | `8f0d7fdee9cf4462afa13a974979bd4e8a29a3fecc42817727a258b52e0db269` |
| Upstream manifest SHA-256 | `2fcd94b0ba91eb48578b021302c6dcb098abc4d4ba3230f16922bec18cba75c2` |
| Candidate manifest SHA-256 | `4dd6755c2f84ff3dc8d5d53f72f873467540a4b92a11246beb391ffe16ca43ed` |

The normalized manifests were byte-identical except for the image tag. Both
used one replica, the same lease, explicit managed-identity selection, the same
node-group discovery tags and bounds, control-plane placement, host networking,
`imagePullPolicy: Never`, ordinary Delete mode, and the same protections and
timings.

## Tested profile

- One stock Ubuntu 24.04 Gen2 control plane outside autoscaled groups.
- Kubernetes `v1.37.0`, containerd `v2.3.5`, runc `v1.5.1`, and CNI plugins
  `v1.9.1`.
- Linux VMSS Uniform main pool with initial/min/max `1/1/2`.
- Linux VMSS Uniform zero pool with initial/min/max `0/0/1`.
- Maximum four `Standard_D2s_v5` VMs and eight vCPUs.
- Calico `v3.32.2` with VXLAN and Azure CCM/CNM `v1.36.6`.
- Separate scoped managed identities for cloud-controller and autoscaler duties.
- Inherited subscription agents remained present for both comparison arms and
  were not altered.
- Worker bootstrap installed pinned components and joined new instances
  autonomously through protected settings. No worker was manually repaired.
- CoreDNS and every other non-DaemonSet system workload stayed on the
  non-autoscaled control plane.

Calico did not publish a Kubernetes 1.37 test claim, and cloud-provider-azure had
not published a 1.37 release. Their versions were accepted as test candidates
and qualified only as part of this exact live profile.

## Paired outcomes

The CPU demand request was fixed at `1200m`. A worker had 2000m allocatable CPU
and 300m of Kubernetes DaemonSet requests, so one demand Pod fit and two did not.

| Case | Upstream | Candidate |
| --- | --- | --- |
| Authentication, discovery, leadership | Passed; one leader, only the two intended groups, bounds `1/2` and `0/1` | Passed with the same configuration |
| Five-minute idle stability | Passed at main/zero `1/0` | Passed at main/zero `1/0` |
| Main growth | Passed `1 -> 2`; new node joined Ready and both demand Pods scheduled | Passed identically |
| Maximum refusal | Third demand Pod stayed Pending for at least two minutes; main stayed 2 and zero stayed 0 | Passed identically |
| Scale from zero | Passed `0 -> 1`; predicted label matched the Ready node and workload | Passed identically |
| Scale to zero | Passed `1 -> 0`; VMSS instance, NIC, and Kubernetes Node disappeared | Passed identically |
| PDB protection | Two replicas occupied separate main nodes; `minAvailable: 2` explicitly blocked both removals for over five minutes | Passed identically |
| Safe Delete | `minAvailable: 1` allowed drain, rescheduling, physical instance/NIC/Node deletion, and healthy survivor | Passed identically |

Desired capacity alone was not treated as deletion evidence. Each deletion
required the selected VMSS instance and Kubernetes Node to disappear, with
remaining workloads Ready.

## Cutover and rollback

The upstream controller was stopped and cloud state settled before changing the
image. While replicas remained zero, only the image changed; the Secret and
normalized Deployment configuration remained unchanged. The candidate then ran
the complete paired cases and was stopped at main/zero `1/0`.

Rollback restored only the saved upstream image while replicas remained zero.
The exact upstream commit reacquired leadership, authenticated, rediscovered the
same groups, and completed a bounded main `1 -> 2 -> 1` growth and physical
Delete smoke. The zero pool stayed at 0 and the control plane remained healthy.

## Bootstrap corrections

Live qualification found and corrected temporary test-harness assumptions
without changing provider, core, API, or dependency code:

- containerd config version 4 uses `pinned_images.sandbox` instead of the older
  `sandbox_image` key.
- The cloud node manager must initialize provider identity, then CNI must make
  the control plane Ready before requiring the cloud-controller Deployment.
- Azure CLI treats `--disable-overprovision` as a switch.
- The temporary runtime image needed `WORKDIR /` because the chart starts
  `./cluster-autoscaler`.
- The shared manifest must use the chart's default lease name because its RBAC
  restricts lease updates to that name.

Every correction applied symmetrically before the affected comparison was
accepted.

## Cleanup

The autoscaler was stopped before cleanup. Bootstrap tokens were revoked, the
three run-created scoped role assignments were removed, the disposable worker
resources were deleted before infrastructure, and absence of the groups and
run-tagged resources was verified. Run-specific kubeconfig, join material, SSH
keypair, known-hosts file, and other credential files were removed. No test
resource remains billable.

## Limits and remaining release gates

- The run did not test AKS, deallocate, Flex, VMs-pool, Windows, HA control
  planes, multiple regions, or broader E2E.
- It did not establish official Kubernetes support ranges for the selected CNI
  or cloud-provider components.
- Inherited subscription agents affected guest resource usage equally in both
  arms. Their telemetry destination and incremental cost were outside this
  acceptance scope.
- The inherited apply-aware fake-client schema limitation remains separate and
  was not treated as live server-side apply proof.
- Hosted CI for the acceptance commit remains separate.
- Chart/application version, official image registry, signing/provenance,
  module publication, ownership, support, and security-contact decisions remain
  required before release.
