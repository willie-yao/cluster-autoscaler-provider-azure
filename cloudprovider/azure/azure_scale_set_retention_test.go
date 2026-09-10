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
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachineclient/mock_virtualmachineclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetclient/mock_virtualmachinescalesetclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetvmclient/mock_virtualmachinescalesetvmclient"
	providerconfig "sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
	"sigs.k8s.io/cluster-autoscaler/pkg/cloudprovider"
	"sigs.k8s.io/cluster-autoscaler/pkg/config"
	"sigs.k8s.io/cluster-autoscaler/pkg/config/dynamic"
	coreoptions "sigs.k8s.io/cluster-autoscaler/pkg/core/options"
	"sigs.k8s.io/cluster-autoscaler/pkg/observers/nodegroupchange"
	"sigs.k8s.io/cluster-autoscaler/pkg/processors"
	"sigs.k8s.io/cluster-autoscaler/pkg/processors/nodegroupconfig"
)

type retentionPoller[T any] struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	done    bool
	err     error
}

func (p *retentionPoller[T]) Done() bool { return p.done }
func (p *retentionPoller[T]) Poll(ctx context.Context) (*http.Response, error) {
	close(p.entered)
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	p.done = true
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}, nil
}
func (p *retentionPoller[T]) Result(context.Context, *T) error { return p.err }
func (p *retentionPoller[T]) complete()                        { p.once.Do(func() { close(p.release) }) }

func newRetentionPoller[T any](t *testing.T, err error) (*runtime.Poller[T], *retentionPoller[T]) {
	t.Helper()
	handler := &retentionPoller[T]{entered: make(chan struct{}), release: make(chan struct{}), err: err}
	poller, createErr := runtime.NewPoller(&http.Response{StatusCode: http.StatusAccepted, Header: http.Header{}, Body: http.NoBody},
		runtime.NewPipeline("test", "v0", runtime.PipelineOptions{}, nil), &runtime.NewPollerOptions[T]{Handler: handler})
	require.NoError(t, createErr)
	t.Cleanup(handler.complete)
	return poller, handler
}

func awaitRetentionPoll(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted operation did not enter its poller")
	}
}

type retentionVMState struct{ power, provisioning string }

type retentionFixture struct {
	group      *ScaleSet
	client     *MockVMSSDeleteClient
	vmssClient *mock_virtualmachinescalesetclient.MockInterface
	vmClient   *mock_virtualmachinescalesetvmclient.MockInterface
	mutex      sync.Mutex
	vms        map[int]retentionVMState
	listErr    error
	vmss       *armcompute.VirtualMachineScaleSet
	kube       *kubefake.Clientset
}

func retentionTestOptions() *coreoptions.AutoscalerOptions {
	client := kubefake.NewClientset()
	return &coreoptions.AutoscalerOptions{
		KubeClient: client, InformerFactory: informers.NewSharedInformerFactory(client, 0),
		Processors: &processors.AutoscalingProcessors{
			NodeGroupConfigProcessor: nodegroupconfig.NewDefaultNodeGroupConfigProcessor(config.NodeGroupAutoscalingOptions{MaxNodeProvisionTime: 15 * time.Minute}),
			ScaleStateNotifier:       nodegroupchange.NewNodeGroupChangeObserversList(),
		},
	}
}

