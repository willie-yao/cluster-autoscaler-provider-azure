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
	"context"
	"fmt"
	"sort"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

const (
	largeTarget      = 50
	largeStep        = 5
	largeStepTimeout = 25 * time.Minute
	largeDownTimeout = 45 * time.Minute
)

type largeNodeIdentity struct {
	Name, UID, ProviderID string
}

var _ = Describe("Large Azure VMSS", Serial, func() {
	It("AZ-P1-011 grows a small-worker pool in steps to fifty and physically returns it to zero", Label("AZ-P1-011", "large", "delete"), func(ctx SpecContext) {
		f := setup(ctx, "large", "AZ-P1-011")
		c := f.env.Config
		Expect(f.env.CheckPausedController(ctx)).To(Succeed())
		baseline := f.stable(ctx, map[string]int{c.MainPool: 1, c.ZeroPool: 0, c.ScalePool: 1})
		Expect(f.env.CheckWorkerIsolation(ctx, baseline, f.namespace.Name)).To(Succeed())
		Expect(f.env.DemandFits(ctx, baseline, c.ScalePool)).To(Succeed())

		work := f.env.Deployment(f.namespace.Name, "large-demand", c.ScaleLabel, c.DemandCPU, largeStep)
		Expect(f.env.K8s.Create(ctx, work)).To(Succeed())
		Eventually(ctx, func() error {
			_, err := f.env.WorkloadState(ctx, f.namespace.Name, work.Name, c.ScalePool, 1, largeStep-1)
			return err
		}, 8*time.Minute, pollInterval).Should(Succeed())
		Expect(f.env.CheckPausedController(ctx)).To(Succeed())
		f.signalStart(ctx)

		var grown environment.Snapshot
		for target := largeStep; target <= largeTarget; target += largeStep {
			if target != largeStep {
				Expect(f.env.K8s.Get(ctx, client.ObjectKeyFromObject(work), work)).To(Succeed())
				work.Spec.Replicas = ptr.To(int32(target))
				Expect(f.env.K8s.Update(ctx, work)).To(Succeed())
			}
			grown = waitLargeStage(ctx, f, work, target)
		}
		instances := make([]environment.Instance, 0, largeTarget)
		for _, instance := range grown.Pools[c.ScalePool].Instances {
			instances = append(instances, instance)
		}
		sort.Slice(instances, func(i, j int) bool { return instances[i].ID < instances[j].ID })
		nodes := make([]largeNodeIdentity, 0, largeTarget)
		for _, node := range environment.PoolNodes(grown.Nodes, c.PoolID(c.ScalePool)) {
			nodes = append(nodes, largeNodeIdentity{node.Name, string(node.UID), node.Spec.ProviderID})
		}
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
		AddReportEntry("large-identities", struct {
			VMs   []environment.Instance
			Nodes []largeNodeIdentity
		}{instances, nodes})

		Expect(f.env.K8s.Delete(ctx, work)).To(Succeed())
		Eventually(ctx, func() error {
			var pods corev1.PodList
			if err := f.env.K8s.List(ctx, &pods, client.InNamespace(f.namespace.Name),
				client.MatchingLabels(work.Spec.Selector.MatchLabels)); err != nil {
				return err
			}
			if len(pods.Items) != 0 {
				return fmt.Errorf("waiting for all large-pool demand Pods to be removed")
			}
			return nil
		}, 5*time.Minute, pollInterval).Should(Succeed())
		Eventually(ctx, func() error {
			after, err := f.read(ctx)
			if err != nil {
				return err
			}
			if err := after.StablePools(c, map[string]int{
				c.MainPool: 1, c.ZeroPool: 0, c.ScalePool: 0,
			}); err != nil {
				return err
			}
			return f.env.Deleted(ctx, grown, after, c.ScalePool, largeTarget)
		}, largeDownTimeout, 20*time.Second).Should(Succeed())
		AddReportEntry("large-physical-delete", "fifty captured VMs, Nodes and NICs were removed and the pool returned to zero")
	}, NodeTimeout(4*time.Hour+30*time.Minute))
})

func waitLargeStage(ctx context.Context, f *fixture, work *appsv1.Deployment, target int) environment.Snapshot {
	c := f.env.Config
	var snapshot environment.Snapshot
	Eventually(ctx, func() error {
		var err error
		snapshot, err = f.active(ctx)
		if err != nil {
			return err
		}
		pool := snapshot.Pools[c.ScalePool]
		if pool.Capacity > target || len(pool.Instances) > target {
			StopTrying("large pool grew beyond the selected step").Now()
		}
		if err := snapshot.StablePools(c, map[string]int{
			c.MainPool: 1, c.ZeroPool: 0, c.ScalePool: target,
		}); err != nil {
			return err
		}
		pods, err := f.env.WorkloadState(ctx, f.namespace.Name, work.Name, c.ScalePool, target, 0)
		if err != nil {
			return err
		}
		nodes := map[string]bool{}
		for _, pod := range pods {
			nodes[pod.Spec.NodeName] = true
		}
		if len(nodes) != target {
			return fmt.Errorf("large-pool Pods share a worker at target %d", target)
		}
		return f.env.CheckWorkerIsolation(ctx, snapshot, f.namespace.Name)
	}, largeStepTimeout, pollInterval).Should(Succeed())
	AddReportEntry("large-step", fmt.Sprintf("%d Ready small workers and %d Ready Pods", target, target))
	return snapshot
}
