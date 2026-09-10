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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/google/uuid"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cluster-autoscaler/pkg/cloudprovider"
)

const (
	scaleDownDelete     = "Delete"
	scaleDownDeallocate = "Deallocate"
)

// ScaleDownPolicies rejects duplicate group IDs before JSON decoding can replace them.
type ScaleDownPolicies map[string]string

// UnmarshalJSON decodes the policy map without accepting ambiguous group IDs.
func (policies *ScaleDownPolicies) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*policies = nil
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('{') {
		return fmt.Errorf("nodeGroupScaleDownPolicies must be an object")
	}
	result := make(ScaleDownPolicies)
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := token.(string)
		if !ok || seen[strings.ToLower(name)] {
			return fmt.Errorf("ambiguous nodeGroupScaleDownPolicies key %q", token)
		}
		seen[strings.ToLower(name)] = true
		var policy string
		if err := decoder.Decode(&policy); err != nil {
			return err
		}
		result[name] = policy
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	*policies = result
	return nil
}

func (cfg *Config) validateScaleDownPolicies() error {
	seen := make(map[string]bool)
	for name, policy := range cfg.NodeGroupScaleDownPolicies {
		key := strings.ToLower(name)
		if name == "" || strings.TrimSpace(name) != name || seen[key] {
			return fmt.Errorf("invalid or ambiguous nodeGroupScaleDownPolicies key %q", name)
		}
		seen[key] = true
		if policy != scaleDownDelete && policy != scaleDownDeallocate {
			return fmt.Errorf("invalid scale-down policy %q for %q: expected Delete or Deallocate", policy, name)
		}
		if policy == scaleDownDeallocate && (cfg.StrictCacheUpdates || !strings.EqualFold(cfg.VMType, "vmss") ||
			cfg.HostedSubscriptionID != "" || cfg.HostedResourceGroup != "" || cfg.HostedResourceProxyURL != "") {
			return fmt.Errorf("Deallocate requires non-hosted VMSS with strictCacheUpdates disabled")
		}
	}
	return nil
}

func (cfg *Config) deallocates(name string) bool {
	for key, policy := range cfg.NodeGroupScaleDownPolicies {
		if strings.EqualFold(key, name) {
			return policy == scaleDownDeallocate
		}
	}
	return false
}

// scaleSetRetention survives replacement of the discovered ScaleSet object.
// Its mutex serializes accepted target changes with snapshots and refreshes.
type scaleSetRetention struct {
	mutex                    sync.Mutex
	initialized              bool
	physical                 int64
	capacityDirty            bool
	instances                map[string]*retentionMember
	capacity                 map[string]retentionCapacity
	capacitySequence         uint64
	capacityCompletedThrough uint64
	lastRefresh              time.Time
	uncertain                error
	viewErr                  error
	published                *cloudprovider.NodeGroupAccountingSnapshot
	failures                 map[string]retentionFailure
	reportedFailures         map[string]bool
	notifying                bool
}

type retentionPhase int

const (
	retentionActive retentionPhase = iota
	retentionRetiring
	retentionRetained
	retentionResuming
)

type retentionInstance struct {
	cloudprovider.Instance
	Phase  retentionPhase
	Resume *resumeAttempt
}

type resumeAttempt struct {
	ID             string
	AcceptedAt     time.Time
	Deadline       time.Time
	StartCompleted bool
	Error          *cloudprovider.InstanceErrorInfo
}

type retentionSnapshot struct {
	TargetSize int
	Instances  []retentionInstance
}

type retentionFailure struct {
	delta int
	info  cloudprovider.InstanceErrorInfo
}

type retentionMember struct {
	instance      retentionInstance
	instanceID    string
	power         string
	provisioning  string
	operation     string
	acceptedAt    time.Time
	stopCompleted bool
	fresh         bool
	deleting      bool
	receipt       *retentionReceipt
	node          *apiv1.Node
	completed     bool
}

type retentionCapacity struct {
	acceptedAt time.Time
	increment  int64
	unassigned int64
	completed  bool
	sequence   uint64
	deadline   time.Time
	failed     bool
}

