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
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestSnapshotStableChecksEachPoolBounds(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		main, zero int
		valid      bool
	}{
		{main: 1, zero: 0, valid: true},
		{main: 2, zero: 1, valid: true},
		{main: 3, zero: 0},
		{main: 1, zero: 2},
		{main: 0, zero: 1},
	} {
		t.Run(fmt.Sprintf("main=%d zero=%d", tt.main, tt.zero), func(t *testing.T) {
			c := testConfig()
			snapshot := Snapshot{Pools: map[string]PoolState{}, VMs: 1 + tt.main + tt.zero, VCPUs: 2 * (1 + tt.main + tt.zero)}
			for name, count := range map[string]int{c.MainPool: tt.main, c.ZeroPool: tt.zero} {
				pool := PoolState{Capacity: count, Instances: map[string]Instance{}}
				for i := 0; i < count; i++ {
					node := testNode(c)
					node.Name = fmt.Sprintf("%s-%d", name, i)
					node.Spec.ProviderID = fmt.Sprintf("azure://%s/virtualMachines/%d", c.PoolID(name), i)
					if name == c.ZeroPool {
						node.Labels[c.PoolLabel] = c.ZeroLabel
					}
					snapshot.Nodes = append(snapshot.Nodes, node)
					id := normalizeID(node.Spec.ProviderID)
					pool.Instances[id] = Instance{ID: id}
				}
				snapshot.Pools[name] = pool
			}
			if err := snapshot.Stable(c, tt.main, tt.zero); (err == nil) != tt.valid {
				t.Fatalf("observed-distribution error=%v, valid=%v", err, tt.valid)
			}
		})
	}
}

func testNode(c Config) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Labels: map[string]string{c.PoolLabel: c.MainLabel}},
		Spec:       corev1.NodeSpec{ProviderID: "azure://" + c.PoolID(c.MainPool) + "/virtualMachines/0"},
		Status: corev1.NodeStatus{
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
		},
	}
}

func testSnapshot(c Config) Snapshot {
	node := testNode(c)
	id := normalizeID(node.Spec.ProviderID)
	return Snapshot{
		Pools: map[string]PoolState{
			c.MainPool: {Capacity: 1, Instances: map[string]Instance{id: {ID: id, NICs: []string{c.PoolID(c.MainPool) + "/virtualMachines/0/networkInterfaces/nic"}}}},
			c.ZeroPool: {Capacity: 0, Instances: map[string]Instance{}},
		},
		Nodes: []corev1.Node{node, {ObjectMeta: metav1.ObjectMeta{Name: "cp"}, Spec: corev1.NodeSpec{ProviderID: "azure://" + c.ControlPlaneID}}},
		VMs:   2, VCPUs: 4,
	}
}

func TestSnapshotStable(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		change func(*Snapshot)
	}{
		{name: "unready worker", change: func(s *Snapshot) { s.Nodes[0].Status.Conditions[0].Status = corev1.ConditionFalse }},
		{name: "cordoned worker", change: func(s *Snapshot) { s.Nodes[0].Spec.Unschedulable = true }},
		{name: "wrong predicted label", change: func(s *Snapshot) { s.Nodes[0].Labels["acceptance-pool"] = "wrong" }},
		{name: "capacity without actual VM", change: func(s *Snapshot) { delete(s.Pools["main"].Instances, normalizeID(s.Nodes[0].Spec.ProviderID)) }},
		{name: "duplicate registration", change: func(s *Snapshot) { s.Nodes = append(s.Nodes, s.Nodes[0]) }},
		{name: "stale provider identity", change: func(s *Snapshot) { s.Nodes[0].Spec.ProviderID += "different" }},
		{name: "resource ceiling", change: func(s *Snapshot) { s.VCPUs = 10 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			s := testSnapshot(c)
			if err := s.Stable(c, 1, 0); err != nil {
				t.Fatalf("control plane must not count as a pool worker: %v", err)
			}
			tt.change(&s)
			if err := s.Stable(c, 1, 0); err == nil {
				t.Fatal("invalid cloud/Node state accepted")
			}
		})
	}
}

func TestSnapshotStableBalancePools(t *testing.T) {
	t.Parallel()
	c := testConfig()
	c.Phase, c.BalancePoolA, c.BalancePoolB, c.BalanceLabel = "balance", "pair-a", "pair-b", "balanced"
	s := testSnapshot(c)
	s.Pools[c.BalancePoolA] = PoolState{Instances: map[string]Instance{}}
	s.Pools[c.BalancePoolB] = PoolState{Instances: map[string]Instance{}}
	s.Nodes[0].Labels[c.PoolLabel] = c.MainLabel
	baseline := map[string]int{c.MainPool: 1, c.ZeroPool: 0, c.BalancePoolA: 0, c.BalancePoolB: 0}
	if err := s.StablePools(c, baseline); err != nil {
		t.Fatal(err)
	}
	if err := s.Stable(c, 1, 0); err == nil {
		t.Fatal("balance phase cannot omit the two extra pool counts")
	}
	for _, name := range []string{c.BalancePoolA, c.BalancePoolB} {
		node := testNode(c)
		node.Name = name + "-0"
		node.Spec.ProviderID = "azure://" + c.PoolID(name) + "/virtualMachines/0"
		node.Labels[c.PoolLabel] = c.BalanceLabel
		s.Nodes = append(s.Nodes, node)
		id := normalizeID(node.Spec.ProviderID)
		s.Pools[name] = PoolState{Capacity: 1, Instances: map[string]Instance{id: {ID: id}}}
	}
	s.VMs, s.VCPUs = 4, 8
	grown := map[string]int{c.MainPool: 1, c.ZeroPool: 0, c.BalancePoolA: 1, c.BalancePoolB: 1}
	if err := s.StablePools(c, grown); err != nil {
		t.Fatal(err)
	}
	s.VMs = 5
	if err := s.StablePools(c, grown); !errors.Is(err, ErrBounds) {
		t.Fatalf("extra VM not rejected: %v", err)
	}
}

