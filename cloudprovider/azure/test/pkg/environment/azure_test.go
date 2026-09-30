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
	"net/url"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	"k8s.io/utils/ptr"
)

type sdkTransport func(*http.Request) (*http.Response, error)

func (f sdkTransport) Do(request *http.Request) (*http.Response, error) { return f(request) }

func TestAzureRead(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		change    func(sets map[string]*armcompute.VirtualMachineScaleSet, vms map[string][]*armcompute.VirtualMachineScaleSetVM)
		errorText string
	}{
		{name: "main and zero pools"},
		{name: "missing max tag", errorText: "no numeric max tag",
			change: func(sets map[string]*armcompute.VirtualMachineScaleSet, _ map[string][]*armcompute.VirtualMachineScaleSetVM) {
				delete(sets["zero"].Tags, "max")
			}},
		{name: "instance without NIC", errorText: "lacks identity or network evidence",
			change: func(_ map[string]*armcompute.VirtualMachineScaleSet, vms map[string][]*armcompute.VirtualMachineScaleSetVM) {
				vms["main"][0].Properties = nil
			}},
		{name: "missing VMSS", errorText: "read VMSS zero failed: HTTP 404",
			change: func(sets map[string]*armcompute.VirtualMachineScaleSet, _ map[string][]*armcompute.VirtualMachineScaleSetVM) {
				delete(sets, "zero")
			}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			mainVM := c.PoolID(c.MainPool) + "/virtualMachines/0"
			sets := map[string]*armcompute.VirtualMachineScaleSet{}
			for name, bounds := range map[string][2]int64{c.MainPool: {1, 2}, c.ZeroPool: {0, 1}} {
				sets[name] = &armcompute.VirtualMachineScaleSet{
					Name: ptr.To(name), SKU: &armcompute.SKU{Capacity: ptr.To(bounds[0])},
					Tags:       map[string]*string{"max": ptr.To(fmt.Sprint(bounds[1]))},
					Properties: &armcompute.VirtualMachineScaleSetProperties{ProvisioningState: ptr.To("Succeeded")},
				}
			}
			sets[c.ZeroPool].Tags[ZeroPoolTaintTag] = ptr.To(c.RunID + ":NoSchedule")
			vms := map[string][]*armcompute.VirtualMachineScaleSetVM{
				c.MainPool: {{ID: ptr.To(mainVM), Properties: &armcompute.VirtualMachineScaleSetVMProperties{
					NetworkProfile: &armcompute.NetworkProfile{NetworkInterfaces: []*armcompute.NetworkInterfaceReference{{
						ID: ptr.To(mainVM + "/networkInterfaces/nic"),
					}}},
				}}},
				c.ZeroPool: {},
			}
			if tt.change != nil {
				tt.change(sets, vms)
			}
			prefix := c.PoolID("")
			factory, err := armcompute.NewClientFactory(c.SubscriptionID, &fake.TokenCredential{}, &arm.ClientOptions{
				ClientOptions: azcore.ClientOptions{
					Retry: policy.RetryOptions{MaxRetries: -1},
					Transport: sdkTransport(func(request *http.Request) (*http.Response, error) {
						name, instances := strings.CutSuffix(strings.TrimPrefix(request.URL.Path, prefix), "/virtualMachines")
						var payload any
						status := http.StatusOK
						switch {
						case request.Method != http.MethodGet || !strings.HasPrefix(request.URL.Path, prefix):
							return nil, fmt.Errorf("unexpected SDK request %s %s", request.Method, request.URL.Path)
						case instances:
							payload = armcompute.VirtualMachineScaleSetVMListResult{Value: vms[name]}
						case sets[name] != nil:
							payload = sets[name]
						default:
							status, payload = http.StatusNotFound, map[string]any{"error": map[string]string{"code": "ResourceNotFound"}}
						}
						body, err := json.Marshal(payload)
						if err != nil {
							return nil, err
						}
						return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
							Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
					}),
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			cloud := &azureCloud{config: c, sets: factory.NewVirtualMachineScaleSetsClient(), vms: factory.NewVirtualMachineScaleSetVMsClient()}
			snapshot, err := cloud.Read(context.Background())
			if tt.errorText != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errorText) {
					t.Fatalf("Read error=%v, want %q", err, tt.errorText)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			main, zero := snapshot.Pools[c.MainPool], snapshot.Pools[c.ZeroPool]
			instance := main.Instances[normalizeID(mainVM)]
			if main.Capacity != 1 || main.Max != 2 || main.ProvisioningState != "Succeeded" || len(main.Instances) != 1 ||
				len(instance.NICs) != 1 || zero.Capacity != 0 || zero.Max != 1 || zero.TemplateTaint != c.RunID+":NoSchedule" {
				t.Fatalf("unexpected observation: %+v", snapshot.Pools)
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
	outside := "/subscriptions/00000000-0000-0000-0000-000000000002/resourceGroups/workers/providers/Microsoft.Network/networkInterfaces/nic"
	if _, err := (&azureCloud{config: testConfig()}).NICExists(context.Background(), outside); err == nil {
		t.Fatal("read a NIC outside the pools' resource group")
	}
}

func TestAzureError(t *testing.T) {
	t.Parallel()
	response := &http.Response{
		StatusCode: http.StatusConflict,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"Conflict","message":"customData bootstrap-secret"}}`)),
		Request:    &http.Request{Method: http.MethodGet, URL: &url.URL{Scheme: "https", Host: "management.azure.com"}},
	}
	err := azureError("read VMSS", runtime.NewResponseError(response))
	if got := err.Error(); got != "read VMSS failed: HTTP 409 code=Conflict" {
		t.Fatalf("Azure response error = %q", got)
	}

	err = azureError("read VMSS", fmt.Errorf("send request: %w", context.Canceled))
	if !errors.Is(err, context.Canceled) || err.Error() != "read VMSS failed: send request: context canceled" {
		t.Fatalf("other Azure error = %v, want the wrapped cause", err)
	}
}
