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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	Expect(configPath).NotTo(BeEmpty(), "-environment is required")
	config, _ := GinkgoConfiguration()
	Expect(config.ParallelTotal).To(Equal(1), "the specs share one cluster")
	cfg, err := environment.LoadConfig(configPath)
	Expect(err).NotTo(HaveOccurred())
	env, err = environment.NewEnvironment(cfg)
	Expect(err).NotTo(HaveOccurred())
})

var _ = BeforeEach(func(ctx SpecContext) {
	Eventually(ctx, env.Controller, time.Minute, pollInterval).Should(Succeed())
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
	It("discovers only the tagged pools and stays idle", func(ctx SpecContext) {
		baseline, err := readSnapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		baselineNodes := []string{}
		for _, node := range baseline.Nodes {
			baselineNodes = append(baselineNodes, node.Name)
		}
		Consistently(ctx, func(g Gomega) {
			g.Expect(env.Controller(ctx)).To(Succeed())
			snapshot, err := readSnapshot(ctx)
			g.Expect(err).NotTo(HaveOccurred())
			for name, pool := range baseline.Pools {
				current := snapshot.Pools[name]
				g.Expect(current.Capacity).To(Equal(pool.Capacity))
				g.Expect(current.Instances).To(HaveLen(len(pool.Instances)))
				for id := range pool.Instances {
					g.Expect(current.Instances).To(HaveKey(id))
				}
			}
			currentNodes := []string{}
			for _, node := range snapshot.Nodes {
				currentNodes = append(currentNodes, node.Name)
			}
			g.Expect(currentNodes).To(ConsistOf(baselineNodes))
		}, 2*time.Minute, pollInterval).Should(Succeed())
	}, NodeTimeout(4*time.Minute))

	It("scales up AKS node pools when pending Pods exist", func(ctx SpecContext) {
		Expect(env.DemandFits(ctx, waitStable(ctx, 1, 0), env.Config.MainPool)).To(Succeed())
		By("creating demand for two main workers")
		deployment := env.Deployment(namespace.Name, "demand", env.Config.MainLabel, env.Config.DemandCPU, 2)
		Expect(env.K8s.Create(ctx, deployment)).To(Succeed())
		grown := waitStable(ctx, 2, 0)
		pods := waitWorkload(ctx, deployment.Name, env.Config.MainPool, 2, 0)
		Expect(pods[0].Spec.NodeName).NotTo(Equal(pods[1].Spec.NodeName))
		By("removing the demand and waiting for the added worker to be deleted")
		Expect(env.K8s.Delete(ctx, deployment)).To(Succeed())
		waitDeleted(ctx, grown, env.Config.MainPool, 1, 1, 0)
	}, NodeTimeout(45*time.Minute))

	It("scales a pool up from zero and deletes the VM, Node and NIC when it returns to zero", func(ctx SpecContext) {
		By("creating demand for the zero pool")
		deployment := env.Deployment(namespace.Name, "zero", env.Config.ZeroLabel, env.Config.DemandCPU, 1)
		Expect(env.K8s.Create(ctx, deployment)).To(Succeed())
		grown := waitStable(ctx, 1, 1)
		waitWorkload(ctx, deployment.Name, env.Config.ZeroPool, 1, 0)
		By("removing the demand and waiting for the zero-pool worker to be deleted")
		Expect(env.K8s.Delete(ctx, deployment)).To(Succeed())
		waitDeleted(ctx, grown, env.Config.ZeroPool, 1, 1, 0)
	}, NodeTimeout(45*time.Minute))
})

func readSnapshot(ctx context.Context) (environment.Snapshot, error) {
	snapshot, err := env.Read(ctx)
	if errors.Is(err, environment.ErrBounds) {
		StopTrying("a pool exceeded its maximum").Wrap(err).Now()
	}
	return snapshot, err
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
	AddReportEntry(fmt.Sprintf("stable-%d-%d", main, zero), snapshot)
	return snapshot
}

func waitWorkload(ctx context.Context, name, pool string, ready, pending int) []corev1.Pod {
	pods := []corev1.Pod{}
	Eventually(ctx, func() error {
		var err error
		pods, err = env.WorkloadState(ctx, namespace.Name, name, pool, ready, pending)
		return err
	}, settleTimeout, pollInterval).Should(Succeed())
	return pods
}

func waitDeleted(ctx context.Context, before environment.Snapshot, pool string, deleted, main, zero int) {
	Eventually(ctx, func() error {
		after, err := readSnapshot(ctx)
		if err != nil {
			return err
		}
		if err := after.Stable(env.Config, main, zero); err != nil {
			return err
		}
		return env.Deleted(ctx, before, after, pool, deleted)
	}, settleTimeout, pollInterval).Should(Succeed())
	AddReportEntry("physical-delete", fmt.Sprintf("pool=%s removedInstances=%d; VM, Node and NIC IDs absent", pool, deleted))
}
