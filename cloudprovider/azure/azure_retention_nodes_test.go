/*
Copyright 2026 The Kubernetes Authors.

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

package azure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	apiv1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubetesting "k8s.io/client-go/testing"
	"sigs.k8s.io/cluster-autoscaler/pkg/cloudprovider"
	coreoptions "sigs.k8s.io/cluster-autoscaler/pkg/core/options"
	"sigs.k8s.io/cluster-autoscaler/pkg/observers/nodegroupchange"
	"sigs.k8s.io/cluster-autoscaler/pkg/utils/taints"
)

func (f *retentionFixture) node(t *testing.T, id int) *apiv1.Node {
	t.Helper()
	node, err := f.kube.CoreV1().Nodes().Get(t.Context(), fmt.Sprintf("retention-%d", id), metav1.GetOptions{})
	require.NoError(t, err)
	return node
}

func (f *retentionFixture) ready(t *testing.T, id int, heartbeat time.Time) {
	t.Helper()
	node := f.node(t, id)
	found := false
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == apiv1.NodeReady {
			node.Status.Conditions[i].Status = apiv1.ConditionTrue
			node.Status.Conditions[i].LastHeartbeatTime = metav1.NewTime(heartbeat)
			found = true
		}
	}
	if !found {
		node.Status.Conditions = append(node.Status.Conditions, apiv1.NodeCondition{
			Type: apiv1.NodeReady, Status: apiv1.ConditionTrue, LastHeartbeatTime: metav1.NewTime(heartbeat),
		})
	}
	_, err := f.kube.CoreV1().Nodes().UpdateStatus(t.Context(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func (f *retentionFixture) resume(t *testing.T, failure error) *retentionPoller[armcompute.VirtualMachineScaleSetsClientStartResponse] {
	t.Helper()
	poller, handler := newRetentionPoller[armcompute.VirtualMachineScaleSetsClientStartResponse](t, failure)
	f.client.EXPECT().BeginStart(gomock.Any(), "rg", "agents", gomock.Any()).Return(poller, nil)
	require.NoError(t, f.group.IncreaseSize(t.Context(), 1))
	awaitRetentionPoll(t, handler.entered)
	return handler
}

func (f *retentionFixture) awaitStart(t *testing.T, id int) {
	t.Helper()
	key := strings.ToLower(newApiNode(armcompute.OrchestrationModeUniform, int64(id)).Spec.ProviderID)
	require.Eventually(t, func() bool {
		f.group.retention.mutex.Lock()
		defer f.group.retention.mutex.Unlock()
		attempt := f.group.retention.instances[key].instance.Resume
		return attempt != nil && (attempt.StartCompleted || attempt.Error != nil)
	}, 5*time.Second, time.Millisecond)
}

func TestDeallocateRequiresBuilderDependenciesAndFalseCordon(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*coreoptions.AutoscalerOptions)
	}{
		{"cordon", func(opts *coreoptions.AutoscalerOptions) { opts.CordonNodeBeforeTerminate = true }},
		{"client", func(opts *coreoptions.AutoscalerOptions) { opts.KubeClient = nil }},
		{"informer", func(opts *coreoptions.AutoscalerOptions) { opts.InformerFactory = nil }},
		{"processors", func(opts *coreoptions.AutoscalerOptions) { opts.Processors = nil }},
		{"deadline processor", func(opts *coreoptions.AutoscalerOptions) { opts.Processors.NodeGroupConfigProcessor = nil }},
		{"notifier", func(opts *coreoptions.AutoscalerOptions) { opts.Processors.ScaleStateNotifier = nil }},
		{"zero or max", func(opts *coreoptions.AutoscalerOptions) { opts.NodeGroupDefaults.ZeroOrMaxNodeScaling = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRetentionFixture(t, 2)
			test.mutate(f.group.manager.autoscalerOptions)
			require.Error(t, f.group.manager.validateRetentionDependencies())
			require.Error(t, f.group.IncreaseSize(t.Context(), 1))
			require.Error(t, f.group.DeleteNodes(t.Context(), []*apiv1.Node{f.node(t, 0)}))
			f.group.manager.config.NodeGroupScaleDownPolicies = nil
			require.NoError(t, f.group.manager.validateRetentionDependencies())
		})
	}
}

func TestDeallocateStatusConflictPreservesNodeFields(t *testing.T) {
	f := newRetentionFixture(t, 2)
	node := f.node(t, 0)
	node.Spec.Unschedulable = true
	node.Spec.Taints = []apiv1.Taint{{Key: "admin", Value: "keep", Effect: apiv1.TaintEffectNoSchedule}}
	node.Annotations = map[string]string{"admin": "keep"}
	node.Status.Conditions = []apiv1.NodeCondition{{Type: apiv1.NodeReady, Status: apiv1.ConditionTrue, LastHeartbeatTime: metav1.Now()}}
	_, err := f.kube.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = f.kube.CoreV1().Nodes().UpdateStatus(t.Context(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
	updates := 0
	f.kube.PrependReactor("update", "nodes", func(action kubetesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "status" {
			updates++
			if updates == 1 {
				return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, node.Name, errors.New("conflict"))
			}
		}
		return false, nil, nil
	})
	f.stop(t, 0)
	require.Equal(t, 2, updates)
	live := f.node(t, 0)
	require.Equal(t, node.UID, live.UID)
	require.True(t, live.Spec.Unschedulable)
	require.Equal(t, node.Annotations, live.Annotations)
	require.Contains(t, live.Spec.Taints, node.Spec.Taints[0])
	require.Equal(t, node.Status.Conditions[0], live.Status.Conditions[0])
	require.True(t, cloudprovider.IsNodeSuspended(live))
	require.True(t, taints.HasToBeDeletedTaint(live))
}

func TestDeallocateAcceptedStopStatusForbiddenStaysOwned(t *testing.T) {
	f := newRetentionFixture(t, 2)
	node := f.mark(t, 0)
	forbidden := true
	f.kube.PrependReactor("update", "nodes", func(action kubetesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "status" && forbidden {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes/status"}, node.Name, errors.New("forbidden"))
		}
		return false, nil, nil
	})
	poller, handler := newRetentionPoller[armcompute.VirtualMachineScaleSetsClientDeallocateResponse](t, nil)
	f.client.EXPECT().BeginDeallocate(gomock.Any(), "rg", "agents", gomock.Any()).Return(poller, nil)
	require.Error(t, f.group.DeleteNodes(t.Context(), []*apiv1.Node{node}))
	awaitRetentionPoll(t, handler.entered)
	state := f.group.retention
	state.mutex.Lock()
	require.Equal(t, 1, state.snapshot().TargetSize)
	require.Equal(t, retentionRetiring, state.instances[strings.ToLower(node.Spec.ProviderID)].instance.Phase)
	state.mutex.Unlock()
	_, err := f.group.GetNodeGroupAccounting(t.Context())
	require.Error(t, err)
	_, handled, err := f.group.CleanToBeDeleted(t.Context(), node, false)
	require.True(t, handled)
	require.Error(t, err)
	require.True(t, taints.HasToBeDeletedTaint(f.node(t, 0)))
	forbidden = false
	require.NoError(t, (&AzureCloudProvider{azureManager: f.group.manager}).Refresh(t.Context()))
	require.True(t, cloudprovider.IsNodeSuspended(f.node(t, 0)))
}

func TestDeallocateRejectedDrainCleansOnlyOwnedTaint(t *testing.T) {
	f := newRetentionFixture(t, 2)
	node := f.node(t, 0)
	node.Spec.Unschedulable = true
	node.Spec.Taints = append(node.Spec.Taints, apiv1.Taint{Key: "admin", Effect: apiv1.TaintEffectNoSchedule})
	_, err := f.kube.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
	marked := f.mark(t, 0)
	updated, handled, err := f.group.CleanToBeDeleted(t.Context(), marked, true)
	require.NoError(t, err)
	require.True(t, handled)
	require.True(t, updated.Spec.Unschedulable)
	require.Equal(t, node.Spec.Taints, updated.Spec.Taints)
	require.Equal(t, node.UID, updated.UID)
	require.Equal(t, 2, f.snapshot(t).TargetSize)
}

func TestDeallocateFinalGuardRejectsChangedOwnership(t *testing.T) {
	for _, change := range []string{"uid", "taint", "suspended", "missing"} {
		t.Run(change, func(t *testing.T) {
			f := newRetentionFixture(t, 2)
			requested := f.mark(t, 0)
			node := f.node(t, 0)
			switch change {
			case "uid":
				node.UID = "replacement"
			case "taint":
				node.Spec.Taints[0].Value = "another-owner"
			case "suspended":
				node.Status.Conditions = append(node.Status.Conditions, apiv1.NodeCondition{Type: "Suspended", Status: apiv1.ConditionTrue, Reason: "another-writer"})
			case "missing":
				require.NoError(t, f.kube.CoreV1().Nodes().Delete(t.Context(), node.Name, metav1.DeleteOptions{}))
			}
			if change != "missing" {
				_, err := f.kube.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
				require.NoError(t, err)
				_, err = f.kube.CoreV1().Nodes().UpdateStatus(t.Context(), node, metav1.UpdateOptions{})
				require.NoError(t, err)
			}
			require.Error(t, f.group.DeleteNodes(t.Context(), []*apiv1.Node{requested}))
		})
	}
}

func TestDeallocateResumePublicationAndNoReplay(t *testing.T) {
	f := newRetentionFixture(t, 2)
	f.ready(t, 0, time.Now().Add(-time.Hour))
	f.park(t, 0)
	stale := f.node(t, 0)
	handler := f.resume(t, nil)
	handler.complete()
	f.awaitStart(t, 0)
	require.NoError(t, f.group.reconcileRetention(t.Context()))
	view, err := f.group.GetNodeGroupAccounting(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, view.UpcomingInactiveNodes)
	require.True(t, taints.HasToBeDeletedTaint(f.node(t, 0)))
	f.ready(t, 0, time.Now())
	f.setVM(0, vmPowerStateRunning, provisioningStateSucceeded)
	require.NoError(t, f.group.reconcileRetention(t.Context()))
	view, err = f.group.GetNodeGroupAccounting(t.Context())
	require.NoError(t, err)
	require.Empty(t, view.InactiveInstanceIDs)
	require.Zero(t, view.UpcomingInactiveNodes)
	normalized, err := cloudprovider.NormalizeNodeGroupObservations([]*apiv1.Node{stale}, map[string]*cloudprovider.NodeGroupAccountingSnapshot{"agents": view})
	require.NoError(t, err)
	require.False(t, cloudprovider.IsNodeSuspended(normalized[0]))
	require.False(t, taints.HasToBeDeletedTaint(normalized[0]))
	node := f.node(t, 0)
	node.Spec.Unschedulable = true
	_, err = f.kube.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == apiv1.NodeReady {
			node.Status.Conditions[i].Status = apiv1.ConditionFalse
		}
	}
	_, err = f.kube.CoreV1().Nodes().UpdateStatus(t.Context(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.kube.ClearActions()
	require.NoError(t, f.group.reconcileRetention(t.Context()))
	require.True(t, f.node(t, 0).Spec.Unschedulable)
	for _, action := range f.kube.Actions() {
		require.NotEqual(t, "update", action.GetVerb(), "completed activation must not replay cleanup")
	}
}

func TestDeallocateResumeStatusForbiddenPreservesProtection(t *testing.T) {
	f := newRetentionFixture(t, 2)
	f.park(t, 0)
	handler := f.resume(t, nil)
	handler.complete()
	f.awaitStart(t, 0)
	f.ready(t, 0, time.Now())
	f.kube.PrependReactor("update", "nodes", func(action kubetesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "status" {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes/status"}, "retention-0", errors.New("forbidden"))
		}
		return false, nil, nil
	})
	require.Error(t, f.group.reconcileRetention(t.Context()))
	require.True(t, taints.HasToBeDeletedTaint(f.node(t, 0)))
	require.True(t, cloudprovider.IsNodeSuspended(f.node(t, 0)))
	_, err := f.group.GetNodeGroupAccounting(t.Context())
	require.Error(t, err)
}

func TestDeallocateResumeConditionChangePreservesProtection(t *testing.T) {
	for _, changeAt := range []string{"before cleanup", "conflict retry", "cleanup response"} {
		t.Run(changeAt, func(t *testing.T) {
			f := newRetentionFixture(t, 2)
			f.park(t, 0)
			handler := f.resume(t, nil)
			handler.complete()
			f.awaitStart(t, 0)
			f.ready(t, 0, time.Now())
			marked := f.node(t, 0)
			changed := false
			f.kube.PrependReactor("*", "nodes", func(action kubetesting.Action) (bool, runtime.Object, error) {
				if changed {
					return false, nil, nil
				}
				var node *apiv1.Node
				switch action := action.(type) {
				case kubetesting.GetAction:
					if changeAt != "before cleanup" || action.GetName() != marked.Name {
						return false, nil, nil
					}
					object, err := f.kube.Tracker().Get(apiv1.SchemeGroupVersion.WithResource("nodes"), "", marked.Name)
					require.NoError(t, err)
					node = object.(*apiv1.Node)
				case kubetesting.UpdateAction:
					if changeAt == "before cleanup" || action.GetSubresource() != "" {
						return false, nil, nil
					}
					node = action.GetObject().(*apiv1.Node).DeepCopy()
					if taints.HasToBeDeletedTaint(node) {
						return false, nil, nil
					}
				default:
					return false, nil, nil
				}
				condition := conditionSuspended(node)
				if condition == nil || condition.Status != apiv1.ConditionFalse {
					return false, nil, nil
				}
				condition.Status, condition.Reason = apiv1.ConditionTrue, "another-writer"
				if changeAt == "conflict retry" {
					node.Spec.Taints = marked.Spec.Taints
				}
				require.NoError(t, f.kube.Tracker().Update(apiv1.SchemeGroupVersion.WithResource("nodes"), node, ""))
				changed = true
				if changeAt == "conflict retry" {
					return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, node.Name, errors.New("status changed"))
				}
				return true, node, nil
			})
			require.Error(t, f.group.reconcileRetention(t.Context()))
			require.True(t, changed)
			live := f.node(t, 0)
			require.Equal(t, marked.Spec.Taints, live.Spec.Taints)
			require.Equal(t, "another-writer", conditionSuspended(live).Reason)
			require.True(t, cloudprovider.IsNodeSuspended(live))
			_, err := f.group.GetNodeGroupAccounting(t.Context())
			require.Error(t, err)
		})
	}
}

func TestDeallocateMissingNodeKeepsPublishedIdentity(t *testing.T) {
	f := newRetentionFixture(t, 2)
	node := f.node(t, 0)
	node.Spec.ProviderID = strings.Replace(node.Spec.ProviderID, "test-asg", "TEST-ASG", 1)
	_, err := f.kube.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.ready(t, 0, time.Now().Add(-time.Hour))
	stale := f.node(t, 0)
	f.park(t, 0)
	before, err := cloudprovider.GetNodeGroupAccounting(t.Context(), f.group)
	require.NoError(t, err)
	require.Equal(t, []string{stale.Spec.ProviderID}, before.InactiveInstanceIDs)
	require.NoError(t, f.kube.CoreV1().Nodes().Delete(t.Context(), node.Name, metav1.DeleteOptions{}))
	require.NoError(t, f.group.reconcileRetention(t.Context()))
	after, err := cloudprovider.GetNodeGroupAccounting(t.Context(), f.group)
	require.NoError(t, err)
	require.Equal(t, before.InactiveInstanceIDs, after.InactiveInstanceIDs)
	for _, observed := range after.NodeObservations {
		require.NotEqual(t, stale.Name, observed.Name, "deleted Nodes must not be republished")
	}
	normalized, err := cloudprovider.NormalizeNodeGroupObservations([]*apiv1.Node{stale},
		map[string]*cloudprovider.NodeGroupAccountingSnapshot{f.group.Id(): after})
	require.NoError(t, err)
	require.True(t, cloudprovider.IsNodeSuspended(normalized[0]))
	require.False(t, cloudprovider.IsNodeSuspended(stale))
}

func TestDeallocatePureSnapshotAndMissingNodeMembership(t *testing.T) {
	f := newRetentionFixture(t, 2)
	f.park(t, 0)
	require.NoError(t, f.kube.CoreV1().Nodes().Delete(t.Context(), "retention-0", metav1.DeleteOptions{}))
	require.NoError(t, f.group.reconcileRetention(t.Context()))
	f.kube.ClearActions()
	view, err := cloudprovider.GetNodeGroupAccounting(t.Context(), f.group)
	require.NoError(t, err)
	require.Len(t, view.Instances, 2)
	require.Len(t, view.InactiveInstanceIDs, 1)
	require.Len(t, view.NodeObservations, 1)
	_, err = f.group.TargetSize(t.Context())
	require.NoError(t, err)
	_, err = f.group.Nodes(t.Context())
	require.NoError(t, err)
	require.Empty(t, f.kube.Actions())
	view.NodeObservations[0].Spec.Unschedulable = true
	view.Instances[0].Status.State = cloudprovider.InstanceDeleting
	again, err := f.group.GetNodeGroupAccounting(t.Context())
	require.NoError(t, err)
	require.False(t, again.NodeObservations[0].Spec.Unschedulable)
	require.Equal(t, cloudprovider.InstanceRunning, again.Instances[0].Status.State)
	node := newApiNode(armcompute.OrchestrationModeUniform, 0)
	node.Annotations = map[string]string{cloudprovider.FakeNodeReasonAnnotation: cloudprovider.FakeNodeUnregistered}
	require.Error(t, f.group.ForceDeleteNodes(t.Context(), []*apiv1.Node{node}))
}

type retentionFailureObserver struct {
	nodegroupchange.NodeGroupChangeObserver
	failed func(context.Context, cloudprovider.NodeGroup, int, cloudprovider.InstanceErrorInfo)
}

func (o *retentionFailureObserver) RegisterFailedScaleUp(ctx context.Context, group cloudprovider.NodeGroup, delta int, info cloudprovider.InstanceErrorInfo, now time.Time) {
	o.failed(ctx, group, delta, info)
}

func TestDeallocateIndependentDeadlinesAndNotificationOutsideLocks(t *testing.T) {
	f := newRetentionFixture(t, 4)
	f.park(t, 0, 1)
	first := f.resume(t, nil)
	state := f.group.retention
	key := strings.ToLower(newApiNode(armcompute.OrchestrationModeUniform, 0).Spec.ProviderID)
	state.mutex.Lock()
	accepted := state.instances[key].instance.Resume.AcceptedAt
	state.instances[key].instance.Resume.Deadline = time.Now().Add(-time.Second)
	state.mutex.Unlock()
	second := f.resume(t, nil)
	f.expectCapacity(t, 5, nil)
	require.NoError(t, f.group.IncreaseSize(t.Context(), 1))
	state.mutex.Lock()
	for operation, capacity := range state.capacity {
		capacity.deadline = time.Now().Add(-time.Second)
		state.capacity[operation] = capacity
	}
	state.mutex.Unlock()
	failures := 0
	f.group.manager.autoscalerOptions.Processors.ScaleStateNotifier.Register(&retentionFailureObserver{failed: func(ctx context.Context, group cloudprovider.NodeGroup, delta int, info cloudprovider.InstanceErrorInfo) {
		require.True(t, state.mutex.TryLock(), "notifier called under operation lock")
		state.mutex.Unlock()
		_, err := f.group.GetNodeGroupAccounting(ctx)
		require.Error(t, err, "failure dispatch must precede publication")
		failures += delta
	}})
	require.NoError(t, f.group.reconcileRetention(t.Context()))
	view, err := f.group.GetNodeGroupAccounting(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, view.UpcomingInactiveNodes)
	require.Equal(t, 2, view.UnavailableTargetNodes)
	require.Equal(t, 5, view.TargetSize)
	require.Equal(t, 2, failures)
	require.NoError(t, f.group.DecreaseTargetSize(t.Context(), -3))
	require.NoError(t, f.group.reconcileRetention(t.Context()))
	require.Equal(t, 2, failures)
	state.mutex.Lock()
	require.Equal(t, accepted, state.instances[key].instance.Resume.AcceptedAt)
	state.mutex.Unlock()
	first.complete()
	second.complete()
	f.awaitStart(t, 0)
	f.ready(t, 0, time.Now())
	require.NoError(t, f.group.reconcileRetention(t.Context()))
	view, err = f.group.GetNodeGroupAccounting(t.Context())
	require.NoError(t, err)
	require.Equal(t, 2, view.UnavailableTargetNodes)
	require.Equal(t, 2, failures)
}

func TestDeallocateFailedStartNotifiesOnce(t *testing.T) {
	f := newRetentionFixture(t, 2)
	f.park(t, 0)
	handler := f.resume(t, &azcore.ResponseError{StatusCode: 400})
	handler.complete()
	f.awaitStart(t, 0)
	failures := 0
	f.group.manager.autoscalerOptions.Processors.ScaleStateNotifier.Register(&retentionFailureObserver{failed: func(context.Context, cloudprovider.NodeGroup, int, cloudprovider.InstanceErrorInfo) { failures++ }})
	require.NoError(t, f.group.reconcileRetention(t.Context()))
	require.NoError(t, f.group.reconcileRetention(t.Context()))
	require.Equal(t, 1, failures)
	view, err := f.group.GetNodeGroupAccounting(t.Context())
	require.NoError(t, err)
	require.Zero(t, view.UpcomingInactiveNodes)
	require.Equal(t, 1, view.UnavailableTargetNodes)
}

func TestDeallocateKnownPolicyLossNeverFallsThroughCleanup(t *testing.T) {
	f := newRetentionFixture(t, 2)
	f.park(t, 0)
	node := f.node(t, 0)
	f.group.manager.config.NodeGroupScaleDownPolicies = nil
	_, handled, err := f.group.CleanToBeDeleted(t.Context(), node, false)
	require.True(t, handled)
	require.Error(t, err)
	_, handled, err = f.group.MarkToBeDeleted(t.Context(), node, false)
	require.True(t, handled)
	require.Error(t, err)
	require.True(t, taints.HasToBeDeletedTaint(f.node(t, 0)))
}

func TestDeallocateReplacementUIDCannotInheritReceipt(t *testing.T) {
	f := newRetentionFixture(t, 2)
	f.park(t, 0)
	node := f.node(t, 0)
	node.UID = "new-registration"
	_, err := f.kube.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Error(t, f.group.reconcileRetention(t.Context()))
	_, err = f.group.GetNodeGroupAccounting(t.Context())
	require.Error(t, err)
	require.Error(t, f.group.IncreaseSize(t.Context(), 1))
	require.True(t, taints.HasToBeDeletedTaint(f.node(t, 0)))
}

func TestDeallocateColdRunningSuspendedNodeIsNotRecovery(t *testing.T) {
	f := newRetentionFixture(t, 2)
	node := f.node(t, 0)
	node.Status.Conditions = append(node.Status.Conditions, apiv1.NodeCondition{
		Type: "Suspended", Status: apiv1.ConditionTrue, Reason: suspendedReason,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour)),
	})
	_, err := f.kube.CoreV1().Nodes().UpdateStatus(t.Context(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Error(t, f.group.reconcileRetention(t.Context()))
	_, err = f.group.GetNodeGroupAccounting(t.Context())
	require.Error(t, err)
	require.Error(t, f.group.IncreaseSize(t.Context(), 1))
}

func TestDeallocateFailedSnapshotReadBlocksWithoutSideEffects(t *testing.T) {
	f := newRetentionFixture(t, 2)
	f.group.manager.invalidateRetentionViews(errors.New("required ARM read failed"))
	f.kube.ClearActions()
	_, err := f.group.GetNodeGroupAccounting(t.Context())
	require.ErrorContains(t, err, "required ARM read failed")
	_, err = f.group.TargetSize(t.Context())
	require.Error(t, err)
	require.Empty(t, f.kube.Actions())
	require.NoError(t, f.group.reconcileRetention(t.Context()))
	_, err = f.group.GetNodeGroupAccounting(t.Context())
	require.NoError(t, err)
}
