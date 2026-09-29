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

package scalephase_test

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

var _ = Describe("Spot eviction", Serial, Label("Slow"), func() {
	It("AZ-P1-013 restores a Ready Spot workload after its exact VM is evicted", Label("AZ-P1-013", "spot-eviction", "delete", "Disruptive"), func(ctx SpecContext) {
		f := setup(ctx, "spot-eviction", "AZ-P1-013")
		c := f.env.Config
		baseline := f.stable(ctx, map[string]int{c.MainPool: 1, c.ZeroPool: 0, c.SpotPool: 1})
		Expect(f.env.Controller(ctx)).To(Succeed())
		Expect(f.env.CheckWorkerIsolation(ctx, baseline, f.namespace.Name)).To(Succeed())
		var original environment.Instance
		for _, instance := range baseline.Pools[c.SpotPool].Instances {
			original = instance
		}
		Expect(original.VMID).NotTo(BeEmpty())
		originalNode := environment.PoolNodes(baseline.Nodes, c.PoolID(c.SpotPool))[0]

		By("running demand on the captured Spot worker")
		work := f.env.Deployment(f.namespace.Name, "spot-eviction-demand", c.SpotLabel, "50m", 1)
		work.Spec.Template.Spec.Tolerations = []corev1.Toleration{{
			Key: "kubernetes.azure.com/scalesetpriority", Operator: corev1.TolerationOpExists,
			Effect: corev1.TaintEffectNoSchedule,
		}}
		Expect(f.env.K8s.Create(ctx, work)).To(Succeed())
		var initialPod corev1.Pod
		Eventually(ctx, func() error {
			pods, err := f.env.WorkloadState(ctx, f.namespace.Name, work.Name, c.SpotPool, 1, 0)
			if err != nil {
				return err
			}
			initialPod = pods[0]
			if initialPod.Spec.NodeName != originalNode.Name {
				return fmt.Errorf("Spot demand must start on the captured VM Node %s, got %s", originalNode.Name, initialPod.Spec.NodeName)
			}
			return nil
		}, 5*time.Minute, pollInterval).Should(Succeed())
		AddReportEntry("evicted-spot-identity", struct {
			VM      environment.Instance
			Node    string
			NodeUID string
			PodUID  string
		}{original, originalNode.Name, string(originalNode.UID), string(initialPod.UID)})

		By("signaling the operator to evict the captured Spot VM")
		Expect(createPhaseSignal(ctx, f, "evict-spot-vm", map[string]string{
			"resource-id": original.ID, "vm-id": original.VMID, "run-id": c.RunID,
		})).To(Succeed())

		By("waiting for a new Spot VM, Node and Ready Pod")
		observedSmaller := false
		var replacement environment.Instance
		Eventually(ctx, func() error {
			snapshot, err := f.active(ctx)
			if err != nil {
				return err
			}
			pool := snapshot.Pools[c.SpotPool]
			if pool.Capacity == 0 {
				observedSmaller = true
			}
			for _, instance := range pool.Instances {
				if instance.VMID != original.VMID {
					replacement = instance
				}
			}
			if replacement.VMID == "" {
				return fmt.Errorf("waiting for a new Spot VM after the eviction")
			}
			if !observedSmaller {
				return fmt.Errorf("Azure VMSS target has not yet been observed below one")
			}
			if err := snapshot.StablePools(c, map[string]int{c.MainPool: 1, c.ZeroPool: 0, c.SpotPool: 1}); err != nil {
				return err
			}
			nodes := environment.PoolNodes(snapshot.Nodes, c.PoolID(c.SpotPool))
			if len(nodes) != 1 || nodes[0].UID == originalNode.UID {
				return fmt.Errorf("waiting for a new Ready Spot Node, observed %d Spot Nodes", len(nodes))
			}
			pods, err := f.env.WorkloadState(ctx, f.namespace.Name, work.Name, c.SpotPool, 1, 0)
			if err != nil {
				return err
			}
			if pods[0].UID == initialPod.UID || pods[0].Spec.NodeName != nodes[0].Name {
				return fmt.Errorf("waiting for a replacement Ready Spot Pod")
			}
			return f.env.DeletedGenerationForNode(ctx, original, originalNode.UID, snapshot, c.SpotPool)
		}, 25*time.Minute, pollInterval).Should(Succeed())
		AddReportEntry("spot-restored-identity", replacement)

		By("removing the demand and waiting for the replacement VM to be deleted")
		Expect(f.env.K8s.Delete(ctx, work)).To(Succeed())
		Eventually(ctx, func() error {
			return workloadPodsGone(ctx, f, work)
		}, 4*time.Minute, pollInterval).Should(Succeed())
		Eventually(ctx, func() error {
			after, err := f.read(ctx)
			if err != nil {
				return err
			}
			if err := after.StablePools(c, map[string]int{c.MainPool: 1, c.ZeroPool: 0, c.SpotPool: 0}); err != nil {
				return err
			}
			return f.env.DeletedGeneration(ctx, replacement, after, c.SpotPool)
		}, waitTimeout, pollInterval).Should(Succeed())
		AddReportEntry("spot-eviction-result", "the exact evicted VM was replaced, its Pod became Ready, and the replacement physically returned to zero")
	}, NodeTimeout(65*time.Minute))
})
