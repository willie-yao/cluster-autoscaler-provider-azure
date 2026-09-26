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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

var _ = Describe("Azure Spot VMSS", Serial, func() {
	It("AZ-P1-010 grows a Spot pool for demand and physically removes its worker afterward", Label("AZ-P1-010", "spot", "delete"), func(ctx SpecContext) {
		f := setup(ctx, "spot", "AZ-P1-010")
		c := f.env.Config
		Expect(f.env.Controller(ctx)).To(Succeed())
		baseline := f.stable(ctx, map[string]int{c.MainPool: 1, c.ZeroPool: 0, c.SpotPool: 0})
		Expect(f.env.CheckWorkerIsolation(ctx, baseline, f.namespace.Name)).To(Succeed())

		work := f.env.Deployment(f.namespace.Name, "spot-demand", c.SpotLabel, "50m", 1)
		work.Spec.Template.Spec.Tolerations = []corev1.Toleration{{
			Key: "kubernetes.azure.com/scalesetpriority", Operator: corev1.TolerationOpExists,
			Effect: corev1.TaintEffectNoSchedule,
		}}
		Expect(f.env.K8s.Create(ctx, work)).To(Succeed())
		var grown environment.Snapshot
		var firstVMID string
		identity := spotIdentity{}
		Eventually(ctx, func() error {
			var err error
			grown, err = f.active(ctx)
			if err != nil {
				return err
			}
			for _, instance := range grown.Pools[c.SpotPool].Instances {
				if firstVMID != "" && instance.VMID != firstVMID {
					spotInfrastructureEvent("Spot VM was replaced while its Pod still needed it", identity)
				}
				firstVMID = instance.VMID
				identity.VM, identity.VMID, identity.NICs = instance.ID, instance.VMID, instance.NICs
			}
			if firstVMID != "" && len(grown.Pools[c.SpotPool].Instances) == 0 {
				spotInfrastructureEvent("Spot VM disappeared while its Pod still needed it", identity)
			}
			if err := grown.StablePools(c, map[string]int{c.MainPool: 1, c.ZeroPool: 0, c.SpotPool: 1}); err != nil {
				return err
			}
			_, err = f.env.WorkloadState(ctx, f.namespace.Name, work.Name, c.SpotPool, 1, 0)
			return err
		}, waitTimeout, pollInterval).Should(Succeed())
		Expect(firstVMID).NotTo(BeEmpty(), "Azure must return the unique Spot VM ID")
		Expect(f.env.CheckWorkerIsolation(ctx, grown, f.namespace.Name)).To(Succeed())
		spotNode := environment.PoolNodes(grown.Nodes, c.PoolID(c.SpotPool))[0]
		identity.Node, identity.NodeUID = spotNode.Name, spotNode.UID
		AddReportEntry("spot-identities", identity)
		Consistently(ctx, func() error {
			current, err := f.active(ctx)
			if err != nil {
				return err
			}
			if len(current.Pools[c.SpotPool].Instances) != 1 ||
				!spotVMIDPresent(current.Pools[c.SpotPool], firstVMID) {
				spotInfrastructureEvent("Spot VM was evicted or replaced while its Pod was Ready", identity)
			}
			nodes := environment.PoolNodes(current.Nodes, c.PoolID(c.SpotPool))
			if len(nodes) != 1 || !environment.Ready(nodes[0]) {
				spotInfrastructureEvent("Spot Node lost readiness while its Pod still needed it", identity)
			}
			if err := current.StablePools(c, map[string]int{c.MainPool: 1, c.ZeroPool: 0, c.SpotPool: 1}); err != nil {
				return err
			}
			_, err = f.env.WorkloadState(ctx, f.namespace.Name, work.Name, c.SpotPool, 1, 0)
			return err
		}, time.Minute, pollInterval).Should(Succeed())

		Expect(f.env.K8s.Delete(ctx, work)).To(Succeed())
		var pods corev1.PodList
		Eventually(ctx, func() error {
			if err := f.env.K8s.List(ctx, &pods, client.InNamespace(f.namespace.Name),
				client.MatchingLabels(work.Spec.Selector.MatchLabels)); err != nil {
				return err
			}
			if len(pods.Items) != 0 {
				return fmt.Errorf("waiting for Spot demand Pods to be removed")
			}
			return nil
		}, 4*time.Minute, pollInterval).Should(Succeed())

		missingSince := time.Time{}
		Eventually(ctx, func() error {
			current, err := f.active(ctx)
			if err != nil {
				return err
			}
			var events corev1.EventList
			if err := f.env.K8s.List(ctx, &events); err != nil {
				return err
			}
			if reason := spotDisruptionEvent(events.Items, spotNode.UID); reason != "" {
				spotInfrastructureEvent("Spot Node had "+reason+" before physical deletion", identity)
			}
			for _, instance := range current.Pools[c.SpotPool].Instances {
				if instance.VMID != firstVMID {
					spotInfrastructureEvent("Spot VM was replaced during scale-down", identity)
				}
			}
			if spotScaleDownEvent(events.Items, spotNode.UID, spotNode.Name) {
				return nil
			}
			if !spotVMIDPresent(current.Pools[c.SpotPool], firstVMID) {
				if missingSince.IsZero() {
					missingSince = time.Now()
				} else if time.Since(missingSince) > time.Minute {
					spotInfrastructureEvent("Spot VM disappeared without a completed cluster autoscaler scale-down", identity)
				}
			} else {
				missingSince = time.Time{}
			}
			return fmt.Errorf("waiting for cluster autoscaler Spot scale-down evidence")
		}, waitTimeout, pollInterval).Should(Succeed())
		Eventually(ctx, func() error {
			after, err := f.read(ctx)
			if err != nil {
				return err
			}
			var events corev1.EventList
			if err := f.env.K8s.List(ctx, &events); err != nil {
				return err
			}
			if reason := spotDisruptionEvent(events.Items, spotNode.UID); reason != "" {
				spotInfrastructureEvent("Spot Node had "+reason+" before physical deletion", identity)
			}
			for _, instance := range after.Pools[c.SpotPool].Instances {
				if instance.VMID != firstVMID {
					spotInfrastructureEvent("Spot VM was replaced before physical deletion", identity)
				}
			}
			if err := after.StablePools(c, map[string]int{c.MainPool: 1, c.ZeroPool: 0, c.SpotPool: 0}); err != nil {
				return err
			}
			return f.env.Deleted(ctx, grown, after, c.SpotPool, 1)
		}, waitTimeout, pollInterval).Should(Succeed())
		AddReportEntry("spot-physical-delete", "one Spot VM, its Node and its NIC were removed after CA scale-down")
	}, NodeTimeout(55*time.Minute))
})

