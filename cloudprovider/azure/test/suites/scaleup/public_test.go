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

var _ = Describe("Azure Provider", Serial, func() {
	It("scales up for 100 CPU-requesting Pods and deletes the unneeded nodes", Label("cpu", "Slow"), func(ctx SpecContext) {
		By("creating 100 CPU-requesting Pods")
		workload := env.Deployment(namespace.Name, "cpu-smoke", "", "200m", 100)
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		By("waiting for three workers that run test Pods")
		grown := waitWorkers(ctx, 3)
		Eventually(ctx, func(g Gomega) {
			pods, err := listWorkload(ctx, workload.Name)
			g.Expect(err).NotTo(HaveOccurred())
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
			g.Expect(pending).To(BeNumerically(">", 0), "20 requested cores cannot fit on three workers")
		}, settleTimeout, pollInterval).Should(Succeed())
		By("removing the demand and waiting for the added workers to be deleted")
		Expect(env.K8s.Delete(ctx, workload)).To(Succeed())
		waitBaselineDeleted(ctx, grown, 0, nil)
	}, NodeTimeout(50*time.Minute))

	It("does not scale up for a Pod larger than every node", Label("memory", "Feature:ClusterSizeAutoscalingScaleUp"), func(ctx SpecContext) {
		By("creating a Pod that requests more memory than a worker has")
		memory := memoryGeometry(ctx, 1.1, false)
		workload := memoryDeployment("oversized", env.Config.MainLabel, 1, memory)
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		pods := waitWorkload(ctx, workload.Name, env.Config.MainPool, 0, 1)
		By("waiting for the NotTriggerScaleUp event and checking that no worker is added")
		waitPodEvent(ctx, pods[0], "NotTriggerScaleUp")
		observeNoGrowth(ctx, workload.Name, env.Config.MainPool, 0, 1)
	}, NodeTimeout(25*time.Minute))

	It("scales up to run 100 small memory-requesting Pods", Label("memory", "Feature:ClusterSizeAutoscalingScaleUp", "Slow"), func(ctx SpecContext) {
		By("creating 100 small memory-requesting Pods")
		memory := memoryGeometry(ctx, 1, false)
		workload := memoryDeployment("small", env.Config.MainLabel, 100, (memory+99)/100)
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		By("waiting for main to grow and run every Pod")
		grown := waitStable(ctx, 2, 0)
		waitWorkload(ctx, workload.Name, env.Config.MainPool, 100, 0)
		By("removing the demand and waiting for the added worker to be deleted")
		Expect(env.K8s.Delete(ctx, workload)).To(Succeed())
		waitBaselineDeleted(ctx, grown, 0, nil)
	}, NodeTimeout(50*time.Minute))

	It("scales up for a host port conflict", Label("host-port", "Feature:ClusterSizeAutoscalingScaleUp", "Slow"), func(ctx SpecContext) {
		workload := env.Deployment(namespace.Name, "host-port", "", "10m", 3)
		workload.Spec.Template.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 4321, HostPort: 4321, Protocol: corev1.ProtocolTCP}}
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		waitWorkers(ctx, 3)
		assertDistinctNodes(waitWorkload(ctx, workload.Name, "", 3, 0), 3)
	}, NodeTimeout(35*time.Minute))

	It("scales up for hostname anti-affinity", Label("anti-affinity", "Feature:ClusterSizeAutoscalingScaleUp", "Slow"), func(ctx SpecContext) {
		By("starting one anti-affinity Pod")
		workload := antiAffinityDeployment("anti-affinity", "", 1)
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		waitWorkload(ctx, workload.Name, "", 1, 0)
		By("scaling to three Pods and waiting for three workers")
		scaleDeployment(ctx, workload, 3)
		waitWorkers(ctx, 3)
		assertDistinctNodes(waitWorkload(ctx, workload.Name, "", 3, 0), 3)
	}, NodeTimeout(35*time.Minute))

	It("scales up for a pending Pod with an EmptyDir volume", Label("emptydir", "Feature:ClusterSizeAutoscalingScaleUp", "Slow"), func(ctx SpecContext) {
		By("starting one EmptyDir Pod")
		workload := antiAffinityDeployment("empty-dir", env.Config.MainLabel, 1)
		workload.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		workload.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "scratch", MountPath: "/scratch"}}
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		waitWorkload(ctx, workload.Name, env.Config.MainPool, 1, 0)
		By("scaling to two Pods and waiting for main to grow")
		scaleDeployment(ctx, workload, 2)
		waitStable(ctx, 2, 0)
		assertDistinctNodes(waitWorkload(ctx, workload.Name, env.Config.MainPool, 2, 0), 2)
	}, NodeTimeout(35*time.Minute))

	It("deletes the added nodes after the demand goes away", Label("scale-down", "Feature:ClusterSizeAutoscalingScaleDown", "Slow"), func(ctx SpecContext) {
		workload, grown := growThree(ctx)
		By("removing the demand and waiting for the added workers to be deleted")
		Expect(env.K8s.Delete(ctx, workload)).To(Succeed())
		waitBaselineDeleted(ctx, grown, 0, nil)
	}, NodeTimeout(50*time.Minute))

	DescribeTable("honors a PodDisruptionBudget during scale-down",
		func(ctx SpecContext, podsPerNode, allowed int) {
			drain(ctx, podsPerNode, allowed, namespace.Name)
		},
		Entry("when one disruption is allowed", Label("drain", "Feature:ClusterSizeAutoscalingScaleDown", "Slow"), NodeTimeout(55*time.Minute), 1, 1),
		Entry("when no disruption is allowed", Label("drain", "Feature:ClusterSizeAutoscalingScaleDown", "Slow"), NodeTimeout(55*time.Minute), 1, 0),
		Entry("when two Pods per node share one disruption", Label("drain", "Feature:ClusterSizeAutoscalingScaleDown", "Slow"), NodeTimeout(55*time.Minute), 2, 1),
	)
})

