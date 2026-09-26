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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	"k8s.io/utils/ptr"
)

type sdkTransport func(*http.Request) (*http.Response, error)

func (f sdkTransport) Do(request *http.Request) (*http.Response, error) { return f(request) }

func TestAzureReadChecksReceivedPageBounds(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		zeroCapacity int64
		bounds       bool
	}{
		{name: "Updating main before over-max zero", zeroCapacity: 2, bounds: true},
		{name: "Updating main with valid zero capacity", zeroCapacity: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			page := armcompute.VirtualMachineScaleSetListResult{
				Value: []*armcompute.VirtualMachineScaleSet{
					{Name: ptr.To(c.MainPool), SKU: &armcompute.SKU{Capacity: ptr.To(int64(2))},
						Tags: map[string]*string{RunLabel: ptr.To(c.RunID), "cluster-autoscaler-name": ptr.To(c.DiscoveryValue),
							"min": ptr.To("1"), "max": ptr.To("2")},
						Properties: &armcompute.VirtualMachineScaleSetProperties{ProvisioningState: ptr.To("Updating"), Overprovision: ptr.To(false)}},
					{Name: ptr.To(c.ZeroPool), SKU: &armcompute.SKU{Capacity: ptr.To(tt.zeroCapacity)},
						Tags: map[string]*string{RunLabel: ptr.To(c.RunID), "cluster-autoscaler-name": ptr.To(c.DiscoveryValue),
							"min": ptr.To("0"), "max": ptr.To("1")},
						Properties: &armcompute.VirtualMachineScaleSetProperties{ProvisioningState: ptr.To("Succeeded"), Overprovision: ptr.To(false)}},
				},
				NextLink: ptr.To("https://example.invalid/unread-page"),
			}
			body, err := json.Marshal(page)
			if err != nil {
				t.Fatal(err)
			}
			requests := 0
			sets, err := armcompute.NewVirtualMachineScaleSetsClient(c.SubscriptionID, &fake.TokenCredential{}, &arm.ClientOptions{
				ClientOptions: azcore.ClientOptions{Transport: sdkTransport(func(request *http.Request) (*http.Response, error) {
					requests++
					if requests != 1 || request.Method != http.MethodGet ||
						request.URL.Path != c.resourcePrefix()+"/providers/Microsoft.Compute/virtualMachineScaleSets" {
						return nil, fmt.Errorf("unexpected SDK request")
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
						Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
				})},
			})
			if err != nil {
				t.Fatal(err)
			}
			cloud := &azureCloud{config: c, sets: sets}
			_, err = cloud.Read(context.Background())
			if err == nil || errors.Is(err, ErrBounds) != tt.bounds || requests != 1 {
				t.Fatalf("Read error=%v, bounds=%t, SDK requests=%d", err, tt.bounds, requests)
			}
			if !tt.bounds && !strings.Contains(err.Error(), "VMSS main must be Succeeded") {
				t.Fatalf("expected ordinary Updating convergence error, got %v", err)
			}
		})
	}
}