func (m *AzureManager) retentionForGroup(name string) *scaleSetRetention {
	m.retentionMutex.Lock()
	defer m.retentionMutex.Unlock()
	if m.retentionGroups == nil {
		m.retentionGroups = make(map[string]*scaleSetRetention)
	}
	key := strings.ToLower(name)
	if m.retentionGroups[key] == nil {
		m.retentionGroups[key] = &scaleSetRetention{
			instances: make(map[string]*retentionMember),
			capacity:  make(map[string]retentionCapacity),
			failures:  make(map[string]retentionFailure),
		}
	}
	return m.retentionGroups[key]
}

func (scaleSet *ScaleSet) validateRetentionMode() error {
	cfg := scaleSet.manager.config
	if err := cfg.validateScaleDownPolicies(); err != nil {
		return err
	}
	if !cfg.deallocates(scaleSet.Name) {
		return fmt.Errorf("cannot remove Deallocate policy from a live retention group %q", scaleSet.Name)
	}
	if err := scaleSet.manager.validateRetentionDependencies(); err != nil {
		return err
	}
	vmss, err := scaleSet.getVMSSFromCache()
	if err != nil {
		return err
	}
	if vmss.Properties == nil || vmss.Properties.OrchestrationMode == nil ||
		*vmss.Properties.OrchestrationMode != armcompute.OrchestrationModeUniform {
		return fmt.Errorf("Deallocate requires Uniform VMSS for %q", scaleSet.Name)
	}
	profile := vmss.Properties.VirtualMachineProfile
	if profile == nil || profile.StorageProfile == nil || profile.StorageProfile.OSDisk == nil ||
		profile.StorageProfile.OSDisk.ManagedDisk == nil || profile.StorageProfile.OSDisk.DiffDiskSettings != nil {
		return fmt.Errorf("Deallocate requires a managed, non-ephemeral OS disk for %q", scaleSet.Name)
	}
	if profile.Priority != nil && *profile.Priority != armcompute.VirtualMachinePriorityTypesRegular {
		return fmt.Errorf("Deallocate requires regular-priority VMs for %q", scaleSet.Name)
	}
	if scaleSet.dedicatedHost || vmss.Properties.HostGroup != nil {
		return fmt.Errorf("Deallocate does not support dedicated hosts for %q", scaleSet.Name)
	}
	for key := range vmss.Tags {
		if strings.HasPrefix(strings.ToLower(key), "aks-managed-") {
			return fmt.Errorf("Deallocate does not support AKS-managed VMSS %q", scaleSet.Name)
		}
	}
	if scaleSet.manager.azClient.vmssClientForDelete == nil {
		return fmt.Errorf("async VMSS client is unavailable for Deallocate group %q", scaleSet.Name)
	}
	return nil
}

func (scaleSet *ScaleSet) retentionSnapshot(ctx context.Context) (*retentionSnapshot, error) {
	if scaleSet.retention == nil {
		return nil, nil
	}
	state := scaleSet.retention
	state.mutex.Lock()
	defer state.mutex.Unlock()
	if err := scaleSet.retentionViewError(); err != nil {
		return nil, err
	}
	return state.snapshot(), nil
}

func (state *scaleSetRetention) snapshot() *retentionSnapshot {
	result := &retentionSnapshot{TargetSize: int(state.physical)}
	for _, member := range state.instances {
		instance := member.instance
		if instance.Phase == retentionRetiring || instance.Phase == retentionRetained {
			result.TargetSize--
		}
		if instance.Status != nil {
			status := *instance.Status
			instance.Status = &status
			if status.ErrorInfo != nil {
				info := *status.ErrorInfo
				instance.Status.ErrorInfo = &info
			}
		}
		if instance.Resume != nil {
			attempt := *instance.Resume
			instance.Resume = &attempt
			if attempt.Error != nil {
				info := *attempt.Error
				instance.Resume.Error = &info
			}
		}
		result.Instances = append(result.Instances, instance)
	}
	sort.Slice(result.Instances, func(i, j int) bool { return result.Instances[i].Id < result.Instances[j].Id })
	return result
}

