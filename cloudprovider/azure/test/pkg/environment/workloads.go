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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Deployment pins workloads to an eligible pool without tolerating control-plane taints.
func (e *Environment) Deployment(namespace, name, poolLabel, cpu string, replicas int32) *appsv1.Deployment {
	labels := map[string]string{RunLabel: e.Config.RunID, "app": name}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptr.To(false),
					NodeSelector:                 map[string]string{e.Config.PoolLabel: poolLabel, "kubernetes.io/os": "linux"},
					Containers: []corev1.Container{{
						Name: "work", Image: e.Config.WorkloadImage,
						Command: []string{"sh", "-c", "while true; do sleep 3600; done"},
						Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse("16Mi"),
						}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
					}},
				},
			},
		},
	}
	if poolLabel == "" {
		delete(deployment.Spec.Template.Spec.NodeSelector, e.Config.PoolLabel)
		deployment.Spec.Template.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key: e.Config.PoolLabel, Operator: corev1.NodeSelectorOpIn, Values: []string{e.Config.MainLabel, e.Config.ZeroLabel},
				}},
			}}},
		}}
	}
	return deployment
}

// WorkloadState requires the expected Pod count, readiness and worker placement.
func (e *Environment) WorkloadState(ctx context.Context, namespace, name, pool string, ready, pending int) ([]corev1.Pod, error) {
	var pods corev1.PodList
	if err := e.K8s.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{RunLabel: e.Config.RunID, "app": name}); err != nil {
		return nil, err
	}
	var readyCount, pendingCount int
	var nodes corev1.NodeList
	if err := e.K8s.List(ctx, &nodes); err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	workers := PoolNodes(nodes.Items, e.Config.PoolID(pool))
	if pool == "" {
		workers = WorkerNodes(nodes.Items, e.Config)
	}
	for _, node := range workers {
		allowed[node.Name] = true
	}
	for _, pod := range pods.Items {
		if PodReady(pod) && allowed[pod.Spec.NodeName] {
			readyCount++
		}
		if pod.Spec.NodeName == "" && pod.Status.Phase == corev1.PodPending {
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse &&
					condition.Reason == corev1.PodReasonUnschedulable {
					pendingCount++
				}
			}
		}
	}
	if len(pods.Items) != ready+pending || readyCount != ready || pendingCount != pending {
		return pods.Items, fmt.Errorf("workload %s: Ready=%d Unschedulable=%d total=%d, want %d/%d/%d",
			name, readyCount, pendingCount, len(pods.Items), ready, pending, ready+pending)
	}
	return pods.Items, nil
}

// DemandFits validates CPU geometry on every currently registered pool worker.
func (e *Environment) DemandFits(ctx context.Context, snapshot Snapshot, pool string) error {
	var pods corev1.PodList
	if err := e.K8s.List(ctx, &pods); err != nil {
		return err
	}
	nodes := PoolNodes(snapshot.Nodes, e.Config.PoolID(pool))
	if len(nodes) == 0 {
		return fmt.Errorf("cannot measure request geometry without a Ready worker")
	}
	cpu := resource.MustParse(e.Config.DemandCPU)
	for _, node := range nodes {
		if err := ValidateDemand(node, pods.Items, cpu.MilliValue()); err != nil {
			return err
		}
	}
	return nil
}
