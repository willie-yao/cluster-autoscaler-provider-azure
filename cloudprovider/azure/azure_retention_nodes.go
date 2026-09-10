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
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	apiv1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/cluster-autoscaler/pkg/cloudprovider"
	"sigs.k8s.io/cluster-autoscaler/pkg/utils/taints"
)

const suspendedReason = "AzureClusterAutoscalerDeallocate"

type retentionReceipt struct {
	name           string
	uid            types.UID
	providerID     string
	taint          apiv1.Taint
	cleanupStarted bool
}

func (m *AzureManager) validateRetentionDependencies() error {
	enabled := false
	for _, policy := range m.config.NodeGroupScaleDownPolicies {
		enabled = enabled || policy == scaleDownDeallocate
	}
	if !enabled {
		return nil
	}
	opts := m.autoscalerOptions
	if opts == nil || opts.KubeClient == nil || opts.InformerFactory == nil ||
		opts.Processors == nil || opts.Processors.NodeGroupConfigProcessor == nil || opts.Processors.ScaleStateNotifier == nil {
		return fmt.Errorf("Deallocate requires the autoscaler Kubernetes client, informer factory, node group configuration processor and scale-state notifier")
	}
	if opts.CordonNodeBeforeTerminate {
		return fmt.Errorf("Deallocate requires --cordon-node-before-terminating=false")
	}
	if opts.NodeGroupDefaults.ZeroOrMaxNodeScaling {
		return fmt.Errorf("Deallocate does not support ZeroOrMaxNodeScaling")
	}
	return nil
}

func (scaleSet *ScaleSet) retentionProvisionTimeout(ctx context.Context) (time.Duration, error) {
	if err := scaleSet.validateRetentionMode(); err != nil {
		return 0, err
	}
	timeout, err := scaleSet.manager.autoscalerOptions.Processors.NodeGroupConfigProcessor.GetMaxNodeProvisionTime(ctx, scaleSet)
	if err != nil {
		return 0, err
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("Deallocate requires positive max node provision time for %q", scaleSet.Name)
	}
	return timeout, nil
}

