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
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	azruntime "github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	apiv1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachineclient/mock_virtualmachineclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetclient/mock_virtualmachinescalesetclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetvmclient/mock_virtualmachinescalesetvmclient"
	providerconfig "sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
	"sigs.k8s.io/cluster-autoscaler/pkg/cloudprovider"
	statusutils "sigs.k8s.io/cluster-autoscaler/pkg/clusterstate/utils"
	"sigs.k8s.io/cluster-autoscaler/pkg/config/dynamic"
	"sigs.k8s.io/cluster-autoscaler/pkg/core"
	"sigs.k8s.io/cluster-autoscaler/pkg/test/integration"
	synctestutils "sigs.k8s.io/cluster-autoscaler/pkg/test/integration/synctest"
	"sigs.k8s.io/cluster-autoscaler/pkg/utils/taints"
	catest "sigs.k8s.io/cluster-autoscaler/pkg/utils/test"
)

const parkingVMSSID = "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/pool"

type parkingWorld struct {
	mu           sync.Mutex
	states       []string
	vmIDs        map[int]string
	provisioning map[int]string
	deleted      map[int]bool
	events       []string
	rejectStart  bool
	failStart    bool
	failStop     bool
	startPolled  chan struct{}
	finishStart  chan struct{}
}

func (w *parkingWorld) record(event string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, event)
}

func (w *parkingWorld) history() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.events)
}

type parkingPowerTransport struct {
	mu               sync.Mutex
	world            *parkingWorld
	requests         []*http.Request
	rejectDeallocate bool
	failDeallocate   int
}

func (t *parkingPowerTransport) Do(request *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.requests = append(t.requests, request)
	t.mu.Unlock()

	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	if request.Method != http.MethodPost || len(parts) < 2 {
		return nil, fmt.Errorf("unexpected power request %s %s", request.Method, request.URL.Path)
	}
	id, action := parts[len(parts)-2], parts[len(parts)-1]
	instance, err := strconv.Atoi(id)
	if err != nil {
		return nil, fmt.Errorf("invalid VM instance %q: %w", id, err)
	}
	expectedPath := fmt.Sprintf("%s/virtualMachines/%s/%s", parkingVMSSID, id, action)
	if !strings.EqualFold(request.URL.Path, expectedPath) {
		return nil, fmt.Errorf("power request addressed %s, expected %s", request.URL.Path, expectedPath)
	}
	t.mu.Lock()
	if action == "deallocate" && t.failDeallocate > 0 {
		t.failDeallocate--
		t.mu.Unlock()
		t.world.record("deallocate-transport-error:" + id)
		return nil, errors.New("injected ambiguous Deallocate transport failure")
	}
	t.mu.Unlock()
	if action == "deallocate" && t.rejectDeallocate {
		t.world.record("deallocate-rejected:" + id)
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"error":{"code":"InvalidParameter","message":"Deallocate rejected before acceptance."}}`,
			)),
			Request: request,
		}, nil
	}

	t.world.record(action + ":" + id)
	t.world.mu.Lock()
	if instance < 0 || instance >= len(t.world.states) {
		t.world.mu.Unlock()
		return nil, fmt.Errorf("VM instance %q is not in the authoritative inventory", id)
	}
	switch action {
	case "deallocate":
		t.world.states[instance] = vmPowerStateDeallocated
	case "start":
		t.world.states[instance] = vmPowerStateRunning
	default:
		t.world.mu.Unlock()
		return nil, fmt.Errorf("unexpected power action %q", action)
	}
	t.world.mu.Unlock()
	t.world.record(action + "-complete:" + id)

	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"status":"Succeeded"}`)),
		Request:    request,
	}, nil
}

func (t *parkingPowerTransport) history() []*http.Request {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.requests)
}

func (w *parkingWorld) vmss() *armcompute.VirtualMachineScaleSet {
	w.mu.Lock()
	defer w.mu.Unlock()
	vmss := newTestVMSSList(int64(len(w.states)-len(w.deleted)), "pool", "eastus", armcompute.OrchestrationModeUniform)[0]
	vmss.ID = ptr.To(parkingVMSSID)
	vmss.Tags = map[string]*string{
		"k8s.io_cluster-autoscaler_node-template_label_pool":       ptr.To("workers"),
		"k8s.io_cluster-autoscaler_node-template_resources_cpu":    ptr.To("4000m"),
		"k8s.io_cluster-autoscaler_node-template_resources_memory": ptr.To("8Gi"),
	}
	vmss.Properties.VirtualMachineProfile = &armcompute.VirtualMachineScaleSetVMProfile{
		StorageProfile: &armcompute.VirtualMachineScaleSetStorageProfile{
			OSDisk: &armcompute.VirtualMachineScaleSetOSDisk{
				ManagedDisk: &armcompute.VirtualMachineScaleSetManagedDiskParameters{},
			},
		},
	}
	return vmss
}

func (w *parkingWorld) vms() []*armcompute.VirtualMachineScaleSetVM {
	w.mu.Lock()
	defer w.mu.Unlock()
	vms := make([]*armcompute.VirtualMachineScaleSetVM, 0, len(w.states))
	for i, state := range w.states {
		if w.deleted[i] {
			continue
		}
		id := strconv.Itoa(i)
		provisioning := w.provisioning[i]
		if provisioning == "" {
			provisioning = provisioningStateSucceeded
		}
		vmID := w.vmIDs[i]
		if vmID == "" {
			vmID = "vm-identity-" + id
		}
		vms = append(vms, &armcompute.VirtualMachineScaleSetVM{
			ID: ptr.To(parkingVMSSID + "/virtualMachines/" + id), InstanceID: ptr.To(id),
			Properties: &armcompute.VirtualMachineScaleSetVMProperties{
				VMID: ptr.To(vmID), ProvisioningState: ptr.To(provisioning),
				OSProfile: &armcompute.OSProfile{ComputerName: ptr.To("node-" + id)},
				InstanceView: &armcompute.VirtualMachineScaleSetVMInstanceView{
					Statuses: []*armcompute.InstanceViewStatus{{Code: ptr.To(state)}},
				},
			},
		})
	}
	return vms
}

type parkingPoller[T any] struct {
	done   bool
	finish func() error
}

func (p *parkingPoller[T]) Done() bool { return p.done }
func (p *parkingPoller[T]) Poll(context.Context) (*http.Response, error) {
	p.done = true
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}, nil
}
func (p *parkingPoller[T]) Result(context.Context, *T) error { return p.finish() }

func newParkingPoller[T any](finish func() error) (*azruntime.Poller[T], error) {
	return azruntime.NewPoller(
		&http.Response{StatusCode: http.StatusAccepted, Header: http.Header{}, Body: http.NoBody},
		azruntime.NewPipeline("test", "v0", azruntime.PipelineOptions{}, nil),
		&azruntime.NewPollerOptions[T]{Handler: &parkingPoller[T]{finish: finish}},
	)
}

func (w *parkingWorld) BeginDeallocate(_ context.Context, _, _, id string, _ *armcompute.VirtualMachineScaleSetVMsClientBeginDeallocateOptions) (*azruntime.Poller[armcompute.VirtualMachineScaleSetVMsClientDeallocateResponse], error) {
	w.record("deallocate:" + id)
	return newParkingPoller[armcompute.VirtualMachineScaleSetVMsClientDeallocateResponse](func() error {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.failStop {
			return errors.New("injected stop failure")
		}
		i, _ := strconv.Atoi(id)
		w.states[i] = vmPowerStateDeallocated
		return nil
	})
}

