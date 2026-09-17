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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

const bypassedScheduler = "non-existing-bypassed-scheduler"

var _ = Describe("Public scheduler bypass scenarios", Serial, Label("public", "scheduler", "uniform"), func() {
	It("CA-017 grows for unprocessed bypassed demand that cannot fit", Label("CA-017", "bypass"), func(ctx SpecContext) {
		checkBypassProfile(ctx, bypassedScheduler, true)
		workload := memoryDeployment("unprocessed", env.Config.MainLabel, 2, memoryGeometry(ctx, 0.7, true))
		workload.Spec.Template.Spec.SchedulerName = bypassedScheduler
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		waitUnprocessed(ctx, workload.Name, 2)
		waitStable(ctx, 2, 0)
		waitUnprocessed(ctx, workload.Name, 2)
	}, NodeTimeout(35*time.Minute))

	It("CA-018 does not grow for bypassed demand that fits an existing worker", Label("CA-018", "bypass"), func(ctx SpecContext) {
		checkBypassProfile(ctx, bypassedScheduler, true)
		workload := memoryDeployment("unprocessed", env.Config.MainLabel, 1, memoryGeometry(ctx, 0.5, true))
		workload.Spec.Template.Spec.SchedulerName = bypassedScheduler
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		waitUnprocessed(ctx, workload.Name, 1)
		observeUnprocessedBaseline(ctx, workload.Name, 1)
	}, NodeTimeout(20*time.Minute))

	It("CA-019 ignores unprocessed demand from an unconfigured scheduler", Label("CA-019"), func(ctx SpecContext) {
		scheduler := "unconfigured-" + namespace.Name
		checkBypassProfile(ctx, scheduler, false)
		workload := memoryDeployment("unprocessed", env.Config.MainLabel, 2, memoryGeometry(ctx, 0.7, true))
		workload.Spec.Template.Spec.SchedulerName = scheduler
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		waitUnprocessed(ctx, workload.Name, 2)
		observeUnprocessedBaseline(ctx, workload.Name, 2)
	}, NodeTimeout(20*time.Minute))
})

func checkBypassProfile(ctx context.Context, scheduler string, expected bool) {
	container := controllerContainer(ctx)
	found := false
	for _, arg := range append(container.Command, container.Args...) {
		if strings.HasPrefix(arg, "--bypassed-scheduler-names=") {
			for _, name := range strings.Split(strings.TrimPrefix(arg, "--bypassed-scheduler-names="), ",") {
				found = found || name == scheduler
			}
		}
	}
	Expect(found).To(Equal(expected), "operator-prepared bypass profile must match this case")
}

func unprocessed(ctx context.Context, name string, replicas int) {
	pods := listWorkload(ctx, name)
	Expect(pods).To(HaveLen(replicas))
	for _, pod := range pods {
		Expect(pod.Spec.NodeName).To(BeEmpty())
		Expect(pod.Status.Phase).To(Equal(corev1.PodPending))
		Expect(environment.PodReady(pod)).To(BeFalse())
		for _, condition := range pod.Status.Conditions {
			Expect(condition.Type).NotTo(Equal(corev1.PodScheduled), "the fixture requires an absent scheduler, not a scheduling rejection")
		}
	}
}

func waitUnprocessed(ctx context.Context, name string, replicas int) {
	Eventually(ctx, func() int { return len(listWorkload(ctx, name)) }, time.Minute, pollInterval).Should(Equal(replicas))
	unprocessed(ctx, name, replicas)
}

func observeUnprocessedBaseline(ctx context.Context, name string, replicas int) {
	Consistently(ctx, func(g Gomega) {
		unprocessed(ctx, name, replicas)
		snapshot, err := readActiveSnapshot(ctx)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(snapshot.Stable(env.Config, 1, 0)).To(Succeed())
	}, 5*time.Minute, pollInterval).Should(Succeed())
}