func newRetentionFixture(t *testing.T, size int) *retentionFixture {
	t.Helper()
	ctrl := gomock.NewController(t)
	f := &retentionFixture{
		client:     NewMockVMSSDeleteClient(ctrl),
		vmssClient: mock_virtualmachinescalesetclient.NewMockInterface(ctrl),
		vmClient:   mock_virtualmachinescalesetvmclient.NewMockInterface(ctrl),
		vms:        make(map[int]retentionVMState),
	}
	for i := 0; i < size; i++ {
		f.vms[i] = retentionVMState{vmPowerStateRunning, provisioningStateSucceeded}
	}
	f.vmss = newTestVMSSList(int64(size), "agents", "eastus", armcompute.OrchestrationModeUniform)[0]
	f.vmss.Properties.VirtualMachineProfile = &armcompute.VirtualMachineScaleSetVMProfile{
		StorageProfile: &armcompute.VirtualMachineScaleSetStorageProfile{
			OSDisk: &armcompute.VirtualMachineScaleSetOSDisk{ManagedDisk: &armcompute.VirtualMachineScaleSetManagedDiskParameters{}},
		},
	}
	f.vmClient.EXPECT().ListVMInstanceView(gomock.Any(), "rg", "agents").DoAndReturn(func(context.Context, string, string) ([]*armcompute.VirtualMachineScaleSetVM, error) {
		f.mutex.Lock()
		defer f.mutex.Unlock()
		if f.listErr != nil {
			return nil, f.listErr
		}
		var result []*armcompute.VirtualMachineScaleSetVM
		for id, state := range f.vms {
			result = append(result, &armcompute.VirtualMachineScaleSetVM{
				ID: ptr.To(fmt.Sprintf(fakeVirtualMachineScaleSetVMID, id)), InstanceID: ptr.To(fmt.Sprint(id)),
				Properties: &armcompute.VirtualMachineScaleSetVMProperties{
					ProvisioningState: ptr.To(state.provisioning),
					InstanceView:      &armcompute.VirtualMachineScaleSetVMInstanceView{Statuses: []*armcompute.InstanceViewStatus{{Code: ptr.To(state.power)}}},
				},
			})
		}
		return result, nil
	}).AnyTimes()
	f.vmssClient.EXPECT().List(gomock.Any(), "rg").Return([]*armcompute.VirtualMachineScaleSet{f.vmss}, nil).AnyTimes()
	virtualMachines := mock_virtualmachineclient.NewMockInterface(ctrl)
	virtualMachines.EXPECT().List(gomock.Any(), "rg").Return([]*armcompute.VirtualMachine{}, nil).AnyTimes()
	manager := &AzureManager{
		autoscalerOptions:    retentionTestOptions(),
		explicitlyConfigured: map[string]bool{"agents": true},
		config: &Config{
			Config:                               providerconfig.Config{ResourceGroup: "rg", VMType: "vmss"},
			NodeGroupScaleDownPolicies:           map[string]string{"AGENTS": scaleDownDeallocate},
			EnableFastDeleteOnFailedProvisioning: true,
		},
		azClient: &azClient{
			vmssClientForDelete: f.client, virtualMachineScaleSetsClient: f.vmssClient,
			virtualMachineScaleSetVMsClient: f.vmClient, virtualMachinesClient: virtualMachines,
		},
	}
	f.kube = manager.autoscalerOptions.KubeClient.(*kubefake.Clientset)
	for i := 0; i < size; i++ {
		node := newApiNode(armcompute.OrchestrationModeUniform, int64(i))
		node.Name = fmt.Sprintf("retention-%d", i)
		node.UID = types.UID(node.Name)
		_, err := f.kube.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	cache, err := newAzureCache(manager.azClient, time.Minute, *manager.config)
	require.NoError(t, err)
	manager.azureCache = cache
	f.group, err = NewScaleSet(&dynamic.NodeGroupSpec{Name: "agents", MinSize: 0, MaxSize: 30}, manager, int64(size), false)
	require.NoError(t, err)
	manager.RegisterNodeGroup(f.group)
	require.NoError(t, f.group.reconcileRetention(context.Background()))
	require.NoError(t, manager.azureCache.regenerate())
	return f
}

func (f *retentionFixture) setVM(id int, power, provisioning string) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.vms[id] = retentionVMState{power, provisioning}
}

func (f *retentionFixture) snapshot(t *testing.T) *retentionSnapshot {
	t.Helper()
	require.NoError(t, f.group.reconcileRetention(context.Background()))
	snapshot, err := f.group.retentionSnapshot(context.Background())
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	return snapshot
}

func (f *retentionFixture) mark(t *testing.T, id int) *apiv1.Node {
	t.Helper()
	node := f.node(t, id)
	updated, handled, err := f.group.MarkToBeDeleted(context.Background(), node, false)
	require.NoError(t, err)
	require.True(t, handled)
	return updated
}

