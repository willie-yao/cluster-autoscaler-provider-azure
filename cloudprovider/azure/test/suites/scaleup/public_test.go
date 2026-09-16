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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

var _ = Describe("Public Delete-mode scenarios", Serial, Label("public", "uniform"), func() {
	It("AZ-001 grows under 100 CPU-requesting Pods and deletes unneeded workers", Label("AZ-001"), func(ctx SpecContext) {
		workload := env.Deployment(namespace.Name, "cpu-smoke", "", "200m", 100)
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		grown := waitWorkers(ctx, 3)
		Eventually(ctx, func(g Gomega) {
			pods := listWorkload(ctx, workload.Name)
			g.Expect(pods).To(HaveLen(100))
			nodes := map[string]bool{}
			pending := 0
			for _, pod := range pods {
				if environment.PodReady(pod) {
					nodes[pod.Spec.NodeName] = true
				} else {
					g.Expect(pod.Status.Phase).To(Equal(corev1.PodPending))
					pending++
				}
			}
			g.Expect(nodes).To(HaveLen(3), "new Azure workers must actually run test Pods")
			g.Expect(pending).To(BeNumerically(">", 0), "20 requested cores cannot fit inside this eight-vCPU envelope")
		}, settleTimeout, pollInterval).Should(Succeed())
		Expect(env.K8s.Delete(ctx, workload)).To(Succeed())
		waitBaselineDeleted(ctx, grown, 0)
	}, NodeTimeout(50*time.Minute))

	It("CA-001 rejects a Pod larger than every eligible worker", Label("CA-001"), func(ctx SpecContext) {
		memory := memoryGeometry(ctx, 1.1, false)
		workload := memoryDeployment("oversized", env.Config.MainLabel, 1, memory)
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		pods := waitWorkload(ctx, workload.Name, env.Config.MainPool, 0, 1)
		waitPodEvent(ctx, pods[0], "NotTriggerScaleUp")
		observeNoGrowth(ctx, workload.Name, env.Config.MainPool, 0, 1)
	}, NodeTimeout(25*time.Minute))

	It("CA-002 grows and runs all 100 small memory-requesting Pods", Label("CA-002"), func(ctx SpecContext) {
		memory := memoryGeometry(ctx, 1, false)
		workload := memoryDeployment("small", env.Config.MainLabel, 100, (memory+99)/100)
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		grown := waitStable(ctx, 2, 0)
		waitWorkload(ctx, workload.Name, env.Config.MainPool, 100, 0)
		Expect(env.K8s.Delete(ctx, workload)).To(Succeed())
		waitBaselineDeleted(ctx, grown, 0)
	}, NodeTimeout(50*time.Minute))

	It("CA-004 grows for a host-port conflict rather than resource pressure", Label("CA-004"), func(ctx SpecContext) {
		workload := env.Deployment(namespace.Name, "host-port", "", "10m", 3)
		workload.Spec.Template.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 4321, HostPort: 4321, Protocol: corev1.ProtocolTCP}}
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		waitWorkers(ctx, 3)
		assertDistinctNodes(waitWorkload(ctx, workload.Name, "", 3, 0), 3)
	}, NodeTimeout(35*time.Minute))

	It("CA-005 grows for hostname anti-affinity", Label("CA-005"), func(ctx SpecContext) {
		workload := antiAffinityDeployment("anti-affinity", "", 1)
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		waitWorkload(ctx, workload.Name, "", 1, 0)
		scaleDeployment(ctx, workload, 3)
		waitWorkers(ctx, 3)
		assertDistinctNodes(waitWorkload(ctx, workload.Name, "", 3, 0), 3)
	}, NodeTimeout(35*time.Minute))

	It("CA-006 permits scale-up for a pending EmptyDir Pod", Label("CA-006"), func(ctx SpecContext) {
		workload := antiAffinityDeployment("empty-dir", env.Config.MainLabel, 1)
		workload.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		workload.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "scratch", MountPath: "/scratch"}}
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		waitWorkload(ctx, workload.Name, env.Config.MainPool, 1, 0)
		scaleDeployment(ctx, workload, 2)
		waitStable(ctx, 2, 0)
		assertDistinctNodes(waitWorkload(ctx, workload.Name, env.Config.MainPool, 2, 0), 2)
	}, NodeTimeout(35*time.Minute))

	It("CA-007 physically deletes workers after pressure disappears", Label("CA-007"), func(ctx SpecContext) {
		workload, grown := growThree(ctx)
		Expect(env.K8s.Delete(ctx, workload)).To(Succeed())
		waitBaselineDeleted(ctx, grown, 0)
	}, NodeTimeout(50*time.Minute))

	DescribeTable("drain honors a test-owned PodDisruptionBudget",
		func(ctx SpecContext, podsPerNode, allowed int) {
			drain(ctx, podsPerNode, allowed, namespace.Name)
		},
		Entry("CA-008 allows rescheduling with one disruption", Label("CA-008"), NodeTimeout(55*time.Minute), 1, 1),
		Entry("CA-009 blocks deletion with zero disruptions", Label("CA-009"), NodeTimeout(55*time.Minute), 1, 0),
		Entry("CA-010 drains multiple Pods sequentially with one disruption", Label("CA-010"), NodeTimeout(55*time.Minute), 2, 1),
	)
})

