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
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/policy/retryrepectthrottled"
	"sigs.k8s.io/cluster-autoscaler/pkg/cloudprovider"
)

type vmssPowerClient interface {
	BeginDeallocate(context.Context, string, string, string,
		*armcompute.VirtualMachineScaleSetVMsClientBeginDeallocateOptions,
	) (*runtime.Poller[armcompute.VirtualMachineScaleSetVMsClientDeallocateResponse], error)
	BeginStart(context.Context, string, string, string,
		*armcompute.VirtualMachineScaleSetVMsClientBeginStartOptions,
	) (*runtime.Poller[armcompute.VirtualMachineScaleSetVMsClientStartResponse], error)
}

// Pending operations and overrides are keyed by VMID, not the reusable instance ID.
type suspendedPowerOperation struct {
	node      *apiv1.Node
	park      bool
	polling   bool
	completed bool
	failed    bool
}

type suspendedVMSSState struct {
	parkMutex              sync.Mutex
	powerMutex             sync.Mutex
	inventoryMutex         sync.Mutex
	pendingPower           map[string]suspendedPowerOperation
	powerOverrides         map[string]bool
	parkedVMIDs            map[string]bool
	suspendedVMs           []*armcompute.VirtualMachineScaleSetVM
	suspendedInventoryTime time.Time
	suspendedReconcileTime time.Time
}

type suspendedStartOperation struct {
	poller     *runtime.Poller[armcompute.VirtualMachineScaleSetVMsClientStartResponse]
	vmID       string
	resourceID string
}

type unsupportedDeallocateError struct {
	name   string
	reason error
}

func (e *unsupportedDeallocateError) Error() string {
	return fmt.Sprintf("Deallocate node group %q is not autoscaled: %v", e.name, e.reason)
}

func normalizeProviderID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

func (s *ScaleSet) suspendsParkedNodes() bool {
	return s.deallocate
}

func (m *AzureManager) suspendedModeEnabled() bool {
	if m.config.Deallocate {
		return true
	}
	if m.azureCache == nil {
		return false
	}
	for _, group := range m.getNodeGroups() {
		if s, ok := group.(*ScaleSet); ok && s.suspendsParkedNodes() {
			return true
		}
	}
	return false
}

func (s *ScaleSet) validateSuspendedMode() error {
	cfg := s.manager.config
	hosted := cfg.HostedSubscriptionID != "" || cfg.HostedResourceGroup != "" || cfg.HostedResourceProxyURL != ""
	if cfg.VMType != "vmss" || hosted {
		return fmt.Errorf("suspended mode requires non-hosted Uniform VMSS")
	}
	vmss, err := s.getVMSSFromCache()
	if err != nil {
		return err
	}
	p := vmss.Properties
	if p == nil || p.OrchestrationMode == nil || *p.OrchestrationMode != armcompute.OrchestrationModeUniform {
		return fmt.Errorf("suspended mode requires Uniform VMSS %s", s.Name)
	}
	if p.VirtualMachineProfile == nil {
		return fmt.Errorf("missing VM profile for %s", s.Name)
	}
	if priority := p.VirtualMachineProfile.Priority; priority != nil && *priority != armcompute.VirtualMachinePriorityTypesRegular {
		return fmt.Errorf("suspended mode requires regular-priority VMSS %s", s.Name)
	}
	for tag := range vmss.Tags {
		if strings.HasPrefix(strings.ToLower(tag), "aks-managed-") {
			return fmt.Errorf("suspended mode does not support AKS-managed VMSS %s", s.Name)
		}
	}
	disks := p.VirtualMachineProfile.StorageProfile
	if disks == nil || disks.OSDisk == nil || disks.OSDisk.ManagedDisk == nil || disks.OSDisk.DiffDiskSettings != nil {
		return fmt.Errorf("suspended mode requires managed non-ephemeral OS disks for %s", s.Name)
	}
	return nil
}

func suspendedPowerState(vm *armcompute.VirtualMachineScaleSetVM) string {
	if vm == nil || vm.Properties == nil || vm.Properties.InstanceView == nil {
		return vmPowerStateUnknown
	}
	return vmPowerStateFromStatuses(vm.Properties.InstanceView.Statuses)
}

