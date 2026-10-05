## Overview:

This document, and directory are focused on the ability to deploy and test a working version of autoscaler from a development branch onto an AKS cluster for testing out a set of changes.

The setup requires Azure credentials, permission to create resources and role assignments, and a disposable subscription or resource group. It creates billable resources and does not clean them up.

Use Docker, Azure CLI, `jq`, `yq`, `kubectl` and Skaffold. The setup script assumes the Azure CLI returns JSON for the deployment result, so configure JSON output before running it.

## Steps:

1. Clone https://github.com/willie-yao/cluster-autoscaler-provider-azure and switch to whatever branch you want to test.

2. run `cd deploy/dev` from the repository root.

3. run `az login` and select your intended subscription.

4. run `./aks-dev-deploy.sh`

5. run `cd ../..`

6. run `skaffold run --filename deploy/dev/skaffold.yaml`

7. inspect the cluster with `kubectl`, and scale the `inflate` deployment for testing as desired.

The `setup-cluster` and `deploy-local` targets in [test/Makefile](../../test/Makefile) run the same setup and deployment steps. The project is pre-release, with no general AKS compatibility claim.