func (f *retentionFixture) stop(t *testing.T, ids ...int) *retentionPoller[armcompute.VirtualMachineScaleSetsClientDeallocateResponse] {
	t.Helper()
	poller, handler := newRetentionPoller[armcompute.VirtualMachineScaleSetsClientDeallocateResponse](t, nil)
	f.client.EXPECT().BeginDeallocate(gomock.Any(), "rg", "agents", gomock.Any()).DoAndReturn(
		func(_ context.Context, _, _ string, options *armcompute.VirtualMachineScaleSetsClientBeginDeallocateOptions) (*runtime.Poller[armcompute.VirtualMachineScaleSetsClientDeallocateResponse], error) {
			require.NotNil(t, options.VMInstanceIDs)
			require.Len(t, options.VMInstanceIDs.InstanceIDs, len(ids))
			require.Nil(t, options.Hibernate)
			for index, id := range ids {
				require.Equal(t, fmt.Sprint(id), *options.VMInstanceIDs.InstanceIDs[index])
			}
			return poller, nil
		})
	nodes := make([]*apiv1.Node, 0, len(ids))
	for _, id := range ids {
		nodes = append(nodes, f.mark(t, id))
	}
	require.NoError(t, f.group.DeleteNodes(context.Background(), nodes))
	awaitRetentionPoll(t, handler.entered)
	return handler
}

func (f *retentionFixture) park(t *testing.T, ids ...int) {
	t.Helper()
	handler := f.stop(t, ids...)
	for _, id := range ids {
		f.setVM(id, vmPowerStateDeallocated, provisioningStateSucceeded)
	}
	handler.complete()
	require.Eventually(t, func() bool {
		f.group.retention.mutex.Lock()
		defer f.group.retention.mutex.Unlock()
		for _, id := range ids {
			if !f.group.retention.instances[strings.ToLower(newApiNode(armcompute.OrchestrationModeUniform, int64(id)).Spec.ProviderID)].stopCompleted {
				return false
			}
		}
		return true
	}, 5*time.Second, time.Millisecond)
	require.NoError(t, f.group.reconcileRetention(context.Background()))
}

func (f *retentionFixture) expectCapacity(t *testing.T, capacity int64, rejection error) {
	t.Helper()
	var poller *runtime.Poller[armcompute.VirtualMachineScaleSetsClientCreateOrUpdateResponse]
	if rejection == nil {
		var handler *retentionPoller[armcompute.VirtualMachineScaleSetsClientCreateOrUpdateResponse]
		poller, handler = newRetentionPoller[armcompute.VirtualMachineScaleSetsClientCreateOrUpdateResponse](t, nil)
		handler.complete()
	}
	f.client.EXPECT().BeginCreateOrUpdate(gomock.Any(), "rg", "agents", gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _, _ string, vmss armcompute.VirtualMachineScaleSet, _ *armcompute.VirtualMachineScaleSetsClientBeginCreateOrUpdateOptions) (*runtime.Poller[armcompute.VirtualMachineScaleSetsClientCreateOrUpdateResponse], error) {
			require.Equal(t, capacity, *vmss.SKU.Capacity)
			return poller, rejection
		})
}

func TestRetentionPhysicalGrowthAfterAcceptedStop(t *testing.T) {
	f := newRetentionFixture(t, 10)
	f.stop(t, 0, 1, 2, 3)
	require.Equal(t, 6, f.snapshot(t).TargetSize)
	f.expectCapacity(t, 11, nil)
	require.NoError(t, f.group.IncreaseSize(context.Background(), 1))
	snapshot := f.snapshot(t)
	require.Equal(t, 7, snapshot.TargetSize)
	require.Len(t, snapshot.Instances, 10)
	for _, instance := range snapshot.Instances[:4] {
		require.Equal(t, retentionRetiring, instance.Phase)
	}
	require.NoError(t, f.group.DecreaseTargetSize(context.Background(), -1))
	require.Equal(t, 7, f.snapshot(t).TargetSize)
}

