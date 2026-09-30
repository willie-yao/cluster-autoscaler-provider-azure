//go:build e2e

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

package scaleup_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/cluster-autoscaler-provider-azure/test/pkg/environment"
)

const (
	settleTimeout = 15 * time.Minute
	pollInterval  = 10 * time.Second
)

var (
	env        *environment.Environment
	configPath string
	namespace  *corev1.Namespace
)

func init() {
	flag.StringVar(&configPath, "environment", "", "JSON binding to the prepared cluster (required)")
}

func TestScaleUp(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Azure E2E suite")
}

var _ = BeforeSuite(func() {
	Expect(configPath).NotTo(BeEmpty(), "-environment is required; no default cluster is selected")
	config, _ := GinkgoConfiguration()
	Expect(config.ParallelTotal).To(Equal(1), "the specs share one cluster and must run serially")
	cfg, err := environment.LoadConfig(configPath)
	Expect(err).NotTo(HaveOccurred())
	env, err = environment.NewEnvironment(cfg)
	Expect(err).NotTo(HaveOccurred())
})

var _ = BeforeEach(func(ctx SpecContext) {
	Expect(env.Controller(ctx)).To(Succeed())
	AddReportEntry("runtime-image", env.Config.ExpectedImage)
	baseline := waitStable(ctx, 1, 0)
	Expect(env.CheckWorkerIsolation(ctx, baseline, "")).To(Succeed())
	namespace = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		GenerateName: "azure-e2e-", Labels: map[string]string{environment.RunLabel: env.Config.RunID},
	}}
	Expect(env.K8s.Create(ctx, namespace)).To(Succeed())
	created := namespace.DeepCopy()
	DeferCleanup(func(ctx SpecContext) {
		Expect(client.IgnoreNotFound(env.K8s.Delete(ctx, created))).To(Succeed())
		Eventually(ctx, func() error {
			err := env.K8s.Get(ctx, client.ObjectKeyFromObject(created), &corev1.Namespace{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			return fmt.Errorf("test namespace is still terminating")
		}, 4*time.Minute, pollInterval).Should(Succeed())
		waitStable(ctx, 1, 0)
	}, NodeTimeout(20*time.Minute))
}, NodeTimeout(20*time.Minute))

