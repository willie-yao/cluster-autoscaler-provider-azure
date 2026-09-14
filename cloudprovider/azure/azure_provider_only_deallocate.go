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
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	apiv1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cluster-autoscaler/pkg/cloudprovider"
)

type vmssPowerClient interface {
	BeginDeallocate(context.Context, string, string, string, *armcompute.VirtualMachineScaleSetVMsClientBeginDeallocateOptions) (*runtime.Poller[armcompute.VirtualMachineScaleSetVMsClientDeallocateResponse], error)
	BeginStart(context.Context, string, string, string, *armcompute.VirtualMachineScaleSetVMsClientBeginStartOptions) (*runtime.Poller[armcompute.VirtualMachineScaleSetVMsClientStartResponse], error)
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

func (s *ScaleSet) parkingInventory() ([]*armcompute.VirtualMachineScaleSetVM, map[string]bool, error) {
	s.powerMutex.Lock()
	defer s.powerMutex.Unlock()
	if err := s.validateParking(); err != nil {
		return nil, nil, err
	}
	vms, err := s.GetScaleSetVms()
	if err != nil {
		return nil, nil, err
	}
	parked := make(map[string]bool, len(vms))
	for _, vm := range vms {
		if vm == nil || vm.ID == nil || vm.InstanceID == nil || vm.Properties == nil {
			return nil, nil, fmt.Errorf("incomplete VM instance view for %s", s.Name)
		}
		state := vmPowerStateUnknown
		if vm.Properties.InstanceView != nil {
			state = vmPowerStateFromStatuses(vm.Properties.InstanceView.Statuses)
		}
		provisioning := ptr.Deref(vm.Properties.ProvisioningState, "")
		if state == vmPowerStateUnknown && provisioning != VMProvisioningStateCreating &&
			provisioning != VMProvisioningStateFailed && provisioning != VMProvisioningStateDeleting {
			return nil, nil, fmt.Errorf("unknown VM power state for %s", *vm.ID)
		}
		isParked := state == vmPowerStateDeallocated && provisioning == provisioningStateSucceeded
		id := *vm.InstanceID
		if override, ok := s.powerOverrides[id]; ok {
			if (override && isParked) || (!override && state == vmPowerStateRunning) {
				delete(s.powerOverrides, id)
			} else {
				isParked = override
			}
		}
		parked[id] = isParked
	}
	for id := range s.powerOverrides {
		if _, found := parked[id]; !found {
			delete(s.powerOverrides, id)
		}
	}
	return vms, parked, nil
}

func (s *ScaleSet) providerOnlyNodes() ([]cloudprovider.Instance, error) {
	vms, parked, err := s.parkingInventory()
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
		instances = append(instances, cloudprovider.Instance{Id: azurePrefix + id, Status: &status})
	}
	return instances, nil
}

func (s *ScaleSet) providerOnlyTargetSize() (int64, error) {
	_, parked, err := s.parkingInventory()
	if err != nil {
		return 0, err
	}
	return s.activeTarget(parked)
}

func (s *ScaleSet) activeTarget(parked map[string]bool) (int64, error) {
	physical, err := s.getCurSize()
	if err != nil {
		return 0, err.error
	}
	for _, isParked := range parked {
		if isParked {
			physical--
		}
	}
	if physical < 0 {
		return 0, fmt.Errorf("parked inventory exceeds VMSS capacity for %s", s.Name)
	}
	return physical, nil
}

func (s *ScaleSet) setPowerOverride(id string, parked bool) {
	s.powerMutex.Lock()
	defer s.powerMutex.Unlock()
	if s.powerOverrides == nil {
		s.powerOverrides = make(map[string]bool)
	}
	s.powerOverrides[id] = parked
}

func (s *ScaleSet) parkNodes(ctx context.Context, nodes []*apiv1.Node) error {
	ctx, cancel := context.WithTimeout(ctx, asyncContextTimeout)
	defer cancel()
	s.parkMutex.Lock()
	defer s.parkMutex.Unlock()
	if s.manager.kubeClient == nil || s.manager.azClient.vmssPowerClient == nil {
		return fmt.Errorf("providerOnlyDeallocate requires Kubernetes and VMSS power clients")
	}
	vms, parked, err := s.parkingInventory()
	if err != nil {
		return err
	}
	active, err := s.activeTarget(parked)
	if err != nil {
		return err
	}
	byProviderID := make(map[string]*armcompute.VirtualMachineScaleSetVM, len(vms))
	for _, vm := range vms {
		byProviderID[strings.ToLower(azurePrefix+*vm.ID)] = vm
	}
	if active-int64(len(nodes)) < int64(s.minSize) {
		return fmt.Errorf("parking %d nodes would fall below minimum %d", len(nodes), s.minSize)
	}
	seen := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		vm := byProviderID[strings.ToLower(node.Spec.ProviderID)]
		if vm == nil || node.UID == "" || seen[*vm.InstanceID] || parked[*vm.InstanceID] {
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
	}
	for _, node := range nodes {
		vm := byProviderID[strings.ToLower(node.Spec.ProviderID)]
		poller, err := s.manager.azClient.vmssPowerClient.BeginDeallocate(ctx, s.manager.config.ResourceGroup, s.Name, *vm.InstanceID, nil)
		if err != nil {
			return fmt.Errorf("deallocate %s: %w", node.Name, err)
		}
		if poller == nil {
			return fmt.Errorf("deallocate %s returned no operation", node.Name)
		}
		if _, err := poller.PollUntilDone(ctx, nil); err != nil {
			return fmt.Errorf("waiting for deallocate %s: %w", node.Name, err)
		}
		s.setPowerOverride(*vm.InstanceID, true)
		// The stopped kubelet cannot race this deletion with re-registration.
		err = s.manager.kubeClient.CoreV1().Nodes().Delete(ctx, node.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: ptr.To(node.UID)},
		})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stopped Node %s: %w", node.Name, err)
		}
	}
	return nil
}

func (s *ScaleSet) increaseWithParked(ctx context.Context, delta int) error {
	ctx, cancel := context.WithTimeout(ctx, asyncContextTimeout)
	defer cancel()
	s.parkMutex.Lock()
	defer s.parkMutex.Unlock()
	if delta <= 0 {
		return fmt.Errorf("size increase must be positive")
	}
	if s.manager.kubeClient == nil || s.manager.azClient.vmssPowerClient == nil {
		return fmt.Errorf("providerOnlyDeallocate requires Kubernetes and VMSS power clients")
	}
	vms, parked, err := s.parkingInventory()
	if err != nil {
		return err
	}
	active, err := s.activeTarget(parked)
	if err != nil {
		return err
	}
	if active+int64(delta) > int64(s.maxSize) {
		return fmt.Errorf("size increase too large - desired:%d max:%d", active+int64(delta), s.maxSize)
	}
	nodeList, err := s.manager.kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
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
		poller, err := s.manager.azClient.vmssPowerClient.BeginStart(ctx, s.manager.config.ResourceGroup, s.Name, *vm.InstanceID, nil)
		if err != nil {
			return fmt.Errorf("start %s: %w", *vm.ID, err)
		}
		if poller == nil {
			return fmt.Errorf("start %s returned no operation", *vm.ID)
		}
		// Accepted starts consume active bounds even if completion is uncertain.
		s.setPowerOverride(*vm.InstanceID, false)
		if _, err := poller.PollUntilDone(ctx, nil); err != nil {
			return fmt.Errorf("waiting for start %s: %w", *vm.ID, err)
		}
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
