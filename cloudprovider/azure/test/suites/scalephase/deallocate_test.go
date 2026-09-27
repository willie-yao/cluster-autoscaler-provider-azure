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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

type parkedWorker struct {
	instance environment.Instance
	node     corev1.Node
}

var _ = Describe("Provider-only deallocation", Serial, func() {
	It("AZ-P3-001 parks and starts the same worker for new demand", Label("AZ-P3-001", "deallocate"), func(ctx SpecContext) {
		f := setup(ctx, "deallocate", "AZ-P3-001")
		anchor, demand, worker := growDeallocateWorker(ctx, f)
		parkDeallocateWorker(ctx, f, demand, worker)
		hold, err := deallocateHold(f.env.Config)
		Expect(err).NotTo(HaveOccurred())
		Consistently(ctx, func() error {
			_, err := parkedState(ctx, f, worker)
			return err
		}, hold, pollInterval).Should(Succeed())
		AddReportEntry("parked-worker", map[string]string{
			"vmID": worker.instance.VMID, "diskID": worker.instance.OSDiskID,
			"nodeUID": string(worker.node.UID), "bootID": worker.node.Status.NodeInfo.BootID,
		})

		started := time.Now().Add(-time.Second)
		Expect(setDeallocateDemand(ctx, f, demand, 1)).To(Succeed())
		var reused environment.Snapshot
		var newNode corev1.Node
		Eventually(ctx, func() error {
			var err error
			reused, err = f.active(ctx)
			if err != nil {
				return err
			}
			if err := reused.StablePools(f.env.Config, map[string]int{f.env.Config.MainPool: 2, f.env.Config.ZeroPool: 0}); err != nil {
				return err
			}
			if err := sameWorker(reused, f.env.Config.MainPool, worker); err != nil {
				return err
			}
			newNode = corev1.Node{}
			nodes := environment.PoolNodes(reused.Nodes, f.env.Config.PoolID(f.env.Config.MainPool))
			for _, node := range nodes {
				if strings.EqualFold(node.Spec.ProviderID, "azure://"+worker.instance.ID) {
					newNode = node
				}
			}
			if newNode.UID == "" || newNode.UID == worker.node.UID ||
				newNode.Status.NodeInfo.BootID == "" || newNode.Status.NodeInfo.BootID == worker.node.Status.NodeInfo.BootID ||
				newNode.Spec.PodCIDR == "" || !environment.Ready(newNode) {
				return fmt.Errorf("waiting for the retained VM to register a new Ready Node and PodCIDR after Start")
			}
			_, err = f.env.WorkloadState(ctx, f.namespace.Name, demand.Name, f.env.Config.MainPool, 1, 0)
			return err
		}, 20*time.Minute, pollInterval).Should(Succeed())
		Expect(retainedDisk(ctx, f, worker)).To(Succeed())
		Eventually(ctx, func() error {
			logs, err := f.env.ReadControllerLogsSince(ctx, started)
			if err != nil {
				return err
			}
			if !strings.Contains(strings.ToLower(logs), "start("+worker.instance.ID+")") {
				return fmt.Errorf("no provider Start recorded for retained VM %s", worker.instance.ID)
			}
			return nil
		}, 2*time.Minute, pollInterval).Should(Succeed())
		Expect(probeDeallocateNetwork(ctx, f)).To(Succeed())
		AddReportEntry("reused-worker", map[string]string{
			"vmID": worker.instance.VMID, "diskID": worker.instance.OSDiskID,
			"oldNodeUID": string(worker.node.UID), "newNodeUID": string(newNode.UID),
			"oldBootID": worker.node.Status.NodeInfo.BootID, "newBootID": newNode.Status.NodeInfo.BootID,
		})
		Expect(f.env.K8s.Delete(ctx, anchor)).To(Succeed())
	}, NodeTimeout(75*time.Minute))

	It("AZ-P3-002 retains a worker that fails to register after Start", Label("AZ-P3-002", "deallocate-failed"), func(ctx SpecContext) {
		f := setup(ctx, "deallocate-failed", "AZ-P3-002")
		_, demand, worker := growDeallocateWorker(ctx, f)
		Expect(createPhaseSignal(ctx, f, "arm-kubelet-fault", map[string]string{
			"run-id": f.env.Config.RunID, "resource-id": worker.instance.ID,
			"vm-id": worker.instance.VMID, "disk-id": worker.instance.OSDiskID,
			"boot-id": worker.node.Status.NodeInfo.BootID,
		})).To(Succeed())
		Eventually(ctx, func() error {
			return operatorFaultReceipt(ctx, f, "fault-armed", worker, worker.node.Status.NodeInfo.BootID)
		}, 15*time.Minute, pollInterval).Should(Succeed())
		current := f.stable(ctx, map[string]int{f.env.Config.MainPool: 2, f.env.Config.ZeroPool: 0})
		Expect(sameWorker(current, f.env.Config.MainPool, worker)).To(Succeed())
		for _, node := range environment.PoolNodes(current.Nodes, f.env.Config.PoolID(f.env.Config.MainPool)) {
			if strings.EqualFold(node.Spec.ProviderID, "azure://"+worker.instance.ID) {
				Expect(node.UID).To(Equal(worker.node.UID))
				Expect(node.Status.NodeInfo.BootID).To(Equal(worker.node.Status.NodeInfo.BootID))
			}
		}
		parkDeallocateWorker(ctx, f, demand, worker)

		started := time.Now().Add(-time.Second)
		Expect(setDeallocateDemand(ctx, f, demand, 1)).To(Succeed())
		var faultBootID string
		Eventually(ctx, func() error {
			logs, err := f.env.ReadControllerLogsSince(ctx, started)
			if err != nil {
				return err
			}
			if !strings.Contains(strings.ToLower(logs), "start("+worker.instance.ID+")") {
				return fmt.Errorf("waiting for provider Start of the parked VM")
			}
			return nil
		}, 15*time.Minute, pollInterval).Should(Succeed())
		Eventually(ctx, func() error {
			var receipt corev1.ConfigMap
			err := f.env.K8s.Get(ctx, client.ObjectKey{Namespace: f.namespace.Name, Name: "fault-observed"}, &receipt)
			if err != nil {
				return err
			}
			boot := receipt.Data["boot-id"]
			if boot == "" || boot == worker.node.Status.NodeInfo.BootID {
				return fmt.Errorf("operator must report the new fault boot ID")
			}
			if err := operatorFaultReceipt(ctx, f, "fault-observed", worker, boot); err != nil {
				return err
			}
			faultBootID = boot
			snapshot, err := f.active(ctx)
			if err != nil {
				return err
			}
			if err := sameWorker(snapshot, f.env.Config.MainPool, worker); err != nil {
				return err
			}
			for _, node := range environment.PoolNodes(snapshot.Nodes, f.env.Config.PoolID(f.env.Config.MainPool)) {
				if strings.EqualFold(node.Spec.ProviderID, "azure://"+worker.instance.ID) {
					StopTrying("faulted worker registered as a Node").Now()
				}
			}
			state, err := f.env.InstancePowerState(ctx, f.env.Config.MainPool, worker.instance.ID)
			if err != nil {
				return err
			}
			if state != "running" {
				return fmt.Errorf("waiting for a running worker with no Node, got %s", state)
			}
			_, err = f.env.WorkloadState(ctx, f.namespace.Name, demand.Name, f.env.Config.MainPool, 0, 1)
			return err
		}, 10*time.Minute, pollInterval).Should(Succeed())
		var final environment.Snapshot
		Eventually(ctx, func() error {
			var err error
			final, err = f.active(ctx)
			if err != nil {
				return err
			}
			if err := sameWorker(final, f.env.Config.MainPool, worker); err != nil {
				return err
			}
			for _, node := range environment.PoolNodes(final.Nodes, f.env.Config.PoolID(f.env.Config.MainPool)) {
				if strings.EqualFold(node.Spec.ProviderID, "azure://"+worker.instance.ID) {
					StopTrying("faulted worker registered as a Node").Now()
				}
			}
			state, err := f.env.InstancePowerState(ctx, f.env.Config.MainPool, worker.instance.ID)
			if err != nil {
				return err
			}
			if state != "deallocated" {
				return fmt.Errorf("waiting for failed registration cleanup to deallocate VM, got %s", state)
			}
			if err := retainedDisk(ctx, f, worker); err != nil {
				return err
			}
			logs, err := f.env.ReadControllerLogsSince(ctx, started)
			if err != nil {
				return err
			}
			if !strings.Contains(strings.ToLower(logs), "synthetic deallocate("+worker.instance.ID+")") {
				return fmt.Errorf("core has not triggered synthetic Deallocate for the same VM")
			}
			return nil
		}, 30*time.Minute, pollInterval).Should(Succeed())
		AddReportEntry("failed-registration-retained", map[string]string{
			"vmID": worker.instance.VMID, "diskID": worker.instance.OSDiskID,
			"oldNodeUID": string(worker.node.UID), "oldBootID": worker.node.Status.NodeInfo.BootID,
			"faultBootID":      faultBootID,
			"physicalCapacity": fmt.Sprint(final.Pools[f.env.Config.MainPool].Capacity),
		})
	}, NodeTimeout(95*time.Minute))
})

