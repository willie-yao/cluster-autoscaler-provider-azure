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
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCheckControllerScope(t *testing.T) {
	t.Parallel()
	c := testConfig()
	for _, tt := range []struct {
		name   string
		change func([]corev1.EnvVar) []corev1.EnvVar
		valid  bool
	}{
		{name: "exact literal scope", valid: true},
		{name: "same pool names in foreign resource group", change: func(env []corev1.EnvVar) []corev1.EnvVar {
			env[1].Value = "foreign-workers"
			return env
		}},
		{name: "foreign subscription", change: func(env []corev1.EnvVar) []corev1.EnvVar {
			env[0].Value = "00000000-0000-0000-0000-000000000002"
			return env
		}},
		{name: "unresolved secret reference", change: func(env []corev1.EnvVar) []corev1.EnvVar {
			env[1].Value = ""
			env[1].ValueFrom = &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "cloud-config"}, Key: "resourceGroup",
			}}
			return env
		}},
		{name: "missing scope", change: func(env []corev1.EnvVar) []corev1.EnvVar { return env[:1] }},
		{name: "duplicate scope", change: func(env []corev1.EnvVar) []corev1.EnvVar { return append(env, env[1]) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := []corev1.EnvVar{{Name: "ARM_SUBSCRIPTION_ID", Value: c.SubscriptionID}, {Name: "ARM_RESOURCE_GROUP", Value: c.ResourceGroup}}
			if tt.change != nil {
				env = tt.change(env)
			}
			if err := checkControllerScope(env, c); (err == nil) != tt.valid {
				t.Fatalf("scope error=%v, valid=%v", err, tt.valid)
			}
		})
	}
}

func TestControllerChecksScopeAndLeader(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, invalidScope, holder string
		hostNetwork, valid         bool
	}{
		{name: "exact scope and Pod leader", valid: true},
		{name: "wrong template scope", invalidScope: "template"},
		{name: "wrong running Pod scope", invalidScope: "running Pod"},
		{name: "host-network Node leader", hostNetwork: true, holder: "control-plane", valid: true},
		{name: "foreign host-network leader", hostNetwork: true, holder: "foreign-node"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			container := corev1.Container{Name: c.AutoscalerContainer, Image: c.ExpectedImage,
				Env: []corev1.EnvVar{{Name: "ARM_SUBSCRIPTION_ID", Value: c.SubscriptionID}, {Name: "ARM_RESOURCE_GROUP", Value: c.ResourceGroup}},
				Args: []string{"--node-group-auto-discovery=label:cluster-autoscaler-name=" + c.DiscoveryValue,
					"--scale-down-delay-after-add=10s", "--scale-down-unneeded-time=10s", "--unremovable-node-recheck-timeout=10s"},
			}
			labels := map[string]string{"app": "autoscaler"}
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: c.AutoscalerDeployment, Namespace: c.AutoscalerNamespace},
				Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: labels},
					Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{*container.DeepCopy()}}}},
				Status: appsv1.DeploymentStatus{ReadyReplicas: 1, UpdatedReplicas: 1},
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "autoscaler-0", Namespace: c.AutoscalerNamespace, Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{*container.DeepCopy()}, HostNetwork: tt.hostNetwork, NodeName: "control-plane"},
				Status: corev1.PodStatus{Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
			}
			if tt.invalidScope == "template" {
				deployment.Spec.Template.Spec.Containers[0].Env[1].Value = "foreign-workers"
			}
			if tt.invalidScope == "running Pod" {
				pod.Spec.Containers[0].Env[1].Value = "foreign-workers"
			}
			lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: c.LeaseName, Namespace: c.AutoscalerNamespace},
				Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To(pod.Name), RenewTime: &metav1.MicroTime{Time: time.Now()}}}
			if tt.holder != "" {
				lease.Spec.HolderIdentity = ptr.To(tt.holder)
			}
			status := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cluster-autoscaler-status", Namespace: c.AutoscalerNamespace},
				Data: map[string]string{"status": fmt.Sprintf("time: %s\nautoscalerStatus: Running\nnodeGroups:\n- name: %s\n- name: %s\n",
					time.Now().UTC().Format(time.RFC3339), c.MainPool, c.ZeroPool)}}
			e := &Environment{Config: c, K8s: fake.NewClientBuilder().WithObjects(deployment, pod, lease, status).Build()}
			if err := e.Controller(context.Background()); (err == nil) != tt.valid {
				t.Fatalf("Controller error=%v, valid=%v", err, tt.valid)
			}
		})
	}
}