func (w *parkingWorld) BeginStart(_ context.Context, _, _, id string, _ *armcompute.VirtualMachineScaleSetVMsClientBeginStartOptions) (*azruntime.Poller[armcompute.VirtualMachineScaleSetVMsClientStartResponse], error) {
	w.record("start:" + id)
	if w.rejectStart {
		return nil, errors.New("injected Start rejection")
	}
	return newParkingPoller[armcompute.VirtualMachineScaleSetVMsClientStartResponse](func() error {
		if w.startPolled != nil {
			close(w.startPolled)
			<-w.finishStart
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.failStart {
			return errors.New("injected Start polling failure")
		}
		i, _ := strconv.Atoi(id)
		w.states[i] = vmPowerStateRunning
		return nil
	})
}

func newParkingProvider(t *testing.T, w *parkingWorld, client *fake.Clientset, minimum, maximum int, enabled bool) (*AzureCloudProvider, *ScaleSet) {
	t.Helper()
	ctrl := gomock.NewController(t)
	vmssClient := mock_virtualmachinescalesetclient.NewMockInterface(ctrl)
	vmssClient.EXPECT().List(gomock.Any(), "rg").DoAndReturn(func(context.Context, string) ([]*armcompute.VirtualMachineScaleSet, error) {
		return []*armcompute.VirtualMachineScaleSet{w.vmss()}, nil
	}).AnyTimes()
	vmClient := mock_virtualmachineclient.NewMockInterface(ctrl)
	vmClient.EXPECT().List(gomock.Any(), "rg").Return(nil, nil).AnyTimes()
	instanceClient := mock_virtualmachinescalesetvmclient.NewMockInterface(ctrl)
	instanceClient.EXPECT().ListVMInstanceView(gomock.Any(), "rg", "pool").DoAndReturn(func(context.Context, string, string) ([]*armcompute.VirtualMachineScaleSetVM, error) {
		return w.vms(), nil
	}).AnyTimes()
	resizeClient := NewMockVMSSDeleteClient(ctrl)
	resizeClient.EXPECT().BeginCreateOrUpdate(gomock.Any(), "rg", "pool", gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _, _ string, vmss armcompute.VirtualMachineScaleSet, _ *armcompute.VirtualMachineScaleSetsClientBeginCreateOrUpdateOptions) (*azruntime.Poller[armcompute.VirtualMachineScaleSetsClientCreateOrUpdateResponse], error) {
			w.record(fmt.Sprintf("grow:%d", *vmss.SKU.Capacity))
			w.mu.Lock()
			for len(w.states)-len(w.deleted) < int(*vmss.SKU.Capacity) {
				w.states = append(w.states, vmPowerStateStarting)
			}
			w.mu.Unlock()
			return newParkingPoller[armcompute.VirtualMachineScaleSetsClientCreateOrUpdateResponse](func() error { return nil })
		}).AnyTimes()
	resizeClient.EXPECT().BeginDeleteInstances(gomock.Any(), "rg", "pool", gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _, _ string, ids armcompute.VirtualMachineScaleSetVMInstanceRequiredIDs, _ *armcompute.VirtualMachineScaleSetsClientBeginDeleteInstancesOptions) (*azruntime.Poller[armcompute.VirtualMachineScaleSetsClientDeleteInstancesResponse], error) {
			for _, id := range ids.InstanceIDs {
				w.record("physical-delete:" + *id)
			}
			return newParkingPoller[armcompute.VirtualMachineScaleSetsClientDeleteInstancesResponse](func() error {
				w.mu.Lock()
				defer w.mu.Unlock()
				if w.deleted == nil {
					w.deleted = make(map[int]bool)
				}
				for _, id := range ids.InstanceIDs {
					i, err := strconv.Atoi(*id)
					if err != nil {
						return err
					}
					w.deleted[i] = true
				}
				return nil
			})
		}).AnyTimes()
	m := &AzureManager{
		config: &Config{
			Config:                 providerconfig.Config{VMType: "vmss", ResourceGroup: "rg", Location: "eastus"},
			ProviderOnlyDeallocate: enabled,
		},
		env: azure.PublicCloud, kubeClient: client, explicitlyConfigured: map[string]bool{"pool": true},
		azClient: &azClient{
			virtualMachineScaleSetsClient: vmssClient, virtualMachineScaleSetVMsClient: instanceClient,
			virtualMachinesClient: vmClient, vmssClientForDelete: resizeClient, vmssPowerClient: w,
		},
	}
	var err error
	m.azureCache, err = newAzureCache(m.azClient, time.Minute, *m.config)
	require.NoError(t, err)
	t.Cleanup(m.Cleanup)
	group, err := NewScaleSet(&dynamic.NodeGroupSpec{Name: "pool", MinSize: minimum, MaxSize: maximum}, m, *w.vmss().SKU.Capacity, false)
	require.NoError(t, err)
	m.RegisterNodeGroup(group)
	require.NoError(t, m.forceRefresh())
	provider, err := BuildAzureCloudProvider(m, cloudprovider.NewResourceLimiter(map[string]int64{}, map[string]int64{}))
	require.NoError(t, err)
	return provider.(*AzureCloudProvider), group
}

// Registration is simulated from fixed kubelet/CCM configuration, not the old Node.
func parkingNode(id int, uid string, ready bool) *apiv1.Node {
	node := catest.BuildTestNode(fmt.Sprintf("node-%d", id), 4000, 8*1024*1024*1024, catest.IsReady(ready))
	node.UID = types.UID(uid)
	node.CreationTimestamp = metav1.NewTime(time.Now())
	node.Spec.ProviderID = fmt.Sprintf("%s%s/virtualMachines/%d", azurePrefix, parkingVMSSID, id)
	node.Labels = map[string]string{
		"pool": "workers", apiv1.LabelHostname: node.Name,
		apiv1.LabelOSStable: "linux", apiv1.LabelArchStable: "amd64",
		apiv1.LabelInstanceTypeStable: "Standard_D4_v2", apiv1.LabelTopologyRegion: "eastus",
	}
	return node
}

func addParkingReceipt(t *testing.T, node *apiv1.Node, vm *armcompute.VirtualMachineScaleSetVM) {
	t.Helper()
	receipt, err := newProviderOnlyDeleteReceipt(node, vm)
	require.NoError(t, err)
	value, err := providerOnlyDeleteReceiptValue(receipt)
	require.NoError(t, err)
	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}
	node.Annotations[providerOnlyDeleteReceiptAnnotation] = value
}

func parkingAutoscaler(t *testing.T, ctx context.Context, infra *integration.TestInfrastructure, provider *AzureCloudProvider) *core.StaticAutoscaler {
	t.Helper()
	opts := integration.NewTestConfig().ResolveOptions()
	opts.CloudProviderName = "azure"
	opts.InitialNodeGroupBackoffDuration = 5 * time.Minute
	opts.MaxNodeGroupBackoffDuration = 30 * time.Minute
	opts.NodeGroupBackoffResetTimeout = 3 * time.Hour
	opts.CordonNodeBeforeTerminate = true
	opts.NodeGroupDefaults.ScaleDownUnneededTime = time.Second
	opts.NodeGroupDefaults.MaxNodeProvisionTime = 10 * time.Minute
	opts.MaxGracefulTerminationSec = 30
	opts.MaxPodEvictionTime = 30 * time.Second
	opts.UnremovableNodeRecheckTimeout = time.Second
	autoscaler, _, err := integration.DefaultAutoscalingBuilder(opts, infra).WithCloudProvider(provider).Build(ctx)
	require.NoError(t, err)
	require.NoError(t, autoscaler.Start())
	static := autoscaler.(*core.StaticAutoscaler)
	go func() {
		<-ctx.Done()
		static.ClusterStateRegistry.Stop()
	}()
	return static
}

func checkParkingSize(t *testing.T, group *ScaleSet, want int) {
	t.Helper()
	size, err := group.TargetSize(context.Background())
	require.NoError(t, err)
	require.Equal(t, want, size)
}

func checkParkingCounts(t *testing.T, group *ScaleSet, physical, active, parked, returning int) {
	t.Helper()
	vms, parkedByID, err := group.parkingInventory()
	require.NoError(t, err)
	require.Len(t, vms, physical)
	actualParked := 0
	for _, isParked := range parkedByID {
		if isParked {
			actualParked++
		}
	}
	require.Equal(t, parked, actualParked)
	require.Equal(t, active, physical-actualParked)
	checkParkingSize(t, group, active)

	group.powerMutex.Lock()
	actualReturning := 0
	for _, isParked := range group.powerOverrides {
		if !isParked {
			actualReturning++
		}
	}
	group.powerMutex.Unlock()
	require.Equal(t, returning, actualReturning)
}