var _ = Describe("Azure Provider", Serial, func() {
	It("discovers only the tagged node groups and stays idle for five minutes", Label("smoke", "idle"), func(ctx SpecContext) {
		Consistently(ctx, func() error {
			snapshot, err := readActiveSnapshot(ctx)
			if err != nil {
				return err
			}
			return snapshot.Stable(env.Config, 1, 0)
		}, 5*time.Minute, pollInterval).Should(Succeed())
	}, NodeTimeout(7*time.Minute))

	It("scales up for pending Pods, stops at the node group maximum and deletes the extra node", Label("smoke", "max-size", "Slow"), func(ctx SpecContext) {
		growthAndDelete(ctx)
	}, NodeTimeout(50*time.Minute))

	It("scales a node group up from zero and deletes the VM, Node and NIC when it returns to zero", Label("smoke", "scale-from-zero", "Slow"), func(ctx SpecContext) {
		By("creating demand for the zero pool")
		deployment := env.Deployment(namespace.Name, "zero", env.Config.ZeroLabel, env.Config.DemandCPU, 1)
		Expect(env.K8s.Create(ctx, deployment)).To(Succeed())
		By("waiting for the zero pool to grow to one worker")
		grown := waitStable(ctx, 1, 1)
		waitWorkload(ctx, deployment.Name, env.Config.ZeroPool, 1, 0)
		By("removing the demand and waiting for the zero-pool worker to be deleted")
		Expect(env.K8s.Delete(ctx, deployment)).To(Succeed())
		waitDeleted(ctx, grown, env.Config.ZeroPool, 1, 1, 0)
	}, NodeTimeout(40*time.Minute))

	It("keeps nodes that a blocking PodDisruptionBudget protects and drains them once it allows one disruption", Label("pdb", "Slow"), func(ctx SpecContext) {
		By("growing main to two workers")
		grow := env.Deployment(namespace.Name, "grow", env.Config.MainLabel, env.Config.DemandCPU, 2)
		Expect(env.DemandFits(ctx, waitStable(ctx, 1, 0), env.Config.MainPool)).To(Succeed())
		Expect(env.K8s.Create(ctx, grow)).To(Succeed())
		grown := waitStable(ctx, 2, 0)
		waitWorkload(ctx, grow.Name, env.Config.MainPool, 2, 0)
		By("spreading a workload across both workers with a PDB that allows no disruption")
		protected, pdb := newProtectedPDBFixture(env, namespace.Name)
		Expect(env.K8s.Create(ctx, pdb)).To(Succeed())
		expectedPDB := pdb.DeepCopy()
		DeferCleanup(func(ctx SpecContext) {
			reportPDBDiagnostics(ctx, "final", expectedPDB, protected.Spec.Selector.MatchLabels)
		}, NodeTimeout(time.Minute))
		Expect(env.K8s.Create(ctx, protected)).To(Succeed())
		pods := waitWorkload(ctx, protected.Name, env.Config.MainPool, 2, 0)
		Expect(pods[0].Spec.NodeName).NotTo(Equal(pods[1].Spec.NodeName))
		By("removing the growth demand and checking that the PDB keeps both workers")
		Expect(env.K8s.Delete(ctx, grow)).To(Succeed())
		Eventually(ctx, func(g Gomega) {
			g.Expect(env.K8s.Get(ctx, client.ObjectKeyFromObject(pdb), pdb)).To(Succeed())
			g.Expect(pdb.Status.ObservedGeneration).To(Equal(pdb.Generation))
			g.Expect(pdb.Status.CurrentHealthy).To(Equal(int32(2)))
			g.Expect(pdb.Status.DisruptionsAllowed).To(BeZero())
		}, time.Minute, pollInterval).Should(Succeed())
		Consistently(ctx, func(g Gomega) {
			snapshot, err := readActiveSnapshot(ctx)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(snapshot.Stable(env.Config, 2, 0)).To(Succeed())
			g.Expect(snapshot.Pools[env.Config.MainPool].Instances).To(Equal(grown.Pools[env.Config.MainPool].Instances))
			_, err = env.WorkloadState(ctx, namespace.Name, protected.Name, env.Config.MainPool, 2, 0)
			g.Expect(err).NotTo(HaveOccurred())
		}, 5*time.Minute, pollInterval).Should(Succeed())
		reportPDBDiagnostics(ctx, "before-relaxation", expectedPDB, protected.Spec.Selector.MatchLabels)
		By("relaxing the PDB to allow one disruption")
		Expect(env.K8s.Get(ctx, client.ObjectKeyFromObject(pdb), pdb)).To(Succeed())
		blocked := pdb.DeepCopy()
		pdb.Spec.MinAvailable = ptr.To(intstr.FromInt32(1))
		blockedGeneration := pdb.Generation
		Expect(env.K8s.Patch(ctx, pdb, client.MergeFrom(blocked))).To(Succeed())
		Eventually(ctx, func(g Gomega) {
			healthy, err := protectedReadyPods(ctx, namespace.Name, protected.Spec.Selector.MatchLabels)
			g.Expect(err).NotTo(HaveOccurred())
			requireReadyReplica(healthy)
			var relaxed policyv1.PodDisruptionBudget
			g.Expect(env.K8s.Get(ctx, client.ObjectKeyFromObject(expectedPDB), &relaxed)).To(Succeed())
			g.Expect(relaxedPDBProcessed(relaxed, expectedPDB.UID, blockedGeneration)).To(Succeed())
		}, time.Minute, time.Second).Should(Succeed())
		reportPDBDiagnostics(ctx, "after-relaxation", expectedPDB, protected.Spec.Selector.MatchLabels)
		By("waiting for the drained worker to be deleted")
		Eventually(ctx, func(g Gomega) {
			healthy, err := protectedReadyPods(ctx, namespace.Name, protected.Spec.Selector.MatchLabels)
			g.Expect(err).NotTo(HaveOccurred())
			requireReadyReplica(healthy)
			after, err := readSnapshot(ctx)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(after.Stable(env.Config, 1, 0)).To(Succeed())
			g.Expect(env.Deleted(ctx, grown, after, env.Config.MainPool, 1)).To(Succeed())
		}, settleTimeout, pollInterval).Should(Succeed())
		pods = waitWorkload(ctx, protected.Name, env.Config.MainPool, 2, 0)
		Expect(pods[0].Spec.NodeName).To(Equal(pods[1].Spec.NodeName))
	}, NodeTimeout(55*time.Minute))

	It("scales up and down with VMSS ETag concurrency enabled", Label("etag", "Slow"), func(ctx SpecContext) {
		Expect(controllerContainer(ctx).Env).To(ContainElement(corev1.EnvVar{Name: "AZURE_ENABLE_VMSS_ETAG", Value: "true"}),
			"the controller must run with ETag enabled for this case")
		growthAndDelete(ctx)
	}, NodeTimeout(50*time.Minute))
})