func TestCheckLeaderLease(t *testing.T) {
	t.Parallel()
	now := time.Now()
	for _, tt := range []struct {
		name, holder string
		hostNetwork  bool
		age          time.Duration
		valid        bool
	}{
		{name: "exact Pod hostname", holder: "autoscaler-0", valid: true},
		{name: "host-network Node hostname", holder: "control-plane", hostNetwork: true, valid: true},
		{name: "foreign Node hostname", holder: "foreign-node", hostNetwork: true},
		{name: "Node hostname without host networking", holder: "control-plane"},
		{name: "stale lease", holder: "autoscaler-0", age: 2 * time.Minute},
		{name: "future lease", holder: "autoscaler-0", age: -time.Minute},
		{name: "unrecognized Pod prefix", holder: "autoscaler-0_foreign"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "autoscaler-0"},
				Spec: corev1.PodSpec{NodeName: "control-plane", HostNetwork: tt.hostNetwork}}
			lease := coordinationv1.Lease{Spec: coordinationv1.LeaseSpec{
				HolderIdentity: ptr.To(tt.holder), RenewTime: &metav1.MicroTime{Time: now.Add(-tt.age)},
			}}
			if err := checkLeaderLease(pod, lease, now); (err == nil) != tt.valid {
				t.Fatalf("lease error=%v, valid=%v", err, tt.valid)
			}
		})
	}
}

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
		{name: "split timing override", args: []string{base, "--scale-down-unneeded-time", "30m"}},
		{name: "split utilization override", args: []string{base, "--scale-down-utilization-threshold", "0.1"}},
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

func TestCheckPhaseArguments(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, phase string
		args        []string
		valid       bool
	}{
		{name: "default fixture", valid: true},
		{name: "balance requires flags", phase: "balance"},
		{name: "balance flags", phase: "balance", valid: true, args: []string{
			"--balance-similar-node-groups=true", "--balancing-label=acceptance-pool", "--max-nodes-total=4",
			"--parallel-scale-up=false", "--salvo-scale-up=false", "--v=1",
		}},
		{name: "extra balance flag", phase: "balance", args: []string{
			"--balance-similar-node-groups=true", "--balancing-label=acceptance-pool", "--max-nodes-total=4",
			"--parallel-scale-up=false", "--salvo-scale-up=false", "--v=1", "--max-nodes-total=5",
		}},
		{name: "no-join flags", phase: "no-join", valid: true, args: []string{"--max-node-provision-time=3m"}},
		{name: "long provision timeout", phase: "no-join", args: []string{"--max-node-provision-time=15m"}},
		{name: "minimum flag", phase: "minimum", valid: true, args: []string{"--enforce-node-group-min-size=true", "--v=1"}},
		{name: "minimum disabled", phase: "minimum", args: []string{"--enforce-node-group-min-size=false", "--v=1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			c.Phase = tt.phase
			if err := CheckPhaseArguments(tt.args, c); (err == nil) != tt.valid {
				t.Fatalf("CheckPhaseArguments = %v, valid=%t", err, tt.valid)
			}
		})
	}
}

func TestCheckPausedController(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		edit func(*appsv1.Deployment)
		pod  bool
		pass bool
	}{
		{name: "prepared minimum controller", pass: true},
		{name: "already running", edit: func(d *appsv1.Deployment) { d.Spec.Replicas = ptr.To(int32(1)) }},
		{name: "missing minimum flag", edit: func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Containers[0].Args = d.Spec.Template.Spec.Containers[0].Args[:4]
		}},
		{name: "old Pod still running", pod: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			c.Phase = "minimum"
			labels := map[string]string{"app": "autoscaler"}
			container := corev1.Container{Name: c.AutoscalerContainer, Image: c.ExpectedImage,
				Env: []corev1.EnvVar{{Name: "ARM_SUBSCRIPTION_ID", Value: c.SubscriptionID}, {Name: "ARM_RESOURCE_GROUP", Value: c.ResourceGroup}},
				Args: []string{"--node-group-auto-discovery=label:cluster-autoscaler-name=" + c.DiscoveryValue,
					"--scale-down-delay-after-add=10s", "--scale-down-unneeded-time=10s",
					"--unremovable-node-recheck-timeout=10s", "--enforce-node-group-min-size=true", "--v=1"},
			}
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: c.AutoscalerDeployment, Namespace: c.AutoscalerNamespace},
				Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(0)), Selector: &metav1.LabelSelector{MatchLabels: labels},
					Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{container}}}},
			}
			if tt.edit != nil {
				tt.edit(deployment)
			}
			objects := []client.Object{deployment}
			if tt.pod {
				objects = append(objects, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					Name: "old-autoscaler", Namespace: c.AutoscalerNamespace, Labels: labels,
				}})
			}
			e := &Environment{Config: c, K8s: fake.NewClientBuilder().WithObjects(objects...).Build()}
			if err := e.CheckPausedController(context.Background()); (err == nil) != tt.pass {
				t.Fatalf("CheckPausedController = %v, pass=%t", err, tt.pass)
			}
		})
	}
}

