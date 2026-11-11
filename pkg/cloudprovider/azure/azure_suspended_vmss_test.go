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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	azruntime "github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachineclient/mock_virtualmachineclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetclient/mock_virtualmachinescalesetclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetvmclient/mock_virtualmachinescalesetvmclient"
	providerconfig "sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
	"sigs.k8s.io/cluster-autoscaler/pkg/cloudprovider"
	"sigs.k8s.io/cluster-autoscaler/pkg/config"
	"sigs.k8s.io/cluster-autoscaler/pkg/config/dynamic"
	"sigs.k8s.io/cluster-autoscaler/pkg/processors/nodegroupconfig"
	catest "sigs.k8s.io/cluster-autoscaler/pkg/utils/test"
)

const suspendedVMSSID = "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/pool"

type suspendedWorld struct {
	mu           sync.Mutex
	states       []string
	identities   map[int]string
	provisioning map[int]string
	deleted      map[int]bool
	events       []string
	startError   map[int]error
	parkError    map[int]error
	nilStart     bool
	nilPark      bool
	startResult  func(int) error
	parkResult   func(int) error
	inventoryErr error
}

func (w *suspendedWorld) history() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.events)
}

func (w *suspendedWorld) vmss() *armcompute.VirtualMachineScaleSet {
	w.mu.Lock()
	defer w.mu.Unlock()
	set := newTestVMSSList(int64(len(w.states)-len(w.deleted)), "pool", "eastus", armcompute.OrchestrationModeUniform)[0]
	set.ID = ptr.To(suspendedVMSSID)
	set.Tags = map[string]*string{nodeLabelTagName + "pool": ptr.To("workers")}
	set.Properties.VirtualMachineProfile = &armcompute.VirtualMachineScaleSetVMProfile{
		StorageProfile: &armcompute.VirtualMachineScaleSetStorageProfile{
			OSDisk: &armcompute.VirtualMachineScaleSetOSDisk{
				ManagedDisk: &armcompute.VirtualMachineScaleSetManagedDiskParameters{},
			},
		},
	}
	return set
}

func (w *suspendedWorld) vms() ([]*armcompute.VirtualMachineScaleSetVM, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	vms := make([]*armcompute.VirtualMachineScaleSetVM, 0, len(w.states))
	for i, state := range w.states {
		if w.deleted[i] {
			continue
		}
		identity := fmt.Sprintf("vm-%d", i)
		if id, ok := w.identities[i]; ok {
			identity = id
		}
		provisioning := provisioningStateSucceeded
		if p, ok := w.provisioning[i]; ok {
			provisioning = p
		}
		vm := &armcompute.VirtualMachineScaleSetVM{
			ID:         ptr.To(fmt.Sprintf("%s/virtualMachines/%d", suspendedVMSSID, i)),
			InstanceID: ptr.To(strconv.Itoa(i)),
			Properties: &armcompute.VirtualMachineScaleSetVMProperties{
				VMID: ptr.To(identity), ProvisioningState: ptr.To(provisioning),
			},
		}
		if state != "" {
			vm.Properties.InstanceView = &armcompute.VirtualMachineScaleSetVMInstanceView{
				Statuses: []*armcompute.InstanceViewStatus{{Code: ptr.To(state)}},
			}
		}
		vms = append(vms, vm)
	}
	return vms, w.inventoryErr
}

type suspendedTestPollingHandler[T any] struct {
	done     bool
	complete func() error
}

func (h *suspendedTestPollingHandler[T]) Done() bool { return h.done }
func (h *suspendedTestPollingHandler[T]) Poll(context.Context) (*http.Response, error) {
	h.done = true
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}, nil
}
func (h *suspendedTestPollingHandler[T]) Result(context.Context, *T) error {
	return h.complete()
}

func suspendedTestPoller[T any](complete func() error) (*azruntime.Poller[T], error) {
	return azruntime.NewPoller(
		&http.Response{StatusCode: http.StatusAccepted, Header: http.Header{}, Body: http.NoBody},
		azruntime.NewPipeline("test", "v0.0.0", azruntime.PipelineOptions{}, nil),
		&azruntime.NewPollerOptions[T]{Handler: &suspendedTestPollingHandler[T]{complete: complete}},
	)
}