// refreshRetention must be called with the retention mutex held. Observations
// cannot invent operation intent or overwrite a newer accepted transition.
func (scaleSet *ScaleSet) refreshRetention(ctx context.Context, force bool) (err error) {
	state := scaleSet.retention
	defer func() {
		if err != nil {
			state.viewErr = err
		}
	}()
	if err := scaleSet.validateRetentionMode(); err != nil {
		return err
	}
	if state.uncertain != nil {
		return state.uncertain
	}
	vmss, err := scaleSet.getVMSSFromCache()
	if err != nil {
		return err
	}
	vmssSizeMutex.Lock()
	var physical int64 = -1
	if vmss.SKU != nil && vmss.SKU.Capacity != nil {
		physical = *vmss.SKU.Capacity
	}
	vmssSizeMutex.Unlock()
	if physical < 0 {
		return fmt.Errorf("VMSS %q has no valid physical capacity", scaleSet.Name)
	}
	if state.initialized && physical != state.physical && !state.capacityDirty {
		return fmt.Errorf("physical capacity changed outside retention operations for %q", scaleSet.Name)
	}
	if state.initialized && physical > state.physical {
		for _, member := range state.instances {
			if member.deleting {
				return fmt.Errorf("waiting for fresh deletion capacity reconciliation in %q", scaleSet.Name)
			}
		}
		state.physical = physical
	}
	if state.initialized && physical == state.physical {
		state.capacityDirty = false
	}
	if state.initialized && !force && state.lastRefresh.Add(scaleSet.instancesRefreshPeriod).After(time.Now()) {
		return nil
	}
	state.lastRefresh = time.Time{}
	requestCtx, cancel := context.WithTimeout(ctx, vmssContextTimeout)
	defer cancel()
	vms, err := scaleSet.manager.azClient.virtualMachineScaleSetVMsClient.ListVMInstanceView(requestCtx, scaleSet.manager.config.ResourceGroup, scaleSet.Name)
	if err != nil {
		return fmt.Errorf("refresh Deallocate inventory for %q: %w", scaleSet.Name, err)
	}
	members := make(map[string]*retentionMember, len(vms))
	capacity := maps.Clone(state.capacity)
	for _, vm := range vms {
		if vm == nil || vm.ID == nil || vm.InstanceID == nil || *vm.InstanceID == "" || vm.Properties == nil || vm.Properties.ProvisioningState == nil {
			return fmt.Errorf("incomplete Deallocate inventory for %q", scaleSet.Name)
		}
		if strings.Contains(*vm.InstanceID, "/") || !strings.HasSuffix(strings.ToLower(*vm.ID),
			"/virtualmachinescalesets/"+strings.ToLower(scaleSet.Name)+"/virtualmachines/"+strings.ToLower(*vm.InstanceID)) {
			return fmt.Errorf("invalid VMSS instance identity %q", *vm.ID)
		}
		resourceID, err := convertResourceGroupNameToLower(*vm.ID)
		if err != nil {
			return err
		}
		id := azurePrefix + resourceID
		key := strings.ToLower(id)
		if members[key] != nil {
			return fmt.Errorf("duplicate retention instance %q", id)
		}
		power := vmPowerStateUnknown
		if vm.Properties.InstanceView != nil {
			power = vmPowerStateFromStatuses(vm.Properties.InstanceView.Statuses)
		}
		provisioning := *vm.Properties.ProvisioningState
		member := &retentionMember{instanceID: *vm.InstanceID, power: power, provisioning: provisioning}
		if old := state.instances[key]; old != nil {
			*member = *old
			member.power, member.provisioning = power, provisioning
		} else {
			member.instance = retentionInstance{
				Instance: cloudprovider.Instance{Id: id},
				Phase:    retentionActive,
			}
			member.fresh = provisioning == VMProvisioningStateCreating
			if state.initialized {
				for operation, accepted := range capacity {
					if accepted.unassigned > 0 {
						member.fresh = true
						accepted.unassigned--
						capacity[operation] = accepted
						break
					}
				}
			}
		}
		if member.instance.Phase == retentionActive {
			if provisioning == VMProvisioningStateFailed && !member.fresh && !isRunningVmPowerState(power) {
				return fmt.Errorf("failed instance %q has no accepted creation or resume intent", id)
			}
			if power == vmPowerStateDeallocated || power == vmPowerStateDeallocating ||
				(power != vmPowerStateRunning && power != vmPowerStateStarting && provisioning != VMProvisioningStateCreating && provisioning != VMProvisioningStateFailed && provisioning != VMProvisioningStateDeleting) {
				return fmt.Errorf("instance %q has no accepted retention intent for power state %q", id, power)
			}
			member.instance.Status = instanceStatusFromProvisioningStateAndPowerState(id, vm.Properties.ProvisioningState, power, scaleSet.enableFastDeleteOnFailedProvisioning)
			if member.deleting {
				member.instance.Status = &cloudprovider.InstanceStatus{State: cloudprovider.InstanceDeleting}
			}
		} else {
			member.instance.Status = &cloudprovider.InstanceStatus{State: cloudprovider.InstanceRunning}
		}
		if member.instance.Phase == retentionRetiring && member.stopCompleted &&
			power == vmPowerStateDeallocated && provisioning == provisioningStateSucceeded {
			member.instance.Phase = retentionRetained
		}
		members[key] = member
	}
	for key, member := range state.instances {
		if members[key] == nil && !member.deleting {
			return fmt.Errorf("retention instance %q disappeared without an accepted delete", member.instance.Id)
		}
	}
	if !state.initialized {
		if int64(len(members)) != physical {
			return fmt.Errorf("Deallocate group %q has unresolved capacity without accepted operation history", scaleSet.Name)
		}
		state.physical = physical
		state.initialized = true
	}
	retained := int64(0)
	for _, member := range members {
		if member.instance.Phase == retentionRetiring || member.instance.Phase == retentionRetained {
			retained++
		}
	}
	if retained > state.physical {
		return fmt.Errorf("retained inventory exceeds physical capacity for %q", scaleSet.Name)
	}
	state.instances = members
	state.capacity = capacity
	for operation, accepted := range state.capacity {
		if accepted.completed && accepted.unassigned == 0 {
			delete(state.capacity, operation)
		}
	}
	state.lastRefresh = time.Now()
	return nil
}

