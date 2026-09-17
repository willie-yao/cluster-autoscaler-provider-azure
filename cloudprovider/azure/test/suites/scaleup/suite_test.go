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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
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
	flag.StringVar(&configPath, "environment", "", "explicit non-secret JSON environment binding (required)")
}

func TestScaleUp(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Azure operator-prepared E2E suite")
}

var _ = BeforeSuite(func(ctx SpecContext) {
	Expect(configPath).NotTo(BeEmpty(), "-environment is required; no default cluster is selected")
	Expect(GinkgoParallelProcess()).To(Equal(1))
	config, _ := GinkgoConfiguration()
	Expect(config.ParallelTotal).To(Equal(1), "one suite owns this disposable fixture")
	cfg, err := environment.LoadConfig(configPath)
	Expect(err).NotTo(HaveOccurred())
	env, err = environment.NewEnvironment(ctx, cfg)
	Expect(err).NotTo(HaveOccurred())
}, NodeTimeout(10*time.Minute))

var _ = BeforeEach(func(ctx SpecContext) {
	Expect(env.Authorize(ctx)).To(Succeed())
	Expect(env.Controller(ctx)).To(Succeed())
	AddReportEntry("runtime-image", env.Config.ExpectedImage)
	baseline := waitStable(ctx, 1, 0)
	Expect(env.CheckWorkerIsolation(ctx, baseline, "")).To(Succeed())
	namespace = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		GenerateName: "azure-e2e-", Labels: map[string]string{environment.RunLabel: env.Config.RunID},
	}}
	Expect(env.K8s.Create(ctx, namespace)).To(Succeed())
	ownedNamespace := namespace.DeepCopy()
	DeferCleanup(func(ctx SpecContext) {
		Expect(env.Authorize(ctx)).To(Succeed())
		var current corev1.Namespace
		err := env.K8s.Get(ctx, client.ObjectKeyFromObject(ownedNamespace), &current)
		if !apierrors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred())
			Expect(current.UID).To(Equal(ownedNamespace.UID))
			Expect(current.Labels[environment.RunLabel]).To(Equal(env.Config.RunID))
			Expect(env.K8s.Delete(ctx, &current, client.Preconditions{UID: &ownedNamespace.UID})).To(Succeed())
		}
		Eventually(ctx, func() error {
			err := env.K8s.Get(ctx, client.ObjectKeyFromObject(ownedNamespace), &corev1.Namespace{})
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

var _ = Describe("Azure VMSS Uniform", Serial, Label("uniform"), func() {
	It("AZ-P1-001 discovers only owned groups and remains idle for five minutes", Label("AZ-P1-001", "idle"), func(ctx SpecContext) {
		Consistently(ctx, func() error {
			snapshot, err := readActiveSnapshot(ctx)
			if err != nil {
				return err
			}
			return snapshot.Stable(env.Config, 1, 0)
		}, 5*time.Minute, pollInterval).Should(Succeed())
	}, NodeTimeout(7*time.Minute))

	It("AZ-P1-002 grows for real demand, refuses above max and physically deletes the excess node", Label("AZ-P1-002", "growth", "max", "delete"), func(ctx SpecContext) {
		growthAndDelete(ctx)
	}, NodeTimeout(50*time.Minute))

	It("AZ-P1-003 scales from zero and deletes the VM, Node and NIC on return to zero", Label("AZ-P1-003", "zero", "delete"), func(ctx SpecContext) {
		deployment := env.Deployment(namespace.Name, "zero", env.Config.ZeroLabel, env.Config.DemandCPU, 1)
		Expect(env.K8s.Create(ctx, deployment)).To(Succeed())
		grown := waitStable(ctx, 1, 1)
		waitWorkload(ctx, deployment.Name, env.Config.ZeroPool, 1, 0)
		Expect(env.K8s.Delete(ctx, deployment)).To(Succeed())
		waitDeleted(ctx, grown, env.Config.ZeroPool, 1, 1, 0)
	}, NodeTimeout(40*time.Minute))

	It("AZ-P1-004 honors a blocking PDB then drains and reschedules with one disruption allowed", Label("AZ-P1-004", "pdb", "delete"), func(ctx SpecContext) {
		grow := env.Deployment(namespace.Name, "grow", env.Config.MainLabel, env.Config.DemandCPU, 2)
		Expect(env.DemandFits(ctx, waitStable(ctx, 1, 0), env.Config.MainPool)).To(Succeed())
		Expect(env.K8s.Create(ctx, grow)).To(Succeed())
		grown := waitStable(ctx, 2, 0)
		waitWorkload(ctx, grow.Name, env.Config.MainPool, 2, 0)
		protected := env.Deployment(namespace.Name, "protected", env.Config.MainLabel, "50m", 2)
		protected.Spec.Template.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
			MaxSkew: 1, TopologyKey: "kubernetes.io/hostname", WhenUnsatisfiable: corev1.DoNotSchedule,
			LabelSelector: protected.Spec.Selector.DeepCopy(),
		}}
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: "protected", Namespace: namespace.Name},
			Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: ptr.To(intstr.FromInt32(2)), Selector: protected.Spec.Selector.DeepCopy()},
		}
		Expect(env.K8s.Create(ctx, pdb)).To(Succeed())
		Expect(env.K8s.Create(ctx, protected)).To(Succeed())
		pods := waitWorkload(ctx, protected.Name, env.Config.MainPool, 2, 0)
		Expect(pods[0].Spec.NodeName).NotTo(Equal(pods[1].Spec.NodeName))
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
		Expect(env.K8s.Get(ctx, client.ObjectKeyFromObject(pdb), pdb)).To(Succeed())
		pdb.Spec.MinAvailable = ptr.To(intstr.FromInt32(1))
		Expect(env.K8s.Update(ctx, pdb)).To(Succeed())
		Eventually(ctx, func(g Gomega) {
			var current corev1.PodList
			g.Expect(env.K8s.List(ctx, &current, client.InNamespace(namespace.Name), client.MatchingLabels{"app": protected.Name})).To(Succeed())
			healthy := 0
			for _, pod := range current.Items {
				if environment.PodReady(pod) {
					healthy++
				}
			}
			// A continuity failure must fail the spec, not merely retry until recovery.
			Expect(healthy).To(BeNumerically(">=", 1), "PDB must preserve at least one Ready replica at each observation")
			after, err := readSnapshot(ctx)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(after.Stable(env.Config, 1, 0)).To(Succeed())
			g.Expect(env.Deleted(ctx, grown, after, env.Config.MainPool, 1)).To(Succeed())
		}, settleTimeout, pollInterval).Should(Succeed())
		pods = waitWorkload(ctx, protected.Name, env.Config.MainPool, 2, 0)
		Expect(pods[0].Spec.NodeName).To(Equal(pods[1].Spec.NodeName))
	}, NodeTimeout(55*time.Minute))

	It("AZ-SUP-ETAG grows and deletes with VMSS ETag concurrency enabled", Label("AZ-SUP-ETAG", "etag"), func(ctx SpecContext) {
		var deployment appsv1.Deployment
		Expect(env.K8s.Get(ctx, client.ObjectKey{Namespace: env.Config.AutoscalerNamespace, Name: env.Config.AutoscalerDeployment}, &deployment)).To(Succeed())
		enabled := false
		for _, container := range deployment.Spec.Template.Spec.Containers {
			if container.Name == env.Config.AutoscalerContainer {
				for _, variable := range container.Env {
					enabled = enabled || variable.Name == "AZURE_ENABLE_VMSS_ETAG" && variable.Value == "true"
				}
			}
		}
		Expect(enabled).To(BeTrue(), "operator must explicitly enable ETag before selecting this supplemental case")
		growthAndDelete(ctx)
	}, NodeTimeout(50*time.Minute))
})

