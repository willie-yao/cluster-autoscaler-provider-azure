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

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

var _ = Describe("Zero-pool template taint", Serial, Label("uniform"), func() {
	It("AZ-P1-005 ignores demand without a matching toleration and grows for tolerated demand", Label("AZ-P1-005", "zero", "taint", "delete"), func(ctx SpecContext) {
		baseline := waitStable(ctx, 1, 0)
		Expect(environment.CheckZeroPoolTaint(baseline, env.Config)).To(Succeed())
		blocked := env.Deployment(namespace.Name, "blocked-taint", env.Config.ZeroLabel, "50m", 1)
		Expect(env.K8s.Create(ctx, blocked)).To(Succeed())
		pods := waitWorkload(ctx, blocked.Name, env.Config.ZeroPool, 0, 1)
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
		Expect(env.K8s.Delete(ctx, blocked)).To(Succeed())
		Expect(env.K8s.Delete(ctx, tolerated)).To(Succeed())
		waitDeleted(ctx, grown, env.Config.ZeroPool, 1, 1, 0)
	}, NodeTimeout(45*time.Minute))
})
