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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEnvironmentAuthorize(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, uid, run string
		valid          bool
	}{
		{name: "operator authorized", uid: "cluster-uid", run: "owned-run", valid: true},
		{name: "wrong kubeconfig target", uid: "production", run: "owned-run"},
		{name: "stale run marker", uid: "cluster-uid", run: "old-run"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			k8s := fake.NewClientBuilder().WithObjects(
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(tt.uid)}},
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: MarkerName}, Data: map[string]string{
					"run-id": tt.run, "cluster-uid": c.ClusterUID, "resource-group": c.ResourceGroup, "subscription-id": c.SubscriptionID,
				}},
			).Build()
			env := &Environment{Config: c, K8s: k8s}
			if err := env.Authorize(context.Background()); (err == nil) != tt.valid {
				t.Fatalf("Authorize = %v, valid=%v", err, tt.valid)
			}
		})
	}
}

func TestCheckControllerArguments(t *testing.T) {
	t.Parallel()
	base := "--node-group-auto-discovery=label:cluster-autoscaler-name=owned-run"
	for _, tt := range []struct {
		name  string
		args  []string
		valid bool
	}{
		{name: "preserved protections", args: []string{base}, valid: true},
		{name: "missing discovery", args: nil},
		{name: "additional explicit group", args: []string{base, "--nodes=1:2:foreign"}},
		{name: "different discovery", args: []string{base, "--node-group-auto-discovery=label:other"}},
		{name: "local storage bypass", args: []string{base, "--skip-nodes-with-local-storage=false"}},
		{name: "system pods bypass", args: []string{base, "--skip-nodes-with-system-pods=false"}},
		{name: "leader disabled", args: []string{base, "--leader-elect=false"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := append(append([]string{}, tt.args...),
				"--scale-down-delay-after-add=10s", "--scale-down-unneeded-time=10s", "--unremovable-node-recheck-timeout=10s")
			if err := CheckControllerArguments(args, "owned-run"); (err == nil) != tt.valid {
				t.Fatalf("arguments = %v, valid=%v", err, tt.valid)
			}
		})
	}
	for _, args := range [][]string{
		{base},
		{base, "--scale-down-delay-after-add=30m", "--scale-down-unneeded-time=10s", "--unremovable-node-recheck-timeout=10s"},
	} {
		if err := CheckControllerArguments(args, "owned-run"); err == nil {
			t.Fatal("negative observation could pass before scale-down was eligible")
		}
	}
}

func TestEnvironmentCheckWorkerIsolation(t *testing.T) {
	t.Parallel()
	c := testConfig()
	for _, tt := range []struct {
		name   string
		owners []metav1.OwnerReference
		valid  bool
	}{
		{name: "daemon on worker", owners: []metav1.OwnerReference{{Kind: "DaemonSet"}}, valid: true},
		{name: "system deployment on worker", owners: []metav1.OwnerReference{{Kind: "ReplicaSet"}}},
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
	var current corev1.Pod
	if err := e.K8s.Get(context.Background(), client.ObjectKeyFromObject(pod), &current); err != nil {
		t.Fatal(err)
	}
}
