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
	"strings"
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
		change func(*Config, *corev1.ConfigMap, *storagev1.StorageClass, *storagev1.CSIDriver, *storagev1.CSINode)
		valid  bool
	}{
		{name: "run-owned CSI class and registered worker", valid: true},
		{name: "missing class binding", change: func(c *Config, _ *corev1.ConfigMap, _ *storagev1.StorageClass, _ *storagev1.CSIDriver, _ *storagev1.CSINode) {
			c.DiskStorageClass = ""
		}},
		{name: "missing marker permission", change: func(_ *Config, marker *corev1.ConfigMap, _ *storagev1.StorageClass, _ *storagev1.CSIDriver, _ *storagev1.CSINode) {
			marker.Data = nil
		}},
		{name: "foreign class", change: func(_ *Config, _ *corev1.ConfigMap, class *storagev1.StorageClass, _ *storagev1.CSIDriver, _ *storagev1.CSINode) {
			class.Labels[RunLabel] = "foreign"
		}},
		{name: "wrong driver", change: func(_ *Config, _ *corev1.ConfigMap, class *storagev1.StorageClass, _ *storagev1.CSIDriver, _ *storagev1.CSINode) {
			class.Provisioner = "other.csi"
		}},
		{name: "premature binding", change: func(_ *Config, _ *corev1.ConfigMap, class *storagev1.StorageClass, _ *storagev1.CSIDriver, _ *storagev1.CSINode) {
			class.VolumeBindingMode = ptr.To(storagev1.VolumeBindingImmediate)
		}},
		{name: "retained disks", change: func(_ *Config, _ *corev1.ConfigMap, class *storagev1.StorageClass, _ *storagev1.CSIDriver, _ *storagev1.CSINode) {
			class.ReclaimPolicy = ptr.To(corev1.PersistentVolumeReclaimRetain)
		}},
		{name: "foreign resource group", change: func(_ *Config, _ *corev1.ConfigMap, class *storagev1.StorageClass, _ *storagev1.CSIDriver, _ *storagev1.CSINode) {
			class.Parameters["resourceGroup"] = "foreign"
		}},
		{name: "foreign subscription", change: func(_ *Config, _ *corev1.ConfigMap, class *storagev1.StorageClass, _ *storagev1.CSIDriver, _ *storagev1.CSINode) {
			class.Parameters["subscriptionID"] = "foreign"
		}},
		{name: "untagged disk", change: func(_ *Config, _ *corev1.ConfigMap, class *storagev1.StorageClass, _ *storagev1.CSIDriver, _ *storagev1.CSINode) {
			class.Parameters["tags"] = ""
		}},
		{name: "duplicated run tag", change: func(c *Config, _ *corev1.ConfigMap, class *storagev1.StorageClass, _ *storagev1.CSIDriver, _ *storagev1.CSINode) {
			class.Parameters["tags"] += "," + RunLabel + "=" + c.RunID
		}},
		{name: "wrong disk SKU", change: func(_ *Config, _ *corev1.ConfigMap, class *storagev1.StorageClass, _ *storagev1.CSIDriver, _ *storagev1.CSINode) {
			class.Parameters["skuName"] = "Premium_LRS"
		}},
		{name: "no CSI attachment", change: func(_ *Config, _ *corev1.ConfigMap, _ *storagev1.StorageClass, driver *storagev1.CSIDriver, _ *storagev1.CSINode) {
			driver.Spec.AttachRequired = ptr.To(false)
		}},
		{name: "unregistered worker", change: func(_ *Config, _ *corev1.ConfigMap, _ *storagev1.StorageClass, _ *storagev1.CSIDriver, node *storagev1.CSINode) {
			node.Spec.Drivers = nil
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			c.DiskStorageClass = "run-disk"
			marker := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: MarkerName, Namespace: "kube-system"},
				Data: map[string]string{"allow-disk-fixture": "AZ-P1-006"}}
			class := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: c.DiskStorageClass,
				Labels: map[string]string{RunLabel: c.RunID}}, Provisioner: DiskCSIDriver,
				VolumeBindingMode: ptr.To(storagev1.VolumeBindingWaitForFirstConsumer),
				ReclaimPolicy:     ptr.To(corev1.PersistentVolumeReclaimDelete),
				Parameters: map[string]string{"resourceGroup": c.ResourceGroup, "subscriptionID": c.SubscriptionID, "skuName": "StandardSSD_LRS",
					"tags": RunLabel + "=" + c.RunID}}
			driver := &storagev1.CSIDriver{ObjectMeta: metav1.ObjectMeta{Name: DiskCSIDriver}}
			node := &storagev1.CSINode{ObjectMeta: metav1.ObjectMeta{Name: "worker-0"},
				Spec: storagev1.CSINodeSpec{Drivers: []storagev1.CSINodeDriver{{Name: DiskCSIDriver, NodeID: "worker-0"}}}}
			if tt.change != nil {
				tt.change(&c, marker, class, driver, node)
			}

			baseline := Snapshot{Nodes: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Labels: map[string]string{corev1.LabelTopologyZone: "westus2-1"}},
				Spec: corev1.NodeSpec{ProviderID: c.PoolID(c.MainPool) + "/virtualMachines/0"}}}}
			e := &Environment{Config: c, K8s: fake.NewClientBuilder().WithObjects(marker, class, driver, node).Build()}
			err := e.CheckDiskFixture(context.Background(), baseline)
			if (err == nil) != tt.valid {
				t.Fatalf("CheckDiskFixture error=%v, valid=%v", err, tt.valid)
			}
		})
	}
	if !hasRunTag("team=test,"+RunLabel+"=owned-run", "owned-run") ||
		hasRunTag(RunLabel+"=foreign,"+RunLabel+"=owned-run", "owned-run") ||
		hasRunTag(strings.ToUpper(RunLabel)+"=owned-run", "owned-run") {
		t.Fatal("run tag must match once")
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

func TestReadDiskIdentityRequiresExplicitKubeconfig(t *testing.T) {
	t.Parallel()
	e := &Environment{Config: Config{Kubeconfig: "/missing/disk-test-kubeconfig", Context: "disposable"}}
	if _, err := e.ReadDiskIdentity(context.Background(), "test", "disk-data-0"); err == nil {
		t.Fatal("accepted a missing explicit kubeconfig")
	}
}

func TestCheckPhaseMarker(t *testing.T) {
	t.Parallel()
	c := testConfig()
	c.Phase = "minimum"
	marker := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: MarkerName, Namespace: "kube-system"},
		Data: map[string]string{"allow-minimum-fixture": "AZ-P1-009"}}
	e := &Environment{Config: c, K8s: fake.NewClientBuilder().WithObjects(marker).Build()}
	if err := e.CheckPhaseMarker(context.Background(), "minimum", "AZ-P1-009"); err != nil {
		t.Fatal(err)
	}
	if err := e.CheckPhaseMarker(context.Background(), "no-join", "AZ-P1-008"); err == nil {
		t.Fatal("accepted a different phase")
	}
	marker.Data["allow-minimum-fixture"] = "foreign"
	if err := e.K8s.Update(context.Background(), marker); err != nil {
		t.Fatal(err)
	}
	if err := e.CheckPhaseMarker(context.Background(), "minimum", "AZ-P1-009"); err == nil {
		t.Fatal("accepted an unauthorized fixture")
	}
}

