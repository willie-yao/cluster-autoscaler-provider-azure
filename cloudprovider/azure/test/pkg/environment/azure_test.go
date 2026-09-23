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