func definiteRetentionRejection(err error) bool {
	var response *azcore.ResponseError
	return errors.As(err, &response) && response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != http.StatusRequestTimeout
}

func (scaleSet *ScaleSet) quarantineRetention(operation string, err error) error {
	scaleSet.retention.uncertain = fmt.Errorf("%s for Deallocate group %q has an unresolved outcome: %w", operation, scaleSet.Name, err)
	return scaleSet.retention.uncertain
}

func (scaleSet *ScaleSet) increaseRetainedSize(ctx context.Context, delta int) error {
	timeout, err := scaleSet.retentionProvisionTimeout(ctx)
	if err != nil {
		return err
	}
	state := scaleSet.retention
	state.mutex.Lock()
	defer state.mutex.Unlock()
	defer scaleSet.publishRetention()
	if err := scaleSet.retentionViewError(); err != nil {
		return err
	}
	if delta <= 0 {
		return fmt.Errorf("size increase must be positive")
	}
	if err := scaleSet.refreshRetention(ctx, true); err != nil {
		return err
	}
	if target := state.snapshot().TargetSize; delta > scaleSet.maxSize-target {
		return fmt.Errorf("size increase too large: desired %d, max %d", target+delta, scaleSet.maxSize)
	}
	remaining := delta
	for _, instance := range state.snapshot().Instances {
		member := state.instances[strings.ToLower(instance.Id)]
		if remaining == 0 || member.instance.Phase != retentionRetained ||
			member.power != vmPowerStateDeallocated || member.provisioning != provisioningStateSucceeded {
			continue
		}
		if err := scaleSet.holdSuspended(ctx, member); err != nil {
			state.viewErr = err
			return err
		}
		requestCtx, cancel := context.WithTimeout(ctx, vmssContextTimeout)
		poller, err := scaleSet.manager.azClient.vmssClientForDelete.BeginStart(requestCtx, scaleSet.manager.config.ResourceGroup, scaleSet.Name,
			&armcompute.VirtualMachineScaleSetsClientBeginStartOptions{VMInstanceIDs: &armcompute.VirtualMachineScaleSetVMInstanceIDs{InstanceIDs: []*string{ptr.To(member.instanceID)}}})
		cancel()
		if err != nil {
			if !definiteRetentionRejection(err) {
				return scaleSet.quarantineRetention("Start", err)
			}
			klog.Warningf("Start rejected for instance %q in Deallocate group %q: %v", instance.Id, scaleSet.Name, err)
			continue
		}
		if poller == nil {
			return scaleSet.quarantineRetention("Start", fmt.Errorf("missing accepted-operation poller"))
		}
		operation := uuid.NewString()
		member.operation = operation
		member.acceptedAt = time.Now()
		member.instance.Phase = retentionResuming
		member.instance.Resume = &resumeAttempt{ID: operation, AcceptedAt: member.acceptedAt, Deadline: member.acceptedAt.Add(timeout)}
		member.completed = false
		remaining--
		go scaleSet.waitForRetentionStart(poller, strings.ToLower(instance.Id), operation)
	}
	if remaining == 0 {
		return nil
	}
	vmss, err := scaleSet.getVMSSFromCache()
	if err != nil {
		return err
	}
	physical := state.physical + int64(remaining)
	requestCtx, cancel := context.WithTimeout(ctx, vmssContextTimeout)
	defer cancel()
	effective, poller, err := scaleSet.initCreateOrUpdate(requestCtx, vmss, physical)
	if err != nil {
		if !definiteRetentionRejection(err) {
			return scaleSet.quarantineRetention("capacity increase", err)
		}
		return err
	}
	if poller == nil {
		if effective == vmss {
			return scaleSet.quarantineRetention("capacity increase", fmt.Errorf("missing accepted-operation poller"))
		}
		vmssSizeMutex.Lock()
		physical = *effective.SKU.Capacity
		vmssSizeMutex.Unlock()
	}
	if poller != nil {
		operation := uuid.NewString()
		increment := physical - state.physical
		state.capacitySequence++
		now := time.Now()
		state.capacity[operation] = retentionCapacity{acceptedAt: now, increment: increment, unassigned: increment, sequence: state.capacitySequence, deadline: now.Add(timeout)}
		go scaleSet.waitForRetentionCapacity(poller, operation)
	}
	state.physical = physical
	state.capacityDirty = true
	return nil
}