func TestProviderOnlyDeallocateStockLoop(t *testing.T) {
	infra := integration.SetupInfrastructure(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer synctestutils.TearDown(cancel)
		client := infra.Fakes.KubeClient
		world := &parkingWorld{states: []string{vmPowerStateRunning, vmPowerStateRunning}}
		provider, group := newParkingProvider(t, world, client, 1, 2, true)
		vmIdentity := *world.vms()[1].Properties.VMID
		for i := range 2 {
			node := parkingNode(i, fmt.Sprintf("old-%d", i), true)
			node.Annotations = map[string]string{"operator.example/note": "not-registration-config"}
			_, err := client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
			require.NoError(t, err)
		}
		busy := catest.BuildScheduledTestPod("busy", 2500, 100, "node-0")
		movable := catest.BuildScheduledTestPod("movable", 100, 100, "node-1")
		movable.Labels = map[string]string{"app": "movable"}
		movable.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "movable", UID: "replicaset", Controller: ptr.To(true)}}
		for _, pod := range []*apiv1.Pod{busy, movable} {
			_, err := client.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
			require.NoError(t, err)
		}
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: "budget", Namespace: movable.Namespace},
			Spec:       policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: movable.Labels}},
			Status:     policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 0},
		}
		_, err := client.PolicyV1().PodDisruptionBudgets(pdb.Namespace).Create(ctx, pdb, metav1.CreateOptions{})
		require.NoError(t, err)
		client.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
			if action.GetSubresource() != "eviction" {
				return false, nil, nil
			}
			eviction := action.(clienttesting.CreateAction).GetObject().(*policyv1beta1.Eviction)
			budget, err := client.Tracker().Get(policyv1.SchemeGroupVersion.WithResource("poddisruptionbudgets"), movable.Namespace, "budget")
			require.NoError(t, err)
			if budget.(*policyv1.PodDisruptionBudget).Status.DisruptionsAllowed == 0 {
				return true, nil, apierrors.NewTooManyRequests("PDB blocks eviction", 1)
			}
			current, err := client.Tracker().Get(apiv1.SchemeGroupVersion.WithResource("nodes"), "", "node-1")
			require.NoError(t, err)
			require.True(t, current.(*apiv1.Node).Spec.Unschedulable)
			require.True(t, taints.HasToBeDeletedTaint(current.(*apiv1.Node)))
			world.record("evict:" + eviction.Name)
			return true, &metav1.Status{Status: "Success"}, client.Tracker().Delete(apiv1.SchemeGroupVersion.WithResource("pods"), action.GetNamespace(), eviction.Name)
		})
		deleteCalls := 0
		client.PrependReactor("delete", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
			a := action.(clienttesting.DeleteAction)
			obj, err := client.Tracker().Get(apiv1.SchemeGroupVersion.WithResource("nodes"), "", a.GetName())
			if err != nil {
				return true, nil, err
			}
			node := obj.(*apiv1.Node)
			preconditions := a.GetDeleteOptions().Preconditions
			require.NotNil(t, preconditions)
			require.Equal(t, node.UID, *preconditions.UID)
			require.Equal(t, vmPowerStateDeallocated, *world.vms()[1].Properties.InstanceView.Statuses[0].Code)
			deleteCalls++
			world.record(fmt.Sprintf("delete:%s:attempt-%d", node.Name, deleteCalls))
			if deleteCalls == 1 {
				return true, nil, apierrors.NewServiceUnavailable("injected transient Node DELETE failure")
			}
			return false, nil, nil
		})
		firstCtx, stopFirst := context.WithCancel(ctx)
		defer stopFirst()
		autoscaler := parkingAutoscaler(t, firstCtx, infra, provider)
		synctestutils.MustRunOnceAfter(t, autoscaler, 10*time.Second)
		synctestutils.MustRunOnceAfter(t, autoscaler, 10*time.Second)
		require.Empty(t, world.history(), "PDB must prevent deallocation")
		pdb.Status.DisruptionsAllowed = 1
		_, err = client.PolicyV1().PodDisruptionBudgets(pdb.Namespace).UpdateStatus(ctx, pdb, metav1.UpdateOptions{})
		require.NoError(t, err)
		for range 3 {
			synctestutils.MustRunOnceAfter(t, autoscaler, 10*time.Second)
			if deleteCalls == 1 {
				break
			}
		}
		require.Equal(t, 1, deleteCalls)
		require.Equal(t, []string{"evict:movable", "deallocate:1", "delete:node-1:attempt-1"}, world.history())
		interrupted, err := client.CoreV1().Nodes().Get(ctx, "node-1", metav1.GetOptions{})
		require.NoError(t, err)
		require.Contains(t, interrupted.Annotations, providerOnlyDeleteReceiptAnnotation)
		require.Equal(t, "not-registration-config", interrupted.Annotations["operator.example/note"])
		require.False(t, taints.HasToBeDeletedTaint(interrupted), "stock cleanup must remove only its deletion taint")
		checkParkingSize(t, group, 1)
		require.Equal(t, int64(2), *world.vmss().SKU.Capacity)

		// Recreate provider and core; refresh must discover the receipt without the old in-memory Node.
		stopFirst()
		synctest.Wait()
		infra.Fakes.InformerFactory = informers.NewSharedInformerFactory(client, 0)
		provider, group = newParkingProvider(t, world, client, 1, 2, true)
		autoscaler = parkingAutoscaler(t, ctx, infra, provider)
		checkParkingSize(t, group, 1)
		require.Empty(t, group.powerOverrides)
		synctestutils.MustRunOnceAfter(t, autoscaler, 10*time.Second)
		require.Equal(t, 2, deleteCalls)
		require.Equal(t, []string{
			"evict:movable",
			"deallocate:1",
			"delete:node-1:attempt-1",
			"delete:node-1:attempt-2",
		}, world.history())
		_, err = client.CoreV1().Nodes().Get(ctx, "node-1", metav1.GetOptions{})
		require.True(t, apierrors.IsNotFound(err))
		has, err := provider.HasInstance(ctx, parkingNode(1, "old-1", true))
		require.NoError(t, err)
		require.False(t, has)
		ng, err := provider.NodeGroupForNode(ctx, parkingNode(1, "old-1", true))
		require.NoError(t, err)
		require.Same(t, group, ng)
		pending := catest.BuildTestPod("demand", 2000, 100, catest.MarkUnschedulable())
		pending.Spec.NodeSelector = map[string]string{"pool": "workers"}
		_, err = client.CoreV1().Pods(pending.Namespace).Create(ctx, pending, metav1.CreateOptions{})
		require.NoError(t, err)
		synctestutils.MustRunOnceAfter(t, autoscaler, 10*time.Second)
		require.Equal(t, "start:1", world.history()[4])
		require.Len(t, world.history(), 5, "reuse must not issue a physical growth request")
		checkParkingSize(t, group, 2)
		synctestutils.MustRunOnceAfter(t, autoscaler, time.Second)
		require.Len(t, world.history(), 5, "incoming reuse must not be requested twice")
		upcoming, _ := autoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
		require.Equal(t, 1, upcoming["pool"])
		currentSize, targetSize := autoscaler.ClusterStateRegistry.GetAutoscaledNodesCount()
		require.Equal(t, 1, currentSize)
		require.Equal(t, 2, targetSize)
		require.Equal(t, vmIdentity, *world.vms()[1].Properties.VMID)

		newNode := parkingNode(1, "new-1", false)
		newNode.Spec.Taints = []apiv1.Taint{
			{Key: "node.cloudprovider.kubernetes.io/uninitialized", Value: "true", Effect: apiv1.TaintEffectNoSchedule},
			{Key: apiv1.TaintNodeNotReady, Effect: apiv1.TaintEffectNoSchedule},
		}
		_, err = client.CoreV1().Nodes().Create(ctx, newNode, metav1.CreateOptions{})
		require.NoError(t, err)
		world.record("register:node-1:new-1")
		require.Empty(t, newNode.Annotations)
		require.NotEqual(t, types.UID("old-1"), newNode.UID)
		synctestutils.MustRunOnceAfter(t, autoscaler, time.Second)
		require.Len(t, world.history(), 6)
		require.Error(t, autoscaler.ClusterSnapshot.CheckPredicates(pending, newNode.Name))

		newNode.Spec.Taints = newNode.Spec.Taints[1:]
		_, err = client.CoreV1().Nodes().Update(ctx, newNode, metav1.UpdateOptions{})
		require.NoError(t, err)
		synctestutils.MustRunOnceAfter(t, autoscaler, time.Second)
		require.Error(t, autoscaler.ClusterSnapshot.CheckPredicates(pending, newNode.Name))

		newNode.Spec.Taints = nil
		catest.IsReady(true)(newNode)
		_, err = client.CoreV1().Nodes().Update(ctx, newNode, metav1.UpdateOptions{})
		require.NoError(t, err)
		synctestutils.MustRunOnceAfter(t, autoscaler, time.Second)
		checkParkingSize(t, group, 2)
		require.Len(t, world.history(), 6)
		require.NoError(t, autoscaler.ClusterSnapshot.CheckPredicates(pending, newNode.Name))
		upcoming, _ = autoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
		require.Zero(t, upcoming["pool"])
		currentSize, targetSize = autoscaler.ClusterStateRegistry.GetAutoscaledNodesCount()
		require.Equal(t, 2, currentSize)
		require.Equal(t, 2, targetSize)
	})
}

func TestProviderOnlyDeallocateInvalidReceiptDoesNotBlockCore(t *testing.T) {
	infra := integration.SetupInfrastructure(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer synctestutils.TearDown(cancel)
		client := infra.Fakes.KubeClient
		world := &parkingWorld{states: []string{vmPowerStateDeallocated}}
		provider, group := newParkingProvider(t, world, client, 0, 1, true)
		unmanaged := parkingNode(9, "unmanaged", false)
		unmanaged.Name = "unmanaged-node"
		unmanaged.Spec.ProviderID = strings.Replace(unmanaged.Spec.ProviderID, "/pool/", "/other-pool/", 1)
		unmanaged.Annotations = map[string]string{providerOnlyDeleteReceiptAnnotation: "{"}
		_, err := client.CoreV1().Nodes().Create(ctx, unmanaged, metav1.CreateOptions{})
		require.NoError(t, err)
		demand := catest.BuildTestPod("demand", 2000, 100, catest.MarkUnschedulable())
		demand.Spec.NodeSelector = map[string]string{"pool": "workers"}
		_, err = client.CoreV1().Pods(demand.Namespace).Create(ctx, demand, metav1.CreateOptions{})
		require.NoError(t, err)

		autoscaler := parkingAutoscaler(t, ctx, infra, provider)
		synctestutils.MustRunOnceAfter(t, autoscaler, time.Second)
		require.Equal(t, []string{"start:0"}, world.history())
		checkParkingSize(t, group, 1)
	})
}

