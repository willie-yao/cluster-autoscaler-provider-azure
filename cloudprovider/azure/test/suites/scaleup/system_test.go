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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

var _ = Describe("Public synthetic system-namespace drain", Serial, Label("public", "uniform"), func() {
	It("CA-011 drains only run-owned synthetic kube-system Pods protected by a PDB", Label("CA-011", "system-fixture"), func(ctx SpecContext) {
		checkSystemFixture(ctx, map[string]string{environment.RunLabel: env.Config.RunID, "app": "drain-" + namespace.Name})
		drain(ctx, 2, 1, "kube-system")
	}, NodeTimeout(55*time.Minute))
})

func checkSystemFixture(ctx context.Context, podLabels map[string]string) {
	var marker corev1.ConfigMap
	Expect(env.K8s.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: environment.MarkerName}, &marker)).To(Succeed())
	Expect(marker.Data["allow-kube-system-fixture"]).To(Equal("CA-011"), "operator must explicitly authorize this synthetic system-namespace case")
	var budgets policyv1.PodDisruptionBudgetList
	Expect(env.K8s.List(ctx, &budgets, client.InNamespace("kube-system"))).To(Succeed())
	for _, budget := range budgets.Items {
		if budget.Spec.Selector == nil {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(budget.Spec.Selector)
		Expect(err).NotTo(HaveOccurred())
		Expect(selector.Matches(labels.Set(podLabels))).To(BeFalse(), "synthetic labels must not overlap any existing system PDB")
	}
}

func cleanupSystemObject(object client.Object) {
	key, uid := client.ObjectKeyFromObject(object), object.GetUID()
	DeferCleanup(func(ctx SpecContext) {
		Expect(env.Authorize(ctx)).To(Succeed())
		var current client.Object
		switch object.(type) {
		case *appsv1.Deployment:
			current = &appsv1.Deployment{}
		case *policyv1.PodDisruptionBudget:
			current = &policyv1.PodDisruptionBudget{}
		default:
			Fail("unsupported synthetic system object")
		}
		err := env.K8s.Get(ctx, key, current)
		if apierrors.IsNotFound(err) {
			return
		}
		Expect(err).NotTo(HaveOccurred())
		Expect(current.GetUID()).To(Equal(uid))
		Expect(current.GetLabels()[environment.RunLabel]).To(Equal(env.Config.RunID))
		Expect(env.K8s.Delete(ctx, current, client.Preconditions{UID: &uid})).To(Succeed())
		if _, deployment := object.(*appsv1.Deployment); deployment {
			Eventually(ctx, func() int { return len(listWorkloadIn(ctx, key.Namespace, key.Name)) }, 4*time.Minute, pollInterval).Should(BeZero())
		}
	}, NodeTimeout(5*time.Minute))
}