func deallocateHold(c environment.Config) (time.Duration, error) {
	if c.DeallocateHold == "" {
		return time.Minute, nil
	}
	return time.ParseDuration(c.DeallocateHold)
}

func setDeallocateDemand(ctx context.Context, f *fixture, demand *appsv1.Deployment, replicas int32) error {
	uid := demand.UID
	if err := f.env.K8s.Get(ctx, client.ObjectKeyFromObject(demand), demand); err != nil {
		return err
	}
	if uid != demand.UID || demand.Labels[environment.RunLabel] != f.env.Config.RunID {
		return fmt.Errorf("demand Deployment identity changed")
	}
	demand.Spec.Replicas = ptr.To(replicas)
	return f.env.K8s.Update(ctx, demand)
}

func growDeallocateWorker(ctx SpecContext, f *fixture) (*appsv1.Deployment, *appsv1.Deployment, parkedWorker) {
	c := f.env.Config
	Expect(f.env.Controller(ctx)).To(Succeed())
	baseline := f.stable(ctx, map[string]int{c.MainPool: 1, c.ZeroPool: 0})
	Expect(f.env.CheckWorkerIsolation(ctx, baseline, f.namespace.Name)).To(Succeed())
	anchor := f.env.Deployment(f.namespace.Name, "deallocate-anchor", c.MainLabel, "1300m", 1)
	Expect(f.env.K8s.Create(ctx, anchor)).To(Succeed())
	Eventually(ctx, func() error {
		_, err := f.env.WorkloadState(ctx, f.namespace.Name, anchor.Name, c.MainPool, 1, 0)
		return err
	}, 5*time.Minute, pollInterval).Should(Succeed())
	demand := f.env.Deployment(f.namespace.Name, "deallocate-demand", c.MainLabel, c.DemandCPU, 1)
	Expect(f.env.K8s.Create(ctx, demand)).To(Succeed())
	var grown environment.Snapshot
	Eventually(ctx, func() error {
		var err error
		grown, err = f.active(ctx)
		if err != nil {
			return err
		}
		if err := grown.StablePools(c, map[string]int{c.MainPool: 2, c.ZeroPool: 0}); err != nil {
			return err
		}
		pods, err := f.env.WorkloadState(ctx, f.namespace.Name, demand.Name, c.MainPool, 1, 0)
		if err != nil {
			return err
		}
		original := environment.PoolNodes(baseline.Nodes, c.PoolID(c.MainPool))[0]
		if pods[0].Spec.NodeName == original.Name {
			return fmt.Errorf("demand scheduled on the original worker")
		}
		return nil
	}, 20*time.Minute, pollInterval).Should(Succeed())
	original := environment.PoolNodes(baseline.Nodes, c.PoolID(c.MainPool))[0]
	for _, node := range environment.PoolNodes(grown.Nodes, c.PoolID(c.MainPool)) {
		if node.Name != original.Name {
			instance, ok := grown.Pools[c.MainPool].Instances[environmentID(node.Spec.ProviderID)]
			Expect(ok).To(BeTrue(), "new Node must match its Azure VM")
			Expect(instance.VMID).NotTo(BeEmpty())
			Expect(instance.OSDiskID).NotTo(BeEmpty())
			Expect(node.Status.NodeInfo.BootID).NotTo(BeEmpty())
			return anchor, demand, parkedWorker{instance: instance, node: node}
		}
	}
	Fail("no second worker registered")
	return nil, nil, parkedWorker{}
}

