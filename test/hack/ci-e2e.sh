#!/usr/bin/env bash

# Copyright The Kubernetes Authors.
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

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
module=$(cd "$here/.." && pwd)
repo=$(cd "$module/.." && pwd)

fail() { echo "$*" >&2; exit 1; }
[[ $# = 1 && ( $1 = build || $1 = test ) ]] || fail "Usage: ci-e2e.sh {build|test}"
[[ -n ${TAG:-} && $TAG =~ ^[a-zA-Z0-9_.-]+$ ]] || fail "Set a safe, explicit image TAG"
[[ -n ${IMAGE:-} && $IMAGE == */* && $IMAGE != /* && ${IMAGE##*/} != *:* ]] ||
    fail "Set IMAGE to a registry/repository (or set REGISTRY)"
image=$IMAGE:$TAG

if [[ $1 = build ]]; then
    make -C "$repo" image IMAGE="$IMAGE" TAG="$TAG" GOARCH=amd64
    docker push "$image"
    exit
fi

for tool in az kubectl helm jq; do
    command -v "$tool" >/dev/null || fail "$tool is required"
done
[[ ${TEST_SUITE:-scaleup} = scaleup ]] || fail "The AKS Prow path runs the scaleup suite only"
[[ -n ${LABEL_FILTER:-} ]] || fail "Choose an explicit Ginkgo label filter"
[[ -n ${CLUSTER_NAME:-} && $CLUSTER_NAME =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] ||
    fail "CAPZ must export a DNS-safe CLUSTER_NAME"
[[ -n ${KUBECONFIG:-} && $KUBECONFIG = /* && -f $KUBECONFIG ]] ||
    fail "CAPZ must export an absolute workload KUBECONFIG"
[[ -n ${AZURE_SUBSCRIPTION_ID:-} && -n ${AZURE_TENANT_ID:-} && -n ${AZURE_LOCATION:-} ]] ||
    fail "CAPZ Azure subscription, tenant and location are required"
[[ -n ${ARTIFACTS:-} ]] || fail "ARTIFACTS is required"
namespace=${CLUSTER_AUTOSCALER_NAMESPACE:-default}
account=${CLUSTER_AUTOSCALER_SERVICEACCOUNT_NAME:-cluster-autoscaler}
[[ $namespace =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ && $account =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] ||
    fail "The autoscaler namespace and service account must be DNS labels"
mkdir -p "$ARTIFACTS"
artifacts=$(cd "$ARTIFACTS" && pwd)
management=${MANAGEMENT_KUBECONFIG:-$(dirname "$KUBECONFIG")/${KIND_CLUSTER_NAME:-capz}.kubeconfig}
[[ -f $management ]] || fail "CAPZ management kubeconfig is missing: $management"
[[ $(az account show --query id -o tsv) = "$AZURE_SUBSCRIPTION_ID" ]] ||
    fail "Azure CLI is on a different subscription"

# Read the AKS objects that CAPZ created, as the upstream test-e2e target does.
mgmt() { kubectl --kubeconfig "$management" -n default "$@"; }
mgmt get cluster "$CLUSTER_NAME" -o json |
    jq -e --arg name "$CLUSTER_NAME" '.metadata.name == $name' >/dev/null
mgmt wait --for=condition=Ready --timeout=15m \
    "managedclusters.containerservice.azure.com/$CLUSTER_NAME" \
    "userassignedidentities.managedidentity.azure.com/$CLUSTER_NAME" \
    "federatedidentitycredentials.managedidentity.azure.com/$CLUSTER_NAME" \
    "roleassignments.authorization.azure.com/$CLUSTER_NAME"
group=$(mgmt get "managedclusters.containerservice.azure.com/$CLUSTER_NAME" -o json |
    jq -er '.status.nodeResourceGroup // empty')
[[ ${group,,} = "mc_${CLUSTER_NAME}_${CLUSTER_NAME}_${AZURE_LOCATION,,}" ]] ||
    fail "Unexpected AKS node resource group: $group"
client_id=$(mgmt get "userassignedidentities.managedidentity.azure.com/$CLUSTER_NAME" -o json |
    jq -er '.status.clientId // empty')
mgmt get managedclustersagentpools.containerservice.azure.com -o json | jq -e --arg run "$CLUSTER_NAME" '
    [.items[] | select(.spec.owner.name == $run)] as $pools |
    ($pools | length) == 3 and all($pools[]; (.status.enableAutoScaling // false) == false)
' >/dev/null || fail "Expected three AKS agent pools with the AKS autoscaler off"

sets=$(az vmss list -g "$group" --subscription "$AZURE_SUBSCRIPTION_ID" -o json)
pool_set() {
    jq -er --arg pool "$1" '[.[] | select(.tags["aks-managed-poolName"] == $pool) | .name] |
        if length == 1 then .[0] else empty end' <<< "$sets"
}
system=$(pool_set pool0) || fail "Expected one AKS VMSS for pool0"
main=$(pool_set main) || fail "Expected one AKS VMSS for main"
zero=$(pool_set zero) || fail "Expected one AKS VMSS for zero"
jq -e --arg run "$CLUSTER_NAME" --arg system "$system" --arg main "$main" --arg zero "$zero" '
    length == 3 and
    any(.[]; .name == $system and .tags["cluster-autoscaler-name"] == null and .sku.capacity == 1) and
    all(.[] | select(.name != $system); .tags["autoscaler-e2e-run"] == $run and
        .tags["cluster-autoscaler-name"] == $run and .overprovision == false) and
    any(.[]; .name == $main and .tags.min == "1" and .tags.max == "2" and
        .tags["k8s.io_cluster-autoscaler_node-template_label_acceptance-pool"] == "main" and .sku.capacity == 1) and
    any(.[]; .name == $zero and .tags.min == "0" and .tags.max == "1" and
        .tags["k8s.io_cluster-autoscaler_node-template_label_acceptance-pool"] == "zero" and .sku.capacity == 0)
' <<< "$sets" >/dev/null || fail "AKS VMSS bounds, tags or settings differ from the E2E fixture"

kubectl wait --for=condition=Ready node --all --timeout=15m
kubectl get pods,deployments,daemonsets,statefulsets -A -o json | jq -e '
    all(.items[]; all((if .kind == "Pod" then .spec.containers else .spec.template.spec.containers end)[];
        (.name | contains("cluster-autoscaler") | not) and
        (.image | contains("cluster-autoscaler") | not)))
' >/dev/null || fail "A cluster autoscaler is already deployed; use a fresh CAPZ cluster"

# AKS schedules some add-on replicas on User pools. Move them to the System
# pool, so that the workers run only test Pods and DaemonSets.
workers=$(kubectl get nodes -l 'kubernetes.azure.com/agentpool in (main,zero)' -o name)
[[ -n $workers ]] || fail "The main pool has no Node"
for node in $workers; do
    kubectl cordon "$node"
done
for node in $workers; do
    kubectl drain "$node" --ignore-daemonsets --delete-emptydir-data --timeout=10m
done
deadline=$((SECONDS + 600))
until kubectl get pods -A -o json | jq -e '
    all(.items[] | select(.status.phase != "Succeeded" and .status.phase != "Failed");
        .spec.nodeName != null and any(.status.conditions[]?; .type == "Ready" and .status == "True"))
' >/dev/null; do
    (( SECONDS < deadline )) || fail "System Pods did not become Ready on the System pool"
    sleep 10
done
for node in $workers; do
    kubectl uncordon "$node"
done

for item in "expendable:-15" "high:1000"; do
    name=${item%%:*}
    value=${item#*:}
    cat <<EOF | kubectl apply -f -
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: $CLUSTER_NAME-$name
  labels:
    autoscaler-e2e-run: $CLUSTER_NAME
value: $value
globalDefault: false
preemptionPolicy: PreemptLowerPriority
EOF
done

# Match the upstream Helm values: workload identity and label discovery.
release=cluster-autoscaler
deployment=$release-azure-cluster-autoscaler
tmp_dir=$(mktemp -d)
trap 'rm -f "$tmp_dir/values.yaml"; rmdir "$tmp_dir"' EXIT
cat > "$tmp_dir/values.yaml" <<EOF
autoDiscovery:
  clusterName: $CLUSTER_NAME
azureTenantID: $AZURE_TENANT_ID
azureSubscriptionID: $AZURE_SUBSCRIPTION_ID
azureResourceGroup: $group
azureUseWorkloadIdentityExtension: true
azureVMType: vmss
azureEnableForceDelete: false
podLabels:
  azure.workload.identity/use: "true"
rbac:
  serviceAccount:
    name: $account
    annotations:
      azure.workload.identity/tenant-id: $AZURE_TENANT_ID
      azure.workload.identity/client-id: $client_id
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
  logtostderr: true
  stderrthreshold: info
  v: 4
  write-status-configmap: true
  leader-elect: true
  leader-elect-resource-lock: leases
  scale-down-enabled: true
  scale-down-delay-after-add: 30s
  scale-down-unneeded-time: 30s
  scale-down-utilization-threshold: 0.5
  unremovable-node-recheck-timeout: 30s
  max-node-provision-time: 20m
  max-nodes-total: 4
  scan-interval: 10s
  skip-nodes-with-local-storage: true
  skip-nodes-with-system-pods: true
  max-graceful-termination-sec: 120
  expendable-pods-priority-cutoff: -10
  bypassed-scheduler-names: non-existing-bypassed-scheduler
updateStrategy:
  type: Recreate
EOF
helm upgrade --install "$release" "$repo/charts/cluster-autoscaler" -n "$namespace" \
    -f "$tmp_dir/values.yaml" --wait --timeout 5m

jq -n --arg kubeconfig "$KUBECONFIG" --arg run "$CLUSTER_NAME" --arg sub "$AZURE_SUBSCRIPTION_ID" \
    --arg group "$group" --arg main "$main" --arg zero "$zero" --arg image "$image" \
    --arg namespace "$namespace" --arg deployment "$deployment" \
    --arg workload "${WORKLOAD_IMAGE:-busybox@sha256:b7f3d86d6e84fc17718c48bcde1450807faa2d56704205c697b4bd5df7b9e29f}" \
    --arg cpu "${DEMAND_CPU:-1200m}" '
    {kubeconfig:$kubeconfig,runID:$run,subscriptionID:$sub,resourceGroup:$group,
     autoscalerNamespace:$namespace,autoscalerDeployment:$deployment,
     autoscalerContainer:"azure-cluster-autoscaler",expectedImage:$image,
     mainPool:$main,zeroPool:$zero,poolLabel:"acceptance-pool",mainLabel:"main",zeroLabel:"zero",
     demandCPU:$cpu,workloadImage:$workload}' > "$artifacts/environment.json"

make -C "$module" e2etests ENVIRONMENT="$artifacts/environment.json" ARTIFACTS="$artifacts" \
    LABEL_FILTER="$LABEL_FILTER" TEST_SUITE=scaleup TEST_TIMEOUT="${TEST_TIMEOUT:-3h}"
