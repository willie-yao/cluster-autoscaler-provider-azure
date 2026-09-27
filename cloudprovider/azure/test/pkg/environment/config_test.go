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

	"k8s.io/utils/ptr"
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
		{name: "spot pool not bound", change: func(c *Config) { c.Phase = "spot" }},
		{name: "Spot pool overlaps main", change: func(c *Config) {
			c.Phase, c.SpotPool, c.SpotLabel = "spot", "MAIN", "spot"
		}},
		{name: "Spot label overlaps zero", change: func(c *Config) {
			c.Phase, c.SpotPool, c.SpotLabel = "spot", "spot-pool", c.ZeroLabel
		}},
		{name: "scale pool not bound", change: func(c *Config) { c.Phase = "large" }},
		{name: "scale pool in Spot phase", change: func(c *Config) {
			c.Phase, c.SpotPool, c.SpotLabel, c.ScalePool = "spot", "spot-pool", "spot", "large-pool"
		}},
		{name: "scale pool without phase", change: func(c *Config) { c.ScalePool = "large-pool" }},
		{name: "failed VM missing provision time", change: func(c *Config) {
			c.Phase, c.FailurePool, c.FailureLabel = "cse", "failed-pool", "failed"
		}},
		{name: "failed VM short provision time", change: func(c *Config) {
			c.Phase, c.FailurePool, c.FailureLabel, c.MaxNodeProvisionTime = "cse", "failed-pool", "failed", "3m"
		}},
		{name: "failed VM long provision time", change: func(c *Config) {
			c.Phase, c.FailurePool, c.FailureLabel, c.MaxNodeProvisionTime = "cse", "failed-pool", "failed", "20m"
		}},
		{name: "failed VM time on default", change: func(c *Config) { c.MaxNodeProvisionTime = "15m" }},
		{name: "failed VM pool overlaps main", change: func(c *Config) {
			c.Phase, c.FailurePool, c.FailureLabel, c.MaxNodeProvisionTime = "cse", "MAIN", "failed", "15m"
		}},
		{name: "eviction not enabled", change: func(c *Config) {
			c.Phase, c.SpotPool, c.SpotLabel = "spot-eviction", "spot-pool", "spot"
		}},
		{name: "eviction enabled on ordinary Spot", change: func(c *Config) {
			c.Phase, c.SpotPool, c.SpotLabel, c.EvictSpot = "spot", "spot-pool", "spot", true
		}},
		{name: "missing VMSS deletion not enabled", change: func(c *Config) {
			c.Phase, c.MissingPool = "missing-vmss", "missing"
		}},
		{name: "foreign missing pool", change: func(c *Config) {
			c.Phase, c.MissingPool, c.DeleteMissingPool = "missing-vmss", "MAIN", true
		}},
		{name: "local storage setting omitted", change: func(c *Config) { c.Phase = "local-storage" }},
		{name: "local storage setting outside phase", change: func(c *Config) { c.SkipLocalStorage = ptr.To(true) }},
		{name: "deallocate hold outside phase", change: func(c *Config) { c.DeallocateHold = "2m" }},
		{name: "deallocate hold too short", change: func(c *Config) {
			c.Phase, c.DeallocateHold = "deallocate", "10s"
		}},
		{name: "deallocate hold too long", change: func(c *Config) {
			c.Phase, c.DeallocateHold = "deallocate-failed", "40m"
		}},
		{name: "invalid deallocate hold", change: func(c *Config) {
			c.Phase, c.DeallocateHold = "deallocate", "later"
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
		{name: "Spot", phase: "spot", names: 3, mainTagMin: 1, mainMax: 2, zeroMax: 0},
		{name: "large", phase: "large", names: 3, mainTagMin: 1, mainMax: 1, zeroMax: 0},
		{name: "failed VM", phase: "cse", names: 3, mainTagMin: 1, mainMax: 2, zeroMax: 0},
		{name: "Spot eviction", phase: "spot-eviction", names: 3, mainTagMin: 1, mainMax: 2, zeroMax: 0},
		{name: "missing VMSS", phase: "missing-vmss", names: 3, mainTagMin: 1, mainMax: 2, zeroMax: 0},
		{name: "local storage", phase: "local-storage", names: 2, mainTagMin: 1, mainMax: 2, zeroMax: 0},
		{name: "park and reuse", phase: "deallocate", names: 2, mainTagMin: 1, mainMax: 2, zeroMax: 0},
		{name: "failed registration", phase: "deallocate-failed", names: 2, mainTagMin: 1, mainMax: 3, zeroMax: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			c.Phase, c.BalancePoolA, c.BalancePoolB, c.BalanceLabel = tt.phase, "a", "b", "balanced"
			if tt.phase != "balance" {
				c.BalancePoolA, c.BalancePoolB, c.BalanceLabel = "", "", ""
			}
			if tt.phase == "spot" {
				c.SpotPool, c.SpotLabel = "spot-pool", "spot"
			}
			if tt.phase == "spot-eviction" {
				c.SpotPool, c.SpotLabel, c.EvictSpot = "spot-pool", "spot", true
			}
			if tt.phase == "large" {
				c.ScalePool, c.ScaleLabel = "large-pool", "large"
			}
			if tt.phase == "cse" {
				c.FailurePool, c.FailureLabel, c.MaxNodeProvisionTime = "failed-pool", "failed", "15m"
			}
			if tt.phase == "missing-vmss" {
				c.MissingPool, c.DeleteMissingPool = "missing-pool", true
			}
			if tt.phase == "local-storage" {
				c.SkipLocalStorage = ptr.To(false)
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
			if tt.phase == "deallocate" || tt.phase == "deallocate-failed" {
				if got := c.DeallocateNodeSpec(); len(got) != 1 || got[0] != "1:2:Deallocate:main" {
					t.Fatalf("unexpected Deallocate spec: %v", got)
				}
				if tt.phase == "deallocate-failed" && pools[c.MainPool].TagMax != 2 {
					t.Fatalf("failed registration must keep discovery max 2: %+v", pools[c.MainPool])
				}
			}
			vms, cpus := c.Limits()
			if tt.phase == "large" {
				if pools[c.ScalePool].Max != 50 || vms != 55 || cpus != 60 {
					t.Fatalf("large phase has wrong caps: pools=%+v VMs=%d vCPUs=%d", pools, vms, cpus)
				}
			} else if vms != MaxVMs || cpus != MaxVCPUs {
				t.Fatalf("ordinary phase limits changed: %d VMs, %d vCPUs", vms, cpus)
			}
		})
	}
}
