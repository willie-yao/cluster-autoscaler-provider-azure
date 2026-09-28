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
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/policy/retryrepectthrottled"
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
		return providerOnlyDeleteReceipt{}, fmt.Errorf("incomplete node identity for provider-only deletion")
	}
	if vm == nil || vm.ID == nil || vm.Properties == nil || vm.Properties.VMID == nil || *vm.Properties.VMID == "" {
		return providerOnlyDeleteReceipt{}, fmt.Errorf("incomplete VM identity for node %s", node.Name)
	}
	providerID := normalizeProviderID(node.Spec.ProviderID)
	if providerID != normalizeProviderID(azurePrefix+*vm.ID) {
		return providerOnlyDeleteReceipt{}, fmt.Errorf("node %s provider ID does not match VM %s", node.Name, *vm.ID)
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
		return fmt.Errorf("node %s does not match provider-only deletion receipt", node.Name)
	}
	value, found := node.Annotations[providerOnlyDeleteReceiptAnnotation]
	if !found {
		return fmt.Errorf("node %s no longer has provider-only deletion receipt", node.Name)
	}
	current, err := parseProviderOnlyDeleteReceipt(value)
	if err != nil {
		return fmt.Errorf("node %s: %w", node.Name, err)
	}
	if current != receipt {
		return fmt.Errorf("node %s provider-only deletion receipt changed", node.Name)
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

// isDefiniteDeallocateRejection reports whether Azure rejected a Deallocate
// request without running it. A throttled (429) request is not run, and the
// client's throttling policy reports it as ErrTooManyRequest. A timeout (408)
// or conflict (409) may mean an operation is already in progress, so it is not
// treated as a rejection.
func isDefiniteDeallocateRejection(err error) bool {
	if errors.Is(err, retryrepectthrottled.ErrTooManyRequest) {
		return true
	}
	var responseError *azcore.ResponseError
	if !errors.As(err, &responseError) {
		return false
	}
	return responseError.StatusCode >= http.StatusBadRequest &&
		responseError.StatusCode < http.StatusInternalServerError &&
		responseError.StatusCode != http.StatusRequestTimeout &&
		responseError.StatusCode != http.StatusConflict
}

// vmssPowerClient starts and deallocates VMSS instances.
type vmssPowerClient interface {
	BeginDeallocate(context.Context, string, string, string, *armcompute.VirtualMachineScaleSetVMsClientBeginDeallocateOptions) (*runtime.Poller[armcompute.VirtualMachineScaleSetVMsClientDeallocateResponse], error)
	BeginStart(context.Context, string, string, string, *armcompute.VirtualMachineScaleSetVMsClientBeginStartOptions) (*runtime.Poller[armcompute.VirtualMachineScaleSetVMsClientStartResponse], error)
}

type acceptedStartOperation struct {
	poller     *runtime.Poller[armcompute.VirtualMachineScaleSetVMsClientStartResponse]
	resourceID string
}

type acceptedParkOperation struct {
	poller     *runtime.Poller[armcompute.VirtualMachineScaleSetVMsClientDeallocateResponse]
	resourceID string
	receipt    providerOnlyDeleteReceipt
}

type acceptedSyntheticDeallocateOperation struct {
	poller     *runtime.Poller[armcompute.VirtualMachineScaleSetVMsClientDeallocateResponse]
	resourceID string
	vmID       string
	token      uint64
}

// providerOnlyGroup returns the provider-only group that owns providerID, or
// nil. A group whose VMSS isn't in the cache can't own an instance, so it is
// skipped.
func (m *AzureManager) providerOnlyGroup(providerID string) *ScaleSet {
	for _, group := range m.getNodeGroups() {
		scaleSet, ok := group.(*ScaleSet)
		if !ok || !scaleSet.providerOnlyDeallocate() {
			continue
		}
		vmss, err := scaleSet.getVMSSFromCache()
		if err != nil || vmss.ID == nil {
			continue
		}
		prefix := strings.ToLower(azurePrefix + *vmss.ID + "/virtualMachines/")
		id := strings.TrimPrefix(strings.ToLower(providerID), prefix)
		if id != strings.ToLower(providerID) && id != "" && !strings.Contains(id, "/") {
			return scaleSet
		}
	}
	return nil
}

func (cfg *Config) validateProviderOnlyDeallocate() error {
	if cfg.VMType != "vmss" || cfg.EnableVMsAgentPool || cfg.EnableVmssFlexNodes ||
		cfg.HostedSubscriptionID != "" || cfg.HostedResourceGroup != "" || cfg.HostedResourceProxyURL != "" {
		return fmt.Errorf("providerOnlyDeallocate requires non-hosted Uniform VMSS")
	}
	return nil
}

// providerOnlyDeallocate reports whether the group parks VMs on scale-down,
// either from its Deallocate spec or from the global setting.
func (s *ScaleSet) providerOnlyDeallocate() bool {
	return s.deallocate || s.manager.config.ProviderOnlyDeallocate
}

func hasExplicitDeallocatePolicy(specs []string) bool {
	for _, spec := range specs {
		parsed, err := parseAzureNodeGroupSpec(spec, true)
		if err == nil && parsed.policy == scaleDownPolicyDeallocate {
			return true
		}
	}
	return false
}

func (m *AzureManager) providerOnlyDeallocateEnabled() bool {
	if m.config.ProviderOnlyDeallocate {
		return true
	}
	for _, group := range m.getNodeGroups() {
		if scaleSet, ok := group.(*ScaleSet); ok && scaleSet.deallocate {
			return true
		}
	}
	return false
}

// validateParking returns an error unless the group's VMSS can be parked.
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
			return fmt.Errorf("node %s changed identity before provider-only deletion receipt", node.Name)
		}
		if current.Annotations != nil {
			if existing, found := current.Annotations[providerOnlyDeleteReceiptAnnotation]; found {
				parsed, err := parseProviderOnlyDeleteReceipt(existing)
				if err != nil {
					return fmt.Errorf("node %s: %w", node.Name, err)
				}
				if parsed != receipt {
					return fmt.Errorf("node %s has a different provider-only deletion receipt", node.Name)
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
			return fmt.Errorf("node %s changed identity before provider-only deletion receipt cleanup", receipt.NodeName)
		}
		existing, found := current.Annotations[providerOnlyDeleteReceiptAnnotation]
		if !found {
			return nil
		}
		if existing != value {
			return fmt.Errorf("node %s provider-only deletion receipt changed before cleanup", receipt.NodeName)
		}
		updated := current.DeepCopy()
		delete(updated.Annotations, providerOnlyDeleteReceiptAnnotation)
		_, err = s.manager.kubeClient.CoreV1().Nodes().Update(ctx, updated, metav1.UpdateOptions{})
		return err
	})
}

