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

package azure

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetvmclient/mock_virtualmachinescalesetvmclient"
	"sigs.k8s.io/cluster-autoscaler/pkg/cloudprovider"
	"sigs.k8s.io/cluster-autoscaler/pkg/config/dynamic"
	"sigs.k8s.io/cluster-autoscaler/pkg/utils/taints"
)

func TestSetSuspendedCondition(t *testing.T) {
	node := &apiv1.Node{}
	parked := metav1.NewTime(time.Unix(100, 0))
	require.True(t, setSuspendedCondition(node, apiv1.ConditionTrue, parkedReason, parked))
	require.False(t, setSuspendedCondition(node, apiv1.ConditionTrue, parkedReason, metav1.NewTime(time.Unix(200, 0))))
	require.Equal(t, parked, nodeSuspended(node).LastTransitionTime)
	resumed := metav1.NewTime(time.Unix(300, 0))
	require.True(t, setSuspendedCondition(node, apiv1.ConditionFalse, resumedReason, resumed))
	require.Len(t, node.Status.Conditions, 1)
	require.Equal(t, resumed, nodeSuspended(node).LastTransitionTime)
}

func TestSuspendedParkPreservesNodeAndInventory(t *testing.T) {
	node := suspendedTestNode(0, false)
	custom := apiv1.Taint{Key: "example.com/keep", Effect: apiv1.TaintEffectPreferNoSchedule}
	node.Spec.Taints = []apiv1.Taint{custom,
		{Key: taints.ToBeDeletedTaint, Effect: apiv1.TaintEffectNoSchedule},
		{Key: taints.DeletionCandidateTaintKey, Effect: apiv1.TaintEffectPreferNoSchedule}}
	node.Spec.Unschedulable = true
	client := fake.NewClientset(node)
	world := &suspendedWorld{states: []string{vmPowerStateRunning}}
	provider, group := newSuspendedProvider(t, world, client, 0, 3)
	require.NoError(t, group.DeleteNodes(t.Context(), []*apiv1.Node{node}))
	parked, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, node.UID, parked.UID)
	require.Equal(t, node.Spec.ProviderID, parked.Spec.ProviderID)
	require.Equal(t, node.Spec.PodCIDR, parked.Spec.PodCIDR)
	require.Equal(t, node.Labels, parked.Labels)
	require.Equal(t, apiv1.ConditionTrue, nodeSuspended(parked).Status)
	require.Equal(t, "true", parked.Annotations[scaleDownDisabledAnnotation])
	require.ElementsMatch(t, []apiv1.Taint{custom, {Key: suspendedTaintKey, Effect: apiv1.TaintEffectNoSchedule}}, parked.Spec.Taints)
	require.False(t, parked.Spec.Unschedulable)
	hasInstance, err := provider.HasInstance(t.Context(), parked)
	require.NoError(t, err)
	require.True(t, hasInstance)
	instances, err := group.Nodes(t.Context())
	require.NoError(t, err)
	require.Len(t, instances, 1)
	require.Equal(t, cloudprovider.InstanceRunning, instances[0].Status.State)
	size, err := group.TargetSize(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, size)
	require.NoError(t, group.ForceDeleteNodes(t.Context(), []*apiv1.Node{parked}))
	require.Equal(t, []string{"deallocate:0"}, world.history())
}

func TestSuspendedIncreaseStartsParkedThenGrows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := suspendedTestNode(1, true)
		client := fake.NewClientset(node)
		world := &suspendedWorld{states: []string{vmPowerStateRunning, vmPowerStateDeallocated}}
		_, group := newSuspendedProvider(t, world, client, 0, 3)
		require.ErrorContains(t, group.IncreaseSize(t.Context(), 3), "size increase too large")
		require.ErrorIs(t, group.AtomicIncreaseSize(t.Context(), 1), cloudprovider.ErrNotImplemented)
		require.NoError(t, group.IncreaseSize(t.Context(), 2))
		synctest.Wait()
		require.Equal(t, []string{"start:1", "grow:3"}, world.history())
		resumed, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, node.UID, resumed.UID)
		require.Equal(t, apiv1.ConditionFalse, nodeSuspended(resumed).Status)
		require.True(t, nodeSuspended(resumed).LastTransitionTime.After(nodeSuspended(node).LastTransitionTime.Time))
		require.NotContains(t, resumed.Annotations, scaleDownDisabledAnnotation)
		require.False(t, taints.HasTaint(resumed, suspendedTaintKey))
		size, err := group.TargetSize(t.Context())
		require.NoError(t, err)
		require.Equal(t, 3, size)
	})
}