func TestProviderOnlyDeallocateBusyGroupDefersReceiptRecovery(t *testing.T) {
	ctx := t.Context()
	client := fake.NewClientset()
	world := &parkingWorld{states: []string{vmPowerStateDeallocated}}
	node := parkingNode(0, "old", true)
	addParkingReceipt(t, node, world.vms()[0])
	_, err := client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	require.NoError(t, err)
	deleteCalls := 0
	client.PrependReactor("delete", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		a := action.(clienttesting.DeleteAction)
		deleteCalls++
		require.Equal(t, node.UID, *a.GetDeleteOptions().Preconditions.UID)
		return false, nil, nil
	})
	provider, group := newParkingProvider(t, world, client, 0, 1, true)

	group.parkMutex.Lock()
	refreshDone := make(chan error, 1)
	go func() {
		refreshDone <- provider.Refresh(ctx)
	}()
	select {
	case err := <-refreshDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		group.parkMutex.Unlock()
		t.Fatal("Refresh blocked behind a busy provider-only power operation")
	}
	require.Zero(t, deleteCalls)
	_, err = client.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	group.parkMutex.Unlock()

	require.NoError(t, provider.Refresh(ctx))
	require.Equal(t, 1, deleteCalls)
	_, err = client.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err))
	require.Empty(t, world.history())
}

func TestProviderOnlyDeallocateReceiptDeleteFailureDoesNotBlockCore(t *testing.T) {
	infra := integration.SetupInfrastructure(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer synctestutils.TearDown(cancel)
		client := infra.Fakes.KubeClient
		world := &parkingWorld{states: []string{
			vmPowerStateDeallocated,
			vmPowerStateDeallocated,
		}}
		node := parkingNode(1, "old", false)
		addParkingReceipt(t, node, world.vms()[1])
		_, err := client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
		require.NoError(t, err)
		rejectDelete := true
		deleteCalls := 0
		client.PrependReactor("delete", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
			a := action.(clienttesting.DeleteAction)
			deleteCalls++
			require.Equal(t, node.UID, *a.GetDeleteOptions().Preconditions.UID)
			world.record(fmt.Sprintf("delete:%s:attempt-%d", node.Name, deleteCalls))
			if rejectDelete {
				return true, nil, apierrors.NewForbidden(
					schema.GroupResource{Resource: "nodes"},
					node.Name,
					errors.New("injected receipt DELETE rejection"),
				)
			}
			return false, nil, nil
		})
		provider, group := newParkingProvider(t, world, client, 0, 2, true)
		demand := catest.BuildTestPod("demand", 2000, 100, catest.MarkUnschedulable())
		demand.Spec.NodeSelector = map[string]string{"pool": "workers"}
		_, err = client.CoreV1().Pods(demand.Namespace).Create(ctx, demand, metav1.CreateOptions{})
		require.NoError(t, err)

		autoscaler := parkingAutoscaler(t, ctx, infra, provider)
		synctestutils.MustRunOnceAfter(t, autoscaler, time.Second)
		require.Equal(t, []string{"delete:node-1:attempt-1", "start:0"}, world.history())
		require.Equal(t, 1, deleteCalls)
		checkParkingSize(t, group, 1)
		current, err := client.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Contains(t, current.Annotations, providerOnlyDeleteReceiptAnnotation)

		rejectDelete = false
		synctestutils.MustRunOnceAfter(t, autoscaler, time.Second)
		require.Equal(t, 2, deleteCalls)
		_, err = client.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		require.True(t, apierrors.IsNotFound(err))
		require.Equal(t, []string{"delete:node-1:attempt-1", "start:0", "delete:node-1:attempt-2"}, world.history())
		checkParkingSize(t, group, 1)
	})
}

func TestProviderOnlyDeallocateZeroActiveAndFailures(t *testing.T) {
	for _, scenario := range []string{"reuse", "reject", "accepted-failed", "old-node-present"} {
		t.Run(scenario, func(t *testing.T) {
			infra := integration.SetupInfrastructure(t)
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer synctestutils.TearDown(cancel)
				world := &parkingWorld{states: []string{vmPowerStateDeallocated}, rejectStart: scenario == "reject", failStart: scenario == "accepted-failed"}
				client := infra.Fakes.KubeClient
				provider, group := newParkingProvider(t, world, client, 0, 1, true)
				if scenario == "old-node-present" {
					_, err := client.CoreV1().Nodes().Create(ctx, parkingNode(0, "old", false), metav1.CreateOptions{})
					require.NoError(t, err)
				}
				checkParkingSize(t, group, 0)
				template, err := group.TemplateNodeInfo(ctx)
				require.NoError(t, err)
				require.Equal(t, "workers", template.Node().Labels["pool"])
				pod := catest.BuildTestPod("demand", 2000, 100, catest.MarkUnschedulable())
				pod.Spec.NodeSelector = map[string]string{"pool": "workers"}
				_, err = client.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
				require.NoError(t, err)
				autoscaler := parkingAutoscaler(t, ctx, infra, provider)
				events := record.NewFakeRecorder(50)
				// Capture the ordinary event sink without changing planning or accounting.
				autoscaler.LogRecorder, err = statusutils.NewStatusMapRecorder(client, "kube-system", events, true, "test-ca-status")
				require.NoError(t, err)
				err = synctestutils.RunOnceAfter(t, autoscaler, time.Second)
				require.NoError(t, err, "stock RunOnce records scale-up errors but continues the loop")
				var eventMessages []string
				for len(events.Events) > 0 {
					eventMessages = append(eventMessages, <-events.Events)
				}
				messages := strings.Join(eventMessages, "\n")
				switch scenario {
				case "reuse":
					require.NoError(t, err)
					checkParkingSize(t, group, 1)
					require.Equal(t, []string{"start:0"}, world.history())
				case "reject":
					require.Contains(t, messages, "FailedToScaleUpGroup")
					require.Contains(t, messages, "injected Start rejection")
					checkParkingSize(t, group, 0)
					require.Equal(t, []string{"start:0"}, world.history())
				case "accepted-failed":
					require.Contains(t, messages, "FailedToScaleUpGroup")
					require.Contains(t, messages, "injected Start polling failure")
					checkParkingSize(t, group, 1)
					require.Equal(t, []string{"start:0"}, world.history())
					upcoming, _ := autoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
					require.Zero(t, upcoming["pool"], "failed acceptance must not be simulated as usable incoming capacity")
				case "old-node-present":
					require.Contains(t, messages, "FailedToScaleUpGroup")
					require.Contains(t, messages, "still has Node")
					checkParkingSize(t, group, 0)
					require.Empty(t, world.history())
				}
			})
		})
	}
}