func TestValidateDemand(t *testing.T) {
	t.Parallel()
	node := testNode(testConfig())
	pods := []corev1.Pod{{Spec: corev1.PodSpec{
		NodeName: node.Name, Containers: []corev1.Container{{
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("300m")}},
		}},
	}}}
	for _, tt := range []struct {
		name   string
		demand int64
		valid  bool
	}{
		{name: "measured Phase1 geometry", demand: 1200, valid: true},
		{name: "two fit on a worker", demand: 800},
		{name: "one cannot fit", demand: 1800},
		{name: "exact two fit", demand: 850},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDemand(node, pods, tt.demand)
			if (err == nil) != tt.valid {
				t.Fatalf("ValidateDemand = %v, valid=%v", err, tt.valid)
			}
		})
	}
}

func TestNodeRequests(t *testing.T) {
	t.Parallel()
	container := func(cpu string) corev1.Container {
		return corev1.Container{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}}}
	}
	sidecar := container("100m")
	sidecar.RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways)
	for _, tt := range []struct {
		name string
		init []corev1.Container
		want int64
	}{
		{name: "Calico unrequested install init", init: []corev1.Container{{Name: "install-cni"}}, want: 250},
		{name: "larger regular init", init: []corev1.Container{container("500m")}, want: 550},
		{name: "regular init max not sum", init: []corev1.Container{container("500m"), container("300m")}, want: 550},
		{name: "restartable sidecar with later init", init: []corev1.Container{sidecar, container("500m")}, want: 650},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := testNode(testConfig())
			pod := corev1.Pod{Spec: corev1.PodSpec{
				NodeName: node.Name, Containers: []corev1.Container{container("200m")}, InitContainers: tt.init,
				Overhead: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
			}}
			requests, err := NodeRequests(node, []corev1.Pod{pod})
			if err != nil || requests.Cpu().MilliValue() != tt.want {
				t.Fatalf("requests=%v err=%v, want %dm", requests, err, tt.want)
			}
			pod.Spec.Resources = &corev1.ResourceRequirements{}
			if _, err := NodeRequests(node, []corev1.Pod{pod}); err == nil {
				t.Fatal("retained helper cannot attest Pod-level requests")
			}
		})
	}
}

func TestCheckStatus(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	status := "time: 2026-09-16T09:00:00Z\nautoscalerStatus: Running\nnodeGroups:\n- name: main\n- name: zero\n"
	for _, tt := range []struct {
		name, status string
		valid        bool
	}{
		{name: "exact reported inventory", status: status, valid: true},
		{name: "missing status", status: ""},
		{name: "wrong group", status: strings.ReplaceAll(status, "zero", "unowned")},
		{name: "extra group", status: status + "- name: unowned\n"},
		{name: "stale controller", status: strings.ReplaceAll(status, "09:00:00", "08:00:00")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckStatus(tt.status, []string{"main", "zero"}, now)
			if (err == nil) != tt.valid {
				t.Fatalf("CheckStatus = %v, valid=%v", err, tt.valid)
			}
		})
	}
}

func TestCheckStatusCapturedRuntimePayload(t *testing.T) {
	t.Parallel()
	payload, err := os.ReadFile("testdata/autoscaler-status.yaml")
	if err != nil {
		t.Fatal(err)
	}
	status := string(payload)
	const capturedTime = "2026-09-16 09:21:08.356413974 +0000 UTC"
	now := time.Date(2026, 9, 16, 9, 21, 9, 0, time.UTC)
	for _, tt := range []struct {
		name, status string
		now          time.Time
		valid        bool
	}{
		{name: "actual nanosecond timestamp", status: status, now: now, valid: true},
		{name: "no fractional seconds", status: strings.ReplaceAll(status, capturedTime, "2026-09-16 09:21:08 +0000 UTC"), now: now, valid: true},
		{name: "millisecond timestamp", status: strings.ReplaceAll(status, capturedTime, "2026-09-16 09:21:08.356 +0000 UTC"), now: now, valid: true},
		{name: "RFC3339 compatibility", status: strings.ReplaceAll(status, capturedTime, "2026-09-16T09:21:08.356413974Z"), now: now, valid: true},
		{name: "stale captured payload", status: status, now: now.Add(3 * time.Minute)},
		{name: "future captured payload", status: status, now: now.Add(-time.Minute)},
		{name: "malformed timestamp", status: strings.ReplaceAll(status, capturedTime, "not-a-timestamp"), now: now},
		{name: "wrong inventory", status: strings.ReplaceAll(status, "name: zero", "name: foreign"), now: now},
		{name: "inactive autoscaler", status: strings.ReplaceAll(status, "autoscalerStatus: Running", "autoscalerStatus: Inactive"), now: now},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := CheckStatus(tt.status, []string{"main", "zero"}, tt.now); (err == nil) != tt.valid {
				t.Fatalf("captured status error=%v, valid=%v", err, tt.valid)
			}
		})
	}
}
