/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package environment

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Environment never creates infrastructure or installs/reconfigures the controller.
type Environment struct {
	Config Config
	K8s    client.Client
	Cloud  Cloud
}

// NewEnvironment uses only the named kubeconfig context, without in-cluster fallback.
func NewEnvironment(ctx context.Context, cfg Config) (*Environment, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: cfg.Kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: cfg.Context},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load explicit kubeconfig/context: %w", err)
	}
	restConfig.Timeout = 30 * time.Second
	k8s, err := client.New(restConfig, client.Options{})
	if err != nil {
		return nil, err
	}
	env := &Environment{Config: cfg, K8s: k8s}
	if err := env.Authorize(ctx); err != nil {
		return nil, err
	}
	env.Cloud, err = newCloud(ctx, cfg)
	return env, err
}

// Authorize checks the cluster fingerprint and operator-issued run marker.
func (e *Environment) Authorize(ctx context.Context) error {
	c := e.Config
	var namespace corev1.Namespace
	if err := e.K8s.Get(ctx, client.ObjectKey{Name: "kube-system"}, &namespace); err != nil {
		return fmt.Errorf("read cluster fingerprint: %w", err)
	}
	if string(namespace.UID) != c.ClusterUID {
		return fmt.Errorf("kube-system UID does not match the authorized cluster")
	}
	var marker corev1.ConfigMap
	if err := e.K8s.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: MarkerName}, &marker); err != nil {
		return fmt.Errorf("read operator authorization marker: %w", err)
	}
	for key, expected := range map[string]string{
		"run-id": c.RunID, "subscription-id": c.SubscriptionID,
		"resource-group": c.ResourceGroup, "cluster-uid": c.ClusterUID,
	} {
		if marker.Data[key] != expected {
			return fmt.Errorf("operator marker %s does not match the environment binding", key)
		}
	}
	return nil
}

// Controller checks deployment ownership, protected settings, leadership and discovery.
func (e *Environment) Controller(ctx context.Context) error {
	return e.controller(ctx, e.Config.PoolNames())
}

// ControllerAfterMissing checks the same controller after the empty group is removed.
func (e *Environment) ControllerAfterMissing(ctx context.Context) error {
	if e.Config.Phase != "missing-vmss" {
		return fmt.Errorf("missing VMSS controller check requires its fixture phase")
	}
	return e.controller(ctx, []string{e.Config.MainPool, e.Config.ZeroPool})
}