func (w *suspendedWorld) BeginStart(
	_ context.Context, _, _, id string,
	_ *armcompute.VirtualMachineScaleSetVMsClientBeginStartOptions,
) (*azruntime.Poller[armcompute.VirtualMachineScaleSetVMsClientStartResponse], error) {
	i, err := strconv.Atoi(id)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.events = append(w.events, "start:"+id)
	err, nilPoller := w.startError[i], w.nilStart
	w.mu.Unlock()
	if err != nil || nilPoller {
		return nil, err
	}
	return suspendedTestPoller[armcompute.VirtualMachineScaleSetVMsClientStartResponse](func() error {
		if w.startResult != nil {
			return w.startResult(i)
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		w.states[i] = vmPowerStateRunning
		return nil
	})
}

func (w *suspendedWorld) BeginDeallocate(
	_ context.Context, _, _, id string,
	_ *armcompute.VirtualMachineScaleSetVMsClientBeginDeallocateOptions,
) (*azruntime.Poller[armcompute.VirtualMachineScaleSetVMsClientDeallocateResponse], error) {
	i, err := strconv.Atoi(id)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.events = append(w.events, "deallocate:"+id)
	err, nilPoller := w.parkError[i], w.nilPark
	w.mu.Unlock()
	if err != nil || nilPoller {
		return nil, err
	}
	return suspendedTestPoller[armcompute.VirtualMachineScaleSetVMsClientDeallocateResponse](func() error {
		if w.parkResult != nil {
			return w.parkResult(i)
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		w.states[i] = vmPowerStateDeallocated
		return nil
	})
}

func newSuspendedProvider(
	t *testing.T, world *suspendedWorld, client *fake.Clientset, minimum, maximum int,
) (*AzureCloudProvider, *ScaleSet) {
	t.Helper()
	ctrl := gomock.NewController(t)
	vmssClient := mock_virtualmachinescalesetclient.NewMockInterface(ctrl)
	vmssClient.EXPECT().List(gomock.Any(), "rg").DoAndReturn(
		func(context.Context, string) ([]*armcompute.VirtualMachineScaleSet, error) {
			return []*armcompute.VirtualMachineScaleSet{world.vmss()}, nil
		}).AnyTimes()
	vmClient := mock_virtualmachineclient.NewMockInterface(ctrl)
	vmClient.EXPECT().List(gomock.Any(), "rg").Return([]*armcompute.VirtualMachine{}, nil).AnyTimes()
	instanceClient := mock_virtualmachinescalesetvmclient.NewMockInterface(ctrl)
	instanceClient.EXPECT().ListVMInstanceView(gomock.Any(), "rg", gomock.Any()).DoAndReturn(
		func(_ context.Context, _ string, name string) ([]*armcompute.VirtualMachineScaleSetVM, error) {
			if !strings.EqualFold(name, "pool") {
				return nil, fmt.Errorf("unexpected VMSS %s", name)
			}
			return world.vms()
		}).AnyTimes()
	resizeClient := NewMockVMSSDeleteClient(ctrl)
	resizeClient.EXPECT().BeginCreateOrUpdate(gomock.Any(), "rg", "pool", gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _, _ string, vmss armcompute.VirtualMachineScaleSet,
			_ *armcompute.VirtualMachineScaleSetsClientBeginCreateOrUpdateOptions,
		) (*azruntime.Poller[armcompute.VirtualMachineScaleSetsClientCreateOrUpdateResponse], error) {
			world.mu.Lock()
			world.events = append(world.events, fmt.Sprintf("grow:%d", *vmss.SKU.Capacity))
			for len(world.states)-len(world.deleted) < int(*vmss.SKU.Capacity) {
				world.states = append(world.states, vmPowerStateStarting)
			}
			world.mu.Unlock()
			return suspendedTestPoller[armcompute.VirtualMachineScaleSetsClientCreateOrUpdateResponse](func() error { return nil })
		}).AnyTimes()
	resizeClient.EXPECT().BeginDeleteInstances(gomock.Any(), "rg", "pool", gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _, _ string, ids armcompute.VirtualMachineScaleSetVMInstanceRequiredIDs,
			_ *armcompute.VirtualMachineScaleSetsClientBeginDeleteInstancesOptions,
		) (*azruntime.Poller[armcompute.VirtualMachineScaleSetsClientDeleteInstancesResponse], error) {
			world.mu.Lock()
			defer world.mu.Unlock()
			if world.deleted == nil {
				world.deleted = make(map[int]bool)
			}
			for _, id := range ids.InstanceIDs {
				i, err := strconv.Atoi(*id)
				if err != nil {
					return nil, err
				}
				world.events = append(world.events, "physical-delete:"+*id)
				world.deleted[i] = true
			}
			return nil, nil
		}).AnyTimes()
	manager := &AzureManager{
		config: &Config{
			Config:     providerconfig.Config{VMType: "vmss", ResourceGroup: "rg", Location: "eastus"},
			Deallocate: true,
		},
		env: azure.PublicCloud, kubeClient: client, cordonNodeBeforeTerminate: true,
		explicitlyConfigured: map[string]bool{"pool": true},
		azClient: &azClient{
			virtualMachineScaleSetsClient: vmssClient, virtualMachineScaleSetVMsClient: instanceClient,
			virtualMachinesClient: vmClient, vmssClientForDelete: resizeClient, vmssPowerClient: world,
		},
	}
	var err error
	manager.azureCache, err = newAzureCache(manager.azClient, time.Minute, *manager.config)
	require.NoError(t, err)
	t.Cleanup(manager.Cleanup)
	require.NoError(t, manager.forceRefresh())
	group, err := NewScaleSet(&dynamic.NodeGroupSpec{Name: "pool", MinSize: minimum, MaxSize: maximum},
		manager, *world.vmss().SKU.Capacity, false)
	require.NoError(t, err)
	manager.RegisterNodeGroup(group)
	require.NoError(t, manager.forceRefresh())
	provider, err := BuildAzureCloudProvider(manager, cloudprovider.NewResourceLimiter(map[string]int64{}, map[string]int64{}))
	require.NoError(t, err)
	return provider.(*AzureCloudProvider), group
}

