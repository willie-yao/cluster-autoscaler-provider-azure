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
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func TestParseAzureNodeGroupSpec(t *testing.T) {
	for _, tc := range []struct {
		name       string
		value      string
		zero       bool
		wantName   string
		wantMin    int
		wantMax    int
		wantPolicy string
		wantLabels map[string]string
		wantTaints string
		wantErr    string
	}{
		{
			name: "plain vmss", value: "0:10:pool", zero: true,
			wantName: "pool", wantMin: 0, wantMax: 10, wantPolicy: "Delete",
		},
		{
			name: "plain standard", value: "1:5:pool",
			wantName: "pool", wantMin: 1, wantMax: 5, wantPolicy: "Delete",
		},
		{
			name: "policy only", value: "1:50:Delete:pool", zero: true,
			wantName: "pool", wantMin: 1, wantMax: 50, wantPolicy: "Delete",
		},
		{
			name: "empty labels and taints", value: "1:50:Delete:pool:{}|", zero: true,
			wantName: "pool", wantMin: 1, wantMax: 50, wantPolicy: "Delete", wantLabels: map[string]string{},
		},
		{
			name: "labels only", value: `1:50:Delete:pool:{"environment":"prod"}`, zero: true,
			wantName: "pool", wantMin: 1, wantMax: 50, wantPolicy: "Delete", wantLabels: map[string]string{"environment": "prod"},
		},
		{
			name: "taints and labels with colons", value: `0:10:Delete:pool/Standard_D2_v2:{"zone":"east:1"}|key=value:NoSchedule,other=yes:NoExecute`, zero: true,
			wantName: "pool/Standard_D2_v2", wantMin: 0, wantMax: 10, wantPolicy: "Delete", wantLabels: map[string]string{"zone": "east:1"},
			wantTaints: "key=value:NoSchedule,other=yes:NoExecute",
		},
		{
			name: "deallocate policy only", value: "1:50:Deallocate:pool", zero: true,
			wantName: "pool", wantMin: 1, wantMax: 50, wantPolicy: "Deallocate",
		},
		{
			name: "deallocate with labels and taints", value: `0:10:Deallocate:pool:{"team":"prod"}|dedicated=yes:NoSchedule`, zero: true,
			wantName: "pool", wantMin: 0, wantMax: 10, wantPolicy: "Deallocate",
			wantLabels: map[string]string{"team": "prod"}, wantTaints: "dedicated=yes:NoSchedule",
		},
		{name: "too few parts", value: "1:5", wantErr: "wrong nodes configuration"},
		{name: "invalid minimum", value: "wrong:5:pool", wantErr: "failed to set min size"},
		{name: "invalid maximum", value: "1:wrong:Delete:pool", wantErr: "failed to set max size"},
		{name: "minimum below zero", value: "-1:5:pool", zero: true, wantErr: "min size must be >= 0"},
		{name: "zero disallowed for standard", value: "0:5:pool", wantErr: "min size must be >= 1"},
		{name: "maximum below minimum", value: "5:1:Delete:pool", zero: true, wantErr: "max size must be greater"},
		{name: "empty name", value: "1:5:Delete:", zero: true, wantErr: "name must not be blank"},
		{name: "unknown policy", value: "1:5:Keep:pool:{}|", wantErr: "want Delete or Deallocate"},
		{name: "deallocate invalid maximum", value: "1:wrong:Deallocate:pool", wantErr: "failed to set max size"},
		{name: "deallocate invalid name", value: "1:5:Deallocate:", wantErr: "name must not be blank"},
		{name: "deallocate invalid json", value: "1:5:Deallocate:pool:{|", wantErr: "invalid labels"},
		{name: "invalid json", value: "1:5:Delete:pool:{|", wantErr: "invalid labels"},
		{name: "invalid label value", value: `1:5:Delete:pool:{"team":42}|`, wantErr: "invalid labels"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := parseAzureNodeGroupSpec(tc.value, tc.zero)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseAzureNodeGroupSpec() error = %v, want %q", err, tc.wantErr)
				}
				if spec.NodeGroupSpec != nil {
					t.Fatalf("invalid spec returned node group %+v", spec)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if spec.Name != tc.wantName || spec.MinSize != tc.wantMin || spec.MaxSize != tc.wantMax || spec.policy != tc.wantPolicy ||
				spec.SupportScaleToZero != tc.zero || !reflect.DeepEqual(spec.labels, tc.wantLabels) || spec.taints != tc.wantTaints {
				t.Errorf("parseAzureNodeGroupSpec(%q) = %+v, policy %q, labels %v, taints %q", tc.value, spec.NodeGroupSpec, spec.policy, spec.labels, spec.taints)
			}
		})
	}
}

