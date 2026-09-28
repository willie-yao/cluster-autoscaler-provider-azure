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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

const (
	draDriver = "gpu.example.com"
	draImage  = "registry.k8s.io/dra-example-driver/dra-example-driver@sha256:728fbb69b99e335cfef2d1b9a3d695d2f502c58dd04f7f81143089a72e4044e3"
)

var draDeviceSelector = resourcev1.DeviceSelector{CEL: &resourcev1.CELDeviceSelector{Expression: "device.driver == 'gpu.example.com'"}}

var _ = Describe("Public synthetic DRA scenarios", Serial, Label("public", "dra", "uniform"), func() {
	BeforeEach(func(ctx SpecContext) {
		checkDRAFixture(ctx)
	})

	It("CA-020 grows main from one to two workers and allocates eight synthetic devices", Label("CA-020", "Feature:ClusterSizeAutoscalingScaleUp", "Slow"), func(ctx SpecContext) {
		By("creating eight Pods that each claim one device")
		workload := draDeployment(ctx, "dra-pressure", env.Config.MainLabel, 8, 1, false)
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		By("waiting for main to grow and allocate eight devices")
		waitStable(ctx, 2, 0)
		waitDRAWorkload(ctx, workload, 8, 1)
	}, NodeTimeout(40*time.Minute))

	It("CA-021 does not scale up for a five-device claim when each worker offers four", Label("CA-021", "Feature:ClusterSizeAutoscalingScaleUp"), func(ctx SpecContext) {
		By("creating a Pod that claims five devices")
		workload := draDeployment(ctx, "dra-oversized", env.Config.MainLabel, 1, 5, false)
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		pods := waitWorkload(ctx, workload.Name, env.Config.MainPool, 0, 1)
		By("waiting for the NotTriggerScaleUp event and checking that no worker is added")
		waitPodEvent(ctx, pods[0], "NotTriggerScaleUp")
		observeNoGrowth(ctx, workload.Name, env.Config.MainPool, 0, 1)
		var claims resourcev1.ResourceClaimList
		Expect(env.K8s.List(ctx, &claims, client.InNamespace(namespace.Name))).To(Succeed())
		Expect(claims.Items).To(HaveLen(1))
		Expect(claims.Items[0].Status.Allocation).To(BeNil(), "claim %s must stay unallocated", claims.Items[0].Name)
	}, NodeTimeout(25*time.Minute))

	It("CA-022 physically deletes a worker and reallocates DRA claims to the survivor", Label("CA-022", "Feature:ClusterSizeAutoscalingScaleDown", "Slow"), func(ctx SpecContext) {
		By("growing main to two workers with one device claim each")
		growth := draDeployment(ctx, "dra-growth", env.Config.MainLabel, 2, 1, true)
		Expect(env.K8s.Create(ctx, growth)).To(Succeed())
		grown := waitStable(ctx, 2, 0)
		waitDRAWorkload(ctx, growth, 2, 1)
		By("running two Pods that each claim two devices")
		protected := draDeployment(ctx, "dra-drain", env.Config.MainLabel, 2, 2, false)
		Expect(env.K8s.Create(ctx, protected)).To(Succeed())
		waitDRAWorkload(ctx, protected, 2, 2)
		assertDistinctNodes(waitWorkload(ctx, protected.Name, env.Config.MainPool, 2, 0), 2)
		By("removing the growth demand and waiting for one worker to be deleted")
		Expect(env.K8s.Delete(ctx, growth)).To(Succeed())
		waitBaselineDeleted(ctx, grown, 0, nil)
		By("checking that both claims are allocated again on the surviving worker")
		waitDRAWorkload(ctx, protected, 2, 2)
		assertDistinctNodes(waitWorkload(ctx, protected.Name, env.Config.MainPool, 2, 0), 1)
	}, NodeTimeout(55*time.Minute))
})