func TestSuspendedIncreaseWhileStartPending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nodes := []*apiv1.Node{suspendedTestNode(0, true), suspendedTestNode(1, true)}
		polling, finish := make(chan struct{}), make(chan struct{})
		defer close(finish)
		world := &suspendedWorld{states: []string{vmPowerStateDeallocated, vmPowerStateDeallocated}}
		world.startResult = func(index int) error {
			if index == 0 {
				close(polling)
				<-finish
			}
			world.mu.Lock()
			world.states[index] = vmPowerStateRunning
			world.mu.Unlock()
			return nil
		}
		_, group := newSuspendedProvider(t, world, fake.NewClientset(nodes[0], nodes[1]), 0, 4)
		require.NoError(t, group.IncreaseSize(t.Context(), 1))
		<-polling
		require.NoError(t, group.IncreaseSize(t.Context(), 1))
		require.Equal(t, []string{"start:0", "start:1"}, world.history(),
			"each IncreaseSize requests additional capacity, not capacity already starting")
		total, err := group.TargetSize(t.Context())
		require.NoError(t, err)
		require.Equal(t, 2, total)
	})
}

func TestSuspendedDecreaseTargetSizeKeepsResumingVM(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := suspendedTestNode(0, true)
		client := fake.NewClientset(node)
		world := &suspendedWorld{states: []string{vmPowerStateDeallocated}}
		world.startResult = func(int) error { return context.DeadlineExceeded }
		provider, group := newSuspendedProvider(t, world, client, 0, 1)
		require.NoError(t, group.IncreaseSize(t.Context(), 1))
		synctest.Wait()
		time.Sleep(16 * time.Minute)
		require.NoError(t, group.DecreaseTargetSize(t.Context(), -1))
		require.NoError(t, provider.Refresh(t.Context()))
		require.Equal(t, []string{"start:0"}, world.history())
		target, err := group.TargetSize(t.Context())
		require.NoError(t, err)
		require.Equal(t, 1, target)
		current, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, apiv1.ConditionFalse, nodeSuspended(current).Status)
		require.NotContains(t, current.Annotations, scaleDownDisabledAnnotation)
		require.False(t, taints.HasTaint(current, suspendedTaintKey))
	})
}

func TestSuspendedRefreshRecoversSettledMetadata(t *testing.T) {
	for _, tc := range []struct {
		name         string
		power        string
		provisioning string
		suspended    bool
		pending      *suspendedPowerOperation
		deleteMode   bool
		want         apiv1.ConditionStatus
	}{
		{name: "park before restart", power: vmPowerStateDeallocated, want: apiv1.ConditionTrue},
		{name: "legacy park", power: vmPowerStateDeallocated, suspended: true, want: apiv1.ConditionTrue},
		{name: "start before restart", power: vmPowerStateRunning, suspended: true, want: apiv1.ConditionFalse},
		{name: "ordinary running node", power: vmPowerStateRunning},
		{name: "pending Start", power: vmPowerStateDeallocated, pending: &suspendedPowerOperation{polling: true},
			suspended: true, want: apiv1.ConditionTrue},
		{name: "uncertain Start", power: vmPowerStateDeallocated, pending: &suspendedPowerOperation{},
			suspended: true, want: apiv1.ConditionTrue},
		{name: "pending Deallocate", power: vmPowerStateDeallocated, pending: &suspendedPowerOperation{park: true, polling: true}},
		{name: "starting", power: vmPowerStateStarting, suspended: true, want: apiv1.ConditionTrue},
		{name: "deallocating", power: vmPowerStateDeallocating},
		{name: "updating", power: vmPowerStateDeallocated, provisioning: "Updating"},
		{name: "running updating", power: vmPowerStateRunning, provisioning: "Updating",
			suspended: true, want: apiv1.ConditionTrue},
		{name: "failed", power: vmPowerStateDeallocated, provisioning: VMProvisioningStateFailed},
		{name: "Delete unchanged", power: vmPowerStateDeallocated, deleteMode: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := suspendedTestNode(0, tc.suspended)
			custom := apiv1.Taint{Key: "example.com/keep", Effect: apiv1.TaintEffectPreferNoSchedule}
			node.Spec.Taints = append(node.Spec.Taints, custom)
			node.Annotations = map[string]string{"example.com/keep": "true"}
			if tc.suspended {
				node.Annotations[scaleDownDisabledAnnotation] = "true"
			}
			client := fake.NewClientset(node)
			world := &suspendedWorld{states: []string{tc.power}, provisioning: map[int]string{}}
			provider, group := newSuspendedProvider(t, world, client, 0, 3)
			world.provisioning[0] = tc.provisioning
			if tc.provisioning == "" {
				delete(world.provisioning, 0)
			}
			group.invalidateSuspendedInventory()
			group.deallocate = !tc.deleteMode
			if tc.pending != nil {
				group.recordPower("vm-0", *tc.pending)
			}
			require.NoError(t, provider.Refresh(t.Context()))
			current, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
			require.NoError(t, err)
			if tc.want == "" {
				require.Equal(t, node, current)
			} else {
				require.Equal(t, tc.want, nodeSuspended(current).Status)
				require.Equal(t, tc.want == apiv1.ConditionTrue, taints.HasTaint(current, suspendedTaintKey))
				require.Equal(t, tc.want == apiv1.ConditionTrue, current.Annotations[scaleDownDisabledAnnotation] == "true")
				transition := nodeSuspended(current).LastTransitionTime
				require.NoError(t, provider.Refresh(t.Context()))
				again, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.Equal(t, transition, nodeSuspended(again).LastTransitionTime)
			}
			require.Equal(t, node.UID, current.UID)
			require.Equal(t, node.Spec.PodCIDR, current.Spec.PodCIDR)
			require.Contains(t, current.Spec.Taints, custom)
			require.Equal(t, "true", current.Annotations["example.com/keep"])
			require.Empty(t, world.history())
		})
	}
}