func suspendedTestNode(id int, suspended bool) *apiv1.Node {
	node := catest.BuildTestNode(fmt.Sprintf("node-%d", id), 4000, 8*1024*1024*1024, catest.IsReady(true))
	node.UID = types.UID(fmt.Sprintf("old-%d", id))
	node.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	node.Spec.ProviderID = fmt.Sprintf("%s%s/virtualMachines/%d", azurePrefix, suspendedVMSSID, id)
	node.Spec.PodCIDR = fmt.Sprintf("10.244.%d.0/24", id)
	node.Labels = map[string]string{
		"pool": "workers", apiv1.LabelHostname: node.Name,
		apiv1.LabelOSStable: "linux", apiv1.LabelArchStable: "amd64",
		apiv1.LabelInstanceTypeStable: "Standard_D4_v2", apiv1.LabelTopologyRegion: "eastus",
	}
	if suspended {
		setSuspendedCondition(node, apiv1.ConditionTrue, parkedReason, metav1.NewTime(time.Now().Add(-time.Hour)))
		node.Annotations = map[string]string{scaleDownDisabledAnnotation: "true"}
		node.Spec.Taints = []apiv1.Taint{{Key: suspendedTaintKey, Effect: apiv1.TaintEffectNoSchedule}}
	}
	return node
}

func nodeSuspended(node *apiv1.Node) *apiv1.NodeCondition {
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == suspendedCondition {
			return &node.Status.Conditions[i]
		}
	}
	return nil
}