func (m *AzureManager) reconcileRetentionGroups(ctx context.Context) error {
	var errs []error
	for _, group := range m.getNodeGroups() {
		if scaleSet, ok := group.(*ScaleSet); ok && scaleSet.retention != nil {
			if err := scaleSet.reconcileRetention(ctx); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (m *AzureManager) invalidateRetentionViews(err error) {
	m.retentionMutex.Lock()
	groups := make([]*scaleSetRetention, 0, len(m.retentionGroups))
	for _, group := range m.retentionGroups {
		groups = append(groups, group)
	}
	m.retentionMutex.Unlock()
	for _, group := range groups {
		group.mutex.Lock()
		group.viewErr = err
		group.published = nil
		group.mutex.Unlock()
	}
}

func (state *scaleSetRetention) queueFailure(operation string, delta int, info cloudprovider.InstanceErrorInfo) {
	if state.reportedFailures[operation] {
		return
	}
	if state.reportedFailures == nil {
		state.reportedFailures = make(map[string]bool)
	}
	state.reportedFailures[operation] = true
	if state.failures == nil {
		state.failures = make(map[string]retentionFailure)
	}
	state.failures[operation] = retentionFailure{delta: delta, info: info}
}

func (scaleSet *ScaleSet) retentionViewError() error {
	state := scaleSet.retention
	if err := scaleSet.validateRetentionMode(); err != nil {
		return err
	}
	if state.uncertain != nil {
		return state.uncertain
	}
	if state.viewErr != nil {
		return state.viewErr
	}
	if !state.initialized || len(state.failures) != 0 || state.notifying {
		return fmt.Errorf("Deallocate group %q requires provider Refresh before accounting", scaleSet.Name)
	}
	return nil
}

// SuspendedNodesIncludedInTargetSize reports active-target units for opted-in groups.
func (scaleSet *ScaleSet) SuspendedNodesIncludedInTargetSize() bool {
	return scaleSet.retention == nil
}

// GetNodeGroupAccounting only reads the last coherent provider observation.
func (scaleSet *ScaleSet) GetNodeGroupAccounting(ctx context.Context) (*cloudprovider.NodeGroupAccountingSnapshot, error) {
	if scaleSet.retention == nil {
		return nil, nil
	}
	state := scaleSet.retention
	state.mutex.Lock()
	defer state.mutex.Unlock()
	if err := scaleSet.retentionViewError(); err != nil {
		return nil, err
	}
	if state.published == nil {
		return nil, fmt.Errorf("Deallocate group %q has no published observation", scaleSet.Name)
	}
	result := *state.published
	result.Instances = slices.Clone(result.Instances)
	for i, instance := range result.Instances {
		if instance.Status != nil {
			status := *instance.Status
			result.Instances[i].Status = &status
			if status.ErrorInfo != nil {
				info := *status.ErrorInfo
				result.Instances[i].Status.ErrorInfo = &info
			}
		}
	}
	result.InactiveInstanceIDs = slices.Clone(result.InactiveInstanceIDs)
	result.NodeObservations = make([]*apiv1.Node, len(state.published.NodeObservations))
	for i, node := range state.published.NodeObservations {
		result.NodeObservations[i] = node.DeepCopy()
	}
	return &result, nil
}

// publishRetention is called under the operation lock, after reconciliation or mutation.
func (scaleSet *ScaleSet) publishRetention() {
	state := scaleSet.retention
	if scaleSet.retentionViewError() != nil {
		state.published = nil
		return
	}
	snapshot := state.snapshot()
	view := &cloudprovider.NodeGroupAccountingSnapshot{TargetSize: snapshot.TargetSize}
	for _, instance := range snapshot.Instances {
		member := state.instances[strings.ToLower(instance.Id)]
		if member.node != nil {
			instance.Id = member.node.Spec.ProviderID
		} else if member.receipt != nil {
			instance.Id = member.receipt.providerID
		}
		view.Instances = append(view.Instances, instance.Instance)
		if instance.Phase != retentionActive {
			view.InactiveInstanceIDs = append(view.InactiveInstanceIDs, instance.Id)
		}
		if instance.Phase == retentionResuming {
			if instance.Resume.Error == nil {
				view.UpcomingInactiveNodes++
			} else {
				view.UnavailableTargetNodes++
			}
		}
		if member.node != nil {
			node := member.node.DeepCopy()
			view.NodeObservations = append(view.NodeObservations, node)
		}
	}
	for _, capacity := range state.capacity {
		if capacity.failed {
			view.UnavailableTargetNodes += int(capacity.unassigned)
		}
	}
	state.published = view
}

func conditionSuspended(node *apiv1.Node) *apiv1.NodeCondition {
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == "Suspended" {
			return &node.Status.Conditions[i]
		}
	}
	return nil
}

func freshResumeReady(node *apiv1.Node, member *retentionMember) bool {
	if member.instance.Resume == nil || !member.instance.Resume.StartCompleted || member.instance.Resume.Error != nil {
		return false
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == apiv1.NodeReady {
			return condition.Status == apiv1.ConditionTrue && condition.LastHeartbeatTime.Time.After(member.instance.Resume.AcceptedAt)
		}
	}
	return false
}

func (receipt *retentionReceipt) checkIdentity(node *apiv1.Node) error {
	if node == nil || node.Name != receipt.name || node.UID != receipt.uid || node.Spec.ProviderID != receipt.providerID {
		return fmt.Errorf("retained Node identity changed for %q", receipt.name)
	}
	return nil
}

func (receipt *retentionReceipt) checkTaint(node *apiv1.Node) error {
	count := 0
	for _, taint := range node.Spec.Taints {
		if taint.Key == taints.ToBeDeletedTaint {
			if !apiequality.Semantic.DeepEqual(taint, receipt.taint) {
				return fmt.Errorf("deletion taint ownership changed on Node %q", node.Name)
			}
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("owned deletion taint missing or duplicated on Node %q", node.Name)
	}
	return nil
}

// MarkToBeDeleted records only a taint written by this provider process.
func (scaleSet *ScaleSet) MarkToBeDeleted(ctx context.Context, node *apiv1.Node, cordon bool) (*apiv1.Node, bool, error) {
	if scaleSet.retention == nil {
		return node, false, nil
	}
	state := scaleSet.retention
	state.mutex.Lock()
	defer state.mutex.Unlock()
	if cordon {
		return nil, true, fmt.Errorf("Deallocate requires --cordon-node-before-terminating=false")
	}
	if err := scaleSet.retentionViewError(); err != nil {
		return nil, true, err
	}
	if node == nil || node.UID == "" {
		return nil, true, fmt.Errorf("Deallocate requires a Node with a UID")
	}
	member := state.instances[strings.ToLower(node.Spec.ProviderID)]
	if member == nil || member.instance.Phase != retentionActive {
		return nil, true, fmt.Errorf("Node %q is not an active Deallocate member", node.Name)
	}
	var updated *apiv1.Node
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live, err := scaleSet.manager.autoscalerOptions.KubeClient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if live.UID != node.UID || live.Spec.ProviderID != node.Spec.ProviderID {
			return fmt.Errorf("Node %q identity changed before marking", node.Name)
		}
		if member.receipt != nil && !member.completed {
			if err := member.receipt.checkIdentity(live); err != nil {
				return err
			}
			if err := member.receipt.checkTaint(live); err != nil {
				return err
			}
			updated = live
			return nil
		}
		if taints.HasToBeDeletedTaint(live) {
			return fmt.Errorf("Node %q has an unowned deletion taint", live.Name)
		}
		if condition := conditionSuspended(live); condition != nil &&
			(condition.Status != apiv1.ConditionFalse || condition.Reason != suspendedReason) {
			return fmt.Errorf("Node %q has an unowned Suspended condition", live.Name)
		}
		taint := apiv1.Taint{Key: taints.ToBeDeletedTaint, Value: strconv.FormatInt(time.Now().Unix(), 10), Effect: apiv1.TaintEffectNoSchedule}
		live.Spec.Taints = append(live.Spec.Taints, taint)
		updated, err = scaleSet.manager.autoscalerOptions.KubeClient.CoreV1().Nodes().Update(ctx, live, metav1.UpdateOptions{})
		if err == nil {
			member.receipt = &retentionReceipt{name: live.Name, uid: live.UID, providerID: live.Spec.ProviderID, taint: taint}
			member.completed = false
			member.node = updated.DeepCopy()
		}
		return err
	})
	if err == nil {
		scaleSet.publishRetention()
	}
	return updated, true, err
}

// CleanToBeDeleted handles rejected drain cleanup without releasing accepted work.
func (scaleSet *ScaleSet) CleanToBeDeleted(ctx context.Context, node *apiv1.Node, cordon bool) (*apiv1.Node, bool, error) {
	if scaleSet.retention == nil {
		return node, false, nil
	}
	state := scaleSet.retention
	state.mutex.Lock()
	defer state.mutex.Unlock()
	if err := scaleSet.retentionViewError(); err != nil {
		return nil, true, err
	}
	if node == nil {
		return nil, true, fmt.Errorf("cannot clean a nil Deallocate Node")
	}
	member := state.instances[strings.ToLower(node.Spec.ProviderID)]
	if member == nil {
		return nil, true, fmt.Errorf("unknown Deallocate Node %q", node.Name)
	}
	if member.instance.Phase != retentionActive {
		return node.DeepCopy(), true, nil
	}
	if member.receipt == nil || member.completed {
		if taints.HasToBeDeletedTaint(node) || cloudprovider.IsNodeSuspended(node) {
			return nil, true, fmt.Errorf("Node %q has restrictions without an ownership receipt", node.Name)
		}
		return node.DeepCopy(), true, nil
	}
	if err := member.receipt.checkIdentity(node); err != nil {
		return nil, true, err
	}
	updated, err := scaleSet.removeOwnedTaint(ctx, member, false)
	if err == nil {
		member.receipt = nil
		member.node = updated.DeepCopy()
		scaleSet.publishRetention()
	}
	return updated, true, err
}

func (scaleSet *ScaleSet) validateRetirementNode(ctx context.Context, member *retentionMember, requested *apiv1.Node) error {
	if member.receipt == nil || member.completed || member.instance.Phase != retentionActive {
		return fmt.Errorf("instance %q has no current retirement receipt", member.instance.Id)
	}
	if err := member.receipt.checkIdentity(requested); err != nil {
		return err
	}
	live, err := scaleSet.manager.autoscalerOptions.KubeClient.CoreV1().Nodes().Get(ctx, member.receipt.name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if err := member.receipt.checkIdentity(live); err != nil {
		return err
	}
	if cloudprovider.IsNodeSuspended(live) {
		return fmt.Errorf("Node %q is already suspended before retirement", live.Name)
	}
	if err := member.receipt.checkTaint(live); err != nil {
		return err
	}
	member.node = live.DeepCopy()
	return nil
}

func (scaleSet *ScaleSet) writeSuspended(ctx context.Context, member *retentionMember, suspended bool) error {
	if member.receipt == nil {
		return fmt.Errorf("instance %q has no Suspended ownership receipt", member.instance.Id)
	}
	receipt := member.receipt
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live, err := scaleSet.manager.autoscalerOptions.KubeClient.CoreV1().Nodes().Get(ctx, receipt.name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := receipt.checkIdentity(live); err != nil {
			return err
		}
		if err := receipt.checkTaint(live); err != nil {
			return err
		}
		condition := conditionSuspended(live)
		if condition != nil && condition.Reason != suspendedReason {
			return fmt.Errorf("Node %q Suspended condition belongs to another writer", live.Name)
		}
		if !suspended && !freshResumeReady(live, member) {
			return fmt.Errorf("Node %q has no post-acceptance Ready heartbeat", live.Name)
		}
		status := apiv1.ConditionFalse
		if suspended {
			status = apiv1.ConditionTrue
		}
		if condition == nil {
			live.Status.Conditions = append(live.Status.Conditions, apiv1.NodeCondition{Type: "Suspended"})
			condition = &live.Status.Conditions[len(live.Status.Conditions)-1]
		}
		if condition.Status != status {
			condition.Status = status
			condition.LastTransitionTime = metav1.Now()
			condition.Reason = suspendedReason
			condition.Message = "Azure Cluster Autoscaler owns retained instance availability"
			live, err = scaleSet.manager.autoscalerOptions.KubeClient.CoreV1().Nodes().UpdateStatus(ctx, live, metav1.UpdateOptions{})
			if err != nil {
				return err
			}
		}
		if err := receipt.checkIdentity(live); err != nil {
			return err
		}
		member.node = live.DeepCopy()
		return nil
	})
}

func (scaleSet *ScaleSet) holdSuspended(ctx context.Context, member *retentionMember) error {
	return scaleSet.writeSuspended(ctx, member, true)
}

func (scaleSet *ScaleSet) removeOwnedTaint(ctx context.Context, member *retentionMember, resume bool) (*apiv1.Node, error) {
	var updated *apiv1.Node
	receipt := member.receipt
	receipt.cleanupStarted = resume
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live, err := scaleSet.manager.autoscalerOptions.KubeClient.CoreV1().Nodes().Get(ctx, receipt.name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := receipt.checkIdentity(live); err != nil {
			return err
		}
		if resume && !freshResumeReady(live, member) {
			return fmt.Errorf("Node %q stopped being Ready during activation", live.Name)
		}
		if resume {
			if condition := conditionSuspended(live); condition == nil || condition.Status != apiv1.ConditionFalse || condition.Reason != suspendedReason {
				return fmt.Errorf("Node %q changed Suspended before cleanup", live.Name)
			}
		}
		if err := receipt.checkTaint(live); err != nil {
			return err
		}
		live.Spec.Taints = slices.DeleteFunc(live.Spec.Taints, func(taint apiv1.Taint) bool {
			return apiequality.Semantic.DeepEqual(taint, receipt.taint)
		})
		updated, err = scaleSet.manager.autoscalerOptions.KubeClient.CoreV1().Nodes().Update(ctx, live, metav1.UpdateOptions{})
		return err
	})
	return updated, err
}

