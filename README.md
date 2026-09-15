# Azure Cluster Autoscaler bootstrap

This is a local bootstrap for a future Azure-owned, Azure-only Cluster Autoscaler repository. It promotes the upstream `cluster-autoscaler/` application to the repository root while preserving its existing extracted-core builder integration.

This baseline supports Azure Delete-mode behavior from upstream. It does not implement AKS deallocate-mode parity, represent an official Azure release, or identify an official image registry or publishing destination.

## Build

Use Go 1.26 or later:

```shell
make build
```

The resulting `cluster-autoscaler-<arch>` binary is Azure-only. `--cloud-provider` defaults to `azure` and lists no other providers.

## Test

```shell
make test-azure
make test-ci
make test-chart # requires Helm
```

To build a local image without publishing it:

```shell
make image IMAGE=cluster-autoscaler-azure TAG=dev
```

The Helm chart remains available at `charts/cluster-autoscaler`. Set `image.repository` and `image.tag` to an image published by your organization before deploying it. No official repository or image publication identity is configured in this bootstrap.

The [Phase 1 migration record](docs/phase-1-migration.md) lists exact pins, local
evidence, live acceptance gates, and cutover/rollback criteria. Phase 1 is not
complete until those live gates pass.

## Azure configuration

Azure provider configuration and deployment guidance are retained under [cloudprovider/azure](cloudprovider/azure/README.md). The APIs module remains local at [apis](apis) through the application module's `replace` directive.

## Upstream provenance

This bootstrap is derived from `kubernetes/autoscaler`. Its Go module identity is intentionally unchanged for the initial executable extraction. A separate module identity and publication decision is required before this repository is released for downstream Go-module consumption.
