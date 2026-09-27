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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

// The provider logs these at V(3) with the VM resource ID when it waits for a
// Start or for the cleanup of a VM that never registered.
const (
	startLogPrefix               = "start("
	syntheticDeallocateLogPrefix = "synthetic deallocate("
)

// parkedWorker is the VMSS VM that a case parks and its Node before parking.
type parkedWorker struct {
	instance environment.Instance
	node     corev1.Node
}

var _ = Describe("Provider-only deallocation", Serial, Label("Slow"), func() {
	It("AZ-P3-001 parks and starts the same worker for new demand", Label("AZ-P3-001", "deallocate"), func(ctx SpecContext) {
		f := setup(ctx, "deallocate", "AZ-P3-001")
		anchor, demand, worker := growDeallocateWorker(ctx, f)
		By("checking Pod traffic between the two workers before parking")
		Expect(probeDeallocateNetwork(ctx, f)).To(Succeed())
		By("writing a marker token to the second worker's disk")
		tokenBytes := make([]byte, 16)
		_, err := rand.Read(tokenBytes)
		Expect(err).NotTo(HaveOccurred())
		token := hex.EncodeToString(tokenBytes)
		runDeallocateMarker(ctx, f, worker.node, token, true)
		parkDeallocateWorker(ctx, f, demand, worker)
		hold, err := deallocateHold(f.env.Config)
		Expect(err).NotTo(HaveOccurred())
		By(fmt.Sprintf("checking that the worker stays parked for %s", hold))
		Consistently(ctx, func() error {
			return parkedState(ctx, f, worker)
		}, hold, pollInterval).Should(Succeed())
		AddReportEntry("parked-worker", map[string]string{
			"instanceID": worker.instance.ID, "vmID": worker.instance.VMID,
			"diskID": worker.instance.OSDiskID, "diskName": worker.instance.OSDiskName,
			"nodeUID": string(worker.node.UID), "bootID": worker.node.Status.NodeInfo.BootID,
		})

		By("restoring demand and waiting for the parked VM to register a new Node")
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
				return fmt.Errorf("waiting for the retained VM to register a new Ready Node with a PodCIDR after Start: "+
					"node %q UID %q boot ID %q PodCIDR %q ready %t",
					newNode.Name, newNode.UID, newNode.Status.NodeInfo.BootID, newNode.Spec.PodCIDR, environment.Ready(newNode))
			}
			_, err = f.env.WorkloadState(ctx, f.namespace.Name, demand.Name, f.env.Config.MainPool, 1, 0)
			return err
		}, 20*time.Minute, pollInterval).Should(Succeed())
		Eventually(ctx, func() error {
			return controllerLogged(ctx, f, started, startLogPrefix, worker.instance.ID)
		}, 2*time.Minute, pollInterval).Should(Succeed())
		By("reading the marker token and checking Pod traffic after Start")
		runDeallocateMarker(ctx, f, newNode, token, false)
		Expect(probeDeallocateNetwork(ctx, f)).To(Succeed())
		AddReportEntry("reused-worker", map[string]string{
			"instanceID": worker.instance.ID, "vmID": worker.instance.VMID,
			"diskID": worker.instance.OSDiskID, "diskName": worker.instance.OSDiskName,
			"oldNodeUID": string(worker.node.UID), "newNodeUID": string(newNode.UID),
			"oldBootID": worker.node.Status.NodeInfo.BootID, "newBootID": newNode.Status.NodeInfo.BootID,
		})
		Expect(f.env.K8s.Delete(ctx, anchor)).To(Succeed())
	}, NodeTimeout(75*time.Minute))

	It("AZ-P3-002 retains a worker that fails to register after Start", Label("AZ-P3-002", "deallocate-failed", "Disruptive"), func(ctx SpecContext) {
		f := setup(ctx, "deallocate-failed", "AZ-P3-002")
		_, demand, worker := growDeallocateWorker(ctx, f)
		By("signaling the operator to arm a kubelet fault on the second worker")
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

		By("restoring demand and waiting for the operator to report the blocked kubelet")
		started := time.Now().Add(-time.Second)
		Expect(setDeallocateDemand(ctx, f, demand, 1)).To(Succeed())
		var faultBootID string
		Eventually(ctx, func() error {
			return controllerLogged(ctx, f, started, startLogPrefix, worker.instance.ID)
		}, 15*time.Minute, pollInterval).Should(Succeed())
		Eventually(ctx, func() error {
			var receipt corev1.ConfigMap
			err := f.env.K8s.Get(ctx, client.ObjectKey{Namespace: f.namespace.Name, Name: "fault-observed"}, &receipt)
			if err != nil {
				return err
			}
			boot := receipt.Data["boot-id"]
			if boot == "" || boot == worker.node.Status.NodeInfo.BootID {
				return fmt.Errorf("operator must report the new fault boot ID, got %q, old %q", boot, worker.node.Status.NodeInfo.BootID)
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
		By("waiting for the core's unregistered cleanup to deallocate the worker")
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
			return controllerLogged(ctx, f, started, syntheticDeallocateLogPrefix, worker.instance.ID)
		}, 30*time.Minute, pollInterval).Should(Succeed())
		AddReportEntry("failed-registration-retained", map[string]string{
			"instanceID": worker.instance.ID, "vmID": worker.instance.VMID,
			"diskID": worker.instance.OSDiskID, "diskName": worker.instance.OSDiskName,
			"oldNodeUID": string(worker.node.UID), "oldBootID": worker.node.Status.NodeInfo.BootID,
			"faultBootID":      faultBootID,
			"physicalCapacity": fmt.Sprint(final.Pools[f.env.Config.MainPool].Capacity),
		})
	}, NodeTimeout(95*time.Minute))
})

