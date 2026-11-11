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
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetclient/mock_virtualmachinescalesetclient"
	"sigs.k8s.io/cluster-autoscaler/pkg/cloudprovider"
)

func TestParseAzureNodeGroupSpec(t *testing.T) {
	for _, tc := range []struct {
		name, value, policy string
		wantError           bool
	}{
		{name: "core", value: "0:3:pool", policy: scaleDownPolicyDelete},
		{name: "Delete", value: "0:3:Delete:pool", policy: scaleDownPolicyDelete},
		{name: "Deallocate", value: "0:3:Deallocate:pool", policy: scaleDownPolicyDeallocate},
		{name: "fifth field rejected", value: `0:3:Deallocate:pool:{}`, wantError: true},
		{name: "empty fifth field rejected", value: "0:3:Deallocate:pool:", wantError: true},
		{name: "invalid policy", value: "0:3:Stop:pool", wantError: true},
		{name: "invalid minimum", value: "-1:3:Deallocate:pool", wantError: true},
		{name: "invalid maximum", value: "2:1:Deallocate:pool", wantError: true},
		{name: "missing name", value: "0:3:Deallocate:", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := parseAzureNodeGroupSpec(tc.value, true)
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "pool", spec.Name)
			require.Equal(t, tc.policy, spec.policy)
		})
	}
	_, err := parseAzureNodeGroupSpec("0:3:pool", false)
	require.Error(t, err)
}

func TestSuspendedGroupGates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*AzureManager, *armcompute.VirtualMachineScaleSet)
	}{
		{name: "hosted", mutate: func(m *AzureManager, _ *armcompute.VirtualMachineScaleSet) {
			m.config.HostedSubscriptionID = "hosted"
		}},
		{name: "standard", mutate: func(m *AzureManager, _ *armcompute.VirtualMachineScaleSet) {
			m.config.VMType = "standard"
		}},
		{name: "flex", mutate: func(_ *AzureManager, set *armcompute.VirtualMachineScaleSet) {
			set.Properties.OrchestrationMode = ptr.To(armcompute.OrchestrationModeFlexible)
		}},
		{name: "Spot", mutate: func(_ *AzureManager, set *armcompute.VirtualMachineScaleSet) {
			set.Properties.VirtualMachineProfile.Priority = ptr.To(armcompute.VirtualMachinePriorityTypesSpot)
		}},
		{name: "AKS managed", mutate: func(_ *AzureManager, set *armcompute.VirtualMachineScaleSet) {
			set.Tags["AKS-MANAGED-POOLNAME"] = ptr.To("workers")
		}},
		{name: "ephemeral", mutate: func(_ *AzureManager, set *armcompute.VirtualMachineScaleSet) {
			set.Properties.VirtualMachineProfile.StorageProfile.OSDisk.DiffDiskSettings = &armcompute.DiffDiskSettings{}
		}},
		{name: "unmanaged disk", mutate: func(_ *AzureManager, set *armcompute.VirtualMachineScaleSet) {
			set.Properties.VirtualMachineProfile.StorageProfile.OSDisk.ManagedDisk = nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			world := &suspendedWorld{states: []string{vmPowerStateRunning}}
			provider, original := newSuspendedProvider(t, world, fake.NewClientset(), 0, 3)
			m := provider.azureManager
			m.UnregisterNodeGroup(original)
			m.config.Deallocate = false
			set := world.vmss()
			tc.mutate(m, set)
			m.azureCache.setScaleSet("pool", set)
			_, err := m.buildNodeGroupFromSpec("1:3:Deallocate:pool")
			var unsupported *unsupportedDeallocateError
			require.ErrorAs(t, err, &unsupported)
			specs := []string{"1:3:Delete:POOL", "1:3:Deallocate:pool"}
			if m.config.VMType == "standard" {
				specs[0] = "1:3:Deallocate:POOL"
			}
			require.NoError(t, m.fetchExplicitNodeGroups(specs))
			require.Empty(t, m.getNodeGroups(), "unsupported policy suppresses duplicate case variants")
			if m.config.VMType == "vmss" {
				m.autoDiscoverySpecs, err = ParseLabelAutoDiscoverySpecs(cloudprovider.NodeGroupDiscoveryOptions{
					NodeGroupAutoDiscoverySpecs: []string{"label:discover=yes"},
				})
				require.NoError(t, err)
				set.Tags["discover"], set.Tags["min"], set.Tags["max"] = ptr.To("yes"), ptr.To("0"), ptr.To("3")
				require.NoError(t, m.fetchAutoNodeGroups())
				require.Empty(t, m.getNodeGroups(), "unsupported explicit group must not be rediscovered as Delete")
			}
		})
	}
}