func spotVMIDPresent(pool environment.PoolState, id string) bool {
	for _, instance := range pool.Instances {
		if instance.VMID == id {
			return true
		}
	}
	return false
}

type spotIdentity struct {
	VM      string
	VMID    string
	NICs    []string
	Node    string
	NodeUID types.UID
}

func spotInfrastructureEvent(message string, identity spotIdentity) {
	AddReportEntry("infrastructure-event", struct {
		Reason   string
		Identity spotIdentity
	}{message, identity})
	Skip(message)
}

func spotScaleDownEvent(events []corev1.Event, nodeUID types.UID, nodeName string) bool {
	for _, event := range events {
		if event.Reason == "ScaleDown" && event.InvolvedObject.Kind == "Node" &&
			event.InvolvedObject.UID == nodeUID && event.Message == "nodes removed by cluster autoscaler" {
			return true
		}
		if (event.Reason == "ScaleDown" || event.Reason == "ScaleDownEmpty") &&
			(strings.Contains(event.Message, "node "+nodeName+" removed") ||
				strings.Contains(event.Message, "empty node "+nodeName+" removed")) {
			return true
		}
	}
	return false
}

func spotDisruptionEvent(events []corev1.Event, nodeUID types.UID) string {
	for _, event := range events {
		if event.InvolvedObject.Kind != "Node" || event.InvolvedObject.UID != nodeUID {
			continue
		}
		switch event.Reason {
		case "ScaleDownFailed", "SpotEviction", "PreemptScheduled", "Evicted":
			return event.Reason
		}
	}
	return ""
}

func TestSpotScaleDownEvent(t *testing.T) {
	t.Parallel()
	uid := types.UID("owned-node")
	for _, tt := range []struct {
		name  string
		event corev1.Event
		valid bool
	}{
		{name: "Node scale-down", valid: true, event: corev1.Event{
			Reason: "ScaleDown", Message: "nodes removed by cluster autoscaler",
			InvolvedObject: corev1.ObjectReference{Kind: "Node", UID: uid},
		}},
		{name: "marking before deletion", event: corev1.Event{
			Reason: "ScaleDown", Message: "marked the node as toBeDeleted/unschedulable",
			InvolvedObject: corev1.ObjectReference{Kind: "Node", UID: uid},
		}},
		{name: "empty-node controller event", valid: true, event: corev1.Event{
			Reason: "ScaleDownEmpty", Message: "Scale-down: empty node spot-0 removed",
		}},
		{name: "another node", event: corev1.Event{
			Reason: "ScaleDown", InvolvedObject: corev1.ObjectReference{Kind: "Node", UID: "foreign"},
		}},
		{name: "foreign node with prefix", event: corev1.Event{
			Reason: "ScaleDownEmpty", Message: "Scale-down: empty node spot-00 removed",
		}},
		{name: "eviction alone", event: corev1.Event{
			Reason: "SpotEviction", Message: "node spot-0 removed",
		}},
		{name: "marking and failed deletion", event: corev1.Event{
			Reason: "ScaleDownFailed", Message: "failed to delete node",
			InvolvedObject: corev1.ObjectReference{Kind: "Node", UID: uid},
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := spotScaleDownEvent([]corev1.Event{tt.event}, uid, "spot-0"); got != tt.valid {
				t.Fatalf("spotScaleDownEvent = %t, valid=%t", got, tt.valid)
			}
		})
	}
}

func TestSpotDisruptionEvent(t *testing.T) {
	t.Parallel()
	uid := types.UID("owned-node")
	events := []corev1.Event{
		{Reason: "ScaleDown", Message: "marked the node as toBeDeleted/unschedulable",
			InvolvedObject: corev1.ObjectReference{Kind: "Node", UID: uid}},
		{Reason: "ScaleDownFailed", InvolvedObject: corev1.ObjectReference{Kind: "Node", UID: uid}},
	}
	if spotScaleDownEvent(events, uid, "spot-0") {
		t.Fatal("marking and failure cannot prove completed CA deletion")
	}
	if got := spotDisruptionEvent(events, uid); got != "ScaleDownFailed" {
		t.Fatalf("missing Spot infrastructure event: %q", got)
	}
	events[1].InvolvedObject.UID = "foreign"
	if got := spotDisruptionEvent(events, uid); got != "" {
		t.Fatalf("foreign disruption was attributed to Spot Node: %q", got)
	}
}
