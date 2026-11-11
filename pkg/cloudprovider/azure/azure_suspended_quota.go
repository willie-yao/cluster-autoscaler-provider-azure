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
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-autoscaler/pkg/cloudprovider"
	coreoptions "sigs.k8s.io/cluster-autoscaler/pkg/core/options"
	"sigs.k8s.io/cluster-autoscaler/pkg/core/utils"
	"sigs.k8s.io/cluster-autoscaler/pkg/resourcequotas"
)

const templateNodeLabel = "cluster-autoscaler.kubernetes.io/template-node"

type suspendedNodeFilter struct{}

func (suspendedNodeFilter) ExcludeFromTracking(node *apiv1.Node) bool {
	// The core copies conditions from existing Nodes into upcoming templates.
	if node == nil || node.Labels[templateNodeLabel] == "true" {
		return false
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == suspendedCondition {
			return condition.Status == apiv1.ConditionTrue
		}
	}
	return false
}

func configureSuspendedQuotas(
	opts *coreoptions.AutoscalerOptions,
	manager *AzureManager,
	limiter *cloudprovider.ResourceLimiter,
) *cloudprovider.ResourceLimiter {
	if !manager.suspendedModeEnabled() {
		return limiter
	}
	filters := []resourcequotas.NodeFilter{utils.VirtualKubeletNodeFilter{}, suspendedNodeFilter{}}
	if opts.QuotasTrackerOptions.NodeFilter != nil {
		filters = append(filters, opts.QuotasTrackerOptions.NodeFilter)
	}
	opts.QuotasTrackerOptions.NodeFilter = resourcequotas.NewCombinedNodeFilter(filters)
	// Otherwise the core inherits the maximum filter for minimum enforcement.
	if opts.MinQuotasTrackerOptions.NodeFilter == nil {
		opts.MinQuotasTrackerOptions.NodeFilter = utils.VirtualKubeletNodeFilter{}
	}
	if opts.MaxNodesTotal > 0 {
		klog.Warningf(
			"--max-nodes-total=%d counts retained suspended Nodes; use maxActiveNodes with --max-nodes-total=0 for an active-node limit",
			opts.MaxNodesTotal,
		)
	}
	if manager.config.MaxActiveNodes == 0 {
		return limiter
	}
	minLimits := make(map[string]int64)
	maxLimits := make(map[string]int64)
	if limiter != nil {
		for _, name := range limiter.GetResources() {
			if limiter.HasMinLimitSet(name) {
				minLimits[name] = limiter.GetMin(name)
			}
			if limiter.HasMaxLimitSet(name) {
				maxLimits[name] = limiter.GetMax(name)
			}
		}
	}
	nodeLimit := manager.config.MaxActiveNodes
	if existing, found := maxLimits[resourcequotas.ResourceNodes]; found {
		nodeLimit = min(nodeLimit, existing)
	}
	maxLimits[resourcequotas.ResourceNodes] = nodeLimit
	return cloudprovider.NewResourceLimiter(minLimits, maxLimits)
}
