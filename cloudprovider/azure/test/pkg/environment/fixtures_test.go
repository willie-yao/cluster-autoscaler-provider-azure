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

package environment

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCheckZeroPoolTaint(t *testing.T) {
	t.Parallel()
	c := testConfig()
	snapshot := Snapshot{Pools: map[string]PoolState{c.ZeroPool: {TemplateTaint: c.RunID + ":NoSchedule"}}}
	if err := CheckZeroPoolTaint(snapshot, c); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", c.RunID + ":PreferNoSchedule", "foreign:NoSchedule"} {
		snapshot.Pools[c.ZeroPool] = PoolState{TemplateTaint: value}
		if err := CheckZeroPoolTaint(snapshot, c); err == nil {
			t.Fatalf("accepted taint tag %q", value)
		}
	}
	node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "zero-0"},
		Spec: corev1.NodeSpec{ProviderID: c.PoolID(c.ZeroPool) + "/virtualMachines/0",
			Taints: []corev1.Taint{{Key: RunLabel, Value: c.RunID, Effect: corev1.TaintEffectNoSchedule}}}}
	snapshot.Nodes = []corev1.Node{node}
	if err := CheckZeroNodeTaint(snapshot, c); err != nil {
		t.Fatal(err)
	}
	for _, taint := range []corev1.Taint{
		{Key: RunLabel, Value: "foreign", Effect: corev1.TaintEffectNoSchedule},
		{Key: RunLabel, Value: c.RunID, Effect: corev1.TaintEffectPreferNoSchedule},
	} {
		node.Spec.Taints = []corev1.Taint{taint}
		snapshot.Nodes = []corev1.Node{node}
		if err := CheckZeroNodeTaint(snapshot, c); err == nil {
			t.Fatalf("accepted Node taint %v", taint)
		}
	}
}

func TestCheckDiskFixture(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		change func(*Config, *storagev1.StorageClass, *storagev1.CSINode)
		valid  bool
	}{
		{name: "CSI class and registered worker", valid: true},
		{name: "missing class binding", change: func(c *Config, _ *storagev1.StorageClass, _ *storagev1.CSINode) {
			c.DiskStorageClass = ""
		}},
		{name: "wrong driver", change: func(_ *Config, class *storagev1.StorageClass, _ *storagev1.CSINode) {
			class.Provisioner = "other.csi"
		}},
		{name: "immediate binding", change: func(_ *Config, class *storagev1.StorageClass, _ *storagev1.CSINode) {
			class.VolumeBindingMode = ptr.To(storagev1.VolumeBindingImmediate)
		}},
		{name: "unregistered worker", change: func(_ *Config, _ *storagev1.StorageClass, node *storagev1.CSINode) {
			node.Spec.Drivers = nil
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			c.DiskStorageClass = "run-disk"
			class := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: c.DiskStorageClass}, Provisioner: DiskCSIDriver,
				VolumeBindingMode: ptr.To(storagev1.VolumeBindingWaitForFirstConsumer)}
			node := &storagev1.CSINode{ObjectMeta: metav1.ObjectMeta{Name: "worker-0"},
				Spec: storagev1.CSINodeSpec{Drivers: []storagev1.CSINodeDriver{{Name: DiskCSIDriver, NodeID: "worker-0"}}}}
			if tt.change != nil {
				tt.change(&c, class, node)
			}
			baseline := Snapshot{Nodes: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Labels: map[string]string{corev1.LabelTopologyZone: "westus2-1"}},
				Spec: corev1.NodeSpec{ProviderID: c.PoolID(c.MainPool) + "/virtualMachines/0"}}}}
			e := &Environment{Config: c, K8s: fake.NewClientBuilder().WithObjects(class, node).Build()}
			err := e.CheckDiskFixture(context.Background(), baseline)
			if (err == nil) != tt.valid {
				t.Fatalf("CheckDiskFixture error=%v, valid=%v", err, tt.valid)
			}
		})
	}
}

func TestCheckDiskWorkers(t *testing.T) {
	t.Parallel()
	c := testConfig()
	zone := corev1.LabelTopologyZone
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Labels: map[string]string{zone: "westus2-1"}},
			Spec: corev1.NodeSpec{ProviderID: c.PoolID(c.MainPool) + "/virtualMachines/0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Labels: map[string]string{zone: "westus2-1"}},
			Spec: corev1.NodeSpec{ProviderID: c.PoolID(c.MainPool) + "/virtualMachines/1"}},
	}
	registered := &storagev1.CSINode{ObjectMeta: metav1.ObjectMeta{Name: "worker-0"},
		Spec: storagev1.CSINodeSpec{Drivers: []storagev1.CSINodeDriver{{Name: DiskCSIDriver, NodeID: "worker-0"}}}}
	other := registered.DeepCopy()
	other.Name = "worker-1"
	e := &Environment{Config: c, K8s: fake.NewClientBuilder().WithObjects(registered, other).Build()}
	snapshot := Snapshot{Nodes: nodes}
	if err := e.CheckDiskWorkers(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.Nodes[1].Labels[zone] = "westus2-2"
	if err := e.CheckDiskWorkers(context.Background(), snapshot); err == nil {
		t.Fatal("accepted different disk topology zones")
	}
	delete(snapshot.Nodes[1].Labels, zone)
	if err := e.CheckDiskWorkers(context.Background(), snapshot); err == nil {
		t.Fatal("accepted a missing disk topology zone")
	}
	snapshot.Nodes[1].Labels[zone] = "westus2-1"
	other.Spec.Drivers = nil
	if err := e.K8s.Update(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if err := e.CheckDiskWorkers(context.Background(), snapshot); err == nil {
		t.Fatal("accepted an unregistered disk worker")
	}
}

func TestReadDiskIdentityRequiresEnvironmentClient(t *testing.T) {
	t.Parallel()
	e := &Environment{Config: testConfig()}
	if _, err := e.ReadDiskIdentity(context.Background(), "test", "disk-data-0"); err == nil {
		t.Fatal("read a Pod file without the client from NewEnvironment")
	}
}
