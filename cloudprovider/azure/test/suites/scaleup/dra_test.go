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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

const (
	draDriver = "gpu.example.com"
	draImage  = "registry.k8s.io/dra-example-driver/dra-example-driver@sha256:728fbb69b99e335cfef2d1b9a3d695d2f502c58dd04f7f81143089a72e4044e3"
)

var _ = Describe("Public synthetic DRA scenarios", Serial, Label("public", "dra", "uniform"), func() {
	BeforeEach(func(ctx SpecContext) {
		checkDRAFixture(ctx)
	})

	It("CA-020 grows main from one to two workers and allocates eight synthetic devices", Label("CA-020"), func(ctx SpecContext) {
		workload := draDeployment(ctx, "dra-pressure", env.Config.MainLabel, 8, 1, false)
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		waitStable(ctx, 2, 0)
		waitDRAWorkload(ctx, workload, 8, 1)
	}, NodeTimeout(40*time.Minute))

	It("CA-021 rejects a five-device claim when each worker offers four", Label("CA-021"), func(ctx SpecContext) {
		workload := draDeployment(ctx, "dra-oversized", env.Config.MainLabel, 1, 5, false)
		Expect(env.K8s.Create(ctx, workload)).To(Succeed())
		pods := waitWorkload(ctx, workload.Name, env.Config.MainPool, 0, 1)
		waitPodEvent(ctx, pods[0], "NotTriggerScaleUp")
		observeNoGrowth(ctx, workload.Name, env.Config.MainPool, 0, 1)
		var claims unstructured.UnstructuredList
		claims.SetGroupVersionKind(schema.GroupVersionKind{Group: "resource.k8s.io", Version: "v1", Kind: "ResourceClaimList"})
		Expect(env.K8s.List(ctx, &claims, client.InNamespace(namespace.Name))).To(Succeed())
		Expect(claims.Items).To(HaveLen(1))
		_, allocated, err := unstructured.NestedMap(claims.Items[0].Object, "status", "allocation")
		Expect(err).NotTo(HaveOccurred())
		Expect(allocated).To(BeFalse())
	}, NodeTimeout(25*time.Minute))

	It("CA-022 physically deletes a worker and reallocates DRA claims to the survivor", Label("CA-022"), func(ctx SpecContext) {
		growth := draDeployment(ctx, "dra-growth", env.Config.MainLabel, 2, 1, true)
		Expect(env.K8s.Create(ctx, growth)).To(Succeed())
		grown := waitStable(ctx, 2, 0)
		waitDRAWorkload(ctx, growth, 2, 1)
		protected := draDeployment(ctx, "dra-drain", env.Config.MainLabel, 2, 2, false)
		Expect(env.K8s.Create(ctx, protected)).To(Succeed())
		waitDRAWorkload(ctx, protected, 2, 2)
		assertDistinctNodes(waitWorkload(ctx, protected.Name, env.Config.MainPool, 2, 0), 2)
		Expect(env.K8s.Delete(ctx, growth)).To(Succeed())
		waitBaselineDeleted(ctx, grown, 0)
		waitDRAWorkload(ctx, protected, 2, 2)
		assertDistinctNodes(waitWorkload(ctx, protected.Name, env.Config.MainPool, 2, 0), 1)
	}, NodeTimeout(55*time.Minute))
})

func resourceObject(kind, name, namespace string) *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(schema.GroupVersionKind{Group: "resource.k8s.io", Version: "v1", Kind: kind})
	object.SetName(name)
	object.SetNamespace(namespace)
	return object
}