// controllerLogged checks that the controller logged prefix with resourceID since the given time.
func controllerLogged(ctx context.Context, f *fixture, since time.Time, prefix, resourceID string) error {
	logs, err := f.env.ReadControllerLogsSince(ctx, since)
	if err != nil {
		return err
	}
	want := strings.ToLower(prefix + resourceID + ")")
	if !strings.Contains(strings.ToLower(logs), want) {
		return fmt.Errorf("controller logs since %s do not contain %q", since.Format(time.RFC3339), want)
	}
	return nil
}

// deallocateHold returns how long AZ-P3-001 keeps the worker parked.
func deallocateHold(c environment.Config) (time.Duration, error) {
	if c.DeallocateHold == "" {
		return time.Minute, nil
	}
	return time.ParseDuration(c.DeallocateHold)
}

// setDeallocateDemand scales the demand Deployment to replicas after checking
// that it is still the run's object.
func setDeallocateDemand(ctx context.Context, f *fixture, demand *appsv1.Deployment, replicas int32) error {
	uid := demand.UID
	if err := f.env.K8s.Get(ctx, client.ObjectKeyFromObject(demand), demand); err != nil {
		return fmt.Errorf("get demand Deployment: %w", err)
	}
	if uid != demand.UID || demand.Labels[environment.RunLabel] != f.env.Config.RunID {
		return fmt.Errorf("demand Deployment identity changed, UID %s want %s, run label %q",
			demand.UID, uid, demand.Labels[environment.RunLabel])
	}
	original := demand.DeepCopy()
	demand.Spec.Replicas = ptr.To(replicas)
	return f.env.K8s.Patch(ctx, demand, client.MergeFrom(original))
}

// growDeallocateWorker keeps the first main worker busy with an anchor Pod and
// grows main to a second worker for one demand Pod. It returns the anchor and
// demand Deployments and the second worker.
func growDeallocateWorker(ctx context.Context, f *fixture) (anchor, demand *appsv1.Deployment, worker parkedWorker) {
	c := f.env.Config
	Expect(f.env.Controller(ctx)).To(Succeed())
	baseline := f.stable(ctx, map[string]int{c.MainPool: 1, c.ZeroPool: 0})
	Expect(f.env.CheckWorkerIsolation(ctx, baseline, f.namespace.Name)).To(Succeed())
	By("growing main to a second worker for one demand Pod")
	anchor = f.env.Deployment(f.namespace.Name, "deallocate-anchor", c.MainLabel, "1300m", 1)
	Expect(f.env.K8s.Create(ctx, anchor)).To(Succeed())
	Eventually(ctx, func() error {
		_, err := f.env.WorkloadState(ctx, f.namespace.Name, anchor.Name, c.MainPool, 1, 0)
		return err
	}, 5*time.Minute, pollInterval).Should(Succeed())
	demand = f.env.Deployment(f.namespace.Name, "deallocate-demand", c.MainLabel, c.DemandCPU, 1)
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
			return fmt.Errorf("demand Pod %s scheduled on the original worker %s", pods[0].Name, original.Name)
		}
		return nil
	}, 20*time.Minute, pollInterval).Should(Succeed())
	original := environment.PoolNodes(baseline.Nodes, c.PoolID(c.MainPool))[0]
	for _, node := range environment.PoolNodes(grown.Nodes, c.PoolID(c.MainPool)) {
		if node.Name != original.Name {
			instance, ok := grown.Pools[c.MainPool].Instances[nodeProviderID(node)]
			Expect(ok).To(BeTrue(), "new Node must match its Azure VM")
			Expect(instance.VMID).NotTo(BeEmpty())
			Expect(instance.OSDiskID).NotTo(BeEmpty())
			Expect(instance.OSDiskName).NotTo(BeEmpty())
			Expect(node.Status.NodeInfo.BootID).NotTo(BeEmpty())
			AddReportEntry("deallocate-worker-before", map[string]string{
				"instanceID": instance.ID, "vmID": instance.VMID,
				"diskID": instance.OSDiskID, "diskName": instance.OSDiskName,
				"nodeUID": string(node.UID), "bootID": node.Status.NodeInfo.BootID,
			})
			return anchor, demand, parkedWorker{instance: instance, node: node}
		}
	}
	Fail("no second worker registered")
	return nil, nil, parkedWorker{}
}

