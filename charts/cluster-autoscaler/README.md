# cluster-autoscaler

Scales Kubernetes worker nodes within Azure autoscaling groups.

The chart installs the Azure provider from this repository. It is based on upstream chart version 9.59.0, with other cloud providers removed.

The project is pre-release. There is no official image yet, and the default image is a placeholder. AKS compatibility work and deallocate mode are not included. See the [project status](../../README.md#status).

## Prerequisites

Use Helm 3 or later and a Kubernetes cluster with RBAC enabled. You also need an image built from this repository and an Azure identity with permission to manage the worker pools. The Kubernetes 1.37.0 library pin does not establish cluster version support.

## Install

Run commands from the repository root. The install and uninstall commands need access to your Azure Kubernetes cluster.

No Deployment is created unless you set `autoDiscovery.clusterName` or `autoscalingGroups`. Choose one method.

Create an `azure-values.yaml` file with your own settings. For example, when the controller can use a managed identity assigned to its host VMSS:

```yaml
azureSubscriptionID: YOUR_SUBSCRIPTION_ID
azureResourceGroup: YOUR_WORKER_RESOURCE_GROUP
azureVMType: vmss
azureUseManagedIdentityExtension: true
azureUserAssignedIdentityID: YOUR_IDENTITY_CLIENT_ID
autoDiscovery:
  clusterName: YOUR_CLUSTER_NAME
```

The resource group must contain the worker VMSS. On AKS, it is the node resource group, not the cluster resource group. Assign the identity and its permissions before installing.

For discovery, tag each worker VMSS with `cluster-autoscaler-enabled=true`, `cluster-autoscaler-name=YOUR_CLUSTER_NAME` and its `min` and `max` limits. See [provider discovery](../../pkg/cloudprovider/azure/README.md#auto-discovery-setup).

For explicit groups, replace `autoDiscovery` with:

```yaml
autoscalingGroups:
  - name: YOUR_VMSS_NAME
    minSize: 1
    maxSize: 10
```

Replace the image placeholders, then render and review the resources:

```sh
helm template cluster-autoscaler charts/cluster-autoscaler \
  --namespace kube-system \
  -f azure-values.yaml \
  --set-string image.repository=YOUR_REGISTRY/cluster-autoscaler-azure \
  --set-string image.tag=YOUR_TAG
```

Install after review:

```sh
helm upgrade --install cluster-autoscaler charts/cluster-autoscaler \
  --namespace kube-system \
  -f azure-values.yaml \
  --set-string image.repository=YOUR_REGISTRY/cluster-autoscaler-azure \
  --set-string image.tag=YOUR_TAG \
  --wait
```

## Authentication

Choose one authentication method. For a service principal, set `azureTenantID`, `azureClientID` and `azureClientSecret` instead of the managed identity settings. Keep credentials out of commits.

The chart creates a Secret by default. To use an existing Secret, set `secretKeyRefNameOverride`. It must contain `SubscriptionID`, `ResourceGroup` and `VMType`. Managed identity also needs the `UserAssignedIdentityID` key, which can be empty for a system-assigned identity. Service principal authentication needs `TenantID`, `ClientID` and `ClientSecret`.

### Workload identity

With the [Azure workload identity](https://azure.github.io/azure-workload-identity/docs/) webhook installed, set:

```yaml
azureUseWorkloadIdentityExtension: true
podLabels:
  azure.workload.identity/use: "true"
rbac:
  serviceAccount:
    name: cluster-autoscaler
    annotations:
      azure.workload.identity/client-id: YOUR_IDENTITY_CLIENT_ID
      azure.workload.identity/tenant-id: YOUR_TENANT_ID
```

Create a federated credential for that service account in the release namespace. The identity must have permission to manage the workers. Do not also enable managed identity authentication.

## Other settings

Use `extraArgs` for autoscaler flags and `customArgs` for complete argument strings. See [values.yaml](values.yaml) for resources, scheduling and other settings.

Set `vpa.enabled` to create a VerticalPodAutoscaler for the controller. Install the VPA controller and CRDs separately.

`rbac.pspEnabled` is a legacy option for clusters that still have PodSecurityPolicy. Leave it disabled on current Kubernetes versions.

## Troubleshooting

With access to the cluster, check the controller logs:

```sh
kubectl logs --namespace kube-system \
  -l app.kubernetes.io/instance=cluster-autoscaler \
  --tail=50
```

Check the Deployment's image, arguments, identity and worker group settings. A successful chart render does not prove that the controller can authenticate or manage the workers.

## Uninstall

With access to the cluster:

```sh
helm uninstall cluster-autoscaler --namespace kube-system
```

The command removes the chart resources, not the Azure worker pools.

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| additionalLabels | object | `{}` | Labels to add to each object of the chart. |
| affinity | object | `{}` | Affinity for pod assignment |
| autoDiscovery.clusterName | string | `nil` | Enable autodiscovery of Azure VMSS tagged with `cluster-autoscaler-enabled=true` and `cluster-autoscaler-name=<clusterName>`. |
| autoscalingGroups | list | `[]` | For Azure AKS. At least one element is required if not using `autoDiscovery`. For example: <pre> - name: asg1<br />   maxSize: 2<br />   minSize: 1 </pre> |
| azureClientID | string | `""` | Service Principal ClientID with contributor permission to Cluster and Node ResourceGroup. Required for Azure service-principal authentication. |
| azureClientSecret | string | `""` | Service Principal ClientSecret with contributor permission to Cluster and Node ResourceGroup. Required for Azure service-principal authentication. |
| azureEnableForceDelete | bool | `false` | Whether to force delete VMs or VMSS instances when scaling down. |
| azureEnableVMSSEtag | bool | `false` | Whether to send the cached VMSS ETag as an `If-Match` header on capacity updates, so concurrent modifications are rejected (HTTP 412) and retried instead of silently overwritten. |
| azureResourceGroup | string | `""` | Azure resource group that contains the worker node VMSS or VMs. For AKS, this is the node resource group. Required for all Azure authentication methods. |
| azureSubscriptionID | string | `""` | Azure subscription where the resources are located. Required for all Azure authentication methods. When empty, the autoscaler tries to read it from the Azure Instance Metadata Service. |
| azureTenantID | string | `""` | Azure tenant where the resources are located. Required for Azure service-principal authentication. |
| azureUseManagedIdentityExtension | bool | `false` | Whether to use Azure's managed identity extension for credentials. If using MSI, ensure subscription ID, resource group, and azure AKS cluster name are set. You can only use one authentication method at a time, either azureUseWorkloadIdentityExtension or azureUseManagedIdentityExtension should be set. |
| azureUseWorkloadIdentityExtension | bool | `false` | Whether to use Azure's workload identity extension for credentials. See the project here: https://github.com/Azure/azure-workload-identity for more details. You can only use one authentication method at a time, either azureUseWorkloadIdentityExtension or azureUseManagedIdentityExtension should be set. |
| azureUserAssignedIdentityID | string | `""` | When vmss has multiple user assigned identity assigned, azureUserAssignedIdentityID specifies which identity to be used |
| azureVMType | string | `"vmss"` | Azure VM type. |
| containerSecurityContext | object | `{}` | [Security context for container](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/) |
| customArgs | list | `[]` | Additional custom container arguments. Refer to https://github.com/kubernetes-sigs/cluster-autoscaler/blob/v0.0.0-k8s.v1.37.0/pkg/FAQ.md#what-are-the-parameters-to-ca for the full list of cluster autoscaler parameters and their default values. List of arguments as strings. |
| deployment.annotations | object | `{}` | Annotations to add to the Deployment object. |
| deployment.selector | object | `{}` | Labels for Deployment `spec.selector.matchLabels`. |
| dnsConfig | object | `{}` | [Pod's DNS Config](https://kubernetes.io/docs/concepts/services-networking/dns-pod-service/#pod-dns-config) |
| dnsPolicy | string | `"ClusterFirst"` | Defaults to `ClusterFirst`. Valid values are: `ClusterFirstWithHostNet`, `ClusterFirst`, `Default` or `None`. If autoscaler does not depend on cluster DNS, recommended to set this to `Default`. |
| envFromConfigMap | string | `""` | ConfigMap name to use as envFrom. |
| envFromSecret | string | `""` | Secret name to use as envFrom. |
| expanderPriorities | object | `{}` | The expanderPriorities is used if `extraArgs.expander` contains `priority` and expanderPriorities is also set with the priorities. If `extraArgs.expander` contains `priority`, then expanderPriorities is used to define cluster-autoscaler-priority-expander priorities. See: https://github.com/kubernetes-sigs/cluster-autoscaler/blob/v0.0.0-k8s.v1.37.0/pkg/expander/priority/readme.md |
| extraArgs | object | `{"logtostderr":true,"stderrthreshold":"info","v":4}` | Additional container arguments. Refer to https://github.com/kubernetes-sigs/cluster-autoscaler/blob/v0.0.0-k8s.v1.37.0/pkg/FAQ.md#what-are-the-parameters-to-ca for the full list of cluster autoscaler parameters and their default values. Everything after the first _ will be ignored allowing the use of multi-string arguments. |
| extraEnv | object | `{}` | Additional container environment variables. |
| extraEnvConfigMaps | object | `{}` | Additional container environment variables from ConfigMaps. |
| extraEnvSecrets | object | `{}` | Additional container environment variables from Secrets. |
| extraObjects | list | `[]` | Extra K8s manifests to deploy |
| extraVolumeMounts | list | `[]` | Additional volumes to mount. |
| extraVolumeSecrets | object | `{}` | Additional volumes to mount from Secrets. |
| extraVolumes | list | `[]` | Additional volumes. |
| fullnameOverride | string | `""` | String to fully override `cluster-autoscaler.fullname` template. |
| hostNetwork | bool | `false` | Whether to expose network interfaces of the host machine to pods. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy |
| image.pullSecrets | list | `[]` | Image pull secrets |
| image.repository | string | `"REPLACE_WITH_YOUR_REGISTRY/cluster-autoscaler"` | Image repository |
| image.tag | string | `"v1.35.0"` | Image tag |
| initContainers | list | `[]` | Any additional init containers. |
| kubeTargetVersionOverride | string | `""` | Allow overriding the `.Capabilities.KubeVersion.GitVersion` check. Useful for `helm template` commands. |
| nameOverride | string | `""` | String to partially override `cluster-autoscaler.fullname` template (will maintain the release name) |
| nodeSelector | object | `{}` | Node labels for pod assignment. Ref: https://kubernetes.io/docs/user-guide/node-selection/. |
| podAnnotations | object | `{}` | Annotations to add to each pod. |
| podDisruptionBudget | object | `{"annotations":{},"maxUnavailable":1,"selector":{}}` | Pod disruption budget. |
| podDisruptionBudget.annotations | object | `{}` | Annotations to add to the PodDisruptionBudget. |
| podDisruptionBudget.selector | object | `{}` | Override labels for PodDisruptionBudget `spec.selector.matchLabels`. |
| podLabels | object | `{}` | Labels to add to each pod. |
| priorityClassName | string | `"system-cluster-critical"` | priorityClassName |
| priorityConfigMapAnnotations | object | `{}` | Annotations to add to `cluster-autoscaler-priority-expander` ConfigMap. |
| prometheusRule.additionalLabels | object | `{}` | Additional labels to be set in metadata. |
| prometheusRule.enabled | bool | `false` | If true, creates a Prometheus Operator PrometheusRule. |
| prometheusRule.interval | string | `nil` | How often rules in the group are evaluated (falls back to `global.evaluation_interval` if not set). |
| prometheusRule.namespace | string | `"monitoring"` | Namespace which Prometheus is running in. |
| prometheusRule.rules | list | `[]` | Rules spec template (see https://github.com/prometheus-operator/prometheus-operator/blob/master/Documentation/api.md#rule). |
| rbac.additionalRules | list | `[]` | Additional rules for role/clusterrole |
| rbac.annotations | object | `{}` | Additional annotations to add to RBAC resources (Role/RoleBinding/ClusterRole/ClusterRoleBinding). |
| rbac.clusterScoped | bool | `true` | If set to false, provision RBAC only for the current namespace. |
| rbac.create | bool | `true` | If `true`, create and use RBAC resources. |
| rbac.pspEnabled | bool | `false` | If `true`, creates and uses RBAC resources required in the cluster with [Pod Security Policies](https://kubernetes.io/docs/concepts/policy/pod-security-policy/) enabled. Must be used with `rbac.create` set to `true`. |
| rbac.serviceAccount.annotations | object | `{}` | Additional Service Account annotations. |
| rbac.serviceAccount.automountServiceAccountToken | bool | `true` | Automount API credentials for a Service Account. |
| rbac.serviceAccount.create | bool | `true` | If `true` and `rbac.create` is also true, a Service Account will be created. |
| rbac.serviceAccount.name | string | `""` | The name of the ServiceAccount to use. If not set and create is `true`, a name is generated using the fullname template. |
| replicaCount | int | `1` | Desired number of pods |
| resources | object | `{}` | Pod resource requests and limits. |
| revisionHistoryLimit | int | `10` | The number of revisions to keep. |
| secretKeyRefNameOverride | string | `""` | Overrides the name of the Secret to use for Azure credentials. |
| securityContext | object | `{}` | [Security context for pod](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/) |
| service.annotations | object | `{}` | Annotations to add to service |
| service.clusterIP | string | `""` | IP address to assign to service |
| service.create | bool | `true` | If `true`, a Service will be created. |
| service.externalIPs | list | `[]` | List of IP addresses at which the service is available. Ref: https://kubernetes.io/docs/concepts/services-networking/service/#external-ips. |
| service.labels | object | `{}` | Labels to add to service |
| service.loadBalancerIP | string | `""` | IP address to assign to load balancer (if supported). |
| service.loadBalancerSourceRanges | list | `[]` | List of IP CIDRs allowed access to load balancer (if supported). |
| service.portName | string | `"http"` | Name for service port. |
| service.selector | object | `{}` | Override labels for Service `spec.selector`. |
| service.servicePort | int | `8085` | Service port to expose. |
| service.type | string | `"ClusterIP"` | Type of service to create. |
| serviceMonitor.annotations | object | `{}` | Annotations to add to service monitor |
| serviceMonitor.enabled | bool | `false` | If true, creates a Prometheus Operator ServiceMonitor. |
| serviceMonitor.interval | string | `"10s"` | Interval that Prometheus scrapes Cluster Autoscaler metrics. |
| serviceMonitor.metricRelabelings | object | `{}` | MetricRelabelConfigs to apply to samples before ingestion. |
| serviceMonitor.namespace | string | `"monitoring"` | Namespace which Prometheus is running in. |
| serviceMonitor.path | string | `"/metrics"` | The path to scrape for metrics; autoscaler exposes `/metrics` (this is standard) |
| serviceMonitor.relabelings | object | `{}` | RelabelConfigs to apply to metrics before scraping. |
| serviceMonitor.selector | object | `{"release":"prometheus-operator"}` | Default to kube-prometheus install (CoreOS recommended), but should be set according to Prometheus install. |
| tolerations | list | `[]` | List of node taints to tolerate (requires Kubernetes >= 1.6). |
| topologySpreadConstraints | list | `[]` | You can use topology spread constraints to control how Pods are spread across your cluster among failure-domains such as regions, zones, nodes, and other user-defined topology domains. (requires Kubernetes >= 1.19). |
| updateStrategy | object | `{}` | [Deployment update strategy](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/#strategy) |
| vpa | object | `{"containerPolicy":{},"enabled":false,"recommender":"default","updateMode":"Auto"}` | Configure a VerticalPodAutoscaler for the cluster-autoscaler Deployment. |
| vpa.containerPolicy | object | `{}` | [ContainerResourcePolicy](https://github.com/kubernetes/autoscaler/blob/vertical-pod-autoscaler/v0.13.0/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1/types.go#L159). The containerName is always set to the deployment's container name. This value is required if VPA is enabled. |
| vpa.enabled | bool | `false` | If true, creates a VerticalPodAutoscaler. |
| vpa.recommender | string | `"default"` | Name of the VPA recommender that will provide recommendations for vertical scaling. |
| vpa.updateMode | string | `"Auto"` | [UpdateMode](https://github.com/kubernetes/autoscaler/blob/vertical-pod-autoscaler/v0.13.0/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1/types.go#L124) |