func TestSuspendedGlobalPolicy(t *testing.T) {
	world := &suspendedWorld{states: []string{vmPowerStateRunning}}
	provider, _ := newSuspendedProvider(t, world, fake.NewClientset(), 0, 3)
	m := provider.azureManager
	group, err := m.buildNodeGroupFromSpec("0:3:Delete:pool")
	require.NoError(t, err)
	scaleSet := group.(*ScaleSet)
	require.True(t, scaleSet.suspendsParkedNodes(), "global policy also applies to explicit Delete")
	set := world.vmss()
	set.Properties.VirtualMachineProfile.Priority = ptr.To(armcompute.VirtualMachinePriorityTypesSpot)
	set.Tags["discover"], set.Tags["min"], set.Tags["max"] = ptr.To("yes"), ptr.To("0"), ptr.To("3")
	m.azureCache.setScaleSet("pool", set)
	_, err = m.buildNodeGroupFromSpec("0:3:Delete:pool")
	require.Error(t, err, "unsupported global groups must not fall back to Delete")
	specs, err := ParseLabelAutoDiscoverySpecs(cloudprovider.NodeGroupDiscoveryOptions{
		NodeGroupAutoDiscoverySpecs: []string{"label:discover=yes"},
	})
	require.NoError(t, err)
	groups, err := m.getFilteredScaleSets(specs)
	require.NoError(t, err)
	require.Empty(t, groups)
}

func TestSuspendedSelectedGroupAndVMsPoolGate(t *testing.T) {
	world := &suspendedWorld{states: []string{vmPowerStateRunning}}
	provider, original := newSuspendedProvider(t, world, fake.NewClientset(), 0, 3)
	m := provider.azureManager
	m.UnregisterNodeGroup(original)
	m.config.Deallocate = false
	require.False(t, m.suspendedModeEnabled())
	require.NoError(t, m.fetchExplicitNodeGroups([]string{"0:3:pool", "0:3:Deallocate:POOL"}))
	require.Len(t, m.getNodeGroups(), 1)
	require.True(t, m.getNodeGroups()[0].(*ScaleSet).suspendsParkedNodes())
	require.True(t, m.suspendedModeEnabled())
	m.azureCache.vmsPoolMap["workers"] = armcontainerservice.AgentPool{}
	_, err := m.buildNodeGroupFromSpec("0:3:Deallocate:workers/Standard_D4_v2")
	require.ErrorContains(t, err, "VMs pools are not supported")
}

func TestSuspendedExplicitLoadsInitialInventory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		listErr error
	}{
		{name: "inventory loaded before gate"},
		{name: "failed inventory is not unsupported", listErr: errors.New("inventory unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			world := &suspendedWorld{states: []string{vmPowerStateRunning}}
			provider, _ := newSuspendedProvider(t, world, fake.NewClientset(), 0, 3)
			ctrl := gomock.NewController(t)
			client := mock_virtualmachinescalesetclient.NewMockInterface(ctrl)
			client.EXPECT().List(gomock.Any(), "rg").DoAndReturn(
				func(context.Context, string) ([]*armcompute.VirtualMachineScaleSet, error) {
					return []*armcompute.VirtualMachineScaleSet{world.vmss()}, tc.listErr
				}).AnyTimes()
			provider.azureManager.azClient.virtualMachineScaleSetsClient = client
			cfg := strings.Replace(validAzureCfg, `"resourceGroup": "fakeId"`, `"resourceGroup": "rg"`, 1)
			manager, err := createAzureManagerInternal(strings.NewReader(cfg),
				cloudprovider.NodeGroupDiscoveryOptions{NodeGroupSpecs: []string{"0:3:Deallocate:pool"}},
				provider.azureManager.azClient)
			if tc.listErr != nil {
				require.ErrorContains(t, err, "load inventory for Deallocate groups")
				require.ErrorIs(t, err, tc.listErr)
				return
			}

			require.NoError(t, err)
			t.Cleanup(manager.Cleanup)
			require.Len(t, manager.getNodeGroups(), 1)
			require.True(t, manager.getNodeGroups()[0].(*ScaleSet).suspendsParkedNodes())
			require.True(t, manager.suspendedModeEnabled())
		})
	}
}

func TestSuspendedCapRequiresManagerMode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		global  bool
		specs   []string
		wantErr bool
	}{
		{name: "global", global: true},
		{name: "per group", specs: []string{"0:3:Deallocate:pool"}},
		{name: "disabled", wantErr: true},
		{name: "Delete group", specs: []string{"0:3:Delete:pool"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, _ := newSuspendedProvider(t, &suspendedWorld{states: []string{vmPowerStateRunning}},
				fake.NewClientset(), 0, 3)
			fields := `"resourceGroup":"rg","maxActiveNodes":2`
			if tc.global {
				fields += `,"deallocate":true`
			}
			cfg := strings.Replace(validAzureCfg, `"resourceGroup": "fakeId"`, fields, 1)
			manager, err := createAzureManagerInternal(strings.NewReader(cfg),
				cloudprovider.NodeGroupDiscoveryOptions{NodeGroupSpecs: tc.specs}, provider.azureManager.azClient)
			if tc.wantErr {
				require.ErrorContains(t, err, "maxActiveNodes requires")
				return
			}
			require.NoError(t, err)
			t.Cleanup(manager.Cleanup)
			require.Equal(t, tc.global, manager.config.Deallocate)
			require.True(t, manager.suspendedModeEnabled())
		})
	}
}