func TestRetentionRestartFirstPartialResults(t *testing.T) {
	for _, test := range []struct {
		name           string
		startRejection bool
		putRejection   bool
		physical       int64
		target         int
	}{
		{name: "two restarts plus one fresh", physical: 11, target: 11},
		{name: "one accepted restart plus two fresh", startRejection: true, physical: 12, target: 11},
		{name: "accepted restarts survive rejected PUT", putRejection: true, physical: 11, target: 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRetentionFixture(t, 10)
			f.park(t, 0, 1)
			accepted, first := newRetentionPoller[armcompute.VirtualMachineScaleSetsClientStartResponse](t, nil)
			f.client.EXPECT().BeginStart(gomock.Any(), "rg", "agents", gomock.Any()).Return(accepted, nil)
			if test.startRejection {
				f.client.EXPECT().BeginStart(gomock.Any(), "rg", "agents", gomock.Any()).Return(nil, &azcore.ResponseError{StatusCode: 409})
			} else {
				second, _ := newRetentionPoller[armcompute.VirtualMachineScaleSetsClientStartResponse](t, nil)
				f.client.EXPECT().BeginStart(gomock.Any(), "rg", "agents", gomock.Any()).Return(second, nil)
			}
			var rejection error
			if test.putRejection {
				rejection = &azcore.ResponseError{StatusCode: 400}
			}
			f.expectCapacity(t, test.physical, rejection)
			err := f.group.IncreaseSize(context.Background(), 3)
			if test.putRejection {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			awaitRetentionPoll(t, first.entered)
			snapshot := f.snapshot(t)
			require.Equal(t, test.target, snapshot.TargetSize)
			resume := snapshot.Instances[0].Resume
			require.NotNil(t, resume)
			require.NotEmpty(t, resume.ID)
			require.False(t, resume.AcceptedAt.IsZero())
			require.False(t, resume.StartCompleted)
			require.NoError(t, f.group.DecreaseTargetSize(context.Background(), -3))
			require.Equal(t, test.target, f.snapshot(t).TargetSize)
			require.Equal(t, resume, f.snapshot(t).Instances[0].Resume)
		})
	}
}

func TestRetentionRefreshReplacementAndMembership(t *testing.T) {
	f := newRetentionFixture(t, 4)
	f.park(t, 0, 1)
	poller, handler := newRetentionPoller[armcompute.VirtualMachineScaleSetsClientStartResponse](t, nil)
	f.client.EXPECT().BeginStart(gomock.Any(), "rg", "agents", gomock.Any()).Return(poller, nil)
	require.NoError(t, f.group.IncreaseSize(context.Background(), 1))
	awaitRetentionPoll(t, handler.entered)
	before := f.snapshot(t)
	replacement, err := NewScaleSet(&dynamic.NodeGroupSpec{Name: "agents", MinSize: 0, MaxSize: 31}, f.group.manager, 4, false)
	require.NoError(t, err)
	require.Same(t, f.group.retention, replacement.retention)
	require.True(t, f.group.manager.RegisterNodeGroup(replacement))
	f.group = replacement
	require.NoError(t, f.group.manager.azureCache.regenerate())
	require.Equal(t, before, f.snapshot(t))
	provider := &AzureCloudProvider{azureManager: f.group.manager}
	for _, id := range []int64{0, 1} {
		node := newApiNode(armcompute.OrchestrationModeUniform, id)
		group, err := provider.NodeGroupForNode(context.Background(), node)
		require.NoError(t, err)
		require.Equal(t, f.group, group)
		exists, err := provider.HasInstance(context.Background(), node)
		require.NoError(t, err)
		require.True(t, exists)
	}
	handler.complete()
	require.Eventually(t, func() bool { return f.snapshot(t).Instances[0].Resume.StartCompleted }, 5*time.Second, time.Millisecond)
	require.Equal(t, before.Instances[0].Resume.ID, f.snapshot(t).Instances[0].Resume.ID)
}

func TestRetentionUniqueBatchAndMinimum(t *testing.T) {
	f := newRetentionFixture(t, 4)
	f.group.minSize = 3
	first, second := f.mark(t, 0), f.mark(t, 1)
	require.Error(t, f.group.DeleteNodes(context.Background(), []*apiv1.Node{first, second}))
	require.Error(t, f.group.DeleteNodes(context.Background(), nil))
	poller, handler := newRetentionPoller[armcompute.VirtualMachineScaleSetsClientDeallocateResponse](t, nil)
	f.client.EXPECT().BeginDeallocate(gomock.Any(), "rg", "agents", gomock.Any()).DoAndReturn(
		func(_ context.Context, _, _ string, options *armcompute.VirtualMachineScaleSetsClientBeginDeallocateOptions) (*runtime.Poller[armcompute.VirtualMachineScaleSetsClientDeallocateResponse], error) {
			require.Len(t, options.VMInstanceIDs.InstanceIDs, 1)
			return poller, nil
		})
	require.NoError(t, f.group.DeleteNodes(context.Background(), []*apiv1.Node{first, first.DeepCopy()}))
	awaitRetentionPoll(t, handler.entered)
	require.Equal(t, 3, f.snapshot(t).TargetSize)
	require.NoError(t, f.group.DeleteNodes(context.Background(), []*apiv1.Node{first}))
	require.Error(t, f.group.DeleteNodes(context.Background(), []*apiv1.Node{second}))
}

