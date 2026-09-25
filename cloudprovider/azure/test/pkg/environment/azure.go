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
	bounds := c.Pools()
	balanceTags := map[string]map[string]*string{}
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
			limit, ok := bounds[*set.Name]
			if !ok {
				continue
			}
			if err := checkCapacity(*set.Name, int(*set.SKU.Capacity), limit.ObservedMin, limit.Max); err != nil {
				return result, err
			}
		}
		for _, set := range page.Value {
			if set == nil || set.Name == nil || set.SKU == nil || set.SKU.Capacity == nil || set.Properties == nil {
				return result, fmt.Errorf("incomplete VMSS observation")
			}
			name := *set.Name
			limit, ok := bounds[name]
			if !ok {
				return result, fmt.Errorf("unexpected VMSS %s in dedicated worker resource group", name)
			}
			if value(set.Tags[RunLabel]) != c.RunID ||
				value(set.Tags["cluster-autoscaler-name"]) != c.DiscoveryValue ||
				value(set.Tags["min"]) != strconv.Itoa(limit.TagMin) || value(set.Tags["max"]) != strconv.Itoa(limit.Max) {
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
			pool := PoolState{
				Capacity: int(*set.SKU.Capacity), Instances: map[string]Instance{},
				TemplateTaint: value(set.Tags[ZeroPoolTaintTag]),
				SKU:           value(set.SKU.Name),
			}
			for _, zone := range set.Zones {
				pool.Zone += value(zone) + ","
			}
			if set.Properties.VirtualMachineProfile != nil && set.Properties.VirtualMachineProfile.StorageProfile != nil {
				pool.Image = imageReference(set.Properties.VirtualMachineProfile.StorageProfile.ImageReference)
			}
			if c.Phase == "no-join" && name == c.ZeroPool &&
				value(set.Tags["autoscaler-e2e-no-join"]) != c.RunID {
				return result, fmt.Errorf("no-join pool needs the operator's run-owned no-join tag")
			}
			if c.Phase == "balance" && (name == c.BalancePoolA || name == c.BalancePoolB) &&
				value(set.Tags["k8s.io_cluster-autoscaler_node-template_label_"+c.PoolLabel]) != c.BalanceLabel {
				return result, fmt.Errorf("balance pool %s lacks its shared node-template label", name)
			}
			if c.Phase == "balance" && (name == c.BalancePoolA || name == c.BalancePoolB) {
				balanceTags[name] = set.Tags
			}
			instanceIDs := map[string]struct{}{}
			instances := a.vms.NewListPager(c.ResourceGroup, name, nil)
			for instances.More() {
				page, err := instances.NextPage(ctx)
				if err != nil {
					return result, azureError("list VMSS instances", err)
				}
				// A received page can prove excess instances without complete NIC evidence.
				for _, vm := range page.Value {
					if vm != nil {
						if id := normalizeID(value(vm.ID)); id != "" {
							instanceIDs[id] = struct{}{}
						}
					}
				}
				if len(instanceIDs) > limit.Max {
					return result, fmt.Errorf("%w: pool %s actual=%d exceeds maximum %d",
						ErrBounds, name, len(instanceIDs), limit.Max)
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
	if len(result.Pools) != len(bounds) {
		return result, fmt.Errorf("expected exactly %d authorized scale sets", len(bounds))
	}
	if c.Phase == "balance" {
		if err := checkBalanceTemplateTags(balanceTags[c.BalancePoolA], balanceTags[c.BalancePoolB]); err != nil {
			return result, err
		}
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
	if c.Phase == "balance" {
		for _, size := range poolCores {
			if MaxVMs*size > MaxVCPUs {
				return result, fmt.Errorf("%w: four workers of this SKU exceed the vCPU limit", ErrBounds)
			}
		}
		if MaxVMs*cores > MaxVCPUs {
			return result, fmt.Errorf("%w: four control-plane VMs of this SKU exceed the vCPU limit", ErrBounds)
		}
	} else if err := checkPeakEnvelope(poolCores[c.MainPool], poolCores[c.ZeroPool], cores); err != nil {
		return result, err
	}
	return result, result.CheckBounds(c)
}

func checkBalanceTemplateTags(a, b map[string]*string) error {
	const prefix = "k8s.io_cluster-autoscaler_node-template_"
	for _, pair := range [][2]map[string]*string{{a, b}, {b, a}} {
		for key, tag := range pair[0] {
			if strings.HasPrefix(key, prefix) {
				other, found := pair[1][key]
				if !found || tag == nil || other == nil || *other != *tag {
					return fmt.Errorf("balance pools need matching node-template scheduling tags")
				}
			}
		}
	}
	return nil
}

func imageReference(image *armcompute.ImageReference) string {
	if image == nil {
		return ""
	}
	parts := []string{
		value(image.CommunityGalleryImageID), value(image.ID), value(image.SharedGalleryImageID),
		value(image.Publisher), value(image.Offer), value(image.SKU), value(image.Version),
		value(image.ExactVersion),
	}
	for _, part := range parts {
		if part != "" {
			return strings.Join(parts, "|")
		}
	}
	return ""
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

// InstanceRunning checks one captured VMSS instance without reading guest settings.
func (e *Environment) InstanceRunning(ctx context.Context, pool, id string) (bool, error) {
	cloud, ok := e.Cloud.(*azureCloud)
	if !ok {
		return false, fmt.Errorf("Azure instance-view reader is unavailable")
	}
	return cloud.instanceRunning(ctx, pool, id)
}

func (a *azureCloud) instanceRunning(ctx context.Context, pool, id string) (bool, error) {
	if _, ok := a.config.Pools()[pool]; !ok {
		return false, fmt.Errorf("VMSS instance belongs to an unauthorized pool")
	}
	prefix := normalizeID(a.config.PoolID(pool)) + "/virtualmachines/"
	normalized := normalizeID(id)
	if !strings.HasPrefix(normalized, prefix) || strings.ContainsAny(strings.TrimPrefix(normalized, prefix), "/?#") ||
		len(normalized) == len(prefix) {
		return false, fmt.Errorf("VMSS instance ID is outside the authorized pool")
	}
	instanceID := strings.TrimPrefix(normalized, prefix)
	view, err := a.vms.GetInstanceView(ctx, a.config.ResourceGroup, pool, instanceID, nil)
	if err != nil {
		return false, azureError("read VMSS instance power state", err)
	}
	for _, status := range view.Statuses {
		if status != nil && strings.EqualFold(value(status.Code), "PowerState/running") {
			return true, nil
		}
	}
	return false, nil
}

func (a *azureCloud) NICExists(ctx context.Context, id string) (bool, error) {
	if !strings.HasPrefix(strings.ToLower(id), strings.ToLower(a.config.resourcePrefix())+"/providers/") ||
		!strings.Contains(strings.ToLower(id), "/networkinterfaces/") || strings.ContainsAny(id, "?#") {
		return false, fmt.Errorf("NIC ID is outside the authorized worker resource group")
	}
	request, err := runtime.NewRequest(ctx, http.MethodGet, a.arm.Endpoint()+id+"?api-version=2018-10-01")
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
