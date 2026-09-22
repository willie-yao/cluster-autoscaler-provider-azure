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

package scaleup_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

type observationCloud struct {
	read func() (environment.Snapshot, error)
}

func (c observationCloud) Read(context.Context) (environment.Snapshot, error) { return c.read() }
func (c observationCloud) NICExists(context.Context, string) (bool, error) {
	return false, fmt.Errorf("unexpected NIC read in observation test")
}

func observationSnapshot(c environment.Config) environment.Snapshot {
	id := strings.ToLower(c.PoolID(c.MainPool) + "/virtualMachines/0")
	ready := corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}
	return environment.Snapshot{
		Pools: map[string]environment.PoolState{
			c.MainPool: {Capacity: 1, Instances: map[string]environment.Instance{id: {ID: id}}},
			c.ZeroPool: {Capacity: 0, Instances: map[string]environment.Instance{}},
		},
		Nodes: []corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{c.PoolLabel: c.MainLabel}},
				Spec: corev1.NodeSpec{ProviderID: "azure://" + id}, Status: ready},
			{ObjectMeta: metav1.ObjectMeta{Name: "cp"}, Spec: corev1.NodeSpec{ProviderID: "azure://" + c.ControlPlaneID}, Status: ready},
		},
		VMs: 2, VCPUs: 4,
	}
}

func observationEnvironment(t *testing.T) {
	t.Helper()
	previous := env
	t.Cleanup(func() { env = previous })
	c := environment.Config{
		RunID: "test-run", SubscriptionID: "test-subscription", ResourceGroup: "test-workers",
		ControlPlaneID: "/subscriptions/test-subscription/resourceGroups/test-control/providers/Microsoft.Compute/virtualMachines/cp",
		MainPool:       "main", ZeroPool: "zero", PoolLabel: "test-pool", MainLabel: "main", ZeroLabel: "zero",
		AutoscalerNamespace: "kube-system", AutoscalerDeployment: "autoscaler", AutoscalerContainer: "autoscaler",
		LeaseName: "autoscaler", ExpectedImage: "candidate@sha256:abc", DiscoveryValue: "test-run",
	}
	snapshot := observationSnapshot(c)
	env = &environment.Environment{
		Config: c, K8s: fake.NewClientBuilder().WithObjects(&snapshot.Nodes[0], &snapshot.Nodes[1]).Build(),
		Cloud: observationCloud{read: func() (environment.Snapshot, error) { return observationSnapshot(c), nil }},
	}
}

func TestReadSnapshotPolling(t *testing.T) {
	for _, tt := range []struct {
		name      string
		change    func(*environment.Snapshot)
		readError error
		k8sError  bool
		terminal  bool
	}{
		{name: "VM envelope", terminal: true, change: func(s *environment.Snapshot) { s.VMs = 5 }},
		{name: "vCPU envelope", terminal: true, change: func(s *environment.Snapshot) { s.VCPUs = 10 }},
		{name: "main desired maximum", terminal: true, change: func(s *environment.Snapshot) {
			pool := s.Pools["main"]
			pool.Capacity = 3
			s.Pools["main"] = pool
		}},
		{name: "zero desired maximum", terminal: true, change: func(s *environment.Snapshot) {
			s.Pools["zero"] = environment.PoolState{Capacity: 2}
		}},
		{name: "main actual maximum", terminal: true, change: func(s *environment.Snapshot) {
			s.Pools["main"].Instances["extra-1"] = environment.Instance{}
			s.Pools["main"].Instances["extra-2"] = environment.Instance{}
		}},
		{name: "main actual minimum", terminal: true, change: func(s *environment.Snapshot) {
			s.Pools["main"] = environment.PoolState{Capacity: 1, Instances: map[string]environment.Instance{}}
		}},
		{name: "zero breach before main convergence", terminal: true, change: func(s *environment.Snapshot) {
			delete(s.Pools, "main")
			s.Pools["zero"] = environment.PoolState{Capacity: 1, Instances: map[string]environment.Instance{"one": {}, "two": {}}}
		}},
		{name: "bound before Kubernetes read error", terminal: true, k8sError: true, change: func(s *environment.Snapshot) { s.VCPUs = 10 }},
		{name: "wrapped cloud bound", terminal: true, readError: fmt.Errorf("cloud: %w", environment.ErrBounds)},
		{name: "ordinary cloud read", readError: fmt.Errorf("temporary cloud transport failure")},
		{name: "ordinary Kubernetes read", k8sError: true},
		{name: "missing pool observation", change: func(s *environment.Snapshot) { delete(s.Pools, "main") }},
		{name: "ordinary convergence", change: func(s *environment.Snapshot) {
			pool := s.Pools["main"]
			pool.Capacity = 2
			s.Pools["main"] = pool
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, withAssertions := range []bool{false, true} {
				t.Run(fmt.Sprintf("gomega-callback=%t", withAssertions), func(t *testing.T) {
					observationEnvironment(t)
					reads := 0
					env.Cloud = observationCloud{read: func() (environment.Snapshot, error) {
						reads++
						snapshot := observationSnapshot(env.Config)
						if reads == 1 {
							if tt.change != nil {
								tt.change(&snapshot)
							}
							return snapshot, tt.readError
						}
						return snapshot, nil
					}}
					if tt.k8sError {
						snapshot := observationSnapshot(env.Config)
						env.K8s = fake.NewClientBuilder().WithObjects(&snapshot.Nodes[0], &snapshot.Nodes[1]).WithInterceptorFuncs(interceptor.Funcs{
							List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
								if reads == 1 {
									return fmt.Errorf("temporary Kubernetes read failure")
								}
								return c.List(ctx, list, opts...)
							},
						}).Build()
					}
					var failures []string
					g := NewGomega(func(message string, _ ...int) { failures = append(failures, message) })
					if withAssertions {
						g.Eventually(func(g Gomega) {
							snapshot, err := readSnapshot(context.Background())
							g.Expect(err).To(Succeed())
							g.Expect(snapshot.Stable(env.Config, 1, 0)).To(Succeed())
						}, time.Second, time.Millisecond).Should(Succeed())
					} else {
						g.Eventually(func() error {
							snapshot, err := readSnapshot(context.Background())
							if err != nil {
								return err
							}
							return snapshot.Stable(env.Config, 1, 0)
						}, time.Second, time.Millisecond).Should(Succeed())
					}
					wantReads := 2
					if tt.terminal {
						wantReads = 1
					}
					if (len(failures) > 0) != tt.terminal || reads != wantReads {
						t.Fatalf("reads=%d, want %d; terminal=%t, failures=%v", reads, wantReads, tt.terminal, failures)
					}
				})
			}
		})
	}
}

