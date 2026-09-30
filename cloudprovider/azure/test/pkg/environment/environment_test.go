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
	"errors"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestController(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		change func(*appsv1.Deployment, *corev1.Pod, *corev1.ConfigMap) []client.Object
		valid  bool
	}{
		{name: "one Ready controller with fresh status", valid: true},
		{name: "two replicas", change: func(deployment *appsv1.Deployment, _ *corev1.Pod, _ *corev1.ConfigMap) []client.Object {
			deployment.Spec.Replicas = ptr.To(int32(2))
			return nil
		}},
		{name: "second Pod", change: func(_ *appsv1.Deployment, pod *corev1.Pod, _ *corev1.ConfigMap) []client.Object {
			other := pod.DeepCopy()
			other.Name = "autoscaler-1"
			return []client.Object{other}
		}},
		{name: "Pod not Ready", change: func(_ *appsv1.Deployment, pod *corev1.Pod, _ *corev1.ConfigMap) []client.Object {
			pod.Status.Conditions[0].Status = corev1.ConditionFalse
			return nil
		}},
		{name: "other image", change: func(_ *appsv1.Deployment, pod *corev1.Pod, _ *corev1.ConfigMap) []client.Object {
			pod.Spec.Containers[0].Image = "other:tag"
			return nil
		}},
		{name: "other container", change: func(_ *appsv1.Deployment, pod *corev1.Pod, _ *corev1.ConfigMap) []client.Object {
			pod.Spec.Containers[0].Name = "other"
			return nil
		}},
		{name: "stale status", change: func(_ *appsv1.Deployment, _ *corev1.Pod, status *corev1.ConfigMap) []client.Object {
			status.Data["status"] = statusText(time.Now().Add(-3*time.Minute), "main", "zero")
			return nil
		}},
		{name: "status without zero", change: func(_ *appsv1.Deployment, _ *corev1.Pod, status *corev1.ConfigMap) []client.Object {
			status.Data["status"] = statusText(time.Now(), "main")
			return nil
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			labels := map[string]string{"app": "autoscaler"}
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: c.AutoscalerDeployment, Namespace: c.AutoscalerNamespace},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: labels}},
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "autoscaler-0", Namespace: c.AutoscalerNamespace, Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: c.AutoscalerContainer, Image: c.ExpectedImage}}},
				Status: corev1.PodStatus{Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
			}
			status := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cluster-autoscaler-status", Namespace: c.AutoscalerNamespace},
				Data: map[string]string{"status": statusText(time.Now(), c.MainPool, c.ZeroPool)}}
			objects := []client.Object{deployment, pod, status}
			if tt.change != nil {
				objects = append(objects, tt.change(deployment, pod, status)...)
			}
			e := &Environment{Config: c, K8s: fake.NewClientBuilder().WithObjects(objects...).Build()}
			if err := e.Controller(context.Background()); (err == nil) != tt.valid {
				t.Fatalf("Controller error=%v, valid=%v", err, tt.valid)
			}
		})
	}
}

func statusText(at time.Time, groups ...string) string {
	text := fmt.Sprintf("time: %s\nautoscalerStatus: Running\nnodeGroups:\n", at.UTC().Format(time.RFC3339))
	for _, group := range groups {
		text += "- name: " + group + "\n"
	}
	return text
}

func TestEnvironmentCheckWorkerIsolation(t *testing.T) {
	t.Parallel()
	c := testConfig()
	for _, tt := range []struct {
		name   string
		owners []metav1.OwnerReference
		valid  bool
	}{
		{name: "daemon on worker", owners: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Controller: ptr.To(true)}}, valid: true},
		{name: "DaemonSet owner that is not the controller", owners: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet"}}},
		{name: "DaemonSet kind from another API group", owners: []metav1.OwnerReference{{APIVersion: "example.com/v1", Kind: "DaemonSet", Controller: ptr.To(true)}}},
		{name: "system deployment on worker", owners: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Controller: ptr.To(true)}}},
		{name: "unowned bare pod on worker"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "system", Namespace: "kube-system", OwnerReferences: tt.owners},
				Spec: corev1.PodSpec{NodeName: "worker-0"}}
			e := &Environment{Config: c, K8s: fake.NewClientBuilder().WithObjects(pod).Build()}
			if err := e.CheckWorkerIsolation(context.Background(), testSnapshot(c), "test"); (err == nil) != tt.valid {
				t.Fatalf("isolation = %v, valid=%v", err, tt.valid)
			}
		})
	}
}

type fakeCloud struct {
	snapshot Snapshot
	exists   bool
	err      error
}

