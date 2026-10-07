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
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	"k8s.io/utils/ptr"
)

// nicAPIVersion is the network API version that reads VMSS NICs. The
// armnetwork VMSS NIC client uses the same version.
const nicAPIVersion = "2018-10-01"

// Cloud is the read-only Azure view that the tests use.
type Cloud interface {
	Read(context.Context) (Snapshot, error)
	NICExists(context.Context, string) (bool, error)
}

type azureCloud struct {
	config Config
	sets   *armcompute.VirtualMachineScaleSetsClient
	vms    *armcompute.VirtualMachineScaleSetVMsClient
	arm    *arm.Client
}

func newCloud(cfg Config) (Cloud, error) {
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("initialize Azure credential: %w", err)
	}
	factory, err := armcompute.NewClientFactory(cfg.SubscriptionID, credential, nil)
	if err != nil {
		return nil, err
	}
	armClient, err := arm.NewClient("autoscaler-e2e", "v0.0.0", credential, nil)
	if err != nil {
		return nil, err
	}
	return &azureCloud{config: cfg, sets: factory.NewVirtualMachineScaleSetsClient(),
		vms: factory.NewVirtualMachineScaleSetVMsClient(), arm: armClient}, nil
}

// Read returns the main and zero VMSS with their VMs and NICs.
func (a *azureCloud) Read(ctx context.Context) (Snapshot, error) {
	c := a.config
	result := Snapshot{Pools: map[string]PoolState{}}
	for _, name := range []string{c.MainPool, c.ZeroPool} {
		response, err := a.sets.Get(ctx, c.ResourceGroup, name, nil)
		if err != nil {
			return result, azureError("read VMSS "+name, err)
		}
		set := response.VirtualMachineScaleSet
		if set.SKU == nil || set.SKU.Capacity == nil || set.Properties == nil {
			return result, fmt.Errorf("incomplete VMSS %s observation", name)
		}
		maximum, err := strconv.Atoi(ptr.Deref(set.Tags["max"], ""))
		if err != nil {
			return result, fmt.Errorf("VMSS %s has no numeric max tag", name)
		}
		pool := PoolState{
			Capacity: int(*set.SKU.Capacity), Max: maximum,
			ProvisioningState: ptr.Deref(set.Properties.ProvisioningState, ""),
			Instances:         map[string]Instance{},
		}
		pager := a.vms.NewListPager(c.ResourceGroup, name, nil)
		for pager.More() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				return result, azureError("list VMSS instances", err)
			}
			for _, vm := range page.Value {
				if vm == nil || vm.ID == nil || vm.Properties == nil ||
					vm.Properties.NetworkProfile == nil || len(vm.Properties.NetworkProfile.NetworkInterfaces) == 0 {
					return result, fmt.Errorf("VMSS %s instance lacks identity or network evidence", name)
				}
				instance := Instance{ID: normalizeID(*vm.ID)}
				for _, nic := range vm.Properties.NetworkProfile.NetworkInterfaces {
					if nic == nil || nic.ID == nil {
						return result, fmt.Errorf("VMSS %s instance lacks NIC ID", name)
					}
					instance.NICs = append(instance.NICs, *nic.ID)
				}
				pool.Instances[instance.ID] = instance
			}
		}
		result.Pools[name] = pool
	}
	return result, nil
}

func (a *azureCloud) NICExists(ctx context.Context, id string) (bool, error) {
	prefix := "/subscriptions/" + a.config.SubscriptionID + "/resourcegroups/" + a.config.ResourceGroup + "/providers/"
	if !strings.HasPrefix(strings.ToLower(id), strings.ToLower(prefix)) ||
		!strings.Contains(strings.ToLower(id), "/networkinterfaces/") || strings.ContainsAny(id, "?#") {
		return false, fmt.Errorf("NIC ID is outside the pools' resource group")
	}
	request, err := runtime.NewRequest(ctx, http.MethodGet, a.arm.Endpoint()+id+"?api-version="+nicAPIVersion)
	if err != nil {
		return false, err
	}
	response, err := a.arm.Pipeline().Do(request)
	if err != nil {
		return false, azureError("read NIC", err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusNotFound:
		return false, nil
	case http.StatusOK:
		return true, nil
	default:
		return false, fmt.Errorf("read NIC: HTTP %d (not deletion evidence)", response.StatusCode)
	}
}

// azureError adds operation to err. An Azure response error keeps only its
// status and error code, because its body can contain VM bootstrap data.
func azureError(operation string, err error) error {
	var response *azcore.ResponseError
	if errors.As(err, &response) {
		return fmt.Errorf("%s failed: HTTP %d code=%s", operation, response.StatusCode, response.ErrorCode)
	}
	return fmt.Errorf("%s failed: %w", operation, err)
}