// parkDeallocateWorker removes the demand and waits until the provider parks
// worker: its VM is deallocated with the same OS disk and its Node is gone.
func parkDeallocateWorker(ctx context.Context, f *fixture, demand *appsv1.Deployment, worker parkedWorker) {
	By("removing the demand and waiting for the second worker to be parked")
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
	current := parked.Pools[f.env.Config.MainPool].Instances[worker.instance.ID]
	AddReportEntry("deallocate-worker-parked", map[string]string{
		"instanceID": current.ID, "vmID": current.VMID,
		"diskID": current.OSDiskID, "diskName": current.OSDiskName,
		"oldNodeUID": string(worker.node.UID), "oldBootID": worker.node.Status.NodeInfo.BootID,
	})
}

// parkedState checks the running controller and then that worker is parked.
func parkedState(ctx context.Context, f *fixture, worker parkedWorker) error {
	current, err := f.active(ctx)
	if err != nil {
		return err
	}
	return parkedStateFromSnapshot(ctx, f, worker, current)
}

// parkedStateFromSnapshot requires main to hold one Ready worker and the
// retained, deallocated VM of worker, whose old Node is deleted.
func parkedStateFromSnapshot(ctx context.Context, f *fixture, worker parkedWorker, snapshot environment.Snapshot) error {
	c := f.env.Config
	if err := sameWorker(snapshot, c.MainPool, worker); err != nil {
		return err
	}
	pool := snapshot.Pools[c.MainPool]
	nodes := len(environment.PoolNodes(snapshot.Nodes, c.PoolID(c.MainPool)))
	if pool.Capacity != 2 || len(pool.Instances) != 2 || nodes != 1 {
		return fmt.Errorf("waiting for one Ready worker and one retained VM: capacity %d, instances %d, nodes %d",
			pool.Capacity, len(pool.Instances), nodes)
	}
	var oldNode corev1.Node
	err := f.env.K8s.Get(ctx, client.ObjectKey{Name: worker.node.Name}, &oldNode)
	if !apierrors.IsNotFound(err) {
		if err != nil {
			return fmt.Errorf("get old Node %s: %w", worker.node.Name, err)
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
	return nil
}

// sameWorker requires pool to still hold the VMSS instance of before, with
// the same VM ID and OS disk, in the Succeeded provisioning state.
func sameWorker(snapshot environment.Snapshot, pool string, before parkedWorker) error {
	current, ok := snapshot.Pools[pool].Instances[before.instance.ID]
	if !ok {
		return fmt.Errorf("VMSS instance %s is missing from pool %s", before.instance.ID, pool)
	}
	if !strings.EqualFold(current.ID, before.instance.ID) ||
		current.VMID == "" || !strings.EqualFold(current.VMID, before.instance.VMID) ||
		current.OSDiskID == "" ||
		!strings.EqualFold(current.OSDiskID, before.instance.OSDiskID) ||
		current.OSDiskName == "" || current.OSDiskName != before.instance.OSDiskName ||
		current.ProvisioningState != "Succeeded" {
		return fmt.Errorf("VMSS instance %s changed, got VM %q disk %q %q state %q, want VM %q disk %q %q state Succeeded",
			before.instance.ID, current.VMID, current.OSDiskID, current.OSDiskName, current.ProvisioningState,
			before.instance.VMID, before.instance.OSDiskID, before.instance.OSDiskName)
	}
	return nil
}

// deallocateMarkerPod returns a Pod bound to node that writes token to, or
// reads and compares it from, a run-owned host path.
func deallocateMarkerPod(c environment.Config, namespace string, node corev1.Node, token string, write bool) *corev1.Pod {
	name, command := "deallocate-marker-read", `test "$(cat /marker/token)" = "$TOKEN"`
	if write {
		name, command = "deallocate-marker-write", `printf '%s' "$TOKEN" > /marker/token`
	}
	path := corev1.HostPathDirectoryOrCreate
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace,
			Labels: map[string]string{environment.RunLabel: c.RunID}},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: ptr.To(false),
			NodeName:                     node.Name,
			RestartPolicy:                corev1.RestartPolicyNever,
			Volumes: []corev1.Volume{{Name: "marker", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/ca-e2e-marker/" + c.RunID, Type: &path},
			}}},
			Containers: []corev1.Container{{
				Name: "marker", Image: c.WorkloadImage,
				Command:      []string{"sh", "-c", command},
				Env:          []corev1.EnvVar{{Name: "TOKEN", Value: token}},
				VolumeMounts: []corev1.VolumeMount{{Name: "marker", MountPath: "/marker", ReadOnly: !write}},
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi"),
				}},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
			}},
		},
	}
}

