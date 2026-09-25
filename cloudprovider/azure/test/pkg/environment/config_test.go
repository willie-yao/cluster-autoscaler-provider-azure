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
		{name: "invalid disk class", change: func(c *Config) { c.DiskStorageClass = "wrong/class" }},
		{name: "unknown phase", change: func(c *Config) { c.Phase = "foreign" }},
		{name: "balance data without phase", change: func(c *Config) { c.BalancePoolA = "extra" }},
		{name: "missing balance pool", change: func(c *Config) {
			c.Phase, c.BalancePoolA, c.BalanceLabel = "balance", "a", "balanced"
		}},
		{name: "overlapping balance pool", change: func(c *Config) {
			c.Phase, c.BalancePoolA, c.BalancePoolB, c.BalanceLabel = "balance", "a", "MAIN", "balanced"
		}},
		{name: "overlapping balance label", change: func(c *Config) {
			c.Phase, c.BalancePoolA, c.BalancePoolB, c.BalanceLabel = "balance", "a", "b", "main"
		}},
		{name: "balance data in no-join phase", change: func(c *Config) {
			c.Phase, c.BalancePoolA = "no-join", "a"
		}},
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

func TestConfigPhasePools(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, phase string
		names       int
		mainTagMin  int
		mainMax     int
		zeroMax     int
	}{
		{name: "default", names: 2, mainTagMin: 1, mainMax: 2, zeroMax: 1},
		{name: "balance", phase: "balance", names: 4, mainTagMin: 1, mainMax: 1, zeroMax: 0},
		{name: "no-join", phase: "no-join", names: 2, mainTagMin: 1, mainMax: 2, zeroMax: 1},
		{name: "minimum", phase: "minimum", names: 2, mainTagMin: 2, mainMax: 2, zeroMax: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			c.Phase, c.BalancePoolA, c.BalancePoolB, c.BalanceLabel = tt.phase, "a", "b", "balanced"
			if tt.phase != "balance" {
				c.BalancePoolA, c.BalancePoolB, c.BalanceLabel = "", "", ""
			}
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			pools := c.Pools()
			if len(pools) != tt.names || len(c.PoolNames()) != tt.names ||
				pools[c.MainPool].TagMin != tt.mainTagMin || pools[c.MainPool].Max != tt.mainMax ||
				pools[c.ZeroPool].Max != tt.zeroMax {
				t.Fatalf("unexpected phase pools: %+v", pools)
			}
			if tt.phase == "minimum" && pools[c.MainPool].ObservedMin != 1 {
				t.Fatal("minimum phase must allow the below-minimum starting capacity")
			}
		})
	}
}