func (scaleSet *ScaleSet) waitForRetentionStart(poller *runtime.Poller[armcompute.VirtualMachineScaleSetsClientStartResponse], key, operation string) {
	ctx, cancel := context.WithTimeout(context.Background(), asyncContextTimeout)
	defer cancel()
	_, err := poller.PollUntilDone(ctx, nil)
	state := scaleSet.retention
	state.mutex.Lock()
	defer state.mutex.Unlock()
	member := state.instances[key]
	if member == nil || member.operation != operation || member.instance.Resume == nil {
		return
	}
	if err != nil {
		member.instance.Resume.Error = &cloudprovider.InstanceErrorInfo{ErrorClass: cloudprovider.OtherErrorClass, ErrorCode: "AzureStartFailed", ErrorMessage: err.Error()}
		state.queueFailure(operation, 1, *member.instance.Resume.Error)
		if !poller.Done() {
			scaleSet.quarantineRetention("accepted Start polling", err)
		}
	} else {
		member.instance.Resume.StartCompleted = true
	}
	state.lastRefresh = time.Time{}
}

func (scaleSet *ScaleSet) waitForRetentionCapacity(poller *runtime.Poller[armcompute.VirtualMachineScaleSetsClientCreateOrUpdateResponse], operation string) {
	ctx, cancel := context.WithTimeout(context.Background(), asyncContextTimeout)
	defer cancel()
	_, err := poller.PollUntilDone(ctx, nil)
	state := scaleSet.retention
	state.mutex.Lock()
	defer state.mutex.Unlock()
	accepted, found := state.capacity[operation]
	if !found || accepted.completed {
		return
	}
	if accepted.sequence < state.capacityCompletedThrough {
		accepted.completed = true
		state.capacity[operation] = accepted
		return
	}
	if err != nil {
		state.queueFailure(operation, int(accepted.increment), cloudprovider.InstanceErrorInfo{ErrorClass: cloudprovider.OtherErrorClass, ErrorCode: "AzureCapacityFailed", ErrorMessage: err.Error()})
		scaleSet.quarantineRetention("accepted capacity increase", err)
		return
	}
	accepted.completed = true
	state.capacity[operation] = accepted
	state.capacityCompletedThrough = accepted.sequence
	state.lastRefresh = time.Time{}
}