func TestSuspendedInventoryPowerStates(t *testing.T) {
	for _, tc := range []struct {
		name, power, provisioning string
		wantError                 bool
		wantState                 cloudprovider.InstanceState
		wantFailure               bool
	}{
		{name: "parked", power: vmPowerStateDeallocated, wantState: cloudprovider.InstanceRunning},
		{name: "running", power: vmPowerStateRunning, wantState: cloudprovider.InstanceRunning},
		{name: "missing", wantError: true},
		{name: "unknown", power: vmPowerStateUnknown, wantError: true},
		{name: "starting", power: vmPowerStateStarting, wantState: cloudprovider.InstanceCreating},
		{name: "deallocating", power: vmPowerStateDeallocating, wantState: cloudprovider.InstanceCreating},
		{name: "failed missing power", provisioning: VMProvisioningStateFailed, wantState: cloudprovider.InstanceCreating},
		{name: "failed transitional", power: vmPowerStateStarting, provisioning: VMProvisioningStateFailed, wantState: cloudprovider.InstanceCreating},
		{name: "failed stopped", power: vmPowerStateStopped, provisioning: VMProvisioningStateFailed,
			wantState: cloudprovider.InstanceCreating, wantFailure: true},
		{name: "failed deallocated", power: vmPowerStateDeallocated, provisioning: VMProvisioningStateFailed,
			wantState: cloudprovider.InstanceCreating, wantFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			world := &suspendedWorld{states: []string{vmPowerStateRunning}, provisioning: map[int]string{}}
			_, group := newSuspendedProvider(t, world, fake.NewClientset(), 0, 3)
			world.mu.Lock()
			world.states[0] = tc.power
			if tc.provisioning != "" {
				world.provisioning[0] = tc.provisioning
			}
			world.mu.Unlock()
			group.invalidateSuspendedInventory()
			group.enableFastDeleteOnFailedProvisioning = true
			instances, err := group.Nodes(t.Context())
			if tc.wantError {
				require.ErrorContains(t, err, "unknown VM power state")
				require.Empty(t, world.history())
				return
			}
			require.NoError(t, err)
			require.Len(t, instances, 1)
			require.Equal(t, tc.wantState, instances[0].Status.State)
			require.Equal(t, tc.wantFailure, instances[0].Status.ErrorInfo != nil)
			instances[0].Status.State = cloudprovider.InstanceDeleting
			again, err := group.Nodes(t.Context())
			require.NoError(t, err)
			require.Equal(t, tc.wantState, again[0].Status.State, "returned status must not alias the cache")
		})
	}
}

func TestSuspendedRunningBounds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		states []string
		parked int
	}{
		{name: "running", states: []string{vmPowerStateRunning, vmPowerStateRunning, vmPowerStateRunning}},
		{name: "partly parked", states: []string{vmPowerStateRunning, vmPowerStateDeallocated, vmPowerStateDeallocated}, parked: 2},
		{name: "fully parked", states: []string{vmPowerStateDeallocated, vmPowerStateDeallocated, vmPowerStateDeallocated}, parked: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, group := newSuspendedProvider(t, &suspendedWorld{states: tc.states}, fake.NewClientset(), 2, 3)
			total, err := group.TargetSize(t.Context())
			require.NoError(t, err)
			require.Equal(t, 3, total)
			require.Equal(t, 2+tc.parked, group.MinSize(t.Context()))
			require.Equal(t, 3+tc.parked, group.MaxSize(t.Context()))
			running := total - tc.parked
			require.Equal(t, 3-running, group.MaxSize(t.Context())-total, "selection and estimator headroom")
			require.Equal(t, 2-running, group.MinSize(t.Context())-total, "minimum-size growth")
			require.Equal(t, fmt.Sprintf("pool (%d:%d)", 2+tc.parked, 3+tc.parked), group.Debug(t.Context()))

			cache, client := provider.azureManager.azureCache, provider.azureManager.azClient
			provider.azureManager.azureCache, provider.azureManager.azClient = nil, nil
			func() {
				defer func() { provider.azureManager.azureCache, provider.azureManager.azClient = cache, client }()
				require.Equal(t, 2+tc.parked, group.MinSize(t.Context()), "bounds must not read Azure or its resource cache")
				require.Equal(t, 3+tc.parked, group.MaxSize(t.Context()))
			}()
		})
	}
}