func environmentID(providerID string) string {
	return strings.ToLower(strings.TrimPrefix(providerID, "azure://"))
}

func parkDeallocateWorker(ctx SpecContext, f *fixture, demand *appsv1.Deployment, worker parkedWorker) environment.Snapshot {
	Expect(setDeallocateDemand(ctx, f, demand, 0)).To(Succeed())
	Eventually(ctx, func() error { return workloadPodsGone(ctx, f, demand) }, 5*time.Minute, pollInterval).Should(Succeed())
	var parked environment.Snapshot
	Eventually(ctx, func() error {
		var err error
		parked, err = f.active(ctx)
		if err != nil {
			return err
		}
		return parkedStateFromSnapshot(ctx, f, worker, parked)
	}, 20*time.Minute, pollInterval).Should(Succeed())
	return parked
}

func parkedState(ctx context.Context, f *fixture, worker parkedWorker) (environment.Snapshot, error) {
	current, err := f.active(ctx)
	if err != nil {
		return current, err
	}
	return current, parkedStateFromSnapshot(ctx, f, worker, current)
}

func parkedStateFromSnapshot(ctx context.Context, f *fixture, worker parkedWorker, snapshot environment.Snapshot) error {
	c := f.env.Config
	if err := sameWorker(snapshot, c.MainPool, worker); err != nil {
		return err
	}
	pool := snapshot.Pools[c.MainPool]
	if pool.Capacity != 2 || len(pool.Instances) != 2 ||
		len(environment.PoolNodes(snapshot.Nodes, c.PoolID(c.MainPool))) != 1 {
		return fmt.Errorf("waiting for one Ready worker and one retained VM")
	}
	var oldNode corev1.Node
	err := f.env.K8s.Get(ctx, client.ObjectKey{Name: worker.node.Name}, &oldNode)
	if !apierrors.IsNotFound(err) {
		if err != nil {
			return err
		}
		return fmt.Errorf("old Node %s still exists", worker.node.Name)
	}
	state, err := f.env.InstancePowerState(ctx, c.MainPool, worker.instance.ID)
	if err != nil {
		return err
	}
	if state != "deallocated" {
		return fmt.Errorf("retained worker power state is %s, want deallocated", state)
	}
	return retainedDisk(ctx, f, worker)
}