// requireReadyReplica fails the spec at once when a PDB-protected workload
// has no Ready Pod, instead of retrying until it recovers.
func requireReadyReplica(healthy int) {
	if healthy < 1 {
		StopTrying(fmt.Sprintf("PDB must preserve at least one Ready replica at each observation, observed %d", healthy)).Now()
	}
}

// protectedReadyPods counts the Ready Pods with labels in namespace.
func protectedReadyPods(ctx context.Context, namespace string, labels map[string]string) (int, error) {
	var current corev1.PodList
	if err := env.K8s.List(ctx, &current, client.InNamespace(namespace), client.MatchingLabels(labels)); err != nil {
		return 0, fmt.Errorf("list protected Pods: %w", err)
	}
	healthy := 0
	for _, pod := range current.Items {
		if environment.PodReady(pod) {
			healthy++
		}
	}
	return healthy, nil
}

// relaxedPDBProcessed returns nil when pdb is the created fixture at a newer
// generation than blockedGeneration, requires one available Pod and has a
// status that the disruption controller computed for that budget.
func relaxedPDBProcessed(pdb policyv1.PodDisruptionBudget, expectedUID types.UID, blockedGeneration int64) error {
	if pdb.UID != expectedUID || pdb.Generation <= blockedGeneration {
		return fmt.Errorf("relaxed PDB identity or generation does not match the created fixture, UID %s generation %d, blocked generation %d",
			pdb.UID, pdb.Generation, blockedGeneration)
	}
	if pdb.Spec.MinAvailable == nil || *pdb.Spec.MinAvailable != intstr.FromInt32(1) {
		return fmt.Errorf("relaxed PDB must require exactly one available Pod, minAvailable %s", intOrString(pdb.Spec.MinAvailable))
	}
	if pdb.Status.ObservedGeneration != pdb.Generation || pdb.Status.DesiredHealthy != 1 {
		return fmt.Errorf("relaxed PDB status has not processed the one-Pod budget, observedGeneration %d generation %d desiredHealthy %d",
			pdb.Status.ObservedGeneration, pdb.Generation, pdb.Status.DesiredHealthy)
	}
	if pdb.Status.CurrentHealthy < 1 || pdb.Status.DisruptionsAllowed < 0 ||
		pdb.Status.DisruptionsAllowed > pdb.Status.CurrentHealthy-pdb.Status.DesiredHealthy ||
		pdb.Status.DisruptionsAllowed > 1 {
		return fmt.Errorf("relaxed PDB status does not reflect a valid one-Pod budget, currentHealthy %d desiredHealthy %d disruptionsAllowed %d",
			pdb.Status.CurrentHealthy, pdb.Status.DesiredHealthy, pdb.Status.DisruptionsAllowed)
	}
	return nil
}

// readSnapshot reads the environment and stops the enclosing poll at once
// when a pool is above its max tag.
func readSnapshot(ctx context.Context) (environment.Snapshot, error) {
	snapshot, err := env.Read(ctx)
	if errors.Is(err, environment.ErrBounds) {
		StopTrying("a pool exceeded its maximum").Wrap(err).Now()
	}
	return snapshot, err
}

// readActiveSnapshot checks the running controller, then works like
// readSnapshot.
func readActiveSnapshot(ctx context.Context) (environment.Snapshot, error) {
	if err := env.Controller(ctx); err != nil {
		return environment.Snapshot{}, err
	}
	return readSnapshot(ctx)
}