func TestProviderOnlyDeallocateRejectedStartRecovery(t *testing.T) {
	infra := integration.SetupInfrastructure(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer synctestutils.TearDown(cancel)

		world := &parkingWorld{states: []string{vmPowerStateRunning, vmPowerStateDeallocated}}
		client := infra.Fakes.KubeClient
		provider, group := newParkingProvider(t, world, client, 1, 3, true)
		_, err := client.CoreV1().Nodes().Create(ctx, parkingNode(0, "active", true), metav1.CreateOptions{})
		require.NoError(t, err)

		busy := catest.BuildScheduledTestPod("busy", 3500, 100, "node-0")
		_, err = client.CoreV1().Pods(busy.Namespace).Create(ctx, busy, metav1.CreateOptions{})
		require.NoError(t, err)
		demand := catest.BuildTestPod("demand", 2000, 100, catest.MarkUnschedulable())
		demand.Spec.NodeSelector = map[string]string{"pool": "workers"}
		_, err = client.CoreV1().Pods(demand.Namespace).Create(ctx, demand, metav1.CreateOptions{})
		require.NoError(t, err)

		transport := &powerOperationTransport{rejectStart: true}
		provider.azureManager.azClient.vmssPowerClient = newTestVMSSPowerClient(t, transport)
		autoscaler := parkingAutoscaler(t, ctx, infra, provider)
		events := record.NewFakeRecorder(50)
		autoscaler.LogRecorder, err = statusutils.NewStatusMapRecorder(
			client,
			"kube-system",
			events,
			true,
			"test-ca-status",
		)
		require.NoError(t, err)

		require.NoError(
			t,
			synctestutils.RunOnceAfter(t, autoscaler, time.Second),
			"stock RunOnce records a definite Start rejection and keeps running",
		)
		var eventMessages []string
		for len(events.Events) > 0 {
			eventMessages = append(eventMessages, <-events.Events)
		}
		messages := strings.Join(eventMessages, "\n")
		require.Contains(t, messages, "FailedToScaleUpGroup")
		require.Contains(t, messages, "OperationNotAllowed")
		require.Contains(t, messages, "Start is not allowed for this VMSS instance")

		require.Len(t, transport.requests, 1)
		request := transport.requests[0]
		require.Equal(t, http.MethodPost, request.Method)
		require.Equal(
			t,
			"/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/"+
				"virtualMachineScaleSets/pool/virtualMachines/1/start",
			request.URL.Path,
		)
		require.Nil(t, request.Body)

		checkParkingSize(t, group, 1)
		vms, parked, err := group.parkingInventory()
		require.NoError(t, err)
		require.Len(t, vms, 2)
		require.False(t, parked["0"])
		require.True(t, parked["1"])
		require.Empty(t, group.powerOverrides, "a definitely rejected Start must not be counted as accepted")
		require.Equal(t, int64(2), *world.vmss().SKU.Capacity)
		require.Empty(t, world.history(), "a definite rejection must not trigger same-attempt fresh growth")

		instances, err := group.Nodes(ctx)
		require.NoError(t, err)
		require.Len(t, instances, 1)
		require.Equal(t, cloudprovider.InstanceRunning, instances[0].Status.State)
		upcoming, _ := autoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
		require.Zero(t, upcoming["pool"])
		currentSize, targetSize := autoscaler.ClusterStateRegistry.GetAutoscaledNodesCount()
		require.Equal(t, 1, currentSize)
		require.Equal(t, 1, targetSize)

		pending, err := client.CoreV1().Pods(demand.Namespace).Get(ctx, demand.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Empty(t, pending.Spec.NodeName)
		backoffStatus := autoscaler.ClusterStateRegistry.BackoffStatusForNodeGroup(ctx, group, time.Now())
		require.True(t, backoffStatus.IsBackedOff)
		require.Contains(t, backoffStatus.ErrorInfo.ErrorMessage, "OperationNotAllowed")

		require.NoError(t, provider.azureManager.forceRefresh())
		checkParkingSize(t, group, 1)
		require.Empty(t, group.powerOverrides)

		synctestutils.MustRunOnceAfter(t, autoscaler, time.Minute)
		require.Len(t, transport.requests, 1, "the production five-minute group backoff must suppress an immediate retry")
		backoffStatus = autoscaler.ClusterStateRegistry.BackoffStatusForNodeGroup(ctx, group, time.Now())
		require.True(t, backoffStatus.IsBackedOff)

		synctestutils.MustRunOnceAfter(t, autoscaler, 5*time.Minute)
		require.Len(t, transport.requests, 2, "pending demand must retry after the initial group backoff")
		checkParkingSize(t, group, 1)
		require.Empty(t, group.powerOverrides)
		require.Empty(t, world.history(), "the incumbent retry remains fail-first without fresh fallback")
		pending, err = client.CoreV1().Pods(demand.Namespace).Get(ctx, demand.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Empty(t, pending.Spec.NodeName)
		backoffStatus = autoscaler.ClusterStateRegistry.BackoffStatusForNodeGroup(ctx, group, time.Now())
		require.True(t, backoffStatus.IsBackedOff, "the second rejection must begin the next backoff")
	})
}

func TestProviderOnlyDeallocateNodeUIDAndDeleteControl(t *testing.T) {
	for _, scenario := range []string{"replacement", "stop-failure", "ordinary-delete"} {
		t.Run(scenario, func(t *testing.T) {
			client := fake.NewClientset()
			world := &parkingWorld{states: []string{vmPowerStateRunning}, failStop: scenario == "stop-failure"}
			_, group := newParkingProvider(t, world, client, 0, 1, scenario != "ordinary-delete")
			old := parkingNode(0, "old", true)
			_, err := client.CoreV1().Nodes().Create(t.Context(), old, metav1.CreateOptions{})
			require.NoError(t, err)
			if scenario == "replacement" {
				client.PrependReactor("delete", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
					a := action.(clienttesting.DeleteAction)
					require.Equal(t, old.UID, *a.GetDeleteOptions().Preconditions.UID)
					replacement := parkingNode(0, "replacement", true)
					require.NoError(t, client.Tracker().Update(apiv1.SchemeGroupVersion.WithResource("nodes"), replacement, ""))
					return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, old.Name, errors.New("UID precondition failed"))
				})
			}

			err = group.DeleteNodes(t.Context(), []*apiv1.Node{old})
			switch scenario {
			case "replacement":
				require.ErrorContains(t, err, "UID precondition failed")
				node, err := client.CoreV1().Nodes().Get(t.Context(), old.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.Equal(t, types.UID("replacement"), node.UID)
				require.Empty(t, node.Spec.Taints)
				require.ErrorContains(t, group.IncreaseSize(t.Context(), 1), "still has Node")
				require.Equal(t, []string{"deallocate:0"}, world.history())
			case "stop-failure":
				require.ErrorContains(t, err, "injected stop failure")
				checkParkingSize(t, group, 1)
				_, err := client.CoreV1().Nodes().Get(t.Context(), old.Name, metav1.GetOptions{})
				require.NoError(t, err)
			case "ordinary-delete":
				require.NoError(t, err)
				require.Equal(t, []string{"physical-delete:0"}, world.history())
				_, err := client.CoreV1().Nodes().Get(t.Context(), old.Name, metav1.GetOptions{})
				require.NoError(t, err)
			}
		})
	}
}

func TestProviderOnlyDeallocateTransientNodeDeleteInterruption(t *testing.T) {
	for _, recreate := range []bool{false, true} {
		t.Run(fmt.Sprintf("recreate-provider-%t", recreate), func(t *testing.T) {
			ctx := t.Context()
			client := fake.NewClientset()
			world := &parkingWorld{states: []string{
				vmPowerStateRunning,
				vmPowerStateRunning,
				vmPowerStateRunning,
			}}
			var nodes []*apiv1.Node
			for i := range 3 {
				node := parkingNode(i, fmt.Sprintf("old-%d", i), true)
				node.Annotations = map[string]string{"operator.example/note": "preserve"}
				created, err := client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
				require.NoError(t, err)
				nodes = append(nodes, created)
			}
			demand := catest.BuildTestPod("persistent-demand", 2000, 100, catest.MarkUnschedulable())
			demand.Spec.NodeSelector = map[string]string{"pool": "workers"}
			_, err := client.CoreV1().Pods(demand.Namespace).Create(ctx, demand, metav1.CreateOptions{})
			require.NoError(t, err)

			deleteCalls := 0
			client.PrependReactor("delete", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
				a := action.(clienttesting.DeleteAction)
				preconditions := a.GetDeleteOptions().Preconditions
				require.NotNil(t, preconditions)
				require.NotNil(t, preconditions.UID)
				require.Equal(t, nodes[2].UID, *preconditions.UID)
				deleteCalls++
				world.record(fmt.Sprintf("delete:%s:%s:attempt-%d", a.GetName(), *preconditions.UID, deleteCalls))
				if deleteCalls == 1 {
					return true, nil, apierrors.NewServiceUnavailable("injected transient Node DELETE failure")
				}
				return false, nil, nil
			})

			provider, group := newParkingProvider(t, world, client, 1, 4, true)
			power := &parkingPowerTransport{world: world}
			provider.azureManager.azClient.vmssPowerClient = newTestVMSSPowerClient(t, power)
			subject := nodes[2]

			err = group.DeleteNodes(ctx, []*apiv1.Node{subject})
			require.ErrorContains(t, err, "delete stopped Node node-2")
			require.Equal(t, metav1.StatusReasonServiceUnavailable, apierrors.ReasonForError(errors.Unwrap(err)))
			require.Equal(t, map[string]bool{"vm-identity-2": true}, group.powerOverrides)
			require.Equal(t, []string{
				"deallocate:2",
				"deallocate-complete:2",
				"delete:node-2:old-2:attempt-1",
			}, world.history())
			current, err := client.CoreV1().Nodes().Get(ctx, subject.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, "preserve", current.Annotations["operator.example/note"])
			receipt, err := parseProviderOnlyDeleteReceipt(current.Annotations[providerOnlyDeleteReceiptAnnotation])
			require.NoError(t, err)
			require.Equal(t, providerOnlyDeleteReceipt{
				Version: providerOnlyDeleteReceiptVersion, NodeName: subject.Name, NodeUID: string(subject.UID),
				ProviderID: normalizeProviderID(subject.Spec.ProviderID), VMID: "vm-identity-2",
			}, receipt)
			checkParkingCounts(t, group, 3, 2, 1, 0)
			require.Empty(t, group.powerOverrides)

			if recreate {
				provider, group = newParkingProvider(t, world, client, 1, 4, true)
				provider.azureManager.azClient.vmssPowerClient = newTestVMSSPowerClient(t, power)
				require.Empty(t, group.powerOverrides)
			}
			require.NoError(t, provider.Refresh(ctx), "ordinary refresh must discover the receipt without the historical subject")
			require.Equal(t, 2, deleteCalls)
			_, err = client.CoreV1().Nodes().Get(ctx, subject.Name, metav1.GetOptions{})
			require.True(t, apierrors.IsNotFound(err))
			require.Len(t, power.history(), 1, "recovery must not repeat Deallocate")
			checkParkingCounts(t, group, 3, 2, 1, 0)

			pending, err := client.CoreV1().Pods(demand.Namespace).Get(ctx, demand.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Empty(t, pending.Spec.NodeName)
			require.NoError(t, group.IncreaseSize(ctx, 1))
			require.Len(t, power.history(), 2)
			require.Equal(t, []string{
				"deallocate:2",
				"deallocate-complete:2",
				"delete:node-2:old-2:attempt-1",
				"delete:node-2:old-2:attempt-2",
				"start:2",
				"start-complete:2",
			}, world.history())
			checkParkingCounts(t, group, 3, 3, 0, 0)
		})
	}
}

