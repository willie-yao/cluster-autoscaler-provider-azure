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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

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
