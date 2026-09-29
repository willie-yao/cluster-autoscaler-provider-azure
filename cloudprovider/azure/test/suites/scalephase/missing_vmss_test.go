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
	"errors"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

var _ = Describe("Missing Azure VMSS", Serial, Label("Slow"), func() {
	It("AZ-P1-014 keeps the controller and main pool working after an empty group disappears", Label("AZ-P1-014", "missing-vmss", "Disruptive"), func(ctx SpecContext) {
		f := setup(ctx, "missing-vmss", "AZ-P1-014")
		c := f.env.Config
		initial := f.stable(ctx, map[string]int{c.MainPool: 1, c.ZeroPool: 0, c.MissingPool: 0})
		Expect(f.env.CheckWorkerIsolation(ctx, initial, f.namespace.Name)).To(Succeed())
		started := time.Now().Add(-time.Second)
		witness, err := readControllerWitness(ctx, f.env, false)
		Expect(err).NotTo(HaveOccurred())
		AddReportEntry("missing-vmss-target", c.PoolID(c.MissingPool))
		By("signaling the operator to delete the empty VMSS")
		Expect(createPhaseSignal(ctx, f, "delete-missing-vmss", map[string]string{
			"resource-id": c.PoolID(c.MissingPool), "run-id": c.RunID,
		})).To(Succeed())

		Eventually(ctx, func() error {
			snapshot, err := f.readAfterMissing(ctx)
			if err != nil {
				return err
			}
			if err := snapshot.StableAfterMissing(c, 1, 0); err != nil {
				return err
			}
			return sameController(ctx, f.env, witness, true)
		}, waitTimeout, pollInterval).Should(Succeed())

		By("growing main after the VMSS is gone")
		demand := f.env.Deployment(f.namespace.Name, "main-after-missing", c.MainLabel, c.DemandCPU, 2)
		Expect(f.env.K8s.Create(ctx, demand)).To(Succeed())
		var grown environment.Snapshot
		Eventually(ctx, func() error {
			var err error
			grown, err = f.readAfterMissing(ctx)
			if err != nil {
				return err
			}
			if err := sameController(ctx, f.env, witness, true); err != nil {
				StopTrying("controller stopped or restarted after the group was removed").Wrap(err).Now()
			}
			if err := grown.StableAfterMissing(c, 2, 0); err != nil {
				return err
			}
			pods, err := f.env.WorkloadState(ctx, f.namespace.Name, demand.Name, c.MainPool, 2, 0)
			if err != nil {
				return err
			}
			if pods[0].Spec.NodeName == pods[1].Spec.NodeName {
				return fmt.Errorf("main demand must run on separate workers after the group disappears, both run on %s", pods[0].Spec.NodeName)
			}
			return nil
		}, waitTimeout, pollInterval).Should(Succeed())
		AddReportEntry("main-after-vmss-deletion", struct {
			Pool      environment.PoolState
			NodeNames []string
		}{grown.Pools[c.MainPool], poolNodeNames(grown.Nodes, c.PoolID(c.MainPool))})

		By("removing the demand and waiting for the added main worker to be deleted")
		Expect(f.env.K8s.Delete(ctx, demand)).To(Succeed())
		Eventually(ctx, func() error {
			after, err := f.readAfterMissing(ctx)
			if err != nil {
				return err
			}
			if err := sameController(ctx, f.env, witness, true); err != nil {
				StopTrying("controller stopped or restarted during main scale-down").Wrap(err).Now()
			}
			if err := after.StableAfterMissing(c, 1, 0); err != nil {
				return err
			}
			return f.env.Deleted(ctx, grown, after, c.MainPool, 1)
		}, waitTimeout, pollInterval).Should(Succeed())
		Expect(noControllerPanic(ctx, f.env, started)).To(Succeed())
		AddReportEntry("missing-vmss-result", "one controller stayed Ready and physically scaled main after the empty VMSS was deleted")
	}, NodeTimeout(55*time.Minute))
})

// readAfterMissing works like read after the empty VMSS is deleted.
func (f *fixture) readAfterMissing(ctx context.Context) (environment.Snapshot, error) {
	snapshot, err := f.env.ReadAfterMissing(ctx)
	if errors.Is(err, environment.ErrBounds) {
		StopTrying("observed resource bounds breach").Wrap(err).Now()
	}
	return snapshot, err
}

// poolNodeNames returns the names of the Nodes of the pool with poolID.
func poolNodeNames(nodes []corev1.Node, poolID string) []string {
	selected := environment.PoolNodes(nodes, poolID)
	names := make([]string, 0, len(selected))
	for _, node := range selected {
		names = append(names, node.Name)
	}
	return names
}