func (scaleSet *ScaleSet) deallocateNodes(ctx context.Context, nodes []*apiv1.Node, force bool) error {
	state := scaleSet.retention
	state.mutex.Lock()
	defer state.mutex.Unlock()
	defer scaleSet.publishRetention()
	if err := scaleSet.retentionViewError(); err != nil {
		return err
	}
	if err := scaleSet.refreshRetention(ctx, false); err != nil {
		return err
	}
	if len(nodes) == 0 {
		return fmt.Errorf("Deallocate requires explicit nonempty instance IDs")
	}
	keys := make([]string, 0, len(nodes))
	ids := make([]*string, 0, len(nodes))
	seen := make(map[string]bool)
	freshCleanup := false
	ordinaryRemoval := false
	for _, node := range nodes {
		if node == nil {
			return fmt.Errorf("cannot deallocate a nil node")
		}
		key := strings.ToLower(node.Spec.ProviderID)
		if seen[key] {
			continue
		}
		seen[key] = true
		member := state.instances[key]
		if member == nil {
			return fmt.Errorf("instance %q does not belong to Deallocate group %q", node.Spec.ProviderID, scaleSet.Name)
		}
		if node.Annotations[cloudprovider.FakeNodeReasonAnnotation] != "" {
			if member.instance.Phase != retentionActive || !member.fresh {
				return fmt.Errorf("cleanup cannot delete retained or resumed instance %q", node.Spec.ProviderID)
			}
			freshCleanup = true
		} else if member.instance.Phase == retentionActive && member.fresh &&
			member.instance.Status != nil && member.instance.Status.State == cloudprovider.InstanceCreating && member.instance.Status.ErrorInfo != nil {
			freshCleanup = true
		} else {
			ordinaryRemoval = true
		}
		if member.deleting {
			continue
		}
		if member.instance.Phase == retentionRetiring || member.instance.Phase == retentionRetained {
			continue
		}
		if member.instance.Phase == retentionResuming {
			return fmt.Errorf("instance %q is still resuming", node.Spec.ProviderID)
		}
		keys = append(keys, key)
		ids = append(ids, ptr.To(member.instanceID))
	}
	if len(keys) == 0 {
		return nil
	}
	if freshCleanup && ordinaryRemoval {
		return fmt.Errorf("cannot mix fresh creation cleanup and retention in one batch")
	}
	if !force && state.snapshot().TargetSize-len(keys) < scaleSet.minSize {
		return fmt.Errorf("deallocate batch would fall below min size %d", scaleSet.minSize)
	}
	if freshCleanup {
		return scaleSet.deleteFreshRetentionInstances(ctx, keys, ids)
	}
	for _, key := range keys {
		member := state.instances[key]
		var requested *apiv1.Node
		for _, node := range nodes {
			if strings.EqualFold(node.Spec.ProviderID, member.instance.Id) {
				requested = node
				break
			}
		}
		if err := scaleSet.validateRetirementNode(ctx, member, requested); err != nil {
			return err
		}
	}
	requestCtx, cancel := context.WithTimeout(ctx, vmssContextTimeout)
	defer cancel()
	poller, err := scaleSet.manager.azClient.vmssClientForDelete.BeginDeallocate(requestCtx, scaleSet.manager.config.ResourceGroup, scaleSet.Name,
		&armcompute.VirtualMachineScaleSetsClientBeginDeallocateOptions{VMInstanceIDs: &armcompute.VirtualMachineScaleSetVMInstanceIDs{InstanceIDs: ids}})
	if err != nil {
		if !definiteRetentionRejection(err) {
			return scaleSet.quarantineRetention("Deallocate", err)
		}
		return err
	}
	if poller == nil {
		return scaleSet.quarantineRetention("Deallocate", fmt.Errorf("missing accepted-operation poller"))
	}
	operation := uuid.NewString()
	for _, key := range keys {
		member := state.instances[key]
		member.operation, member.stopCompleted = operation, false
		member.acceptedAt = time.Now()
		member.instance.Phase, member.instance.Resume = retentionRetiring, nil
		member.completed = false
		member.fresh = false
	}
	go scaleSet.waitForRetentionStop(poller, keys, operation)
	for _, key := range keys {
		member := state.instances[key]
		if err := scaleSet.holdSuspended(ctx, member); err != nil {
			state.viewErr = err
			return err
		}
	}
	return nil
}