func retainedDisk(ctx context.Context, f *fixture, worker parkedWorker) error {
	exists, err := f.env.DiskExists(ctx, worker.instance.OSDiskID)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("retained VM OS disk %s is absent", worker.instance.OSDiskID)
	}
	return nil
}

func sameWorker(snapshot environment.Snapshot, pool string, before parkedWorker) error {
	current, ok := snapshot.Pools[pool].Instances[before.instance.ID]
	if !ok || !strings.EqualFold(current.VMID, before.instance.VMID) ||
		!strings.EqualFold(current.OSDiskID, before.instance.OSDiskID) ||
		current.ProvisioningState != "Succeeded" {
		return fmt.Errorf("VM or full OS disk identity changed for %s", before.instance.ID)
	}
	return nil
}

func operatorFaultReceipt(ctx context.Context, f *fixture, name string, worker parkedWorker, bootID string) error {
	var receipt corev1.ConfigMap
	if err := f.env.K8s.Get(ctx, client.ObjectKey{Namespace: f.namespace.Name, Name: name}, &receipt); err != nil {
		return err
	}
	for key, expected := range map[string]string{
		"run-id": f.env.Config.RunID, "resource-id": worker.instance.ID,
		"vm-id": worker.instance.VMID, "disk-id": worker.instance.OSDiskID, "boot-id": bootID,
	} {
		if receipt.Data[key] != expected || receipt.Labels[environment.RunLabel] != f.env.Config.RunID {
			return fmt.Errorf("operator %s receipt does not match %s or run tag", name, key)
		}
	}
	return nil
}

func probeDeallocateNetwork(ctx SpecContext, f *fixture) error {
	c := f.env.Config
	probes := f.env.Deployment(f.namespace.Name, "deallocate-network", c.MainLabel, "50m", 2)
	probes.Spec.Template.Spec.Containers[0].Command = []string{"sh", "-c", "mkdir -p /www; hostname >/www/index.html; exec httpd -f -p 8080 -h /www"}
	probes.Spec.Template.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			TopologyKey: "kubernetes.io/hostname", LabelSelector: probes.Spec.Selector.DeepCopy(),
		}},
	}}
	if err := f.env.K8s.Create(ctx, probes); err != nil {
		return err
	}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "deallocate-network", Namespace: f.namespace.Name,
		Labels: map[string]string{environment.RunLabel: c.RunID}}, Spec: corev1.ServiceSpec{
		Selector: probes.Spec.Selector.MatchLabels,
		Ports:    []corev1.ServicePort{{Port: 8080}},
	}}
	if err := f.env.K8s.Create(ctx, service); err != nil {
		return err
	}
	var pods []corev1.Pod
	Eventually(ctx, func() error {
		var err error
		pods, err = f.env.WorkloadState(ctx, f.namespace.Name, probes.Name, c.MainPool, 2, 0)
		if err != nil {
			return err
		}
		if pods[0].Spec.NodeName == pods[1].Spec.NodeName || pods[0].Status.PodIP == "" || pods[1].Status.PodIP == "" {
			return fmt.Errorf("waiting for one network probe per worker")
		}
		return nil
	}, 5*time.Minute, pollInterval).Should(Succeed())
	for i, pod := range pods {
		other := pods[1-i]
		Eventually(ctx, func() error {
			return f.env.CheckNetworkProbe(ctx, f.namespace.Name, pod.Name, other.Status.PodIP, other.Name)
		}, 3*time.Minute, pollInterval).Should(Succeed())
		Eventually(ctx, func() error {
			return f.env.CheckNetworkProbe(ctx, f.namespace.Name, pod.Name,
				service.Name+"."+f.namespace.Name+".svc.cluster.local", pod.Name, other.Name)
		}, 3*time.Minute, pollInterval).Should(Succeed())
	}
	return nil
}
