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
repo=$(cd "$module/../../.." && pwd)

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

for tool in az kubectl helm jq yq; do
    command -v "$tool" >/dev/null || fail "$tool is required"
done
[[ ${TEST_SUITE:-scaleup} = scaleup ]] ||
    fail "The AKS Prow path runs scaleup only; phased cases need custom self-managed VMSS"
prepare=${E2E_PREPARE:-}
[[ $prepare =~ ^(etag|disk|dra|taint)?$ ]] || fail "E2E_PREPARE must be empty, etag, disk, dra or taint"
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
# workload uses the template's workload identity. kubelet uses the AKS kubelet
# identity through IMDS, for subscriptions that don't allow federated
# credentials; the template then needs no identity resources.
identity=${CLUSTER_AUTOSCALER_IDENTITY:-workload}
[[ $identity = workload || $identity = kubelet ]] ||
    fail "CLUSTER_AUTOSCALER_IDENTITY must be workload or kubelet"
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
resources=("managedclusters.containerservice.azure.com/$CLUSTER_NAME")
if [[ $identity = workload ]]; then
    resources+=("userassignedidentities.managedidentity.azure.com/$CLUSTER_NAME"
        "federatedidentitycredentials.managedidentity.azure.com/$CLUSTER_NAME"
        "roleassignments.authorization.azure.com/$CLUSTER_NAME")
fi
mgmt wait --for=condition=Ready --timeout=15m "${resources[@]}"
group=$(mgmt get "managedclusters.containerservice.azure.com/$CLUSTER_NAME" -o json |
    jq -er '.status.nodeResourceGroup // empty')
[[ ${group,,} = "mc_${CLUSTER_NAME}_${CLUSTER_NAME}_${AZURE_LOCATION,,}" ]] ||
    fail "Unexpected AKS node resource group: $group"
if [[ $identity = workload ]]; then
    client_id=$(mgmt get "userassignedidentities.managedidentity.azure.com/$CLUSTER_NAME" -o json |
        jq -er '.status.clientId // empty')
else
    kubelet=$(mgmt get "managedclusters.containerservice.azure.com/$CLUSTER_NAME" -o json |
        jq -ec '.status.identityProfile.kubeletidentity | select(.clientId and .objectId)') ||
        fail "The AKS cluster has no kubelet identity"
    client_id=$(jq -r .clientId <<< "$kubelet")
    # Give the kubelet identity the role that the template gives the workload
    # identity.
    az role assignment create --assignee-object-id "$(jq -r .objectId <<< "$kubelet")" \
        --assignee-principal-type ServicePrincipal --role Contributor \
        --scope "/subscriptions/$AZURE_SUBSCRIPTION_ID/resourceGroups/$group" -o none
fi
# Only the taint shard's template gives the zero pool the run taint and its
# node-template tag. AZ-P1-003 and the other zero-pool cases need neither.
zero_taint=
if [[ $prepare = taint ]]; then
    zero_taint=$CLUSTER_NAME:NoSchedule
