# Azure E2E skeleton

The manual GitHub Actions workflow uses a self-hosted 1ES runner with a user-assigned managed identity. CAPZ runs in kind on the runner and creates an AKS workload cluster through ASO. AKS exposes its public API endpoint, so the management cluster needs no peering.

The `scaleup` suite checks tagged-pool discovery, scale-up for pending Pods, and scale from zero followed by VM, Node and NIC deletion. The System pool stays outside discovery. ETag and the rest of the suite are left for later PRs.

## Run

Dispatch **Azure E2E matrix** on a trusted branch, or dispatch **Azure E2E** with `suite=scaleup`. Both workflows accept `kubernetes_version` (default `1.37.0`) and `location` (default `E2E_LOCATION`). They have no PR, push or scheduled triggers, and reusable calls only run when the caller was manually dispatched.

GitHub requires a dispatch workflow to exist on the repository's default branch before it can be run against another branch.

| Repository variable | Value |
| --- | --- |
| `E2E_RUNS_ON` | JSON label array, such as `["self-hosted","linux","x64","your-1es-pool"]` |
| `E2E_CLIENT_ID` | Runner user-assigned managed identity client ID |
| `E2E_SUBSCRIPTION_ID` | Disposable Azure subscription ID |
| `E2E_LOCATION` | AKS region with the requested version and D-series quota, such as `eastus2` |
| `E2E_RESOURCE_PREFIX` | Optional unique alphanumeric prefix starting with `e2ea`, default `e2ea` |

The runner needs Ubuntu, Docker with buildx, Git, Make, jq and Azure CLI. Actions installs Go 1.26.7, which the pinned CAPZ revision requires, and CAPZ installs its pinned kubectl, Helm, kind and kustomize. Reserve at least 16 GB of memory and 40 GB of free disk. Do not expose the runner label to workflows that execute untrusted PR code.

The runner identity needs subscription Contributor and Role Based Access Control Administrator with a condition allowing only Contributor, AcrPush and AcrPull assignments to service principals. The workflow grants itself AcrPush on its registry, and grants the workload cluster's kubelet identity Contributor on the node resource group and AcrPull on the registry. The autoscaler uses that kubelet identity through IMDS. No client secrets or federated identity credentials are needed.

Each run owns an ACR group, an AKS group and the AKS node group. Their exact names are recorded before creation. The workflow always collects logs and deletes its role assignments, clusters and resource groups. It waits for the groups to disappear. Cancellation or a runner failure can prevent those steps, so the pool operator must provide external cleanup for interrupted runs. Runs are serialized per subscription and have a 170-minute job deadline.

Artifacts include JUnit and JSON reports, the image source commit and digest, selected cluster logs, and the cleanup result. Kubeconfigs, Azure credentials and CAPZ's raw dumps are not uploaded.

## Local checks

```sh
make -C test test-local
```

On a disposable Azure VM with the same runner prerequisites, log in with its managed identity and select the test subscription. Export the workflow's Azure variables, `KUBERNETES_VERSION`, `CLUSTER_NAME` and an absolute `E2E_ROOT`, then run `bash test/hack/ci-e2e.sh run`. Run `collect` and `cleanup` even when creation or tests fail.

For an already prepared cluster, run `make -C test e2etests ENVIRONMENT=/absolute/path/to/environment.json TEST_SUITE=scaleup`. The binding names the kubeconfig, subscription, node resource group, main and zero VMSS, pool labels, autoscaler Deployment and image, and workload image and CPU request. The suite never selects a default cluster.