func (f fakeCloud) Read(context.Context) (Snapshot, error) { return f.snapshot, f.err }
func (f fakeCloud) NICExists(context.Context, string) (bool, error) {
	return f.exists, f.err
}

func TestEnvironmentRead(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		change func(*Snapshot)
		cloud  error
		bounds bool
	}{
		{name: "within max tags"},
		{name: "desired capacity above max", bounds: true, change: func(s *Snapshot) {
			pool := s.Pools["zero"]
			pool.Capacity = 2
			s.Pools["zero"] = pool
		}},
		{name: "VMs above max", bounds: true, change: func(s *Snapshot) {
			s.Pools["main"].Instances["extra-1"] = Instance{}
			s.Pools["main"].Instances["extra-2"] = Instance{}
		}},
		{name: "ordinary cloud read failure", cloud: errors.New("temporary failure")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			snapshot := testSnapshot(c)
			snapshot.Nodes = nil
			if tt.change != nil {
				tt.change(&snapshot)
			}
			node := testNode(c)
			e := &Environment{Config: c, K8s: fake.NewClientBuilder().WithObjects(&node).Build(),
				Cloud: fakeCloud{snapshot: snapshot, err: tt.cloud}}
			result, err := e.Read(context.Background())
			if errors.Is(err, ErrBounds) != tt.bounds || (err == nil) != (!tt.bounds && tt.cloud == nil) {
				t.Fatalf("Read error=%v, bounds=%t", err, tt.bounds)
			}
			if err == nil && len(result.Nodes) != 1 {
				t.Fatalf("Read returned %d Nodes, want 1", len(result.Nodes))
			}
		})
	}
}

func TestEnvironmentDeleted(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		cloud   fakeCloud
		node    bool
		removed bool
		valid   bool
	}{
		{name: "physical deletion", removed: true, valid: true},
		{name: "desired capacity only"},
		{name: "NIC still present", removed: true, cloud: fakeCloud{exists: true}},
		{name: "Node still present", removed: true, node: true},
		{name: "NIC authorization error", removed: true, cloud: fakeCloud{err: errors.New("forbidden")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			before, after := testSnapshot(c), testSnapshot(c)
			if tt.removed {
				after.Pools[c.MainPool] = PoolState{Instances: map[string]Instance{}}
			}
			if !tt.node {
				after.Nodes = nil
			}
			e := &Environment{Config: c, Cloud: tt.cloud}
			if err := e.Deleted(context.Background(), before, after, c.MainPool, 1); (err == nil) != tt.valid {
				t.Fatalf("Deleted = %v, valid=%v", err, tt.valid)
			}
		})
	}
}

func TestEnvironmentDeletedEveryCapturedInstance(t *testing.T) {
	t.Parallel()
	c := testConfig()
	first := normalizeID(c.PoolID(c.ZeroPool) + "/virtualMachines/0")
	replacement := normalizeID(c.PoolID(c.ZeroPool) + "/virtualMachines/1")
	before := Snapshot{Pools: map[string]PoolState{c.ZeroPool: {Instances: map[string]Instance{
		first:       {ID: first, NICs: []string{first + "/networkInterfaces/first"}},
		replacement: {ID: replacement, NICs: []string{replacement + "/networkInterfaces/replacement"}},
	}}}}
	after := Snapshot{Pools: map[string]PoolState{c.ZeroPool: {Instances: map[string]Instance{}}}}
	e := &Environment{Cloud: fakeCloud{}}
	if err := e.Deleted(context.Background(), before, after, c.ZeroPool, 2); err != nil {
		t.Fatal(err)
	}
	after.Pools[c.ZeroPool].Instances[replacement] = before.Pools[c.ZeroPool].Instances[replacement]
	if err := e.Deleted(context.Background(), before, after, c.ZeroPool, 2); err == nil {
		t.Fatal("accepted a replacement VM that still exists")
	}
}

func TestWorkloadStateRequiresSchedulingEvidence(t *testing.T) {
	t.Parallel()
	c := testConfig()
	node := testNode(c)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demand", Namespace: "test", Labels: map[string]string{RunLabel: c.RunID, "app": "demand"}},
		Status: corev1.PodStatus{Phase: corev1.PodPending}}
	e := &Environment{Config: c, K8s: fake.NewClientBuilder().WithObjects(&node, pod).Build()}
	if _, err := e.WorkloadState(context.Background(), "test", "demand", c.MainPool, 0, 1); err == nil {
		t.Fatal("ordinary Pending accepted as scheduler rejection")
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable}}
	if err := e.K8s.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if _, err := e.WorkloadState(context.Background(), "test", "demand", c.MainPool, 0, 1); err != nil {
		t.Fatal(err)
	}
}
