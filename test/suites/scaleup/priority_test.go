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
	"context"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("Azure Provider", Serial, Label("priority"), func() {
	var memory int64
	var low, high string

	BeforeEach(func(ctx SpecContext) {
		low, high = priorityClasses(ctx)
		memory = memoryGeometry(ctx, 0.7, true)
	})

	It("does not scale up for a pending expendable Pod", Label("Feature:ClusterSizeAutoscalingScaleUp", "Slow"), func(ctx SpecContext) {
		By("creating two expendable Pods that need two workers")
		workload := priorityDeployment("expendable", env.Config.MainLabel, low, 2, memory)
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		waitWorkload(ctx, workload.Name, env.Config.MainPool, 1, 1)
		By("checking that main does not grow for the pending expendable Pod")
		observeNoGrowth(ctx, workload.Name, env.Config.MainPool, 1, 1)
	}, NodeTimeout(30*time.Minute))

	It("scales up for pending non-expendable Pods", Label("Feature:ClusterSizeAutoscalingScaleUp", "Slow"), func(ctx SpecContext) {
		By("creating two high-priority Pods that need two workers")
		workload := priorityDeployment("high", env.Config.MainLabel, high, 2, memory)
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		By("waiting for main to grow and run both Pods")
		waitStable(ctx, 2, 0)
		assertDistinctNodes(waitWorkload(ctx, workload.Name, env.Config.MainPool, 2, 0), 2)
	}, NodeTimeout(35*time.Minute))

	It("preempts expendable Pods and doesn't scale up for their replacements", Label("Feature:ClusterSizeAutoscalingScaleUp", "Slow"), func(ctx SpecContext) {
		By("running one expendable Pod")
		expendable := priorityDeployment("low", env.Config.MainLabel, low, 1, memory)
		Expect(env.K8s.Create(ctx, expendable)).To(Succeed())
		old := waitWorkload(ctx, expendable.Name, env.Config.MainPool, 1, 0)[0]
		By("creating a high-priority Pod that preempts it")
		important := priorityDeployment("high", env.Config.MainLabel, high, 1, memory)
		Expect(env.K8s.Create(ctx, important)).To(Succeed())
		waitWorkload(ctx, important.Name, env.Config.MainPool, 1, 0)
		Eventually(ctx, func() error {
			return env.K8s.Get(ctx, client.ObjectKeyFromObject(&old), &corev1.Pod{})
		}, 2*time.Minute, pollInterval).Should(MatchError(apierrors.IsNotFound, "IsNotFound"),
			"the original low-priority Pod %s must actually be preempted", old.Name)
		By("checking that main does not grow for the replacement expendable Pod")
		waitWorkload(ctx, expendable.Name, env.Config.MainPool, 0, 1)
		observeNoGrowth(ctx, expendable.Name, env.Config.MainPool, 0, 1)
		waitWorkload(ctx, important.Name, env.Config.MainPool, 1, 0)
	}, NodeTimeout(35*time.Minute))

	It("deletes nodes that run only expendable Pods", Label("Feature:ClusterSizeAutoscalingScaleDown", "Slow"), func(ctx SpecContext) {
		growth, grown := growThree(ctx)
		By("running one expendable reservation on each worker")
		workload := priorityDeployment("expendable", "", low, 3, memory)
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		assertDistinctNodes(waitWorkload(ctx, workload.Name, "", 3, 0), 3)
		By("removing the growth demand and waiting for the added workers to be deleted")
		Expect(env.K8s.Delete(ctx, growth)).To(Succeed())
		waitBaselineDeleted(ctx, grown, 0, nil)
		waitWorkload(ctx, workload.Name, env.Config.MainPool, 1, 2)
	}, NodeTimeout(50*time.Minute))

	It("keeps nodes that run non-expendable Pods", Label("Feature:ClusterSizeAutoscalingScaleDown", "Slow"), func(ctx SpecContext) {
		growth, grown := growThree(ctx)
		By("running one high-priority reservation on each worker")
		workload := priorityDeployment("important", "", high, 3, memory)
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		assertDistinctNodes(waitWorkload(ctx, workload.Name, "", 3, 0), 3)
		By("removing the growth demand and checking that every worker stays")
		Expect(env.K8s.Delete(ctx, growth)).To(Succeed())
		Consistently(ctx, func(g Gomega) {
			current, err := readActiveSnapshot(ctx)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(current.Stable(env.Config, 2, 1)).To(Succeed())
			g.Expect(current.Pools).To(Equal(grown.Pools))
			_, err = env.WorkloadState(ctx, namespace.Name, workload.Name, "", 3, 0)
			g.Expect(err).NotTo(HaveOccurred())
		}, 5*time.Minute, pollInterval).Should(Succeed())
	}, NodeTimeout(40*time.Minute))
})

// controllerContainer returns the autoscaler container from the Deployment
// template.
func controllerContainer(ctx context.Context) corev1.Container {
	var deployment appsv1.Deployment
	Expect(env.K8s.Get(ctx, client.ObjectKey{Namespace: env.Config.AutoscalerNamespace, Name: env.Config.AutoscalerDeployment}, &deployment)).To(Succeed())
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == env.Config.AutoscalerContainer {
			return container
		}
	}
	Fail("autoscaler container is missing")
	return corev1.Container{}
}

// priorityClasses checks the expendable cutoff flag and the run's two
// PriorityClasses, and returns the low and high class names.
func priorityClasses(ctx context.Context) (low, high string) {
	container := controllerContainer(ctx)
	Expect(slices.Concat(container.Command, container.Args)).To(ContainElement("--expendable-pods-priority-cutoff=-10"))
	low, high = env.Config.RunID+"-expendable", env.Config.RunID+"-high"
	for name, expected := range map[string]int32{low: -15, high: 1000} {
		var class schedulingv1.PriorityClass
		Expect(env.K8s.Get(ctx, client.ObjectKey{Name: name}, &class)).To(Succeed(), "the run's PriorityClasses must exist")
		Expect(class.Value).To(Equal(expected))
		Expect(class.GlobalDefault).To(BeFalse())
		if class.PreemptionPolicy != nil {
			Expect(*class.PreemptionPolicy).To(Equal(corev1.PreemptLowerPriority))
		}
	}
	return low, high
}

// priorityDeployment returns a memory Deployment that uses PriorityClass class.
func priorityDeployment(name, pool, class string, replicas int32, bytes int64) *appsv1.Deployment {
	workload := memoryDeployment(name, pool, replicas, bytes)
	workload.Spec.Template.Spec.PriorityClassName = class
	return workload
}