func TestCheckNoPendingDemand(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		phase     corev1.PodPhase
		node      string
		deleting  bool
		hasDemand bool
	}{
		{name: "no pending demand", phase: corev1.PodRunning, node: "worker-0"},
		{name: "unscheduled Pod", phase: corev1.PodPending, hasDemand: true},
		{name: "Pod starting on assigned Node", phase: corev1.PodPending, node: "worker-0"},
		{name: "deleting unscheduled Pod", phase: corev1.PodPending, deleting: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default"},
				Spec: corev1.PodSpec{NodeName: tt.node}, Status: corev1.PodStatus{Phase: tt.phase}}
			if tt.deleting {
				now := metav1.Now()
				pod.DeletionTimestamp = &now
				pod.Finalizers = []string{"test/finalizer"}
			}
			e := &Environment{K8s: fake.NewClientBuilder().WithObjects(pod).Build()}
			if err := e.CheckNoPendingDemand(context.Background()); (err != nil) != tt.hasDemand {
				t.Fatalf("CheckNoPendingDemand = %v, hasDemand=%t", err, tt.hasDemand)
			}
		})
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

func TestEnvironmentDeletedGenerationWithReusedInstanceID(t *testing.T) {
	t.Parallel()
	c := testConfig()
	id := normalizeID(c.PoolID(c.ZeroPool) + "/virtualMachines/0")
	original := Instance{ID: id, VMID: "old-vm", NICs: []string{id + "/networkInterfaces/old"}}
	replacement := Instance{ID: id, VMID: "new-vm", NICs: []string{id + "/networkInterfaces/new"}}
	after := Snapshot{Pools: map[string]PoolState{c.ZeroPool: {Instances: map[string]Instance{id: replacement}}}}
	e := &Environment{Cloud: fakeCloud{}}
	if err := e.DeletedGeneration(context.Background(), original, after, c.ZeroPool); err != nil {
		t.Fatalf("reused instance ID must not hide the removed generation: %v", err)
	}
	after.Pools[c.ZeroPool].Instances[id] = original
	if err := e.DeletedGeneration(context.Background(), original, after, c.ZeroPool); err == nil {
		t.Fatal("accepted the same VM generation still present")
	}
	after.Pools[c.ZeroPool].Instances[id] = replacement
	e.Cloud = fakeCloud{exists: true}
	if err := e.DeletedGeneration(context.Background(), original, after, c.ZeroPool); err == nil {
		t.Fatal("accepted the old generation's NIC still present")
	}
	e.Cloud = fakeCloud{}
	after.Nodes = []corev1.Node{{Spec: corev1.NodeSpec{ProviderID: "azure://" + id}}}
	if err := e.DeletedGeneration(context.Background(), original, after, c.ZeroPool); err == nil {
		t.Fatal("accepted an old Node after VM deletion")
	}
	after.Nodes = nil
	replacement.NICs = original.NICs
	after.Pools[c.ZeroPool].Instances[id] = replacement
	e.Cloud = fakeCloud{exists: true}
	if err := e.DeletedGeneration(context.Background(), original, after, c.ZeroPool); err != nil {
		t.Fatalf("replacement reused the old VM and NIC paths: %v", err)
	}
	after.Pools[c.ZeroPool] = PoolState{Instances: map[string]Instance{}}
	if err := e.DeletedGeneration(context.Background(), original, after, c.ZeroPool); err == nil {
		t.Fatal("accepted the reused NIC still present after the final VM was removed")
	}
	original.VMID = ""
	if err := e.DeletedGeneration(context.Background(), original, after, c.ZeroPool); err == nil {
		t.Fatal("accepted a VM without a generation-specific identity")
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