fi
mgmt get managedclustersagentpools.containerservice.azure.com -o json |
    jq -e --arg run "$CLUSTER_NAME" --arg taint "$zero_taint" '
    [.items[] | select(.spec.owner.name == $run)] as $pools |
    ($pools | length) == 3 and all($pools[]; (.status.enableAutoScaling // false) == false) and
    any($pools[]; .spec.azureName == "main" and (.status.nodeTaints // []) == []) and
    any($pools[]; .spec.azureName == "zero" and (.status.nodeTaints // []) ==
        (if $taint == "" then [] else ["autoscaler-e2e-run=" + $taint] end))
' >/dev/null || fail "Expected three AKS agent pools with the AKS autoscaler off and the shard's node taints"

sets=$(az vmss list -g "$group" --subscription "$AZURE_SUBSCRIPTION_ID" -o json)
pool_set() {
    jq -er --arg pool "$1" '[.[] | select(.tags["aks-managed-poolName"] == $pool) | .name] |
        if length == 1 then .[0] else empty end' <<< "$sets"
}
system=$(pool_set pool0) || fail "Expected one AKS VMSS for pool0"
main=$(pool_set main) || fail "Expected one AKS VMSS for main"
zero=$(pool_set zero) || fail "Expected one AKS VMSS for zero"
jq -e --arg run "$CLUSTER_NAME" --arg system "$system" --arg main "$main" --arg zero "$zero" --arg taint "$zero_taint" '
    length == 3 and
    any(.[]; .name == $system and .tags["cluster-autoscaler-name"] == null and .sku.capacity == 1) and
    all(.[] | select(.name != $system); .tags["autoscaler-e2e-run"] == $run and
        .tags["cluster-autoscaler-name"] == $run and .overprovision == false) and
    any(.[]; .name == $main and .tags.min == "1" and .tags.max == "2" and
        .tags["k8s.io_cluster-autoscaler_node-template_label_acceptance-pool"] == "main" and
        .tags["k8s.io_cluster-autoscaler_node-template_taint_autoscaler-e2e-run"] == null and .sku.capacity == 1) and
    any(.[]; .name == $zero and .tags.min == "0" and .tags.max == "1" and
        .tags["k8s.io_cluster-autoscaler_node-template_label_acceptance-pool"] == "zero" and
        (.tags["k8s.io_cluster-autoscaler_node-template_taint_autoscaler-e2e-run"] // "") == $taint and .sku.capacity == 0)
' <<< "$sets" >/dev/null || fail "AKS VMSS bounds, tags or settings differ from the E2E fixture"

# AKS keeps setting up a new cluster after CAPZ reports it Ready. About 12 to
# 35 minutes after it creates the pools, it rolls out new kube-system add-on
# revisions and adds AKSLinuxExtension to each VMSS, which puts the VMSS in
# Updating while it upgrades the instances. Wait for that work to finish, so
# that add-on Pods don't return to the drained workers and the strict VMSS
# checks don't see an AKS upgrade. The checks follow upstream's AllVMSSStable
# and the provisioning state checks in CAPZ's AKS tests.
cluster_id=$(mgmt get "managedclusters.containerservice.azure.com/$CLUSTER_NAME" -o json |
    jq -er '.status.id // empty')
[[ ${cluster_id,,} == "/subscriptions/${AZURE_SUBSCRIPTION_ID,,}/resourcegroups/"*"/providers/microsoft.containerservice/managedclusters/"* ]] ||
    fail "Unexpected AKS cluster ID: $cluster_id"
aks_group=${cluster_id#*/resource[Gg]roups/}
aks_group=${aks_group%%/*}
aks_name=${cluster_id##*/}
aks_settled() {
    az aks show -g "$aks_group" -n "$aks_name" --subscription "$AZURE_SUBSCRIPTION_ID" -o json | jq -e '
        .provisioningState == "Succeeded" and
        (.agentPoolProfiles | length == 3 and all(.[]; .provisioningState == "Succeeded"))
    ' >/dev/null || { echo "The AKS cluster or an agent pool is not Succeeded"; return 1; }
    local current set capacity
    current=$(az vmss list -g "$group" --subscription "$AZURE_SUBSCRIPTION_ID" -o json)
    jq -e 'length == 3 and all(.[]; .provisioningState == "Succeeded" and
        any(.virtualMachineProfile.extensionProfile.extensions[]?; .name == "AKSLinuxExtension"))
    ' <<< "$current" >/dev/null || { echo "A VMSS is not Succeeded or has no AKSLinuxExtension"; return 1; }
    for set in "$system" "$main" "$zero"; do
        az vmss list-instances -g "$group" -n "$set" --subscription "$AZURE_SUBSCRIPTION_ID" -o json |
            jq -e 'all(.[]; .provisioningState == "Succeeded" and .latestModelApplied == true)' >/dev/null ||
            { echo "VMSS $set has instances that are not on its latest model"; return 1; }
    done
    capacity=$(jq '[.[].sku.capacity] | add' <<< "$current")
    kubectl get nodes -o json | jq -e --argjson capacity "$capacity" '
        (.items | length) == $capacity and
        all(.items[]; any(.status.conditions[]; .type == "Ready" and .status == "True"))
    ' >/dev/null || { echo "The Ready Nodes don't match the VMSS capacity"; return 1; }
    kubectl -n kube-system get deployments -o json | jq -e '
        all(.items[]; (.status.observedGeneration // 0) >= .metadata.generation and
            (.status.replicas // 0) == .spec.replicas and
            (.status.updatedReplicas // 0) == .spec.replicas and
            (.status.availableReplicas // 0) == .spec.replicas)
    ' >/dev/null || { echo "A kube-system Deployment is still rolling out"; return 1; }
    kubectl -n kube-system get deployment konnectivity-agent -o json | jq -e '
        .spec.template.metadata.annotations["checksum/service-account-key"] != null and
        (.status.observedGeneration // 0) >= .metadata.generation and
        (.status.updatedReplicas // 0) == .spec.replicas and
        (.status.availableReplicas // 0) == .spec.replicas
    ' >/dev/null || { echo "konnectivity-agent has not rolled out its service account key revision"; return 1; }
}
echo "Waiting for AKS to finish its background setup at $(date -u +%FT%TZ)"
start=$SECONDS
passes=0
reason=
# Require three passing checks in a row, so that the next step of an AKS
# upgrade can't start between two checks.
while true; do
    if reason=$(aks_settled); then
        passes=$((passes + 1))
        (( passes < 3 )) || break
    else
        passes=0
        echo "$(date -u +%T) $reason"
    fi
    (( SECONDS - start < 2700 )) || fail "AKS did not finish its background setup within 45 minutes: $reason"
    sleep 30
done
echo "AKS finished its background setup after $((SECONDS - start)) seconds"

kubectl get pods,deployments,daemonsets,statefulsets -A -o json | jq -e '
    all(.items[]; all((if .kind == "Pod" then .spec.containers else .spec.template.spec.containers end)[];
        (.name | contains("cluster-autoscaler") | not) and
        (.image | contains("cluster-autoscaler") | not)))
' >/dev/null || fail "A cluster autoscaler is already deployed; use a fresh CAPZ cluster"
if kubectl -n kube-system get configmap autoscaler-e2e-authorization >/dev/null 2>&1; then
    fail "An E2E authorization marker already exists"
fi

# AKS's metrics-server-vpa sidecar resizes metrics-server when the Node count
# changes by more than 5%, and the new Pods can land on a drained worker. Keep
# the current resources for any Node count, as AKS documents under "Manually
# configure Metrics Server resource usage", and restart it to load the config.
metrics_request() {
    kubectl -n kube-system get deployment metrics-server -o json | jq -er --arg resource "$1" '
        [.spec.template.spec.containers[] | select(.name == "metrics-server") |
            .resources.requests[$resource] // empty] | if length == 1 then .[0] else empty end'
}
metrics_cpu=$(metrics_request cpu) || fail "metrics-server has no CPU request"
metrics_memory=$(metrics_request memory) || fail "metrics-server has no memory request"
[[ $metrics_cpu =~ ^[0-9]+m?$ && $metrics_memory =~ ^[0-9]+([KMGT]i|[kMGT])?$ ]] ||
    fail "Unexpected metrics-server requests: $metrics_cpu, $metrics_memory"
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: metrics-server-config
  namespace: kube-system
  labels:
    kubernetes.io/cluster-service: "true"
    addonmanager.kubernetes.io/mode: EnsureExists
data:
  NannyConfiguration: |-
    apiVersion: nannyconfig/v1alpha1
    kind: NannyConfiguration
    baseCPU: "$metrics_cpu"
    cpuPerNode: 0m
    baseMemory: "$metrics_memory"
    memoryPerNode: 0Mi
EOF
kubectl -n kube-system rollout restart deployment/metrics-server
kubectl -n kube-system rollout status deployment/metrics-server --timeout=10m
generation=$(kubectl -n kube-system get deployment metrics-server -o jsonpath='{.metadata.generation}')
# The sidecar checks the resources when it starts. Give it time to resize the
# Deployment if it didn't load the config, then check that nothing changed.
sleep 60
metrics=$(kubectl -n kube-system get deployment metrics-server -o json)
jq -e --argjson generation "$generation" --arg cpu "$metrics_cpu" --arg memory "$metrics_memory" '
    .metadata.generation == $generation and .status.observedGeneration == $generation and
    (.status.replicas // 0) == .spec.replicas and (.status.updatedReplicas // 0) == .spec.replicas and
    (.status.availableReplicas // 0) == .spec.replicas and
    all(.spec.template.spec.containers[] | select(.name == "metrics-server");
        .resources.requests.cpu == $cpu and .resources.requests.memory == $memory)
' <<< "$metrics" >/dev/null || fail "metrics-server did not settle after the resource change"
selector=$(jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")' <<< "$metrics")
pods=$(kubectl -n kube-system get pods -l "$selector" -o json |
    jq -c '[.items[] | select(.metadata.deletionTimestamp == null) | .metadata.name]')
jq -e --argjson pods "$pods" '($pods | length) == .spec.replicas' <<< "$metrics" >/dev/null ||
    fail "metrics-server has old or missing Pods after its restart"
for pod in $(jq -r '.[]' <<< "$pods"); do
    log=$(kubectl -n kube-system logs "$pod" -c metrics-server-vpa)
    grep -qF "cpu: $metrics_cpu, extra_cpu: 0m, memory: $metrics_memory, extra_memory: 0Mi" <<< "$log" ||
        fail "metrics-server Pod $pod did not load metrics-server-config"
done

# AKS schedules some add-on replicas on User pools. Move them to the System
# pool, which takes the place of the fixture's control plane.
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

uid=$(kubectl get namespace kube-system -o jsonpath='{.metadata.uid}')
context=$(kubectl config current-context)
[[ -n $uid && -n $context ]] || fail "Workload kubeconfig has no context or cluster UID"

# Prepare only what the selected shard needs. The specs check each marker key
# and fixture again.
allow=(--from-literal=allow-kube-system-fixture=CA-011)
disk_class=
case $prepare in
etag|taint) allow=() ;;
disk)
    kubectl get csidriver disk.csi.azure.com >/dev/null || fail "AKS has no Azure Disk CSI driver"
    kubectl -n kube-system rollout status daemonset/csi-azuredisk-node --timeout=8m
    disk_class=$CLUSTER_NAME-disk
    cat <<EOF | kubectl apply -f -
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: $disk_class
  labels:
    autoscaler-e2e-run: $CLUSTER_NAME
provisioner: disk.csi.azure.com
volumeBindingMode: WaitForFirstConsumer
reclaimPolicy: Delete
parameters:
  skuName: StandardSSD_LRS
  subscriptionID: $AZURE_SUBSCRIPTION_ID
  resourceGroup: $group
  tags: autoscaler-e2e-run=$CLUSTER_NAME
EOF
    allow=(--from-literal=allow-disk-fixture=AZ-P1-006)
    ;;
dra)
    sed "s/@RUN_ID@/$CLUSTER_NAME/g" "$here/dra-driver.yaml.in" | kubectl apply -f -
    kubectl -n kube-system rollout status daemonset/dra-example-driver-kubeletplugin --timeout=6m
    deadline=$((SECONDS + 225))
    until kubectl get resourceslices -o json | jq -e '[.items[] | select(.spec.driver == "gpu.example.com" and
        (.spec.devices | length) == 4)] | length >= 1' >/dev/null; do
        (( SECONDS < deadline )) || fail "The DRA driver did not publish four devices on the main worker"
        sleep 5
    done
    allow=("--from-literal=allow-dra-fixture=CA-020,CA-021,CA-022")
    ;;
esac

kubectl -n kube-system create configmap autoscaler-e2e-authorization \
    --from-literal=run-id="$CLUSTER_NAME" \
    --from-literal=subscription-id="$AZURE_SUBSCRIPTION_ID" \
    --from-literal=resource-group="$group" \
    --from-literal=cluster-uid="$uid" \
    "${allow[@]}"
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

# Match the upstream Helm values: workload identity by default and label discovery.
release=cluster-autoscaler
deployment=$release-azure-cluster-autoscaler
etag=false
if [[ $prepare = etag ]]; then
    etag=true
fi
tmp_dir=$(mktemp -d)
trap 'rm -f "$tmp_dir/values.yaml" "$tmp_dir/ca.yaml"; rmdir "$tmp_dir"' EXIT
if [[ $identity = workload ]]; then
    cat > "$tmp_dir/values.yaml" <<EOF
azureUseWorkloadIdentityExtension: true
podLabels:
  azure.workload.identity/use: "true"
rbac:
  serviceAccount:
    name: $account
    annotations:
      azure.workload.identity/tenant-id: $AZURE_TENANT_ID
      azure.workload.identity/client-id: $client_id
EOF
else
    cat > "$tmp_dir/values.yaml" <<EOF
azureUseManagedIdentityExtension: true
azureUserAssignedIdentityID: $client_id
rbac:
  serviceAccount:
    name: $account
EOF
fi
cat >> "$tmp_dir/values.yaml" <<EOF
autoDiscovery:
  clusterName: $CLUSTER_NAME
azureTenantID: $AZURE_TENANT_ID
azureSubscriptionID: $AZURE_SUBSCRIPTION_ID
azureResourceGroup: $group
azureVMType: vmss
azureEnableForceDelete: false
azureEnableVMSSEtag: $etag
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
helm template "$release" "$repo/charts/cluster-autoscaler" -n "$namespace" \
    -f "$tmp_dir/values.yaml" > "$tmp_dir/ca.yaml"
# The suite requires one discovery tag and literal Azure scope values.
export CLUSTER_NAME AZURE_SUBSCRIPTION_ID
export group
yq eval --inplace '
  (select(.kind == "Deployment") | .spec.template.spec.containers[] |
    select(.name == "azure-cluster-autoscaler") | .command[] |
    select(. == "--node-group-auto-discovery=label:cluster-autoscaler-enabled=true,cluster-autoscaler-name=" + strenv(CLUSTER_NAME))) =
      "--node-group-auto-discovery=label:cluster-autoscaler-name=" + strenv(CLUSTER_NAME) |
  (select(.kind == "Deployment") | .spec.template.spec.containers[] |
    select(.name == "azure-cluster-autoscaler") | .env[] | select(.name == "ARM_SUBSCRIPTION_ID")) =
      {"name":"ARM_SUBSCRIPTION_ID","value":strenv(AZURE_SUBSCRIPTION_ID)} |
  (select(.kind == "Deployment") | .spec.template.spec.containers[] |
    select(.name == "azure-cluster-autoscaler") | .env[] | select(.name == "ARM_RESOURCE_GROUP")) =
      {"name":"ARM_RESOURCE_GROUP","value":strenv(group)}
' "$tmp_dir/ca.yaml"
[[ $(yq eval 'select(.kind == "Deployment") | .spec.template.spec.containers[] |
    select(.name == "azure-cluster-autoscaler") | .command[] |
    select(. == "--node-group-auto-discovery=label:cluster-autoscaler-name=" + strenv(CLUSTER_NAME))' \
    "$tmp_dir/ca.yaml") = "--node-group-auto-discovery=label:cluster-autoscaler-name=$CLUSTER_NAME" ]] ||
    fail "Rendered controller has the wrong discovery selector"
kubectl apply -f "$tmp_dir/ca.yaml"
kubectl -n "$namespace" rollout status deployment/"$deployment" --timeout=5m
# The autoscaler writes Running to its status ConfigMap a few seconds after
# its Pod is Ready, and the suite's first check reads that status.
deadline=$((SECONDS + 300))
until kubectl -n "$namespace" get configmap cluster-autoscaler-status -o jsonpath='{.data.status}' 2>/dev/null |
    yq -e '.autoscalerStatus == "Running"' >/dev/null 2>&1; do
    (( SECONDS < deadline )) || fail "The autoscaler status ConfigMap did not report Running within 5 minutes"
    sleep 5
done

jq -n --arg kubeconfig "$KUBECONFIG" --arg context "$context" --arg uid "$uid" \
    --arg run "$CLUSTER_NAME" --arg sub "$AZURE_SUBSCRIPTION_ID" --arg group "$group" \
    --arg system "$system" --arg main "$main" --arg zero "$zero" \
    --arg location "$AZURE_LOCATION" --arg image "$image" \
    --arg namespace "$namespace" --arg deployment "$deployment" --arg disk "$disk_class" \
    --arg workload "${WORKLOAD_IMAGE:-busybox@sha256:b7f3d86d6e84fc17718c48bcde1450807faa2d56704205c697b4bd5df7b9e29f}" \
    --arg cpu "${DEMAND_CPU:-1200m}" '
    {kubeconfig:$kubeconfig,context:$context,clusterUID:$uid,runID:$run,
     subscriptionID:$sub,resourceGroup:$group,resourceGroupMode:"aks",
     systemPool:$system,location:$location,discoveryValue:$run,
     autoscalerNamespace:$namespace,autoscalerDeployment:$deployment,
     autoscalerContainer:"azure-cluster-autoscaler",leaseName:"cluster-autoscaler",
     expectedImage:$image,mainPool:$main,zeroPool:$zero,
     poolLabel:"acceptance-pool",mainLabel:"main",zeroLabel:"zero",
     demandCPU:$cpu,workloadImage:$workload,diskStorageClass:$disk}' > "$artifacts/environment.json"

make -C "$module" e2etests ENVIRONMENT="$artifacts/environment.json" ARTIFACTS="$artifacts" \
    LABEL_FILTER="$LABEL_FILTER" TEST_SUITE=scaleup TEST_TIMEOUT="${TEST_TIMEOUT:-3h}"
