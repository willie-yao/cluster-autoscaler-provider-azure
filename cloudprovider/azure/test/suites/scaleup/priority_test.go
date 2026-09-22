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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

var _ = Describe("Public priority scenarios", Serial, Label("public", "priority", "uniform"), func() {
	var memory int64
	var low, high string

	BeforeEach(func(ctx SpecContext) {
		low, high = priorityClasses(ctx)
		memory = memoryGeometry(ctx, 0.7, true)
	})

	It("CA-012 does not grow for a real expendable pending Pod", Label("CA-012"), func(ctx SpecContext) {
		workload := priorityDeployment("expendable", env.Config.MainLabel, low, 2, memory)
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		waitWorkload(ctx, workload.Name, env.Config.MainPool, 1, 1)
		observeNoGrowth(ctx, workload.Name, env.Config.MainPool, 1, 1)
	}, NodeTimeout(30*time.Minute))

	It("CA-013 grows for non-expendable pending Pods", Label("CA-013"), func(ctx SpecContext) {
		workload := priorityDeployment("high", env.Config.MainLabel, high, 2, memory)
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		waitStable(ctx, 2, 0)
		assertDistinctNodes(waitWorkload(ctx, workload.Name, env.Config.MainPool, 2, 0), 2)
	}, NodeTimeout(35*time.Minute))

	It("CA-014 preempts expendable demand without growing for its replacement", Label("CA-014"), func(ctx SpecContext) {
		expendable := priorityDeployment("low", env.Config.MainLabel, low, 1, memory)
		Expect(env.K8s.Create(ctx, expendable)).To(Succeed())
		old := waitWorkload(ctx, expendable.Name, env.Config.MainPool, 1, 0)[0]
		important := priorityDeployment("high", env.Config.MainLabel, high, 1, memory)
		Expect(env.K8s.Create(ctx, important)).To(Succeed())
		waitWorkload(ctx, important.Name, env.Config.MainPool, 1, 0)
		Eventually(ctx, func() bool {
			err := env.K8s.Get(ctx, client.ObjectKeyFromObject(&old), &corev1.Pod{})
			if err != nil && !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}
			return apierrors.IsNotFound(err)
		}, 2*time.Minute, pollInterval).Should(BeTrue(), "the original low-priority Pod must actually be preempted")
		waitWorkload(ctx, expendable.Name, env.Config.MainPool, 0, 1)
		observeNoGrowth(ctx, expendable.Name, env.Config.MainPool, 0, 1)
		waitWorkload(ctx, important.Name, env.Config.MainPool, 1, 0)
	}, NodeTimeout(35*time.Minute))

	It("CA-015 deletes workers despite running expendable reservations", Label("CA-015"), func(ctx SpecContext) {
		growth, grown := growThree(ctx)
		workload := priorityDeployment("expendable", "", low, 3, memory)
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		assertDistinctNodes(waitWorkload(ctx, workload.Name, "", 3, 0), 3)
		Expect(env.K8s.Delete(ctx, growth)).To(Succeed())
		waitBaselineDeleted(ctx, grown, 0)
		waitWorkload(ctx, workload.Name, env.Config.MainPool, 1, 2)
	}, NodeTimeout(50*time.Minute))

	It("CA-016 retains workers occupied by non-expendable reservations", Label("CA-016"), func(ctx SpecContext) {
		growth, grown := growThree(ctx)
		workload := priorityDeployment("important", "", high, 3, memory)
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		assertDistinctNodes(waitWorkload(ctx, workload.Name, "", 3, 0), 3)
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

func controllerContainer(ctx context.Context) corev1.Container {
	var deployment appsv1.Deployment
	Expect(env.K8s.Get(ctx, client.ObjectKey{Namespace: env.Config.AutoscalerNamespace, Name: env.Config.AutoscalerDeployment}, &deployment)).To(Succeed())
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == env.Config.AutoscalerContainer {
			return container
		}
	}
	Fail("authorized autoscaler container is missing")
	return corev1.Container{}
}

func priorityClasses(ctx context.Context) (string, string) {
	container := controllerContainer(ctx)
	Expect(append(container.Command, container.Args...)).To(ContainElement("--expendable-pods-priority-cutoff=-10"))
	low, high := env.Config.RunID+"-expendable", env.Config.RunID+"-high"
	for name, expected := range map[string]int32{low: -15, high: 1000} {
		var class schedulingv1.PriorityClass
		Expect(env.K8s.Get(ctx, client.ObjectKey{Name: name}, &class)).To(Succeed(), "operator must precreate the run-owned PriorityClasses")
		Expect(class.Labels[environment.RunLabel]).To(Equal(env.Config.RunID))
		Expect(class.Value).To(Equal(expected))
		Expect(class.GlobalDefault).To(BeFalse())
		if class.PreemptionPolicy != nil {
			Expect(*class.PreemptionPolicy).To(Equal(corev1.PreemptLowerPriority))
		}
	}
	return low, high
}

func priorityDeployment(name, pool, class string, replicas int32, bytes int64) *appsv1.Deployment {
	workload := memoryDeployment(name, pool, replicas, bytes)
	workload.Spec.Template.Spec.PriorityClassName = class
	return workload
}