func TestBuildNodeGroupFromSpecUnsupportedDeallocate(t *testing.T) {
	manager := newTestAzureManager(t)
	group, err := manager.buildNodeGroupFromSpec("1:5:Deallocate:paused-vmss:{}|")
	require.Nil(t, group)
	var policyErr *unsupportedDeallocateError
	require.True(t, errors.As(err, &policyErr))
	require.Equal(t, "paused-vmss", policyErr.name)
	require.Equal(t, `Deallocate scale-down is not supported by this build, so node group "paused-vmss" is not autoscaled`, err.Error())
}

func TestExtendedScaleSetNodeTemplate(t *testing.T) {
	manager := newTestAzureManager(t)
	vmss := *manager.azureCache.getScaleSets()["test-asg"]
	vmss.Tags = map[string]*string{
		nodeLabelTagName + "env":      ptr.To("tag"),
		nodeLabelTagName + "tag-only": ptr.To("tag"),
		nodeTaintTagName + "from-tag": ptr.To("tag:NoExecute"),
	}
	manager.azureCache.setScaleSet("test-asg", &vmss)

	group, err := manager.buildNodeGroupFromSpec(`0:5:Delete:test-asg:{"env":"prod"}|from-spec=prod:NoSchedule`)
	require.NoError(t, err)
	scaleSet, ok := group.(*ScaleSet)
	require.True(t, ok)
	require.Equal(t, map[string]string{"env": "prod"}, scaleSet.labels)
	require.Equal(t, "from-spec=prod:NoSchedule", scaleSet.taints)

	nodeInfo, err := scaleSet.TemplateNodeInfo(context.Background())
	require.NoError(t, err)
	require.Equal(t, "prod", nodeInfo.Node().Labels["env"])
	require.Empty(t, nodeInfo.Node().Labels["tag-only"])
	require.Equal(t, []apiv1.Taint{{Key: "from-spec", Value: "prod", Effect: apiv1.TaintEffectNoSchedule}}, nodeInfo.Node().Spec.Taints)

	legacy, err := manager.buildNodeGroupFromSpec("0:5:test-asg")
	require.NoError(t, err)
	legacyInfo, err := legacy.TemplateNodeInfo(context.Background())
	require.NoError(t, err)
	require.Equal(t, "tag", legacyInfo.Node().Labels["env"])
	require.Equal(t, "tag", legacyInfo.Node().Labels["tag-only"])
	require.Equal(t, []apiv1.Taint{{Key: "from-tag", Value: "tag", Effect: apiv1.TaintEffectNoExecute}}, legacyInfo.Node().Spec.Taints)
}

func TestExtendedVMPoolNodeTemplate(t *testing.T) {
	manager := newTestAzureManager(t)
	manager.azClient.agentPoolClient = NewMockAgentPoolsClient(gomock.NewController(t))
	pool := getTestVMsAgentPool(false)
	pool.Properties.NodeLabels = map[string]*string{
		"env":       ptr.To("from-pool"),
		"pool-only": ptr.To("pool"),
	}
	pool.Properties.NodeTaints = []*string{ptr.To("from-pool=yes:NoExecute")}
	manager.azureCache.vmsPoolMap[vmsAgentPoolName] = pool

	group, err := manager.buildNodeGroupFromSpec(fmt.Sprintf(
		`1:5:Delete:%s:{"env":"prod"}|from-spec=yes:NoSchedule`, vmsNodeGroupName))
	require.NoError(t, err)
	vmPool, ok := group.(*VMPool)
	require.True(t, ok)
	require.Equal(t, map[string]string{"env": "prod"}, vmPool.labels)
	require.Equal(t, "from-spec=yes:NoSchedule", vmPool.taints)

	nodeInfo, err := vmPool.TemplateNodeInfo(context.Background())
	require.NoError(t, err)
	require.Equal(t, "prod", nodeInfo.Node().Labels["env"])
	require.Equal(t, "pool", nodeInfo.Node().Labels["pool-only"])
	require.ElementsMatch(t, []apiv1.Taint{
		{Key: "from-pool", Value: "yes", Effect: apiv1.TaintEffectNoExecute},
		{Key: "from-spec", Value: "yes", Effect: apiv1.TaintEffectNoSchedule},
	}, nodeInfo.Node().Spec.Taints)

	otherGroup, err := manager.buildNodeGroupFromSpec(fmt.Sprintf(
		`1:5:Delete:%s/Standard_D4_v2:{"other":"second"}|`, vmsAgentPoolName))
	require.NoError(t, err)
	otherInfo, err := otherGroup.TemplateNodeInfo(context.Background())
	require.NoError(t, err)
	require.Equal(t, "from-pool", otherInfo.Node().Labels["env"])
	require.Equal(t, "second", otherInfo.Node().Labels["other"])
	require.Equal(t, []apiv1.Taint{{Key: "from-pool", Value: "yes", Effect: apiv1.TaintEffectNoExecute}}, otherInfo.Node().Spec.Taints)
	require.Equal(t, "from-pool", *manager.azureCache.getVMsPoolMap()[vmsAgentPoolName].Properties.NodeLabels["env"])
	require.NotContains(t, manager.azureCache.getVMsPoolMap()[vmsAgentPoolName].Properties.NodeLabels, "other")
}