func TestSuspendedGetOptions(t *testing.T) {
	for _, tc := range []struct {
		name          string
		startup       time.Duration
		provision     time.Duration
		tags          map[string]string
		missingVMSS   bool
		deleteMode    bool
		wantStartup   time.Duration
		wantProvision time.Duration
	}{
		{name: "defaults", startup: 15 * time.Minute, provision: 15 * time.Minute,
			wantStartup: 21*time.Minute + time.Second, wantProvision: 15 * time.Minute},
		{name: "global provision", startup: time.Minute, provision: 30 * time.Minute,
			wantStartup: 36*time.Minute + time.Second, wantProvision: 30 * time.Minute},
		{name: "longer global startup", startup: time.Hour, provision: 15 * time.Minute,
			wantStartup: time.Hour, wantProvision: 15 * time.Minute},
		{name: "pool provision", startup: 15 * time.Minute, provision: 15 * time.Minute,
			tags:        map[string]string{config.DefaultMaxNodeProvisionTimeKey: "30m"},
			wantStartup: 36*time.Minute + time.Second, wantProvision: 30 * time.Minute},
		{name: "longer pool startup", startup: 15 * time.Minute, provision: 15 * time.Minute,
			tags:        map[string]string{config.DefaultMaxNodeStartupTimeKey: "1h"},
			wantStartup: time.Hour, wantProvision: 15 * time.Minute},
		{name: "shorter pool startup", startup: time.Hour, provision: 15 * time.Minute,
			tags:        map[string]string{config.DefaultMaxNodeStartupTimeKey: "1m"},
			wantStartup: 21*time.Minute + time.Second, wantProvision: 15 * time.Minute},
		{name: "invalid pool options", startup: 15 * time.Minute, provision: 15 * time.Minute,
			tags:        map[string]string{config.DefaultMaxNodeStartupTimeKey: "invalid", config.DefaultMaxNodeProvisionTimeKey: "invalid"},
			wantStartup: 21*time.Minute + time.Second, wantProvision: 15 * time.Minute},
		{name: "missing VMSS", startup: 15 * time.Minute, provision: 15 * time.Minute, missingVMSS: true,
			wantStartup: 21*time.Minute + time.Second, wantProvision: 15 * time.Minute},
		{name: "Delete unchanged", startup: time.Minute, provision: time.Hour, deleteMode: true,
			tags:        map[string]string{config.DefaultMaxNodeStartupTimeKey: "2h", config.DefaultMaxNodeProvisionTimeKey: "2h"},
			wantStartup: time.Minute, wantProvision: time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, group := newSuspendedProvider(t, &suspendedWorld{states: []string{vmPowerStateRunning}},
				fake.NewClientset(), 0, 3)
			group.deallocate = !tc.deleteMode
			group.manager.azureCache.autoscalingOptions[azureRef{Name: group.Name}] = tc.tags
			if tc.missingVMSS {
				delete(group.manager.azureCache.scaleSets, group.Name)
			}
			defaults := config.NodeGroupAutoscalingOptions{
				MaxNodeStartupTime: tc.startup, MaxNodeProvisionTime: tc.provision,
				ScaleDownUnneededTime: time.Hour, ScaleDownUnreadyTime: time.Minute,
				ScaleDownUtilizationThreshold: 0.3, IgnoreDaemonSetsUtilization: true,
			}
			options, err := group.GetOptions(t.Context(), defaults)
			require.NoError(t, err)
			expected := defaults
			expected.MaxNodeStartupTime, expected.MaxNodeProvisionTime = tc.wantStartup, tc.wantProvision
			require.Equal(t, expected, *options)
			processor := nodegroupconfig.NewDefaultNodeGroupConfigProcessor(defaults)
			startup, err := processor.GetMaxNodeStartupTime(t.Context(), group)
			require.NoError(t, err)
			provision, err := processor.GetMaxNodeProvisionTime(t.Context(), group)
			require.NoError(t, err)
			require.Equal(t, tc.wantStartup, startup)
			require.Equal(t, tc.wantProvision, provision)
			if tc.deleteMode {
				return
			}
			transition := metav1.NewTime(time.Unix(100, 999999999))
			data, err := json.Marshal(transition)
			require.NoError(t, err)
			var stored metav1.Time
			require.NoError(t, json.Unmarshal(data, &stored))
			requestTime := transition.Add(2 * vmssContextTimeout)
			deadline := requestTime.Add(provision)
			require.True(t, stored.Add(startup).After(deadline),
				"the resumed Node must remain NotStarted past the scale-up deadline")
		})
	}
}

