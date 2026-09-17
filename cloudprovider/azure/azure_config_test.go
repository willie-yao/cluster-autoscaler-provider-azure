/*
Copyright 2020 The Kubernetes Authors.

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
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	providerazureconsts "sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

func TestCloudProviderAzureConsts(t *testing.T) {
	// Just detect user-facing breaking changes from cloud-provider-azure.
	// Shouldn't really change a lot, but just in case.
	assert.Equal(t, "vmss", providerazureconsts.VMTypeVMSS)
	assert.Equal(t, "standard", providerazureconsts.VMTypeStandard)
}

// Note: The previous tests for InitializeCloudProviderRateLimitConfig were removed
// because that function no longer exists in cloud-provider-azure v1.32.0+.
// The rate limit configuration has been restructured to use
// ratelimit.CloudProviderRateLimitConfig from pkg/azclient/policy/ratelimit
// with an Entries map for per-client configuration.

func TestBuildAzureConfigMigrationPrecedence(t *testing.T) {
	originalEnv := saveAndClearEnv()
	t.Cleanup(func() { loadEnv(originalEnv) })

	for _, tc := range []struct {
		name   string
		fields map[string]interface{}
		env    map[string]string
		check  func(*testing.T, *Config)
		err    string
	}{
		{
			name: "baseline defaults and empty environment",
			env:  map[string]string{"ARM_RESOURCE_GROUP": " ", "AZURE_ENABLE_FORCE_DELETE": "", "AZURE_VMSS_CACHE_TTL": " "},
			check: func(t *testing.T, cfg *Config) {
				assert.Equal(t, "fakeId", cfg.ResourceGroup)
				assert.Equal(t, 60, cfg.VmssCacheTTLInSeconds)
				assert.Equal(t, "vmss", cfg.VMType)
				assert.False(t, cfg.EnableForceDelete)
				assert.False(t, cfg.EnableVmssFlexNodes)
				assert.False(t, cfg.EnableVMsAgentPool)
			},
		},
		{
			name: "legacy file fields override modern fields",
			fields: map[string]interface{}{
				"vmssCacheTTL": 91, "vmssVmsCacheTTL": 92,
				"useFederatedWorkloadIdentityExtension": false, "useWorkloadIdentityExtension": true,
				"enableVmssFlexNodes": false, "enableVmssFlex": true,
			},
			check: func(t *testing.T, cfg *Config) {
				assert.Equal(t, 91, cfg.VmssCacheTTLInSeconds)
				assert.Equal(t, 92, cfg.VmssVirtualMachinesCacheTTLInSeconds)
				assert.True(t, cfg.UseFederatedWorkloadIdentityExtension)
				assert.True(t, cfg.EnableVmssFlexNodes)
			},
		},
		{
			name: "environment overrides legacy file fields",
			fields: map[string]interface{}{
				"vmssCacheTTL": 91, "vmssVmsCacheTTL": 92,
				"useWorkloadIdentityExtension": true, "enableVmssFlex": true,
			},
			env: map[string]string{
				"AZURE_VMSS_CACHE_TTL_IN_SECONDS": "101", "AZURE_VMSS_VMS_CACHE_TTL_IN_SECONDS": "102",
				"ARM_USE_FEDERATED_WORKLOAD_IDENTITY_EXTENSION": "false", "AZURE_ENABLE_VMSS_FLEX_NODES": "false",
			},
			check: func(t *testing.T, cfg *Config) {
				assert.Equal(t, 101, cfg.VmssCacheTTLInSeconds)
				assert.Equal(t, 102, cfg.VmssVirtualMachinesCacheTTLInSeconds)
				assert.False(t, cfg.UseFederatedWorkloadIdentityExtension)
				assert.False(t, cfg.EnableVmssFlexNodes)
			},
		},
		{
			name: "environment alias precedence",
			env: map[string]string{
				"ARM_TENANT_ID": "arm-tenant", "AZURE_TENANT_ID": "azure-tenant",
				"ARM_CLIENT_ID": "arm-client", "AZURE_CLIENT_ID": "azure-client",
				"AZURE_VMSS_CACHE_TTL_IN_SECONDS": "101", "AZURE_VMSS_CACHE_TTL": "201",
				"AZURE_VMSS_VMS_CACHE_TTL_IN_SECONDS": "102", "AZURE_VMSS_VMS_CACHE_TTL": "202",
				"ARM_USE_FEDERATED_WORKLOAD_IDENTITY_EXTENSION": "true", "ARM_USE_WORKLOAD_IDENTITY_EXTENSION": "false",
				"AZURE_ENABLE_VMSS_FLEX_NODES": "true", "AZURE_ENABLE_VMSS_FLEX": "false",
			},
			check: func(t *testing.T, cfg *Config) {
				assert.Equal(t, "azure-tenant", cfg.TenantID)
				assert.Equal(t, "azure-client", cfg.AADClientID)
				assert.Equal(t, 201, cfg.VmssCacheTTLInSeconds)
				assert.Equal(t, 202, cfg.VmssVirtualMachinesCacheTTLInSeconds)
				assert.False(t, cfg.UseFederatedWorkloadIdentityExtension)
				assert.False(t, cfg.EnableVmssFlexNodes)
			},
		},
		{
			name: "conflicting authentication is rejected",
			env:  map[string]string{"ARM_USE_MANAGED_IDENTITY_EXTENSION": "true", "ARM_USE_WORKLOAD_IDENTITY_EXTENSION": "true"},
			err:  "you can not combine both managed identity and workload identity",
		},
		{
			name: "invalid boolean is rejected",
			env:  map[string]string{"AZURE_ENABLE_FORCE_DELETE": "not-a-bool"},
			err:  "failed to parse AZURE_ENABLE_FORCE_DELETE",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fields map[string]interface{}
			require.NoError(t, json.Unmarshal([]byte(validAzureCfg), &fields))
			for key, value := range tc.fields {
				fields[key] = value
			}
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			data, err := json.Marshal(fields)
			require.NoError(t, err)
			cfg, err := BuildAzureConfig(strings.NewReader(string(data)))
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				assert.Nil(t, cfg)
				return
			}
			require.NoError(t, err)
			tc.check(t, cfg)
		})
	}
}