func TestRetentionUnknownSubmissionQuarantines(t *testing.T) {
	f := newRetentionFixture(t, 4)
	f.park(t, 0)
	f.client.EXPECT().BeginStart(gomock.Any(), "rg", "agents", gomock.Any()).Return(nil, errors.New("connection lost after submission"))
	require.ErrorContains(t, f.group.IncreaseSize(context.Background(), 1), "unresolved outcome")
	_, err := f.group.retentionSnapshot(context.Background())
	require.Error(t, err)
	_, err = f.group.TargetSize(context.Background())
	require.Error(t, err)
	require.Error(t, f.group.IncreaseSize(context.Background(), 1))
	require.Error(t, f.group.DecreaseTargetSize(context.Background(), -1))
	require.Error(t, f.group.ForceDeleteNodes(context.Background(), []*apiv1.Node{newApiNode(armcompute.OrchestrationModeUniform, 1)}))
}

func TestRetentionResumeFailureIsNotFailedCreation(t *testing.T) {
	f := newRetentionFixture(t, 4)
	f.park(t, 0)
	poller, handler := newRetentionPoller[armcompute.VirtualMachineScaleSetsClientStartResponse](t, &azcore.ResponseError{StatusCode: 400, ErrorCode: "AllocationFailed"})
	f.client.EXPECT().BeginStart(gomock.Any(), "rg", "agents", gomock.Any()).Return(poller, nil)
	require.NoError(t, f.group.IncreaseSize(context.Background(), 1))
	handler.complete()
	require.Eventually(t, func() bool { return f.snapshot(t).Instances[0].Resume.Error != nil }, 5*time.Second, time.Millisecond)
	snapshot := f.snapshot(t)
	require.Equal(t, retentionResuming, snapshot.Instances[0].Phase)
	require.Equal(t, cloudprovider.InstanceRunning, snapshot.Instances[0].Status.State)
	require.Nil(t, snapshot.Instances[0].Status.ErrorInfo)
	node := newApiNode(armcompute.OrchestrationModeUniform, 0)
	node.Annotations = map[string]string{cloudprovider.FakeNodeReasonAnnotation: cloudprovider.FakeNodeUnregistered}
	require.Error(t, f.group.ForceDeleteNodes(context.Background(), []*apiv1.Node{node}))
}

func TestRetentionFreshFailureUsesDeleteNotStop(t *testing.T) {
	f := newRetentionFixture(t, 2)
	f.expectCapacity(t, 3, nil)
	require.NoError(t, f.group.IncreaseSize(context.Background(), 1))
	f.setVM(2, vmPowerStateUnknown, VMProvisioningStateFailed)
	require.NoError(t, f.group.reconcileRetention(context.Background()))
	require.Equal(t, cloudprovider.InstanceCreating, f.snapshot(t).Instances[2].Status.State)
	poller, handler := newRetentionPoller[armcompute.VirtualMachineScaleSetsClientDeleteInstancesResponse](t, nil)
	f.client.EXPECT().BeginDeleteInstances(gomock.Any(), "rg", "agents", gomock.Any(), gomock.Any()).Return(poller, nil)
	node := newApiNode(armcompute.OrchestrationModeUniform, 2)
	node.Annotations = map[string]string{cloudprovider.FakeNodeReasonAnnotation: cloudprovider.FakeNodeUnregistered}
	require.NoError(t, f.group.ForceDeleteNodes(context.Background(), []*apiv1.Node{node}))
	awaitRetentionPoll(t, handler.entered)
	require.Equal(t, 2, f.snapshot(t).TargetSize)
}

func TestRetentionColdStartAndRefreshFailures(t *testing.T) {
	f := newRetentionFixture(t, 4)
	f.park(t, 0)
	f.group.retention = &scaleSetRetention{instances: make(map[string]*retentionMember), capacity: make(map[string]retentionCapacity)}
	require.ErrorContains(t, f.group.reconcileRetention(context.Background()), "no accepted retention intent")
	_, err := f.group.TargetSize(context.Background())
	require.ErrorContains(t, err, "no accepted retention intent")
	require.Error(t, f.group.IncreaseSize(context.Background(), 1))
	require.Error(t, f.group.DecreaseTargetSize(context.Background(), -1))
	require.Error(t, f.group.DeleteNodes(context.Background(), []*apiv1.Node{newApiNode(armcompute.OrchestrationModeUniform, 1)}))
}

