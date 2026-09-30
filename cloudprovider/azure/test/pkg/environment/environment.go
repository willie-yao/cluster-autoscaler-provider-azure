/*
Copyright 2024 The Kubernetes Authors.

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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Environment reads one prepared cluster and its Azure resources. It does
// not create infrastructure or change the controller.
type Environment struct {
	Config Config
	K8s    client.Client
	Cloud  Cloud

	restConfig *rest.Config
}

// NewEnvironment validates cfg, connects to the kubeconfig that cfg names
// and opens a read-only Azure client. It never falls back to an in-cluster
// or default kubeconfig.
func NewEnvironment(cfg Config) (*Environment, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: cfg.Kubeconfig}, &clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	restConfig.Timeout = 30 * time.Second
	k8s, err := client.New(restConfig, client.Options{})
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	cloud, err := newCloud(cfg)
	if err != nil {
		return nil, err
	}
	return &Environment{Config: cfg, K8s: k8s, Cloud: cloud, restConfig: restConfig}, nil
}

// Controller checks that the autoscaler Deployment has one replica and one
// Ready Pod that runs the expected image, and that its status is fresh,
// Running and lists exactly the main and zero pools.
func (e *Environment) Controller(ctx context.Context) error {
	c := e.Config
	var deployment appsv1.Deployment
	if err := e.K8s.Get(ctx, client.ObjectKey{Namespace: c.AutoscalerNamespace, Name: c.AutoscalerDeployment}, &deployment); err != nil {
		return fmt.Errorf("get autoscaler Deployment: %w", err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
		return fmt.Errorf("autoscaler Deployment must have one replica")
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return fmt.Errorf("parse autoscaler selector: %w", err)
	}
	var pods corev1.PodList
	if err := e.K8s.List(ctx, &pods, client.InNamespace(c.AutoscalerNamespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return fmt.Errorf("list autoscaler Pods: %w", err)
	}
	if len(pods.Items) != 1 || !PodReady(pods.Items[0]) {
		return fmt.Errorf("autoscaler must have one Ready Pod, got %d Pods", len(pods.Items))
	}
	found := false
	for _, container := range pods.Items[0].Spec.Containers {
		if container.Name != c.AutoscalerContainer {
			continue
		}
		found = true
		if container.Image != c.ExpectedImage {
			return fmt.Errorf("autoscaler image %q differs from the expected %q", container.Image, c.ExpectedImage)
		}
	}
	if !found {
		return fmt.Errorf("autoscaler container %s is missing", c.AutoscalerContainer)
	}
	var status corev1.ConfigMap
	if err := e.K8s.Get(ctx, client.ObjectKey{Namespace: c.AutoscalerNamespace, Name: "cluster-autoscaler-status"}, &status); err != nil {
		return fmt.Errorf("get autoscaler status ConfigMap: %w", err)
	}
	return CheckStatus(status.Data["status"], []string{c.MainPool, c.ZeroPool}, time.Now())
}

// Read returns the Azure pools and the Kubernetes Nodes. It returns an
// ErrBounds error when a pool is above its max tag.
func (e *Environment) Read(ctx context.Context) (Snapshot, error) {
	result, err := e.Cloud.Read(ctx)
	if err != nil {
		return result, err
	}
	if err := result.checkMax(); err != nil {
		return result, err
	}
	var nodes corev1.NodeList
	if err := e.K8s.List(ctx, &nodes); err != nil {
		return result, fmt.Errorf("list Nodes: %w", err)
	}
	result.Nodes = nodes.Items
	return result, nil
}

// CheckWorkerIsolation returns an error if a running Pod on a main or zero
// worker is neither a DaemonSet Pod nor a run-labeled Pod in namespace.
func (e *Environment) CheckWorkerIsolation(ctx context.Context, snapshot Snapshot, namespace string) error {
	workers := map[string]bool{}
	for _, node := range WorkerNodes(snapshot.Nodes, e.Config) {
		workers[node.Name] = true
	}
	var pods corev1.PodList
	if err := e.K8s.List(ctx, &pods); err != nil {
		return fmt.Errorf("list Pods: %w", err)
	}
	daemonSet := appsv1.SchemeGroupVersion.WithKind("DaemonSet")
	for _, pod := range pods.Items {
		if !workers[pod.Spec.NodeName] || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if pod.Namespace == namespace && pod.Labels[RunLabel] == e.Config.RunID {
			continue
		}
		if owner := metav1.GetControllerOf(&pod); owner != nil &&
			owner.APIVersion == daemonSet.GroupVersion().String() && owner.Kind == daemonSet.Kind {
			continue
		}
		return fmt.Errorf("non-DaemonSet Pod %s/%s occupies test worker %s", pod.Namespace, pod.Name, pod.Spec.NodeName)
	}
	return nil
}

// Deleted returns nil when exactly count instances of pool in before are
// gone from after, and none of them still has a Node or a NIC.
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
				return fmt.Errorf("check NIC %s: %w", nic, err)
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

// PodReady reports whether pod is Running, not being deleted and has a true
// Ready condition.
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
