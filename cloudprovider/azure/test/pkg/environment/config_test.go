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

package environment

import (
	"os"
	"path/filepath"
	"testing"
)

func testConfig() Config {
	return Config{
		Kubeconfig: "/explicit/kubeconfig", Context: "disposable", ClusterUID: "cluster-uid", RunID: "owned-run",
		SubscriptionID: "00000000-0000-0000-0000-000000000001", ResourceGroup: "workers", Location: "westus2",
		ControlPlaneID: "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/control/providers/Microsoft.Compute/virtualMachines/cp",
		DiscoveryValue: "owned-run", AutoscalerNamespace: "kube-system", AutoscalerDeployment: "autoscaler",
		AutoscalerContainer: "autoscaler", LeaseName: "cluster-autoscaler", ExpectedImage: "candidate@sha256:abc",
		MainPool: "main", ZeroPool: "zero", PoolLabel: "acceptance-pool", MainLabel: "main", ZeroLabel: "zero",
		DemandCPU: "1200m", WorkloadImage: "busybox@sha256:abc",
	}
}

func TestLoadConfig(t *testing.T) {
	for _, tt := range []struct {
		name, body string
	}{
		{name: "unknown input", body: `{"kubeconfigPath": "/tmp/kubeconfig"}`},
		{name: "multiple objects", body: `{} {}`},
		{name: "missing inputs", body: `{}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "environment.json")
			if err := os.WriteFile(path, []byte(tt.body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(path); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		change func(*Config)
	}{
		{name: "missing cluster fingerprint", change: func(c *Config) { c.ClusterUID = "" }},
		{name: "ambient kubeconfig", change: func(c *Config) { c.Kubeconfig = "config" }},
		{name: "ambiguous pool", change: func(c *Config) { c.ZeroPool = c.MainPool }},
		{name: "ambiguous labels", change: func(c *Config) { c.ZeroLabel = c.MainLabel }},
		{name: "foreign control plane", change: func(c *Config) {
			c.ControlPlaneID = "/subscriptions/foreign/resourceGroups/a/providers/Microsoft.Compute/virtualMachines/cp"
		}},
		{name: "resource path injection", change: func(c *Config) { c.ResourceGroup = "workers/other" }},
		{name: "nonpositive demand", change: func(c *Config) { c.DemandCPU = "0" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			tt.change(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}