func TestSuspendedBoundsKeepLastSnapshotOnError(t *testing.T) {
	world := &suspendedWorld{states: []string{vmPowerStateRunning, vmPowerStateDeallocated}}
	_, group := newSuspendedProvider(t, world, fake.NewClientset(), 1, 3)
	require.Equal(t, 2, group.MinSize(t.Context()))
	require.Equal(t, 4, group.MaxSize(t.Context()))
	world.mu.Lock()
	world.inventoryErr = errors.New("inventory unavailable")
	world.mu.Unlock()
	group.invalidateSuspendedInventory()
	_, err := group.Nodes(t.Context())
	require.ErrorContains(t, err, "inventory unavailable")
	_, err = group.TargetSize(t.Context())
	require.ErrorContains(t, err, "inventory unavailable")
	require.ErrorContains(t, group.IncreaseSize(t.Context(), 1), "inventory unavailable")
	require.Equal(t, 2, group.MinSize(t.Context()))
	require.Equal(t, 4, group.MaxSize(t.Context()))
	require.Empty(t, world.history())
}

func TestSuspendedInventoryDoesNotReuseVMIDOverride(t *testing.T) {
	world := &suspendedWorld{states: []string{vmPowerStateDeallocated}, identities: map[int]string{}}
	_, group := newSuspendedProvider(t, world, fake.NewClientset(), 0, 3)
	group.recordPower("vm-0", suspendedPowerOperation{})
	instances, err := group.Nodes(t.Context())
	require.NoError(t, err)
	require.Equal(t, cloudprovider.InstanceCreating, instances[0].Status.State)
	world.mu.Lock()
	world.identities[0] = "replacement"
	world.mu.Unlock()
	group.invalidateSuspendedInventory()
	instances, err = group.Nodes(t.Context())
	require.NoError(t, err)
	require.Equal(t, cloudprovider.InstanceRunning, instances[0].Status.State)
	_, pending := group.powerOperation("vm-0")
	require.False(t, pending)
}

func TestSuspendedMutationsRequireFreshInventory(t *testing.T) {
	node := suspendedTestNode(0, false)
	world := &suspendedWorld{states: []string{vmPowerStateRunning}}
	_, group := newSuspendedProvider(t, world, fake.NewClientset(node), 0, 3)
	inventoryErr := errors.New("inventory unavailable")
	world.mu.Lock()
	world.inventoryErr = inventoryErr
	world.mu.Unlock()
	require.ErrorIs(t, group.IncreaseSize(t.Context(), 1), inventoryErr)
	require.ErrorIs(t, group.DeleteNodes(t.Context(), []*apiv1.Node{node}), inventoryErr)
	require.Empty(t, world.history())
}

func TestSuspendedRediscoveryKeepsPendingOperations(t *testing.T) {
	world := &suspendedWorld{states: []string{vmPowerStateDeallocated}}
	provider, group := newSuspendedProvider(t, world, fake.NewClientset(), 0, 3)
	group.recordPower("vm-0", suspendedPowerOperation{})
	next, err := NewScaleSet(&dynamic.NodeGroupSpec{Name: "pool", MinSize: 0, MaxSize: 4},
		provider.azureManager, 1, false)
	require.NoError(t, err)
	require.True(t, provider.azureManager.RegisterNodeGroup(next))
	_, pending := next.powerOperation("vm-0")
	require.True(t, pending)
	require.NoError(t, next.IncreaseSize(t.Context(), 1))
	require.Equal(t, []string{"grow:2"}, world.history())
	_, pending = next.powerOperation("vm-0")
	require.True(t, pending, "new demand preserves the original accepted VMID")
}