func TestSuspendedRefreshRepairsPartialMetadata(t *testing.T) {
	for _, parked := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "park"}[parked], func(t *testing.T) {
			node := suspendedTestNode(0, true)
			power := vmPowerStateRunning
			if parked {
				node.Spec.Taints = nil
				node.Annotations = nil
				power = vmPowerStateDeallocated
			} else {
				setSuspendedCondition(node, apiv1.ConditionFalse, resumedReason, metav1.Now())
			}
			client := fake.NewClientset(node)
			provider, _ := newSuspendedProvider(t, &suspendedWorld{states: []string{power}}, client, 0, 3)
			fail := true
			client.PrependReactor("update", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
				if fail {
					return true, nil, errors.New("metadata unavailable")
				}
				return false, nil, nil
			})
			require.ErrorContains(t, provider.Refresh(t.Context()), "metadata unavailable")
			fail = false
			require.NoError(t, provider.Refresh(t.Context()))
			current, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, nodeSuspended(node).LastTransitionTime, nodeSuspended(current).LastTransitionTime)
			require.Equal(t, parked, taints.HasTaint(current, suspendedTaintKey))
			require.Equal(t, parked, current.Annotations[scaleDownDisabledAnnotation] == "true")
		})
	}
}

func TestSuspendedRefreshSkipsCompleteMetadata(t *testing.T) {
	node := suspendedTestNode(0, true)
	client := fake.NewClientset(node)
	world := &suspendedWorld{states: []string{vmPowerStateDeallocated}}
	provider, _ := newSuspendedProvider(t, world, client, 0, 3)
	client.ClearActions()
	require.NoError(t, provider.Refresh(t.Context()))
	require.Len(t, client.Actions(), 1)
	require.Equal(t, "list", client.Actions()[0].GetVerb())
	require.Empty(t, world.history())
}

func TestSuspendedRefreshSharesAndLimitsNodeLists(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		firstNode := suspendedTestNode(0, false)
		secondNode := firstNode.DeepCopy()
		secondNode.Name = "node-pool2"
		secondNode.UID = types.UID("pool2")
		secondNode.Spec.ProviderID = strings.Replace(firstNode.Spec.ProviderID, "/pool/", "/pool2/", 1)
		client := fake.NewClientset(firstNode, secondNode)
		world := &suspendedWorld{states: []string{vmPowerStateDeallocated}}
		provider, first := newSuspendedProvider(t, world, client, 0, 3)
		manager := provider.azureManager
		instanceClient := mock_virtualmachinescalesetvmclient.NewMockInterface(gomock.NewController(t))
		instanceClient.EXPECT().ListVMInstanceView(gomock.Any(), "rg", gomock.Any()).DoAndReturn(
			func(_ context.Context, _ string, name string) ([]*armcompute.VirtualMachineScaleSetVM, error) {
				vms, err := world.vms()
				for _, vm := range vms {
					vm.ID = ptr.To(strings.Replace(*vm.ID, "/pool/", "/"+name+"/", 1))
				}
				return vms, err
			}).AnyTimes()
		manager.azClient.virtualMachineScaleSetVMsClient = instanceClient
		secondTemplate := world.vmss()
		secondTemplate.Name = ptr.To("pool2")
		secondTemplate.ID = ptr.To(strings.Replace(*secondTemplate.ID, "/pool", "/pool2", 1))
		manager.azureCache.setScaleSet("pool2", secondTemplate)
		second, err := NewScaleSet(&dynamic.NodeGroupSpec{Name: "pool2", MinSize: 0, MaxSize: 3},
			manager, 1, false)
		require.NoError(t, err)
		manager.RegisterNodeGroup(second)
		manager.azureCache.refreshInterval = time.Hour
		first.instancesRefreshPeriod, second.instancesRefreshPeriod = time.Minute, time.Minute
		listCalls := func() int {
			count := 0
			for _, action := range client.Actions() {
				if action.GetVerb() == "list" && action.GetResource().Resource == "nodes" {
					count++
				}
			}
			return count
		}
		client.ClearActions()
		require.NoError(t, provider.Refresh(t.Context()))
		require.Equal(t, 1, listCalls())
		for _, node := range []*apiv1.Node{firstNode, secondNode} {
			current, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.True(t, hasSuspendedMetadata(current))
		}
		for range 5 {
			require.NoError(t, provider.Refresh(t.Context()))
		}
		require.Equal(t, 1, listCalls())
		current, err := client.CoreV1().Nodes().Get(t.Context(), firstNode.Name, metav1.GetOptions{})
		require.NoError(t, err)
		setSuspendedCondition(current, apiv1.ConditionFalse, resumedReason, metav1.Now())
		_, err = client.CoreV1().Nodes().UpdateStatus(t.Context(), current, metav1.UpdateOptions{})
		require.NoError(t, err)
		first.recordPower("vm-0", suspendedPowerOperation{node: firstNode, park: true, completed: true})
		require.NoError(t, provider.Refresh(t.Context()))
		current, err = client.CoreV1().Nodes().Get(t.Context(), firstNode.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, apiv1.ConditionTrue, nodeSuspended(current).Status)
		require.Equal(t, 1, listCalls(), "pending power reconciliation must not wait for a settled scan")
		time.Sleep(time.Minute - time.Second)
		require.NoError(t, provider.Refresh(t.Context()))
		require.Equal(t, 1, listCalls())
		time.Sleep(time.Second)
		require.NoError(t, provider.Refresh(t.Context()))
		require.Equal(t, 2, listCalls())
		require.NoError(t, provider.Refresh(t.Context()))
		require.Equal(t, 2, listCalls())
		require.Empty(t, world.history())
	})
}

func TestSuspendedRefreshRepairsParkedMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*apiv1.Node)
	}{
		{name: "condition missing", mutate: func(n *apiv1.Node) { n.Status.Conditions = nil }},
		{name: "annotation missing", mutate: func(n *apiv1.Node) { n.Annotations = nil }},
		{name: "taint missing", mutate: func(n *apiv1.Node) { n.Spec.Taints = nil }},
		{name: "taint wrong effect", mutate: func(n *apiv1.Node) { n.Spec.Taints[0].Effect = apiv1.TaintEffectNoExecute }},
		{name: "taint duplicated", mutate: func(n *apiv1.Node) { n.Spec.Taints = append(n.Spec.Taints, n.Spec.Taints[0]) }},
		{name: "deletion taint", mutate: func(n *apiv1.Node) {
			n.Spec.Unschedulable = true
			n.Spec.Taints = append(n.Spec.Taints, apiv1.Taint{Key: taints.ToBeDeletedTaint, Effect: apiv1.TaintEffectNoSchedule})
		}},
		{name: "deletion candidate taint", mutate: func(n *apiv1.Node) {
			n.Spec.Taints = append(n.Spec.Taints, apiv1.Taint{Key: taints.DeletionCandidateTaintKey})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := suspendedTestNode(0, true)
			tc.mutate(node)
			client := fake.NewClientset(node)
			provider, _ := newSuspendedProvider(t,
				&suspendedWorld{states: []string{vmPowerStateDeallocated}}, client, 0, 3)
			require.NoError(t, provider.Refresh(t.Context()))
			current, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.True(t, hasSuspendedMetadata(current))
			require.False(t, current.Spec.Unschedulable)
		})
	}
}

func TestSuspendedResumeAtConfiguredMaximum(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nodes := []*apiv1.Node{suspendedTestNode(0, false), suspendedTestNode(1, false), suspendedTestNode(2, false)}
		world := &suspendedWorld{states: []string{vmPowerStateRunning, vmPowerStateRunning, vmPowerStateRunning}}
		_, group := newSuspendedProvider(t, world, fake.NewClientset(nodes[0], nodes[1], nodes[2]), 1, 3)
		require.NoError(t, group.DeleteNodes(t.Context(), nodes[:2]))
		total, err := group.TargetSize(t.Context())
		require.NoError(t, err)
		require.Equal(t, 3, total)
		require.Equal(t, 5, group.MaxSize(t.Context()))
		require.Equal(t, 2, group.MaxSize(t.Context())-total)
		require.NoError(t, group.IncreaseSize(t.Context(), 2))
		synctest.Wait()
		require.Equal(t, []string{"deallocate:0", "deallocate:1", "start:0", "start:1"}, world.history())
		total, err = group.TargetSize(t.Context())
		require.NoError(t, err)
		require.Equal(t, 3, total)
		require.Equal(t, 1, group.MinSize(t.Context()))
		require.Equal(t, 3, group.MaxSize(t.Context()))
		require.ErrorContains(t, group.IncreaseSize(t.Context(), 1), "size increase too large")
		require.Len(t, world.history(), 4)
	})
}

