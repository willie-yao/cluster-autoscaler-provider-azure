# cluster-autoscaler

Scales Kubernetes worker nodes within Azure autoscaling groups.

## TL;DR

```console
# Method 1 - Using Autodiscovery
$ helm install my-release ./charts/cluster-autoscaler \
    --set image.repository=<YOUR REGISTRY>/cluster-autoscaler \
    --set image.tag=<YOUR TAG> \
    --set 'autoDiscovery.clusterName'=<CLUSTER NAME>

# Method 2 - Specifying groups manually
$ helm install my-release ./charts/cluster-autoscaler \
    --set image.repository=<YOUR REGISTRY>/cluster-autoscaler \
    --set image.tag=<YOUR TAG> \
    --set "autoscalingGroups[0].name=your-vmss-name" \
    --set "autoscalingGroups[0].maxSize=10" \
    --set "autoscalingGroups[0].minSize=1"
```

## Introduction

This chart bootstraps a cluster-autoscaler deployment on a [Kubernetes](http://kubernetes.io) cluster using the [Helm](https://helm.sh) package manager.

The chart is based on version 9.59.0 of the upstream `cluster-autoscaler` chart, and it supports only Azure. The `cloudProvider` value and the values for other cloud providers are removed.

## Prerequisites

- Helm 3+
- Kubernetes 1.10+ with RBAC enabled
- A cluster autoscaler image built from this repository. The default `image.repository` is a placeholder, so you must set `image.repository` and `image.tag`.

## Installing the Chart

**By default, no deployment is created and nothing will autoscale**.

You must provide some minimal configuration, either to specify instance groups or enable auto-discovery. It is not recommended to do both.

Either:

- Set `autoDiscovery.clusterName` **or**
- Set static node group configurations for one or more node groups using `autoscalingGroups`.

To create a valid configuration, follow the [Azure](#azure) instructions.

### Templating the autoDiscovery.clusterName

The cluster name can be templated in the `autoDiscovery.clusterName` variable. This is useful when the cluster name is dynamically generated based on other values coming from external systems like Argo CD or Flux. This also allows you to use global Helm values to set the cluster name, e.g., `autoDiscovery.clusterName={{ .Values.global.clusterName }}`, so that you don't need to set it in more than 1 location in the values file.

### Azure

The following parameters are required:

- `image.repository` and `image.tag`
- `azureSubscriptionID: "your-azure-subscription-id"`
- `azureResourceGroup: "your-vmss-resource-group-name"`
- `azureVMType: "vmss"`
- Either `autoDiscovery.clusterName` or `autoscalingGroups[0].name=your-vmss,autoscalingGroups[0].maxSize=10,autoscalingGroups[0].minSize=1`

The resource group must be the one that contains the VMSS. For AKS, this is the node resource group, not the cluster resource group.

For service principal authentication, also set:

- `azureClientID: "your-service-principal-app-id"`
- `azureClientSecret: "your-service-principal-client-secret"`
- `azureTenantID: "your-azure-tenant-id"`

To use a managed identity instead, set `azureUseManagedIdentityExtension=true`. Set `azureUserAssignedIdentityID` when the VMSS has more than one user assigned identity. To use workload identity, see [Azure - Using azure workload identity](#azure---using-azure-workload-identity).

With `autoDiscovery.clusterName`, the autoscaler manages each VMSS that has the tags `cluster-autoscaler-enabled=true` and `cluster-autoscaler-name=<CLUSTER NAME>`. The `min` and `max` tags on each VMSS set its size limits. See the [Azure provider README](../../pkg/cloudprovider/azure/README.md#auto-discovery-setup) for more details.

By default, the chart creates a Secret from the values above. To use an existing Secret instead, set `secretKeyRefNameOverride` to its name. The Secret must have the keys `SubscriptionID`, `ResourceGroup` and `VMType`. It must also have `UserAssignedIdentityID` for a managed identity, or `TenantID`, `ClientID` and `ClientSecret` for service principal authentication.

## Uninstalling the Chart

To uninstall `my-release`:

```console
$ helm uninstall my-release
```

The command removes all the Kubernetes components associated with the chart and deletes the release.

> **Tip**: List all releases using `helm list` or start clean with `helm uninstall my-release`

## Additional Configuration

### Azure - Using azure workload identity

You can use the project [Azure workload identity](https://github.com/Azure/azure-workload-identity), to automatically configure the correct setup for your pods to use federated identity with Azure.

You can also set the correct settings yourself instead of relying on this project.

For example the following configuration will configure the Autoscaler to use your federated identity:

```yaml
azureUseWorkloadIdentityExtension: true
extraEnv:
  AZURE_CLIENT_ID: USER ASSIGNED IDENTITY CLIENT ID
  AZURE_TENANT_ID: USER ASSIGNED IDENTITY TENANT ID
  AZURE_FEDERATED_TOKEN_FILE: /var/run/secrets/tokens/azure-identity-token
  AZURE_AUTHORITY_HOST: https://login.microsoftonline.com/
extraVolumes:
- name: azure-identity-token
  projected:
    defaultMode: 420
    sources:
    - serviceAccountToken:
        audience: api://AzureADTokenExchange
        expirationSeconds: 3600
        path: azure-identity-token
extraVolumeMounts:
- mountPath: /var/run/secrets/tokens
  name: azure-identity-token
  readOnly: true
```

### Custom arguments

You can use the `customArgs` value to give any argument to cluster autoscaler command.

Typical use case is to give an environment variable as an argument which will be interpolated at execution time.

This is helpful when you need to inject values from configmap or secret.

## Troubleshooting

The chart will succeed even if the container arguments are incorrect. A few minutes after starting `kubectl logs -l "app.kubernetes.io/name=azure-cluster-autoscaler" --tail=50` should loop through something like

```
polling_autoscaler.go:111] Poll finished
static_autoscaler.go:97] Starting main loop
utils.go:435] No pod using affinity / antiaffinity found in cluster, disabling affinity predicate for this loop
static_autoscaler.go:230] Filtering out schedulables
```

If not, find a pod that the deployment created and `describe` it, paying close attention to the arguments under `Command`. e.g.:

```
Containers:
  cluster-autoscaler:
    Command:
      ./cluster-autoscaler
      --cloud-provider=azure
# if specifying VMSS groups manually
      --nodes=1:10:your-vmss-name
# if using autodiscovery
      --node-group-auto-discovery=label:cluster-autoscaler-enabled=true,cluster-autoscaler-name=<ClusterName>
      --v=4
```

### PodSecurityPolicy

Though enough for the majority of installations, the default PodSecurityPolicy _could_ be too restrictive depending on the specifics of your release. Please make sure to check that the template fits with any customizations made or disable it by setting `rbac.pspEnabled` to `false`.

### VerticalPodAutoscaler

The CA Helm Chart can install a [`VerticalPodAutoscaler`](https://github.com/kubernetes/autoscaler/blob/master/vertical-pod-autoscaler/README.md) object from Chart version `9.27.0`
onwards for the Cluster Autoscaler Deployment to scale the CA as appropriate, but for that, we
need to install the VPA to the cluster separately. A VPA can help minimize wasted resources
when usage spikes periodically or remediate containers that are being OOMKilled.

The following example snippet can be used to install VPA that allows scaling down from the default recommendations of the deployment template:

```yaml
vpa:
  enabled: true
  containerPolicy:
    minAllowed:
      cpu: 20m
      memory: 50Mi
```

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
