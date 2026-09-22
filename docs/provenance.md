# Source provenance and compatibility scope

## Source identity

| Component | Source |
| --- | --- |
| Upstream application and chart | [`kubernetes/autoscaler@c85f5afed954f7ecdbfb9091da2202426806d8d8`](https://github.com/kubernetes/autoscaler/tree/c85f5afed954f7ecdbfb9091da2202426806d8d8) |
| Extracted autoscaling core | [`kubernetes-sigs/cluster-autoscaler@3d1c7137cdac`](https://github.com/kubernetes-sigs/cluster-autoscaler/commit/3d1c7137cdac), module `sigs.k8s.io/cluster-autoscaler v0.0.0-20260903143621-3d1c7137cdac` |

The application keeps `k8s.io/autoscaler/cluster-autoscaler` and its local
`./apis` replacement. Upstream history, source licenses and copyrights are
retained.

The Azure-only source omits the GCE-only fault-injection utility and unused
protobuf-generator module; active build, verification and Kubernetes
dependency-maintenance tooling is retained.

## Compatibility scope

The provider follows ordinary upstream Azure Delete-mode behavior. It does not
implement AKS deallocate-mode behavior or claim parity with the
[`Azure/autoscaler` fork](https://github.com/Azure/autoscaler).

Azure-only registration, image placeholders and the managed-identity
existing-Secret correction are intentional differences from the upstream
application. The [chart oracle](../charts/testdata/azure-compatibility/README.md)
records the precise packaging comparison and attribution.

For local checks and validation boundaries, see [testing](testing.md).