// deleteProviderOnlyReceiptNode deletes the receipt's Node if it still matches
// the receipt. The delete has a UID precondition.
func (s *ScaleSet) deleteProviderOnlyReceiptNode(ctx context.Context, receipt providerOnlyDeleteReceipt) error {
	current, err := s.manager.kubeClient.CoreV1().Nodes().Get(ctx, receipt.NodeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get stopped node %s: %w", receipt.NodeName, err)
	}
	if err := validateProviderOnlyDeleteReceiptNode(current, receipt); err != nil {
		return err
	}
	if current.DeletionTimestamp != nil {
		return nil
	}
	klog.V(3).Infof("Deleting stopped node %s for parked VM %s", current.Name, receipt.ProviderID)
	err = s.manager.kubeClient.CoreV1().Nodes().Delete(ctx, current.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: ptr.To(current.UID)},
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete stopped node %s: %w", current.Name, err)
	}
	return nil
}

// parkingInventory returns the group's VMs and two maps keyed by instance ID:
// parked reports VMs that are deallocated and out of the target, and
// deallocating reports VMs with an accepted Deallocate that hasn't finished.
// Callers that act on the VMs set refresh, which lists the VMs with ctx.
// Otherwise the VMs come from the instance cache, which is read again when it
// expired or its size differs from the VMSS capacity.
func (s *ScaleSet) parkingInventory(ctx context.Context, refresh bool) ([]*armcompute.VirtualMachineScaleSetVM, map[string]bool, map[string]bool, error) {
	if err := s.validateParking(); err != nil {
		return nil, nil, nil, err
	}
	capacity, sizeErr := s.getCurSize()
	if sizeErr != nil {
		return nil, nil, nil, sizeErr.error
	}
	s.instanceMutex.Lock()
	defer s.instanceMutex.Unlock()
	if refresh {
		vms, err := s.getScaleSetVms(ctx)
		if err != nil {
			return nil, nil, nil, err
		}
		s.parkingVMs = vms
	} else if int64(len(s.parkingVMs)) != capacity ||
		!s.lastInstanceRefresh.Add(s.instancesRefreshPeriod).After(time.Now()) {
		if err := s.updateInstanceCache(); err != nil {
			return nil, nil, nil, err
		}
	}
	vms := s.parkingVMs
	s.powerMutex.Lock()
	defer s.powerMutex.Unlock()
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
		deallocating[*vm.InstanceID] = cleanupPending || s.parking[vmID]
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

// providerOnlyNodes returns the group's instances without the parked VMs.
func (s *ScaleSet) providerOnlyNodes() ([]cloudprovider.Instance, error) {
	vms, parked, deallocating, err := s.parkingInventory(context.Background(), false)
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

// providerOnlyTargetSize returns the VMSS capacity without the parked VMs and
// the VMs being deallocated.
func (s *ScaleSet) providerOnlyTargetSize() (int64, error) {
	_, parked, deallocating, err := s.parkingInventory(context.Background(), false)
	if err != nil {
		return 0, err
	}
	return s.activeTarget(parked, deallocating)
}

// activeTarget subtracts the parked and deallocating VMs from the VMSS capacity.
// A VM in both maps is subtracted once.
func (s *ScaleSet) activeTarget(parked map[string]bool, deallocating map[string]bool) (int64, error) {
	capacity, sizeErr := s.getCurSize()
	if sizeErr != nil {
		return 0, sizeErr.error
	}
	var parkedCount, deallocatingCount int64
	for _, isParked := range parked {
		if isParked {
			parkedCount++
		}
	}
	for instanceID, isDeallocating := range deallocating {
		if isDeallocating && !parked[instanceID] {
			deallocatingCount++
		}
	}
	target := capacity - parkedCount - deallocatingCount
	if target < 0 {
		return 0, fmt.Errorf("parked and deallocating inventory exceeds VMSS capacity for %s", s.Name)
	}
	klog.V(4).Infof("Provider-only target for %s: capacity %d, parked %d, deallocating %d, target %d",
		s.Name, capacity, parkedCount, deallocatingCount, target)
	return target, nil
}

func (s *ScaleSet) setPowerOverride(vmID string, parked bool) {
	s.powerMutex.Lock()
	defer s.powerMutex.Unlock()
	if s.powerOverrides == nil {
		s.powerOverrides = make(map[string]bool)
	}
	s.powerOverrides[vmID] = parked
}

func (s *ScaleSet) setParking(vmID string, parking bool) {
	s.powerMutex.Lock()
	defer s.powerMutex.Unlock()
	if !parking {
		delete(s.parking, vmID)
		return
	}
	if s.parking == nil {
		s.parking = make(map[string]bool)
	}
	s.parking[vmID] = true
}

func (s *ScaleSet) isParking(vmID string) bool {
	s.powerMutex.Lock()
	defer s.powerMutex.Unlock()
	return s.parking[vmID]
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

// parkNodesWithMinimum deallocates the VMs of registered nodes and then deletes
// the Node objects. It holds parkMutex only while it checks the nodes and
// submits the Deallocate requests, and waits for them without the lock. A
// submitted VM counts as deallocating until its Node is deleted.
func (s *ScaleSet) parkNodesWithMinimum(ctx context.Context, nodes []*apiv1.Node, enforceMinimum bool) error {
	ctx, cancel := context.WithTimeout(ctx, asyncContextTimeout)
	defer cancel()
	if s.manager.kubeClient == nil || s.manager.azClient.vmssPowerClient == nil {
		return fmt.Errorf("providerOnlyDeallocate requires Kubernetes and VMSS power clients")
	}
	accepted, err := s.submitParking(ctx, nodes, enforceMinimum)
	for _, operation := range accepted {
		if finishErr := s.finishParking(ctx, operation); finishErr != nil {
			if err == nil {
				err = finishErr
			} else {
				err = errors.Join(err, finishErr)
			}
		}
	}
	return err
}

func (s *ScaleSet) submitParking(ctx context.Context, nodes []*apiv1.Node, enforceMinimum bool) ([]acceptedParkOperation, error) {
	if err := s.lockParkOperation(ctx); err != nil {
		return nil, fmt.Errorf("waiting to submit provider-only scale-down for %s: %w", s.Name, err)
	}
	defer s.parkMutex.Unlock()
	vms, parked, deallocating, err := s.parkingInventory(ctx, true)
	if err != nil {
		return nil, err
	}
	active, err := s.activeTarget(parked, deallocating)
	if err != nil {
		return nil, err
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
		if vm == nil || node.UID == "" || seen[*vm.InstanceID] || deallocating[*vm.InstanceID] {
			return nil, fmt.Errorf("node %s is not a unique active VM in %s", node.Name, s.Name)
		}
		seen[*vm.InstanceID] = true
		current, err := s.manager.kubeClient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get node %s before parking: %w", node.Name, err)
		}
		if current.UID != node.UID || current.Spec.ProviderID != node.Spec.ProviderID {
			return nil, fmt.Errorf("node %s changed identity before parking", node.Name)
		}
		receipt, err := newProviderOnlyDeleteReceipt(current, vm)
		if err != nil {
			return nil, err
		}
		if value, found := current.Annotations[providerOnlyDeleteReceiptAnnotation]; found {
			existing, err := parseProviderOnlyDeleteReceipt(value)
			if err != nil {
				return nil, fmt.Errorf("node %s: %w", current.Name, err)
			}
			if existing != receipt {
				return nil, fmt.Errorf("node %s has a different provider-only deletion receipt", current.Name)
			}
			if !isAuthoritativelyParked(vm) {
				return nil, fmt.Errorf("node %s has unresolved provider-only deletion receipt for running VM %s", current.Name, *vm.ID)
			}
			candidates = append(candidates, parkingCandidate{
				node: current, vm: vm, receipt: receipt, resumeDeletion: true,
			})
			continue
		}
		if parked[*vm.InstanceID] {
			return nil, fmt.Errorf("node %s is not a unique active VM in %s", node.Name, s.Name)
		}
		newStops++
		candidates = append(candidates, parkingCandidate{node: current, vm: vm, receipt: receipt})
	}
	if enforceMinimum && active-int64(newStops) < int64(s.minSize) {
		return nil, fmt.Errorf("parking %d nodes would fall below minimum %d", newStops, s.minSize)
	}
	accepted := make([]acceptedParkOperation, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.resumeDeletion {
			if err := s.deleteProviderOnlyReceiptNode(ctx, candidate.receipt); err != nil {
				return accepted, err
			}
			continue
		}
		existed, err := s.ensureProviderOnlyDeleteReceipt(ctx, candidate.node, candidate.receipt)
		if err != nil {
			cleanupErr := s.removeProviderOnlyDeleteReceipt(ctx, candidate.receipt)
			return accepted, errors.Join(fmt.Errorf("record deletion receipt for node %s: %w", candidate.node.Name, err), cleanupErr)
		}
		if existed {
			return accepted, fmt.Errorf("node %s deletion receipt appeared before Deallocate; refusing repeated operation", candidate.node.Name)
		}
		klog.V(3).Infof("Deallocating VM %s to park node %s", *candidate.vm.ID, candidate.node.Name)
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
			return accepted, errors.Join(fmt.Errorf("deallocate %s: %w", candidate.node.Name, err), cleanupErr)
		}
		if poller == nil {
			return accepted, fmt.Errorf("deallocate %s returned no operation", candidate.node.Name)
		}
		s.setParking(candidate.receipt.VMID, true)
		accepted = append(accepted, acceptedParkOperation{
			poller: poller, resourceID: *candidate.vm.ID, receipt: candidate.receipt,
		})
	}
	return accepted, nil
}

// finishParking waits for an accepted park and then deletes the stopped Node.
func (s *ScaleSet) finishParking(ctx context.Context, operation acceptedParkOperation) error {
	defer s.setParking(operation.receipt.VMID, false)
	klog.V(3).Infof("Calling PollUntilDone for park Deallocate(%s)", operation.resourceID)
	_, err := operation.poller.PollUntilDone(ctx, nil)
	s.invalidateInstanceCache()
	if err != nil {
		return fmt.Errorf("waiting for deallocate %s: %w", operation.receipt.NodeName, err)
	}
	s.setPowerOverride(operation.receipt.VMID, true)
	// The stopped kubelet cannot race this deletion with re-registration.
	return s.deleteProviderOnlyReceiptNode(ctx, operation.receipt)
}

// reconcileProviderOnlyDeleteReceipts deletes the Nodes that still have a
// deletion receipt after their VM was parked, e.g., after a restart.
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
			klog.Warningf("Provider-only deletion receipt for node %s is blocked: %v", node.Name, err)
			continue
		}
		if err := validateProviderOnlyDeleteReceiptNode(node, receipt); err != nil {
			klog.Warningf("Provider-only deletion receipt for node %s is blocked: %v", node.Name, err)
			continue
		}
		group := m.providerOnlyGroup(receipt.ProviderID)
		if group == nil {
			klog.Warningf("Provider-only deletion receipt for node %s is blocked: no matching provider-only VMSS", node.Name)
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
	pending := make([]providerOnlyDeleteReceipt, 0, len(receipts))
	for _, receipt := range receipts {
		if s.isParking(receipt.VMID) {
			klog.V(3).Infof("Waiting for the running park of node %s", receipt.NodeName)
			continue
		}
		pending = append(pending, receipt)
	}
	if len(pending) == 0 {
		return nil
	}
	// A cached power state could be older than a restart, so read the VMs again.
	vms, _, _, err := s.parkingInventory(ctx, true)
	if err != nil {
		return fmt.Errorf("load VM inventory for provider-only deletion recovery in %s: %w", s.Name, err)
	}
	byProviderID := make(map[string]*armcompute.VirtualMachineScaleSetVM, len(vms))
	for _, vm := range vms {
		byProviderID[normalizeProviderID(azurePrefix+*vm.ID)] = vm
	}
	for _, receipt := range pending {
		current, err := s.manager.kubeClient.CoreV1().Nodes().Get(ctx, receipt.NodeName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			klog.Errorf(
				"Provider-only deletion retry for node %s in VMSS %s failed during GET: %v",
				receipt.NodeName,
				s.Name,
				err,
			)
			continue
		}
		if err := validateProviderOnlyDeleteReceiptNode(current, receipt); err != nil {
			klog.Warningf("Provider-only deletion receipt for node %s is blocked: %v", current.Name, err)
			continue
		}
		if current.DeletionTimestamp != nil {
			klog.V(3).Infof("Waiting for stopped node %s finalizers before provider-only reuse", current.Name)
			continue
		}
		vm := byProviderID[receipt.ProviderID]
		if vm == nil {
			klog.Warningf("Provider-only deletion receipt for node %s is blocked: VM is absent from %s", current.Name, s.Name)
			continue
		}
		if ptr.Deref(vm.Properties.VMID, "") != receipt.VMID {
			klog.Warningf("Provider-only deletion receipt for node %s is blocked: VM incarnation changed", current.Name)
			continue
		}
		if !isAuthoritativelyParked(vm) {
			klog.Warningf("Provider-only deletion receipt for node %s is blocked: VM is not authoritatively deallocated", current.Name)
			continue
		}
		if err := s.deleteProviderOnlyReceiptNode(ctx, receipt); err != nil {
			klog.Errorf(
				"Provider-only deletion retry for node %s in VMSS %s failed during DELETE: %v",
				receipt.NodeName,
				s.Name,
				err,
			)
		}
	}
	return nil
}