// listWorkload lists the Pods of workload name in the test namespace.
func listWorkload(ctx context.Context, name string) ([]corev1.Pod, error) {
	return listWorkloadIn(ctx, namespace.Name, name)
}

// listWorkloadIn lists the Pods of workload name in workloadNamespace.
func listWorkloadIn(ctx context.Context, workloadNamespace, name string) ([]corev1.Pod, error) {
	var pods corev1.PodList
	if err := env.K8s.List(ctx, &pods, client.InNamespace(workloadNamespace),
		client.MatchingLabels{environment.RunLabel: env.Config.RunID, "app": name}); err != nil {
		return nil, fmt.Errorf("list workload %s Pods: %w", name, err)
	}
	return pods.Items, nil
}

// assertDistinctNodes requires pods to run on count different Nodes.
func assertDistinctNodes(pods []corev1.Pod, count int) {
	nodes := map[string]bool{}
	for _, pod := range pods {
		nodes[pod.Spec.NodeName] = true
	}
	Expect(nodes).To(HaveLen(count), "Pods must run on %d different Nodes", count)
}

// scaleDeployment sets the replica count of workload with a merge patch.
func scaleDeployment(ctx context.Context, workload *appsv1.Deployment, replicas int32) {
	original := workload.DeepCopy()
	workload.Spec.Replicas = ptr.To(replicas)
	Expect(env.K8s.Patch(ctx, workload, client.MergeFrom(original))).To(Succeed())
}

// antiAffinityDeployment returns a Deployment whose Pods must run on
// different Nodes.
func antiAffinityDeployment(name, pool string, replicas int32) *appsv1.Deployment {
	workload := env.Deployment(namespace.Name, name, pool, "10m", replicas)
	if workload.Spec.Template.Spec.Affinity == nil {
		workload.Spec.Template.Spec.Affinity = &corev1.Affinity{}
	}
	workload.Spec.Template.Spec.Affinity.PodAntiAffinity = &corev1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			TopologyKey: corev1.LabelHostname, LabelSelector: workload.Spec.Selector.DeepCopy(),
		}},
	}
	return workload
}

// growThree starts three anti-affinity Pods and waits until they run on
// three workers. It returns the Deployment and the grown snapshot.
func growThree(ctx context.Context) (*appsv1.Deployment, environment.Snapshot) {
	By("growing to three workers with anti-affinity Pods")
	workload := antiAffinityDeployment("grow-three", "", 3)
	Expect(env.K8s.Create(ctx, workload)).To(Succeed())
	grown := waitWorkers(ctx, 3)
	assertDistinctNodes(waitWorkload(ctx, workload.Name, "", 3, 0), 3)
	return workload, grown
}