// runDeallocateMarker runs the marker Pod on node until it succeeds, then
// deletes it.
func runDeallocateMarker(ctx context.Context, f *fixture, node corev1.Node, token string, write bool) {
	var currentNode corev1.Node
	Expect(f.env.K8s.Get(ctx, client.ObjectKey{Name: node.Name}, &currentNode)).To(Succeed())
	Expect(currentNode.UID).To(Equal(node.UID))
	pod := deallocateMarkerPod(f.env.Config, f.namespace.Name, node, token, write)
	Expect(f.env.K8s.Create(ctx, pod)).To(Succeed())
	DeferCleanup(func(ctx SpecContext) {
		Expect(deleteDeallocateMarkerPod(ctx, f, pod)).To(Succeed())
	})
	Eventually(ctx, func() error {
		var current corev1.Pod
		if err := f.env.K8s.Get(ctx, client.ObjectKeyFromObject(pod), &current); err != nil {
			return err
		}
		if current.UID != pod.UID || current.Labels[environment.RunLabel] != f.env.Config.RunID ||
			current.Spec.NodeName != node.Name {
			return fmt.Errorf("marker Pod %s changed, UID %s want %s, Node %s want %s",
				pod.Name, current.UID, pod.UID, current.Spec.NodeName, node.Name)
		}
		if current.Status.Phase == corev1.PodFailed {
			StopTrying("marker Pod failed on its assigned worker").Now()
		}
		if current.Status.Phase != corev1.PodSucceeded {
			return fmt.Errorf("waiting for marker Pod on %s, phase %s", node.Name, current.Status.Phase)
		}
		return nil
	}, 5*time.Minute, pollInterval).Should(Succeed())
	Expect(f.env.K8s.Get(ctx, client.ObjectKey{Name: node.Name}, &currentNode)).To(Succeed())
	Expect(currentNode.UID).To(Equal(node.UID))
	Expect(deleteDeallocateMarkerPod(ctx, f, pod)).To(Succeed())
	Eventually(ctx, func() error {
		err := f.env.K8s.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("marker Pod %s still exists", pod.Name)
	}, 2*time.Minute, pollInterval).Should(Succeed())
}

// deleteDeallocateMarkerPod deletes pod if it is still the run's object.
func deleteDeallocateMarkerPod(ctx context.Context, f *fixture, pod *corev1.Pod) error {
	var current corev1.Pod
	err := f.env.K8s.Get(ctx, client.ObjectKeyFromObject(pod), &current)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.UID != pod.UID || current.Labels[environment.RunLabel] != f.env.Config.RunID {
		return fmt.Errorf("marker Pod %s changed, UID %s want %s, run label %q",
			pod.Name, current.UID, pod.UID, current.Labels[environment.RunLabel])
	}
	return f.env.K8s.Delete(ctx, &current, client.Preconditions{UID: &pod.UID})
}