// waitStable waits until main and zero have the given number of Ready
// workers and matching Azure capacity.
func waitStable(ctx context.Context, main, zero int) environment.Snapshot {
	var snapshot environment.Snapshot
	Eventually(ctx, func() error {
		var err error
		snapshot, err = readSnapshot(ctx)
		if err != nil {
			return err
		}
		return snapshot.Stable(env.Config, main, zero)
	}, settleTimeout, pollInterval).Should(Succeed())
	reportSnapshot(fmt.Sprintf("stable-%d-%d", main, zero), snapshot)
	return snapshot
}

// reportSnapshot adds the pools and worker provider IDs of snapshot to the
// report.
func reportSnapshot(name string, snapshot environment.Snapshot) {
	nodes := map[string]string{}
	for _, node := range environment.WorkerNodes(snapshot.Nodes, env.Config) {
		nodes[node.Name] = node.Spec.ProviderID
	}
	AddReportEntry(name, struct {
		Pools           map[string]environment.PoolState
		NodeProviderIDs map[string]string
	}{snapshot.Pools, nodes})
}

// waitWorkload waits for the workload state in the test namespace. See
// environment.Environment.WorkloadState.
func waitWorkload(ctx context.Context, name, pool string, ready, pending int) []corev1.Pod {
	return waitWorkloadIn(ctx, namespace.Name, name, pool, ready, pending)
}

// waitWorkloadIn works like waitWorkload in workloadNamespace.
func waitWorkloadIn(ctx context.Context, workloadNamespace, name, pool string, ready, pending int) []corev1.Pod {
	var pods []corev1.Pod
	Eventually(ctx, func() error {
		var err error
		pods, err = env.WorkloadState(ctx, workloadNamespace, name, pool, ready, pending)
		return err
	}, settleTimeout, pollInterval).Should(Succeed())
	return pods
}

// waitDeleted waits until main and zero are stable at the given counts and
// deleted instances of pool in before are physically gone.
func waitDeleted(ctx context.Context, before environment.Snapshot, pool string, deleted, main, zero int) {
	var after environment.Snapshot
	Eventually(ctx, func() error {
		var err error
		after, err = readSnapshot(ctx)
		if err != nil {
			return err
		}
		if err := after.Stable(env.Config, main, zero); err != nil {
			return err
		}
		return env.Deleted(ctx, before, after, pool, deleted)
	}, settleTimeout, pollInterval).Should(Succeed())
	reportSnapshot("physical-delete-survivors", after)
	AddReportEntry("physical-delete", fmt.Sprintf("pool=%s removedInstances=%d; captured instance, Node and NIC IDs absent", pool, deleted))
}

// growthAndDelete grows main to its maximum of two workers, checks that a
// third Pod stays pending without growth, then removes the demand and waits
// for the added worker to be deleted.
func growthAndDelete(ctx context.Context) {
	Expect(env.DemandFits(ctx, waitStable(ctx, 1, 0), env.Config.MainPool)).To(Succeed())
	By("creating demand for two main workers")
	deployment := env.Deployment(namespace.Name, "demand", env.Config.MainLabel, env.Config.DemandCPU, 2)
	Expect(env.K8s.Create(ctx, deployment)).To(Succeed())
	grown := waitStable(ctx, 2, 0)
	pods := waitWorkload(ctx, deployment.Name, env.Config.MainPool, 2, 0)
	Expect(pods[0].Spec.NodeName).NotTo(Equal(pods[1].Spec.NodeName))
	By("adding a third Pod and checking that main stays at its maximum")
	scaleDeployment(ctx, deployment, 3)
	waitWorkload(ctx, deployment.Name, env.Config.MainPool, 2, 1)
	Consistently(ctx, func() error {
		snapshot, err := readActiveSnapshot(ctx)
		if err != nil {
			return err
		}
		if err := snapshot.Stable(env.Config, 2, 0); err != nil {
			return err
		}
		_, err = env.WorkloadState(ctx, namespace.Name, deployment.Name, env.Config.MainPool, 2, 1)
		return err
	}, 2*time.Minute, pollInterval).Should(Succeed())
	By("removing the demand and waiting for the added worker to be deleted")
	Expect(env.K8s.Delete(ctx, deployment)).To(Succeed())
	waitDeleted(ctx, grown, env.Config.MainPool, 1, 1, 0)
}
