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
	"testing"

	"github.com/stretchr/testify/require"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-autoscaler/pkg/cloudprovider"
	cptest "sigs.k8s.io/cluster-autoscaler/pkg/cloudprovider/test"
	cacontext "sigs.k8s.io/cluster-autoscaler/pkg/context"
	coreoptions "sigs.k8s.io/cluster-autoscaler/pkg/core/options"
	"sigs.k8s.io/cluster-autoscaler/pkg/processors/customresources"
	"sigs.k8s.io/cluster-autoscaler/pkg/resourcequotas"
	catest "sigs.k8s.io/cluster-autoscaler/pkg/utils/test"
	"sigs.k8s.io/cluster-autoscaler/pkg/utils/units"
)

type quotaNodeFilter func(*apiv1.Node) bool

func (f quotaNodeFilter) ExcludeFromTracking(node *apiv1.Node) bool {
	return f(node)
}

func TestSuspendedNodeFilter(t *testing.T) {
	tests := []struct {
		name     string
		node     *apiv1.Node
		excluded bool
	}{
		{name: "nil"},
		{name: "ordinary", node: &apiv1.Node{}},
		{
			name: "suspended",
			node: &apiv1.Node{Status: apiv1.NodeStatus{Conditions: []apiv1.NodeCondition{
				{Type: suspendedCondition, Status: apiv1.ConditionTrue},
			}}},
			excluded: true,
		},
		{
			name: "resuming",
			node: &apiv1.Node{Status: apiv1.NodeStatus{Conditions: []apiv1.NodeCondition{
				{Type: suspendedCondition, Status: apiv1.ConditionFalse},
			}}},
		},
		{
			name: "unknown",
			node: &apiv1.Node{Status: apiv1.NodeStatus{Conditions: []apiv1.NodeCondition{
				{Type: suspendedCondition, Status: apiv1.ConditionUnknown},
			}}},
		},
		{
			name: "upcoming copied from suspended",
			node: &apiv1.Node{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{templateNodeLabel: "true"}},
				Status: apiv1.NodeStatus{Conditions: []apiv1.NodeCondition{
					{Type: suspendedCondition, Status: apiv1.ConditionTrue},
				}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.excluded, (suspendedNodeFilter{}).ExcludeFromTracking(tt.node))
		})
	}
}

func TestConfigureSuspendedQuotas(t *testing.T) {
	parked := catest.BuildTestNode("parked", 1000, units.GiB)
	parked.Status.Conditions = append(parked.Status.Conditions,
		apiv1.NodeCondition{Type: suspendedCondition, Status: apiv1.ConditionTrue})
	virtual := catest.BuildTestNode("virtual", 1000, units.GiB)
	virtual.Labels["type"] = "virtual-kubelet"
	limiter := cloudprovider.NewResourceLimiter(
		map[string]int64{"cpu": 1, "memory": units.GiB, "gpu": 1},
		map[string]int64{"cpu": 8, "memory": 8 * units.GiB, "gpu": 4},
	)
	t.Run("disabled does not change options", func(t *testing.T) {
		opts := &coreoptions.AutoscalerOptions{}
		manager := &AzureManager{config: &Config{}}
		require.Same(t, limiter, configureSuspendedQuotas(opts, manager, limiter))
		require.Nil(t, opts.QuotasTrackerOptions.NodeFilter)
		require.Nil(t, opts.MinQuotasTrackerOptions.NodeFilter)
	})
	t.Run("distinct maximum and minimum filters", func(t *testing.T) {
		opts := &coreoptions.AutoscalerOptions{}
		manager := &AzureManager{config: &Config{Deallocate: true}}
		require.Same(t, limiter, configureSuspendedQuotas(opts, manager, limiter))
		require.True(t, opts.QuotasTrackerOptions.NodeFilter.ExcludeFromTracking(parked))
		require.False(t, opts.MinQuotasTrackerOptions.NodeFilter.ExcludeFromTracking(parked))
		require.True(t, opts.QuotasTrackerOptions.NodeFilter.ExcludeFromTracking(virtual))
		require.True(t, opts.MinQuotasTrackerOptions.NodeFilter.ExcludeFromTracking(virtual))
	})
	t.Run("preserves supplied filters", func(t *testing.T) {
		custom := quotaNodeFilter(func(node *apiv1.Node) bool { return node.Name == "custom" })
		opts := &coreoptions.AutoscalerOptions{
			QuotasTrackerOptions:    resourcequotas.TrackerOptions{NodeFilter: custom},
			MinQuotasTrackerOptions: resourcequotas.TrackerOptions{NodeFilter: custom},
		}
		manager := &AzureManager{config: &Config{Deallocate: true}}
		configureSuspendedQuotas(opts, manager, limiter)
		require.True(t, opts.QuotasTrackerOptions.NodeFilter.ExcludeFromTracking(&apiv1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "custom"},
		}))
		require.True(t, opts.QuotasTrackerOptions.NodeFilter.ExcludeFromTracking(parked))
		require.False(t, opts.MinQuotasTrackerOptions.NodeFilter.ExcludeFromTracking(parked))
	})
	t.Run("adds cap without mutating resource bounds", func(t *testing.T) {
		opts := &coreoptions.AutoscalerOptions{}
		manager := &AzureManager{config: &Config{Deallocate: true, MaxActiveNodes: 3}}
		got := configureSuspendedQuotas(opts, manager, limiter)
		require.Equal(t, int64(3), got.GetMax(resourcequotas.ResourceNodes))
		require.False(t, limiter.HasMaxLimitSet(resourcequotas.ResourceNodes))
		for _, name := range limiter.GetResources() {
			require.Equal(t, limiter.GetMin(name), got.GetMin(name))
			require.Equal(t, limiter.GetMax(name), got.GetMax(name))
		}
	})
	t.Run("keeps stricter existing node cap", func(t *testing.T) {
		manager := &AzureManager{config: &Config{Deallocate: true, MaxActiveNodes: 3}}
		original := cloudprovider.NewResourceLimiter(nil, map[string]int64{resourcequotas.ResourceNodes: 2})
		got := configureSuspendedQuotas(&coreoptions.AutoscalerOptions{}, manager, original)
		require.Equal(t, int64(2), got.GetMax(resourcequotas.ResourceNodes))
		got.Limits()[resourcequotas.ResourceNodes] = 1
		require.Equal(t, int64(2), original.GetMax(resourcequotas.ResourceNodes))
	})
}