func TestSuspendedRunningMinimum(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nodes := []*apiv1.Node{suspendedTestNode(0, false), suspendedTestNode(1, false), suspendedTestNode(2, false)}
		world := &suspendedWorld{states: []string{vmPowerStateRunning, vmPowerStateRunning, vmPowerStateRunning}}
		_, group := newSuspendedProvider(t, world, fake.NewClientset(nodes[0], nodes[1], nodes[2]), 1, 3)
		require.ErrorContains(t, group.DeleteNodes(t.Context(), nodes), "min size reached")
		require.Empty(t, world.history())
		require.NoError(t, group.DeleteNodes(t.Context(), nodes[:2]))
		require.Equal(t, 3, group.MinSize(t.Context()))
		require.ErrorContains(t, group.DeleteNodes(t.Context(), nodes[2:]), "min size reached")
		require.NoError(t, group.DeleteNodes(t.Context(), nodes[:1]), "an already parked Node costs no running capacity")
		require.Len(t, world.history(), 2)
		require.NoError(t, group.ForceDeleteNodes(t.Context(), nodes[2:]))
		total, err := group.TargetSize(t.Context())
		require.NoError(t, err)
		deficit := group.MinSize(t.Context()) - total
		require.Equal(t, 1, deficit)
		require.NoError(t, group.IncreaseSize(t.Context(), deficit))
		synctest.Wait()
		require.Equal(t, []string{"deallocate:0", "deallocate:1", "deallocate:2", "start:0"}, world.history())
		total, err = group.TargetSize(t.Context())
		require.NoError(t, err)
		require.Equal(t, total, group.MinSize(t.Context()), "resuming one VM meets the running minimum")
	})
}

func TestSuspendedPendingParksReserveMinimum(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nodes := []*apiv1.Node{suspendedTestNode(0, false), suspendedTestNode(1, false), suspendedTestNode(2, false)}
		polling, finish := make(chan struct{}), make(chan struct{})
		world := &suspendedWorld{states: []string{vmPowerStateRunning, vmPowerStateRunning, vmPowerStateRunning}}
		world.parkResult = func(int) error {
			close(polling)
			<-finish
			world.mu.Lock()
			world.states[0] = vmPowerStateDeallocated
			world.mu.Unlock()
			return nil
		}
		_, group := newSuspendedProvider(t, world, fake.NewClientset(nodes[0], nodes[1], nodes[2]), 1, 3)
		result := make(chan error, 1)
		go func() { result <- group.DeleteNodes(t.Context(), nodes[:1]) }()
		<-polling
		require.Equal(t, 2, group.MinSize(t.Context()))
		require.Equal(t, 4, group.MaxSize(t.Context()))
		require.ErrorContains(t, group.DeleteNodes(t.Context(), nodes[1:]), "min size reached")
		require.Equal(t, []string{"deallocate:0"}, world.history())
		close(finish)
		require.NoError(t, <-result)
		require.Equal(t, 2, group.MinSize(t.Context()))
	})
}

func TestSuspendedFailedParkRetry(t *testing.T) {
	node := suspendedTestNode(0, false)
	client := fake.NewClientset(node)
	world := &suspendedWorld{states: []string{vmPowerStateRunning}}
	world.parkResult = func(int) error {
		return &azcore.ResponseError{StatusCode: http.StatusOK}
	}
	_, group := newSuspendedProvider(t, world, client, 0, 1)
	require.Error(t, group.DeleteNodes(t.Context(), []*apiv1.Node{node}))
	current, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Nil(t, nodeSuspended(current))
	require.Zero(t, group.MinSize(t.Context()))
	require.Equal(t, 1, group.MaxSize(t.Context()))
	world.parkResult = nil
	require.NoError(t, group.DeleteNodes(t.Context(), []*apiv1.Node{node}))
	require.Equal(t, []string{"deallocate:0", "deallocate:0"}, world.history(),
		"a failed Deallocate must not make a running VM appear parked on retry")
}

func TestSuspendedStartSubmissionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		submitErr error
		nilPoller bool
		pollErr   error
		rejected  bool
		cacheLag  bool
	}{
		{name: "definite rejection", submitErr: &azcore.ResponseError{StatusCode: http.StatusBadRequest}, rejected: true},
		{name: "throttling", submitErr: &azcore.ResponseError{StatusCode: http.StatusTooManyRequests}, rejected: true},
		{name: "ambiguous timeout", submitErr: context.DeadlineExceeded},
		{name: "ambiguous conflict", submitErr: &azcore.ResponseError{StatusCode: http.StatusConflict}},
		{name: "nil poller", nilPoller: true},
		{name: "poll timeout", pollErr: context.DeadlineExceeded},
		{name: "async failure", pollErr: &azcore.ResponseError{StatusCode: http.StatusOK}},
		{name: "successful start cache lag", cacheLag: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				node := suspendedTestNode(0, true)
				client := fake.NewClientset(node)
				world := &suspendedWorld{
					states: []string{vmPowerStateDeallocated}, startError: map[int]error{0: tc.submitErr}, nilStart: tc.nilPoller,
				}
				if tc.pollErr != nil || tc.cacheLag {
					world.startResult = func(int) error { return tc.pollErr }
				}
				_, group := newSuspendedProvider(t, world, client, 0, 3)
				err := group.IncreaseSize(t.Context(), 1)
				if tc.pollErr == nil && !tc.cacheLag {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
				synctest.Wait()
				current, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
				require.NoError(t, err)
				if tc.rejected {
					require.Equal(t, 4, group.MaxSize(t.Context()))
					require.Equal(t, 1, group.MinSize(t.Context()))
					require.Equal(t, node.Status, current.Status)
					require.Equal(t, node.Spec, current.Spec)
					require.Equal(t, node.Annotations, current.Annotations)
					require.Empty(t, group.pendingPower)
					return
				}
				require.Equal(t, apiv1.ConditionFalse, nodeSuspended(current).Status)
				require.Equal(t, 3, group.MaxSize(t.Context()), "accepted starts consume headroom despite parked Azure inventory")
				require.Zero(t, group.MinSize(t.Context()))
				require.NoError(t, group.IncreaseSize(t.Context(), 1))
				require.Equal(t, []string{"start:0", "grow:2"}, world.history(),
					"new demand grows without resubmitting the uncertain start")
				instances, err := group.Nodes(t.Context())
				require.NoError(t, err)
				require.Equal(t, cloudprovider.InstanceCreating, instances[0].Status.State)
				require.Nil(t, instances[0].Status.ErrorInfo)
			})
		})
	}
}

func TestSuspendedPartialSubmissions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nodes := []*apiv1.Node{suspendedTestNode(0, true), suspendedTestNode(1, true)}
		client := fake.NewClientset(nodes[0], nodes[1])
		world := &suspendedWorld{
			states:      []string{vmPowerStateDeallocated, vmPowerStateDeallocated},
			startError:  map[int]error{1: &azcore.ResponseError{StatusCode: http.StatusBadRequest}},
			startResult: func(int) error { return context.DeadlineExceeded },
		}
		_, group := newSuspendedProvider(t, world, client, 0, 5)
		require.Error(t, group.IncreaseSize(t.Context(), 2))
		synctest.Wait()
		for i, status := range []apiv1.ConditionStatus{apiv1.ConditionFalse, apiv1.ConditionTrue} {
			current, err := client.CoreV1().Nodes().Get(t.Context(), nodes[i].Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, status, nodeSuspended(current).Status)
		}
		world.mu.Lock()
		delete(world.startError, 1)
		world.mu.Unlock()
		require.NoError(t, group.IncreaseSize(t.Context(), 1))
		require.Equal(t, []string{"start:0", "start:1", "start:1"}, world.history())
	})
	t.Run("partial park", func(t *testing.T) {
		nodes := []*apiv1.Node{suspendedTestNode(0, false), suspendedTestNode(1, false)}
		client := fake.NewClientset(nodes[0], nodes[1])
		world := &suspendedWorld{
			states:    []string{vmPowerStateRunning, vmPowerStateRunning},
			parkError: map[int]error{1: &azcore.ResponseError{StatusCode: http.StatusBadRequest}},
		}
		_, group := newSuspendedProvider(t, world, client, 0, 5)
		require.Error(t, group.DeleteNodes(t.Context(), nodes))
		first, err := client.CoreV1().Nodes().Get(t.Context(), nodes[0].Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, apiv1.ConditionTrue, nodeSuspended(first).Status)
		second, err := client.CoreV1().Nodes().Get(t.Context(), nodes[1].Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Nil(t, nodeSuspended(second))
	})
}

func TestSuspendedFailedStartCanBeParked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := suspendedTestNode(0, true)
		client := fake.NewClientset(node)
		world := &suspendedWorld{states: []string{vmPowerStateDeallocated},
			startResult: func(int) error {
				return &azcore.ResponseError{StatusCode: http.StatusOK}
			}}
		_, group := newSuspendedProvider(t, world, client, 0, 3)
		require.NoError(t, group.IncreaseSize(t.Context(), 1))
		synctest.Wait()
		require.NoError(t, group.ForceDeleteNodes(t.Context(), []*apiv1.Node{node}))
		current, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, apiv1.ConditionTrue, nodeSuspended(current).Status)
		require.Equal(t, node.UID, current.UID)
		require.Equal(t, []string{"start:0"}, world.history())
	})
}

func TestSuspendedRegisteredFailedVMCanBeParked(t *testing.T) {
	node := suspendedTestNode(0, false)
	client := fake.NewClientset(node)
	world := &suspendedWorld{states: []string{vmPowerStateDeallocated},
		provisioning: map[int]string{0: VMProvisioningStateFailed}}
	_, group := newSuspendedProvider(t, world, client, 0, 3)
	require.NoError(t, group.ForceDeleteNodes(t.Context(), []*apiv1.Node{node}))
	current, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, apiv1.ConditionTrue, nodeSuspended(current).Status)
	require.Equal(t, []string{"deallocate:0"}, world.history())
}