func listWorkload(ctx context.Context, name string) []corev1.Pod {
	return listWorkloadIn(ctx, namespace.Name, name)
}

func listWorkloadIn(ctx context.Context, workloadNamespace, name string) []corev1.Pod {
	var pods corev1.PodList
	Expect(env.K8s.List(ctx, &pods, client.InNamespace(workloadNamespace),
		client.MatchingLabels{environment.RunLabel: env.Config.RunID, "app": name})).To(Succeed())
	return pods.Items
}

func assertDistinctNodes(pods []corev1.Pod, count int) {
	nodes := map[string]bool{}
	for _, pod := range pods {
		nodes[pod.Spec.NodeName] = true
	}
	Expect(nodes).To(HaveLen(count))
}

func scaleDeployment(ctx context.Context, workload *appsv1.Deployment, replicas int32) {
	Expect(env.K8s.Get(ctx, client.ObjectKeyFromObject(workload), workload)).To(Succeed())
	workload.Spec.Replicas = ptr.To(replicas)
	Expect(env.K8s.Update(ctx, workload)).To(Succeed())
}

func antiAffinityDeployment(name, pool string, replicas int32) *appsv1.Deployment {
	workload := env.Deployment(namespace.Name, name, pool, "10m", replicas)
	if workload.Spec.Template.Spec.Affinity == nil {
		workload.Spec.Template.Spec.Affinity = &corev1.Affinity{}
	}
	workload.Spec.Template.Spec.Affinity.PodAntiAffinity = &corev1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			TopologyKey: "kubernetes.io/hostname", LabelSelector: workload.Spec.Selector.DeepCopy(),
		}},
	}
	return workload
}

func growThree(ctx context.Context) (*appsv1.Deployment, environment.Snapshot) {
	workload := antiAffinityDeployment("grow-three", "", 3)
	Expect(env.K8s.Create(ctx, workload)).To(Succeed())
	grown := waitWorkers(ctx, 3)
	assertDistinctNodes(waitWorkload(ctx, workload.Name, "", 3, 0), 3)
	return workload, grown
}

func waitWorkers(ctx context.Context, count int) environment.Snapshot {
	var snapshot environment.Snapshot
	Eventually(ctx, func() error {
		var err error
		snapshot, err = env.Read(ctx)
		if err != nil {
			return err
		}
		main, zero := snapshot.Pools[env.Config.MainPool].Capacity, snapshot.Pools[env.Config.ZeroPool].Capacity
		if main+zero != count {
			return fmt.Errorf("observed %d target workers, want %d", main+zero, count)
		}
		return snapshot.Stable(env.Config, main, zero)
	}, settleTimeout, pollInterval).Should(Succeed())
	reportSnapshot("stable-workers", snapshot)
	return snapshot
}

// memoryGeometry uses a current eligible worker, not a tainted control plane.
func memoryGeometry(ctx context.Context, fraction float64, mustFit bool) int64 {
	snapshot := waitStable(ctx, 1, 0)
	node := environment.PoolNodes(snapshot.Nodes, env.Config.PoolID(env.Config.MainPool))[0]
	allocatable := node.Status.Allocatable.Memory().Value()
	Expect(allocatable).To(BeNumerically(">", 0))
	memory := int64(float64(allocatable) * fraction)
	var pods corev1.PodList
	Expect(env.K8s.List(ctx, &pods)).To(Succeed())
	requests, err := environment.NodeRequests(node, pods.Items)
	Expect(err).NotTo(HaveOccurred())
	occupied := requests.Memory().Value()
	Expect(occupied).To(BeNumerically(">", 0), "the source small-Pod geometry requires measured background requests")
	if mustFit {
		Expect(memory+16*1024*1024).To(BeNumerically("<=", allocatable-occupied), "one reservation plus the growth fixture must fit")
	}
	if fraction > 0.5 {
		Expect(2*memory).To(BeNumerically(">", allocatable), "two reservations must not fit on a worker")
	}
	AddReportEntry("memory-geometry", fmt.Sprintf("worker=%s allocatableBytes=%d existingRequestsBytes=%d requestBytes=%d", node.Name, allocatable, occupied, memory))
	return memory
}

func memoryDeployment(name, pool string, replicas int32, bytes int64) *appsv1.Deployment {
	workload := env.Deployment(namespace.Name, name, pool, "5m", replicas)
	workload.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = *resource.NewQuantity(bytes, resource.BinarySI)
	return workload
}

