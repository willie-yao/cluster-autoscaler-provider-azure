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
	"k8s.io/apimachinery/pkg/util/wait"
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
	config        Config
	sets          *armcompute.VirtualMachineScaleSetsClient
	vms           *armcompute.VirtualMachineScaleSetVMsClient
	standaloneVMs *armcompute.VirtualMachinesClient
	arm           *arm.Client
	cores         map[string]int
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
		vms: factory.NewVirtualMachineScaleSetVMsClient(), standaloneVMs: factory.NewVirtualMachinesClient(),
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
			if sku == nil || ptr.Deref(sku.ResourceType, "") != "virtualMachines" {
				continue
			}
			for _, capability := range sku.Capabilities {
				if capability != nil && ptr.Deref(capability.Name, "") == "vCPUs" {
					n, err := strconv.Atoi(ptr.Deref(capability.Value, ""))
					if err != nil || n <= 0 {
						return nil, fmt.Errorf("invalid vCPU capability for %s", ptr.Deref(sku.Name, ""))
					}
					cloud.cores[strings.ToLower(ptr.Deref(sku.Name, ""))] = n
				}
			}
		}
	}
	return cloud, nil
}

func (a *azureCloud) Read(ctx context.Context) (Snapshot, error) {
	return a.read(ctx, false)
}

func (a *azureCloud) readAfterMissing(ctx context.Context) (Snapshot, error) {
	if a.config.Phase != "missing-vmss" {
		return Snapshot{}, fmt.Errorf("surviving-pool read requires the missing-vmss phase")
	}
	return a.read(ctx, true)
}

