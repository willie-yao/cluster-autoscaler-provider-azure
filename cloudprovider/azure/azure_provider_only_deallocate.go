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
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	azcorepolicy "github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	apiv1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cluster-autoscaler/pkg/cloudprovider"
)

const (
	providerOnlyDeleteReceiptAnnotation = "cluster-autoscaler.kubernetes.io/azure-provider-only-delete"
	providerOnlyDeleteReceiptVersion    = 1
	providerOnlyParkLockRetryInterval   = 100 * time.Millisecond
)

type providerOnlyDeleteReceipt struct {
	Version    int    `json:"version"`
	NodeName   string `json:"nodeName"`
	NodeUID    string `json:"nodeUID"`
	ProviderID string `json:"providerID"`
	VMID       string `json:"vmID"`
}

func normalizeProviderID(providerID string) string {
	return strings.ToLower(strings.TrimSpace(providerID))
}

func newProviderOnlyDeleteReceipt(node *apiv1.Node, vm *armcompute.VirtualMachineScaleSetVM) (providerOnlyDeleteReceipt, error) {
	if node == nil || node.Name == "" || node.UID == "" || node.Spec.ProviderID == "" {
		return providerOnlyDeleteReceipt{}, fmt.Errorf("incomplete Node identity for provider-only deletion")
	}
	if vm == nil || vm.ID == nil || vm.Properties == nil || vm.Properties.VMID == nil || *vm.Properties.VMID == "" {
		return providerOnlyDeleteReceipt{}, fmt.Errorf("incomplete VM identity for Node %s", node.Name)
	}
	providerID := normalizeProviderID(node.Spec.ProviderID)
	if providerID != normalizeProviderID(azurePrefix+*vm.ID) {
		return providerOnlyDeleteReceipt{}, fmt.Errorf("Node %s provider ID does not match VM %s", node.Name, *vm.ID)
	}
	return providerOnlyDeleteReceipt{
		Version:    providerOnlyDeleteReceiptVersion,
		NodeName:   node.Name,
		NodeUID:    string(node.UID),
		ProviderID: providerID,
		VMID:       *vm.Properties.VMID,
	}, nil
}

func parseProviderOnlyDeleteReceipt(value string) (providerOnlyDeleteReceipt, error) {
	var receipt providerOnlyDeleteReceipt
	if err := json.Unmarshal([]byte(value), &receipt); err != nil {
		return providerOnlyDeleteReceipt{}, fmt.Errorf("invalid provider-only deletion receipt: %w", err)
	}
	if receipt.Version != providerOnlyDeleteReceiptVersion {
		return providerOnlyDeleteReceipt{}, fmt.Errorf("unsupported provider-only deletion receipt version %d", receipt.Version)
	}
	if receipt.NodeName == "" || receipt.NodeUID == "" || receipt.ProviderID == "" || receipt.VMID == "" {
		return providerOnlyDeleteReceipt{}, fmt.Errorf("incomplete provider-only deletion receipt")
	}
	receipt.ProviderID = normalizeProviderID(receipt.ProviderID)
	return receipt, nil
}

func providerOnlyDeleteReceiptValue(receipt providerOnlyDeleteReceipt) (string, error) {
	value, err := json.Marshal(receipt)
	if err != nil {
		return "", fmt.Errorf("marshal provider-only deletion receipt: %w", err)
	}
	return string(value), nil
}

func validateProviderOnlyDeleteReceiptNode(node *apiv1.Node, receipt providerOnlyDeleteReceipt) error {
	if node.Name != receipt.NodeName || string(node.UID) != receipt.NodeUID ||
		normalizeProviderID(node.Spec.ProviderID) != receipt.ProviderID {
		return fmt.Errorf("Node %s does not match provider-only deletion receipt", node.Name)
	}
	value, found := node.Annotations[providerOnlyDeleteReceiptAnnotation]
	if !found {
		return fmt.Errorf("Node %s no longer has provider-only deletion receipt", node.Name)
	}
	current, err := parseProviderOnlyDeleteReceipt(value)
	if err != nil {
		return fmt.Errorf("Node %s: %w", node.Name, err)
	}
	if current != receipt {
		return fmt.Errorf("Node %s provider-only deletion receipt changed", node.Name)
	}
	return nil
}

func isAuthoritativelyParked(vm *armcompute.VirtualMachineScaleSetVM) bool {
	if vm == nil || vm.Properties == nil {
		return false
	}
	state := vmPowerStateUnknown
	if vm.Properties.InstanceView != nil {
		state = vmPowerStateFromStatuses(vm.Properties.InstanceView.Statuses)
	}
	return state == vmPowerStateDeallocated &&
		ptr.Deref(vm.Properties.ProvisioningState, "") == provisioningStateSucceeded
}