// operatorFaultReceipt requires the operator's receipt ConfigMap name to
// carry the run label and the exact IDs of worker and bootID.
func operatorFaultReceipt(ctx context.Context, f *fixture, name string, worker parkedWorker, bootID string) error {
	var receipt corev1.ConfigMap
	if err := f.env.K8s.Get(ctx, client.ObjectKey{Namespace: f.namespace.Name, Name: name}, &receipt); err != nil {
		return fmt.Errorf("get operator %s receipt: %w", name, err)
	}
	if receipt.Labels[environment.RunLabel] != f.env.Config.RunID {
		return fmt.Errorf("operator %s receipt has run label %q, want %q", name, receipt.Labels[environment.RunLabel], f.env.Config.RunID)
	}
	for key, expected := range map[string]string{
		"run-id": f.env.Config.RunID, "resource-id": worker.instance.ID,
		"vm-id": worker.instance.VMID, "disk-id": worker.instance.OSDiskID, "boot-id": bootID,
	} {
		if receipt.Data[key] != expected {
			return fmt.Errorf("operator %s receipt has %s %q, want %q", name, key, receipt.Data[key], expected)
		}
	}
	return nil
}

// probeDeallocateNetwork runs one HTTP probe Pod on each main worker and
// checks Pod IP and Service traffic in both directions, then removes them.
func probeDeallocateNetwork(ctx context.Context, f *fixture) error {
	probes, service := deallocateNetworkProbe(f.env, f.namespace.Name)
	if err := f.env.K8s.Create(ctx, probes); err != nil {
		return err
	}
	if err := f.env.K8s.Create(ctx, service); err != nil {
		return err
	}
	var pods []corev1.Pod
	Eventually(ctx, func() error {
		var err error
		pods, err = f.env.WorkloadState(ctx, f.namespace.Name, probes.Name, f.env.Config.MainPool, 2, 0)
		if err != nil {
			return err
		}
		if pods[0].Spec.NodeName == pods[1].Spec.NodeName || pods[0].Status.PodIP == "" || pods[1].Status.PodIP == "" {
			return fmt.Errorf("waiting for one network probe per worker, got Nodes %s and %s, IPs %q and %q",
				pods[0].Spec.NodeName, pods[1].Spec.NodeName, pods[0].Status.PodIP, pods[1].Status.PodIP)
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
	if err := f.env.K8s.Delete(ctx, service, client.Preconditions{UID: &service.UID}); err != nil {
		return err
	}
	if err := f.env.K8s.Delete(ctx, probes, client.Preconditions{UID: &probes.UID}); err != nil {
		return err
	}
	Eventually(ctx, func() error {
		if err := workloadPodsGone(ctx, f, probes); err != nil {
			return err
		}
		for _, object := range []client.Object{service, probes} {
			err := f.env.K8s.Get(ctx, client.ObjectKeyFromObject(object), object)
			if !apierrors.IsNotFound(err) {
				if err != nil {
					return fmt.Errorf("get network probe %s: %w", object.GetName(), err)
				}
				return fmt.Errorf("network probe %s still exists", object.GetName())
			}
		}
		return nil
	}, 2*time.Minute, pollInterval).Should(Succeed())
	return nil
}

// deallocateNetworkProbe returns two main-pool HTTP Pods on separate workers
// and a Service that selects them.
func deallocateNetworkProbe(e *environment.Environment, namespace string) (*appsv1.Deployment, *corev1.Service) {
	c := e.Config
	probes := e.Deployment(namespace, "deallocate-network", c.MainLabel, "50m", 2)
	probes.Spec.Template.Spec.Containers[0].Command = []string{"sh", "-c", "mkdir -p /www; hostname >/www/index.html; exec httpd -f -p 8080 -h /www"}
	probes.Spec.Template.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			TopologyKey: corev1.LabelHostname, LabelSelector: probes.Spec.Selector.DeepCopy(),
		}},
	}}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: probes.Name, Namespace: namespace,
		Labels: map[string]string{environment.RunLabel: c.RunID}}, Spec: corev1.ServiceSpec{
		Selector: probes.Spec.Selector.MatchLabels,
		Ports:    []corev1.ServicePort{{Port: 8080}},
	}}
	return probes, service
}

