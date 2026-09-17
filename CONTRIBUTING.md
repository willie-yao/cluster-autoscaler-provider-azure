# Contributing

Changes should preserve the Azure provider's contracts with Kubernetes and the
shared autoscaling core. Read the [architecture guide](docs/architecture.md)
before changing module boundaries, discovery, templates or scaling operations.

## Local workflow

Use Go 1.26 or later, Git, Make and a C toolchain for race tests. Helm is needed
for chart checks; Docker is needed only when building an image.

Create a focused branch from current `main`, reproduce the issue, and add a
regression at the closest existing test boundary. Follow nearby Go conventions
and preserve source attribution and license headers. Use the existing module
pins unless a dependency change is part of the proposal.

From the repository root:

```sh
make format
make test-ci
make test-chart
git diff --check
```

On macOS, use `make test-ci GOOS=darwin`. Start with a targeted test while
iterating, then run the applicable checks before requesting review. The
[testing guide](docs/testing.md) explains which targets enter the nested API
and E2E modules. A root `go test ./...` does not test those modules.

For API changes, use the existing `apis/Makefile` targets (`manifests`,
`generate`, `clients`) as appropriate and include the resulting generated
changes. Do not hand-edit generated clients to hide a generator mismatch.

## Chart and E2E changes

Chart changes must pass `make test-chart`. The frozen upstream YAML is an
independent oracle, not output to regenerate from the candidate. Explain
intentional compatibility changes and preserve the
[fixture provenance](charts/testdata/azure-compatibility/README.md).
Chart/app version changes are separate release decisions; the existing PR
chart-version check is not waived by a local test pass.

The maintained E2E module uses an explicitly prepared disposable environment.
Use `make test-e2e-local` for development without cloud credentials. Never use
legacy setup targets as a substitute for the
[operator contract](cloudprovider/azure/test/README.md#operator-contract).
Live runs need separate authorization, ownership, budget and cleanup planning.
Keep credentials, kubeconfigs and private run artifacts out of commits.

## Pull requests

Describe the problem, intended behavior and compatibility impact. Include
reproduction steps, the checks run and any untested boundaries. For live test
results, distinguish the test-source commit from the runtime image's source
commit and digest. Link historical evidence at immutable commits instead of
claiming it validates new code.

Keep changes reviewable and avoid unrelated refactors. Update directly affected
documentation, fixtures and CodeTour anchors. Verify each tour path, line or
pattern still points at the described code.

Use the repository issue and PR templates to provide context. This guide does
not establish release authority, an official registry, named maintainers,
security contacts or additional contribution agreements.