func isDefiniteDeallocateRejection(err error) bool {
	var responseError *azcore.ResponseError
	if !errors.As(err, &responseError) {
		return false
	}
	return responseError.StatusCode >= http.StatusBadRequest &&
		responseError.StatusCode < http.StatusInternalServerError &&
		responseError.StatusCode != http.StatusRequestTimeout &&
		responseError.StatusCode != http.StatusConflict &&
		responseError.StatusCode != http.StatusTooManyRequests
}

type vmssPowerClient interface {
	BeginDeallocate(context.Context, string, string, string, *armcompute.VirtualMachineScaleSetVMsClientBeginDeallocateOptions) (*runtime.Poller[armcompute.VirtualMachineScaleSetVMsClientDeallocateResponse], error)
	BeginStart(context.Context, string, string, string, *armcompute.VirtualMachineScaleSetVMsClientBeginStartOptions) (*runtime.Poller[armcompute.VirtualMachineScaleSetVMsClientStartResponse], error)
}

type acceptedStartOperation struct {
	poller     *runtime.Poller[armcompute.VirtualMachineScaleSetVMsClientStartResponse]
	resourceID string
}

type acceptedSyntheticDeallocateOperation struct {
	poller     *runtime.Poller[armcompute.VirtualMachineScaleSetVMsClientDeallocateResponse]
	resourceID string
	vmID       string
	token      uint64
}

func (m *AzureManager) providerOnlyGroup(providerID string) (*ScaleSet, error) {
	for _, group := range m.getNodeGroups() {
		scaleSet, ok := group.(*ScaleSet)
		if !ok {
			continue
		}
		vmss, err := scaleSet.getVMSSFromCache()
		if err != nil {
			return nil, err
		}
		if vmss.ID == nil {
			return nil, fmt.Errorf("VMSS %s has no resource ID", scaleSet.Name)
		}
		prefix := strings.ToLower(azurePrefix + *vmss.ID + "/virtualMachines/")
		id := strings.TrimPrefix(strings.ToLower(providerID), prefix)
		if id != strings.ToLower(providerID) && id != "" && !strings.Contains(id, "/") {
			return scaleSet, nil
		}
	}
	return nil, nil
}

func (cfg *Config) validateProviderOnlyDeallocate() error {
	if cfg.VMType != "vmss" || cfg.EnableVMsAgentPool || cfg.EnableVmssFlexNodes ||
		cfg.HostedSubscriptionID != "" || cfg.HostedResourceGroup != "" || cfg.HostedResourceProxyURL != "" {
		return fmt.Errorf("providerOnlyDeallocate requires non-hosted Uniform VMSS")
	}
	return nil
}

func (s *ScaleSet) validateParking() error {
	if err := s.manager.config.validateProviderOnlyDeallocate(); err != nil {
		return err
	}
	vmss, err := s.getVMSSFromCache()
	if err != nil {
		return err
	}
	p := vmss.Properties
	if p == nil || p.OrchestrationMode == nil || *p.OrchestrationMode != armcompute.OrchestrationModeUniform ||
		p.VirtualMachineProfile == nil {
		return fmt.Errorf("providerOnlyDeallocate requires regular-priority Uniform VMSS %s", s.Name)
	}
	if priority := p.VirtualMachineProfile.Priority; priority != nil && *priority != armcompute.VirtualMachinePriorityTypesRegular {
		return fmt.Errorf("providerOnlyDeallocate requires regular-priority Uniform VMSS %s", s.Name)
	}
	for tag := range vmss.Tags {
		if strings.HasPrefix(strings.ToLower(tag), "aks-managed-") {
			return fmt.Errorf("providerOnlyDeallocate does not support AKS-managed VMSS %s", s.Name)
		}
	}
	disks := p.VirtualMachineProfile.StorageProfile
	if disks == nil || disks.OSDisk == nil || disks.OSDisk.ManagedDisk == nil || disks.OSDisk.DiffDiskSettings != nil {
		return fmt.Errorf("providerOnlyDeallocate requires managed non-ephemeral OS disks for %s", s.Name)
	}
	return nil
}

