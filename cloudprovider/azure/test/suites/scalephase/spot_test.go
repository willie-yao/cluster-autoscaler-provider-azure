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
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

var _ = Describe("Azure Spot VMSS", Serial, Label("Slow"), func() {
	It("AZ-P1-010 grows a Spot pool for demand and physically removes its worker afterward", Label("AZ-P1-010", "spot", "delete"), func(ctx SpecContext) {
		f := setup(ctx, "spot", "AZ-P1-010")
		c := f.env.Config
		Expect(f.env.Controller(ctx)).To(Succeed())
		baseline := f.stable(ctx, map[string]int{c.MainPool: 1, c.ZeroPool: 0, c.SpotPool: 0})
		Expect(f.env.CheckWorkerIsolation(ctx, baseline, f.namespace.Name)).To(Succeed())

		By("creating demand for the Spot pool")
		work := f.env.Deployment(f.namespace.Name, "spot-demand", c.SpotLabel, "50m", 1)
		work.Spec.Template.Spec.Tolerations = []corev1.Toleration{{
			Key: "kubernetes.azure.com/scalesetpriority", Operator: corev1.TolerationOpExists,
			Effect: corev1.TaintEffectNoSchedule,
		}}
		Expect(f.env.K8s.Create(ctx, work)).To(Succeed())
		By("waiting for one Ready Spot worker that runs the Pod")
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
					spotDisruption(ctx, f, "Spot VM was replaced while its Pod still needed it", identity)
				}
				firstVMID = instance.VMID
				identity.VM, identity.VMID, identity.NICs = instance.ID, instance.VMID, instance.NICs
			}
			if firstVMID != "" && len(grown.Pools[c.SpotPool].Instances) == 0 {
				spotDisruption(ctx, f, "Spot VM disappeared while its Pod still needed it", identity)
			}
			// Keep the first Spot Node, so eviction events on it are found after a replacement.
			if nodes := environment.PoolNodes(grown.Nodes, c.PoolID(c.SpotPool)); identity.NodeUID == "" && len(nodes) == 1 {
				identity.Node, identity.NodeUID = nodes[0].Name, nodes[0].UID
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
		By("checking that the Spot VM and its Pod stay in place for one minute")
		Consistently(ctx, func() error {
			current, err := f.active(ctx)
			if err != nil {
				return err
			}
			if len(current.Pools[c.SpotPool].Instances) != 1 ||
				!spotVMIDPresent(current.Pools[c.SpotPool], firstVMID) {
				spotDisruption(ctx, f, "Spot VM was removed or replaced while its Pod was Ready", identity)
			}
			nodes := environment.PoolNodes(current.Nodes, c.PoolID(c.SpotPool))
			if len(nodes) != 1 || !environment.Ready(nodes[0]) {
				spotDisruption(ctx, f, "Spot Node lost readiness while its Pod still needed it", identity)
			}
			if err := current.StablePools(c, map[string]int{c.MainPool: 1, c.ZeroPool: 0, c.SpotPool: 1}); err != nil {
				return err
			}
			_, err = f.env.WorkloadState(ctx, f.namespace.Name, work.Name, c.SpotPool, 1, 0)
			return err
		}, time.Minute, pollInterval).Should(Succeed())

		By("removing the demand and waiting for the autoscaler scale-down event")
		Expect(f.env.K8s.Delete(ctx, work)).To(Succeed())
		Eventually(ctx, func() error {
			return workloadPodsGone(ctx, f, work)
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
			checkSpotNodeEvents(events.Items, identity)
			for _, instance := range current.Pools[c.SpotPool].Instances {
				if instance.VMID != firstVMID {
					spotDisruption(ctx, f, "Spot VM was replaced during scale-down", identity)
				}
			}
			if spotScaleDownEvent(events.Items, spotNode.UID, spotNode.Name) {
				return nil
			}
			if !spotVMIDPresent(current.Pools[c.SpotPool], firstVMID) {
				if missingSince.IsZero() {
					missingSince = time.Now()
				} else if time.Since(missingSince) > time.Minute {
					spotDisruption(ctx, f, "Spot VM disappeared without a completed cluster autoscaler scale-down", identity)
				}
			} else {
				missingSince = time.Time{}
			}
			return fmt.Errorf("waiting for cluster autoscaler Spot scale-down evidence for Node %s", spotNode.Name)
		}, waitTimeout, pollInterval).Should(Succeed())
		By("waiting for the Spot VM, Node and NIC to be deleted")
		Eventually(ctx, func() error {
			after, err := f.read(ctx)
			if err != nil {
				return err
			}
			var events corev1.EventList
			if err := f.env.K8s.List(ctx, &events); err != nil {
				return err
			}
			checkSpotNodeEvents(events.Items, identity)
			for _, instance := range after.Pools[c.SpotPool].Instances {
				if instance.VMID != firstVMID {
					spotDisruption(ctx, f, "Spot VM was replaced before physical deletion", identity)
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

// spotVMIDPresent reports whether pool has a VM with the unique VM ID id.
func spotVMIDPresent(pool environment.PoolState, id string) bool {
	for _, instance := range pool.Instances {
		if instance.VMID == id {
			return true
		}
	}
	return false
}

// spotIdentity records the Spot VM and Node that the spec follows.
type spotIdentity struct {
	VM      string
	VMID    string
	NICs    []string
	Node    string
	NodeUID types.UID
}

// spotDisruption stops the poll after the Spot worker changed outside the
// expected flow. It skips the spec only when a Node event shows a Spot
// eviction, and fails it otherwise.
func spotDisruption(ctx context.Context, f *fixture, message string, identity spotIdentity) {
	var events corev1.EventList
	if err := f.env.K8s.List(ctx, &events); err != nil {
		StopTrying(message + "; Node events could not be listed").Wrap(err).Now()
	}
	checkSpotNodeEvents(events.Items, identity)
	AddReportEntry("spot-disruption", struct {
		Reason   string
		Identity spotIdentity
	}{message, identity})
	StopTrying(fmt.Sprintf("%s without a Spot eviction event on Node %q", message, identity.Node)).Now()
}

// checkSpotNodeEvents skips the spec when the Spot Node has an eviction
// event, and fails it when the Node has a ScaleDownFailed event.
func checkSpotNodeEvents(events []corev1.Event, identity spotIdentity) {
	event, found := spotNodeEvent(events, identity.NodeUID)
	if !found {
		return
	}
	AddReportEntry("spot-node-event", struct {
		Reason   string
		Message  string
		Identity spotIdentity
	}{event.Reason, event.Message, identity})
	if event.Reason == "ScaleDownFailed" {
		StopTrying(fmt.Sprintf("cluster autoscaler reported ScaleDownFailed for Spot Node %q: %s", identity.Node, event.Message)).Now()
	}
	Skip(fmt.Sprintf("Azure evicted Spot Node %q: %s %s", identity.Node, event.Reason, event.Message))
}

// spotScaleDownEvent reports whether events show that the autoscaler removed
// the Node with nodeUID or nodeName.
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

// spotNodeEvent returns the event on the Node that ends the Spot spec. A
// SpotEviction or PreemptScheduled event is direct evidence of an Azure
// eviction and wins over a ScaleDownFailed event that the eviction can cause.
func spotNodeEvent(events []corev1.Event, nodeUID types.UID) (corev1.Event, bool) {
	var failed *corev1.Event
	for i, event := range events {
		if nodeUID == "" || event.InvolvedObject.Kind != "Node" || event.InvolvedObject.UID != nodeUID {
			continue
		}
		switch event.Reason {
		case "SpotEviction", "PreemptScheduled":
			return event, true
		case "ScaleDownFailed":
			if failed == nil {
				failed = &events[i]
			}
		}
	}
	if failed != nil {
		return *failed, true
	}
	return corev1.Event{}, false
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

func TestSpotNodeEvent(t *testing.T) {
	t.Parallel()
	uid := types.UID("owned-node")
	node := corev1.ObjectReference{Kind: "Node", UID: uid}
	marked := corev1.Event{Reason: "ScaleDown", Message: "marked the node as toBeDeleted/unschedulable", InvolvedObject: node}
	failed := corev1.Event{Reason: "ScaleDownFailed", Message: "failed to delete empty node", InvolvedObject: node}
	evicted := corev1.Event{Reason: "SpotEviction", InvolvedObject: node}
	foreign := failed
	foreign.InvolvedObject.UID = "foreign"
	if spotScaleDownEvent([]corev1.Event{marked, failed}, uid, "spot-0") {
		t.Fatal("marking and failure cannot prove completed CA deletion")
	}
	for _, tt := range []struct {
		name    string
		events  []corev1.Event
		nodeUID types.UID
		reason  string
	}{
		{name: "failed deletion", events: []corev1.Event{marked, failed}, nodeUID: uid, reason: "ScaleDownFailed"},
		{name: "eviction wins over failed deletion", events: []corev1.Event{failed, evicted}, nodeUID: uid, reason: "SpotEviction"},
		{name: "preemption notice", events: []corev1.Event{{Reason: "PreemptScheduled", InvolvedObject: node}}, nodeUID: uid, reason: "PreemptScheduled"},
		{name: "marking only", events: []corev1.Event{marked}, nodeUID: uid},
		{name: "foreign Node", events: []corev1.Event{foreign}, nodeUID: uid},
		{name: "unknown Node", events: []corev1.Event{failed, {Reason: "SpotEviction"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			event, found := spotNodeEvent(tt.events, tt.nodeUID)
			if found != (tt.reason != "") || event.Reason != tt.reason {
				t.Fatalf("spotNodeEvent = %q, %t; want %q", event.Reason, found, tt.reason)
			}
		})
	}
}
