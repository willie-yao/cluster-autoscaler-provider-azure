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

package scalephase_test

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

var _ = Describe("Local Pod storage", Serial, Label("Slow"), func() {
	It("AZ-P1-015 protects or evicts an EmptyDir Pod as configured", Label("AZ-P1-015", "local-storage", "delete"), func(ctx SpecContext) {
		f := setup(ctx, "local-storage", "AZ-P1-015")
		c := f.env.Config
		Expect(f.env.Controller(ctx)).To(Succeed())
		baseline := f.stable(ctx, map[string]int{c.MainPool: 1, c.ZeroPool: 0})
		Expect(f.env.CheckWorkerIsolation(ctx, baseline, f.namespace.Name)).To(Succeed())
		Expect(f.env.DemandFitsWithHeadroom(ctx, baseline, c.MainPool, 100)).To(Succeed())
		AddReportEntry("skip-local-storage", *c.SkipLocalStorage)

		By("growing main to two workers")
		growth := f.env.Deployment(f.namespace.Name, "local-growth", c.MainLabel, c.DemandCPU, 2)
		Expect(f.env.K8s.Create(ctx, growth)).To(Succeed())
		grown := f.stable(ctx, map[string]int{c.MainPool: 2, c.ZeroPool: 0})
		var candidate, survivor corev1.Node
		for _, node := range environment.PoolNodes(grown.Nodes, c.PoolID(c.MainPool)) {
			if _, old := baseline.Pools[c.MainPool].Instances[nodeProviderID(node)]; old {
				survivor = node
			} else {
				candidate = node
			}
		}
		Expect(candidate.Name).NotTo(BeEmpty())
		Expect(survivor.Name).NotTo(BeEmpty())
		AddReportEntry("local-storage-target", struct {
			Node string
			UID  string
			VM   string
		}{candidate.Name, string(candidate.UID), candidate.Spec.ProviderID})

		By("starting an EmptyDir Pod on the new worker and an anchor Pod on the old one")
		local := f.env.Deployment(f.namespace.Name, "local-disk", c.MainLabel, "50m", 1)
		local.Spec.Template.Spec.Volumes = []corev1.Volume{{
			Name: "local", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}}
		local.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "local", MountPath: "/data"}}
		local.Spec.Template.Spec.Containers[0].Command = []string{
			"sh", "-c", "printf owned > /data/marker; " + environment.InfiniteSleepCommand,
		}
		local.Spec.Template.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{
				Weight: 100, Preference: corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{candidate.Name},
				}}},
			}},
		}}
		Expect(f.env.K8s.Create(ctx, local)).To(Succeed())
		var original corev1.Pod
		Eventually(ctx, func() error {
			pods, err := f.env.WorkloadState(ctx, f.namespace.Name, local.Name, c.MainPool, 1, 0)
			if err != nil {
				return err
			}
			original = pods[0]
			if original.Spec.NodeName != candidate.Name {
				return fmt.Errorf("local storage Pod must start on the removable worker %s, got %s", candidate.Name, original.Spec.NodeName)
			}
			return nil
		}, 5*time.Minute, pollInterval).Should(Succeed())

		anchor := f.env.Deployment(f.namespace.Name, "local-survivor", c.MainLabel, "50m", 1)
		anchor.Spec.Template.Spec.NodeSelector[corev1.LabelHostname] = survivor.Name
		anchor.Spec.Template.Annotations = map[string]string{"cluster-autoscaler.kubernetes.io/safe-to-evict": "false"}
		Expect(f.env.K8s.Create(ctx, anchor)).To(Succeed())
		Eventually(ctx, func() error {
			pods, err := f.env.WorkloadState(ctx, f.namespace.Name, anchor.Name, c.MainPool, 1, 0)
			if err != nil {
				return err
			}
			if pods[0].Spec.NodeName != survivor.Name {
				return fmt.Errorf("anchor Pod runs on %s instead of the baseline worker %s", pods[0].Spec.NodeName, survivor.Name)
			}
			return nil
		}, 5*time.Minute, pollInterval).Should(Succeed())
		Expect(f.env.K8s.Delete(ctx, growth)).To(Succeed())
		if *c.SkipLocalStorage {
			By("checking that the EmptyDir Pod keeps its worker for five minutes")
			Consistently(ctx, func() error {
				snapshot, err := f.active(ctx)
				if err != nil {
					return err
				}
				if err := snapshot.StablePools(c, map[string]int{c.MainPool: 2, c.ZeroPool: 0}); err != nil {
					return err
				}
				pods, err := f.env.WorkloadState(ctx, f.namespace.Name, local.Name, c.MainPool, 1, 0)
				if err != nil {
					return err
				}
				if pods[0].UID != original.UID || pods[0].Spec.NodeName != candidate.Name {
					return fmt.Errorf("local storage Pod moved while protection was enabled")
				}
				return nil
			}, 5*time.Minute, pollInterval).Should(Succeed())
			AddReportEntry("local-storage-blocked", "EmptyDir Pod stayed on the protected worker for five minutes")
			By("removing the EmptyDir Pod and waiting for its worker to be deleted")
			Expect(f.env.K8s.Delete(ctx, local)).To(Succeed())
			Eventually(ctx, func() error {
				after, err := f.active(ctx)
				if err != nil {
					return err
				}
				if err := after.StablePools(c, map[string]int{c.MainPool: 1, c.ZeroPool: 0}); err != nil {
					return err
				}
				if _, exists := after.Pools[c.MainPool].Instances[nodeProviderID(survivor)]; !exists {
					return fmt.Errorf("baseline worker was removed instead of the former local-storage worker")
				}
				return f.env.Deleted(ctx, grown, after, c.MainPool, 1)
			}, waitTimeout, pollInterval).Should(Succeed())
		} else {
			By("waiting for the EmptyDir worker to be deleted and the Pod to move")
			Eventually(ctx, func() error {
				after, err := f.active(ctx)
				if err != nil {
					return err
				}
				if err := after.StablePools(c, map[string]int{c.MainPool: 1, c.ZeroPool: 0}); err != nil {
					return err
				}
				if err := f.env.Deleted(ctx, grown, after, c.MainPool, 1); err != nil {
					return err
				}
				pods, err := f.env.WorkloadState(ctx, f.namespace.Name, local.Name, c.MainPool, 1, 0)
				if err != nil {
					return err
				}
				if pods[0].UID == original.UID || pods[0].Spec.NodeName != survivor.Name {
					return fmt.Errorf("local storage Pod did not restart on the surviving worker")
				}
				return nil
			}, waitTimeout, pollInterval).Should(Succeed())
			AddReportEntry("local-storage-moved", "the EmptyDir Pod restarted on the survivor after physical VM, Node and NIC deletion")
			Expect(f.env.K8s.Delete(ctx, local)).To(Succeed())
		}
		Expect(f.env.K8s.Delete(ctx, anchor)).To(Succeed())
		Eventually(ctx, func() error {
			snapshot, err := f.read(ctx)
			if err != nil {
				return err
			}
			return snapshot.StablePools(c, map[string]int{c.MainPool: 1, c.ZeroPool: 0})
		}, waitTimeout, pollInterval).Should(Succeed())
	}, NodeTimeout(65*time.Minute))
})

// nodeProviderID returns the lowercase Azure resource ID of node.
func nodeProviderID(node corev1.Node) string {
	return strings.ToLower(strings.TrimPrefix(node.Spec.ProviderID, "azure://"))
}
