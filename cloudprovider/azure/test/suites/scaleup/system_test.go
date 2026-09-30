//go:build e2e

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

package scaleup_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("Azure Provider", Serial, func() {
	It("drains kube-system Pods that a PodDisruptionBudget protects", Label("system-pods", "Feature:ClusterSizeAutoscalingScaleDown", "Slow"), func(ctx SpecContext) {
		drain(ctx, 2, 1, "kube-system")
	}, NodeTimeout(55*time.Minute))
})

// cleanupSystemObject deletes object from kube-system after the spec. For a
// Deployment, it also waits for the Pods to go.
func cleanupSystemObject(object client.Object) {
	DeferCleanup(func(ctx SpecContext) {
		Expect(client.IgnoreNotFound(env.K8s.Delete(ctx, object))).To(Succeed())
		if deployment, ok := object.(*appsv1.Deployment); ok {
			Eventually(ctx, func() ([]corev1.Pod, error) { return listWorkloadIn(ctx, deployment.Namespace, deployment.Name) },
				4*time.Minute, pollInterval).Should(BeEmpty())
		}
	}, NodeTimeout(5*time.Minute))
}