func TestRetentionInventoryErrorsDoNotReturnCachedSuccess(t *testing.T) {
	f := newRetentionFixture(t, 4)
	f.park(t, 0)
	f.mutex.Lock()
	f.listErr = &azcore.ResponseError{StatusCode: http.StatusTooManyRequests}
	f.mutex.Unlock()
	require.Error(t, f.group.reconcileRetention(context.Background()))
	_, err := f.group.TargetSize(context.Background())
	require.Error(t, err)
	_, err = f.group.retentionSnapshot(context.Background())
	require.Error(t, err)
	require.Error(t, f.group.IncreaseSize(context.Background(), 1))
}

func TestRetentionRejectsAtomicAndZeroOrMax(t *testing.T) {
	f := newRetentionFixture(t, 4)
	require.Error(t, f.group.AtomicIncreaseSize(context.Background(), 1))
	_, err := f.group.GetOptions(context.Background(), config.NodeGroupAutoscalingOptions{ZeroOrMaxNodeScaling: true})
	require.Error(t, err)
	require.Error(t, f.group.IncreaseSize(context.Background(), 1))
}

func TestRetentionSnapshotCopiesAttempt(t *testing.T) {
	f := newRetentionFixture(t, 4)
	f.park(t, 0)
	poller, _ := newRetentionPoller[armcompute.VirtualMachineScaleSetsClientStartResponse](t, nil)
	f.client.EXPECT().BeginStart(gomock.Any(), "rg", "agents", gomock.Any()).Return(poller, nil)
	require.NoError(t, f.group.IncreaseSize(context.Background(), 1))
	snapshot := f.snapshot(t)
	snapshot.Instances[0].Resume.StartCompleted = true
	snapshot.Instances[0].Status.State = cloudprovider.InstanceDeleting
	assert.False(t, f.snapshot(t).Instances[0].Resume.StartCompleted)
	assert.Equal(t, cloudprovider.InstanceRunning, f.snapshot(t).Instances[0].Status.State)
}

func TestRetentionStaleStopWaiterCannotOverwriteResume(t *testing.T) {
	f := newRetentionFixture(t, 4)
	f.park(t, 0)
	key := strings.ToLower(newApiNode(armcompute.OrchestrationModeUniform, 0).Spec.ProviderID)
	f.group.retention.mutex.Lock()
	oldOperation := f.group.retention.instances[key].operation
	f.group.retention.mutex.Unlock()
	poller, _ := newRetentionPoller[armcompute.VirtualMachineScaleSetsClientStartResponse](t, nil)
	f.client.EXPECT().BeginStart(gomock.Any(), "rg", "agents", gomock.Any()).Return(poller, nil)
	require.NoError(t, f.group.IncreaseSize(context.Background(), 1))
	before := f.snapshot(t)
	stale, handler := newRetentionPoller[armcompute.VirtualMachineScaleSetsClientDeallocateResponse](t, errors.New("obsolete operation failed"))
	handler.complete()
	f.group.waitForRetentionStop(stale, []string{key}, oldOperation)
	require.Equal(t, before, f.snapshot(t))
}