func (s *ScaleSet) ensureProviderOnlyDeleteReceipt(
	ctx context.Context,
	node *apiv1.Node,
	receipt providerOnlyDeleteReceipt,
) (bool, error) {
	value, err := providerOnlyDeleteReceiptValue(receipt)
	if err != nil {
		return false, err
	}
	existed := false
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := s.manager.kubeClient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.Name != receipt.NodeName || string(current.UID) != receipt.NodeUID ||
			normalizeProviderID(current.Spec.ProviderID) != receipt.ProviderID {
			return fmt.Errorf("Node %s changed identity before provider-only deletion receipt", node.Name)
		}
		if current.Annotations != nil {
			if existing, found := current.Annotations[providerOnlyDeleteReceiptAnnotation]; found {
				parsed, err := parseProviderOnlyDeleteReceipt(existing)
				if err != nil {
					return fmt.Errorf("Node %s: %w", node.Name, err)
				}
				if parsed != receipt {
					return fmt.Errorf("Node %s has a different provider-only deletion receipt", node.Name)
				}
				existed = true
				return nil
			}
		}
		updated := current.DeepCopy()
		if updated.Annotations == nil {
			updated.Annotations = make(map[string]string)
		}
		updated.Annotations[providerOnlyDeleteReceiptAnnotation] = value
		_, err = s.manager.kubeClient.CoreV1().Nodes().Update(ctx, updated, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return false, err
	}
	return existed, nil
}

func (s *ScaleSet) removeProviderOnlyDeleteReceipt(ctx context.Context, receipt providerOnlyDeleteReceipt) error {
	value, err := providerOnlyDeleteReceiptValue(receipt)
	if err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := s.manager.kubeClient.CoreV1().Nodes().Get(ctx, receipt.NodeName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.Name != receipt.NodeName || string(current.UID) != receipt.NodeUID {
			return fmt.Errorf("Node %s changed identity before provider-only deletion receipt cleanup", receipt.NodeName)
		}
		existing, found := current.Annotations[providerOnlyDeleteReceiptAnnotation]
		if !found {
			return nil
		}
		if existing != value {
			return fmt.Errorf("Node %s provider-only deletion receipt changed before cleanup", receipt.NodeName)
		}
		updated := current.DeepCopy()
		delete(updated.Annotations, providerOnlyDeleteReceiptAnnotation)
		_, err = s.manager.kubeClient.CoreV1().Nodes().Update(ctx, updated, metav1.UpdateOptions{})
		return err
	})
}

func (s *ScaleSet) deleteProviderOnlyReceiptNode(ctx context.Context, receipt providerOnlyDeleteReceipt) error {
	current, err := s.manager.kubeClient.CoreV1().Nodes().Get(ctx, receipt.NodeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := validateProviderOnlyDeleteReceiptNode(current, receipt); err != nil {
		return err
	}
	if current.DeletionTimestamp != nil {
		return nil
	}
	err = s.manager.kubeClient.CoreV1().Nodes().Delete(ctx, current.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: ptr.To(current.UID)},
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete stopped Node %s: %w", current.Name, err)
	}
	return nil
}

func (s *ScaleSet) parkingInventory() ([]*armcompute.VirtualMachineScaleSetVM, map[string]bool, map[string]bool, error) {
	return s.parkingInventoryWithList(s.GetScaleSetVms)
}

func (s *ScaleSet) parkingInventoryWithContext(ctx context.Context) ([]*armcompute.VirtualMachineScaleSetVM, map[string]bool, map[string]bool, error) {
	return s.parkingInventoryWithList(func() ([]*armcompute.VirtualMachineScaleSetVM, error) {
		return s.getScaleSetVms(ctx)
	})
}

func (s *ScaleSet) parkingInventoryWithList(
	list func() ([]*armcompute.VirtualMachineScaleSetVM, error),
) ([]*armcompute.VirtualMachineScaleSetVM, map[string]bool, map[string]bool, error) {
	s.powerMutex.Lock()
	defer s.powerMutex.Unlock()
	if err := s.validateParking(); err != nil {
		return nil, nil, nil, err
	}
	vms, err := list()
	if err != nil {
		return nil, nil, nil, err
	}
	parked := make(map[string]bool, len(vms))
	deallocating := make(map[string]bool, len(vms))
	observedVMIDs := make(map[string]bool, len(vms))
	for _, vm := range vms {
		if vm == nil || vm.ID == nil || vm.InstanceID == nil || vm.Properties == nil ||
			vm.Properties.VMID == nil || *vm.Properties.VMID == "" {
			return nil, nil, nil, fmt.Errorf("incomplete VM instance view for %s", s.Name)
		}
		state := vmPowerStateUnknown
		if vm.Properties.InstanceView != nil {
			state = vmPowerStateFromStatuses(vm.Properties.InstanceView.Statuses)
		}
		provisioning := ptr.Deref(vm.Properties.ProvisioningState, "")
		if state == vmPowerStateUnknown && provisioning != VMProvisioningStateCreating &&
			provisioning != VMProvisioningStateFailed && provisioning != VMProvisioningStateDeleting {
			return nil, nil, nil, fmt.Errorf("unknown VM power state for %s", *vm.ID)
		}
		isParked := isAuthoritativelyParked(vm)
		vmID := *vm.Properties.VMID
		observedVMIDs[vmID] = true
		_, cleanupPending := s.syntheticDeallocating[vmID]
		if cleanupPending && isParked {
			delete(s.syntheticDeallocating, vmID)
			if s.powerOverrides == nil {
				s.powerOverrides = make(map[string]bool)
			}
			s.powerOverrides[vmID] = true
			cleanupPending = false
		}
		if override, ok := s.powerOverrides[vmID]; ok {
			if (override && isParked) || (!override && state == vmPowerStateRunning) {
				delete(s.powerOverrides, vmID)
			} else {
				isParked = override
			}
		}
		parked[*vm.InstanceID] = isParked
		deallocating[*vm.InstanceID] = cleanupPending
	}
	for vmID := range s.powerOverrides {
		if !observedVMIDs[vmID] {
			delete(s.powerOverrides, vmID)
		}
	}
	for vmID := range s.syntheticDeallocating {
		if !observedVMIDs[vmID] {
			delete(s.syntheticDeallocating, vmID)
		}
	}
	return vms, parked, deallocating, nil
}