func (e *Environment) controller(ctx context.Context, groups []string) error {
	c := e.Config
	var deployment appsv1.Deployment
	if err := e.K8s.Get(ctx, client.ObjectKey{Namespace: c.AutoscalerNamespace, Name: c.AutoscalerDeployment}, &deployment); err != nil {
		return err
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 ||
		deployment.Status.ObservedGeneration < deployment.Generation ||
		deployment.Status.ReadyReplicas != 1 || deployment.Status.UpdatedReplicas != 1 {
		return fmt.Errorf("authorized autoscaler Deployment must have exactly one updated Ready replica")
	}
	var found bool
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name != c.AutoscalerContainer {
			continue
		}
		found = true
		if container.Image != c.ExpectedImage {
			return fmt.Errorf("autoscaler image differs from the frozen candidate")
		}
		if err := checkControllerScope(container.Env, c); err != nil {
			return err
		}
		if err := checkPhaseControllerEnv(container.Env, c); err != nil {
			return err
		}
		args := append(append([]string{}, container.Command...), container.Args...)
		if err := checkControllerArguments(args, c.DiscoveryValue, c.Phase == "local-storage" && c.SkipLocalStorage != nil && !*c.SkipLocalStorage, c.DeallocateNodeSpec()...); err != nil {
			return err
		}
		if err := CheckPhaseArguments(args, c); err != nil {
			return err
		}
	}
	if !found {
		return fmt.Errorf("authorized autoscaler container is missing")
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return err
	}
	var pods corev1.PodList
	if err := e.K8s.List(ctx, &pods, client.InNamespace(c.AutoscalerNamespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return err
	}
	if len(pods.Items) != 1 || !PodReady(pods.Items[0]) {
		return fmt.Errorf("autoscaler must have one Ready Pod, without overlapping rollout")
	}
	pod := pods.Items[0]
	found = false
	for _, container := range pod.Spec.Containers {
		if container.Name != c.AutoscalerContainer {
			continue
		}
		found = true
		if container.Image != c.ExpectedImage {
			return fmt.Errorf("running autoscaler image differs from the frozen candidate")
		}
		if err := checkControllerScope(container.Env, c); err != nil {
			return err
		}
		if err := checkPhaseControllerEnv(container.Env, c); err != nil {
			return err
		}
		args := append(append([]string{}, container.Command...), container.Args...)
		if len(c.DeallocateNodeSpec()) != 0 {
			if err := checkControllerArguments(args, c.DiscoveryValue, false, c.DeallocateNodeSpec()...); err != nil {
				return err
			}
		}
		if err := CheckPhaseArguments(args, c); err != nil {
			return err
		}
	}
	if !found {
		return fmt.Errorf("running autoscaler container is missing")
	}
	var lease coordinationv1.Lease
	if err := e.K8s.Get(ctx, client.ObjectKey{Namespace: c.AutoscalerNamespace, Name: c.LeaseName}, &lease); err != nil {
		return err
	}
	if err := checkLeaderLease(pod, lease, time.Now()); err != nil {
		return err
	}
	var status corev1.ConfigMap
	if err := e.K8s.Get(ctx, client.ObjectKey{Namespace: c.AutoscalerNamespace, Name: "cluster-autoscaler-status"}, &status); err != nil {
		return err
	}
	return CheckStatus(status.Data["status"], groups, time.Now())
}

// CheckPhaseArguments requires the flags that make each optional case meaningful.
func CheckPhaseArguments(args []string, c Config) error {
	required := map[string]string{}
	switch c.Phase {
	case "balance":
		required["balance-similar-node-groups"] = "true"
		required["balancing-label"] = c.PoolLabel
		required["max-nodes-total"] = "4"
		required["parallel-scale-up"] = "false"
		required["salvo-scale-up"] = "false"
		required["v"] = "1"
	case "no-join":
		required["max-node-provision-time"] = "3m"
	case "minimum":
		required["enforce-node-group-min-size"] = "true"
		required["v"] = "1"
	case "spot":
		required["max-nodes-total"] = "4"
	case "large":
		required["max-nodes-total"] = "52"
		required["max-node-provision-time"] = "20m"
		required["scan-interval"] = "10s"
	case "cse":
		required["max-nodes-total"] = "4"
		required["max-node-provision-time"] = c.MaxNodeProvisionTime
		required["scan-interval"] = "10s"
		required["v"] = "3"
	case "spot-eviction":
		required["max-nodes-total"] = "4"
	case "missing-vmss":
		required["max-nodes-total"] = "4"
	case "local-storage":
		required["max-nodes-total"] = "4"
		required["skip-nodes-with-local-storage"] = fmt.Sprint(*c.SkipLocalStorage)
	case "deallocate", "deallocate-failed":
		required["max-nodes-total"] = "4"
		required["v"] = "3"
		if c.Phase == "deallocate-failed" {
			required["max-node-provision-time"] = "15m"
		}
	}
	for key, value := range required {
		found := 0
		for _, arg := range args {
			if arg == "--"+key {
				return fmt.Errorf("%s needs an explicit value", key)
			}
			if strings.HasPrefix(arg, "--"+key+"=") {
				found++
				if arg != "--"+key+"="+value {
					return fmt.Errorf("%s must equal %s in the %s phase", key, value, c.Phase)
				}
			}
		}
		if found != 1 {
			return fmt.Errorf("%s must occur once in the %s phase", key, c.Phase)
		}
	}
	return nil
}

func checkPhaseControllerEnv(variables []corev1.EnvVar, c Config) error {
	required := map[string]string{}
	switch c.Phase {
	case "cse":
		required["AZURE_ENABLE_FAST_DELETE_ON_FAILED_PROVISIONING"] = "false"
		required["AZURE_ENABLE_DETAILED_CSE_MESSAGE"] = "false"
	case "spot-eviction":
		required["AZURE_GET_VMSS_SIZE_REFRESH_PERIOD"] = "5"
	}
	for name, expected := range required {
		count := 0
		for _, variable := range variables {
			if variable.Name == name {
				count++
				if variable.ValueFrom != nil || variable.Value != expected {
					return fmt.Errorf("controller %s must be one literal %s", name, expected)
				}
			}
		}
		if count != 1 {
			return fmt.Errorf("controller requires exactly one literal %s", name)
		}
	}
	return nil
}

// CheckNoPendingDemand rejects unscheduled Pods while proving minimum-size growth.
func (e *Environment) CheckNoPendingDemand(ctx context.Context) error {
	var pods corev1.PodList
	if err := e.K8s.List(ctx, &pods); err != nil {
		return err
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodPending && pod.Spec.NodeName == "" && pod.DeletionTimestamp == nil {
			return fmt.Errorf("unscheduled Pod %s/%s could cause demand-based growth", pod.Namespace, pod.Name)
		}
	}
	return nil
}

func checkLeaderLease(pod corev1.Pod, lease coordinationv1.Lease, now time.Time) error {
	if lease.Spec.HolderIdentity == nil || lease.Spec.RenewTime == nil ||
		now.Sub(lease.Spec.RenewTime.Time) > time.Minute || lease.Spec.RenewTime.Time.After(now.Add(10*time.Second)) {
		return fmt.Errorf("autoscaler leader lease is missing or stale")
	}
	holder := *lease.Spec.HolderIdentity
	// The pinned autoscaler uses os.Hostname() without adding a UUID.
	if holder != pod.Name && !(pod.Spec.HostNetwork && pod.Spec.NodeName != "" && holder == pod.Spec.NodeName) {
		return fmt.Errorf("fresh leader lease does not belong to the authorized autoscaler Pod")
	}
	return nil
}

// These non-secret environment values override the provider's cloud-config file.
func checkControllerScope(variables []corev1.EnvVar, c Config) error {
	for name, expected := range map[string]string{
		"ARM_SUBSCRIPTION_ID": c.SubscriptionID,
		"ARM_RESOURCE_GROUP":  c.ResourceGroup,
	} {
		count := 0
		for _, variable := range variables {
			if variable.Name != name {
				continue
			}
			count++
			if variable.ValueFrom != nil || variable.Value != expected {
				return fmt.Errorf("controller %s must be a literal matching the authorized scope", name)
			}
		}
		if count != 1 {
			return fmt.Errorf("controller requires exactly one literal %s", name)
		}
	}
	return nil
}

// CheckControllerArguments rejects alternate discovery and weakened scale-down protections.
func CheckControllerArguments(args []string, discoveryValue string) error {
	return checkControllerArguments(args, discoveryValue, false)
}

func checkControllerArguments(args []string, discoveryValue string, allowLocalStorageFalse bool, deallocateSpec ...string) error {
	var discovery int
	var explicit int
	timings := map[string]bool{
		"scale-down-delay-after-add":       false,
		"scale-down-unneeded-time":         false,
		"unremovable-node-recheck-timeout": false,
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--nodes") {
			if len(deallocateSpec) == 0 || arg != "--nodes="+deallocateSpec[0] {
				return fmt.Errorf("unexpected explicit node group in this fixture")
			}
			explicit++
		}
		if strings.HasPrefix(arg, "--node-group-auto-discovery") {
			if arg != "--node-group-auto-discovery=label:cluster-autoscaler-name="+discoveryValue {
				return fmt.Errorf("unexpected autoscaler discovery argument")
			}
			discovery++
		}
		for _, key := range []string{"skip-nodes-with-system-pods", "skip-nodes-with-local-storage", "leader-elect", "scale-down-enabled"} {
			if key == "skip-nodes-with-local-storage" && allowLocalStorageFalse && arg == "--skip-nodes-with-local-storage=false" {
				continue
			}
			if (arg == "--"+key || strings.HasPrefix(arg, "--"+key+"=")) && arg != "--"+key+"=true" {
				return fmt.Errorf("%s must not be disabled", key)
			}
		}
		for key := range timings {
			if arg == "--"+key {
				return fmt.Errorf("%s requires --%s=value for explicit fixture validation", key, key)
			}
			if strings.HasPrefix(arg, "--"+key+"=") {
				duration, err := time.ParseDuration(strings.TrimPrefix(arg, "--"+key+"="))
				if err != nil || duration < 0 || duration > time.Minute || timings[key] {
					return fmt.Errorf("%s must occur once with a duration between zero and one minute", key)
				}
				timings[key] = true
			}
		}
		if (arg == "--scale-down-utilization-threshold" || strings.HasPrefix(arg, "--scale-down-utilization-threshold=")) &&
			arg != "--scale-down-utilization-threshold=0.5" {
			return fmt.Errorf("this fixture requires the default scale-down utilization threshold")
		}
	}
	if discovery != 1 {
		return fmt.Errorf("expected exactly one authorized discovery argument")
	}
	if len(deallocateSpec) != explicit {
		return fmt.Errorf("deallocate phase requires exactly one explicit Deallocate node group")
	}
	for key, explicit := range timings {
		if !explicit {
			return fmt.Errorf("%s must be explicit for bounded negative observations", key)
		}
	}
	return nil
}

