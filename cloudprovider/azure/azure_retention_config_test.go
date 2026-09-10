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
	"fmt"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	providerazureconfig "sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
	"sigs.k8s.io/cluster-autoscaler/pkg/config/dynamic"
)

func TestConfigScaleDownPoliciesJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		policies ScaleDownPolicies
		wantErr  bool
	}{
		{
			name:     "per-group policies",
			input:    `{"nodeGroupScaleDownPolicies":{"AGENTS":"Deallocate","other":"Delete"}}`,
			policies: ScaleDownPolicies{"AGENTS": "Deallocate", "other": "Delete"},
		},
		{name: "omitted", input: `{}`},
		{name: "null", input: `{"nodeGroupScaleDownPolicies":null}`},
		{name: "empty", input: `{"nodeGroupScaleDownPolicies":{}}`, policies: ScaleDownPolicies{}},
		{name: "duplicate key", input: `{"nodeGroupScaleDownPolicies":{"agents":"Delete","agents":"Deallocate"}}`, wantErr: true},
		{name: "duplicate cannot override Deallocate", input: `{"nodeGroupScaleDownPolicies":{"agents":"Deallocate","agents":"Delete"}}`, wantErr: true},
		{name: "case-folded duplicate", input: `{"nodeGroupScaleDownPolicies":{"agents":"Delete","AGENTS":"Deallocate"}}`, wantErr: true},
		{name: "case-folded duplicate cannot override Deallocate", input: `{"nodeGroupScaleDownPolicies":{"agents":"Deallocate","AGENTS":"Delete"}}`, wantErr: true},
		{name: "array", input: `{"nodeGroupScaleDownPolicies":["Deallocate"]}`, wantErr: true},
		{name: "string instead of object", input: `{"nodeGroupScaleDownPolicies":"Deallocate"}`, wantErr: true},
		{name: "non-string value", input: `{"nodeGroupScaleDownPolicies":{"agents":true}}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var cfg Config
			err := json.Unmarshal([]byte(test.input), &cfg)
			if test.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.policies, cfg.NodeGroupScaleDownPolicies)
		})
	}
}

func TestConfigDeallocates(t *testing.T) {
	tests := []struct {
		name     string
		policies ScaleDownPolicies
		group    string
		want     bool
	}{
		{name: "default Delete", group: "agents"},
		{name: "unconfigured group", policies: ScaleDownPolicies{"other": "Deallocate"}, group: "agents"},
		{name: "explicit Delete", policies: ScaleDownPolicies{"agents": "Delete"}, group: "agents"},
		{name: "Deallocate", policies: ScaleDownPolicies{"agents": "Deallocate"}, group: "agents", want: true},
		{name: "case-insensitive group", policies: ScaleDownPolicies{"AgEnTs": "Deallocate"}, group: "AGENTS", want: true},
		{name: "exact group", policies: ScaleDownPolicies{"agents": "Deallocate"}, group: "agents-other"},
		{name: "lowercase value is not Deallocate", policies: ScaleDownPolicies{"agents": "deallocate"}, group: "agents"},
		{name: "uppercase value is not Deallocate", policies: ScaleDownPolicies{"agents": "DEALLOCATE"}, group: "agents"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &Config{NodeGroupScaleDownPolicies: test.policies}
			assert.Equal(t, test.want, cfg.deallocates(test.group))
		})
	}
}

func TestConfigValidateScaleDownPolicies(t *testing.T) {
	tests := []struct {
		name      string
		policies  ScaleDownPolicies
		configure func(*Config)
		wantErr   string
	}{
		{name: "default Delete"},
		{name: "empty map", policies: ScaleDownPolicies{}},
		{name: "explicit Delete", policies: ScaleDownPolicies{"agents": "Delete"}},
		{name: "mixed policies", policies: ScaleDownPolicies{"AGENTS": "Deallocate", "other": "Delete"}},
		{
			name: "case-insensitive VM type", policies: ScaleDownPolicies{"agents": "Deallocate"},
			configure: func(cfg *Config) { cfg.VMType = "VMSS" },
		},
		{name: "empty key", policies: ScaleDownPolicies{"": "Delete"}, wantErr: "key"},
		{name: "blank key", policies: ScaleDownPolicies{" \t": "Deallocate"}, wantErr: "key"},
		{name: "leading whitespace", policies: ScaleDownPolicies{" agents": "Deallocate"}, wantErr: "key"},
		{name: "trailing whitespace", policies: ScaleDownPolicies{"agents\n": "Delete"}, wantErr: "key"},
		{name: "case-folded duplicate", policies: ScaleDownPolicies{"agents": "Deallocate", "AGENTS": "Delete"}, wantErr: "ambiguous"},
		{name: "empty value", policies: ScaleDownPolicies{"agents": ""}, wantErr: "expected Delete or Deallocate"},
		{name: "unknown value", policies: ScaleDownPolicies{"agents": "Stop"}, wantErr: "expected Delete or Deallocate"},
		{name: "lowercase Delete", policies: ScaleDownPolicies{"agents": "delete"}, wantErr: "expected Delete or Deallocate"},
		{name: "lowercase Deallocate", policies: ScaleDownPolicies{"agents": "deallocate"}, wantErr: "expected Delete or Deallocate"},
		{name: "padded value", policies: ScaleDownPolicies{"agents": "Deallocate "}, wantErr: "expected Delete or Deallocate"},
		{
			name: "strict cache updates", policies: ScaleDownPolicies{"agents": "Deallocate"},
			configure: func(cfg *Config) { cfg.StrictCacheUpdates = true }, wantErr: "strictCacheUpdates disabled",
		},
		{
			name: "standard VM type", policies: ScaleDownPolicies{"agents": "Deallocate"},
			configure: func(cfg *Config) { cfg.VMType = "standard" }, wantErr: "non-hosted VMSS",
		},
		{
			name: "hosted subscription", policies: ScaleDownPolicies{"agents": "Deallocate"},
			configure: func(cfg *Config) { cfg.HostedSubscriptionID = "hosted-subscription" }, wantErr: "non-hosted VMSS",
		},
		{
			name: "hosted resource group", policies: ScaleDownPolicies{"agents": "Deallocate"},
			configure: func(cfg *Config) { cfg.HostedResourceGroup = "hosted-rg" }, wantErr: "non-hosted VMSS",
		},
		{
			name: "hosted proxy", policies: ScaleDownPolicies{"agents": "Deallocate"},
			configure: func(cfg *Config) { cfg.HostedResourceProxyURL = "https://proxy.invalid" }, wantErr: "non-hosted VMSS",
		},
		{
			name: "Delete does not restrict deployment", policies: ScaleDownPolicies{"agents": "Delete"},
			configure: func(cfg *Config) {
				cfg.StrictCacheUpdates = true
				cfg.VMType = "standard"
				cfg.HostedSubscriptionID = "hosted-subscription"
				cfg.HostedResourceGroup = "hosted-rg"
				cfg.HostedResourceProxyURL = "https://proxy.invalid"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := newRetentionConfigManager(t)
			manager.config.NodeGroupScaleDownPolicies = test.policies
			if test.configure != nil {
				test.configure(manager.config)
			}
			err := manager.config.validateScaleDownPolicies()
			group, createErr := NewScaleSet(&dynamic.NodeGroupSpec{Name: "agents", MinSize: 0, MaxSize: 10}, manager, 3, false)
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
				require.ErrorContains(t, createErr, test.wantErr)
				assert.Nil(t, group)
				assert.Empty(t, manager.retentionGroups)
				return
			}
			require.NoError(t, err)
			require.NoError(t, createErr)
			require.NotNil(t, group)
			assert.Equal(t, manager.config.deallocates("agents"), group.retention != nil)
		})
	}
}

func TestNewScaleSetRetentionMetadata(t *testing.T) {
	tests := []struct {
		name          string
		mutate        func(*AzureManager, *armcompute.VirtualMachineScaleSet)
		dedicatedHost bool
		wantErr       string
	}{
		{name: "Uniform regular managed disk"},
		{
			name: "implicit regular priority",
			mutate: func(_ *AzureManager, vmss *armcompute.VirtualMachineScaleSet) {
				vmss.Properties.VirtualMachineProfile.Priority = nil
			},
		},
		{
			name: "missing VMSS",
			mutate: func(manager *AzureManager, _ *armcompute.VirtualMachineScaleSet) {
				manager.azureCache.scaleSets = nil
			},
			wantErr: "could not find vmss",
		},
		{
			name: "missing properties",
			mutate: func(_ *AzureManager, vmss *armcompute.VirtualMachineScaleSet) {
				vmss.Properties = nil
			},
			wantErr: "Uniform VMSS",
		},
		{
			name: "missing orchestration mode",
			mutate: func(_ *AzureManager, vmss *armcompute.VirtualMachineScaleSet) {
				vmss.Properties.OrchestrationMode = nil
			},
			wantErr: "Uniform VMSS",
		},
		{
			name: "Flexible",
			mutate: func(_ *AzureManager, vmss *armcompute.VirtualMachineScaleSet) {
				vmss.Properties.OrchestrationMode = ptr.To(armcompute.OrchestrationModeFlexible)
			},
			wantErr: "Uniform VMSS",
		},
		{
			name: "missing VM profile",
			mutate: func(_ *AzureManager, vmss *armcompute.VirtualMachineScaleSet) {
				vmss.Properties.VirtualMachineProfile = nil
			},
			wantErr: "managed, non-ephemeral OS disk",
		},
		{
			name: "missing storage profile",
			mutate: func(_ *AzureManager, vmss *armcompute.VirtualMachineScaleSet) {
				vmss.Properties.VirtualMachineProfile.StorageProfile = nil
			},
			wantErr: "managed, non-ephemeral OS disk",
		},
		{
			name: "missing OS disk",
			mutate: func(_ *AzureManager, vmss *armcompute.VirtualMachineScaleSet) {
				vmss.Properties.VirtualMachineProfile.StorageProfile.OSDisk = nil
			},
			wantErr: "managed, non-ephemeral OS disk",
		},
		{
			name: "unmanaged OS disk",
			mutate: func(_ *AzureManager, vmss *armcompute.VirtualMachineScaleSet) {
				vmss.Properties.VirtualMachineProfile.StorageProfile.OSDisk.ManagedDisk = nil
			},
			wantErr: "managed, non-ephemeral OS disk",
		},
		{
			name: "ephemeral OS disk",
			mutate: func(_ *AzureManager, vmss *armcompute.VirtualMachineScaleSet) {
				vmss.Properties.VirtualMachineProfile.StorageProfile.OSDisk.DiffDiskSettings = &armcompute.DiffDiskSettings{
					Option: ptr.To(armcompute.DiffDiskOptionsLocal),
				}
			},
			wantErr: "managed, non-ephemeral OS disk",
		},
		{
			name: "Spot",
			mutate: func(_ *AzureManager, vmss *armcompute.VirtualMachineScaleSet) {
				vmss.Properties.VirtualMachineProfile.Priority = ptr.To(armcompute.VirtualMachinePriorityTypesSpot)
			},
			wantErr: "regular-priority",
		},
		{
			name: "Low priority",
			mutate: func(_ *AzureManager, vmss *armcompute.VirtualMachineScaleSet) {
				vmss.Properties.VirtualMachineProfile.Priority = ptr.To(armcompute.VirtualMachinePriorityTypesLow)
			},
			wantErr: "regular-priority",
		},
		{
			name: "host group metadata",
			mutate: func(_ *AzureManager, vmss *armcompute.VirtualMachineScaleSet) {
				vmss.Properties.HostGroup = &armcompute.SubResource{ID: ptr.To("host-group")}
			},
			wantErr: "dedicated hosts",
		},
		{name: "dedicated host flag", dedicatedHost: true, wantErr: "dedicated hosts"},
		{
			name: "AKS-managed tag",
			mutate: func(_ *AzureManager, vmss *armcompute.VirtualMachineScaleSet) {
				vmss.Tags["aks-managed-poolName"] = ptr.To("agents")
			},
			wantErr: "AKS-managed",
		},
		{
			name: "case-insensitive AKS-managed tag",
			mutate: func(_ *AzureManager, vmss *armcompute.VirtualMachineScaleSet) {
				vmss.Tags["AKS-MANAGED-creationSource"] = nil
			},
			wantErr: "AKS-managed",
		},
		{
			name: "missing async client",
			mutate: func(manager *AzureManager, _ *armcompute.VirtualMachineScaleSet) {
				manager.azClient.vmssClientForDelete = nil
			},
			wantErr: "async VMSS client",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, policy := range []string{"", "Delete", "Deallocate"} {
				name := policy
				if name == "" {
					name = "default Delete"
				}
				t.Run(name, func(t *testing.T) {
					manager := newRetentionConfigManager(t)
					manager.config.NodeGroupScaleDownPolicies = nil
					if policy != "" {
						manager.config.NodeGroupScaleDownPolicies = ScaleDownPolicies{"agents": policy}
					}
					if test.mutate != nil {
						test.mutate(manager, manager.azureCache.scaleSets["agents"])
					}
					group, err := NewScaleSet(&dynamic.NodeGroupSpec{Name: "AGENTS", MinSize: 0, MaxSize: 10}, manager, 3, test.dedicatedHost)
					if policy == "Deallocate" && test.wantErr != "" {
						require.ErrorContains(t, err, test.wantErr)
						assert.Nil(t, group)
						assert.Empty(t, manager.retentionGroups)
						return
					}
					require.NoError(t, err)
					require.NotNil(t, group)
					assert.Equal(t, policy == "Deallocate", group.retention != nil)
					assert.Equal(t, int64(3), group.curSize)
					if policy != "Deallocate" {
						snapshot, err := group.retentionSnapshot(context.Background())
						require.NoError(t, err)
						assert.Nil(t, snapshot)
					}
				})
			}
		})
	}
}

func TestRetentionConfigGroupApplication(t *testing.T) {
	for _, discovery := range []bool{false, true} {
		name := "explicit"
		if discovery {
			name = "discovery"
		}
		t.Run(name, func(t *testing.T) {
			manager := newRetentionConfigManager(t)
			manager.config.NodeGroupScaleDownPolicies = ScaleDownPolicies{
				"AGENTS": "Deallocate", "DELETE-AGENTS": "Delete", "unselected": "Deallocate",
			}
			for _, group := range []string{"delete-agents", "default-agents", "unselected"} {
				manager.azureCache.scaleSets[group] = retentionConfigVMSS(group)
			}
			manager.azureCache.scaleSets["unselected"].Tags["cluster-autoscaler-name"] = ptr.To("another-cluster")

			minSize, maxSize := 1, 5
			if discovery {
				minSize, maxSize = 0, 10
				manager.autoDiscoverySpecs = []labelAutoDiscoveryConfig{{Selector: map[string]string{"cluster-autoscaler-name": "test-cluster"}}}
				require.NoError(t, manager.fetchAutoNodeGroups())
			} else {
				require.NoError(t, manager.fetchExplicitNodeGroups([]string{"1:5:agents", "1:5:delete-agents", "1:5:default-agents"}))
			}
			groups := manager.getNodeGroups()
			require.Len(t, groups, 3)
			for _, nodeGroup := range groups {
				group, ok := nodeGroup.(*ScaleSet)
				require.True(t, ok)
				assert.Equal(t, group.Id() == "agents", group.retention != nil, group.Id())
				assert.Equal(t, minSize, group.MinSize(context.Background()))
				assert.Equal(t, maxSize, group.MaxSize(context.Background()))
				assert.Equal(t, !discovery, manager.explicitlyConfigured[group.Id()])
			}
			assert.Len(t, manager.retentionGroups, 1)
		})
	}
}

func TestRetentionConfigGroupApplicationRejectsUnsupportedVMSS(t *testing.T) {
	for _, discovery := range []bool{false, true} {
		name := "explicit"
		if discovery {
			name = "discovery"
		}
		t.Run(name, func(t *testing.T) {
			manager := newRetentionConfigManager(t)
			manager.azureCache.scaleSets["agents"].Properties.VirtualMachineProfile.Priority = ptr.To(armcompute.VirtualMachinePriorityTypesSpot)
			var err error
			if discovery {
				manager.autoDiscoverySpecs = []labelAutoDiscoveryConfig{{Selector: map[string]string{"cluster-autoscaler-name": "test-cluster"}}}
				err = manager.fetchAutoNodeGroups()
			} else {
				err = manager.fetchExplicitNodeGroups([]string{"1:5:agents"})
			}
			require.ErrorContains(t, err, "regular-priority")
			assert.Empty(t, manager.getNodeGroups())
			assert.Empty(t, manager.retentionGroups)
		})
	}
}

func TestExplicitGroupConfigurationIsCaseInsensitive(t *testing.T) {
	manager := &AzureManager{
		explicitlyConfigured: map[string]bool{
			"pool/Standard_D2s_v3": true,
			"discovered":           false,
		},
	}
	for _, name := range []string{"pool/Standard_D2s_v3", "pool/standard_d2s_v3", "POOL/STANDARD_D2S_V3"} {
		assert.True(t, manager.isExplicitlyConfigured(name), name)
	}
	assert.False(t, manager.isExplicitlyConfigured("discovered"))
	assert.False(t, manager.isExplicitlyConfigured("other"))
}

func TestRetentionInvalidDiscoveryPreservesGroup(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]*string)
	}{
		{"invalid min", func(tags map[string]*string) { tags["min"] = ptr.To("invalid") }},
		{"nil min", func(tags map[string]*string) { tags["min"] = nil }},
		{"missing min", func(tags map[string]*string) { delete(tags, "min") }},
		{"negative min", func(tags map[string]*string) { tags["min"] = ptr.To("-1") }},
		{"invalid max", func(tags map[string]*string) { tags["max"] = ptr.To("invalid") }},
		{"nil max", func(tags map[string]*string) { tags["max"] = nil }},
		{"missing max", func(tags map[string]*string) { delete(tags, "max") }},
		{"max below min", func(tags map[string]*string) { tags["max"] = ptr.To("-1") }},
		{"missing tags", func(tags map[string]*string) { clear(tags) }},
		{"missing selector", func(tags map[string]*string) { delete(tags, "cluster-autoscaler-name") }},
		{"changed selector", func(tags map[string]*string) { tags["cluster-autoscaler-name"] = ptr.To("another-cluster") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRetentionFixture(t, 3)
			f.vmss.Tags = retentionConfigVMSS("agents").Tags
			f.group.manager.explicitlyConfigured = nil
			f.group.manager.autoDiscoverySpecs = []labelAutoDiscoveryConfig{{Selector: map[string]string{"cluster-autoscaler-name": "test-cluster"}}}
			f.park(t, 0)
			f.resume(t, nil)
			require.NoError(t, f.kube.CoreV1().Nodes().Delete(t.Context(), "retention-0", metav1.DeleteOptions{}))
			require.NoError(t, f.group.manager.forceRefresh())
			before := f.group.manager.getNodeGroups()
			require.Len(t, before, 1)
			view, err := f.group.GetNodeGroupAccounting(t.Context())
			require.NoError(t, err)
			require.Equal(t, 3, view.TargetSize)
			require.Equal(t, 1, view.UpcomingInactiveNodes)
			test.mutate(f.vmss.Tags)
			require.Error(t, f.group.manager.forceRefresh())
			after := f.group.manager.getNodeGroups()
			require.Len(t, after, 1)
			require.Same(t, before[0], after[0])
			_, err = f.group.GetNodeGroupAccounting(t.Context())
			require.Error(t, err, "invalid discovery must invalidate the old observation")
			f.vmss.Tags = retentionConfigVMSS("agents").Tags
			require.NoError(t, f.group.manager.forceRefresh())
			view, err = f.group.GetNodeGroupAccounting(t.Context())
			require.NoError(t, err)
			require.Equal(t, 3, view.TargetSize)
			require.Equal(t, 1, view.UpcomingInactiveNodes)
		})
		t.Run(test.name+"/Delete", func(t *testing.T) {
			manager := newRetentionConfigManager(t)
			manager.config.NodeGroupScaleDownPolicies = nil
			manager.autoDiscoverySpecs = []labelAutoDiscoveryConfig{{Selector: map[string]string{"cluster-autoscaler-name": "test-cluster"}}}
			require.NoError(t, manager.fetchAutoNodeGroups())
			require.Len(t, manager.getNodeGroups(), 1)
			test.mutate(manager.azureCache.scaleSets["agents"].Tags)
			require.NoError(t, manager.fetchAutoNodeGroups())
			require.Empty(t, manager.getNodeGroups(), "Delete retains legacy discovery filtering")
		})
	}
}

func TestRetentionDiscoveryPolicyLossPreservesGroup(t *testing.T) {
	for _, invalidSize := range []bool{false, true} {
		t.Run(fmt.Sprint(invalidSize), func(t *testing.T) {
			manager := newRetentionConfigManager(t)
			manager.autoDiscoverySpecs = []labelAutoDiscoveryConfig{{Selector: map[string]string{"cluster-autoscaler-name": "test-cluster"}}}
			require.NoError(t, manager.fetchAutoNodeGroups())
			before := manager.getNodeGroups()
			require.Len(t, before, 1)
			manager.config.NodeGroupScaleDownPolicies = nil
			if invalidSize {
				manager.azureCache.scaleSets["agents"].Tags["min"] = ptr.To("invalid")
			}
			require.Error(t, manager.fetchAutoNodeGroups())
			after := manager.getNodeGroups()
			require.Len(t, after, 1)
			require.Same(t, before[0], after[0])
		})
	}
}

func newRetentionConfigManager(t *testing.T) *AzureManager {
	t.Helper()
	return &AzureManager{
		autoscalerOptions: retentionTestOptions(),
		config: &Config{
			Config:                     providerazureconfig.Config{VMType: "vmss", ResourceGroup: "rg"},
			NodeGroupScaleDownPolicies: ScaleDownPolicies{"AGENTS": "Deallocate"},
		},
		azClient:             &azClient{vmssClientForDelete: NewMockVMSSDeleteClient(gomock.NewController(t))},
		explicitlyConfigured: make(map[string]bool),
		azureCache: &azureCache{
			refreshInterval: time.Minute,
			scaleSets:       map[string]*armcompute.VirtualMachineScaleSet{"agents": retentionConfigVMSS("agents")},
		},
	}
}

func retentionConfigVMSS(name string) *armcompute.VirtualMachineScaleSet {
	return &armcompute.VirtualMachineScaleSet{
		Name: ptr.To(name),
		SKU:  &armcompute.SKU{Capacity: ptr.To(int64(3))},
		Tags: map[string]*string{
			"min": ptr.To("0"), "max": ptr.To("10"), "cluster-autoscaler-name": ptr.To("test-cluster"),
		},
		Properties: &armcompute.VirtualMachineScaleSetProperties{
			OrchestrationMode: ptr.To(armcompute.OrchestrationModeUniform),
			VirtualMachineProfile: &armcompute.VirtualMachineScaleSetVMProfile{
				Priority: ptr.To(armcompute.VirtualMachinePriorityTypesRegular),
				StorageProfile: &armcompute.VirtualMachineScaleSetStorageProfile{
					OSDisk: &armcompute.VirtualMachineScaleSetOSDisk{
						ManagedDisk: &armcompute.VirtualMachineScaleSetManagedDiskParameters{},
					},
				},
			},
		},
	}
}