func isAuthoritativelyParked(vm *armcompute.VirtualMachineScaleSetVM) bool {
	return vm != nil && vm.Properties != nil && suspendedPowerState(vm) == vmPowerStateDeallocated &&
		ptr.Deref(vm.Properties.ProvisioningState, "") == provisioningStateSucceeded
}

func (s *ScaleSet) lockParkOperation(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.parkMutex.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// suspendedInventory never substitutes stale data for a failed mutation-time read.
func (s *ScaleSet) suspendedInventory(ctx context.Context, refresh bool) ([]*armcompute.VirtualMachineScaleSetVM, error) {
	if err := s.validateSuspendedMode(); err != nil {
		return nil, err
	}
	size, err := s.getScaleSetSize()
	if err != nil {
		return nil, err
	}
	s.inventoryMutex.Lock()
	defer s.inventoryMutex.Unlock()
	stale := !s.suspendedInventoryTime.Add(s.instancesRefreshPeriod).After(time.Now())
	if refresh || stale || int64(len(s.suspendedVMs)) != size {
		readCtx, cancel := context.WithTimeout(ctx, vmssContextTimeout)
		defer cancel()
		vms, err := s.manager.azClient.virtualMachineScaleSetVMsClient.ListVMInstanceView(
			readCtx, s.manager.config.ResourceGroup, s.Name)
		if err != nil {
			return nil, fmt.Errorf("read suspended VMSS inventory %s: %w", s.Name, err)
		}
		s.suspendedVMs = vms
		s.suspendedInventoryTime = time.Now()
	}
	s.powerMutex.Lock()
	defer s.powerMutex.Unlock()
	observed := make(map[string]bool, len(s.suspendedVMs))
	instances := make(map[string]bool, len(s.suspendedVMs))
	for _, vm := range s.suspendedVMs {
		if vm == nil || vm.ID == nil || vm.InstanceID == nil || vm.Properties == nil {
			return nil, fmt.Errorf("incomplete VM instance view for %s", s.Name)
		}
		vmID := ptr.Deref(vm.Properties.VMID, "")
		if vmID == "" || *vm.ID == "" || instances[*vm.InstanceID] || observed[vmID] {
			return nil, fmt.Errorf("missing or duplicate VM identity for %s", s.Name)
		}
		instances[*vm.InstanceID], observed[vmID] = true, true
		_, pending := s.pendingPower[vmID]
		_, overridden := s.powerOverrides[vmID]
		provisioning := ptr.Deref(vm.Properties.ProvisioningState, "")
		inFlight := provisioning == VMProvisioningStateCreating || provisioning == VMProvisioningStateDeleting
		if suspendedPowerState(vm) == vmPowerStateUnknown && !pending && !overridden &&
			!inFlight && provisioning != VMProvisioningStateFailed {
			return nil, fmt.Errorf("unknown VM power state for %s", *vm.ID)
		}
		if override, ok := s.powerOverrides[vmID]; ok && !pending {
			if override && isAuthoritativelyParked(vm) || !override && suspendedPowerState(vm) == vmPowerStateRunning {
				delete(s.powerOverrides, vmID)
			}
		}
	}
	for vmID := range s.powerOverrides {
		if !observed[vmID] {
			delete(s.powerOverrides, vmID)
		}
	}
	for vmID := range s.pendingPower {
		if !observed[vmID] {
			delete(s.pendingPower, vmID)
		}
	}
	parked := make(map[string]bool, len(s.suspendedVMs))
	for _, vm := range s.suspendedVMs {
		vmID := *vm.Properties.VMID
		isParked := isAuthoritativelyParked(vm)
		if override, ok := s.powerOverrides[vmID]; ok {
			isParked = override
		}
		if isParked {
			parked[vmID] = true
		}
	}
	s.parkedVMIDs = parked
	return s.suspendedVMs, nil
}

// Bounds use the last validated inventory; operational reads return refresh errors.
func (s *ScaleSet) parkedCount() int {
	s.powerMutex.Lock()
	defer s.powerMutex.Unlock()
	return len(s.parkedVMIDs)
}

func (s *ScaleSet) recordPower(vmID string, operation suspendedPowerOperation) {
	s.powerMutex.Lock()
	defer s.powerMutex.Unlock()
	if s.pendingPower == nil {
		s.pendingPower = make(map[string]suspendedPowerOperation)
	}
	if s.powerOverrides == nil {
		s.powerOverrides = make(map[string]bool)
	}
	s.pendingPower[vmID] = operation
	s.powerOverrides[vmID] = operation.park
	if s.parkedVMIDs == nil {
		s.parkedVMIDs = make(map[string]bool)
	}
	if operation.park {
		s.parkedVMIDs[vmID] = true
	} else {
		delete(s.parkedVMIDs, vmID)
	}
}

func (s *ScaleSet) powerOperation(vmID string) (suspendedPowerOperation, bool) {
	s.powerMutex.Lock()
	defer s.powerMutex.Unlock()
	op, ok := s.pendingPower[vmID]
	return op, ok
}

func (s *ScaleSet) finishPowerPolling(vmID string, err error) {
	s.powerMutex.Lock()
	defer s.powerMutex.Unlock()
	if op, ok := s.pendingPower[vmID]; ok {
		op.polling, op.completed = false, err == nil
		op.failed = isTerminalPowerOperationFailure(err)
		s.pendingPower[vmID] = op
		if op.park && op.failed {
			delete(s.powerOverrides, vmID)
			delete(s.parkedVMIDs, vmID)
		}
	}
}

func (s *ScaleSet) reconcileSuspendedPower(ctx context.Context, vms []*armcompute.VirtualMachineScaleSetVM) error {
	for _, vm := range vms {
		vmID := *vm.Properties.VMID
		op, pending := s.powerOperation(vmID)
		if !pending {
			continue
		}
		if op.park && (op.polling || !op.completed && !isAuthoritativelyParked(vm)) {
			continue
		}
		if op.node != nil {
			if err := s.setNodeSuspended(ctx, op.node, op.park); err != nil {
				return err
			}
		}
		if op.park || !op.polling && suspendedPowerState(vm) == vmPowerStateRunning {
			s.powerMutex.Lock()
			delete(s.pendingPower, vmID)
			s.powerMutex.Unlock()
		}
	}
	return nil
}

func (m *AzureManager) reconcileSuspendedNodes() error {
	ctx, cancel := context.WithTimeout(context.Background(), vmssContextTimeout)
	defer cancel()
	var nodeList *apiv1.NodeList
	listNodes := func() (*apiv1.NodeList, error) {
		if nodeList != nil {
			return nodeList, nil
		}
		if m.kubeClient == nil {
			return nil, fmt.Errorf("suspended mode requires a Kubernetes client")
		}
		var err error
		nodeList, err = m.kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list nodes to reconcile suspended metadata: %w", err)
		}
		return nodeList, nil
	}
	for _, group := range m.getNodeGroups() {
		s, ok := group.(*ScaleSet)
		if !ok || !s.suspendsParkedNodes() {
			continue
		}
		if err := s.reconcileSuspendedNodes(ctx, listNodes); err != nil {
			return fmt.Errorf("reconcile suspended nodes in %s: %w", s.Name, err)
		}
	}
	return nil
}