// Read joins Azure and Kubernetes observations without emitting raw API objects.
func (e *Environment) Read(ctx context.Context) (Snapshot, error) {
	return e.read(ctx, e.Cloud.Read)
}

// ReadAfterMissing checks surviving pools after the authorized empty VMSS is removed.
func (e *Environment) ReadAfterMissing(ctx context.Context) (Snapshot, error) {
	cloud, ok := e.Cloud.(*azureCloud)
	if !ok || e.Config.Phase != "missing-vmss" {
		return Snapshot{}, fmt.Errorf("surviving-pool read requires the authorized Azure missing-vmss phase")
	}
	return e.read(ctx, cloud.readAfterMissing)
}

func (e *Environment) read(ctx context.Context, observe func(context.Context) (Snapshot, error)) (Snapshot, error) {
	result, err := observe(ctx)
	if err != nil {
		return result, err
	}
	if err := result.CheckBounds(e.Config); err != nil {
		return result, err
	}
	var nodes corev1.NodeList
	if err := e.K8s.List(ctx, &nodes); err != nil {
		return result, err
	}
	result.Nodes = nodes.Items
	var controlPlanes int
	for _, node := range nodes.Items {
		if observeMissing, ok := e.Cloud.(*azureCloud); ok && observeMissing.config.Phase == "missing-vmss" {
			if _, exists := result.Pools[e.Config.MissingPool]; !exists &&
				len(PoolNodes([]corev1.Node{node}, e.Config.PoolID(e.Config.MissingPool))) != 0 {
				return result, fmt.Errorf("deleted VMSS still has a Kubernetes Node")
			}
		}
		if normalizeID(node.Spec.ProviderID) == normalizeID(e.Config.ControlPlaneID) {
			controlPlanes++
			if !Ready(node) {
				return result, fmt.Errorf("authorized control plane is not Ready")
			}
			continue
		}
		owned := 0
		for _, name := range e.Config.PoolNames() {
			owned += len(PoolNodes([]corev1.Node{node}, e.Config.PoolID(name)))
		}
		if owned != 1 {
			return result, fmt.Errorf("Node %s belongs to an unexpected cloud resource", node.Name)
		}
	}
	if controlPlanes != 1 {
		return result, fmt.Errorf("expected exactly one authorized control-plane Node")
	}
	return result, nil
}