func TestSuspendedConcurrentParking(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := suspendedTestNode(0, false)
		polling, finish := make(chan struct{}), make(chan struct{})
		world := &suspendedWorld{states: []string{vmPowerStateRunning}}
		world.parkResult = func(int) error {
			close(polling)
			<-finish
			world.mu.Lock()
			world.states[0] = vmPowerStateDeallocated
			world.mu.Unlock()
			return nil
		}
		_, group := newSuspendedProvider(t, world, fake.NewClientset(node), 0, 3)
		result := make(chan error, 1)
		go func() { result <- group.DeleteNodes(t.Context(), []*apiv1.Node{node}) }()
		<-polling
		instances, err := group.Nodes(t.Context())
		require.NoError(t, err)
		require.Equal(t, cloudprovider.InstanceCreating, instances[0].Status.State)
		require.ErrorContains(t, group.DeleteNodes(t.Context(), []*apiv1.Node{node}), "power operation pending")
		require.ErrorContains(t, group.IncreaseSize(t.Context(), 1), "parking is still in progress")
		require.Equal(t, []string{"deallocate:0"}, world.history())
		close(finish)
		require.NoError(t, <-result)
	})
}

func TestSuspendedParkSubmissionAndCacheLag(t *testing.T) {
	for _, tc := range []struct {
		name      string
		submitErr error
		nilPoller bool
		pollErr   error
		rejected  bool
		cacheLag  bool
	}{
		{name: "rejected", submitErr: &azcore.ResponseError{StatusCode: http.StatusBadRequest}, rejected: true},
		{name: "nil poller", nilPoller: true},
		{name: "ambiguous submission", submitErr: context.DeadlineExceeded},
		{name: "poll timeout", pollErr: context.DeadlineExceeded},
		{name: "completed with inventory lag", cacheLag: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := suspendedTestNode(0, false)
			client := fake.NewClientset(node)
			world := &suspendedWorld{states: []string{vmPowerStateRunning},
				parkError: map[int]error{0: tc.submitErr}, nilPark: tc.nilPoller}
			if tc.pollErr != nil || tc.cacheLag {
				world.parkResult = func(int) error { return tc.pollErr }
			}
			provider, group := newSuspendedProvider(t, world, client, 0, 3)
			err := group.DeleteNodes(t.Context(), []*apiv1.Node{node})
			if tc.cacheLag {
				require.NoError(t, err)
				require.NoError(t, provider.Refresh(t.Context()))
				current, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.Equal(t, apiv1.ConditionTrue, nodeSuspended(current).Status)
				require.NoError(t, group.DeleteNodes(t.Context(), []*apiv1.Node{current}))
				require.Equal(t, []string{"deallocate:0"}, world.history())
				return
			}
			require.Error(t, err)
			current, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Nil(t, nodeSuspended(current), "a park must not be published before confirmation")
			if tc.rejected {
				require.Empty(t, group.pendingPower)
				return
			}
			require.ErrorContains(t, group.DeleteNodes(t.Context(), []*apiv1.Node{node}), "power operation pending")
			require.ErrorContains(t, group.IncreaseSize(t.Context(), 1), "parking is still in progress")
			require.Equal(t, []string{"deallocate:0"}, world.history())
			world.mu.Lock()
			world.states[0] = vmPowerStateDeallocated
			world.mu.Unlock()
			require.NoError(t, group.DeleteNodes(t.Context(), []*apiv1.Node{node}))
			current, err = client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, apiv1.ConditionTrue, nodeSuspended(current).Status)
			require.Equal(t, []string{"deallocate:0"}, world.history())
		})
	}
}