func (s *ScaleSet) providerOnlyNodes() ([]cloudprovider.Instance, error) {
	vms, parked, deallocating, err := s.parkingInventory()
	if err != nil {
		return nil, err
	}
	s.instanceMutex.Lock()
	defer s.instanceMutex.Unlock()
	instances := make([]cloudprovider.Instance, 0, len(vms))
	for _, vm := range vms {
		if parked[*vm.InstanceID] {
			continue
		}
		id, err := convertResourceGroupNameToLower(*vm.ID)
		if err != nil {
			return nil, err
		}
		status := cloudprovider.InstanceStatus{State: cloudprovider.InstanceCreating}
		if observed := s.instanceStatusFromVM(vm); observed != nil {
			status = *observed
		}
		if status.ErrorInfo == nil && status.State != cloudprovider.InstanceDeleting &&
			ptr.Deref(vm.Properties.ProvisioningState, "") != VMProvisioningStateFailed &&
			(vm.Properties.InstanceView == nil || vmPowerStateFromStatuses(vm.Properties.InstanceView.Statuses) != vmPowerStateRunning) {
			status.State = cloudprovider.InstanceCreating
		}
		if deallocating[*vm.InstanceID] {
			status = cloudprovider.InstanceStatus{State: cloudprovider.InstanceDeleting}
		}
		instances = append(instances, cloudprovider.Instance{Id: azurePrefix + id, Status: &status})
	}
	return instances, nil
}

func (s *ScaleSet) providerOnlyTargetSize() (int64, error) {
	_, parked, deallocating, err := s.parkingInventory()
	if err != nil {
		return 0, err
	}
	return s.activeTarget(parked, deallocating)
}

func (s *ScaleSet) activeTarget(parked map[string]bool, deallocating map[string]bool) (int64, error) {
	physical, err := s.getCurSize()
	if err != nil {
		return 0, err.error
	}
	for _, isParked := range parked {
		if isParked {
			physical--
		}
	}
	for instanceID, isDeallocating := range deallocating {
		if isDeallocating && !parked[instanceID] {
			physical--
		}
	}
	if physical < 0 {
		return 0, fmt.Errorf("parked and deallocating inventory exceeds VMSS capacity for %s", s.Name)
	}
	return physical, nil
}

func (s *ScaleSet) setPowerOverride(vmID string, parked bool) {
	s.powerMutex.Lock()
	defer s.powerMutex.Unlock()
	if s.powerOverrides == nil {
		s.powerOverrides = make(map[string]bool)
	}
	s.powerOverrides[vmID] = parked
}

func (s *ScaleSet) isSyntheticDeallocating(vmID string) bool {
	s.powerMutex.Lock()
	defer s.powerMutex.Unlock()
	_, found := s.syntheticDeallocating[vmID]
	return found
}

func (s *ScaleSet) setSyntheticDeallocating(vmID string) uint64 {
	s.powerMutex.Lock()
	defer s.powerMutex.Unlock()
	if s.syntheticDeallocating == nil {
		s.syntheticDeallocating = make(map[string]uint64)
	}
	s.nextSyntheticDeallocation++
	s.syntheticDeallocating[vmID] = s.nextSyntheticDeallocation
	return s.nextSyntheticDeallocation
}

func (s *ScaleSet) clearSyntheticDeallocating(vmID string, token uint64) bool {
	s.powerMutex.Lock()
	defer s.powerMutex.Unlock()
	if s.syntheticDeallocating[vmID] != token {
		return false
	}
	delete(s.syntheticDeallocating, vmID)
	return true
}

func (s *ScaleSet) completeSyntheticDeallocate(vmID string, token uint64) bool {
	s.powerMutex.Lock()
	defer s.powerMutex.Unlock()
	if s.syntheticDeallocating[vmID] != token {
		return false
	}
	delete(s.syntheticDeallocating, vmID)
	if s.powerOverrides == nil {
		s.powerOverrides = make(map[string]bool)
	}
	s.powerOverrides[vmID] = true
	return true
}

