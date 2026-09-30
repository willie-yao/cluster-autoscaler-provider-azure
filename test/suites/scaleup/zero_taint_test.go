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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"github.com/Azure/cluster-autoscaler-provider-azure/test/pkg/environment"
)

var _ = Describe("Azure Provider", Serial, func() {
	It("scales a tainted node group up from zero only for Pods that tolerate the taint", Label("taint", "scale-from-zero", "Slow"), func(ctx SpecContext) {
		baseline := waitStable(ctx, 1, 0)
		Expect(environment.CheckZeroPoolTaint(baseline, env.Config)).To(Succeed())
		By("creating zero-pool demand without the toleration")
		blocked := env.Deployment(namespace.Name, "blocked-taint", env.Config.ZeroLabel, "50m", 1)
		Expect(env.K8s.Create(ctx, blocked)).To(Succeed())
		pods := waitWorkload(ctx, blocked.Name, env.Config.ZeroPool, 0, 1)
		By("checking that the zero pool does not grow for it")
		waitPodEvent(ctx, pods[0], "NotTriggerScaleUp")
		Consistently(ctx, func() error {
			snapshot, err := readActiveSnapshot(ctx)
			if err != nil {
				return err
			}
			if err := snapshot.Stable(env.Config, 1, 0); err != nil {
				return err
			}
			if err := environment.CheckZeroPoolTaint(snapshot, env.Config); err != nil {
				return err
			}
			_, err = env.WorkloadState(ctx, namespace.Name, blocked.Name, env.Config.ZeroPool, 0, 1)
			return err
		}, 5*time.Minute, pollInterval).Should(Succeed())

		By("creating zero-pool demand with the toleration and waiting for growth")
		tolerated := env.Deployment(namespace.Name, "tolerated-taint", env.Config.ZeroLabel, "50m", 1)
		tolerated.Spec.Template.Spec.Tolerations = []corev1.Toleration{{
			Key: environment.RunLabel, Operator: corev1.TolerationOpEqual,
			Value: env.Config.RunID, Effect: corev1.TaintEffectNoSchedule,
		}}
		Expect(env.K8s.Create(ctx, tolerated)).To(Succeed())
		grown := waitStable(ctx, 1, 1)
		Expect(environment.CheckZeroPoolTaint(grown, env.Config)).To(Succeed())
		Expect(environment.CheckZeroNodeTaint(grown, env.Config)).To(Succeed())
		waitWorkload(ctx, tolerated.Name, env.Config.ZeroPool, 1, 0)
		waitWorkload(ctx, blocked.Name, env.Config.ZeroPool, 0, 1)
		By("removing the demand and waiting for the zero-pool worker to be deleted")
		Expect(env.K8s.Delete(ctx, blocked)).To(Succeed())
		Expect(env.K8s.Delete(ctx, tolerated)).To(Succeed())
		waitDeleted(ctx, grown, env.Config.ZeroPool, 1, 1, 0)
	}, NodeTimeout(45*time.Minute))
})