func TestSuspendedNodeUpdateRetriesAndFencing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace func(*apiv1.Node)
	}{
		{name: "conflict"},
		{name: "UID replaced", replace: func(n *apiv1.Node) { n.UID = types.UID("replacement") }},
		{name: "provider ID replaced", replace: func(n *apiv1.Node) { n.Spec.ProviderID += "-replacement" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := suspendedTestNode(0, false)
			client := fake.NewClientset(node)
			_, group := newSuspendedProvider(t, &suspendedWorld{states: []string{vmPowerStateRunning}}, client, 0, 3)
			if tc.replace != nil {
				replacement := node.DeepCopy()
				tc.replace(replacement)
				_, err := client.CoreV1().Nodes().Update(t.Context(), replacement, metav1.UpdateOptions{})
				require.NoError(t, err)
				require.ErrorContains(t, group.setNodeSuspended(t.Context(), node, true), "changed identity")
				current, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.Nil(t, nodeSuspended(current))
				return
			}
			conflicts := 0
			client.PrependReactor("update", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
				if action.GetSubresource() == "status" && conflicts == 0 {
					conflicts++
					return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, node.Name, errors.New("conflict"))
				}
				return false, nil, nil
			})
			require.NoError(t, group.setNodeSuspended(t.Context(), node, true))
			require.Equal(t, 1, conflicts)
		})
		t.Run("replacement after Azure accepted deallocation", func(t *testing.T) {
			node := suspendedTestNode(0, false)
			client := fake.NewClientset(node)
			world := &suspendedWorld{states: []string{vmPowerStateRunning}}
			world.parkResult = func(int) error {
				current, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				current.UID = types.UID("replacement")
				if _, err := client.CoreV1().Nodes().Update(t.Context(), current, metav1.UpdateOptions{}); err != nil {
					return err
				}
				world.mu.Lock()
				world.states[0] = vmPowerStateDeallocated
				world.mu.Unlock()
				return nil
			}
			_, group := newSuspendedProvider(t, world, client, 0, 3)
			require.ErrorContains(t, group.DeleteNodes(t.Context(), []*apiv1.Node{node}), "changed identity")
			current, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Nil(t, nodeSuspended(current))
			require.Equal(t, types.UID("replacement"), current.UID)
		})
	}
	t.Run("accepted start repairs failed Node update without resubmission", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			node := suspendedTestNode(0, true)
			client := fake.NewClientset(node)
			world := &suspendedWorld{states: []string{vmPowerStateDeallocated}}
			provider, group := newSuspendedProvider(t, world, client, 0, 1)
			fail := true
			client.PrependReactor("update", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
				if fail {
					return true, nil, errors.New("status unavailable")
				}
				return false, nil, nil
			})
			require.ErrorContains(t, group.IncreaseSize(t.Context(), 1), "status unavailable")
			synctest.Wait()
			total, err := group.TargetSize(t.Context())
			require.NoError(t, err)
			require.Equal(t, group.MaxSize(t.Context()), total)
			fail = false
			require.NoError(t, provider.Refresh(t.Context()))
			require.Equal(t, []string{"start:0"}, world.history())
			current, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, apiv1.ConditionFalse, nodeSuspended(current).Status)
			require.False(t, taints.HasTaint(current, suspendedTaintKey))
			require.NotContains(t, current.Annotations, scaleDownDisabledAnnotation)
		})
	})
	t.Run("accepted park repairs failed Node update through refresh", func(t *testing.T) {
		node := suspendedTestNode(0, false)
		client := fake.NewClientset(node)
		world := &suspendedWorld{states: []string{vmPowerStateRunning}}
		provider, group := newSuspendedProvider(t, world, client, 0, 1)
		fail := true
		client.PrependReactor("update", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
			if fail {
				return true, nil, errors.New("status unavailable")
			}
			return false, nil, nil
		})
		require.ErrorContains(t, group.DeleteNodes(t.Context(), []*apiv1.Node{node}), "status unavailable")
		fail = false
		require.NoError(t, provider.Refresh(t.Context()))
		require.Equal(t, []string{"deallocate:0"}, world.history())
		current, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, apiv1.ConditionTrue, nodeSuspended(current).Status)
		require.True(t, taints.HasTaint(current, suspendedTaintKey))
		require.Equal(t, "true", current.Annotations[scaleDownDisabledAnnotation])
	})
}

func TestSuspendedSyntheticCleanupIsPhysical(t *testing.T) {
	for _, reason := range []string{cloudprovider.FakeNodeUnregistered, cloudprovider.FakeNodeCreateError} {
		t.Run(reason, func(t *testing.T) {
			world := &suspendedWorld{states: []string{vmPowerStateRunning}}
			node := suspendedTestNode(0, false)
			node.UID = ""
			node.Annotations = map[string]string{cloudprovider.FakeNodeReasonAnnotation: reason}
			client := fake.NewClientset()
			provider, group := newSuspendedProvider(t, world, client, 0, 3)
			require.NoError(t, group.ForceDeleteNodes(t.Context(), []*apiv1.Node{node}))
			require.Equal(t, []string{"physical-delete:0"}, world.history())
			hasInstance, err := provider.HasInstance(t.Context(), node)
			require.NoError(t, err)
			require.False(t, hasInstance)
			instances, err := group.Nodes(t.Context())
			require.NoError(t, err)
			for _, instance := range instances {
				require.Equal(t, cloudprovider.InstanceDeleting, instance.Status.State)
			}
		})
	}
	t.Run("registered instance cannot enter synthetic cleanup", func(t *testing.T) {
		world := &suspendedWorld{states: []string{vmPowerStateRunning}}
		registered := suspendedTestNode(0, false)
		_, group := newSuspendedProvider(t, world, fake.NewClientset(registered), 0, 3)
		synthetic := registered.DeepCopy()
		synthetic.UID = ""
		synthetic.Annotations = map[string]string{cloudprovider.FakeNodeReasonAnnotation: cloudprovider.FakeNodeCreateError}
		require.ErrorContains(t, group.ForceDeleteNodes(t.Context(), []*apiv1.Node{synthetic}), "registered instance")
		require.Empty(t, world.history())
	})
}
