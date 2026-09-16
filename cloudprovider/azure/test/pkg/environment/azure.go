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
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
)

// Cloud exposes only read operations to the test suite.
type Cloud interface {
	Read(context.Context) (Snapshot, error)
	NICExists(context.Context, string) (bool, error)
}

type azureCloud struct {
	config Config
	sets   *armcompute.VirtualMachineScaleSetsClient
	vms    *armcompute.VirtualMachineScaleSetVMsClient
	other  *armcompute.VirtualMachinesClient
	arm    *arm.Client
	cores  map[string]int
}

func newCloud(ctx context.Context, cfg Config) (Cloud, error) {
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("initialize Azure credential: %w", err)
	}
	return newAzureCloud(ctx, cfg, credential)
}

func newAzureCloud(ctx context.Context, cfg Config, credential azcore.TokenCredential) (Cloud, error) {
	factory, err := armcompute.NewClientFactory(cfg.SubscriptionID, credential, nil)
	if err != nil {
		return nil, err
	}
	armClient, err := arm.NewClient("autoscaler-e2e", "v0.0.0", credential, nil)
	if err != nil {
		return nil, err
	}
	cloud := &azureCloud{
		config: cfg, sets: factory.NewVirtualMachineScaleSetsClient(),
		vms: factory.NewVirtualMachineScaleSetVMsClient(), other: factory.NewVirtualMachinesClient(),
		arm: armClient, cores: map[string]int{},
	}
	filter := "location eq '" + cfg.Location + "'"
	pager := factory.NewResourceSKUsClient().NewListPager(&armcompute.ResourceSKUsClientListOptions{Filter: &filter})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, azureError("list VM SKU core counts", err)
		}
		for _, sku := range page.Value {
			if sku == nil || value(sku.ResourceType) != "virtualMachines" {
				continue
			}
			for _, capability := range sku.Capabilities {
				if capability != nil && value(capability.Name) == "vCPUs" {
					n, err := strconv.Atoi(value(capability.Value))
					if err != nil || n <= 0 {
						return nil, fmt.Errorf("invalid vCPU capability for %s", value(sku.Name))
					}
					cloud.cores[strings.ToLower(value(sku.Name))] = n
				}
			}
		}
	}
	return cloud, nil
}

