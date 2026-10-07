/*
Copyright (c) Microsoft Corporation.

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
		Kubeconfig: "/explicit/kubeconfig", RunID: "run",
		SubscriptionID: "00000000-0000-0000-0000-000000000001", ResourceGroup: "workers",
		AutoscalerNamespace: "default", AutoscalerDeployment: "autoscaler", AutoscalerContainer: "autoscaler",
		ExpectedImage: "candidate:tag", MainPool: "main", ZeroPool: "zero",
		PoolLabel: "acceptance-pool", MainLabel: "main", ZeroLabel: "zero",
		DemandCPU: "1200m", WorkloadImage: "busybox@sha256:abc",
	}
}

func TestLoadConfig(t *testing.T) {
	for _, tt := range []struct {
		name, body string
	}{
		{name: "unknown field", body: `{"kubeconfigPath": "/tmp/kubeconfig"}`},
		{name: "multiple objects", body: `{} {}`},
		{name: "missing fields", body: `{}`},
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
		{name: "missing run ID", change: func(c *Config) { c.RunID = " " }},
		{name: "relative kubeconfig", change: func(c *Config) { c.Kubeconfig = "config" }},
		{name: "same pool", change: func(c *Config) { c.ZeroPool = "MAIN" }},
		{name: "same label", change: func(c *Config) { c.ZeroLabel = c.MainLabel }},
		{name: "nonpositive demand", change: func(c *Config) { c.DemandCPU = "0" }},
		{name: "invalid demand", change: func(c *Config) { c.DemandCPU = "a lot" }},
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