func isProviderOnlySyntheticCleanup(nodes []*apiv1.Node) bool {
	if len(nodes) == 0 {
		return false
	}
	for _, node := range nodes {
		if node == nil {
			return false
		}
		reason := node.Annotations[cloudprovider.FakeNodeReasonAnnotation]
		if node.UID != "" || (reason != cloudprovider.FakeNodeUnregistered && reason != cloudprovider.FakeNodeCreateError) {
			return false
		}
	}
	return true
}

func (s *ScaleSet) parkNodes(ctx context.Context, nodes []*apiv1.Node) error {
	return s.parkNodesWithMinimum(ctx, nodes, true)
}

func (s *ScaleSet) parkNodesWithMinimum(ctx context.Context, nodes []*apiv1.Node, enforceMinimum bool) error {
	ctx, cancel := context.WithTimeout(ctx, asyncContextTimeout)
	defer cancel()
	s.parkMutex.Lock()
	defer s.parkMutex.Unlock()
	if s.manager.kubeClient == nil || s.manager.azClient.vmssPowerClient == nil {
		return fmt.Errorf("providerOnlyDeallocate requires Kubernetes and VMSS power clients")
	}
	vms, parked, deallocating, err := s.parkingInventory()
	if err != nil {
		return err
	}
	active, err := s.activeTarget(parked, deallocating)
	if err != nil {
		return err
	}
	byProviderID := make(map[string]*armcompute.VirtualMachineScaleSetVM, len(vms))
	for _, vm := range vms {
		byProviderID[normalizeProviderID(azurePrefix+*vm.ID)] = vm
	}
	type parkingCandidate struct {
		node           *apiv1.Node
		vm             *armcompute.VirtualMachineScaleSetVM
		receipt        providerOnlyDeleteReceipt
		resumeDeletion bool
	}
	candidates := make([]parkingCandidate, 0, len(nodes))
	seen := make(map[string]bool, len(nodes))
	newStops := 0
	for _, node := range nodes {
		vm := byProviderID[normalizeProviderID(node.Spec.ProviderID)]
		if vm == nil || node.UID == "" || seen[*vm.InstanceID] {
			return fmt.Errorf("node %s is not a unique active VM in %s", node.Name, s.Name)
		}
		seen[*vm.InstanceID] = true
		current, err := s.manager.kubeClient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.UID != node.UID || current.Spec.ProviderID != node.Spec.ProviderID {
			return fmt.Errorf("node %s changed identity before parking", node.Name)
		}
		receipt, err := newProviderOnlyDeleteReceipt(current, vm)
		if err != nil {
			return err
		}
		if value, found := current.Annotations[providerOnlyDeleteReceiptAnnotation]; found {
			existing, err := parseProviderOnlyDeleteReceipt(value)
			if err != nil {
				return fmt.Errorf("Node %s: %w", current.Name, err)
			}
			if existing != receipt {
				return fmt.Errorf("Node %s has a different provider-only deletion receipt", current.Name)
			}
			if !isAuthoritativelyParked(vm) {
				return fmt.Errorf("Node %s has unresolved provider-only deletion receipt for running VM %s", current.Name, *vm.ID)
			}
			candidates = append(candidates, parkingCandidate{
				node: current, vm: vm, receipt: receipt, resumeDeletion: true,
			})
			continue
		}
		if parked[*vm.InstanceID] {
			return fmt.Errorf("node %s is not a unique active VM in %s", node.Name, s.Name)
		}
		newStops++
		candidates = append(candidates, parkingCandidate{node: current, vm: vm, receipt: receipt})
	}
	if enforceMinimum && active-int64(newStops) < int64(s.minSize) {
		return fmt.Errorf("parking %d nodes would fall below minimum %d", newStops, s.minSize)
	}
	for _, candidate := range candidates {
		if candidate.resumeDeletion {
			if err := s.deleteProviderOnlyReceiptNode(ctx, candidate.receipt); err != nil {
				return err
			}
			continue
		}
		existed, err := s.ensureProviderOnlyDeleteReceipt(ctx, candidate.node, candidate.receipt)
		if err != nil {
			cleanupErr := s.removeProviderOnlyDeleteReceipt(ctx, candidate.receipt)
			return errors.Join(fmt.Errorf("record deletion receipt for Node %s: %w", candidate.node.Name, err), cleanupErr)
		}
		if existed {
			return fmt.Errorf("Node %s deletion receipt appeared before Deallocate; refusing repeated operation", candidate.node.Name)
		}
		beginCtx := azcorepolicy.WithRetryOptions(ctx, azcorepolicy.RetryOptions{MaxRetries: -1})
		poller, err := s.manager.azClient.vmssPowerClient.BeginDeallocate(
			beginCtx,
			s.manager.config.ResourceGroup,
			s.Name,
			*candidate.vm.InstanceID,
			nil,
		)
		if err != nil {
			var cleanupErr error
			if isDefiniteDeallocateRejection(err) {
				cleanupErr = s.removeProviderOnlyDeleteReceipt(ctx, candidate.receipt)
			}
			return errors.Join(fmt.Errorf("deallocate %s: %w", candidate.node.Name, err), cleanupErr)
		}
		if poller == nil {
			return fmt.Errorf("deallocate %s returned no operation", candidate.node.Name)
		}
		if _, err := poller.PollUntilDone(ctx, nil); err != nil {
			return fmt.Errorf("waiting for deallocate %s: %w", candidate.node.Name, err)
		}
		s.setPowerOverride(candidate.receipt.VMID, true)
		// The stopped kubelet cannot race this deletion with re-registration.
		if err := s.deleteProviderOnlyReceiptNode(ctx, candidate.receipt); err != nil {
			return err
		}
	}
	return nil
}