func (a *azureCloud) Read(ctx context.Context) (Snapshot, error) {
	c := a.config
	result := Snapshot{Pools: map[string]PoolState{}}
	poolCores := map[string]int{}
	pager := a.sets.NewListPager(c.ResourceGroup, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return result, azureError("list scale sets", err)
		}
		// Check received capacities before any per-set convergence failure.
		for _, set := range page.Value {
			if set == nil || set.Name == nil || set.SKU == nil || set.SKU.Capacity == nil ||
				value(set.Tags[RunLabel]) != c.RunID || value(set.Tags["cluster-autoscaler-name"]) != c.DiscoveryValue {
				continue
			}
			minimum, maximum := 1, 2
			if *set.Name == c.ZeroPool {
				minimum, maximum = 0, 1
			} else if *set.Name != c.MainPool {
				continue
			}
			if err := checkCapacity(*set.Name, int(*set.SKU.Capacity), minimum, maximum); err != nil {
				return result, err
			}
		}
		for _, set := range page.Value {
			if set == nil || set.Name == nil || set.SKU == nil || set.SKU.Capacity == nil || set.Properties == nil {
				return result, fmt.Errorf("incomplete VMSS observation")
			}
			name := *set.Name
			minimum, maximum := 1, 2
			if name == c.ZeroPool {
				minimum, maximum = 0, 1
			} else if name != c.MainPool {
				return result, fmt.Errorf("unexpected VMSS %s in dedicated worker resource group", name)
			}
			if value(set.Tags[RunLabel]) != c.RunID ||
				value(set.Tags["cluster-autoscaler-name"]) != c.DiscoveryValue ||
				value(set.Tags["min"]) != strconv.Itoa(minimum) || value(set.Tags["max"]) != strconv.Itoa(maximum) {
				return result, fmt.Errorf("VMSS %s ownership/discovery/bounds do not match authorization", name)
			}
			if err := checkScaleDownTags(set.Tags); err != nil {
				return result, fmt.Errorf("VMSS %s: %w", name, err)
			}
			// Flex uses standalone VM resource IDs and needs a separate observation adapter.
			if set.Properties.OrchestrationMode != nil && *set.Properties.OrchestrationMode != armcompute.OrchestrationModeUniform {
				return result, fmt.Errorf("VMSS %s is not Uniform; this suite cannot attest that profile", name)
			}
			if value(set.Properties.ProvisioningState) != "Succeeded" || set.Properties.Overprovision == nil || *set.Properties.Overprovision {
				return result, fmt.Errorf("VMSS %s must be Succeeded with overprovision disabled", name)
			}
			pool := PoolState{Capacity: int(*set.SKU.Capacity), Instances: map[string]Instance{}}
			instances := a.vms.NewListPager(c.ResourceGroup, name, nil)
			for instances.More() {
				page, err := instances.NextPage(ctx)
				if err != nil {
					return result, azureError("list VMSS instances", err)
				}
				for _, vm := range page.Value {
					if vm == nil || vm.ID == nil || vm.Properties == nil ||
						vm.Properties.NetworkProfile == nil || len(vm.Properties.NetworkProfile.NetworkInterfaces) == 0 {
						return result, fmt.Errorf("VMSS %s instance lacks identity/network evidence", name)
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
			if err := result.CheckBounds(c); err != nil {
				return result, err
			}
			n := maxInt(pool.Capacity, len(pool.Instances))
			cores := a.cores[strings.ToLower(value(set.SKU.Name))]
			if cores == 0 {
				return result, fmt.Errorf("no core count for VMSS %s SKU", name)
			}
			result.VMs += n
			result.VCPUs += n * cores
			poolCores[name] = cores
			if err := result.CheckBounds(c); err != nil {
				return result, err
			}
		}
	}
	if len(result.Pools) != 2 {
		return result, fmt.Errorf("expected exactly the two authorized scale sets")
	}
	standalone := a.other.NewListPager(c.ResourceGroup, nil)
	for standalone.More() {
		page, err := standalone.NextPage(ctx)
		if err != nil {
			return result, azureError("list worker resource group VMs", err)
		}
		for _, vm := range page.Value {
			if vm == nil || !strings.EqualFold(value(vm.ID), c.ControlPlaneID) {
				return result, fmt.Errorf("unexpected standalone VM in worker resource group")
			}
		}
	}
	cpParts := strings.Split(c.ControlPlaneID, "/")
	cp, err := a.other.Get(ctx, cpParts[4], cpParts[8], nil)
	if err != nil {
		return result, azureError("read authorized control plane", err)
	}
	if value(cp.Tags[RunLabel]) != c.RunID || cp.Properties == nil || cp.Properties.HardwareProfile == nil {
		return result, fmt.Errorf("control plane ownership or hardware evidence missing")
	}
	cores := a.cores[strings.ToLower(stringValue(cp.Properties.HardwareProfile.VMSize))]
	if cores == 0 {
		return result, fmt.Errorf("no core count for control plane SKU")
	}
	result.VMs++
	result.VCPUs += cores
	if err := checkPeakEnvelope(poolCores[c.MainPool], poolCores[c.ZeroPool], cores); err != nil {
		return result, err
	}
	return result, result.CheckBounds(c)
}

func checkScaleDownTags(tags map[string]*string) error {
	const prefix = "k8s.io_cluster-autoscaler_node-template_autoscaling-options_"
	if raw := tags[prefix+"scaledownunneededtime"]; raw != nil {
		duration, err := time.ParseDuration(strings.ToLower(*raw))
		if err != nil || duration < 0 || duration > time.Minute {
			return fmt.Errorf("per-pool scale-down unneeded time must be between zero and one minute")
		}
	}
	if raw := tags[prefix+"scaledownutilizationthreshold"]; raw != nil {
		threshold, err := strconv.ParseFloat(strings.ToLower(*raw), 64)
		if err != nil || threshold != 0.5 {
			return fmt.Errorf("per-pool scale-down utilization threshold must be 0.5")
		}
	}
	return nil
}

func checkPeakEnvelope(mainCores, zeroCores, controlPlaneCores int) error {
	if mainCores <= 0 || zeroCores <= 0 || controlPlaneCores <= 0 {
		return fmt.Errorf("peak resource envelope requires each VM SKU core count")
	}
	// The authorized pool maxima are two main workers and one zero-pool worker.
	peak := 2*mainCores + zeroCores + controlPlaneCores
	if peak > MaxVCPUs {
		return fmt.Errorf("%w: configured pool maxima and control plane require %d vCPUs, exceeding %d", ErrBounds, peak, MaxVCPUs)
	}
	return nil
}

func (a *azureCloud) NICExists(ctx context.Context, id string) (bool, error) {
	if !strings.HasPrefix(strings.ToLower(id), strings.ToLower(a.config.resourcePrefix())+"/providers/") ||
		!strings.Contains(strings.ToLower(id), "/networkinterfaces/") || strings.ContainsAny(id, "?#") {
		return false, fmt.Errorf("NIC ID is outside the authorized worker resource group")
	}
	request, err := runtime.NewRequest(ctx, http.MethodGet, a.arm.Endpoint()+id+"?api-version=2023-09-01")
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

// SDK response bodies can contain VM bootstrap data. Do not include them in reports.
func azureError(operation string, err error) error {
	var response *azcore.ResponseError
	if errors.As(err, &response) {
		return fmt.Errorf("%s failed: HTTP %d code=%s", operation, response.StatusCode, response.ErrorCode)
	}
	return fmt.Errorf("%s failed (%T); inspect Azure diagnostics privately", operation, err)
}

func value(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func stringValue[T ~string](p *T) string {
	if p == nil {
		return ""
	}
	return string(*p)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