func TestAzureReadBalancePoolBoundBeforeConvergence(t *testing.T) {
	t.Parallel()
	c := testConfig()
	c.Phase, c.BalancePoolA, c.BalancePoolB, c.BalanceLabel = "balance", "pair-a", "pair-b", "balanced"
	page := armcompute.VirtualMachineScaleSetListResult{
		Value: []*armcompute.VirtualMachineScaleSet{
			{Name: ptr.To(c.MainPool), SKU: &armcompute.SKU{Capacity: ptr.To(int64(1))},
				Tags:       map[string]*string{RunLabel: ptr.To(c.RunID), "cluster-autoscaler-name": ptr.To(c.DiscoveryValue)},
				Properties: &armcompute.VirtualMachineScaleSetProperties{ProvisioningState: ptr.To("Updating")}},
			{Name: ptr.To(c.BalancePoolA), SKU: &armcompute.SKU{Capacity: ptr.To(int64(3))},
				Tags: map[string]*string{RunLabel: ptr.To(c.RunID), "cluster-autoscaler-name": ptr.To(c.DiscoveryValue)}},
		},
		NextLink: ptr.To("https://example.invalid/unread-page"),
	}
	body, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	sets, err := armcompute.NewVirtualMachineScaleSetsClient(c.SubscriptionID, &fake.TokenCredential{}, &arm.ClientOptions{
		ClientOptions: azcore.ClientOptions{Transport: sdkTransport(func(request *http.Request) (*http.Response, error) {
			requests++
			if requests != 1 {
				return nil, fmt.Errorf("unexpected second page")
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	cloud := &azureCloud{config: c, sets: sets}
	if _, err := cloud.Read(context.Background()); !errors.Is(err, ErrBounds) || requests != 1 {
		t.Fatalf("balance bound before convergence = %v, requests=%d", err, requests)
	}
}

func TestAzureReadOptionalPoolBoundBeforeConvergence(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, phase, pool, label string
		capacity                 int64
	}{
		{name: "Spot pool", phase: "spot", pool: "spot-pool", label: "spot", capacity: 2},
		{name: "large pool", phase: "large", pool: "large-pool", label: "large", capacity: 51},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			c.Phase = tt.phase
			if c.Phase == "spot" {
				c.SpotPool, c.SpotLabel = tt.pool, tt.label
			} else {
				c.ScalePool, c.ScaleLabel = tt.pool, tt.label
			}
			page := armcompute.VirtualMachineScaleSetListResult{
				Value: []*armcompute.VirtualMachineScaleSet{
					{Name: ptr.To(c.MainPool), SKU: &armcompute.SKU{Capacity: ptr.To(int64(1))},
						Tags:       map[string]*string{RunLabel: ptr.To(c.RunID), "cluster-autoscaler-name": ptr.To(c.DiscoveryValue)},
						Properties: &armcompute.VirtualMachineScaleSetProperties{ProvisioningState: ptr.To("Updating")}},
					{Name: ptr.To(tt.pool), SKU: &armcompute.SKU{Capacity: ptr.To(tt.capacity)},
						Tags: map[string]*string{RunLabel: ptr.To(c.RunID), "cluster-autoscaler-name": ptr.To(c.DiscoveryValue)}},
				},
				NextLink: ptr.To("https://example.invalid/unread-page"),
			}
			body, err := json.Marshal(page)
			if err != nil {
				t.Fatal(err)
			}
			requests := 0
			sets, err := armcompute.NewVirtualMachineScaleSetsClient(c.SubscriptionID, &fake.TokenCredential{}, &arm.ClientOptions{
				ClientOptions: azcore.ClientOptions{Transport: sdkTransport(func(request *http.Request) (*http.Response, error) {
					requests++
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
						Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
				})},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (&azureCloud{config: c, sets: sets}).Read(context.Background()); !errors.Is(err, ErrBounds) || requests != 1 {
				t.Fatalf("optional pool bound before convergence = %v, requests=%d", err, requests)
			}
		})
	}
}

func TestSettledFailedPool(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, phase, pool, initial string
		states                     []string
		wantState                  string
		wantTimeout                bool
	}{
		{name: "failed pool settles after two retries", phase: "cse", pool: "failed-pool", initial: "Updating",
			states: []string{"Updating", "Updating", "Failed"}, wantState: "Failed"},
		{name: "failed pool stays updating", phase: "cse", pool: "failed-pool", initial: "Updating",
			states: []string{"Updating"}, wantTimeout: true},
		{name: "ordinary pool is unchanged", phase: "cse", pool: "main", initial: "Updating", wantState: "Updating"},
		{name: "default phase is unchanged", pool: "failed-pool", initial: "Updating", wantState: "Updating"},
		{name: "already failed pool needs no retry", phase: "cse", pool: "failed-pool", initial: "Failed", wantState: "Failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			c.Phase, c.FailurePool = tt.phase, "failed-pool"
			requests := 0
			sets, err := armcompute.NewVirtualMachineScaleSetsClient(c.SubscriptionID, &fake.TokenCredential{}, &arm.ClientOptions{
				ClientOptions: azcore.ClientOptions{Transport: sdkTransport(func(request *http.Request) (*http.Response, error) {
					requests++
					if request.Method != http.MethodGet || request.URL.Path != c.PoolID("failed-pool") || len(tt.states) == 0 {
						return nil, fmt.Errorf("unexpected failed VM pool request")
					}
					state := tt.states[min(requests-1, len(tt.states)-1)]
					body, err := json.Marshal(armcompute.VirtualMachineScaleSet{
						Name: ptr.To("failed-pool"),
						Properties: &armcompute.VirtualMachineScaleSetProperties{
							ProvisioningState: ptr.To(state),
						},
					})
					if err != nil {
						return nil, err
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
						Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
				})},
			})
			if err != nil {
				t.Fatal(err)
			}
			set := &armcompute.VirtualMachineScaleSet{Name: ptr.To(tt.pool),
				Properties: &armcompute.VirtualMachineScaleSetProperties{ProvisioningState: ptr.To(tt.initial)}}
			got, err := (&azureCloud{config: c, sets: sets}).settledFailedPool(context.Background(), set,
				2*time.Millisecond, 25*time.Millisecond)
			if (err != nil) != tt.wantTimeout {
				t.Fatalf("settledFailedPool error = %v, wantTimeout=%t", err, tt.wantTimeout)
			}
			if tt.wantTimeout {
				if !strings.Contains(err.Error(), "remained Updating") || requests < 2 {
					t.Fatalf("unbounded or unreported Updating state: error=%v, requests=%d", err, requests)
				}
			} else if got == nil || value(got.Properties.ProvisioningState) != tt.wantState {
				t.Fatalf("settledFailedPool = %v, want state %s", got, tt.wantState)
			}
			if len(tt.states) == 0 && requests != 0 {
				t.Fatalf("unexpected Azure read outside updating failed pool: %d", requests)
			}
		})
	}
}

func TestCheckSpotPool(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		change func(*armcompute.VirtualMachineScaleSet)
		valid  bool
	}{
		{name: "Spot Delete up to on-demand", valid: true},
		{name: "ordinary VM", change: func(s *armcompute.VirtualMachineScaleSet) {
			s.Properties.VirtualMachineProfile.Priority = nil
		}},
		{name: "deallocated on eviction", change: func(s *armcompute.VirtualMachineScaleSet) {
			s.Properties.VirtualMachineProfile.EvictionPolicy = ptr.To(armcompute.VirtualMachineEvictionPolicyTypesDeallocate)
		}},
		{name: "higher max price", change: func(s *armcompute.VirtualMachineScaleSet) {
			s.Properties.VirtualMachineProfile.BillingProfile.MaxPrice = ptr.To(0.20)
		}},
		{name: "Spot restore on", change: func(s *armcompute.VirtualMachineScaleSet) {
			s.Properties.SpotRestorePolicy = &armcompute.SpotRestorePolicy{Enabled: ptr.To(true)}
		}},
		{name: "wrong zero template label", change: func(s *armcompute.VirtualMachineScaleSet) {
			s.Tags["k8s.io_cluster-autoscaler_node-template_label_acceptance-pool"] = ptr.To("other")
		}},
		{name: "wrong SKU", change: func(s *armcompute.VirtualMachineScaleSet) {
			s.SKU.Name = ptr.To("Standard_B1ms")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			c.Phase, c.SpotPool, c.SpotLabel = "spot", "spot-pool", "spot"
			set := &armcompute.VirtualMachineScaleSet{
				SKU:   &armcompute.SKU{Name: ptr.To("Standard_D2s_v5")},
				Zones: []*string{ptr.To("1")},
				Tags:  map[string]*string{"k8s.io_cluster-autoscaler_node-template_label_acceptance-pool": ptr.To(c.SpotLabel)},
				Properties: &armcompute.VirtualMachineScaleSetProperties{
					VirtualMachineProfile: &armcompute.VirtualMachineScaleSetVMProfile{
						Priority:       ptr.To(armcompute.VirtualMachinePriorityTypesSpot),
						EvictionPolicy: ptr.To(armcompute.VirtualMachineEvictionPolicyTypesDelete),
						BillingProfile: &armcompute.BillingProfile{MaxPrice: ptr.To(-1.0)},
					},
				},
			}
			if tt.change != nil {
				tt.change(set)
			}
			if err := checkSpotPool(set, c); (err == nil) != tt.valid {
				t.Fatalf("CheckSpotPool = %v, valid=%t", err, tt.valid)
			}
		})
	}
}

func TestCheckFailedExtensionPool(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		edit  func(*armcompute.VirtualMachineScaleSet)
		valid bool
	}{
		{name: "run-owned unsuppressed CustomScript", valid: true},
		{name: "missing declaration", edit: func(set *armcompute.VirtualMachineScaleSet) {
			delete(set.Tags, "autoscaler-e2e-failing-extension")
		}},
		{name: "different extension publisher", edit: func(set *armcompute.VirtualMachineScaleSet) {
			set.Properties.VirtualMachineProfile.ExtensionProfile.Extensions[0].Properties.Publisher = ptr.To("other")
		}},
		{name: "suppressed failure", edit: func(set *armcompute.VirtualMachineScaleSet) {
			set.Properties.VirtualMachineProfile.ExtensionProfile.Extensions[0].Properties.SuppressFailures = ptr.To(true)
		}},
		{name: "two CustomScript extensions", edit: func(set *armcompute.VirtualMachineScaleSet) {
			ext := set.Properties.VirtualMachineProfile.ExtensionProfile.Extensions[0]
			set.Properties.VirtualMachineProfile.ExtensionProfile.Extensions = append(
				set.Properties.VirtualMachineProfile.ExtensionProfile.Extensions, ext)
		}},
		{name: "wrong instance SKU", edit: func(set *armcompute.VirtualMachineScaleSet) {
			set.SKU.Name = ptr.To("Standard_B1ms")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			c.Phase, c.FailurePool, c.FailureLabel = "cse", "failed", "failed"
			set := &armcompute.VirtualMachineScaleSet{
				SKU:   &armcompute.SKU{Name: ptr.To("Standard_D2s_v5")},
				Zones: []*string{ptr.To("1")},
				Tags: map[string]*string{
					"autoscaler-e2e-failing-extension":                             ptr.To(c.RunID),
					"k8s.io_cluster-autoscaler_node-template_label_" + c.PoolLabel: ptr.To(c.FailureLabel),
				},
				Properties: &armcompute.VirtualMachineScaleSetProperties{VirtualMachineProfile: &armcompute.VirtualMachineScaleSetVMProfile{
					ExtensionProfile: &armcompute.VirtualMachineScaleSetExtensionProfile{Extensions: []*armcompute.VirtualMachineScaleSetExtension{{
						Properties: &armcompute.VirtualMachineScaleSetExtensionProperties{
							Publisher: ptr.To("Microsoft.Azure.Extensions"), Type: ptr.To("CustomScript"),
						},
					}}},
				}},
			}
			if tt.edit != nil {
				tt.edit(set)
			}
			if err := checkFailedExtensionPool(set, c); (err == nil) != tt.valid {
				t.Fatalf("failed extension pool error=%v, valid=%t", err, tt.valid)
			}
		})
	}
}

func TestAzureReadAfterMissingPool(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name              string
		missingPoolExists bool
		valid             bool
	}{
		{name: "only two surviving scale sets", valid: true},
		{name: "target VMSS still exists", missingPoolExists: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			c.Phase, c.MissingPool, c.DeleteMissingPool = "missing-vmss", "missing-pool", true
			setPath := c.resourcePrefix() + "/providers/Microsoft.Compute/virtualMachineScaleSets"
			mainVM := c.PoolID(c.MainPool) + "/virtualMachines/0"
			sets := armcompute.VirtualMachineScaleSetListResult{Value: []*armcompute.VirtualMachineScaleSet{}}
			for _, name := range c.PoolNames() {
				if name == c.MissingPool && !tt.missingPoolExists {
					continue
				}
				minimum, maximum, capacity := "0", "0", int64(0)
				if name == c.MainPool {
					minimum, maximum, capacity = "1", "2", 1
				}
				sets.Value = append(sets.Value, &armcompute.VirtualMachineScaleSet{
					Name: ptr.To(name), SKU: &armcompute.SKU{Name: ptr.To("Standard_D2s_v5"), Capacity: ptr.To(capacity)},
					Tags: map[string]*string{RunLabel: ptr.To(c.RunID), "cluster-autoscaler-name": ptr.To(c.DiscoveryValue),
						"min": ptr.To(minimum), "max": ptr.To(maximum)},
					Properties: &armcompute.VirtualMachineScaleSetProperties{
						ProvisioningState: ptr.To("Succeeded"), Overprovision: ptr.To(false),
					},
				})
			}
			cpParts := strings.Split(c.ControlPlaneID, "/")
			responses := map[string]interface{}{
				setPath: sets,
				setPath + "/" + c.MainPool + "/virtualMachines": armcompute.VirtualMachineScaleSetVMListResult{
					Value: []*armcompute.VirtualMachineScaleSetVM{{
						ID: ptr.To(mainVM), Properties: &armcompute.VirtualMachineScaleSetVMProperties{
							NetworkProfile: &armcompute.NetworkProfile{NetworkInterfaces: []*armcompute.NetworkInterfaceReference{{
								ID: ptr.To(mainVM + "/networkInterfaces/nic"),
							}}},
						},
					}},
				},
				setPath + "/" + c.ZeroPool + "/virtualMachines":                     armcompute.VirtualMachineScaleSetVMListResult{},
				setPath + "/" + c.MissingPool + "/virtualMachines":                  armcompute.VirtualMachineScaleSetVMListResult{},
				c.resourcePrefix() + "/providers/Microsoft.Compute/virtualMachines": armcompute.VirtualMachineListResult{},
				"/subscriptions/" + c.SubscriptionID + "/resourceGroups/" + cpParts[4] +
					"/providers/Microsoft.Compute/virtualMachines/" + cpParts[8]: armcompute.VirtualMachine{
					Tags: map[string]*string{RunLabel: ptr.To(c.RunID)},
					Properties: &armcompute.VirtualMachineProperties{HardwareProfile: &armcompute.HardwareProfile{
						VMSize: ptr.To(armcompute.VirtualMachineSizeTypes("Standard_D2s_v5")),
					}},
				},
			}
			factory, err := armcompute.NewClientFactory(c.SubscriptionID, &fake.TokenCredential{}, &arm.ClientOptions{
				ClientOptions: azcore.ClientOptions{Transport: sdkTransport(func(request *http.Request) (*http.Response, error) {
					payload, exists := responses[request.URL.Path]
					if !exists {
						return nil, fmt.Errorf("unexpected SDK request %s", request.URL.Path)
					}
					body, err := json.Marshal(payload)
					if err != nil {
						t.Fatal(err)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
						Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
				})},
			})
			if err != nil {
				t.Fatal(err)
			}
			cloud := &azureCloud{config: c, sets: factory.NewVirtualMachineScaleSetsClient(),
				vms: factory.NewVirtualMachineScaleSetVMsClient(), other: factory.NewVirtualMachinesClient(),
				cores: map[string]int{"standard_d2s_v5": 2}}
			snapshot, err := cloud.readAfterMissing(context.Background())
			if (err == nil) != tt.valid {
				t.Fatalf("read after deletion error=%v, valid=%t", err, tt.valid)
			}
			if tt.valid {
				if len(snapshot.Pools) != 2 || snapshot.VMs != 2 || snapshot.VCPUs != 4 {
					t.Fatalf("surviving pool observation=%+v", snapshot)
				}
				if err := snapshot.StableAfterMissing(c, 1, 0); err == nil {
					t.Fatal("without Node evidence stable read must not pass")
				}
			}
		})
	}
}

func TestCheckPhasePeakEnvelope(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, phase string
		spot, scale int
		valid       bool
	}{
		{name: "Spot at four two-core VMs", phase: "spot", spot: 2, valid: true},
		{name: "Spot SKU over cap", phase: "spot", spot: 4},
		{name: "Spot eviction at four two-core VMs", phase: "spot-eviction", spot: 2, valid: true},
		{name: "failed VM plus two main workers", phase: "cse", spot: 2, valid: true},
		{name: "oversized failed VM", phase: "cse", spot: 4},
		{name: "two main workers and empty missing VMSS", phase: "missing-vmss", valid: true},
		{name: "two main workers and empty zero pool", phase: "local-storage", valid: true},
		{name: "fifty B1ms and two D2s", phase: "large", scale: 1, valid: true},
		{name: "fifty two-core workers exceed cap", phase: "large", scale: 2},
		{name: "missing pool cores", phase: "large"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			c.Phase = tt.phase
			cores := map[string]int{c.MainPool: 2, c.ZeroPool: 2}
			switch tt.phase {
			case "spot", "spot-eviction":
				c.SpotPool, c.SpotLabel = "spot-pool", "spot"
				cores[c.SpotPool] = tt.spot
			case "cse":
				c.FailurePool, c.FailureLabel = "failed-pool", "failed"
				cores[c.FailurePool] = tt.spot
			case "missing-vmss":
				c.MissingPool, c.DeleteMissingPool = "missing-pool", true
				cores[c.MissingPool] = 2
			case "large":
				c.ScalePool, c.ScaleLabel = "large-pool", "large"
				cores[c.ScalePool] = tt.scale
			}
			if err := checkPhasePeakEnvelope(c, cores, 2); (err == nil) != tt.valid {
				t.Fatalf("phase peak = %v, valid=%t", err, tt.valid)
			}
		})
	}
}