func (scaleSet *ScaleSet) waitForRetentionStop(poller *runtime.Poller[armcompute.VirtualMachineScaleSetsClientDeallocateResponse], keys []string, operation string) {
	ctx, cancel := context.WithTimeout(context.Background(), asyncContextTimeout)
	defer cancel()
	_, err := poller.PollUntilDone(ctx, nil)
	state := scaleSet.retention
	state.mutex.Lock()
	defer state.mutex.Unlock()
	for _, key := range keys {
		member := state.instances[key]
		if member == nil || member.operation != operation || member.instance.Phase != retentionRetiring {
			continue
		}
		if err != nil {
			scaleSet.quarantineRetention("accepted Deallocate", err)
		} else {
			member.stopCompleted = true
		}
	}
	state.lastRefresh = time.Time{}
}

func (scaleSet *ScaleSet) reconcileRetention(ctx context.Context) error {
	state := scaleSet.retention
	state.mutex.Lock()
	if state.notifying {
		state.mutex.Unlock()
		return fmt.Errorf("Deallocate group %q is publishing failure notifications", scaleSet.Name)
	}
	err := scaleSet.refreshRetention(ctx, true)
	if err == nil {
		err = scaleSet.reconcileRetentionNodes(ctx)
	}
	state.viewErr = err
	failures := state.failures
	state.failures = nil
	state.notifying = len(failures) > 0
	state.mutex.Unlock()
	for _, failure := range failures {
		scaleSet.manager.autoscalerOptions.Processors.ScaleStateNotifier.RegisterFailedScaleUp(ctx, scaleSet, failure.delta, failure.info, time.Now())
	}
	state.mutex.Lock()
	defer state.mutex.Unlock()
	state.notifying = false
	scaleSet.publishRetention()
	return scaleSet.retentionViewError()
}

func (scaleSet *ScaleSet) refreshRetentionReadOnly(ctx context.Context) error {
	state := scaleSet.retention
	state.mutex.Lock()
	defer state.mutex.Unlock()
	err := scaleSet.refreshRetention(ctx, true)
	if err != nil {
		state.viewErr = err
	}
	return err
}

func (scaleSet *ScaleSet) deleteFreshRetentionInstances(ctx context.Context, keys []string, ids []*string) error {
	state := scaleSet.retention
	requestCtx, cancel := context.WithTimeout(ctx, vmssContextTimeout)
	defer cancel()
	poller, err := scaleSet.manager.azClient.vmssClientForDelete.BeginDeleteInstances(requestCtx, scaleSet.manager.config.ResourceGroup, scaleSet.Name,
		armcompute.VirtualMachineScaleSetVMInstanceRequiredIDs{InstanceIDs: ids}, &armcompute.VirtualMachineScaleSetsClientBeginDeleteInstancesOptions{})
	if err != nil {
		if !definiteRetentionRejection(err) {
			return scaleSet.quarantineRetention("fresh instance deletion", err)
		}
		return err
	}
	if poller == nil {
		return scaleSet.quarantineRetention("fresh instance deletion", fmt.Errorf("missing accepted-operation poller"))
	}
	operation := uuid.NewString()
	for _, key := range keys {
		member := state.instances[key]
		member.operation, member.deleting = operation, true
		member.acceptedAt = time.Now()
		member.instance.Status = &cloudprovider.InstanceStatus{State: cloudprovider.InstanceDeleting}
	}
	state.physical -= int64(len(keys))
	state.capacityDirty = true
	go scaleSet.waitForFreshRetentionDelete(poller, keys, operation)
	return nil
}

func (scaleSet *ScaleSet) waitForFreshRetentionDelete(poller *runtime.Poller[armcompute.VirtualMachineScaleSetsClientDeleteInstancesResponse], keys []string, operation string) {
	ctx, cancel := context.WithTimeout(context.Background(), asyncContextTimeout)
	defer cancel()
	_, err := poller.PollUntilDone(ctx, nil)
	state := scaleSet.retention
	state.mutex.Lock()
	defer state.mutex.Unlock()
	for _, key := range keys {
		member := state.instances[key]
		if member == nil || member.operation != operation {
			continue
		}
		if err != nil {
			scaleSet.quarantineRetention("accepted fresh instance deletion", err)
			break
		}
	}
	state.lastRefresh = time.Time{}
}