func TestProviderOnlyDeallocateReplacementAfterStopInterruption(t *testing.T) {
	ctx := t.Context()
	client := fake.NewClientset()
	world := &parkingWorld{states: []string{
		vmPowerStateRunning,
		vmPowerStateRunning,
		vmPowerStateRunning,
	}}
	var nodes []*apiv1.Node
	for i := range 3 {
		node := parkingNode(i, fmt.Sprintf("old-%d", i), true)
		created, err := client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
		require.NoError(t, err)
		nodes = append(nodes, created)
	}
	subject := nodes[2]
	replacement := parkingNode(2, "replacement-2", true)
	deleteCalls := 0
	client.PrependReactor("delete", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		a := action.(clienttesting.DeleteAction)
		deleteCalls++
		preconditions := a.GetDeleteOptions().Preconditions
		require.NotNil(t, preconditions, "every recovery DELETE must remain UID-safe")
		require.NotNil(t, preconditions.UID, "every recovery DELETE must remain UID-safe")
		require.Equal(t, subject.UID, *preconditions.UID, "recovery must never adopt the replacement UID")
		world.record(fmt.Sprintf("delete:%s:%s:attempt-%d", a.GetName(), *preconditions.UID, deleteCalls))
		if deleteCalls == 1 {
			original, err := client.Tracker().Get(apiv1.SchemeGroupVersion.WithResource("nodes"), "", a.GetName())
			require.NoError(t, err)
			replacement.Annotations = make(map[string]string, len(original.(*apiv1.Node).Annotations))
			for key, value := range original.(*apiv1.Node).Annotations {
				replacement.Annotations[key] = value
			}
			require.NoError(t, client.Tracker().Update(
				apiv1.SchemeGroupVersion.WithResource("nodes"),
				replacement,
				"",
			))
			world.record("replace:node-2:replacement-2")
		}
		current, err := client.Tracker().Get(apiv1.SchemeGroupVersion.WithResource("nodes"), "", a.GetName())
		require.NoError(t, err)
		if current.(*apiv1.Node).UID != *preconditions.UID {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Resource: "nodes"},
				a.GetName(),
				errors.New("UID precondition failed"),
			)
		}
		return false, nil, nil
	})

	provider, group := newParkingProvider(t, world, client, 1, 4, true)
	power := &parkingPowerTransport{world: world}
	provider.azureManager.azClient.vmssPowerClient = newTestVMSSPowerClient(t, power)

	err := group.DeleteNodes(ctx, []*apiv1.Node{subject})
	require.ErrorContains(t, err, "UID precondition failed")
	require.Equal(t, metav1.StatusReasonConflict, apierrors.ReasonForError(errors.Unwrap(err)))
	require.Equal(t, map[string]bool{"vm-identity-2": true}, group.powerOverrides)
	require.Equal(t, []string{
		"deallocate:2",
		"deallocate-complete:2",
		"delete:node-2:old-2:attempt-1",
		"replace:node-2:replacement-2",
	}, world.history(), "the replacement appears only after the original Azure stop completed")
	require.Equal(t, vmPowerStateDeallocated, ptr.Deref(world.vms()[2].Properties.InstanceView.Statuses[0].Code, ""))
	require.Equal(t, provisioningStateSucceeded, ptr.Deref(world.vms()[2].Properties.ProvisioningState, ""))
	require.Equal(t, "vm-identity-2", ptr.Deref(world.vms()[2].Properties.VMID, ""))
	checkParkingCounts(t, group, 3, 2, 1, 0)
	require.Empty(t, group.powerOverrides)

	current, err := client.CoreV1().Nodes().Get(ctx, subject.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, replacement.UID, current.UID)
	require.Empty(t, current.Spec.Taints)
	replacementAnnotations := current.Annotations
	require.Contains(t, replacementAnnotations, providerOnlyDeleteReceiptAnnotation)
	require.NoError(t, provider.Refresh(ctx))
	require.ErrorContains(t, group.IncreaseSize(ctx, 1), "still has Node node-2; refusing reuse")
	require.Equal(t, 1, deleteCalls)
	require.Len(t, power.history(), 1, "recovery must issue neither another Deallocate nor a Start")

	recreatedProvider, recreatedGroup := newParkingProvider(t, world, client, 1, 4, true)
	recreatedProvider.azureManager.azClient.vmssPowerClient = newTestVMSSPowerClient(t, power)
	require.Empty(t, recreatedGroup.powerOverrides)
	checkParkingCounts(t, recreatedGroup, 3, 2, 1, 0)
	require.NoError(t, recreatedProvider.Refresh(ctx))
	require.ErrorContains(t, recreatedGroup.IncreaseSize(ctx, 1), "still has Node node-2; refusing reuse")
	require.Equal(t, 1, deleteCalls)
	require.Len(t, power.history(), 1)

	err = client.CoreV1().Nodes().Delete(ctx, subject.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: ptr.To(subject.UID)},
	})
	require.ErrorContains(t, err, "UID precondition failed")
	require.Equal(t, 2, deleteCalls)
	current, err = client.CoreV1().Nodes().Get(ctx, subject.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, replacement.UID, current.UID)
	require.Empty(t, current.Spec.Taints)
	require.Equal(t, replacementAnnotations, current.Annotations)
	require.Len(t, power.history(), 1)
	require.Equal(t, []string{
		"deallocate:2",
		"deallocate-complete:2",
		"delete:node-2:old-2:attempt-1",
		"replace:node-2:replacement-2",
		"delete:node-2:old-2:attempt-2",
	}, world.history())
}

func TestProviderOnlyDeallocateReceiptVMIncarnationMismatch(t *testing.T) {
	ctx := t.Context()
	client := fake.NewClientset()
	world := &parkingWorld{states: []string{
		vmPowerStateRunning,
		vmPowerStateRunning,
		vmPowerStateRunning,
	}}
	var nodes []*apiv1.Node
	for i := range 3 {
		created, err := client.CoreV1().Nodes().Create(
			ctx,
			parkingNode(i, fmt.Sprintf("old-%d", i), true),
			metav1.CreateOptions{},
		)
		require.NoError(t, err)
		nodes = append(nodes, created)
	}
	deleteCalls := 0
	client.PrependReactor("delete", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		a := action.(clienttesting.DeleteAction)
		deleteCalls++
		world.record(fmt.Sprintf("delete:%s:attempt-%d", a.GetName(), deleteCalls))
		return true, nil, apierrors.NewServiceUnavailable("injected transient Node DELETE failure")
	})
	provider, _ := newParkingProvider(t, world, client, 1, 4, true)
	power := &parkingPowerTransport{world: world}
	provider.azureManager.azClient.vmssPowerClient = newTestVMSSPowerClient(t, power)
	require.ErrorContains(t, provider.azureManager.getNodeGroups()[0].DeleteNodes(ctx, []*apiv1.Node{nodes[2]}), "delete stopped Node")

	world.mu.Lock()
	world.vmIDs = map[int]string{2: "replacement-vm-identity-2"}
	world.mu.Unlock()
	recreatedProvider, recreatedGroup := newParkingProvider(t, world, client, 1, 4, true)
	recreatedProvider.azureManager.azClient.vmssPowerClient = newTestVMSSPowerClient(t, power)
	require.NoError(t, recreatedProvider.Refresh(ctx))
	require.Equal(t, 1, deleteCalls)
	require.Len(t, power.history(), 1)
	require.ErrorContains(t, recreatedGroup.IncreaseSize(ctx, 1), "still has Node node-2; refusing reuse")
	current, err := client.CoreV1().Nodes().Get(ctx, nodes[2].Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, nodes[2].UID, current.UID)
	receipt, err := parseProviderOnlyDeleteReceipt(current.Annotations[providerOnlyDeleteReceiptAnnotation])
	require.NoError(t, err)
	require.Equal(t, "vm-identity-2", receipt.VMID)
	require.Equal(t, "replacement-vm-identity-2", ptr.Deref(world.vms()[2].Properties.VMID, ""))
	require.Equal(t, []string{
		"deallocate:2",
		"deallocate-complete:2",
		"delete:node-2:attempt-1",
	}, world.history())
}

