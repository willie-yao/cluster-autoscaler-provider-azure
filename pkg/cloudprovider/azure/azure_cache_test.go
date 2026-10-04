/*
Copyright 2020 The Kubernetes Authors.

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
	"testing"

	providerazureconsts "sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cluster-autoscaler/pkg/cloudprovider"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cluster-autoscaler/pkg/config/dynamic"
)

func TestFetchVMsPools(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	provider := newTestProvider(t)
	ac := provider.azureManager.azureCache
	mockAgentpoolclient := NewMockAgentPoolsClient(ctrl)
	ac.azClient.agentPoolClient = mockAgentpoolclient

	vmsPool := getTestVMsAgentPool(false)
	vmssPoolType := armcontainerservice.AgentPoolTypeVirtualMachineScaleSets
	vmssPool := armcontainerservice.AgentPool{
		Name: ptr.To("vmsspool1"),
		Properties: &armcontainerservice.ManagedClusterAgentPoolProfileProperties{
			Type: &vmssPoolType,
		},
	}
	invalidPool := armcontainerservice.AgentPool{}
	fakeAPListPager := getFakeAgentpoolListPager(&vmsPool, &vmssPool, &invalidPool)
	mockAgentpoolclient.EXPECT().NewListPager(gomock.Any(), gomock.Any(), nil).
		Return(fakeAPListPager)

	vmsPoolMap, err := ac.fetchVMsPools()
	assert.NoError(t, err)
	assert.Equal(t, 1, len(vmsPoolMap))

	_, ok := vmsPoolMap[ptr.Deref(vmsPool.Name, "")]
	assert.True(t, ok)
}

func TestRegister(t *testing.T) {
	provider := newTestProvider(t)
	ss := newTestScaleSet(provider.azureManager, "ss")

	ac := provider.azureManager.azureCache
	ac.registeredNodeGroups = []cloudprovider.NodeGroup{ss}

	isSuccess := ac.Register(ss)
	assert.False(t, isSuccess)

	ss1 := newTestScaleSet(provider.azureManager, "ss")
	ss1.minSize = 2
	isSuccess = ac.Register(ss1)
	assert.True(t, isSuccess)
}

func TestRegisterSuspendedConfiguredBounds(t *testing.T) {
	world := &suspendedWorld{states: []string{vmPowerStateDeallocated, vmPowerStateRunning, vmPowerStateDeallocated}}
	provider, group := newSuspendedProvider(t, world, fake.NewClientset(), 0, 3)
	manager := provider.azureManager
	group.recordPower("vm-0", suspendedPowerOperation{})
	require.Equal(t, 1, group.MinSize(t.Context()))
	require.Equal(t, 4, group.MaxSize(t.Context()))
	next, err := NewScaleSet(&dynamic.NodeGroupSpec{Name: "pool", MinSize: 0, MaxSize: 3}, manager, 3, false)
	require.NoError(t, err)
	require.Equal(t, 2, next.MinSize(t.Context()))
	require.Equal(t, 5, next.MaxSize(t.Context()))
	require.False(t, manager.azureCache.Register(next), "dynamic differences must not replace a group")
	require.Same(t, group, manager.getNodeGroups()[0])

	set := world.vmss()
	set.Tags["discover"], set.Tags["min"], set.Tags["max"] = ptr.To("yes"), ptr.To("0"), ptr.To("3")
	manager.azureCache.setScaleSet("pool", set)
	manager.explicitlyConfigured = make(map[string]bool)
	manager.autoDiscoverySpecs, err = ParseLabelAutoDiscoverySpecs(cloudprovider.NodeGroupDiscoveryOptions{
		NodeGroupAutoDiscoverySpecs: []string{"label:discover=yes"},
	})
	require.NoError(t, err)
	for range 2 {
		require.NoError(t, manager.fetchAutoNodeGroups())
		require.Same(t, group, manager.getNodeGroups()[0])
		op, pending := group.powerOperation("vm-0")
		require.True(t, pending)
		require.False(t, op.park)
		require.Equal(t, 4, group.MaxSize(t.Context()))
	}
}

func TestRegisterScaleSetModeChange(t *testing.T) {
	provider, group := newSuspendedProvider(t, &suspendedWorld{states: []string{vmPowerStateRunning}}, fake.NewClientset(), 0, 3)
	manager := provider.azureManager
	manager.config.Deallocate = false
	next, err := NewScaleSet(&dynamic.NodeGroupSpec{Name: "pool", MinSize: 0, MaxSize: 3}, manager, 1, false)
	require.NoError(t, err)
	require.Equal(t, group.MinSize(context.Background()), next.MinSize(context.Background()))
	require.Equal(t, group.MaxSize(context.Background()), next.MaxSize(context.Background()))
	require.True(t, manager.azureCache.Register(next))
	require.Same(t, next, manager.getNodeGroups()[0])
	require.False(t, next.suspendsParkedNodes())
}

func TestUnRegister(t *testing.T) {
	provider := newTestProvider(t)
	ss := newTestScaleSet(provider.azureManager, "ss")
	ss1 := newTestScaleSet(provider.azureManager, "ss1")

	ac := provider.azureManager.azureCache
	ac.registeredNodeGroups = []cloudprovider.NodeGroup{ss, ss1}

	isSuccess := ac.Unregister(ss)
	assert.True(t, isSuccess)
	assert.Equal(t, 1, len(ac.registeredNodeGroups))
}

func TestFindForInstance(t *testing.T) {
	provider := newTestProvider(t)
	ac := provider.azureManager.azureCache

	inst := azureRef{Name: "/subscriptions/sub/resourceGroups/rg/providers/foo"}
	ac.unownedInstances = make(map[azureRef]bool)
	ac.unownedInstances[inst] = true
	nodeGroup, err := ac.FindForInstance(&inst, providerazureconsts.VMTypeVMSS)
	assert.Nil(t, nodeGroup)
	assert.NoError(t, err)

	ac.unownedInstances[inst] = false
	nodeGroup, err = ac.FindForInstance(&inst, providerazureconsts.VMTypeStandard)
	assert.Nil(t, nodeGroup)
	assert.NoError(t, err)
	assert.True(t, ac.unownedInstances[inst])
}