// CheckWorkerIsolation rejects unrelated non-DaemonSet Pods on either worker pool.
func (e *Environment) CheckWorkerIsolation(ctx context.Context, snapshot Snapshot, namespace string) error {
	workers := map[string]bool{}
	for _, pool := range e.Config.PoolNames() {
		for _, node := range PoolNodes(snapshot.Nodes, e.Config.PoolID(pool)) {
			workers[node.Name] = true
		}
	}
	var pods corev1.PodList
	if err := e.K8s.List(ctx, &pods); err != nil {
		return err
	}
	for _, pod := range pods.Items {
		if !workers[pod.Spec.NodeName] || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if pod.Namespace == namespace && pod.Labels[RunLabel] == e.Config.RunID {
			continue
		}
		daemon := false
		for _, owner := range pod.OwnerReferences {
			daemon = daemon || owner.Kind == "DaemonSet"
		}
		if !daemon {
			return fmt.Errorf("unrelated non-DaemonSet Pod %s/%s occupies test worker %s", pod.Namespace, pod.Name, pod.Spec.NodeName)
		}
	}
	return nil
}

// Deleted verifies disappearance of captured instances, their Nodes and their NICs.
func (e *Environment) Deleted(ctx context.Context, before, after Snapshot, pool string, count int) error {
	var deleted int
	for id, instance := range before.Pools[pool].Instances {
		if _, exists := after.Pools[pool].Instances[id]; exists {
			continue
		}
		deleted++
		for _, node := range after.Nodes {
			if normalizeID(node.Spec.ProviderID) == id {
				return fmt.Errorf("deleted instance still has Node %s", node.Name)
			}
		}
		for _, nic := range instance.NICs {
			exists, err := e.Cloud.NICExists(ctx, nic)
			if err != nil {
				return err
			}
			if exists {
				return fmt.Errorf("deleted instance still has NIC %s", nic)
			}
		}
	}
	if deleted != count {
		return fmt.Errorf("observed %d captured instance deletions, want %d", deleted, count)
	}
	return nil
}