// waitWorkers waits until main and zero have count workers in total and
// both pools are stable.
func waitWorkers(ctx context.Context, count int) environment.Snapshot {
	var snapshot environment.Snapshot
	Eventually(ctx, func() error {
		var err error
		snapshot, err = readSnapshot(ctx)
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

// memoryGeometry returns fraction of the memory that the one main worker can
// allocate. With mustFit, it requires that request and a small growth Pod to
// fit beside the worker's current requests. With a fraction above one half,
// it requires two such requests not to fit on one worker.
func memoryGeometry(ctx context.Context, fraction float64, mustFit bool) int64 {
	snapshot := waitStable(ctx, 1, 0)
	node := environment.PoolNodes(snapshot.Nodes, env.Config.PoolID(env.Config.MainPool))[0]
	allocatable := node.Status.Allocatable.Memory().Value()
	Expect(allocatable).To(BeNumerically(">", 0), "worker %s must report allocatable memory", node.Name)
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

// memoryDeployment returns a Deployment whose Pods request bytes of memory.
func memoryDeployment(name, pool string, replicas int32, bytes int64) *appsv1.Deployment {
	workload := env.Deployment(namespace.Name, name, pool, "5m", replicas)
	workload.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = *resource.NewQuantity(bytes, resource.BinarySI)
	return workload
}

// waitPodEvent waits for an event with reason on the exact Pod UID.
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
		return fmt.Errorf("no %s event for test Pod %s with UID %s", reason, pod.Name, pod.UID)
	}, 5*time.Minute, pollInterval).Should(Succeed())
}

// observeNoGrowth requires the baseline pools and the workload state to stay
// unchanged for five minutes.
func observeNoGrowth(ctx context.Context, name, pool string, ready, pending int) {
	Consistently(ctx, func() error {
		snapshot, err := readActiveSnapshot(ctx)
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

// waitBaselineDeleted waits until the pools return to one main worker and
// every worker added since the baseline is physically deleted. When protected
// is not nil, the spec fails at once if fewer than minimumReady of its Pods
// are Ready.
func waitBaselineDeleted(ctx context.Context, before environment.Snapshot, minimumReady int, protected *appsv1.Deployment) {
	var after environment.Snapshot
	Eventually(ctx, func() error {
		if protected != nil {
			pods, err := listWorkloadIn(ctx, protected.Namespace, protected.Name)
			if err != nil {
				return err
			}
			ready := 0
			for _, pod := range pods {
				if environment.PodReady(pod) {
					ready++
				}
			}
			if ready < minimumReady {
				StopTrying(fmt.Sprintf("workload %s had %d Ready Pods during the drain, want at least %d", protected.Name, ready, minimumReady)).Now()
			}
		}
		var err error
		after, err = readSnapshot(ctx)
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

// drain grows to three workers, spreads a PDB-protected workload across them
// and removes the growth demand. With allowed set to zero, it requires the
// workers to stay. Otherwise it waits for the added workers to be deleted
// while the PDB keeps the protected Pods available.
func drain(ctx context.Context, podsPerNode, allowed int, workloadNamespace string) {
	growth, grown := growThree(ctx)
	By("spreading a PDB-protected workload across the workers")
	replicas := int32(3 * podsPerNode)
	protected := env.Deployment(workloadNamespace, "drain-"+namespace.Name, "", "10m", replicas)
	protected.Spec.Template.Spec.Affinity.PodAntiAffinity = &corev1.PodAntiAffinity{
		PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
			Weight: 100, PodAffinityTerm: corev1.PodAffinityTerm{TopologyKey: corev1.LabelHostname, LabelSelector: protected.Spec.Selector.DeepCopy()},
		}},
	}
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: protected.Name, Namespace: workloadNamespace, Labels: protected.Labels},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: ptr.To(intstr.FromInt32(replicas - int32(allowed))),
			Selector:     protected.Spec.Selector.DeepCopy(),
		},
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
	Expect(distribution).To(HaveLen(3), "drain fixture must start with movable replicas on every worker")
	for node, count := range distribution {
		Expect(count).To(Equal(podsPerNode), "drain fixture must start with %d movable replicas on worker %s", podsPerNode, node)
	}
	Eventually(ctx, func(g Gomega) {
		g.Expect(env.K8s.Get(ctx, client.ObjectKeyFromObject(pdb), pdb)).To(Succeed())
		g.Expect(pdb.Status.ObservedGeneration).To(Equal(pdb.Generation))
		g.Expect(pdb.Status.DisruptionsAllowed).To(Equal(int32(allowed)))
	}, time.Minute, pollInterval).Should(Succeed())
	By("removing the growth demand")
	Expect(env.K8s.Delete(ctx, growth)).To(Succeed())
	if allowed == 0 {
		By("checking that the PDB keeps every worker for five minutes")
		Consistently(ctx, func(g Gomega) {
			current, err := readActiveSnapshot(ctx)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(current.Stable(env.Config, 2, 1)).To(Succeed())
			g.Expect(current.Pools).To(Equal(grown.Pools))
			_, err = env.WorkloadState(ctx, workloadNamespace, protected.Name, "", int(replicas), 0)
			g.Expect(err).NotTo(HaveOccurred())
		}, 5*time.Minute, pollInterval).Should(Succeed())
		return
	}
	By("waiting for the drained workers to be deleted")
	waitBaselineDeleted(ctx, grown, int(replicas)-allowed, protected)
	waitWorkloadIn(ctx, workloadNamespace, protected.Name, env.Config.MainPool, int(replicas), 0)
}
