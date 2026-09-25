# Source provenance and compatibility scope

## Source identity

| Component | Source |
| --- | --- |
| Upstream application and chart | [`kubernetes/autoscaler@c85f5afed954f7ecdbfb9091da2202426806d8d8`](https://github.com/kubernetes/autoscaler/tree/c85f5afed954f7ecdbfb9091da2202426806d8d8) |
| Extracted autoscaling core | [`kubernetes-sigs/cluster-autoscaler@v0.0.0-k8s.v1.37.0`](https://github.com/kubernetes-sigs/cluster-autoscaler/tree/v0.0.0-k8s.v1.37.0), module `sigs.k8s.io/cluster-autoscaler v0.0.0-k8s.v1.37.0` |
| Go module dependency versions | [`kubernetes/autoscaler@9c6b587bbf54825ec8253caaae77db612a75ef61`](https://github.com/kubernetes/autoscaler/blob/9c6b587bbf54825ec8253caaae77db612a75ef61/cluster-autoscaler/go.mod), which adopted the core's Kubernetes 1.37.0 release |
| Autoscaler APIs | [`kubernetes/autoscaler@eec9bc4dc1d2`](https://github.com/kubernetes/autoscaler/tree/eec9bc4dc1d2/cluster-autoscaler/apis), module `k8s.io/autoscaler/cluster-autoscaler/apis v0.0.0-20260717085528-eec9bc4dc1d2`, the version required by the extracted core |

The application keeps the `k8s.io/autoscaler/cluster-autoscaler` module path.
It uses the published API module rather than a local copy; that module's Go
source is identical to the API source at the upstream application pin.
The root module's required versions match upstream at `9c6b587b`. Between the
upstream application pin and `9c6b587b`, upstream changed no Azure provider
source, main program or chart. Upstream history, source licenses and
copyrights are retained.

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