func TestCheckBalancePools(t *testing.T) {
	t.Parallel()
	c := testConfig()
	c.Phase, c.BalancePoolA, c.BalancePoolB, c.BalanceLabel = "balance", "pair-a", "pair-b", "balanced"
	baseline := Snapshot{Pools: map[string]PoolState{
		"pair-a": {Instances: map[string]Instance{}, SKU: "Standard_D2s_v5", Zone: "1,", TemplateCustomData: true},
		"pair-b": {Instances: map[string]Instance{}, SKU: "Standard_D2s_v5", Zone: "1,", TemplateCustomData: true},
	}}
	if err := CheckBalancePools(baseline, c); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		change func(*PoolState)
	}{
		{name: "wrong SKU", change: func(p *PoolState) { p.SKU = "Standard_D4s_v5" }},
		{name: "wrong zone", change: func(p *PoolState) { p.Zone = "2," }},
		{name: "no join template", change: func(p *PoolState) { p.TemplateCustomData = false }},
		{name: "tainted template", change: func(p *PoolState) { p.TemplateTaint = "foreign:NoSchedule" }},
		{name: "already growing", change: func(p *PoolState) { p.Capacity = 1 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pool := baseline.Pools[c.BalancePoolB]
			tt.change(&pool)
			baseline.Pools[c.BalancePoolB] = pool
			if err := CheckBalancePools(baseline, c); err == nil {
				t.Fatal("accepted unmatched balance pool")
			}
			baseline.Pools[c.BalancePoolB] = PoolState{Instances: map[string]Instance{}, SKU: "Standard_D2s_v5", Zone: "1,", TemplateCustomData: true}
		})
	}
}