func (scaleSet *ScaleSet) restoreOwnedProtection(ctx context.Context, member *retentionMember) error {
	receipt := member.receipt
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := scaleSet.manager.autoscalerOptions.KubeClient.CoreV1().Nodes().Get(ctx, receipt.name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := receipt.checkIdentity(node); err != nil {
			return err
		}
		if taints.HasToBeDeletedTaint(node) {
			return receipt.checkTaint(node)
		}
		node.Spec.Taints = append(node.Spec.Taints, receipt.taint)
		_, err = scaleSet.manager.autoscalerOptions.KubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return err
	}
	receipt.cleanupStarted = false
	return scaleSet.holdSuspended(ctx, member)
}

func (scaleSet *ScaleSet) reconcileRetentionNodes(ctx context.Context) error {
	state := scaleSet.retention
	// A live list also covers initial Refresh before the shared informer starts.
	nodes, err := scaleSet.manager.autoscalerOptions.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	observed := make(map[string]*apiv1.Node)
	for i := range nodes.Items {
		node := &nodes.Items[i]
		key := strings.ToLower(node.Spec.ProviderID)
		if state.instances[key] == nil {
			continue
		}
		if observed[key] != nil || node.UID == "" {
			return fmt.Errorf("invalid or duplicate Node identity for %q", node.Spec.ProviderID)
		}
		observed[key] = node
	}
	now := time.Now()
	for operation, capacity := range state.capacity {
		if !capacity.failed && capacity.unassigned > 0 && !now.Before(capacity.deadline) {
			capacity.failed = true
			state.capacity[operation] = capacity
			state.queueFailure(operation, int(capacity.unassigned), cloudprovider.InstanceErrorInfo{
				ErrorClass: cloudprovider.OtherErrorClass, ErrorCode: "AzureCapacityTimeout", ErrorMessage: "accepted fresh capacity did not materialize before its deadline",
			})
		}
	}
	for key, member := range state.instances {
		node := observed[key]
		member.node = nil
		if member.instance.Phase == retentionActive {
			if node != nil {
				if cloudprovider.IsNodeSuspended(node) {
					return fmt.Errorf("Node %q is suspended without accepted retention intent", node.Name)
				}
				if member.receipt == nil && taints.HasToBeDeletedTaint(node) {
					return fmt.Errorf("Node %q has a deletion taint without process-local ownership", node.Name)
				}
				if member.receipt != nil && !member.completed {
					if err := member.receipt.checkIdentity(node); err != nil {
						return err
					}
				}
				member.node = node.DeepCopy()
			}
			continue
		}
		if member.instance.Phase == retentionResuming && member.instance.Resume.Error == nil && !now.Before(member.instance.Resume.Deadline) {
			member.instance.Resume.Error = &cloudprovider.InstanceErrorInfo{
				ErrorClass: cloudprovider.OtherErrorClass, ErrorCode: "AzureStartTimeout", ErrorMessage: "accepted resume did not become usable before its deadline",
			}
			state.queueFailure(member.operation, 1, *member.instance.Resume.Error)
		}
		if node == nil {
			// Retained physical members remain associated even without a Kubernetes Node.
			continue
		}
		if member.receipt == nil {
			return fmt.Errorf("inactive Node %q has no process-local ownership receipt", node.Name)
		}
		if err := member.receipt.checkIdentity(node); err != nil {
			return err
		}
		if member.receipt.cleanupStarted {
			if err := scaleSet.restoreOwnedProtection(ctx, member); err != nil {
				return err
			}
			node = member.node
		}
		if err := member.receipt.checkTaint(node); err != nil {
			return err
		}
		if member.instance.Phase == retentionResuming && freshResumeReady(node, member) {
			// Keep NoSchedule protection until status publication succeeds.
			if err := scaleSet.writeSuspended(ctx, member, false); err != nil {
				return err
			}
			updated, err := scaleSet.removeOwnedTaint(ctx, member, true)
			if err != nil {
				protectionErr := scaleSet.restoreOwnedProtection(ctx, member)
				return errors.Join(err, protectionErr)
			}
			if err := member.receipt.checkIdentity(updated); err != nil {
				return err
			}
			if !freshResumeReady(updated, member) {
				err := fmt.Errorf("Node %q lost readiness during cleanup", updated.Name)
				return errors.Join(err, scaleSet.restoreOwnedProtection(ctx, member))
			}
			if condition := conditionSuspended(updated); condition == nil || condition.Status != apiv1.ConditionFalse || condition.Reason != suspendedReason {
				protectionErr := scaleSet.restoreOwnedProtection(ctx, member)
				return errors.Join(scaleSet.quarantineRetention("resume cleanup", fmt.Errorf("Node %q changed Suspended during cleanup", updated.Name)), protectionErr)
			}
			member.node = updated.DeepCopy()
			member.completed = true
			member.receipt.cleanupStarted = false
			member.instance.Phase = retentionActive
		} else if err := scaleSet.holdSuspended(ctx, member); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
	}
	return nil
}