func TestAzureReadNoJoinRequiresOperatorTag(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, tag string
		valid     bool
	}{
		{name: "missing tag"},
		{name: "wrong run", tag: "other"},
		{name: "operator declaration", tag: "owned-run", valid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			c.Phase = "no-join"
			tags := map[string]*string{RunLabel: ptr.To(c.RunID), "cluster-autoscaler-name": ptr.To(c.DiscoveryValue),
				"min": ptr.To("0"), "max": ptr.To("1")}
			if tt.tag != "" {
				tags["autoscaler-e2e-no-join"] = ptr.To(tt.tag)
			}
			page := armcompute.VirtualMachineScaleSetListResult{Value: []*armcompute.VirtualMachineScaleSet{{
				Name: ptr.To(c.ZeroPool), SKU: &armcompute.SKU{Name: ptr.To("Standard_D2s_v5"), Capacity: ptr.To(int64(0))}, Tags: tags,
				Properties: &armcompute.VirtualMachineScaleSetProperties{
					ProvisioningState: ptr.To("Succeeded"), Overprovision: ptr.To(false),
				},
			}}}
			setBody, err := json.Marshal(page)
			if err != nil {
				t.Fatal(err)
			}
			instanceBody, err := json.Marshal(armcompute.VirtualMachineScaleSetVMListResult{})
			if err != nil {
				t.Fatal(err)
			}
			requests := 0
			factory, err := armcompute.NewClientFactory(c.SubscriptionID, &fake.TokenCredential{}, &arm.ClientOptions{
				ClientOptions: azcore.ClientOptions{Transport: sdkTransport(func(request *http.Request) (*http.Response, error) {
					requests++
					body := setBody
					if requests == 2 {
						body = instanceBody
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
						Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
				})},
			})
			if err != nil {
				t.Fatal(err)
			}
			cloud := &azureCloud{config: c, sets: factory.NewVirtualMachineScaleSetsClient(),
				vms: factory.NewVirtualMachineScaleSetVMsClient(), cores: map[string]int{"standard_d2s_v5": 2}}
			_, err = cloud.Read(context.Background())
			if tt.valid {
				if err == nil || !strings.Contains(err.Error(), "expected exactly 2 authorized scale sets") || requests != 2 {
					t.Fatalf("declared no-join tag did not pass preflight: error=%v requests=%d", err, requests)
				}
			} else if err == nil || !strings.Contains(err.Error(), "operator's run-owned no-join tag") || requests != 1 {
				t.Fatalf("undeclared no-join fixture accepted: error=%v requests=%d", err, requests)
			}
		})
	}
}