// DeletedGeneration checks a VM separately from a replacement using the same VMSS instance ID.
func (e *Environment) DeletedGeneration(ctx context.Context, before Instance, after Snapshot, pool string) error {
	return e.deletedGeneration(ctx, before, "", after, pool)
}

// DeletedGenerationForNode accepts a replacement Node at the same VMSS instance path.
func (e *Environment) DeletedGenerationForNode(ctx context.Context, before Instance, oldNodeUID types.UID, after Snapshot, pool string) error {
	if oldNodeUID == "" {
		return fmt.Errorf("captured Node UID is required for a reused VMSS instance path")
	}
	return e.deletedGeneration(ctx, before, oldNodeUID, after, pool)
}

func (e *Environment) deletedGeneration(ctx context.Context, before Instance, oldNodeUID types.UID, after Snapshot, pool string) error {
	if before.ID == "" || before.VMID == "" {
		return fmt.Errorf("captured VM has no unique Azure identity")
	}
	state, ok := after.Pools[pool]
	if !ok {
		return fmt.Errorf("pool %s is missing from the deletion observation", pool)
	}
	current, replacement := state.Instances[before.ID]
	if replacement && current.VMID == before.VMID {
		return fmt.Errorf("captured VM generation still exists in pool %s", pool)
	}
	for _, node := range after.Nodes {
		if oldNodeUID == "" && normalizeID(node.Spec.ProviderID) == before.ID ||
			oldNodeUID != "" && node.UID == oldNodeUID {
			return fmt.Errorf("deleted VM generation still has Node %s", node.Name)
		}
	}
	for _, nic := range before.NICs {
		reused := false
		if replacement {
			for _, currentNIC := range current.NICs {
				reused = reused || strings.EqualFold(nic, currentNIC)
			}
		}
		if reused {
			continue
		}
		exists, err := e.Cloud.NICExists(ctx, nic)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("deleted VM generation still has NIC %s", nic)
		}
	}
	return nil
}

// PodReady excludes terminating Pods and requires the explicit Ready condition.
func PodReady(pod corev1.Pod) bool {
	if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