func readSnapshot(ctx context.Context) (environment.Snapshot, error) {
	snapshot, err := env.Read(ctx)
	if errors.Is(err, environment.ErrBounds) {
		StopTrying("observed resource bounds breach").Wrap(err).Now()
	}
	return snapshot, err
}

func readActiveSnapshot(ctx context.Context) (environment.Snapshot, error) {
	if err := env.Controller(ctx); err != nil {
		return environment.Snapshot{}, err
	}
	return readSnapshot(ctx)
}

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

func reportSnapshot(name string, snapshot environment.Snapshot) {
	nodes := map[string]string{}
	for _, node := range environment.WorkerNodes(snapshot.Nodes, env.Config) {
		nodes[node.Name] = node.Spec.ProviderID
	}
	AddReportEntry(name, struct {
		Pools           map[string]environment.PoolState
		NodeProviderIDs map[string]string
		VMs, VCPUs      int
	}{snapshot.Pools, nodes, snapshot.VMs, snapshot.VCPUs})
}

func waitWorkload(ctx context.Context, name, pool string, ready, pending int) []corev1.Pod {
	return waitWorkloadIn(ctx, namespace.Name, name, pool, ready, pending)
}

func waitWorkloadIn(ctx context.Context, workloadNamespace, name, pool string, ready, pending int) []corev1.Pod {
	var pods []corev1.Pod
	Eventually(ctx, func() error {
		var err error
		pods, err = env.WorkloadState(ctx, workloadNamespace, name, pool, ready, pending)
		return err
	}, settleTimeout, pollInterval).Should(Succeed())
	return pods
}

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

func growthAndDelete(ctx context.Context) {
	Expect(env.DemandFits(ctx, waitStable(ctx, 1, 0), env.Config.MainPool)).To(Succeed())
	deployment := env.Deployment(namespace.Name, "demand", env.Config.MainLabel, env.Config.DemandCPU, 2)
	Expect(env.K8s.Create(ctx, deployment)).To(Succeed())
	grown := waitStable(ctx, 2, 0)
	pods := waitWorkload(ctx, deployment.Name, env.Config.MainPool, 2, 0)
	Expect(pods[0].Spec.NodeName).NotTo(Equal(pods[1].Spec.NodeName))
	Expect(env.K8s.Get(ctx, client.ObjectKeyFromObject(deployment), deployment)).To(Succeed())
	deployment.Spec.Replicas = ptr.To(int32(3))
	Expect(env.K8s.Update(ctx, deployment)).To(Succeed())
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
	Expect(env.K8s.Delete(ctx, deployment)).To(Succeed())
	waitDeleted(ctx, grown, env.Config.MainPool, 1, 1, 0)
}
