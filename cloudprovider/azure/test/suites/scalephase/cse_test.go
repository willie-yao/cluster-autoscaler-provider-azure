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
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

var _ = Describe("Failed CustomScript extension", Serial, func() {
	It("AZ-P1-012 removes an unregistered VM with a failed extension while main still grows", Label("AZ-P1-012", "cse", "delete"), func(ctx SpecContext) {
		f := setup(ctx, "cse", "AZ-P1-012")
		c := f.env.Config
		baseline := f.stable(ctx, map[string]int{c.MainPool: 1, c.ZeroPool: 0, c.FailurePool: 0})
		Expect(f.env.CheckWorkerIsolation(ctx, baseline, f.namespace.Name)).To(Succeed())
		extensionName := baseline.Pools[c.FailurePool].FailedExtensionName
		witness, err := readControllerWitness(ctx, f.env, false)
		Expect(err).NotTo(HaveOccurred())
		started := time.Now().Add(-time.Second)
		AddReportEntry("max-node-provision-time", c.MaxNodeProvisionTime)

		failureDemand := f.env.Deployment(f.namespace.Name, "failed-vm-demand", c.FailureLabel, "50m", 1)
		mainDemand := f.env.Deployment(f.namespace.Name, "healthy-main-demand", c.MainLabel, c.DemandCPU, 2)
		Expect(f.env.DemandFits(ctx, baseline, c.MainPool)).To(Succeed())
		Expect(f.env.K8s.Create(ctx, failureDemand)).To(Succeed())
		Expect(f.env.K8s.Create(ctx, mainDemand)).To(Succeed())
		var failed environment.Instance
		seen := map[string]environment.Instance{}
		var mainGrown environment.Snapshot
		Eventually(ctx, func() error {
			snapshot, err := f.active(ctx)
			if err != nil {
				return err
			}
			if err := sameController(ctx, f.env, witness, false); err != nil {
				StopTrying("controller restarted while a VM extension failed").Wrap(err).Now()
			}
			if err := noControllerPanic(ctx, f.env, started); err != nil {
				StopTrying("controller logged a panic while a VM extension failed").Wrap(err).Now()
			}
			if len(environment.PoolNodes(snapshot.Nodes, c.PoolID(c.FailurePool))) != 0 {
				StopTrying("failed-extension VM unexpectedly registered as a Node").Now()
			}
			if snapshot.Pools[c.FailurePool].FailedExtensionName != extensionName {
				StopTrying("failed CustomScript extension name changed during the case").Now()
			}
			for _, instance := range snapshot.Pools[c.FailurePool].Instances {
				seen[instance.VMID] = instance
				failedAndRunning, err := f.env.FailedCustomScriptRunning(ctx, c.FailurePool, instance.ID, extensionName)
				if err != nil {
					return err
				}
				if !failedAndRunning {
					continue
				}
				if instance.ProvisioningState != "Succeeded" {
					return fmt.Errorf("VM with failed extension must report Succeeded for the unregistered cleanup path")
				}
				failed = instance
			}
			if failed.VMID == "" {
				return fmt.Errorf("waiting for a Running VM with a failed CustomScript extension")
			}
			return nil
		}, 18*time.Minute, pollInterval).Should(Succeed())
		AddReportEntry("failed-extension-vm", failed)

		observedAt := time.Now()
		samples := 0
		Consistently(ctx, func() error {
			snapshot, err := f.active(ctx)
			if err != nil {
				return err
			}
			if err := sameController(ctx, f.env, witness, false); err != nil {
				return err
			}
			if err := noControllerPanic(ctx, f.env, started); err != nil {
				return err
			}
			if len(environment.PoolNodes(snapshot.Nodes, c.PoolID(c.FailurePool))) != 0 {
				return fmt.Errorf("VM with failed extension registered as a Node")
			}
			instance, present := snapshot.Pools[c.FailurePool].Instances[failed.ID]
			if !present || instance.VMID != failed.VMID || instance.ProvisioningState != "Succeeded" ||
				snapshot.Pools[c.FailurePool].FailedExtensionName != extensionName {
				return fmt.Errorf("VM with failed extension did not stay present and Succeeded across controller scans")
			}
			failedAndRunning, err := f.env.FailedCustomScriptRunning(ctx, c.FailurePool, failed.ID, extensionName)
			if err != nil {
				return err
			}
			if !failedAndRunning {
				return fmt.Errorf("VM did not stay Running with a failed CustomScript extension")
			}
			samples++
			return nil
		}, 75*time.Second, pollInterval).Should(Succeed())
		Expect(samples).To(BeNumerically(">=", 4), "the VM with a failed extension needs at least four separate observations")
		logs, err := f.env.ReadControllerLogsSince(ctx, observedAt)
		Expect(err).NotTo(HaveOccurred())
		scans := controllerScans(logs)
		Expect(scans).To(BeNumerically(">=", 4), "the controller must scan four times while the failed-extension VM stays present")
		AddReportEntry("failed-extension-observation", fmt.Sprintf("%d VM observations and %d controller scans without a restart or panic", samples, scans))

		Eventually(ctx, func() error {
			snapshot, err := f.active(ctx)
			if err != nil {
				return err
			}
			if err := sameController(ctx, f.env, witness, false); err != nil {
				StopTrying("controller restarted instead of growing the healthy pool").Wrap(err).Now()
			}
			for _, instance := range snapshot.Pools[c.FailurePool].Instances {
				seen[instance.VMID] = instance
			}
			main := snapshot.Pools[c.MainPool]
			if main.Capacity != 2 || len(main.Instances) != 2 ||
				len(environment.PoolNodes(snapshot.Nodes, c.PoolID(c.MainPool))) != 2 {
				return fmt.Errorf("main has not grown to two workers despite separate demand")
			}
			_, err = f.env.WorkloadState(ctx, f.namespace.Name, mainDemand.Name, c.MainPool, 2, 0)
			if err == nil {
				mainGrown = snapshot
			}
			return err
		}, waitTimeout, pollInterval).Should(Succeed())

		Eventually(ctx, func() error {
			snapshot, err := f.active(ctx)
			if err != nil {
				return err
			}
			if err := sameController(ctx, f.env, witness, false); err != nil {
				StopTrying("controller restarted before removing the failed VM").Wrap(err).Now()
			}
			for _, instance := range snapshot.Pools[c.FailurePool].Instances {
				seen[instance.VMID] = instance
			}
			if len(environment.PoolNodes(snapshot.Nodes, c.PoolID(c.FailurePool))) != 0 {
				StopTrying("VM with failed extension unexpectedly registered as a Node").Now()
			}
			var events corev1.EventList
			if err := f.env.K8s.List(ctx, &events, client.InNamespace(c.AutoscalerNamespace)); err != nil {
				return err
			}
			if !unregisteredDeletionEvent(events.Items, failed.ID) {
				return fmt.Errorf("waiting for autoscaler unregistered-node deletion")
			}
			return f.env.DeletedGeneration(ctx, failed, snapshot, c.FailurePool)
		}, 22*time.Minute, pollInterval).Should(Succeed())
		Expect(noControllerPanic(ctx, f.env, started)).To(Succeed())
		AddReportEntry("failed-extension-path", "the Running VM reported Succeeded and was removed as unregistered, so fast delete was not exercised")

		Expect(f.env.K8s.Delete(ctx, failureDemand)).To(Succeed())
		Eventually(ctx, func() error {
			snapshot, err := f.active(ctx)
			if err != nil {
				return err
			}
			for _, instance := range snapshot.Pools[c.FailurePool].Instances {
				seen[instance.VMID] = instance
			}
			if len(environment.PoolNodes(snapshot.Nodes, c.PoolID(c.FailurePool))) != 0 {
				StopTrying("VM with failed extension registered after demand was removed").Now()
			}
			return workloadPodsGone(ctx, f, failureDemand)
		}, 4*time.Minute, pollInterval).Should(Succeed())
		Eventually(ctx, func() error {
			snapshot, err := f.active(ctx)
			if err != nil {
				return err
			}
			if err := sameController(ctx, f.env, witness, false); err != nil {
				StopTrying("controller restarted during failed-extension cleanup").Wrap(err).Now()
			}
			for _, instance := range snapshot.Pools[c.FailurePool].Instances {
				seen[instance.VMID] = instance
			}
			if snapshot.Pools[c.FailurePool].Capacity != 0 || len(snapshot.Pools[c.FailurePool].Instances) != 0 {
				return fmt.Errorf("failed-extension pool has not returned to zero")
			}
			for _, instance := range seen {
				if err := f.env.DeletedGeneration(ctx, instance, snapshot, c.FailurePool); err != nil {
					return err
				}
			}
			return nil
		}, 25*time.Minute, pollInterval).Should(Succeed())

		Expect(f.env.K8s.Delete(ctx, mainDemand)).To(Succeed())
		Eventually(ctx, func() error {
			snapshot, err := f.active(ctx)
			if err != nil {
				return err
			}
			if err := sameController(ctx, f.env, witness, false); err != nil {
				StopTrying("controller restarted during main scale-down").Wrap(err).Now()
			}
			if err := snapshot.StablePools(c, map[string]int{
				c.MainPool: 1, c.ZeroPool: 0, c.FailurePool: 0,
			}); err != nil {
				return err
			}
			return f.env.Deleted(ctx, mainGrown, snapshot, c.MainPool, 1)
		}, waitTimeout, pollInterval).Should(Succeed())
		Expect(noControllerPanic(ctx, f.env, started)).To(Succeed())
		AddReportEntry("failed-extension-result", fmt.Sprintf("%d captured VMs with failed extensions and their NICs were removed without restarting CA; main scaled independently", len(seen)))
	}, NodeTimeout(90*time.Minute))
})

func controllerScans(logs string) int {
	seen := map[string]bool{}
	for _, line := range strings.Split(logs, "\n") {
		_, suffix, found := strings.Cut(line, `iterationId="`)
		if !found {
			continue
		}
		id, _, found := strings.Cut(suffix, `"`)
		if found && id != "" {
			seen[id] = true
		}
	}
	return len(seen)
}

func TestControllerScans(t *testing.T) {
	t.Parallel()
	logs := "iterationId=\"one\" work\niterationId=\"one\" more\n" +
		"iterationId=\"two\" work\niterationId=\"three\" work\niterationId=\"four\" work\n" +
		"iterationId=\"\" work\nother work"
	if got := controllerScans(logs); got != 4 {
		t.Fatalf("controllerScans = %d, want four unique scans", got)
	}
}
