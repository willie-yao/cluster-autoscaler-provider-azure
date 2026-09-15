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
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachineclient/mock_virtualmachineclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetclient/mock_virtualmachinescalesetclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetvmclient/mock_virtualmachinescalesetvmclient"
	"sigs.k8s.io/cluster-autoscaler/pkg/cloudprovider"
)

func TestVMSSMigrationBoundaries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		ctrl := gomock.NewController(t)
		manager := newTestAzureManager(t)
		manager.config.EnableForceDelete = false
		var capacity atomic.Int64

		// Only the tagged pool belongs to this controller.
		vmssClient := mock_virtualmachinescalesetclient.NewMockInterface(ctrl)
		vmssClient.EXPECT().List(gomock.Any(), "rg").DoAndReturn(func(context.Context, string) ([]*armcompute.VirtualMachineScaleSet, error) {
			pools := newTestVMSSList(capacity.Load(), testASG, "eastus", armcompute.OrchestrationModeUniform)
			pools[0].Tags = map[string]*string{"cluster-autoscaler-name": ptr.To("migration"), "min": ptr.To("0"), "max": ptr.To("3")}
			return append(pools, newTestVMSSList(2, "unowned-pool", "eastus", armcompute.OrchestrationModeUniform)...), nil
		}).AnyTimes()
		manager.azClient.virtualMachineScaleSetsClient = vmssClient
		vmClient := mock_virtualmachineclient.NewMockInterface(ctrl)
		vmClient.EXPECT().List(gomock.Any(), "rg").Return([]*armcompute.VirtualMachine{}, nil).AnyTimes()
		manager.azClient.virtualMachinesClient = vmClient
		vmssVMClient := mock_virtualmachinescalesetvmclient.NewMockInterface(ctrl)
		vmssVMClient.EXPECT().ListVMInstanceView(gomock.Any(), "rg", testASG).DoAndReturn(func(context.Context, string, string) ([]*armcompute.VirtualMachineScaleSetVM, error) {
			return newTestVMSSVMList(int(capacity.Load())), nil
		}).AnyTimes()
		manager.azClient.virtualMachineScaleSetVMsClient = vmssVMClient
		operations := NewMockVMSSDeleteClient(ctrl)
		manager.azClient.vmssClientForDelete = operations

		var err error
		manager.autoDiscoverySpecs, err = ParseLabelAutoDiscoverySpecs(cloudprovider.NodeGroupDiscoveryOptions{
			NodeGroupAutoDiscoverySpecs: []string{"label:cluster-autoscaler-name=migration"},
		})
		require.NoError(t, err)
		require.NoError(t, manager.forceRefresh())
		require.NoError(t, manager.fetchAutoNodeGroups())
		groups := manager.getNodeGroups()
		require.Len(t, groups, 1)
		group := groups[0]
		require.Equal(t, testASG, group.Id())
		require.Equal(t, 0, group.MinSize(ctx))
		require.Equal(t, 3, group.MaxSize(ctx))
		size, err := group.TargetSize(ctx)
		require.NoError(t, err)
		require.Zero(t, size)
		template, err := group.TemplateNodeInfo(ctx)
		require.NoError(t, err)
		require.Positive(t, template.Node().Status.Capacity.Cpu().MilliValue())
		require.Positive(t, template.Node().Status.Capacity.Memory().Value())

		// Growth is bounded and reaches the Azure capacity-update boundary.
		require.ErrorContains(t, group.IncreaseSize(ctx, 4), "size increase too large")
		operations.EXPECT().BeginCreateOrUpdate(gomock.Any(), "rg", testASG, gomock.Any(), gomock.Any()).Do(func(_ context.Context, _, _ string, vmss armcompute.VirtualMachineScaleSet, _ *armcompute.VirtualMachineScaleSetsClientBeginCreateOrUpdateOptions) {
			assert.Equal(t, int64(2), *vmss.SKU.Capacity)
			capacity.Store(*vmss.SKU.Capacity)
		}).Return(newTestCreateOrUpdatePoller(t, nil), nil).Times(1)
		require.NoError(t, group.IncreaseSize(ctx, 2))
		synctest.Wait()
		require.Equal(t, int64(2), capacity.Load())
		size, err = group.TargetSize(ctx)
		require.NoError(t, err)
		require.Equal(t, 2, size)
		require.NoError(t, manager.forceRefresh())

		// Ordinary scale-down deletes the selected instance without force deletion.
		operations.EXPECT().BeginDeleteInstances(gomock.Any(), "rg", testASG,
			armcompute.VirtualMachineScaleSetVMInstanceRequiredIDs{InstanceIDs: []*string{ptr.To("1")}},
			&armcompute.VirtualMachineScaleSetsClientBeginDeleteInstancesOptions{ForceDeletion: ptr.To(false)},
		).Do(func(context.Context, string, string, armcompute.VirtualMachineScaleSetVMInstanceRequiredIDs, *armcompute.VirtualMachineScaleSetsClientBeginDeleteInstancesOptions) {
			capacity.Add(-1)
		}).Return(nil, nil).Times(1)
		require.NoError(t, group.DeleteNodes(ctx, []*apiv1.Node{newApiNode(armcompute.OrchestrationModeUniform, 1)}))
		synctest.Wait()
		require.Equal(t, int64(1), capacity.Load())

		// An already-minimum pool rejects deletion without another cloud call.
		capacity.Store(0)
		require.NoError(t, manager.forceRefresh())
		scaleSet := group.(*ScaleSet)
		scaleSet.invalidateLastSizeRefreshWithLock()
		require.ErrorContains(t, group.DeleteNodes(ctx, []*apiv1.Node{newApiNode(armcompute.OrchestrationModeUniform, 0)}), "min size reached")
	})
}