// deallocateSyntheticNodes deallocates the VMs of instances that never
// registered, which the core reports as nodes without a UID. It returns once
// Azure accepts the requests.
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

	vms, _, _, err := s.parkingInventory(submissionCtx, true)
	if err != nil {
		return err
	}
	byProviderID := make(map[string]*armcompute.VirtualMachineScaleSetVM, len(vms))
	for _, vm := range vms {
		byProviderID[normalizeProviderID(azurePrefix+*vm.ID)] = vm
	}
	realNodes, err := s.manager.kubeClient.CoreV1().Nodes().List(submissionCtx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list nodes before provider-only cleanup: %w", err)
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
		klog.V(3).Infof("Deallocating unregistered VM %s", *vm.ID)
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

	// The E2E suite reads this line and the one in waitForStart.
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

// increaseWithParked starts parked VMs first and grows the VMSS capacity only
// for the rest of delta. It returns once Azure accepts the requests.
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

	vms, parked, deallocating, err := s.parkingInventory(submissionCtx, true)
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
		return fmt.Errorf("list nodes before provider-only scale-up: %w", err)
	}
	for _, vm := range vms {
		// A VM whose park is still deleting its Node is not ready for reuse.
		if delta == 0 || !parked[*vm.InstanceID] || deallocating[*vm.InstanceID] {
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
		klog.V(3).Infof("Starting parked VM %s", *vm.ID)
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

// lockParkOperation takes parkMutex, or returns the context error first.
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

// isTerminalPowerOperationFailure reports whether a poller error is Azure's
// final answer. A failed long-running operation ends with a 2xx poll response
// that reports the failure, while other errors mean the result wasn't seen.
func isTerminalPowerOperationFailure(err error) bool {
	var responseError *azcore.ResponseError
	if !errors.As(err, &responseError) {
		return false
	}
	return responseError.StatusCode >= http.StatusOK &&
		responseError.StatusCode < http.StatusMultipleChoices
}
