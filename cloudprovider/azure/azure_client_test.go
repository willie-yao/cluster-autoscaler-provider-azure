/*
Copyright 2018 The Kubernetes Authors.

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
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	armpolicy "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	azcorepolicy "github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	armcompute "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	autorestazure "github.com/Azure/go-autorest/autorest/azure"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetvmclient"
)

// Note: The previous tests for GetServicePrincipalToken were removed
// because that function no longer exists in cloud-provider-azure v1.32.0+.
// The authentication mechanism has been changed to use Azure Identity SDK
// instead of the older ADAL library.

type staticTokenCredential struct{}

func (staticTokenCredential) GetToken(context.Context, azcorepolicy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

type recordingTransport struct {
	request *http.Request
}

func (transport *recordingTransport) Do(request *http.Request) (*http.Response, error) {
	transport.request = request
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"value":[]}`)),
		Request:    request,
	}, nil
}

type powerOperationTransport struct {
	requests []*http.Request
}

func (transport *powerOperationTransport) Do(request *http.Request) (*http.Response, error) {
	transport.requests = append(transport.requests, request)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"status":"Succeeded"}`)),
		Request:    request,
	}
	if request.Method == http.MethodPost {
		response.StatusCode = http.StatusAccepted
		response.Header.Set("Location", "https://management.test/operations/1")
		response.Body = io.NopCloser(strings.NewReader(`{}`))
	}
	return response, nil
}

func TestVMSSPowerClientBodylessRequests(t *testing.T) {
	testCases := []struct {
		name   string
		action string
		begin  func(context.Context, vmssPowerClient) error
	}{
		{
			name:   "deallocate",
			action: "deallocate",
			begin: func(ctx context.Context, client vmssPowerClient) error {
				poller, err := client.BeginDeallocate(ctx, "resource-group", "scale-set", "0", nil)
				if err != nil {
					return err
				}
				_, err = poller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{Frequency: time.Millisecond})
				return err
			},
		},
		{
			name:   "start",
			action: "start",
			begin: func(ctx context.Context, client vmssPowerClient) error {
				poller, err := client.BeginStart(ctx, "resource-group", "scale-set", "0", nil)
				if err != nil {
					return err
				}
				_, err = poller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{Frequency: time.Millisecond})
				return err
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			transport := &powerOperationTransport{}
			client, err := newVMSSPowerClient(
				"subscription",
				staticTokenCredential{},
				&azclient.ARMClientConfig{Cloud: "AzurePublicCloud"},
				cloud.Configuration{
					Services: map[cloud.ServiceName]cloud.ServiceConfiguration{
						cloud.ResourceManager: {
							Endpoint: "https://management.test/",
							Audience: "https://management.test/",
						},
					},
				},
				func(options *armpolicy.ClientOptions) {
					options.Transport = transport
				},
			)
			require.NoError(t, err)

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			require.NoError(t, testCase.begin(ctx, client))

			require.Len(t, transport.requests, 2)
			actionRequest := transport.requests[0]
			assert.Equal(t, http.MethodPost, actionRequest.Method)
			assert.Equal(t, "/subscriptions/subscription/resourceGroups/resource-group/providers/Microsoft.Compute/virtualMachineScaleSets/scale-set/virtualMachines/0/"+testCase.action, actionRequest.URL.Path)
			assert.NotEmpty(t, actionRequest.URL.Query().Get("api-version"))
			assert.Nil(t, actionRequest.Body)

			pollRequest := transport.requests[1]
			assert.Equal(t, http.MethodGet, pollRequest.Method)
			assert.Equal(t, "/operations/1", pollRequest.URL.Path)
		})
	}
}

func TestNewVMSSPowerClientControlsAzureStackAPIVersion(t *testing.T) {
	testCases := []struct {
		name                   string
		disableAzureStackCloud bool
		wantAzureStackVersion  bool
	}{
		{
			name:                  "Azure Stack API version enabled",
			wantAzureStackVersion: true,
		},
		{
			name:                   "Azure Stack API version disabled",
			disableAzureStackCloud: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			transport := &recordingTransport{}
			client, err := newVMSSPowerClient(
				"subscription",
				staticTokenCredential{},
				&azclient.ARMClientConfig{
					Cloud:                  "AzureStackCloud",
					DisableAzureStackCloud: testCase.disableAzureStackCloud,
				},
				cloud.Configuration{
					ActiveDirectoryAuthorityHost: "https://login.microsoftonline.com/",
					Services: map[cloud.ServiceName]cloud.ServiceConfiguration{
						cloud.ResourceManager: {
							Endpoint: "https://management.test/",
							Audience: "https://management.test/",
						},
					},
				},
				func(options *armpolicy.ClientOptions) {
					options.Transport = transport
				},
			)
			require.NoError(t, err)

			_, err = client.BeginDeallocate(context.Background(), "resource-group", "scale-set", "0", nil)
			require.NoError(t, err)
			require.NotNil(t, transport.request)

			apiVersion := transport.request.URL.Query().Get("api-version")
			if testCase.wantAzureStackVersion {
				assert.Equal(t, virtualmachinescalesetvmclient.AzureStackCloudAPIVersion, apiVersion)
			} else {
				assert.NotEqual(t, virtualmachinescalesetvmclient.AzureStackCloudAPIVersion, apiVersion)
			}
		})
	}
}

func TestNewARMClientConfigControlsAzureStackVMSSDeleteAPIVersion(t *testing.T) {
	testCases := []struct {
		name                   string
		disableAzureStackCloud bool
		wantAzureStackVersion  bool
	}{
		{
			name:                  "Azure Stack API version enabled",
			wantAzureStackVersion: true,
		},
		{
			name:                   "Azure Stack API version disabled",
			disableAzureStackCloud: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := &Config{}
			cfg.Cloud = "AzureStackCloud"
			cfg.DisableAzureStackCloud = testCase.disableAzureStackCloud
			armConfig := newARMClientConfig(cfg, &autorestazure.Environment{})

			transport := &recordingTransport{}
			factory, err := azclient.NewClientFactory(
				&azclient.ClientFactoryConfig{SubscriptionID: "subscription"},
				armConfig,
				cloud.Configuration{
					ActiveDirectoryAuthorityHost: "https://login.microsoftonline.com/",
					Services: map[cloud.ServiceName]cloud.ServiceConfiguration{
						cloud.ResourceManager: {
							Endpoint: "https://management.test/",
							Audience: "https://management.test/",
						},
					},
				},
				staticTokenCredential{},
				func(options *armpolicy.ClientOptions) {
					options.Transport = transport
				},
			)
			require.NoError(t, err)

			deleteClient := NewVMSSDeleteClient(factory.GetVirtualMachineScaleSetClient())
			require.NotNil(t, deleteClient)
			instanceID := "0"
			forceDeletion := true
			_, err = deleteClient.BeginDeleteInstances(
				context.Background(),
				"resource-group",
				"scale-set",
				armcompute.VirtualMachineScaleSetVMInstanceRequiredIDs{InstanceIDs: []*string{&instanceID}},
				&armcompute.VirtualMachineScaleSetsClientBeginDeleteInstancesOptions{ForceDeletion: &forceDeletion},
			)
			require.NoError(t, err)
			require.NotNil(t, transport.request)

			assert.Equal(t, http.MethodPost, transport.request.Method)
			assert.Equal(t, "/subscriptions/subscription/resourceGroups/resource-group/providers/Microsoft.Compute/virtualMachineScaleSets/scale-set/delete", transport.request.URL.Path)
			assert.Equal(t, "true", transport.request.URL.Query().Get("forceDeletion"))
			apiVersion := transport.request.URL.Query().Get("api-version")
			if testCase.wantAzureStackVersion {
				assert.Equal(t, virtualmachinescalesetclient.AzureStackCloudAPIVersion, apiVersion)
			} else {
				assert.NotEqual(t, virtualmachinescalesetclient.AzureStackCloudAPIVersion, apiVersion)
			}
		})
	}
}