func TestReadActiveSnapshotRejectsControllerLoss(t *testing.T) {
	for _, state := range []string{"missing Pod", "stale lease", "stale status"} {
		t.Run(state, func(t *testing.T) {
			observationEnvironment(t)
			ctx := context.Background()
			c := env.Config
			container := corev1.Container{Name: c.AutoscalerContainer, Image: c.ExpectedImage,
				Env: []corev1.EnvVar{{Name: "ARM_SUBSCRIPTION_ID", Value: c.SubscriptionID}, {Name: "ARM_RESOURCE_GROUP", Value: c.ResourceGroup}},
				Args: []string{"--node-group-auto-discovery=label:cluster-autoscaler-name=" + c.DiscoveryValue,
					"--scale-down-delay-after-add=10s", "--scale-down-unneeded-time=10s", "--unremovable-node-recheck-timeout=10s"},
			}
			labels := map[string]string{"app": "autoscaler"}
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: c.AutoscalerDeployment, Namespace: c.AutoscalerNamespace},
				Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: labels},
					Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{container}}}},
				Status: appsv1.DeploymentStatus{ReadyReplicas: 1, UpdatedReplicas: 1},
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "autoscaler-0", Namespace: c.AutoscalerNamespace, Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{container}},
				Status: corev1.PodStatus{Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
			}
			lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: c.LeaseName, Namespace: c.AutoscalerNamespace},
				Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To(pod.Name), RenewTime: &metav1.MicroTime{Time: time.Now()}}}
			status := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cluster-autoscaler-status", Namespace: c.AutoscalerNamespace},
				Data: map[string]string{"status": fmt.Sprintf("time: %s\nautoscalerStatus: Running\nnodeGroups:\n- name: %s\n- name: %s\n",
					time.Now().UTC().Format(time.RFC3339), c.MainPool, c.ZeroPool)}}
			for _, object := range []client.Object{deployment, pod, lease, status} {
				if err := env.K8s.Create(ctx, object); err != nil {
					t.Fatal(err)
				}
			}
			observations := 0
			var failures []string
			g := NewGomega(func(message string, _ ...int) { failures = append(failures, message) })
			g.Consistently(func() error {
				observations++
				snapshot, err := readActiveSnapshot(ctx)
				if err != nil {
					return err
				}
				if err := snapshot.Stable(c, 1, 0); err != nil {
					return err
				}
				if observations == 1 {
					switch state {
					case "missing Pod":
						err = env.K8s.Delete(ctx, pod)
					case "stale lease":
						lease.Spec.RenewTime.Time = time.Now().Add(-2 * time.Minute)
						err = env.K8s.Update(ctx, lease)
					case "stale status":
						status.Data["status"] = fmt.Sprintf("time: %s\nautoscalerStatus: Running\nnodeGroups:\n- name: %s\n- name: %s\n",
							time.Now().Add(-3*time.Minute).UTC().Format(time.RFC3339), c.MainPool, c.ZeroPool)
						err = env.K8s.Update(ctx, status)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				return nil
			}, time.Second, time.Millisecond).Should(Succeed())
			if observations != 2 || len(failures) != 1 {
				t.Fatalf("initially healthy then %s: observations=%d, failures=%v", state, observations, failures)
			}
		})
	}
}