func (m *AzureManager) reconcileProviderOnlyDeleteReceipts() error {
	if m.kubeClient == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), vmssContextTimeout)
	defer cancel()
	return m.reconcileProviderOnlyDeleteReceiptsWithContext(ctx)
}

func (m *AzureManager) reconcileProviderOnlyDeleteReceiptsWithContext(ctx context.Context) error {
	nodes, err := m.kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list Nodes for provider-only deletion recovery: %w", err)
	}
	byGroup := make(map[*ScaleSet][]providerOnlyDeleteReceipt)
	var errs []error
	for i := range nodes.Items {
		node := &nodes.Items[i]
		value, found := node.Annotations[providerOnlyDeleteReceiptAnnotation]
		if !found {
			continue
		}
		receipt, err := parseProviderOnlyDeleteReceipt(value)
		if err != nil {
			klog.Errorf("Provider-only deletion receipt for Node %s is blocked: %v", node.Name, err)
			continue
		}
		if err := validateProviderOnlyDeleteReceiptNode(node, receipt); err != nil {
			klog.Errorf("Provider-only deletion receipt for Node %s is blocked: %v", node.Name, err)
			continue
		}
		group, err := m.providerOnlyGroup(receipt.ProviderID)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolve deletion receipt for Node %s: %w", node.Name, err))
			continue
		}
		if group == nil {
			klog.Errorf("Provider-only deletion receipt for Node %s is blocked: no matching provider-only VMSS", node.Name)
			continue
		}
		byGroup[group] = append(byGroup[group], receipt)
	}
	for group, receipts := range byGroup {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("provider-only deletion recovery deadline: %w", err))
			break
		}
		if !group.parkMutex.TryLock() {
			klog.V(3).Infof("Deferring provider-only deletion recovery for busy VMSS %s", group.Name)
			continue
		}
		if err := group.reconcileProviderOnlyDeleteReceipts(ctx, receipts); err != nil {
			errs = append(errs, err)
		}
		group.parkMutex.Unlock()
	}
	return errors.Join(errs...)
}

func (s *ScaleSet) reconcileProviderOnlyDeleteReceipts(
	ctx context.Context,
	receipts []providerOnlyDeleteReceipt,
) error {
	vms, _, _, err := s.parkingInventoryWithContext(ctx)
	if err != nil {
		return fmt.Errorf("load VM inventory for provider-only deletion recovery in %s: %w", s.Name, err)
	}
	byProviderID := make(map[string]*armcompute.VirtualMachineScaleSetVM, len(vms))
	for _, vm := range vms {
		byProviderID[normalizeProviderID(azurePrefix+*vm.ID)] = vm
	}
	for _, receipt := range receipts {
		current, err := s.manager.kubeClient.CoreV1().Nodes().Get(ctx, receipt.NodeName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			klog.Errorf(
				"Provider-only deletion retry for Node %s in VMSS %s failed during GET: %v",
				receipt.NodeName,
				s.Name,
				err,
			)
			continue
		}
		if err := validateProviderOnlyDeleteReceiptNode(current, receipt); err != nil {
			klog.Errorf("Provider-only deletion receipt for Node %s is blocked: %v", current.Name, err)
			continue
		}
		if current.DeletionTimestamp != nil {
			klog.V(3).Infof("Waiting for stopped Node %s finalizers before provider-only reuse", current.Name)
			continue
		}
		vm := byProviderID[receipt.ProviderID]
		if vm == nil {
			klog.Errorf("Provider-only deletion receipt for Node %s is blocked: VM is absent from %s", current.Name, s.Name)
			continue
		}
		if ptr.Deref(vm.Properties.VMID, "") != receipt.VMID {
			klog.Errorf("Provider-only deletion receipt for Node %s is blocked: VM incarnation changed", current.Name)
			continue
		}
		if !isAuthoritativelyParked(vm) {
			klog.Errorf("Provider-only deletion receipt for Node %s is blocked: VM is not authoritatively deallocated", current.Name)
			continue
		}
		if err := s.deleteProviderOnlyReceiptNode(ctx, receipt); err != nil {
			klog.Errorf(
				"Provider-only deletion retry for Node %s in VMSS %s failed during DELETE: %v",
				receipt.NodeName,
				s.Name,
				err,
			)
		}
	}
	return nil
}