func TestDeallocateWorkerIdentity(t *testing.T) {
	before := parkedWorker{instance: environment.Instance{
		ID: "vmss/virtualmachines/1", VMID: "0123-ABCD",
		OSDiskID: "disks/ABC", OSDiskName: "osdisk_1",
		ProvisioningState: "Succeeded",
	}}
	check := func(instance environment.Instance, key string, want bool) {
		t.Helper()
		snapshot := environment.Snapshot{Pools: map[string]environment.PoolState{
			"main": {Instances: map[string]environment.Instance{key: instance}},
		}}
		if got := sameWorker(snapshot, "main", before); (got == nil) != want {
			t.Errorf("sameWorker(%+v, key %q) = %v, want match %t", instance, key, got, want)
		}
	}
	check(before.instance, before.instance.ID, true)
	folded := before.instance
	folded.VMID, folded.OSDiskID = strings.ToLower(folded.VMID), strings.ToLower(folded.OSDiskID)
	check(folded, before.instance.ID, true)
	check(before.instance, "vmss/virtualmachines/2", false)
	for _, field := range []string{"id", "vmID", "diskID", "diskName", "state"} {
		changed := before.instance
		switch field {
		case "id":
			changed.ID = "vmss/virtualmachines/2"
		case "vmID":
			changed.VMID = "other"
		case "diskID":
			changed.OSDiskID = "disks/other"
		case "diskName":
			changed.OSDiskName = "osdisk_2"
		case "state":
			changed.ProvisioningState = "Failed"
		}
		check(changed, before.instance.ID, false)
	}
}

func TestDeallocateNetworkProbe(t *testing.T) {
	c := environment.Config{
		RunID: "test-123", MainLabel: "main", PoolLabel: "acceptance-pool",
		WorkloadImage: "image@sha256:abc",
	}
	e := &environment.Environment{Config: c}
	probes, service := deallocateNetworkProbe(e, "test-ns")
	wantLabels := map[string]string{environment.RunLabel: c.RunID, "app": probes.Name}
	if probes.Name != "deallocate-network" || probes.Namespace != "test-ns" ||
		*probes.Spec.Replicas != 2 || !maps.Equal(probes.Spec.Selector.MatchLabels, wantLabels) ||
		!maps.Equal(probes.Spec.Template.Labels, wantLabels) ||
		probes.Spec.Template.Spec.NodeSelector[c.PoolLabel] != c.MainLabel {
		t.Fatalf("network probes do not target the run and main pool: %+v", probes)
	}
	terms := probes.Spec.Template.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if len(terms) != 1 || terms[0].TopologyKey != corev1.LabelHostname ||
		!maps.Equal(terms[0].LabelSelector.MatchLabels, wantLabels) {
		t.Fatalf("network probes are not spread across workers: %+v", terms)
	}
	if service.Name != probes.Name || service.Namespace != probes.Namespace ||
		service.Labels[environment.RunLabel] != c.RunID ||
		!maps.Equal(service.Spec.Selector, wantLabels) ||
		len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Port != 8080 {
		t.Fatalf("network Service does not select the probes: %+v", service)
	}
}

func TestDeallocateMarkerPod(t *testing.T) {
	c := environment.Config{RunID: "test-123", WorkloadImage: "image@sha256:abc"}
	node := corev1.Node{}
	node.Name = "worker-b"
	for _, write := range []bool{true, false} {
		pod := deallocateMarkerPod(c, "test-ns", node, "random-token", write)
		if pod.Namespace != "test-ns" || pod.Labels[environment.RunLabel] != c.RunID ||
			pod.Spec.NodeName != node.Name || pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
			t.Errorf("marker Pod is not bound to this run and worker: %+v", pod)
		}
		if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].HostPath == nil ||
			pod.Spec.Volumes[0].HostPath.Path != "/var/lib/ca-e2e-marker/"+c.RunID ||
			*pod.Spec.Volumes[0].HostPath.Type != corev1.HostPathDirectoryOrCreate {
			t.Errorf("marker Pod writes outside the run-owned path: %+v", pod.Spec.Volumes)
		}
		container := pod.Spec.Containers[0]
		if container.Image != c.WorkloadImage || len(container.Env) != 1 ||
			container.Env[0].Value != "random-token" || len(container.VolumeMounts) != 1 ||
			container.VolumeMounts[0].MountPath != "/marker" || container.VolumeMounts[0].ReadOnly == write {
			t.Errorf("marker Pod container does not match mode %t: %+v", write, container)
		}
		if write && (pod.Name != "deallocate-marker-write" ||
			container.Command[2] != `printf '%s' "$TOKEN" > /marker/token`) {
			t.Errorf("marker writer command changed: %+v", container.Command)
		}
		if !write && (pod.Name != "deallocate-marker-read" ||
			container.Command[2] != `test "$(cat /marker/token)" = "$TOKEN"`) {
			t.Errorf("marker reader command changed: %+v", container.Command)
		}
	}
}