func waitPodEvent(ctx context.Context, pod corev1.Pod, reason string) {
	Eventually(ctx, func() error {
		var events corev1.EventList
		if err := env.K8s.List(ctx, &events, client.InNamespace(pod.Namespace)); err != nil {
			return err
		}
		for _, event := range events.Items {
			if event.InvolvedObject.UID == pod.UID && event.InvolvedObject.Kind == "Pod" && event.Reason == reason {
				return nil
			}
		}
		return fmt.Errorf("no %s event for the exact test Pod UID", reason)
	}, 5*time.Minute, pollInterval).Should(Succeed())
}

func observeNoGrowth(ctx context.Context, name, pool string, ready, pending int) {
	Consistently(ctx, func() error {
		snapshot, err := env.Read(ctx)
		if err != nil {
			return err
		}
		if err := snapshot.Stable(env.Config, 1, 0); err != nil {
			return err
		}
		_, err = env.WorkloadState(ctx, namespace.Name, name, pool, ready, pending)
		return err
	}, 5*time.Minute, pollInterval).Should(Succeed())
}

func waitBaselineDeleted(ctx context.Context, before environment.Snapshot, minimumReady int, workload ...*appsv1.Deployment) {
	var after environment.Snapshot
	Eventually(ctx, func() error {
		if len(workload) > 0 {
			ready := 0
			for _, pod := range listWorkloadIn(ctx, workload[0].Namespace, workload[0].Name) {
				if environment.PodReady(pod) {
					ready++
				}
			}
			Expect(ready).To(BeNumerically(">=", minimumReady), "workload continuity during drain")
		}
		var err error
		after, err = env.Read(ctx)
		if err != nil {
			return err
		}
		if err := after.Stable(env.Config, 1, 0); err != nil {
			return err
		}
		for pool, baseline := range map[string]int{env.Config.MainPool: 1, env.Config.ZeroPool: 0} {
			if err := env.Deleted(ctx, before, after, pool, len(before.Pools[pool].Instances)-baseline); err != nil {
				return err
			}
		}
		return nil
	}, settleTimeout, pollInterval).Should(Succeed())
	reportSnapshot("physical-delete-baseline", after)
}

func drain(ctx context.Context, podsPerNode, allowed int, workloadNamespace string) {
	growth, grown := growThree(ctx)
	replicas := int32(3 * podsPerNode)
	protected := env.Deployment(workloadNamespace, "drain-"+namespace.Name, "", "10m", replicas)
	protected.Spec.Template.Spec.Affinity.PodAntiAffinity = &corev1.PodAntiAffinity{
		PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
			Weight: 100, PodAffinityTerm: corev1.PodAffinityTerm{TopologyKey: "kubernetes.io/hostname", LabelSelector: protected.Spec.Selector.DeepCopy()},
		}},
	}
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: protected.Name, Namespace: workloadNamespace, Labels: protected.Labels},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: ptr.To(intstr.FromInt32(replicas - int32(allowed))),
			Selector:     protected.Spec.Selector.DeepCopy(),
		},
	}
	if workloadNamespace == "kube-system" {
		checkSystemFixture(ctx, protected.Labels)
	}
	Expect(env.K8s.Create(ctx, pdb)).To(Succeed())
	if workloadNamespace == "kube-system" {
		cleanupSystemObject(pdb)
	}
	Expect(env.K8s.Create(ctx, protected)).To(Succeed())
	if workloadNamespace == "kube-system" {
		cleanupSystemObject(protected)
	}
	pods := waitWorkloadIn(ctx, workloadNamespace, protected.Name, "", int(replicas), 0)
	distribution := map[string]int{}
	for _, pod := range pods {
		distribution[pod.Spec.NodeName]++
	}
	Expect(distribution).To(HaveLen(3))
	for _, count := range distribution {
		Expect(count).To(Equal(podsPerNode), "drain fixture must start with movable replicas on every worker")
	}
	Eventually(ctx, func(g Gomega) {
		g.Expect(env.K8s.Get(ctx, client.ObjectKeyFromObject(pdb), pdb)).To(Succeed())
		g.Expect(pdb.Status.ObservedGeneration).To(Equal(pdb.Generation))
		g.Expect(pdb.Status.DisruptionsAllowed).To(Equal(int32(allowed)))
	}, time.Minute, pollInterval).Should(Succeed())
	Expect(env.K8s.Delete(ctx, growth)).To(Succeed())
	if allowed == 0 {
		Consistently(ctx, func(g Gomega) {
			current, err := env.Read(ctx)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(current.Stable(env.Config, 2, 1)).To(Succeed())
			g.Expect(current.Pools).To(Equal(grown.Pools))
			_, err = env.WorkloadState(ctx, workloadNamespace, protected.Name, "", int(replicas), 0)
			g.Expect(err).NotTo(HaveOccurred())
		}, 5*time.Minute, pollInterval).Should(Succeed())
		return
	}
	waitBaselineDeleted(ctx, grown, int(replicas)-allowed, protected)
	waitWorkloadIn(ctx, workloadNamespace, protected.Name, env.Config.MainPool, int(replicas), 0)
}