// checkDRAFixture checks the operator marker, the run-owned DeviceClass, the
// pinned driver DaemonSet and four synthetic devices on every worker.
func checkDRAFixture(ctx context.Context) {
	var marker corev1.ConfigMap
	Expect(env.K8s.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: environment.MarkerName}, &marker)).To(Succeed())
	Expect(marker.Data["allow-dra-fixture"]).To(Equal("CA-020,CA-021,CA-022"))
	var class resourcev1.DeviceClass
	Expect(env.K8s.Get(ctx, client.ObjectKey{Name: "gpu"}, &class)).To(Succeed(), "operator-prepared resource.k8s.io/v1 DRA API and DeviceClass required")
	Expect(class.Labels[environment.RunLabel]).To(Equal(env.Config.RunID))
	Expect(class.Spec.Selectors).To(ContainElement(draDeviceSelector))
	var daemon appsv1.DaemonSet
	Expect(env.K8s.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "dra-example-driver-kubeletplugin"}, &daemon)).To(Succeed())
	Expect(daemon.Labels[environment.RunLabel]).To(Equal(env.Config.RunID))
	Expect(daemon.Status.ObservedGeneration).To(Equal(daemon.Generation))
	Expect(daemon.Status.NumberReady).To(Equal(daemon.Status.DesiredNumberScheduled))
	var plugin *corev1.Container
	for i := range daemon.Spec.Template.Spec.Containers {
		if daemon.Spec.Template.Spec.Containers[i].Name == "plugin" {
			plugin = &daemon.Spec.Template.Spec.Containers[i]
		}
	}
	Expect(plugin).NotTo(BeNil(), "the DRA driver DaemonSet must have a plugin container")
	Expect(plugin.Image).To(Equal(draImage))
	Expect(plugin.Env).To(ContainElements(
		corev1.EnvVar{Name: "NUM_DEVICES", Value: "4"},
		corev1.EnvVar{Name: "DRIVER_NAME", Value: draDriver},
	))
	Eventually(ctx, func() error { _, err := draDevices(ctx); return err }, 3*time.Minute, pollInterval).Should(Succeed())
}

func TestDRAFixtureImage(t *testing.T) {
	RegisterFailHandler(Fail)
	const approved = "registry.k8s.io/dra-example-driver/dra-example-driver@sha256:728fbb69b99e335cfef2d1b9a3d695d2f502c58dd04f7f81143089a72e4044e3"
	for _, tt := range []struct {
		name, image string
		valid       bool
	}{
		{name: "approved immutable image", image: approved, valid: true},
		{name: "mutable release tag", image: "registry.k8s.io/dra-example-driver/dra-example-driver:v0.2.1"},
		{name: "wrong digest", image: "registry.k8s.io/dra-example-driver/dra-example-driver@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"},
		{name: "arbitrary image", image: "example.invalid/driver:latest"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			observationEnvironment(t)
			ctx := context.Background()
			class := &resourcev1.DeviceClass{
				ObjectMeta: metav1.ObjectMeta{Name: "gpu", Labels: map[string]string{environment.RunLabel: env.Config.RunID}},
				Spec:       resourcev1.DeviceClassSpec{Selectors: []resourcev1.DeviceSelector{draDeviceSelector}},
			}
			slice := &resourcev1.ResourceSlice{
				ObjectMeta: metav1.ObjectMeta{Name: "worker-devices"},
				Spec: resourcev1.ResourceSliceSpec{
					Driver: draDriver, NodeName: ptr.To("worker"), Pool: resourcev1.ResourcePool{Name: "worker"},
					Devices: []resourcev1.Device{{Name: "device-0"}, {Name: "device-1"}, {Name: "device-2"}, {Name: "device-3"}},
				},
			}
			daemon := &appsv1.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: "dra-example-driver-kubeletplugin", Namespace: "kube-system",
					Labels: map[string]string{environment.RunLabel: env.Config.RunID}},
				Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "plugin", Image: tt.image, Env: []corev1.EnvVar{{Name: "NUM_DEVICES", Value: "4"}, {Name: "DRIVER_NAME", Value: draDriver}},
				}}}}},
				Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 1, NumberReady: 1},
			}
			marker := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: environment.MarkerName, Namespace: "kube-system"},
				Data: map[string]string{"allow-dra-fixture": "CA-020,CA-021,CA-022"}}
			for _, object := range []client.Object{class, slice, daemon, marker} {
				if err := env.K8s.Create(ctx, object); err != nil {
					t.Fatal(err)
				}
			}
			failures := InterceptGomegaFailures(func() { checkDRAFixture(ctx) })
			wantFailures := 1
			if tt.valid {
				wantFailures = 0
			}
			if len(failures) != wantFailures {
				t.Fatalf("valid=%t, failures=%v", tt.valid, failures)
			}
		})
	}
}