func TestRetentionCompletedResumeCanBeRetiredAgain(t *testing.T) {
	f := newRetentionFixture(t, 4)
	f.park(t, 0)
	poller, handler := newRetentionPoller[armcompute.VirtualMachineScaleSetsClientStartResponse](t, nil)
	f.client.EXPECT().BeginStart(gomock.Any(), "rg", "agents", gomock.Any()).Return(poller, nil)
	require.NoError(t, f.group.IncreaseSize(context.Background(), 1))
	f.setVM(0, vmPowerStateRunning, provisioningStateSucceeded)
	handler.complete()
	require.Eventually(t, func() bool { return f.snapshot(t).Instances[0].Resume.StartCompleted }, 5*time.Second, time.Millisecond)
	node, err := f.kube.CoreV1().Nodes().Get(context.Background(), "retention-0", metav1.GetOptions{})
	require.NoError(t, err)
	node.Status.Conditions = append(node.Status.Conditions, apiv1.NodeCondition{Type: apiv1.NodeReady, Status: apiv1.ConditionTrue, LastHeartbeatTime: metav1.Now()})
	_, err = f.kube.CoreV1().Nodes().UpdateStatus(context.Background(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, f.group.reconcileRetention(context.Background()))
	f.stop(t, 0)
	require.Equal(t, 3, f.snapshot(t).TargetSize)
	require.Nil(t, f.snapshot(t).Instances[0].Resume)
}

func TestRetentionRejectedStopDoesNotReserveTarget(t *testing.T) {
	f := newRetentionFixture(t, 4)
	node := f.mark(t, 0)
	f.client.EXPECT().BeginDeallocate(gomock.Any(), "rg", "agents", gomock.Any()).Return(nil, &azcore.ResponseError{StatusCode: 400})
	require.Error(t, f.group.DeleteNodes(context.Background(), []*apiv1.Node{node}))
	require.Equal(t, 4, f.snapshot(t).TargetSize)
	require.Equal(t, retentionActive, f.snapshot(t).Instances[0].Phase)
}

func TestRetentionMixedExpiryPreservesCapacityCorrelation(t *testing.T) {
	f := newRetentionFixture(t, 4)
	f.park(t, 0, 1)
	first, firstHandler := newRetentionPoller[armcompute.VirtualMachineScaleSetsClientStartResponse](t, nil)
	second, secondHandler := newRetentionPoller[armcompute.VirtualMachineScaleSetsClientStartResponse](t, nil)
	f.client.EXPECT().BeginStart(gomock.Any(), "rg", "agents", gomock.Any()).Return(first, nil)
	f.client.EXPECT().BeginStart(gomock.Any(), "rg", "agents", gomock.Any()).Return(second, nil)
	capacity, capacityHandler := newRetentionPoller[armcompute.VirtualMachineScaleSetsClientCreateOrUpdateResponse](t, nil)
	f.client.EXPECT().BeginCreateOrUpdate(gomock.Any(), "rg", "agents", gomock.Any(), gomock.Any()).Return(capacity, nil)
	require.NoError(t, f.group.IncreaseSize(context.Background(), 3))
	awaitRetentionPoll(t, firstHandler.entered)
	awaitRetentionPoll(t, secondHandler.entered)
	awaitRetentionPoll(t, capacityHandler.entered)
	before := f.snapshot(t)
	require.Equal(t, 5, before.TargetSize)
	firstHandler.complete()
	f.setVM(0, vmPowerStateRunning, provisioningStateSucceeded)
	require.Eventually(t, func() bool { return f.snapshot(t).Instances[0].Resume.StartCompleted }, 5*time.Second, time.Millisecond)
	require.NoError(t, f.group.DecreaseTargetSize(context.Background(), -3))
	f.group.retention.mutex.Lock()
	require.Len(t, f.group.retention.capacity, 1)
	for _, accepted := range f.group.retention.capacity {
		require.EqualValues(t, 1, accepted.increment)
		require.EqualValues(t, 1, accepted.unassigned)
		require.False(t, accepted.acceptedAt.IsZero())
		require.False(t, accepted.completed)
	}
	f.group.retention.mutex.Unlock()
	after := f.snapshot(t)
	require.Equal(t, before.TargetSize, after.TargetSize)
	for id := 0; id < 2; id++ {
		require.Equal(t, before.Instances[id].Resume.ID, after.Instances[id].Resume.ID)
		require.Equal(t, before.Instances[id].Resume.AcceptedAt, after.Instances[id].Resume.AcceptedAt)
	}
}

func TestRetentionETagRetryCannotShrinkFreshPhysicalCapacity(t *testing.T) {
	f := newRetentionFixture(t, 10)
	f.stop(t, 0, 1, 2, 3)
	f.group.manager.config.EnableVMSSEtag = true
	f.vmss.Etag = ptr.To("old")
	f.client.EXPECT().BeginCreateOrUpdate(gomock.Any(), "rg", "agents", gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _, _ string, update armcompute.VirtualMachineScaleSet, options *armcompute.VirtualMachineScaleSetsClientBeginCreateOrUpdateOptions) (*runtime.Poller[armcompute.VirtualMachineScaleSetsClientCreateOrUpdateResponse], error) {
			require.EqualValues(t, 11, *update.SKU.Capacity)
			require.Equal(t, "old", *options.IfMatch)
			return nil, &azcore.ResponseError{StatusCode: http.StatusPreconditionFailed}
		})
	fresh := *f.vmss
	fresh.SKU = &armcompute.SKU{Name: f.vmss.SKU.Name, Capacity: ptr.To[int64](12)}
	fresh.Etag = ptr.To("new")
	f.vmssClient.EXPECT().Get(gomock.Any(), "rg", "agents", nil).Return(&fresh, nil)
	require.NoError(t, f.group.IncreaseSize(context.Background(), 1))
	require.Equal(t, 8, f.snapshot(t).TargetSize)
	require.NoError(t, f.group.DecreaseTargetSize(context.Background(), -1))
	require.Equal(t, 8, f.snapshot(t).TargetSize)
}

