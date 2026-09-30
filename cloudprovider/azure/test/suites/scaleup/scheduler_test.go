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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

const bypassedScheduler = "non-existing-bypassed-scheduler"

var _ = Describe("Azure Provider", Serial, Label("scheduler"), func() {
	It("scales up for Pods of a bypassed scheduler that don't fit", Label("Feature:ClusterSizeAutoscalingScaleUp", "Slow"), func(ctx SpecContext) {
		checkBypassProfile(ctx, bypassedScheduler, true)
		By("creating two Pods for the bypassed scheduler that need two workers")
		workload := memoryDeployment("unprocessed", env.Config.MainLabel, 2, memoryGeometry(ctx, 0.7, true))
		workload.Spec.Template.Spec.SchedulerName = bypassedScheduler
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		waitUnprocessed(ctx, workload.Name, 2)
		By("waiting for main to grow while the Pods stay unprocessed")
		waitStable(ctx, 2, 0)
		waitUnprocessed(ctx, workload.Name, 2)
	}, NodeTimeout(35*time.Minute))

	It("does not scale up for Pods of a bypassed scheduler that fit on a node", Label("Feature:ClusterSizeAutoscalingScaleUp"), func(ctx SpecContext) {
		checkBypassProfile(ctx, bypassedScheduler, true)
		By("creating one Pod for the bypassed scheduler that fits the worker")
		workload := memoryDeployment("unprocessed", env.Config.MainLabel, 1, memoryGeometry(ctx, 0.5, true))
		workload.Spec.Template.Spec.SchedulerName = bypassedScheduler
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		waitUnprocessed(ctx, workload.Name, 1)
		By("checking that main does not grow")
		observeUnprocessedBaseline(ctx, workload.Name, 1)
	}, NodeTimeout(20*time.Minute))

	It("ignores pending Pods of a scheduler that isn't bypassed", Label("Feature:ClusterSizeAutoscalingScaleUp"), func(ctx SpecContext) {
		scheduler := "unconfigured-" + namespace.Name
		checkBypassProfile(ctx, scheduler, false)
		By("creating two Pods for a scheduler that is not bypassed")
		workload := memoryDeployment("unprocessed", env.Config.MainLabel, 2, memoryGeometry(ctx, 0.7, true))
		workload.Spec.Template.Spec.SchedulerName = scheduler
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		waitUnprocessed(ctx, workload.Name, 2)
		By("checking that main does not grow")
		observeUnprocessedBaseline(ctx, workload.Name, 2)
	}, NodeTimeout(20*time.Minute))
})

// checkBypassProfile requires scheduler to be in the controller's
// --bypassed-scheduler-names flag when expected is true, and absent otherwise.
func checkBypassProfile(ctx context.Context, scheduler string, expected bool) {
	container := controllerContainer(ctx)
	var bypassed []string
	for _, arg := range slices.Concat(container.Command, container.Args) {
		if names, found := strings.CutPrefix(arg, "--bypassed-scheduler-names="); found {
			bypassed = append(bypassed, strings.Split(names, ",")...)
		}
	}
	if expected {
		Expect(bypassed).To(ContainElement(scheduler), "the controller must bypass this scheduler")
	} else {
		Expect(bypassed).NotTo(ContainElement(scheduler), "the controller must not bypass this scheduler")
	}
}

// unprocessed requires replicas Pods of workload name that no scheduler has
// processed: pending, without a Node and without a PodScheduled condition.
func unprocessed(ctx context.Context, g Gomega, name string, replicas int) {
	pods, err := listWorkload(ctx, name)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pods).To(HaveLen(replicas))
	for _, pod := range pods {
		g.Expect(pod.Spec.NodeName).To(BeEmpty(), "Pod %s must not be bound", pod.Name)
		g.Expect(pod.Status.Phase).To(Equal(corev1.PodPending), "Pod %s must stay pending", pod.Name)
		g.Expect(environment.PodReady(pod)).To(BeFalse(), "Pod %s must not be Ready", pod.Name)
		for _, condition := range pod.Status.Conditions {
			g.Expect(condition.Type).NotTo(Equal(corev1.PodScheduled), "the fixture requires an absent scheduler, not a scheduling rejection")
		}
	}
}

// waitUnprocessed waits for replicas Pods of workload name, then requires
// them to be unprocessed.
func waitUnprocessed(ctx context.Context, name string, replicas int) {
	Eventually(ctx, func() ([]corev1.Pod, error) { return listWorkload(ctx, name) }, time.Minute, pollInterval).Should(HaveLen(replicas))
	unprocessed(ctx, Default, name, replicas)
}

// observeUnprocessedBaseline requires the Pods to stay unprocessed and the
// pools to stay at the baseline for five minutes.
func observeUnprocessedBaseline(ctx context.Context, name string, replicas int) {
	Consistently(ctx, func(g Gomega) {
		unprocessed(ctx, g, name, replicas)
		snapshot, err := readActiveSnapshot(ctx)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(snapshot.Stable(env.Config, 1, 0)).To(Succeed())
	}, 5*time.Minute, pollInterval).Should(Succeed())
}