func (s *ScaleSet) deallocateSyntheticNodes(ctx context.Context, nodes []*apiv1.Node) error {
	submissionCtx, cancel := context.WithTimeout(ctx, vmssContextTimeout)
	defer cancel()
	if s.manager.kubeClient == nil || s.manager.azClient.vmssPowerClient == nil {
		return fmt.Errorf("providerOnlyDeallocate requires Kubernetes and VMSS power clients")
	}
	if err := s.lockParkOperation(submissionCtx); err != nil {
		return fmt.Errorf("waiting to submit provider-only cleanup for %s: %w", s.Name, err)
	}
	accepted := make([]acceptedSyntheticDeallocateOperation, 0, len(nodes))
	defer func() {
		s.parkMutex.Unlock()
		for _, operation := range accepted {
			go s.waitForSyntheticDeallocate(operation)
		}
	}()

	vms, _, _, err := s.parkingInventoryWithContext(submissionCtx)
	if err != nil {
		return err
	}
	byProviderID := make(map[string]*armcompute.VirtualMachineScaleSetVM, len(vms))
	for _, vm := range vms {
		byProviderID[normalizeProviderID(azurePrefix+*vm.ID)] = vm
	}
	realNodes, err := s.manager.kubeClient.CoreV1().Nodes().List(submissionCtx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		vm := byProviderID[normalizeProviderID(node.Spec.ProviderID)]
		if vm == nil || seen[*vm.InstanceID] {
			return fmt.Errorf("synthetic node %s is not a unique VM in %s", node.Name, s.Name)
		}
		seen[*vm.InstanceID] = true
		vmID := *vm.Properties.VMID
		if s.isSyntheticDeallocating(vmID) {
			continue
		}
		if vm.Properties.OSProfile == nil || ptr.Deref(vm.Properties.OSProfile.ComputerName, "") == "" {
			return fmt.Errorf("synthetic cleanup VM %s has no registration name", *vm.ID)
		}
		for _, realNode := range realNodes.Items {
			if strings.EqualFold(realNode.Spec.ProviderID, azurePrefix+*vm.ID) ||
				strings.EqualFold(realNode.Name, *vm.Properties.OSProfile.ComputerName) {
				return fmt.Errorf("synthetic cleanup VM %s still has Node %s", *vm.ID, realNode.Name)
			}
		}
		poller, err := s.manager.azClient.vmssPowerClient.BeginDeallocate(
			submissionCtx,
			s.manager.config.ResourceGroup,
			s.Name,
			*vm.InstanceID,
			nil,
		)
		if err != nil {
			return fmt.Errorf("deallocate synthetic VM %s: %w", *vm.ID, err)
		}
		if poller == nil {
			return fmt.Errorf("deallocate synthetic VM %s returned no operation", *vm.ID)
		}
		token := s.setSyntheticDeallocating(vmID)
		accepted = append(accepted, acceptedSyntheticDeallocateOperation{
			poller: poller, resourceID: *vm.ID, vmID: vmID, token: token,
		})
	}
	return nil
}

func (s *ScaleSet) waitForSyntheticDeallocate(operation acceptedSyntheticDeallocateOperation) {
	ctx, cancel := getContextWithTimeout(asyncContextTimeout)
	defer cancel()

	klog.V(3).Infof("Calling PollUntilDone for synthetic Deallocate(%s)", operation.resourceID)
	_, err := operation.poller.PollUntilDone(ctx, nil)
	s.invalidateInstanceCache()
	if err != nil {
		if isTerminalPowerOperationFailure(err) {
			if !s.clearSyntheticDeallocating(operation.vmID, operation.token) {
				klog.V(3).Infof("Ignoring superseded synthetic Deallocate failure for %s", operation.resourceID)
				return
			}
			klog.Errorf(
				"Synthetic Deallocate operation for %s completed with failure after acceptance: %v; active charge retained for ordinary cleanup retry",
				operation.resourceID,
				err,
			)
		} else {
			klog.Errorf(
				"Failed to observe completion of accepted synthetic Deallocate operation for %s: %v; active charge and deallocating state retained",
				operation.resourceID,
				err,
			)
		}
		return
	}
	if !s.completeSyntheticDeallocate(operation.vmID, operation.token) {
		klog.V(3).Infof("Ignoring superseded synthetic Deallocate completion for %s", operation.resourceID)
		return
	}
	klog.V(3).Infof("PollUntilDone for synthetic Deallocate(%s) success", operation.resourceID)
}