func checkDRAFixture(ctx context.Context) {
	var marker corev1.ConfigMap
	Expect(env.K8s.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: environment.MarkerName}, &marker)).To(Succeed())
	Expect(marker.Data["allow-dra-fixture"]).To(Equal("CA-020,CA-021,CA-022"))
	class := resourceObject("DeviceClass", "gpu", "")
	Expect(env.K8s.Get(ctx, client.ObjectKey{Name: "gpu"}, class)).To(Succeed(), "operator-prepared resource.k8s.io/v1 DRA API and DeviceClass required")
	Expect(class.GetLabels()[environment.RunLabel]).To(Equal(env.Config.RunID))
	selectors, found, err := unstructured.NestedSlice(class.Object, "spec", "selectors")
	Expect(err).NotTo(HaveOccurred())
	Expect(found).To(BeTrue())
	Expect(selectors).To(ContainElement(map[string]interface{}{"cel": map[string]interface{}{"expression": "device.driver == 'gpu.example.com'"}}))
	var daemon appsv1.DaemonSet
	Expect(env.K8s.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "dra-example-driver-kubeletplugin"}, &daemon)).To(Succeed())
	Expect(daemon.Labels[environment.RunLabel]).To(Equal(env.Config.RunID))
	Expect(daemon.Status.ObservedGeneration).To(Equal(daemon.Generation))
	Expect(daemon.Status.NumberReady).To(Equal(daemon.Status.DesiredNumberScheduled))
	foundPlugin := false
	for _, container := range daemon.Spec.Template.Spec.Containers {
		if container.Name != "plugin" {
			continue
		}
		foundPlugin = true
		Expect(container.Image).To(Equal(draImage))
		Expect(container.Env).To(ContainElements(
			corev1.EnvVar{Name: "NUM_DEVICES", Value: "4"},
			corev1.EnvVar{Name: "DRIVER_NAME", Value: draDriver},
		))
	}
	Expect(foundPlugin).To(BeTrue())
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
			class := resourceObject("DeviceClass", "gpu", "")
			class.SetLabels(map[string]string{environment.RunLabel: env.Config.RunID})
			class.Object["spec"] = map[string]interface{}{"selectors": []interface{}{
				map[string]interface{}{"cel": map[string]interface{}{"expression": "device.driver == 'gpu.example.com'"}},
			}}
			slice := resourceObject("ResourceSlice", "worker-devices", "")
			slice.Object["spec"] = map[string]interface{}{
				"driver": draDriver, "nodeName": "worker", "pool": map[string]interface{}{"name": "worker"},
				"devices": []interface{}{
					map[string]interface{}{"name": "device-0"}, map[string]interface{}{"name": "device-1"},
					map[string]interface{}{"name": "device-2"}, map[string]interface{}{"name": "device-3"},
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

func draDevices(ctx context.Context) (map[string]string, error) {
	var slices unstructured.UnstructuredList
	slices.SetGroupVersionKind(schema.GroupVersionKind{Group: "resource.k8s.io", Version: "v1", Kind: "ResourceSliceList"})
	if err := env.K8s.List(ctx, &slices); err != nil {
		return nil, err
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
	for _, node := range devices {
		if _, owned := counts[node]; !owned {
			return nil, fmt.Errorf("DRA slice advertises devices outside current owned workers")
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

func draDeployment(ctx context.Context, name, pool string, replicas int32, devices int64, antiAffinity bool) *appsv1.Deployment {
	template := resourceObject("ResourceClaimTemplate", name, namespace.Name)
	template.SetLabels(map[string]string{environment.RunLabel: env.Config.RunID})
	template.Object["spec"] = map[string]interface{}{
		"metadata": map[string]interface{}{"labels": map[string]interface{}{environment.RunLabel: env.Config.RunID}},
		"spec": map[string]interface{}{"devices": map[string]interface{}{"requests": []interface{}{
			map[string]interface{}{"name": "req-1", "exactly": map[string]interface{}{
				"deviceClassName": "gpu", "allocationMode": "ExactCount", "count": devices,
			}},
		}}},
	}
	Expect(env.K8s.Create(ctx, template)).To(Succeed())
	workload := env.Deployment(namespace.Name, name, pool, "10m", replicas)
	if antiAffinity {
		workload = antiAffinityDeployment(name, pool, replicas)
	}
	workload.Spec.Template.Spec.ResourceClaims = []corev1.PodResourceClaim{{Name: "devices", ResourceClaimTemplateName: ptr.To(name)}}
	workload.Spec.Template.Spec.Containers[0].Resources.Claims = []corev1.ResourceClaim{{Name: "devices"}}
	return workload
}

func waitDRAWorkload(ctx context.Context, workload *appsv1.Deployment, replicas, perPod int) {
	waitWorkload(ctx, workload.Name, env.Config.MainPool, replicas, 0)
	Eventually(ctx, func() error {
		devices, err := draDevices(ctx)
		if err != nil {
			return err
		}
		used := map[string]bool{}
		pods := listWorkload(ctx, workload.Name)
		if len(pods) != replicas {
			return fmt.Errorf("DRA workload has %d Pods, want %d", len(pods), replicas)
		}
		for _, pod := range pods {
			if !environment.PodReady(pod) || len(pod.Status.ResourceClaimStatuses) != 1 ||
				pod.Status.ResourceClaimStatuses[0].ResourceClaimName == nil {
				return fmt.Errorf("DRA Pod is not Ready with an actual generated claim")
			}
			name := *pod.Status.ResourceClaimStatuses[0].ResourceClaimName
			claim := resourceObject("ResourceClaim", name, pod.Namespace)
			if err := env.K8s.Get(ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
				return err
			}
			owned := false
			for _, owner := range claim.GetOwnerReferences() {
				owned = owned || owner.Kind == "Pod" && owner.UID == pod.UID
			}
			if !owned || claim.GetLabels()[environment.RunLabel] != env.Config.RunID {
				return fmt.Errorf("DRA claim is not owned by the exact test Pod")
			}
			allocated, err := environment.DRAAllocation(*claim, draDriver, perPod)
			if err != nil {
				return err
			}
			for _, key := range allocated {
				if devices[key] != pod.Spec.NodeName || used[key] {
					return fmt.Errorf("claim allocation is duplicated or does not match its actual worker")
				}
				used[key] = true
			}
		}
		return nil
	}, 5*time.Minute, pollInterval).Should(Succeed())
	AddReportEntry("dra-allocation", fmt.Sprintf("%d Ready Pods, %d unique allocated synthetic devices, verified against worker ResourceSlices", replicas, replicas*perPod))
}