func TestRetentionStaleCapacityWaiterCannotUndoNewerCompletion(t *testing.T) {
	f := newRetentionFixture(t, 4)
	first, firstHandler := newRetentionPoller[armcompute.VirtualMachineScaleSetsClientCreateOrUpdateResponse](t, errors.New("superseded"))
	second, secondHandler := newRetentionPoller[armcompute.VirtualMachineScaleSetsClientCreateOrUpdateResponse](t, nil)
	f.client.EXPECT().BeginCreateOrUpdate(gomock.Any(), "rg", "agents", gomock.Any(), gomock.Any()).Return(first, nil)
	require.NoError(t, f.group.IncreaseSize(context.Background(), 1))
	awaitRetentionPoll(t, firstHandler.entered)
	f.client.EXPECT().BeginCreateOrUpdate(gomock.Any(), "rg", "agents", gomock.Any(), gomock.Any()).Return(second, nil)
	require.NoError(t, f.group.IncreaseSize(context.Background(), 1))
	secondHandler.complete()
	require.Eventually(t, func() bool {
		f.group.retention.mutex.Lock()
		defer f.group.retention.mutex.Unlock()
		return f.group.retention.capacityCompletedThrough == 2
	}, 5*time.Second, time.Millisecond)
	firstHandler.complete()
	require.Eventually(t, func() bool {
		f.group.retention.mutex.Lock()
		defer f.group.retention.mutex.Unlock()
		for _, accepted := range f.group.retention.capacity {
			if !accepted.completed {
				return false
			}
		}
		return true
	}, 5*time.Second, time.Millisecond)
	require.Equal(t, 6, f.snapshot(t).TargetSize)
}

func TestRetentionUnsupportedRefreshBlocksExistingGroup(t *testing.T) {
	f := newRetentionFixture(t, 4)
	f.vmss.Properties.VirtualMachineProfile.Priority = ptr.To(armcompute.VirtualMachinePriorityTypesSpot)
	require.Error(t, f.group.manager.forceRefresh())
	require.Len(t, f.group.manager.getNodeGroups(), 1)
	_, err := f.group.TargetSize(context.Background())
	require.Error(t, err)
	require.Error(t, f.group.IncreaseSize(context.Background(), 1))
	require.Error(t, f.group.DeleteNodes(context.Background(), []*apiv1.Node{newApiNode(armcompute.OrchestrationModeUniform, 0)}))
	f.vmss.Properties.VirtualMachineProfile.Priority = ptr.To(armcompute.VirtualMachinePriorityTypesRegular)
	require.NoError(t, f.group.manager.forceRefresh())
	require.Equal(t, 4, f.snapshot(t).TargetSize)
}

func TestRetentionPolicyRemovalCannotReinterpretRetainedInventory(t *testing.T) {
	f := newRetentionFixture(t, 4)
	f.park(t, 0)
	f.group.manager.config.NodeGroupScaleDownPolicies = nil
	_, err := f.group.TargetSize(context.Background())
	require.Error(t, err)
	_, err = NewScaleSet(&dynamic.NodeGroupSpec{Name: "agents", MinSize: 0, MaxSize: 30}, f.group.manager, 4, false)
	require.Error(t, err)
	require.Error(t, f.group.ForceDeleteNodes(context.Background(), []*apiv1.Node{newApiNode(armcompute.OrchestrationModeUniform, 0)}))
}

func TestRetentionColdStartCannotAdoptUnresolvedCapacity(t *testing.T) {
	f := newRetentionFixture(t, 4)
	f.mutex.Lock()
	delete(f.vms, 3)
	f.mutex.Unlock()
	f.group.retention = &scaleSetRetention{instances: make(map[string]*retentionMember), capacity: make(map[string]retentionCapacity)}
	require.ErrorContains(t, f.group.reconcileRetention(context.Background()), "unresolved capacity")
	_, err := f.group.TargetSize(context.Background())
	require.ErrorContains(t, err, "unresolved capacity")
	require.Error(t, f.group.IncreaseSize(context.Background(), 1))
}