func TestProviderOnlyDeallocateReceiptBoundaries(t *testing.T) {
	t.Run("receipt-write-failure", func(t *testing.T) {
		for _, committed := range []bool{false, true} {
			t.Run(fmt.Sprintf("committed-%t", committed), func(t *testing.T) {
				ctx := t.Context()
				client := fake.NewClientset()
				node := parkingNode(0, "old", true)
				node.Spec.Unschedulable = true
				node.Annotations = map[string]string{"operator.example/note": "preserve"}
				_, err := client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
				require.NoError(t, err)
				updateCalls := 0
				client.PrependReactor("update", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
					updated := action.(clienttesting.UpdateAction).GetObject().(*apiv1.Node)
					updateCalls++
					if updateCalls == 1 {
						require.Contains(t, updated.Annotations, providerOnlyDeleteReceiptAnnotation)
						if committed {
							require.NoError(t, client.Tracker().Update(
								apiv1.SchemeGroupVersion.WithResource("nodes"),
								updated,
								"",
							))
						}
						return true, nil, apierrors.NewServiceUnavailable("injected receipt write failure")
					}
					return false, nil, nil
				})
				world := &parkingWorld{states: []string{vmPowerStateRunning}}
				_, group := newParkingProvider(t, world, client, 0, 1, true)
				err = group.DeleteNodes(ctx, []*apiv1.Node{node})
				require.ErrorContains(t, err, "record deletion receipt")
				require.Empty(t, world.history(), "receipt failure must prevent Deallocate")
				current, err := client.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.NotContains(t, current.Annotations, providerOnlyDeleteReceiptAnnotation)
				require.Equal(t, "preserve", current.Annotations["operator.example/note"])
				require.True(t, current.Spec.Unschedulable)
				if committed {
					require.Equal(t, 2, updateCalls, "cleanup must remove an uncertain committed receipt")
				} else {
					require.Equal(t, 1, updateCalls)
				}
			})
		}
	})

	t.Run("definite-begin-rejection", func(t *testing.T) {
		ctx := t.Context()
		client := fake.NewClientset()
		node := parkingNode(0, "old", true)
		node.Annotations = map[string]string{"operator.example/note": "preserve"}
		_, err := client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
		require.NoError(t, err)
		world := &parkingWorld{states: []string{vmPowerStateRunning}}
		provider, group := newParkingProvider(t, world, client, 0, 1, true)
		power := &parkingPowerTransport{world: world, rejectDeallocate: true}
		provider.azureManager.azClient.vmssPowerClient = newTestVMSSPowerClient(t, power)

		err = group.DeleteNodes(ctx, []*apiv1.Node{node})
		require.ErrorContains(t, err, "InvalidParameter")
		require.Equal(t, []string{"deallocate-rejected:0"}, world.history())
		require.Len(t, power.history(), 1)
		current, err := client.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.NotContains(t, current.Annotations, providerOnlyDeleteReceiptAnnotation)
		require.Equal(t, "preserve", current.Annotations["operator.example/note"])
		checkParkingCounts(t, group, 1, 1, 0, 0)
	})

	t.Run("ambiguous-begin-transport-failure", func(t *testing.T) {
		ctx := t.Context()
		client := fake.NewClientset()
		node := parkingNode(0, "old", true)
		_, err := client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
		require.NoError(t, err)
		world := &parkingWorld{states: []string{vmPowerStateRunning}}
		provider, group := newParkingProvider(t, world, client, 0, 1, true)
		power := &parkingPowerTransport{
			world: world, failDeallocate: 1, rejectDeallocate: true,
		}
		provider.azureManager.azClient.vmssPowerClient = newTestVMSSPowerClient(t, power)

		err = group.DeleteNodes(ctx, []*apiv1.Node{node})
		require.ErrorContains(t, err, "injected ambiguous Deallocate transport failure")
		require.Len(t, power.history(), 1, "the receipt decision requires a single non-retried Begin attempt")
		require.Equal(t, []string{"deallocate-transport-error:0"}, world.history())
		current, err := client.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Contains(t, current.Annotations, providerOnlyDeleteReceiptAnnotation)
		require.NoError(t, provider.Refresh(ctx))
		require.Len(t, power.history(), 1)
		checkParkingCounts(t, group, 1, 1, 0, 0)
	})

	t.Run("ambiguous-poll-failure", func(t *testing.T) {
		ctx := t.Context()
		client := fake.NewClientset()
		node := parkingNode(0, "old", true)
		_, err := client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
		require.NoError(t, err)
		world := &parkingWorld{states: []string{vmPowerStateRunning}, failStop: true}
		provider, group := newParkingProvider(t, world, client, 0, 1, true)

		err = group.DeleteNodes(ctx, []*apiv1.Node{node})
		require.ErrorContains(t, err, "injected stop failure")
		current, err := client.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Contains(t, current.Annotations, providerOnlyDeleteReceiptAnnotation)
		require.NoError(t, provider.Refresh(ctx))
		require.Equal(t, []string{"deallocate:0"}, world.history())
		checkParkingCounts(t, group, 1, 1, 0, 0)
	})

	t.Run("malformed-receipt", func(t *testing.T) {
		ctx := t.Context()
		client := fake.NewClientset()
		node := parkingNode(0, "old", true)
		node.Annotations = map[string]string{providerOnlyDeleteReceiptAnnotation: "{"}
		_, err := client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
		require.NoError(t, err)
		world := &parkingWorld{states: []string{vmPowerStateDeallocated}}
		provider, _ := newParkingProvider(t, world, client, 0, 1, true)
		require.NoError(t, provider.Refresh(ctx))
		current, err := client.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, node.UID, current.UID)
		require.Empty(t, world.history())

		disabledProvider, _ := newParkingProvider(t, world, client, 0, 1, false)
		require.NoError(t, disabledProvider.Refresh(ctx), "feature-disabled refresh must ignore provider-only receipts")
	})

	t.Run("deletion-finalizers", func(t *testing.T) {
		ctx := t.Context()
		client := fake.NewClientset()
		node := parkingNode(0, "old", true)
		node.Finalizers = []string{"example.test/finalizer"}
		_, err := client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
		require.NoError(t, err)
		world := &parkingWorld{states: []string{vmPowerStateRunning}}
		deleteCalls := 0
		client.PrependReactor("delete", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
			a := action.(clienttesting.DeleteAction)
			deleteCalls++
			current, err := client.Tracker().Get(apiv1.SchemeGroupVersion.WithResource("nodes"), "", a.GetName())
			require.NoError(t, err)
			terminating := current.(*apiv1.Node).DeepCopy()
			now := metav1.Now()
			terminating.DeletionTimestamp = &now
			require.NoError(t, client.Tracker().Update(
				apiv1.SchemeGroupVersion.WithResource("nodes"),
				terminating,
				"",
			))
			return true, nil, nil
		})
		provider, group := newParkingProvider(t, world, client, 0, 1, true)
		power := &parkingPowerTransport{world: world}
		provider.azureManager.azClient.vmssPowerClient = newTestVMSSPowerClient(t, power)

		require.NoError(t, group.DeleteNodes(ctx, []*apiv1.Node{node}))
		require.Equal(t, 1, deleteCalls)
		require.NoError(t, provider.Refresh(ctx))
		require.Equal(t, 1, deleteCalls, "refresh must wait rather than repeat DELETE while finalizers remain")
		current, err := client.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.NotNil(t, current.DeletionTimestamp)
		require.Equal(t, node.Finalizers, current.Finalizers)
		require.Contains(t, current.Annotations, providerOnlyDeleteReceiptAnnotation)

		recreatedProvider, recreatedGroup := newParkingProvider(t, world, client, 0, 1, true)
		recreatedProvider.azureManager.azClient.vmssPowerClient = newTestVMSSPowerClient(t, power)
		require.NoError(t, recreatedProvider.Refresh(ctx))
		require.Equal(t, 1, deleteCalls)
		require.NoError(t, client.Tracker().Delete(apiv1.SchemeGroupVersion.WithResource("nodes"), "", node.Name))
		require.NoError(t, recreatedProvider.Refresh(ctx))
		require.NoError(t, recreatedGroup.IncreaseSize(ctx, 1))
		require.Len(t, power.history(), 2)
		require.Equal(t, []string{
			"deallocate:0",
			"deallocate-complete:0",
			"start:0",
			"start-complete:0",
		}, world.history())
	})
}