func (s *ScaleSet) reconcileSuspendedNodes(ctx context.Context, listNodes func() (*apiv1.NodeList, error)) error {
	if err := s.lockParkOperation(ctx); err != nil {
		return err
	}
	defer s.parkMutex.Unlock()
	vms, err := s.suspendedInventory(ctx, false)
	if err != nil {
		return err
	}
	if err := s.reconcileSuspendedPower(ctx, vms); err != nil {
		return err
	}
	if s.suspendedReconcileTime.Add(s.instancesRefreshPeriod).After(time.Now()) {
		return nil
	}
	nodeList, err := listNodes()
	if err != nil {
		return err
	}
	if err := s.reconcileSettledSuspendedNodes(ctx, vms, nodeList); err != nil {
		return err
	}
	s.suspendedReconcileTime = time.Now()
	return nil
}

func (s *ScaleSet) effectivelyParked(vm *armcompute.VirtualMachineScaleSetVM) bool {
	s.powerMutex.Lock()
	defer s.powerMutex.Unlock()
	if override, ok := s.powerOverrides[ptr.Deref(vm.Properties.VMID, "")]; ok {
		return override
	}
	return isAuthoritativelyParked(vm)
}

func (s *ScaleSet) suspendedNodes(ctx context.Context) ([]cloudprovider.Instance, error) {
	vms, err := s.suspendedInventory(ctx, false)
	if err != nil {
		return nil, err
	}
	instances := make([]cloudprovider.Instance, 0, len(vms))
	s.instanceMutex.Lock()
	defer s.instanceMutex.Unlock()
	// Keep the stock physical-deletion overlay; parking never writes it.
	deleting := make(map[string]bool)
	if s.lastInstanceRefresh.Add(s.instancesRefreshPeriod).After(time.Now()) {
		for _, instance := range s.instanceCache {
			if instance.Status != nil && instance.Status.State == cloudprovider.InstanceDeleting {
				deleting[normalizeProviderID(instance.Id)] = true
			}
		}
	}
	for _, vm := range vms {
		resourceID, err := convertResourceGroupNameToLower(*vm.ID)
		if err != nil {
			return nil, err
		}
		instance := cloudprovider.Instance{Id: azurePrefix + resourceID, Status: s.suspendedInstanceStatus(vm)}
		if deleting[normalizeProviderID(instance.Id)] {
			instance.Status = &cloudprovider.InstanceStatus{State: cloudprovider.InstanceDeleting}
		}
		instances = append(instances, instance)
	}
	return instances, nil
}