func (a *azureCloud) read(ctx context.Context, missing bool) (Snapshot, error) {
	c := a.config
	result := Snapshot{Pools: map[string]PoolState{}}
	poolCores := map[string]int{}
	bounds := c.Pools()
	balanceTags := map[string]map[string]*string{}
	systemVMs, systemCores := 0, 0
	pager := a.sets.NewListPager(c.ResourceGroup, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return result, azureError("list scale sets", err)
		}
		// Check received capacities before any per-set convergence failure.
		for _, set := range page.Value {
			if set == nil || set.Name == nil || set.SKU == nil || set.SKU.Capacity == nil ||
				ptr.Deref(set.Tags[RunLabel], "") != c.RunID || ptr.Deref(set.Tags["cluster-autoscaler-name"], "") != c.DiscoveryValue {
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
			if c.AKS() && strings.EqualFold(name, c.SystemPool) {
				// AKS system add-ons run here, so the System pool takes the control plane's place in the budget.
				if _, tagged := set.Tags["cluster-autoscaler-name"]; tagged || *set.SKU.Capacity < 1 {
					return result, fmt.Errorf("AKS System pool %s must be untagged for discovery and keep at least one VM", name)
				}
				cores := a.cores[strings.ToLower(ptr.Deref(set.SKU.Name, ""))]
				if cores == 0 {
					return result, fmt.Errorf("no core count for AKS System pool SKU")
				}
				instances := map[string]struct{}{}
				systemPager := a.vms.NewListPager(c.ResourceGroup, name, nil)
				for systemPager.More() {
					page, err := systemPager.NextPage(ctx)
					if err != nil {
						return result, azureError("list AKS System pool instances", err)
					}
					for _, vm := range page.Value {
						if vm != nil && vm.ID != nil {
							instances[normalizeID(*vm.ID)] = struct{}{}
						}
					}
				}
				n := max(int(*set.SKU.Capacity), len(instances))
				systemVMs += n
				systemCores += n * cores
				result.VMs += n
				result.VCPUs += n * cores
				if err := result.CheckBounds(c); err != nil {
					return result, err
				}
				continue
			}
			limit, ok := bounds[name]
			if !ok {
				return result, fmt.Errorf("unexpected VMSS %s in dedicated worker resource group", name)
			}
			set, err = a.settledFailedPool(ctx, set, 5*time.Second, time.Minute)
			if err != nil {
				return result, err
			}
			if set == nil || ptr.Deref(set.Name, "") != name || set.SKU == nil || set.SKU.Capacity == nil || set.Properties == nil {
				return result, fmt.Errorf("incomplete VMSS %s after provisioning retry", name)
			}
			if err := checkCapacity(name, int(*set.SKU.Capacity), limit.ObservedMin, limit.Max); err != nil {
				return result, err
			}
			if ptr.Deref(set.Tags[RunLabel], "") != c.RunID ||
				ptr.Deref(set.Tags["cluster-autoscaler-name"], "") != c.DiscoveryValue ||
				ptr.Deref(set.Tags["min"], "") != strconv.Itoa(limit.TagMin) || ptr.Deref(set.Tags["max"], "") != strconv.Itoa(limit.Max) {
				return result, fmt.Errorf("VMSS %s ownership/discovery/bounds do not match authorization, tags min=%q max=%q, want %d and %d",
					name, ptr.Deref(set.Tags["min"], ""), ptr.Deref(set.Tags["max"], ""), limit.TagMin, limit.Max)
			}
			if err := checkScaleDownTags(set.Tags); err != nil {
				return result, fmt.Errorf("VMSS %s: %w", name, err)
			}
			// Flex uses standalone VM resource IDs and needs a separate observation adapter.
			if set.Properties.OrchestrationMode != nil && *set.Properties.OrchestrationMode != armcompute.OrchestrationModeUniform {
				return result, fmt.Errorf("VMSS %s is not Uniform; this suite cannot attest that profile", name)
			}
			if (ptr.Deref(set.Properties.ProvisioningState, "") != "Succeeded" &&
				!(c.Phase == "cse" && name == c.FailurePool && ptr.Deref(set.Properties.ProvisioningState, "") == "Failed")) ||
				set.Properties.Overprovision == nil || *set.Properties.Overprovision {
				return result, fmt.Errorf("VMSS %s must be Succeeded with overprovision disabled", name)
			}
			pool := PoolState{
				Capacity: int(*set.SKU.Capacity), Instances: map[string]Instance{},
				TemplateTaint: ptr.Deref(set.Tags[ZeroPoolTaintTag], ""),
				SKU:           ptr.Deref(set.SKU.Name, ""),
			}
			for _, zone := range set.Zones {
				pool.Zone += ptr.Deref(zone, "") + ","
			}
			if set.Properties.VirtualMachineProfile != nil && set.Properties.VirtualMachineProfile.StorageProfile != nil {
				pool.Image = imageReference(set.Properties.VirtualMachineProfile.StorageProfile.ImageReference)
			}
			if c.Phase == "no-join" && name == c.ZeroPool &&
				ptr.Deref(set.Tags["autoscaler-e2e-no-join"], "") != c.RunID {
				return result, fmt.Errorf("no-join pool needs the operator's run-owned no-join tag")
			}
			if c.Phase == "balance" && (name == c.BalancePoolA || name == c.BalancePoolB) &&
				ptr.Deref(set.Tags["k8s.io_cluster-autoscaler_node-template_label_"+c.PoolLabel], "") != c.BalanceLabel {
				return result, fmt.Errorf("balance pool %s lacks its shared node-template label", name)
			}
			if c.Phase == "balance" && (name == c.BalancePoolA || name == c.BalancePoolB) {
				balanceTags[name] = set.Tags
			}
			if (c.Phase == "spot" || c.Phase == "spot-eviction") && name == c.SpotPool {
				if err := checkSpotPool(set, c); err != nil {
					return result, err
				}
			}
			if c.Phase == "cse" && name == c.FailurePool {
				extensionName, err := checkFailedExtensionPool(set, c)
				if err != nil {
					return result, err
				}
				pool.FailedExtensionName = extensionName
			}
			if c.Phase == "large" && name == c.ScalePool {
				if ptr.Deref(set.SKU.Name, "") != "Standard_B1ms" || len(set.Zones) != 1 || ptr.Deref(set.Zones[0], "") != "1" ||
					ptr.Deref(set.Tags["k8s.io_cluster-autoscaler_node-template_label_"+c.PoolLabel], "") != c.ScaleLabel {
					return result, fmt.Errorf("large pool must use zonal B1ms and its run-owned node-template label")
				}
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
						if id := normalizeID(ptr.Deref(vm.ID, "")); id != "" {
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
					instance := Instance{
						ID: normalizeID(*vm.ID), VMID: ptr.Deref(vm.Properties.VMID, ""),
						ProvisioningState: ptr.Deref(vm.Properties.ProvisioningState, ""),
					}
					if (c.Phase == "no-join" || c.Phase == "spot-eviction" && name == c.SpotPool ||
						c.Phase == "spot" && name == c.SpotPool || c.Phase == "cse" && name == c.FailurePool) && instance.VMID == "" {
						return result, fmt.Errorf("VMSS %s instance lacks its unique VM ID", name)
					}
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
			n := max(pool.Capacity, len(pool.Instances))
			cores := a.cores[strings.ToLower(ptr.Deref(set.SKU.Name, ""))]
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
	if missing {
		if _, exists := result.Pools[c.MissingPool]; exists || len(result.Pools) != len(bounds)-1 {
			return result, fmt.Errorf("expected the authorized missing VMSS to be absent, with two surviving scale sets")
		}
	} else if len(result.Pools) != len(bounds) {
		return result, fmt.Errorf("expected exactly %d authorized scale sets", len(bounds))
	}
	if c.Phase == "balance" {
		if err := checkBalanceTemplateTags(balanceTags[c.BalancePoolA], balanceTags[c.BalancePoolB]); err != nil {
			return result, err
		}
	}
	standalone := a.standaloneVMs.NewListPager(c.ResourceGroup, nil)
	for standalone.More() {
		page, err := standalone.NextPage(ctx)
		if err != nil {
			return result, azureError("list worker resource group VMs", err)
		}
		for _, vm := range page.Value {
			if vm == nil || c.AKS() || !strings.EqualFold(ptr.Deref(vm.ID, ""), c.ControlPlaneID) {
				return result, fmt.Errorf("unexpected standalone VM in worker resource group")
			}
		}
	}
	cores := systemCores
	if c.AKS() {
		if systemVMs == 0 {
			return result, fmt.Errorf("AKS System pool %s is missing", c.SystemPool)
		}
		maxVMs, maxVCPUs := c.Limits()
		peakVMs, peakVCPUs := result.VMs, result.VCPUs
		for name, pool := range result.Pools {
			headroom := bounds[name].Max - max(pool.Capacity, len(pool.Instances))
			peakVMs += headroom
			peakVCPUs += headroom * poolCores[name]
		}
		if peakVMs > maxVMs || peakVCPUs > maxVCPUs {
			return result, fmt.Errorf("%w: AKS System pool and authorized pool peaks exceed the fixture budget", ErrBounds)
		}
	} else {
		cpParts := strings.Split(c.ControlPlaneID, "/")
		cp, err := a.standaloneVMs.Get(ctx, cpParts[4], cpParts[8], nil)
		if err != nil {
			return result, azureError("read authorized control plane", err)
		}
		if ptr.Deref(cp.Tags[RunLabel], "") != c.RunID || cp.Properties == nil || cp.Properties.HardwareProfile == nil {
			return result, fmt.Errorf("control plane ownership or hardware evidence missing")
		}
		cores = a.cores[strings.ToLower(string(ptr.Deref(cp.Properties.HardwareProfile.VMSize, "")))]
		if cores == 0 {
			return result, fmt.Errorf("no core count for control plane SKU")
		}
		result.VMs++
		result.VCPUs += cores
	}
	if c.Phase == "balance" {
		for _, size := range poolCores {
			if MaxVMs*size > MaxVCPUs {
				return result, fmt.Errorf("%w: four workers of this SKU exceed the vCPU limit", ErrBounds)
			}
		}
		if MaxVMs*cores > MaxVCPUs {
			return result, fmt.Errorf("%w: four control-plane VMs of this SKU exceed the vCPU limit", ErrBounds)
		}
	} else if missing {
		// The local-storage phase has the bounds of the main and zero pools
		// that remain, with no zero-pool growth.
		surviving := c
		surviving.Phase = "local-storage"
		if err := checkPhasePeakEnvelope(surviving, poolCores, cores); err != nil {
			return result, err
		}
	} else if c.Phase == "spot" || c.Phase == "spot-eviction" || c.Phase == "large" ||
		c.Phase == "cse" || c.Phase == "missing-vmss" || c.Phase == "local-storage" {
		if err := checkPhasePeakEnvelope(c, poolCores, cores); err != nil {
			return result, err
		}
	} else if err := checkPeakEnvelope(poolCores[c.MainPool], poolCores[c.ZeroPool], cores); err != nil {
		return result, err
	}
	return result, result.CheckBounds(c)
}

func (a *azureCloud) settledFailedPool(ctx context.Context, set *armcompute.VirtualMachineScaleSet, interval, timeout time.Duration) (*armcompute.VirtualMachineScaleSet, error) {
	name := ptr.Deref(set.Name, "")
	if a.config.Phase != "cse" || name != a.config.FailurePool || ptr.Deref(set.Properties.ProvisioningState, "") != "Updating" {
		return set, nil
	}
	var settled *armcompute.VirtualMachineScaleSet
	var readErr error
	err := wait.PollUntilContextTimeout(ctx, interval, timeout, true, func(ctx context.Context) (bool, error) {
		response, err := a.sets.Get(ctx, a.config.ResourceGroup, name, nil)
		if err != nil {
			readErr = azureError("read updating failed VM pool", err)
			return false, readErr
		}
		settled = &response.VirtualMachineScaleSet
		return settled.Properties != nil && ptr.Deref(settled.Properties.ProvisioningState, "") != "Updating", nil
	})
	if readErr != nil {
		return nil, readErr
	}
	if err != nil {
		return nil, fmt.Errorf("VMSS %s remained Updating after %s: %w", name, timeout, err)
	}
	return settled, nil
}

func checkSpotPool(set *armcompute.VirtualMachineScaleSet, c Config) error {
	profile := set.Properties.VirtualMachineProfile
	if ptr.Deref(set.SKU.Name, "") != "Standard_D2s_v5" || len(set.Zones) != 1 || ptr.Deref(set.Zones[0], "") != "1" ||
		ptr.Deref(set.Tags["k8s.io_cluster-autoscaler_node-template_label_"+c.PoolLabel], "") != c.SpotLabel ||
		profile == nil || profile.Priority == nil || *profile.Priority != armcompute.VirtualMachinePriorityTypesSpot ||
		profile.EvictionPolicy == nil || *profile.EvictionPolicy != armcompute.VirtualMachineEvictionPolicyTypesDelete ||
		profile.BillingProfile == nil || profile.BillingProfile.MaxPrice == nil || *profile.BillingProfile.MaxPrice != -1 ||
		set.Properties.SpotRestorePolicy != nil && set.Properties.SpotRestorePolicy.Enabled != nil &&
			*set.Properties.SpotRestorePolicy.Enabled {
		return fmt.Errorf("Spot pool must be zonal D2s_v5 with Delete eviction, on-demand price cap, no automatic restore and its template label")
	}
	return nil
}

func checkFailedExtensionPool(set *armcompute.VirtualMachineScaleSet, c Config) (string, error) {
	if ptr.Deref(set.SKU.Name, "") != "Standard_D2s_v5" || len(set.Zones) != 1 || ptr.Deref(set.Zones[0], "") != "1" ||
		ptr.Deref(set.Tags["autoscaler-e2e-failing-extension"], "") != c.RunID ||
		ptr.Deref(set.Tags["k8s.io_cluster-autoscaler_node-template_label_"+c.PoolLabel], "") != c.FailureLabel ||
		set.Properties.VirtualMachineProfile == nil ||
		set.Properties.VirtualMachineProfile.ExtensionProfile == nil {
		return "", fmt.Errorf("failed VM pool needs a run-owned failing CustomScript extension and node-template label")
	}
	found := 0
	name := ""
	for _, extension := range set.Properties.VirtualMachineProfile.ExtensionProfile.Extensions {
		if extension == nil || extension.Properties == nil ||
			ptr.Deref(extension.Properties.Publisher, "") != "Microsoft.Azure.Extensions" ||
			ptr.Deref(extension.Properties.Type, "") != "CustomScript" {
			continue
		}
		if extension.Properties.SuppressFailures != nil && *extension.Properties.SuppressFailures {
			return "", fmt.Errorf("failed VM pool must not suppress CustomScript extension failures")
		}
		name = ptr.Deref(extension.Name, "")
		found++
	}
	if found != 1 || name == "" {
		return "", fmt.Errorf("failed VM pool needs exactly one named CustomScript extension")
	}
	return name, nil
}

func checkPhasePeakEnvelope(c Config, poolCores map[string]int, controlPlaneCores int) error {
	maxVMs, maxVCPUs := c.Limits()
	vms, cpus := 1, controlPlaneCores
	for name, bounds := range c.Pools() {
		cores := poolCores[name]
		if cores <= 0 {
			return fmt.Errorf("no SKU core count for pool %s", name)
		}
		vms += bounds.Max
		cpus += bounds.Max * cores
	}
	if vms > maxVMs || cpus > maxVCPUs {
		return fmt.Errorf("%w: configured pool maxima need %d VMs and %d vCPUs, exceeding %d/%d", ErrBounds,
			vms, cpus, maxVMs, maxVCPUs)
	}
	return nil
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
		ptr.Deref(image.CommunityGalleryImageID, ""), ptr.Deref(image.ID, ""), ptr.Deref(image.SharedGalleryImageID, ""),
		ptr.Deref(image.Publisher, ""), ptr.Deref(image.Offer, ""), ptr.Deref(image.SKU, ""), ptr.Deref(image.Version, ""),
		ptr.Deref(image.ExactVersion, ""),
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

// InstanceRunning reports whether the VMSS VM id in pool is running. It reads
// only the instance view, not guest settings.
func (e *Environment) InstanceRunning(ctx context.Context, pool, id string) (bool, error) {
	cloud, ok := e.Cloud.(*azureCloud)
	if !ok {
		return false, fmt.Errorf("Azure instance-view reader is unavailable")
	}
	return cloud.instanceRunning(ctx, pool, id)
}

// FailedCustomScriptRunning reports whether the VM id in the failed-extension
// pool is running and its extensionName extension has failed.
func (e *Environment) FailedCustomScriptRunning(ctx context.Context, pool, id, extensionName string) (bool, error) {
	cloud, ok := e.Cloud.(*azureCloud)
	if !ok {
		return false, fmt.Errorf("Azure instance-view reader is unavailable")
	}
	return cloud.failedCustomScriptRunning(ctx, pool, id, extensionName)
}

func (a *azureCloud) instanceView(ctx context.Context, pool, id string) (armcompute.VirtualMachineScaleSetVMInstanceView, error) {
	if _, ok := a.config.Pools()[pool]; !ok {
		return armcompute.VirtualMachineScaleSetVMInstanceView{}, fmt.Errorf("VMSS instance belongs to an unauthorized pool")
	}
	prefix := normalizeID(a.config.PoolID(pool)) + "/virtualmachines/"
	normalized := normalizeID(id)
	if !strings.HasPrefix(normalized, prefix) || strings.ContainsAny(strings.TrimPrefix(normalized, prefix), "/?#") ||
		len(normalized) == len(prefix) {
		return armcompute.VirtualMachineScaleSetVMInstanceView{}, fmt.Errorf("VMSS instance ID is outside the authorized pool")
	}
	instanceID := strings.TrimPrefix(normalized, prefix)
	view, err := a.vms.GetInstanceView(ctx, a.config.ResourceGroup, pool, instanceID, nil)
	if err != nil {
		return armcompute.VirtualMachineScaleSetVMInstanceView{}, azureError("read VMSS instance power state", err)
	}
	return view.VirtualMachineScaleSetVMInstanceView, nil
}

func (a *azureCloud) instanceRunning(ctx context.Context, pool, id string) (bool, error) {
	view, err := a.instanceView(ctx, pool, id)
	if err != nil {
		return false, err
	}
	for _, status := range view.Statuses {
		if status != nil && strings.EqualFold(ptr.Deref(status.Code, ""), "PowerState/running") {
			return true, nil
		}
	}
	return false, nil
}

func (a *azureCloud) failedCustomScriptRunning(ctx context.Context, pool, id, extensionName string) (bool, error) {
	if a.config.Phase != "cse" || pool != a.config.FailurePool || extensionName == "" {
		return false, fmt.Errorf("failed CustomScript check requires the authorized failed VM pool")
	}
	view, err := a.instanceView(ctx, pool, id)
	if err != nil {
		return false, err
	}
	running := false
	for _, status := range view.Statuses {
		if status != nil && strings.EqualFold(ptr.Deref(status.Code, ""), "PowerState/running") {
			running = true
		}
	}
	if !running {
		return false, nil
	}
	for _, extension := range view.Extensions {
		if extension == nil || !strings.EqualFold(ptr.Deref(extension.Name, ""), extensionName) {
			continue
		}
		for _, status := range extension.Statuses {
			if status == nil {
				continue
			}
			code := strings.ToLower(ptr.Deref(status.Code, ""))
			if code == "provisioningstate/failed" || strings.HasPrefix(code, "provisioningstate/failed/") {
				return true, nil
			}
		}
	}
	return false, nil
}

func (a *azureCloud) NICExists(ctx context.Context, id string) (bool, error) {
	if !strings.HasPrefix(strings.ToLower(id), strings.ToLower(a.config.resourcePrefix())+"/providers/") ||
		!strings.Contains(strings.ToLower(id), "/networkinterfaces/") || strings.ContainsAny(id, "?#") {
		return false, fmt.Errorf("NIC ID is outside the authorized worker resource group")
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
