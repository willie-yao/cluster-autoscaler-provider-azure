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
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/cluster-autoscaler/pkg/cloudprovider"
	"sigs.k8s.io/cluster-autoscaler/pkg/config/dynamic"
	coreoptions "sigs.k8s.io/cluster-autoscaler/pkg/core/options"
	"sigs.k8s.io/cluster-autoscaler/pkg/processors"
	"sigs.k8s.io/cluster-autoscaler/pkg/test/integration"
	"sigs.k8s.io/cluster-autoscaler/pkg/test/integration/clusterstateregistry"
	testutils "sigs.k8s.io/cluster-autoscaler/pkg/utils/test"
)

func TestAzureClusterStateRegistryRetention(t *testing.T) {
	clusterstateregistry.RunTestClusterStateRegistryRetention(t, func(t *testing.T, ctx context.Context, args clusterstateregistry.RetentionSetupArgs) clusterstateregistry.RetentionSetup {
		f := newRetentionFixture(t, args.NodeCount)
		infra := integration.SetupInfrastructure(t)
		opts := integration.NewTestConfig().WithOverrides(args.OptsOverride).ResolveOptions()
		f.group.manager.autoscalerOptions = &coreoptions.AutoscalerOptions{
			AutoscalingOptions: opts,
			KubeClient:         infra.Fakes.KubeClient, InformerFactory: infra.Fakes.InformerFactory,
			Processors: processors.DefaultProcessors(opts),
		}
		template, err := f.group.TemplateNodeInfo(ctx)
		require.NoError(t, err)
		for id := 0; id < args.NodeCount; id++ {
			node := template.Node().DeepCopy()
			node.Name = fmt.Sprintf("retention-%d", id)
			node.UID = types.UID(node.Name)
			node.Spec.ProviderID = newApiNode(armcompute.OrchestrationModeUniform, int64(id)).Spec.ProviderID
			testutils.SetNodeReadyState(node, true, time.Now().Add(-time.Hour))
			for i := range node.Status.Conditions {
				if node.Status.Conditions[i].Type == apiv1.NodeReady {
					node.Status.Conditions[i].LastHeartbeatTime = metav1.NewTime(time.Now().Add(-time.Hour))
				}
			}
			infra.Fakes.K8s.AddNode(node)
		}
		require.NoError(t, f.group.reconcileRetention(ctx))
		f.client.EXPECT().BeginDeallocate(gomock.Any(), "rg", "agents", gomock.Any()).DoAndReturn(
			func(_ context.Context, _, _ string, options *armcompute.VirtualMachineScaleSetsClientBeginDeallocateOptions) (*runtime.Poller[armcompute.VirtualMachineScaleSetsClientDeallocateResponse], error) {
				for _, id := range options.VMInstanceIDs.InstanceIDs {
					number, err := strconv.Atoi(*id)
					require.NoError(t, err)
					f.setVM(number, vmPowerStateDeallocated, provisioningStateSucceeded)
				}
				poller, handler := newRetentionPoller[armcompute.VirtualMachineScaleSetsClientDeallocateResponse](t, nil)
				handler.complete()
				return poller, nil
			}).AnyTimes()
		var mutex sync.Mutex
		starts := make(map[int]*retentionPoller[armcompute.VirtualMachineScaleSetsClientStartResponse])
		f.client.EXPECT().BeginStart(gomock.Any(), "rg", "agents", gomock.Any()).DoAndReturn(
			func(_ context.Context, _, _ string, options *armcompute.VirtualMachineScaleSetsClientBeginStartOptions) (*runtime.Poller[armcompute.VirtualMachineScaleSetsClientStartResponse], error) {
				require.Len(t, options.VMInstanceIDs.InstanceIDs, 1)
				number, err := strconv.Atoi(*options.VMInstanceIDs.InstanceIDs[0])
				require.NoError(t, err)
				poller, handler := newRetentionPoller[armcompute.VirtualMachineScaleSetsClientStartResponse](t, nil)
				mutex.Lock()
				require.Nil(t, starts[number], "duplicate Start for one accepted activation")
				starts[number] = handler
				mutex.Unlock()
				return poller, nil
			}).AnyTimes()
		provider := &AzureCloudProvider{azureManager: f.group.manager,
			resourceLimiter: cloudprovider.NewResourceLimiter(map[string]int64{}, map[string]int64{})}
		autoscaler, _, err := integration.DefaultAutoscalingBuilder(opts, infra).WithCloudProvider(provider).Build(ctx)
		require.NoError(t, err)
		return clusterstateregistry.RetentionSetup{
			Autoscaler: autoscaler, NodeGroup: f.group, K8s: infra.Fakes.K8s,
			CompleteResumes: func() error {
				mutex.Lock()
				defer mutex.Unlock()
				for id, handler := range starts {
					f.setVM(id, vmPowerStateRunning, provisioningStateSucceeded)
					handler.complete()
				}
				return nil
			},
			Refresh: func() error {
				replacement, err := NewScaleSet(&dynamic.NodeGroupSpec{Name: "agents", MinSize: 0, MaxSize: f.group.maxSize + 1}, f.group.manager, int64(args.NodeCount), false)
				if err != nil {
					return err
				}
				f.group.manager.RegisterNodeGroup(replacement)
				f.group = replacement
				return provider.Refresh(ctx)
			},
		}
	})
}