func (s *ScaleSet) suspendedInstanceStatus(vm *armcompute.VirtualMachineScaleSetVM) *cloudprovider.InstanceStatus {
	status := &cloudprovider.InstanceStatus{State: cloudprovider.InstanceCreating}
	if vm == nil || vm.Properties == nil {
		return status
	}
	if op, pending := s.powerOperation(ptr.Deref(vm.Properties.VMID, "")); pending {
		if op.park && op.completed {
			status.State = cloudprovider.InstanceRunning
		}
		return status
	}
	if s.effectivelyParked(vm) {
		status.State = cloudprovider.InstanceRunning
		return status
	}
	state := suspendedPowerState(vm)
	switch ptr.Deref(vm.Properties.ProvisioningState, "") {
	case VMProvisioningStateDeleting:
		status.State = cloudprovider.InstanceDeleting
	case VMProvisioningStateCreating:
		return status
	case VMProvisioningStateFailed:
		if state == vmPowerStateRunning {
			status.State = cloudprovider.InstanceRunning
		} else if s.enableFastDeleteOnFailedProvisioning &&
			(state == vmPowerStateStopped || state == vmPowerStateDeallocated) {
			status.ErrorInfo = &cloudprovider.InstanceErrorInfo{
				ErrorClass: cloudprovider.OutOfResourcesErrorClass, ErrorCode: "provisioning-state-failed",
				ErrorMessage: "Azure failed to provision a node for this node group",
			}
		}
	default:
		if state == vmPowerStateRunning {
			status.State = cloudprovider.InstanceRunning
		}
	}
	return status
}

// Timeouts and conflicts can hide acceptance; only definite rejections allow retry.
func isDefinitePowerRejection(err error) bool {
	if errors.Is(err, retryrepectthrottled.ErrTooManyRequest) {
		return true
	}
	var responseError *azcore.ResponseError
	if !errors.As(err, &responseError) {
		return false
	}
	code := responseError.StatusCode
	return code >= http.StatusBadRequest && code < http.StatusInternalServerError &&
		code != http.StatusRequestTimeout && code != http.StatusConflict
}

func isTerminalPowerOperationFailure(err error) bool {
	var responseError *azcore.ResponseError
	return errors.As(err, &responseError) &&
		responseError.StatusCode >= http.StatusOK && responseError.StatusCode < http.StatusMultipleChoices
}

func (s *ScaleSet) waitForSuspendedStart(operation suspendedStartOperation) {
	ctx, cancel := context.WithTimeout(context.Background(), asyncContextTimeout)
	defer cancel()
	_, err := operation.poller.PollUntilDone(ctx, nil)
	s.finishPowerPolling(operation.vmID, err)
	s.invalidateSuspendedInventory()
	if err != nil {
		klog.Errorf("Start %s: %v; accepted capacity remains charged until fresh power state is observed", operation.resourceID, err)
	}
}

func (s *ScaleSet) invalidateSuspendedInventory() {
	s.inventoryMutex.Lock()
	defer s.inventoryMutex.Unlock()
	s.suspendedInventoryTime = time.Time{}
}