func TestAzureReadNoJoinRequiresVMID(t *testing.T) {
	t.Parallel()
	c := testConfig()
	c.Phase = "no-join"
	setPath := c.resourcePrefix() + "/providers/Microsoft.Compute/virtualMachineScaleSets"
	instancePath := setPath + "/" + c.ZeroPool + "/virtualMachines"
	set := armcompute.VirtualMachineScaleSetListResult{Value: []*armcompute.VirtualMachineScaleSet{{
		Name: ptr.To(c.ZeroPool), SKU: &armcompute.SKU{Name: ptr.To("Standard_D2s_v5"), Capacity: ptr.To(int64(1))},
		Tags: map[string]*string{RunLabel: ptr.To(c.RunID), "cluster-autoscaler-name": ptr.To(c.DiscoveryValue),
			"min": ptr.To("0"), "max": ptr.To("1"), "autoscaler-e2e-no-join": ptr.To(c.RunID)},
		Properties: &armcompute.VirtualMachineScaleSetProperties{
			ProvisioningState: ptr.To("Succeeded"), Overprovision: ptr.To(false),
		},
	}}}
	identifier := c.PoolID(c.ZeroPool) + "/virtualMachines/0"
	instance := armcompute.VirtualMachineScaleSetVMListResult{Value: []*armcompute.VirtualMachineScaleSetVM{{
		ID: ptr.To(identifier), Properties: &armcompute.VirtualMachineScaleSetVMProperties{
			NetworkProfile: &armcompute.NetworkProfile{
				NetworkInterfaces: []*armcompute.NetworkInterfaceReference{{ID: ptr.To(identifier + "/networkInterfaces/nic")}},
			},
		},
	}}}
	data := map[string]interface{}{setPath: set, instancePath: instance}
	factory, err := armcompute.NewClientFactory(c.SubscriptionID, &fake.TokenCredential{}, &arm.ClientOptions{
		ClientOptions: azcore.ClientOptions{Transport: sdkTransport(func(request *http.Request) (*http.Response, error) {
			payload, found := data[request.URL.Path]
			if !found {
				return nil, fmt.Errorf("unexpected Azure SDK request %s", request.URL.Path)
			}
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	cloud := &azureCloud{config: c, sets: factory.NewVirtualMachineScaleSetsClient(),
		vms: factory.NewVirtualMachineScaleSetVMsClient(), cores: map[string]int{"standard_d2s_v5": 2}}
	if _, err := cloud.Read(context.Background()); err == nil || !strings.Contains(err.Error(), "unique VM ID") {
		t.Fatalf("missing VM generation was accepted: %v", err)
	}
}

func TestCheckBalanceTemplateTags(t *testing.T) {
	t.Parallel()
	labels := "k8s.io_cluster-autoscaler_node-template_label_acceptance-pool"
	taints := "k8s.io_cluster-autoscaler_node-template_taint_dedicated"
	a := map[string]*string{labels: ptr.To("balanced"), taints: ptr.To("test:NoSchedule")}
	b := map[string]*string{labels: ptr.To("balanced"), taints: ptr.To("test:NoSchedule")}
	if err := checkBalanceTemplateTags(a, b); err != nil {
		t.Fatal(err)
	}
	b[taints] = ptr.To("other:NoSchedule")
	if err := checkBalanceTemplateTags(a, b); err == nil {
		t.Fatal("accepted different taint tags")
	}
	delete(b, taints)
	if err := checkBalanceTemplateTags(a, b); err == nil {
		t.Fatal("accepted a missing taint tag")
	}
	b[taints] = ptr.To("test:NoSchedule")
	a["k8s.io_cluster-autoscaler_node-template_label_empty"] = ptr.To("")
	if err := checkBalanceTemplateTags(a, b); err == nil {
		t.Fatal("accepted a missing empty-valued label tag")
	}
	b["k8s.io_cluster-autoscaler_node-template_label_empty"] = ptr.To("")
	if err := checkBalanceTemplateTags(a, b); err != nil {
		t.Fatal(err)
	}
	delete(a, "k8s.io_cluster-autoscaler_node-template_label_empty")
	delete(b, "k8s.io_cluster-autoscaler_node-template_label_empty")
	b[taints] = ptr.To("test:NoSchedule")
	b["k8s.io_cluster-autoscaler_node-template_resources_gpu"] = ptr.To("1")
	if err := checkBalanceTemplateTags(a, b); err == nil {
		t.Fatal("accepted an extra resource template tag")
	}
}

func TestImageReferenceIncludesResolvedVersion(t *testing.T) {
	t.Parallel()
	a := &armcompute.ImageReference{Publisher: ptr.To("test"), Offer: ptr.To("linux"),
		SKU: ptr.To("vm"), Version: ptr.To("latest"), ExactVersion: ptr.To("1.0.0")}
	b := *a
	if imageReference(a) == "" || imageReference(a) != imageReference(&b) {
		t.Fatal("matching returned image references differ")
	}
	b.ExactVersion = ptr.To("1.0.1")
	if imageReference(a) == imageReference(&b) {
		t.Fatal("different resolved image versions matched")
	}
	if imageReference(nil) != "" || imageReference(&armcompute.ImageReference{}) != "" {
		t.Fatal("missing image reference must not pass balance preflight")
	}
}

func TestAzureInstanceRunning(t *testing.T) {
	t.Parallel()
	c := testConfig()
	path := c.PoolID(c.ZeroPool) + "/virtualMachines/0/instanceView"
	for _, tt := range []struct {
		name, power string
		running     bool
	}{
		{name: "running", power: "PowerState/running", running: true},
		{name: "starting", power: "PowerState/starting"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body, err := json.Marshal(armcompute.VirtualMachineScaleSetVMInstanceView{
				Statuses: []*armcompute.InstanceViewStatus{{Code: ptr.To(tt.power)}},
			})
			if err != nil {
				t.Fatal(err)
			}
			requests := 0
			factory, err := armcompute.NewClientFactory(c.SubscriptionID, &fake.TokenCredential{}, &arm.ClientOptions{
				ClientOptions: azcore.ClientOptions{Transport: sdkTransport(func(request *http.Request) (*http.Response, error) {
					requests++
					if request.Method != http.MethodGet || !strings.EqualFold(request.URL.Path, path) {
						t.Fatalf("unexpected instance view request %s %s", request.Method, request.URL.Path)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
						Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
				})},
			})
			if err != nil {
				t.Fatal(err)
			}
			cloud := &azureCloud{config: c, vms: factory.NewVirtualMachineScaleSetVMsClient()}
			running, err := cloud.instanceRunning(context.Background(), c.ZeroPool, c.PoolID(c.ZeroPool)+"/virtualMachines/0")
			if err != nil || running != tt.running || requests != 1 {
				t.Fatalf("instance running=%t error=%v requests=%d", running, err, requests)
			}
			if _, err := cloud.instanceRunning(context.Background(), c.ZeroPool, c.PoolID(c.MainPool)+"/virtualMachines/0"); err == nil || requests != 1 {
				t.Fatal("accepted an instance in another pool")
			}
		})
	}
}

func TestAzureReadChecksReceivedInstancePageBounds(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		pages      [][]string
		missingNIC string
		bounds     bool
		requests   int
		errorText  string
	}{
		{
			name: "over-limit page before unread NextLink", pages: [][]string{{"0", "1", "2"}},
			bounds: true, requests: 2,
		},
		{
			name: "over-limit page before incomplete NIC", pages: [][]string{{"0", "1", "2"}},
			missingNIC: "0", bounds: true, requests: 2,
		},
		{
			name: "over-limit across pages", pages: [][]string{{"0", "1"}, {"2"}},
			bounds: true, requests: 3,
		},
		{
			name: "empty partial page before over-limit page", pages: [][]string{{}, {"0", "1", "2"}},
			bounds: true, requests: 3,
		},
		{
			name: "within-limit incomplete NIC", pages: [][]string{{"0", "1"}},
			missingNIC: "0", requests: 2, errorText: "lacks identity/network evidence",
		},
		{
			name: "within-limit read failure", pages: [][]string{{"0", "1"}},
			requests: 3, errorText: "list VMSS instances",
		},
		{
			name: "duplicate normalized identities across pages", pages: [][]string{{"a", "b"}, {"A", "B"}},
			requests: 4, errorText: "list VMSS instances",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			setPath := c.resourcePrefix() + "/providers/Microsoft.Compute/virtualMachineScaleSets"
			instancePath := setPath + "/" + c.MainPool + "/virtualMachines"
			responses := map[string]interface{}{
				setPath: armcompute.VirtualMachineScaleSetListResult{
					Value: []*armcompute.VirtualMachineScaleSet{{
						Name: ptr.To(c.MainPool), SKU: &armcompute.SKU{Capacity: ptr.To(int64(2))},
						Tags: map[string]*string{
							RunLabel: ptr.To(c.RunID), "cluster-autoscaler-name": ptr.To(c.DiscoveryValue),
							"min": ptr.To("1"), "max": ptr.To("2"),
						},
						Properties: &armcompute.VirtualMachineScaleSetProperties{
							ProvisioningState: ptr.To("Succeeded"), Overprovision: ptr.To(false),
						},
					}},
				},
			}
			for i, ids := range tt.pages {
				page := armcompute.VirtualMachineScaleSetVMListResult{
					Value:    []*armcompute.VirtualMachineScaleSetVM{},
					NextLink: ptr.To(fmt.Sprintf("https://management.azure.com/instance-pages/%d", i+1)),
				}
				for _, id := range ids {
					vm := &armcompute.VirtualMachineScaleSetVM{ID: ptr.To(c.PoolID(c.MainPool) + "/virtualMachines/" + id)}
					if id != tt.missingNIC {
						vm.Properties = &armcompute.VirtualMachineScaleSetVMProperties{
							NetworkProfile: &armcompute.NetworkProfile{
								NetworkInterfaces: []*armcompute.NetworkInterfaceReference{{ID: ptr.To(*vm.ID + "/nic")}},
							},
						}
					}
					page.Value = append(page.Value, vm)
				}
				path := instancePath
				if i > 0 {
					path = fmt.Sprintf("/instance-pages/%d", i)
				}
				responses[path] = page
			}
			var requests int
			factory, err := armcompute.NewClientFactory(c.SubscriptionID, &fake.TokenCredential{}, &arm.ClientOptions{
				ClientOptions: azcore.ClientOptions{
					Retry: policy.RetryOptions{MaxRetries: -1},
					Transport: sdkTransport(func(request *http.Request) (*http.Response, error) {
						requests++
						if request.Method != http.MethodGet {
							t.Fatalf("unexpected SDK method %s", request.Method)
						}
						status := http.StatusOK
						response, ok := responses[request.URL.Path]
						if !ok {
							if request.URL.Path != fmt.Sprintf("/instance-pages/%d", len(tt.pages)) {
								t.Fatalf("unexpected SDK path %s", request.URL.Path)
							}
							status = http.StatusServiceUnavailable
							response = map[string]interface{}{"error": map[string]string{"code": "FixtureReadFailure"}}
						}
						body, err := json.Marshal(response)
						if err != nil {
							t.Fatal(err)
						}
						return &http.Response{
							StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
							Body: io.NopCloser(bytes.NewReader(body)), Request: request,
						}, nil
					}),
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			cloud := &azureCloud{
				config: c, sets: factory.NewVirtualMachineScaleSetsClient(), vms: factory.NewVirtualMachineScaleSetVMsClient(),
			}
			_, err = cloud.Read(context.Background())
			if err == nil || errors.Is(err, ErrBounds) != tt.bounds || requests != tt.requests {
				t.Fatalf("Read error=%v, want bounds=%t and SDK requests=%d, got %d", err, tt.bounds, tt.requests, requests)
			}
			if tt.errorText != "" && !strings.Contains(err.Error(), tt.errorText) {
				t.Fatalf("expected ordinary %q error, got %v", tt.errorText, err)
			}
		})
	}
}

func TestAzureCloudNICExists(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		status    int
		transport error
		exists    bool
		wantError bool
	}{
		{name: "NIC exists", status: http.StatusOK, exists: true},
		{name: "NIC deleted", status: http.StatusNotFound},
		{name: "authorization error is not absence", status: http.StatusForbidden, wantError: true},
		{name: "transport error is not absence", transport: errors.New("transport unavailable"), wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			nicID := c.PoolID(c.MainPool) + "/virtualMachines/0/networkInterfaces/nic"
			requests := 0
			armClient, err := arm.NewClient("autoscaler-e2e", "v0.0.0", &fake.TokenCredential{}, &arm.ClientOptions{
				ClientOptions: azcore.ClientOptions{
					Retry: policy.RetryOptions{MaxRetries: -1},
					Transport: sdkTransport(func(request *http.Request) (*http.Response, error) {
						requests++
						if request.Method != http.MethodGet || request.URL.Path != nicID ||
							request.URL.Host != "management.azure.com" || request.URL.RawQuery != "api-version=2018-10-01" {
							t.Fatalf("unexpected NIC request: %s %s", request.Method, request.URL)
						}
						if tt.transport != nil {
							return nil, tt.transport
						}
						return &http.Response{
							StatusCode: tt.status, Header: http.Header{"Content-Type": []string{"application/json"}},
							Body: io.NopCloser(strings.NewReader("{}")), Request: request,
						}, nil
					}),
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			exists, err := (&azureCloud{config: c, arm: armClient}).NICExists(context.Background(), nicID)
			if exists != tt.exists || (err != nil) != tt.wantError || requests != 1 {
				t.Fatalf("NICExists() exists=%t, error=%v, requests=%d; want exists=%t, error=%t, requests=1",
					exists, err, requests, tt.exists, tt.wantError)
			}
		})
	}
}

func TestCheckScaleDownTags(t *testing.T) {
	t.Parallel()
	const prefix = "k8s.io_cluster-autoscaler_node-template_autoscaling-options_"
	for _, tt := range []struct {
		name, unneeded, utilization string
		valid                       bool
	}{
		{name: "global settings", valid: true},
		{name: "bounded pool overrides", unneeded: "30s", utilization: "0.50", valid: true},
		{name: "provider lowercases duration", unneeded: "1M", valid: true},
		{name: "longer than negative observation", unneeded: "10m"},
		{name: "negative duration", unneeded: "-1m"},
		{name: "invalid duration", unneeded: "soon"},
		{name: "conflicting utilization", utilization: "0.1"},
		{name: "invalid utilization", utilization: "NaN"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tags := map[string]*string{}
			if tt.unneeded != "" {
				tags[prefix+"scaledownunneededtime"] = ptr.To(tt.unneeded)
			}
			if tt.utilization != "" {
				tags[prefix+"scaledownutilizationthreshold"] = ptr.To(tt.utilization)
			}
			if err := checkScaleDownTags(tags); (err == nil) != tt.valid {
				t.Fatalf("tag error=%v, valid=%v", err, tt.valid)
			}
		})
	}
}

func TestCheckPeakEnvelope(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name           string
		main, zero, cp int
		valid          bool
		bounds         bool
	}{
		{name: "four two-core VMs at maximum", main: 2, zero: 2, cp: 2, valid: true},
		{name: "idle zero pool hides sixteen-core SKU", main: 2, zero: 16, cp: 2, bounds: true},
		{name: "main peak exceeds current one-worker count", main: 4, zero: 2, cp: 2, bounds: true},
		{name: "control plane included", main: 2, zero: 2, cp: 4, bounds: true},
		{name: "unknown zero pool cores", main: 2, zero: 0, cp: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := checkPeakEnvelope(tt.main, tt.zero, tt.cp)
			if (err == nil) != tt.valid || errors.Is(err, ErrBounds) != tt.bounds {
				t.Fatalf("peak error=%v, valid=%v", err, tt.valid)
			}
		})
	}
}