func TestProviderOnlyDeallocateIncoming(t *testing.T) {
	infra := integration.SetupInfrastructure(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer synctestutils.TearDown(cancel)
		world := &parkingWorld{
			states: []string{vmPowerStateDeallocated}, failStart: true,
			startPolled: make(chan struct{}), finishStart: make(chan struct{}),
		}
		provider, group := newParkingProvider(t, world, infra.Fakes.KubeClient, 0, 1, true)
		pod := catest.BuildTestPod("demand", 2000, 100, catest.MarkUnschedulable())
		_, err := infra.Fakes.KubeClient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
		require.NoError(t, err)
		autoscaler := parkingAutoscaler(t, ctx, infra, provider)
		done := make(chan error, 1)
		go func() { done <- autoscaler.RunOnce(ctx, time.Now()) }()
		<-world.startPolled
		checkParkingSize(t, group, 1)
		instances, err := group.Nodes(ctx)
		require.NoError(t, err)
		require.Len(t, instances, 1)
		require.Equal(t, cloudprovider.InstanceCreating, instances[0].Status.State)
		has, err := provider.HasInstance(ctx, parkingNode(0, "not-yet-registered", false))
		require.NoError(t, err)
		require.True(t, has)
		close(world.finishStart)
		require.NoError(t, <-done)
		checkParkingSize(t, group, 1)
		require.Equal(t, []string{"start:0"}, world.history())
	})
}

func TestProviderOnlyDeallocateReuseBeforeGrowth(t *testing.T) {
	infra := integration.SetupInfrastructure(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer synctestutils.TearDown(cancel)
		world := &parkingWorld{states: []string{vmPowerStateRunning, vmPowerStateDeallocated}}
		client := infra.Fakes.KubeClient
		provider, group := newParkingProvider(t, world, client, 1, 3, true)
		_, err := client.CoreV1().Nodes().Create(ctx, parkingNode(0, "active", true), metav1.CreateOptions{})
		require.NoError(t, err)
		busy := catest.BuildScheduledTestPod("busy", 3500, 100, "node-0")
		_, err = client.CoreV1().Pods(busy.Namespace).Create(ctx, busy, metav1.CreateOptions{})
		require.NoError(t, err)
		for i := range 2 {
			pod := catest.BuildTestPod(fmt.Sprintf("demand-%d", i), 3000, 100, catest.MarkUnschedulable())
			_, err := client.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
			require.NoError(t, err)
		}
		autoscaler := parkingAutoscaler(t, ctx, infra, provider)
		synctestutils.MustRunOnceAfter(t, autoscaler, time.Second)
		require.Equal(t, []string{"start:1", "grow:3"}, world.history())
		checkParkingSize(t, group, 3)
		require.Equal(t, int64(3), *world.vmss().SKU.Capacity)
	})
}

func TestProviderOnlyDeallocateUnownedNode(t *testing.T) {
	client := fake.NewClientset()
	world := &parkingWorld{states: []string{vmPowerStateRunning}}
	provider, _ := newParkingProvider(t, world, client, 0, 1, true)
	node := parkingNode(0, "unowned", true)
	node.Spec.ProviderID = strings.Replace(node.Spec.ProviderID, "/pool/", "/other-pool/", 1)
	group, err := provider.NodeGroupForNode(t.Context(), node)
	require.NoError(t, err)
	require.Nil(t, group)
	has, err := provider.HasInstance(t.Context(), node)
	require.ErrorIs(t, err, cloudprovider.ErrNotImplemented)
	require.False(t, has)
}

func TestProviderOnlyDeallocateUnsupportedPools(t *testing.T) {
	for _, scenario := range []string{"spot", "low", "flexible", "ephemeral", "aks-managed", "standard", "hosted"} {
		t.Run(scenario, func(t *testing.T) {
			client := fake.NewClientset()
			world := &parkingWorld{states: []string{vmPowerStateDeallocated}}
			provider, group := newParkingProvider(t, world, client, 0, 1, true)
			vmss := world.vmss()
			switch scenario {
			case "spot":
				vmss.Properties.VirtualMachineProfile.Priority = ptr.To(armcompute.VirtualMachinePriorityTypesSpot)
			case "low":
				vmss.Properties.VirtualMachineProfile.Priority = ptr.To(armcompute.VirtualMachinePriorityTypesLow)
			case "flexible":
				vmss.Properties.OrchestrationMode = ptr.To(armcompute.OrchestrationModeFlexible)
			case "ephemeral":
				vmss.Properties.VirtualMachineProfile.StorageProfile.OSDisk.DiffDiskSettings = &armcompute.DiffDiskSettings{Option: ptr.To(armcompute.DiffDiskOptionsLocal)}
			case "aks-managed":
				vmss.Tags["aks-managed-poolName"] = ptr.To("pool")
			case "standard":
				provider.azureManager.config.VMType = "standard"
			case "hosted":
				provider.azureManager.config.HostedSubscriptionID = "hosted"
			}
			provider.azureManager.azureCache.setScaleSet("pool", vmss)
			_, err := group.TargetSize(t.Context())
			require.Error(t, err)
			require.Error(t, group.IncreaseSize(t.Context(), 1))
			require.Empty(t, world.history())
		})
	}
}

func TestProviderOnlyDeallocateUnregisteredCleanup(t *testing.T) {
	for _, failStart := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed-start-%t", failStart), func(t *testing.T) {
			infra := integration.SetupInfrastructure(t)
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer synctestutils.TearDown(cancel)
				world := &parkingWorld{states: []string{vmPowerStateDeallocated}, failStart: failStart}
				client := infra.Fakes.KubeClient
				provider, group := newParkingProvider(t, world, client, 0, 1, true)
				pod := catest.BuildTestPod("demand", 2000, 100, catest.MarkUnschedulable())
				_, err := client.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
				require.NoError(t, err)
				autoscaler := parkingAutoscaler(t, ctx, infra, provider)
				require.False(t, autoscaler.ForceDeleteLongUnregisteredNodes)
				synctestutils.MustRunOnceAfter(t, autoscaler, time.Second)
				require.Equal(t, []string{"start:0"}, world.history())
				require.NoError(t, client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}))
				synctestutils.MustRunOnceAfter(t, autoscaler, time.Second)
				synctestutils.MustRunOnceAfter(t, autoscaler, 3*time.Minute)
				synctestutils.MustRunOnceAfter(t, autoscaler, 11*time.Minute)
				require.Equal(t, []string{"start:0", "physical-delete:0"}, world.history())
				synctestutils.MustRunOnceAfter(t, autoscaler, time.Minute)
				checkParkingSize(t, group, 0)
				require.Empty(t, group.powerOverrides)
			})
		})
	}
}

func TestProviderOnlyDeallocateFailedFreshVM(t *testing.T) {
	for _, power := range []string{vmPowerStateDeallocated, vmPowerStateStopped} {
		t.Run(power, func(t *testing.T) {
			infra := integration.SetupInfrastructure(t)
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer synctestutils.TearDown(cancel)
				world := &parkingWorld{}
				client := infra.Fakes.KubeClient
				provider, group := newParkingProvider(t, world, client, 0, 1, true)
				group.enableFastDeleteOnFailedProvisioning = true
				pod := catest.BuildTestPod("demand", 2000, 100, catest.MarkUnschedulable())
				_, err := client.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
				require.NoError(t, err)
				autoscaler := parkingAutoscaler(t, ctx, infra, provider)
				synctestutils.MustRunOnceAfter(t, autoscaler, time.Second)
				require.Equal(t, []string{"grow:1"}, world.history())
				require.NoError(t, client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}))
				world.mu.Lock()
				world.states[0] = power
				world.provisioning = map[int]string{0: VMProvisioningStateFailed}
				world.mu.Unlock()
				checkParkingSize(t, group, 1)
				instances, err := group.Nodes(ctx)
				require.NoError(t, err)
				require.Len(t, instances, 1)
				require.Equal(t, cloudprovider.InstanceCreating, instances[0].Status.State)
				require.NotNil(t, instances[0].Status.ErrorInfo)
				require.Equal(t, "provisioning-state-failed", instances[0].Status.ErrorInfo.ErrorCode)
				for range 2 {
					synctestutils.MustRunOnceAfter(t, autoscaler, time.Minute)
				}
				require.Equal(t, []string{"grow:1", "physical-delete:0"}, world.history())
				checkParkingSize(t, group, 0)
			})
		})
	}
}