// draDevices maps each synthetic device to its worker. It returns an error
// unless every current main or zero worker, and only those, has four devices.
func draDevices(ctx context.Context) (map[string]string, error) {
	var slices resourcev1.ResourceSliceList
	if err := env.K8s.List(ctx, &slices); err != nil {
		return nil, fmt.Errorf("list ResourceSlices: %w", err)
	}
	devices, err := environment.DRADevices(slices.Items, draDriver)
	if err != nil {
		return nil, err
	}
	snapshot, err := readSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, node := range environment.WorkerNodes(snapshot.Nodes, env.Config) {
		counts[node.Name] = 0
	}
	for device, node := range devices {
		if _, owned := counts[node]; !owned {
			return nil, fmt.Errorf("DRA slice advertises device %s on Node %s, outside current owned workers", device, node)
		}
		counts[node]++
	}
	for node, count := range counts {
		if count != 4 {
			return nil, fmt.Errorf("worker %s has %d synthetic devices, want four", node, count)
		}
	}
	return devices, nil
}

// draDeployment creates a ResourceClaimTemplate that requests devices from
// the gpu DeviceClass and returns a Deployment whose Pods each use a claim
// from it.
func draDeployment(ctx context.Context, name, pool string, replicas int32, devices int64, antiAffinity bool) *appsv1.Deployment {
	labels := map[string]string{environment.RunLabel: env.Config.RunID}
	template := &resourcev1.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace.Name, Labels: labels},
		Spec: resourcev1.ResourceClaimTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
				Name: "req-1", Exactly: &resourcev1.ExactDeviceRequest{
					DeviceClassName: "gpu", AllocationMode: resourcev1.DeviceAllocationModeExactCount, Count: devices,
				},
			}}}},
		},
	}
	Expect(env.K8s.Create(ctx, template)).To(Succeed())
	var workload *appsv1.Deployment
	if antiAffinity {
		workload = antiAffinityDeployment(name, pool, replicas)
	} else {
		workload = env.Deployment(namespace.Name, name, pool, "10m", replicas)
	}
	workload.Spec.Template.Spec.ResourceClaims = []corev1.PodResourceClaim{{Name: "devices", ResourceClaimTemplateName: ptr.To(name)}}
	workload.Spec.Template.Spec.Containers[0].Resources.Claims = []corev1.ResourceClaim{{Name: "devices"}}
	return workload
}

// waitDRAWorkload waits until replicas Pods of workload are Ready on main,
// each with its own generated claim that holds perPod devices on the Pod's
// worker, and no device is shared.
func waitDRAWorkload(ctx context.Context, workload *appsv1.Deployment, replicas, perPod int) {
	waitWorkload(ctx, workload.Name, env.Config.MainPool, replicas, 0)
	Eventually(ctx, func() error {
		devices, err := draDevices(ctx)
		if err != nil {
			return err
		}
		used := map[string]bool{}
		pods, err := listWorkload(ctx, workload.Name)
		if err != nil {
			return err
		}
		if len(pods) != replicas {
			return fmt.Errorf("DRA workload has %d Pods, want %d", len(pods), replicas)
		}
		for _, pod := range pods {
			if !environment.PodReady(pod) || len(pod.Status.ResourceClaimStatuses) != 1 ||
				pod.Status.ResourceClaimStatuses[0].ResourceClaimName == nil {
				return fmt.Errorf("DRA Pod %s is not Ready with an actual generated claim, phase %s", pod.Name, pod.Status.Phase)
			}
			name := *pod.Status.ResourceClaimStatuses[0].ResourceClaimName
			var claim resourcev1.ResourceClaim
			if err := env.K8s.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: name}, &claim); err != nil {
				return fmt.Errorf("get claim %s: %w", name, err)
			}
			owned := false
			for _, owner := range claim.OwnerReferences {
				owned = owned || owner.Kind == "Pod" && owner.UID == pod.UID
			}
			if !owned || claim.Labels[environment.RunLabel] != env.Config.RunID {
				return fmt.Errorf("DRA claim %s is not owned by test Pod %s", name, pod.Name)
			}
			allocated, err := environment.DRAAllocation(claim, draDriver, perPod)
			if err != nil {
				return err
			}
			for _, key := range allocated {
				if devices[key] != pod.Spec.NodeName || used[key] {
					return fmt.Errorf("claim %s allocation of %s is duplicated or is on Node %q instead of %s",
						name, key, devices[key], pod.Spec.NodeName)
				}
				used[key] = true
			}
		}
		return nil
	}, 5*time.Minute, pollInterval).Should(Succeed())
	AddReportEntry("dra-allocation", fmt.Sprintf("%d Ready Pods, %d unique allocated synthetic devices, verified against worker ResourceSlices", replicas, replicas*perPod))
}