func TestSuspendedQuotaNodeLimit(t *testing.T) {
	tests := []struct {
		name     string
		running  int
		parked   int
		upcoming int
		limit    int64
		allowed  int
	}{
		{name: "parked nodes leave headroom", parked: 3, limit: 2, allowed: 2},
		{name: "exact boundary", running: 2, parked: 1, limit: 3, allowed: 1},
		{name: "upcoming templates consume cap", running: 1, parked: 2, upcoming: 1, limit: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := &coreoptions.AutoscalerOptions{}
			manager := &AzureManager{config: &Config{Deallocate: true, MaxActiveNodes: tt.limit}}
			limiter := configureSuspendedQuotas(opts, manager, cloudprovider.NewResourceLimiter(nil, nil))
			provider := cptest.NewTestCloudProviderBuilder().Build()
			provider.SetResourceLimiter(limiter)
			ctx := &cacontext.AutoscalingContext{CloudProvider: provider}
			nodes := make([]*apiv1.Node, 0, tt.running+tt.parked+tt.upcoming)
			for i := range tt.running + tt.parked + tt.upcoming {
				node := catest.BuildTestNode("node", 1000, units.GiB)
				if i >= tt.running {
					node.Status.Conditions = append(node.Status.Conditions,
						apiv1.NodeCondition{Type: suspendedCondition, Status: apiv1.ConditionTrue})
				}
				if i >= tt.running+tt.parked {
					node.Labels[templateNodeLabel] = "true"
				}
				nodes = append(nodes, node)
			}
			opts.QuotasTrackerOptions.CustomResourcesProcessor = customresources.NewDefaultCustomResourcesProcessor(false, false)
			opts.QuotasTrackerOptions.QuotaProvider = resourcequotas.NewCloudQuotasProvider(provider)
			tracker, err := resourcequotas.NewTrackerFactory(opts.QuotasTrackerOptions).NewMaxQuotasTracker(
				t.Context(), ctx, nodes,
			)
			require.NoError(t, err)
			result, err := tracker.CheckQuota(
				t.Context(), ctx, nil, catest.BuildTestNode("new", 1000, units.GiB), 10,
			)
			require.NoError(t, err)
			require.Equal(t, tt.allowed, result.AllowedDelta)
		})
	}
}

func TestSuspendedQuotaPreservesMinimum(t *testing.T) {
	opts := &coreoptions.AutoscalerOptions{}
	manager := &AzureManager{config: &Config{Deallocate: true}}
	limiter := configureSuspendedQuotas(opts, manager, cloudprovider.NewResourceLimiter(
		map[string]int64{"cpu": 1, "memory": units.GiB}, nil,
	))
	provider := cptest.NewTestCloudProviderBuilder().Build()
	provider.SetResourceLimiter(limiter)
	ctx := &cacontext.AutoscalingContext{CloudProvider: provider}
	running := catest.BuildTestNode("running", 1000, units.GiB)
	parked := catest.BuildTestNode("parked", 1000, units.GiB)
	parked.Status.Conditions = append(parked.Status.Conditions,
		apiv1.NodeCondition{Type: suspendedCondition, Status: apiv1.ConditionTrue})
	opts.MinQuotasTrackerOptions.CustomResourcesProcessor = customresources.NewDefaultCustomResourcesProcessor(false, false)
	opts.MinQuotasTrackerOptions.QuotaProvider = resourcequotas.NewCloudMinProvider(provider)
	tracker, err := resourcequotas.NewTrackerFactory(opts.MinQuotasTrackerOptions).NewMinQuotasTracker(
		t.Context(), ctx, []*apiv1.Node{running, parked},
	)
	require.NoError(t, err)
	result, err := tracker.CheckQuota(t.Context(), ctx, nil, running, 2)
	require.NoError(t, err)
	require.Equal(t, 1, result.AllowedDelta)
}
