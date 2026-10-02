# Legacy AKS development setup

The files in this directory came from upstream's AKS development setup. They are separate from the E2E module in `test/`. The old `azure/autoscaler` Codespaces instructions do not apply to this repository.

| File | Purpose |
| --- | --- |
| [aks-dev.bicep](aks-dev.bicep) | Creates AKS, ACR and a workload identity |
| [aks-dev-deploy.sh](aks-dev-deploy.sh) | Creates resources and writes local deployment settings |
| [skaffold.yaml](skaffold.yaml) | Builds the root application and deploys it with a sample workload |
| [cluster-autoscaler-vmss-wi-dynamic.yaml.tpl](cluster-autoscaler-vmss-wi-dynamic.yaml.tpl) | Template for the generated workload identity manifest |

The setup requires Azure credentials, permission to create resources and role assignments, and a disposable subscription or resource group. It creates billable resources and does not clean them up.

The manifest template currently has whitespace before the VMSS name in its `--nodes` argument. The setup script also assumes the Azure CLI returns JSON for the deployment result. Review and correct those issues before using the legacy workflow. Do not use `make -C test setup-cluster` or the legacy deployment targets as E2E preparation.

For a manual installation, use the [root chart guide](../../README.md#install). The project is pre-release, with no general AKS compatibility claim.
