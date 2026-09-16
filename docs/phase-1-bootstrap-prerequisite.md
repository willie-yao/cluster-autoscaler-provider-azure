# Bootstrap prerequisite: blocked

Read-only assessment on September 15, 2026. The Kubernetes v1.37.0, West US 3,
Linux VMSS Uniform and four-VM acceptance envelope is unchanged. **No usable,
fully pinned bootstrap was established within this scope. No provisioning
approval is requested.**

## Existing source inspected

The concrete self-managed reference is CAPZ **v1.27.0**, commit
`3d9e0c36bebc1432048148f168a9bb7415e756a7`:

- `templates/cluster-template-machinepool.yaml`: KubeadmControlPlane,
  AzureMachinePool and KubeadmConfig init/join wiring.
- `templates/test/ci/prow-machine-pool-ci-version/patches/kubeadm-bootstrap-k8s-ci-binaries.yaml`:
  an existing boot-time Kubernetes binary/image replacement path.
- `azure/defaults.go` and `docs/book/src/self-managed/custom-images.md`:
  published reference-image discovery.
- `templates/addons/cluster-api-helm/{calico,cloud-provider-azure}.yaml`:
  addon configuration.

This is an inspectable candidate source, not a selected turnkey bootstrap. Its
normal lifecycle retains CAPI/CAPZ ownership of worker capacity; a handoff that
avoids competing with the direct Azure autoscaler is not established by the
unmodified template. No management cluster or replacement bootstrap was created.

## Artifact status

| Component | Evidence | Status |
| --- | --- | --- |
| Node image | Release-pinned gallery `ClusterAPI-f72ceb4f-5159-4c26-a0fe-2ea738f0d019`, definition `capi-ubun2-2404` | West US 3 version listing returned `NotFound`; no usable image pin there |
| Catalog cross-check | Same gallery/definition in documented `northcentralus` | Listing succeeded: 78 versions, maximum `1.36.2`, no `1.37.0` |
| containerd/runc | Catalog responses contain empty `artifactTags`; the inspected Kubernetes replacement helper does not pin/install these runtimes | No verified runtime/BOM pin; no guest inspection performed |
| Kubernetes binaries | Official v1.37.0 Linux/amd64 checksum metadata retrieved below | Expected hashes pinned; binaries not downloaded/executed |
| CNI | CAPZ static manifest references Calico `v3.29.4`; Helm alternative requires `${CALICO_VERSION}` | Reference inputs found, but no v1.37.0 acceptance artifact set selected |
| CCM | CAPZ HelmChartProxy uses the cloud-provider-azure `master` Helm repository without a chart version | Not an immutable CCM/chart pin |

The existing binary replacement helper is not image or runtime proof. I did not
copy an image between regions, substitute an older image, select arbitrary
runtime/addon versions, change application dependencies, or start an image build.
The gallery result does not prove that no suitable image exists anywhere; it
establishes that this inspected public route does not currently supply the
required lock set for the retained envelope.

### Published Kubernetes checksum metadata

Source: `https://dl.k8s.io/release/v1.37.0/bin/linux/amd64/<binary>.sha256`.
These are expected download hashes, not verification of downloaded binaries.

| Binary | SHA-256 |
| --- | --- |
| `kubeadm` | `9406f2e75c7fc99391f5fb0575b07914f2d90750d2b8c2c473e9c9c5194c9352` |
| `kubelet` | `ee554f77da57ad40a5d5f8625ac9c4af8b75d9d5a259367a885e5e816954b044` |
| `kubectl` | `6129359f4e1f3848a5572ccb0b26cf28b8ca08cef38c95a765b2f64a2c961a2f` |

## Remaining inputs

**Technical unblocker, not provisioning approval:** an existing, inspectable
self-managed bootstrap and image reference available in West US 3, including
image version/resource ID and build provenance for its runtime and components.
It must support autonomous VMSS joining and avoid a competing capacity controller.
Supplying such a reference would allow another bounded read-only audit; it would
not by itself authorize deployment. I am not asking the operator to approve
unknown image/runtime/CNI/CCM versions.

After that is resolved, ordinary operator inputs remain:

- Approved subscription, zone/SKU quota, resource names and non-overlapping CIDRs.
- Operator source IP and SSH public key, not the private key.
- Approved scoped identity/role plan and setup permissions.
- Run window, primary cleanup owner and named backup within the retained
  eight-hour/$20 ceiling.

The acceptance plan now requires CoreDNS and all other non-DaemonSet system
workloads on the control plane, with required placement. Autonomous joining,
correct identity/version, sustained readiness, working networking/DNS and system
placement of the first worker must pass before either autoscaler starts.

No cloud mutation, role assignment, guest operation, binary execution, image
transfer/publication, dependency edit, commit or push occurred in this assessment.
The reviewed candidate remains `48f997fcd8d4a93f25d06837e185ea77d4e2c5cd`.
