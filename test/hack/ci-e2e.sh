#!/usr/bin/env bash

# Copyright (c) Microsoft Corporation.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail
umask 077

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
fail() { echo "$*" >&2; exit 1; }
[[ $# = 1 && $1 =~ ^(run|test|collect|cleanup)$ ]] || fail "Usage: ci-e2e.sh {run|test|collect|cleanup}"
: "${E2E_ROOT:?Set an absolute directory for this run}"
: "${CLUSTER_NAME:?Set a unique e2ea-prefixed cluster name}"
: "${AZURE_SUBSCRIPTION_ID:?}"
: "${AZURE_LOCATION:?}"
[[ $E2E_ROOT = /* && $CLUSTER_NAME =~ ^e2ea[a-z0-9]+$ && ${#CLUSTER_NAME} -le 36 ]] ||
    fail "Expected an absolute E2E_ROOT and an e2ea-prefixed alphanumeric cluster name (up to 36 characters)"
[[ $AZURE_LOCATION =~ ^[a-z0-9]+$ ]] || fail "Invalid Azure location"
capz=$E2E_ROOT/capz
export ARTIFACTS=$E2E_ROOT/artifacts
export KIND_CLUSTER_NAME=$CLUSTER_NAME
export MANAGEMENT_KUBECONFIG=$capz/$KIND_CLUSTER_NAME.kubeconfig
export PATH="$capz/hack/tools/bin:$PATH"
node_group=MC_${CLUSTER_NAME}_${CLUSTER_NAME}_${AZURE_LOCATION}
registry_group=$CLUSTER_NAME-registry
acr=${CLUSTER_NAME}acr
groups=("$CLUSTER_NAME" "$node_group" "$registry_group")
mkdir -p "$ARTIFACTS"
azs() { az "$@" --subscription "$AZURE_SUBSCRIPTION_ID"; }
mgmt() { kubectl --kubeconfig "$MANAGEMENT_KUBECONFIG" -n default "$@"; }
workload() { kubectl --kubeconfig "$capz/kubeconfig" --request-timeout=30s "$@"; }

run() {
    : "${AZURE_CLIENT_ID:?}"
    : "${KUBERNETES_VERSION:?}"
    [[ ${TEST_SUITE:-scaleup} = scaleup ]] || fail "Only the scaleup suite is supported"
    for tool in az docker git make jq; do
        command -v "$tool" >/dev/null || fail "$tool is required"
    done
    docker info >/dev/null
    [[ ! -e $E2E_ROOT/groups.txt ]] || fail "This run directory was already used"
    for group in "${groups[@]}"; do
        [[ $(azs group exists --name "$group") = false ]] || fail "Resource group already exists: $group"
    done
    printf '%s\n' "${groups[@]}" > "$E2E_ROOT/groups.txt"
    azs group create --name "$registry_group" --location "$AZURE_LOCATION" \
        --tags "autoscaler-e2e-run=$CLUSTER_NAME" --output none
    azs acr create --name "$acr" --resource-group "$registry_group" --sku Basic --admin-enabled false --output none
    acr_id=$(azs acr show --name "$acr" --resource-group "$registry_group" --query id --output tsv)
    principal=$(azs identity list --query "[?clientId=='$AZURE_CLIENT_ID'].principalId | [0]" --output tsv)
    [[ -n $principal ]] || fail "Could not find the runner's user-assigned identity"
    azs role assignment create --assignee-object-id "$principal" --assignee-principal-type ServicePrincipal \
        --role AcrPush --scope "$acr_id" --query id --output tsv >> "$E2E_ROOT/assignments.txt"
    export REGISTRY=$acr.azurecr.io TAG
    TAG=$(git -C "$repo" rev-parse HEAD)
    export AZURE_TENANT_ID
    AZURE_TENANT_ID=$(azs account show --query tenantId --output tsv)
    capz_sha=0c46a7695efe78669ccd85105cef15aaff27cc9c
    git init -q "$capz"
    git -C "$capz" fetch -q --depth=1 https://github.com/kubernetes-sigs/cluster-api-provider-azure.git "$capz_sha"
    git -C "$capz" checkout -q FETCH_HEAD
    # CAPZ's Makefile installs its pinned kubectl, Helm, kind and kustomize.
    export AZWI_ENABLED=false ASO_CREDENTIAL_SECRET_MODE=podidentity
    export AZURE_CLIENT_ID_USER_ASSIGNED_IDENTITY=$AZURE_CLIENT_ID
    export ADDITIONAL_ASO_CRDS='authorization.azure.com/*;managedidentity.azure.com/*'
    export CLUSTER_TEMPLATE=$repo/test/templates/cluster-template-prow-aks-aso-e2e.yaml
    export CLUSTER_CREATE_ATTEMPTS=1 AKS_TIER=Free JOB_NAME=$CLUSTER_NAME
    export SKIP_CLEANUP=true
    cd "$capz"
    unset KUBECONFIG
    ARTIFACTS=$E2E_ROOT/capz-artifacts bash scripts/ci-entrypoint.sh \
        make -C "$repo/test" test-e2e TEST_SUITE="${TEST_SUITE:-scaleup}" \
        ARTIFACTS="$E2E_ROOT/artifacts" REGISTRY="$REGISTRY" TAG="$TAG"
}

test_cluster() {
    : "${KUBECONFIG:?CAPZ must provide the workload kubeconfig}"
    : "${IMAGE:?}"
    : "${TAG:?}"
    mgmt wait --for=condition=Ready --timeout=15m "managedclusters.containerservice.azure.com/$CLUSTER_NAME"
    group=$(mgmt get "managedclusters.containerservice.azure.com/$CLUSTER_NAME" -o json | jq -er '.status.nodeResourceGroup')
    [[ ${group,,} = "${node_group,,}" ]] || fail "Unexpected node resource group: $group"
    kubelet=$(mgmt get "managedclusters.containerservice.azure.com/$CLUSTER_NAME" -o json |
        jq -ec '.status.identityProfile.kubeletidentity | select(.clientId and .objectId)')
    client_id=$(jq -r .clientId <<< "$kubelet")
    principal=$(jq -r .objectId <<< "$kubelet")
    azs role assignment create --assignee-object-id "$principal" --assignee-principal-type ServicePrincipal \
        --role Contributor --scope "/subscriptions/$AZURE_SUBSCRIPTION_ID/resourceGroups/$group" \
        --query id --output tsv >> "$E2E_ROOT/assignments.txt"
    acr_id=$(azs acr show --name "$acr" --resource-group "$registry_group" --query id --output tsv)
    azs role assignment create --assignee-object-id "$principal" --assignee-principal-type ServicePrincipal \
        --role AcrPull --scope "$acr_id" --query id --output tsv >> "$E2E_ROOT/assignments.txt"

    sets=$(azs vmss list --resource-group "$group" --output json)
    pool_set() {
        jq -er --arg pool "$1" '[.[] | select(.tags["aks-managed-poolName"] == $pool) | .name] |
            if length == 1 then .[0] else empty end' <<< "$sets"
    }
    main=$(pool_set main)
    zero=$(pool_set zero)
    jq -e --arg run "$CLUSTER_NAME" '
        length == 3 and
        any(.[]; .tags["aks-managed-poolName"] == "pool0" and .sku.capacity == 1 and
            .tags["cluster-autoscaler-name"] == null) and
        any(.[]; .tags["aks-managed-poolName"] == "main" and .sku.capacity == 1 and
            .tags.min == "1" and .tags.max == "2" and .tags["cluster-autoscaler-name"] == $run) and
        any(.[]; .tags["aks-managed-poolName"] == "zero" and .sku.capacity == 0 and
            .tags.min == "0" and .tags.max == "1" and .tags["cluster-autoscaler-name"] == $run)
    ' <<< "$sets" >/dev/null || fail "The three VMSS do not match the test fixture"
    mgmt get managedclustersagentpools.containerservice.azure.com -o json |
        jq -e --arg run "$CLUSTER_NAME" '[.items[] | select(.spec.owner.name == $run)] |
            length == 3 and all(.[]; (.status.enableAutoScaling // false) == false)' >/dev/null

    # AKS still rolls out add-ons and VM extensions after CAPZ reports Ready.
    settled() {
        local current set cluster instances
        cluster=$(azs aks show --resource-group "$CLUSTER_NAME" --name "$CLUSTER_NAME" --output json) ||
            fail "Could not read AKS state"
        jq -e '.provisioningState == "Succeeded" and (.agentPoolProfiles |
            length == 3 and all(.[]; .provisioningState == "Succeeded"))' <<< "$cluster" >/dev/null || return 1
        current=$(azs vmss list --resource-group "$group" --output json) || fail "Could not read VMSS state"
        jq -e 'length == 3 and all(.[]; .provisioningState == "Succeeded")' \
            <<< "$current" >/dev/null || return 1
        for set in $(jq -r '.[].name' <<< "$current"); do
            instances=$(azs vmss list-instances --resource-group "$group" --name "$set" --output json) ||
                fail "Could not read VMSS instances"
            jq -e 'all(.[]; .provisioningState == "Succeeded" and .latestModelApplied == true)' \
                <<< "$instances" >/dev/null || return 1
        done
        kubectl get nodes -o json | jq -e '(.items | length) == 2 and
            all(.items[]; any(.status.conditions[]; .type == "Ready" and .status == "True"))' >/dev/null || return 1
        kubectl -n kube-system get deployments -o json | jq -e 'all(.items[];
            (.status.observedGeneration // 0) >= .metadata.generation and
            (.status.replicas // 0) == .spec.replicas and
            (.status.updatedReplicas // 0) == .spec.replicas and
            (.status.availableReplicas // 0) == .spec.replicas)' >/dev/null
    }
    deadline=$((SECONDS + 2700))
    passes=0
    until (( passes == 3 )); do
        if settled; then passes=$((passes + 1)); else passes=0; fi
        echo "AKS settlement checks: $passes/3"
        (( SECONDS < deadline )) || fail "AKS did not settle within 45 minutes"
        sleep 30
    done
    # Pin metrics-server's current requests so its VPA doesn't replace Pods during scale-down.
    metrics=$(kubectl -n kube-system get deployment metrics-server -o json)
    cpu=$(jq -er '.spec.template.spec.containers[] | select(.name == "metrics-server") | .resources.requests.cpu' <<< "$metrics")
    memory=$(jq -er '.spec.template.spec.containers[] | select(.name == "metrics-server") | .resources.requests.memory' <<< "$metrics")
    kubectl -n kube-system create configmap metrics-server-config --dry-run=client -o yaml \
        --from-literal="NannyConfiguration=apiVersion: nannyconfig/v1alpha1
kind: NannyConfiguration
baseCPU: $cpu
cpuPerNode: 0m
baseMemory: $memory
memoryPerNode: 0Mi" | kubectl apply -f -
    kubectl -n kube-system rollout restart deployment/metrics-server
    kubectl -n kube-system rollout status deployment/metrics-server --timeout=10m
    # Keep AKS add-on replicas on the untagged System pool.
    workers=$(kubectl get nodes -l 'kubernetes.azure.com/agentpool in (main,zero)' -o name)
    [[ -n $workers ]] || fail "The main pool has no Node"
    for node in $workers; do kubectl cordon "$node"; done
    for node in $workers; do kubectl drain "$node" --ignore-daemonsets --delete-emptydir-data --timeout=10m; done
    kubectl -n kube-system wait --for=condition=Ready pod --all --timeout=10m
    for node in $workers; do kubectl uncordon "$node"; done

    helm upgrade --install cluster-autoscaler "$repo/charts/cluster-autoscaler" \
        --namespace default --wait --timeout=5m --values /dev/stdin <<EOF
azureUseManagedIdentityExtension: true
azureUserAssignedIdentityID: $client_id
azureSubscriptionID: $AZURE_SUBSCRIPTION_ID
azureResourceGroup: $group
autoDiscovery:
  clusterName: $CLUSTER_NAME
replicaCount: 1
image:
  repository: $IMAGE
  tag: $TAG
  pullPolicy: Always
nodeSelector:
  kubernetes.io/os: linux
  kubernetes.azure.com/agentpool: pool0
resources:
  requests: {cpu: 100m, memory: 256Mi}
  limits: {cpu: 500m, memory: 512Mi}
extraArgs:
  write-status-configmap: true
  leader-elect: true
  scale-down-delay-after-add: 30s
  scale-down-unneeded-time: 30s
  unremovable-node-recheck-timeout: 30s
  max-node-provision-time: 20m
  max-nodes-total: 4
  scan-interval: 10s
  skip-nodes-with-local-storage: true
  skip-nodes-with-system-pods: true
EOF
    azs acr repository show --name "$acr" --image "${IMAGE#*/}:$TAG" --query digest --output tsv \
        > "$ARTIFACTS/image-digest.txt"
    git -C "$repo" rev-parse HEAD > "$ARTIFACTS/source-commit.txt"
    jq -n --arg kubeconfig "$KUBECONFIG" --arg run "$CLUSTER_NAME" --arg sub "$AZURE_SUBSCRIPTION_ID" \
        --arg group "$group" --arg main "$main" --arg zero "$zero" --arg image "$IMAGE:$TAG" '
        {kubeconfig:$kubeconfig,runID:$run,subscriptionID:$sub,resourceGroup:$group,
         autoscalerNamespace:"default",autoscalerDeployment:"cluster-autoscaler-azure-cluster-autoscaler",
         autoscalerContainer:"azure-cluster-autoscaler",expectedImage:$image,
         mainPool:$main,zeroPool:$zero,poolLabel:"acceptance-pool",mainLabel:"main",zeroLabel:"zero",
         demandCPU:"1200m",workloadImage:"busybox@sha256:b7f3d86d6e84fc17718c48bcde1450807faa2d56704205c697b4bd5df7b9e29f"}' \
         > "$ARTIFACTS/environment.json"
    make -C "$repo/test" e2etests ENVIRONMENT="$ARTIFACTS/environment.json" \
        ARTIFACTS="$ARTIFACTS" TEST_SUITE=scaleup TEST_TIMEOUT="${TEST_TIMEOUT:-75m}"
}

collect() {
    if [[ -f $capz/kubeconfig ]]; then
        for kind in nodes pods events; do
            workload get "$kind" -A -o wide > "$ARTIFACTS/$kind.txt" 2>&1 ||
                echo "Could not collect $kind" >&2
        done
        workload -n default logs deployment/cluster-autoscaler-azure-cluster-autoscaler \
            > "$ARTIFACTS/autoscaler.log" 2>&1 || echo "Could not collect autoscaler logs" >&2
    fi
    if [[ -f $MANAGEMENT_KUBECONFIG ]]; then
        mgmt get cluster,machinepools,managedclusters.containerservice.azure.com \
            -o wide > "$ARTIFACTS/management.txt" 2>&1 || echo "Could not collect management status" >&2
    fi
}

cleanup() {
    [[ -f $E2E_ROOT/groups.txt ]] || return 0
    diff -u "$E2E_ROOT/groups.txt" <(printf '%s\n' "${groups[@]}") ||
        fail "Resource group manifest does not match this run"
    local result=0 exists
    if [[ -f $E2E_ROOT/assignments.txt ]]; then
        while IFS= read -r id; do
            if [[ $id != "/subscriptions/$AZURE_SUBSCRIPTION_ID/resourceGroups/$node_group/providers/Microsoft.Authorization/roleAssignments/"* &&
                $id != "/subscriptions/$AZURE_SUBSCRIPTION_ID/resourceGroups/$registry_group/providers/Microsoft.ContainerRegistry/registries/$acr/providers/Microsoft.Authorization/roleAssignments/"* ]]; then
                echo "Role assignment is outside this run's resource groups: $id" >&2
                result=1
                continue
            fi
            azs role assignment delete --ids "$id" ||
                { echo "Could not delete role assignment $id" >&2; result=1; }
        done < "$E2E_ROOT/assignments.txt"
    fi
    if [[ -f $MANAGEMENT_KUBECONFIG ]]; then
        timeout 1200 kubectl --kubeconfig "$MANAGEMENT_KUBECONFIG" -n default delete cluster "$CLUSTER_NAME" \
            --ignore-not-found --wait=true || echo "CAPZ delete did not finish; deleting the exact groups directly" >&2
    fi
    if command -v kind >/dev/null; then
        kind delete cluster --name "$KIND_CLUSTER_NAME" ||
            { echo "Could not stop the kind management cluster" >&2; result=1; }
    fi
    for group in "${groups[@]}"; do
        if ! exists=$(azs group exists --name "$group"); then
            echo "Could not check resource group $group" >&2
            result=1
            continue
        fi
        if [[ $exists = true ]]; then
            azs group delete --name "$group" --yes --no-wait ||
                { echo "Could not request deletion of $group" >&2; result=1; }
        fi
    done
    deadline=$((SECONDS + 1200))
    for group in "${groups[@]}"; do
        while true; do
            if ! exists=$(azs group exists --name "$group"); then
                echo "Could not verify resource group deletion: $group" >&2
                result=1
                break
            fi
            [[ $exists = true ]] || break
            if (( SECONDS >= deadline )); then
                echo "Resource group still exists: $group" >&2
                result=1
                break
            fi
            sleep 20
        done
        echo "$group: exists=$exists" | tee -a "$ARTIFACTS/cleanup.txt"
    done
    return "$result"
}

case $1 in
    run) run ;;
    test) test_cluster ;;
    collect) collect ;;
    cleanup) cleanup ;;
esac