func (s *ScaleSet) increaseWithParked(ctx context.Context, delta int) error {
	submissionCtx, cancel := context.WithTimeout(ctx, vmssContextTimeout)
	defer cancel()
	if delta <= 0 {
		return fmt.Errorf("size increase must be positive")
	}
	if s.manager.kubeClient == nil || s.manager.azClient.vmssPowerClient == nil {
		return fmt.Errorf("providerOnlyDeallocate requires Kubernetes and VMSS power clients")
	}
	if err := s.lockParkOperation(submissionCtx); err != nil {
		return fmt.Errorf("waiting to submit provider-only scale-up for %s: %w", s.Name, err)
	}
	accepted := make([]acceptedStartOperation, 0, delta)
	defer func() {
		s.parkMutex.Unlock()
		for _, operation := range accepted {
			go s.waitForStart(operation)
		}
	}()

	vms, parked, deallocating, err := s.parkingInventoryWithContext(submissionCtx)
	if err != nil {
		return err
	}
	active, err := s.activeTarget(parked, deallocating)
	if err != nil {
		return err
	}
	if active+int64(delta) > int64(s.maxSize) {
		return fmt.Errorf("size increase too large - desired:%d max:%d", active+int64(delta), s.maxSize)
	}
	nodeList, err := s.manager.kubeClient.CoreV1().Nodes().List(submissionCtx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, vm := range vms {
		if delta == 0 || !parked[*vm.InstanceID] {
			continue
		}
		if vm.Properties.OSProfile == nil || ptr.Deref(vm.Properties.OSProfile.ComputerName, "") == "" {
			return fmt.Errorf("parked VM %s has no registration name", *vm.ID)
		}
		for _, node := range nodeList.Items {
			if strings.EqualFold(node.Spec.ProviderID, azurePrefix+*vm.ID) || strings.EqualFold(node.Name, *vm.Properties.OSProfile.ComputerName) {
				return fmt.Errorf("parked VM %s still has Node %s; refusing reuse", *vm.ID, node.Name)
			}
		}
		poller, err := s.manager.azClient.vmssPowerClient.BeginStart(submissionCtx, s.manager.config.ResourceGroup, s.Name, *vm.InstanceID, nil)
		if err != nil {
			return fmt.Errorf("start %s: %w", *vm.ID, err)
		}
		if poller == nil {
			return fmt.Errorf("start %s returned no operation", *vm.ID)
		}
		// Accepted starts consume active bounds even if completion is uncertain.
		s.setPowerOverride(*vm.Properties.VMID, false)
		accepted = append(accepted, acceptedStartOperation{poller: poller, resourceID: *vm.ID})
		delta--
	}
	if delta == 0 {
		return nil
	}
	vmss, err := s.getVMSSFromCache()
	if err != nil {
		return err
	}
	physical, sizeErr := s.getCurSize()
	if sizeErr != nil {
		return sizeErr.error
	}
	return s.createOrUpdateInstances(vmss, physical+int64(delta))
}

func (s *ScaleSet) lockParkOperation(ctx context.Context) error {
	ticker := time.NewTicker(providerOnlyParkLockRetryInterval)
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

func (s *ScaleSet) waitForStart(operation acceptedStartOperation) {
	ctx, cancel := getContextWithTimeout(asyncContextTimeout)
	defer cancel()

	klog.V(3).Infof("Calling PollUntilDone for Start(%s)", operation.resourceID)
	_, err := operation.poller.PollUntilDone(ctx, nil)
	s.invalidateInstanceCache()
	if err != nil {
		if isTerminalPowerOperationFailure(err) {
			klog.Errorf(
				"Start operation for %s completed with failure after acceptance: %v; accepted capacity remains charged pending ordinary instance cleanup",
				operation.resourceID,
				err,
			)
		} else {
			klog.Errorf(
				"Failed to observe completion of accepted Start operation for %s: %v; accepted capacity remains charged",
				operation.resourceID,
				err,
			)
		}
		return
	}
	klog.V(3).Infof("PollUntilDone for Start(%s) success", operation.resourceID)
}

func isTerminalPowerOperationFailure(err error) bool {
	var responseError *azcore.ResponseError
	if !errors.As(err, &responseError) || responseError.RawResponse == nil {
		return false
	}
	return responseError.RawResponse.StatusCode >= http.StatusOK &&
		responseError.RawResponse.StatusCode < http.StatusMultipleChoices
}
