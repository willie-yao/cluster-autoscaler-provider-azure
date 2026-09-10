/*
Copyright 2017 The Kubernetes Authors.

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

//go:generate go run azure_instance_types/gen.go

package azure

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/go-autorest/autorest/azure"
	"k8s.io/apimachinery/pkg/util/wait"
	kretry "k8s.io/client-go/util/retry"
	klog "k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	providerazureconsts "sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cluster-autoscaler/pkg/cloudprovider"
	"sigs.k8s.io/cluster-autoscaler/pkg/config"
	"sigs.k8s.io/cluster-autoscaler/pkg/config/dynamic"
	coreoptions "sigs.k8s.io/cluster-autoscaler/pkg/core/options"
)

const (
	azurePrefix = "azure://"

	scaleToZeroSupportedStandard = false
	scaleToZeroSupportedVMSS     = true
	refreshInterval              = 1 * time.Minute
)

// isErrorRetriable checks if an error is retriable.
func isErrorRetriable(err error) bool {
	if err == nil {
		return false
	}

	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		// 429 (Too Many Requests) and 5xx errors are retriable
		if respErr.StatusCode == http.StatusTooManyRequests ||
			(respErr.StatusCode >= 500 && respErr.StatusCode < 600) {
			return true
		}
	}

	return false
}

// AzureManager handles Azure communication and data caching.
type AzureManager struct {
	config   *Config
	azClient *azClient
	env      azure.Environment

	// azureCache is used for caching Azure resources.
	// It keeps track of nodegroups and instances
	// (and of which nodegroup instances belong to)
	azureCache *azureCache
	// lastRefresh is the time azureCache was last refreshed.
	// Together with azureCache.refreshInterval is it used to decide whether
	// it is time to refresh the cache from Azure resources.
	//
	// Cache invalidation can also be requested via invalidateCache()
	// (used by both AzureManager and ScaleSet), which manipulates
	// lastRefresh to force refresh on the next check.
	lastRefresh time.Time

	autoDiscoverySpecs   []labelAutoDiscoveryConfig
	explicitlyConfigured map[string]bool

	retentionMutex    sync.Mutex
	retentionGroups   map[string]*scaleSetRetention
	autoscalerOptions *coreoptions.AutoscalerOptions
}

// createAzureManagerInternal allows for a custom azClient to be passed in by tests.
func createAzureManagerInternal(configReader io.Reader, discoveryOpts cloudprovider.NodeGroupDiscoveryOptions, azClient *azClient) (*AzureManager, error) {
	return createAzureManagerWithOptions(configReader, discoveryOpts, azClient, nil)
}

func createAzureManagerWithOptions(configReader io.Reader, discoveryOpts cloudprovider.NodeGroupDiscoveryOptions, azClient *azClient, opts *coreoptions.AutoscalerOptions) (*AzureManager, error) {
	cfg, err := BuildAzureConfig(configReader)
	if err != nil {
		return nil, err
	}

	// Defaulting env to Azure Public Cloud.
	env := azure.PublicCloud
	if cfg.Cloud != "" {
		env, err = azure.EnvironmentFromName(cfg.Cloud)
		if err != nil {
			return nil, err
		}
	}

	klog.Infof("Starting azure manager with subscription ID %q", cfg.SubscriptionID)

	if azClient == nil {
		azClient, err = newAzClient(cfg, &env)
		if err != nil {
			return nil, err
		}
	}

	// Create azure manager.
	manager := &AzureManager{
		config:               cfg,
		env:                  env,
		azClient:             azClient,
		explicitlyConfigured: make(map[string]bool),
		autoscalerOptions:    opts,
	}
	if err := manager.validateRetentionDependencies(); err != nil {
		return nil, err
	}
	if opts != nil && opts.InformerFactory != nil {
		opts.InformerFactory.Core().V1().Nodes().Informer()
	}

	cacheTTL := refreshInterval
	if cfg.VmssCacheTTLInSeconds != 0 {
		cacheTTL = time.Duration(cfg.VmssCacheTTLInSeconds) * time.Second
	}
	cache, err := newAzureCache(azClient, cacheTTL, *cfg)
	if err != nil {
		return nil, err
	}
	manager.azureCache = cache

	if !manager.azureCache.HasVMSKUs() {
		klog.Warning("No VM SKU info loaded, using only static SKU list")
		cfg.EnableDynamicInstanceList = false
	}

	specs, err := ParseLabelAutoDiscoverySpecs(discoveryOpts)
	if err != nil {
		return nil, err
	}
	manager.autoDiscoverySpecs = specs

	if err := manager.fetchExplicitNodeGroups(discoveryOpts.NodeGroupSpecs); err != nil {
		return nil, err
	}

	retryBackoff := wait.Backoff{
		Duration: 2 * time.Minute,
		Factor:   1.0,
		Jitter:   0.1,
		Steps:    6,
		Cap:      10 * time.Minute,
	}

	// skuCache will already be created at this step by newAzureCache()
	err = kretry.OnError(retryBackoff, isErrorRetriable, func() (err error) {
		return manager.forceRefresh()
	})
	if err != nil {
		return nil, err
	}

	return manager, nil
}

// CreateAzureManager creates Azure Manager object to work with Azure.
func CreateAzureManager(configReader io.Reader, discoveryOpts cloudprovider.NodeGroupDiscoveryOptions) (*AzureManager, error) {
	return createAzureManagerInternal(configReader, discoveryOpts, nil)
}

func (m *AzureManager) fetchExplicitNodeGroups(specs []string) error {
	changed := false
	for _, spec := range specs {
		nodeGroup, err := m.buildNodeGroupFromSpec(spec)
		if err != nil {
			return fmt.Errorf("failed to parse node group spec: %v", err)
		}
		if m.RegisterNodeGroup(nodeGroup) {
			changed = true
		}
		m.explicitlyConfigured[nodeGroup.Id()] = true
	}

	if changed {
		m.invalidateCache()
	}
	return nil
}

// parseSKUAndVMsAgentpoolNameFromSpecName parses the spec name for a mixed-SKU VMs pool.
// The spec name should be in the format <agentpoolname>/<sku>, e.g., "mypool1/Standard_D2s_v3", if the agent pool is a VMs pool.
// This method returns a boolean indicating if the agent pool is a VMs pool, along with the agent pool name and SKU.
func (m *AzureManager) parseSKUAndVMsAgentpoolNameFromSpecName(name string) (bool, string, string) {
	parts := strings.Split(name, "/")
	if len(parts) == 2 {
		agentPoolName := parts[0]
		sku := parts[1]

		vmsPoolMap := m.azureCache.getVMsPoolMap()
		if _, ok := vmsPoolMap[agentPoolName]; ok {
			return true, agentPoolName, sku
		}
	}
	return false, "", ""
}

func (m *AzureManager) buildNodeGroupFromSpec(spec string) (cloudprovider.NodeGroup, error) {
	scaleToZeroSupported := scaleToZeroSupportedStandard
	if strings.EqualFold(m.config.VMType, providerazureconsts.VMTypeVMSS) {
		scaleToZeroSupported = scaleToZeroSupportedVMSS
	}
	s, err := dynamic.SpecFromString(spec, scaleToZeroSupported)
	if err != nil {
		return nil, fmt.Errorf("failed to parse node group spec: %v", err)
	}

	// Starting from release 1.30, a cluster may have both VMSS and VMs pools.
	// Therefore, we cannot solely rely on the VMType to determine the node group type.
	// Instead, we need to check the cache to determine if the agent pool is a VMs pool.
	isVMsPool, agentPoolName, sku := m.parseSKUAndVMsAgentpoolNameFromSpecName(s.Name)
	if isVMsPool {
		if m.config.deallocates(s.Name) {
			return nil, fmt.Errorf("Deallocate is not supported for VMs pool %q", s.Name)
		}
		return NewVMPool(s, m, agentPoolName, sku)
	}

	switch m.config.VMType {
	case providerazureconsts.VMTypeStandard:
		if m.config.deallocates(s.Name) {
			return nil, fmt.Errorf("Deallocate is not supported for standard pool %q", s.Name)
		}
		return NewAgentPool(s, m)
	case providerazureconsts.VMTypeVMSS:
		return NewScaleSet(s, m, -1, false)
	default:
		return nil, fmt.Errorf("vmtype %s not supported", m.config.VMType)
	}
}

// Refresh is called before every main loop and can be used to dynamically update cloud provider state.
// In particular the list of node groups returned by NodeGroups can change as a result of CloudProvider.Refresh().
func (m *AzureManager) Refresh() error {
	if m.lastRefresh.Add(m.azureCache.refreshInterval).After(time.Now()) {
		return m.reconcileRetentionGroups(context.Background())
	}
	return m.forceRefresh()
}

func (m *AzureManager) forceRefresh() (err error) {
	defer func() {
		if err != nil {
			m.invalidateRetentionViews(err)
		}
	}()
	retention := false
	for _, policy := range m.config.NodeGroupScaleDownPolicies {
		retention = retention || policy == scaleDownDeallocate
	}
	if retention {
		if err := m.azureCache.fetchAzureResources(); err != nil {
			return err
		}
	}
	if err := m.fetchAutoNodeGroups(); err != nil {
		klog.Errorf("Failed to fetch autodiscovered nodegroups: %v", err)
		return err
	}
	refreshMappings := m.azureCache.regenerate
	if retention {
		refreshMappings = m.azureCache.regenerateInstanceMappings
		if err := m.reconcileRetentionGroups(context.Background()); err != nil {
			return err
		}
	}
	if err := refreshMappings(); err != nil {
		klog.Errorf("Failed to regenerate Azure cache: %v", err)
		return err
	}
	m.lastRefresh = time.Now()
	klog.V(2).Infof("Refreshed Azure VM and VMSS list, next refresh after %v", m.lastRefresh.Add(m.azureCache.refreshInterval))
	return nil
}

// invalidateCache forces cache reload on the next check
// by manipulating lastRefresh timestamp
func (m *AzureManager) invalidateCache() {
	m.lastRefresh = time.Now().Add(-1 * m.azureCache.refreshInterval)
	klog.V(2).Infof("Invalidated Azure cache")
}

// Fetch automatically discovered NodeGroups. These NodeGroups should be unregistered if
// they no longer exist in Azure.
func (m *AzureManager) fetchAutoNodeGroups() error {
	groups, err := m.getFilteredNodeGroups(m.autoDiscoverySpecs)
	if err != nil {
		return fmt.Errorf("cannot autodiscover NodeGroups: %s", err)
	}

	exists := make(map[string]bool)
	for _, group := range groups {
		exists[strings.ToLower(group.Id())] = true
	}
	for _, group := range m.getNodeGroups() {
		if exists[strings.ToLower(group.Id())] || m.isExplicitlyConfigured(group.Id()) {
			continue
		}
		if scaleSet, ok := group.(*ScaleSet); ok && scaleSet.retention != nil {
			return fmt.Errorf("cannot stop discovering live Deallocate node group %q", group.Id())
		}
	}

	changed := false
	for _, group := range groups {
		id := group.Id()
		if m.isExplicitlyConfigured(id) {
			// This NodeGroup was explicitly configured, but would also be
			// autodiscovered. We want the explicitly configured min and max
			// nodes to take precedence.
			klog.V(3).Infof("Ignoring explicitly configured NodeGroup %s for autodiscovery.", group.Id())
			continue
		}
		if m.RegisterNodeGroup(group) {
			klog.V(3).Infof("Autodiscovered NodeGroup %s using tags %v", group.Id(), m.autoDiscoverySpecs)
			changed = true
		}
	}

	for _, nodeGroup := range m.getNodeGroups() {
		nodeGroupID := nodeGroup.Id()
		if !exists[strings.ToLower(nodeGroupID)] && !m.isExplicitlyConfigured(nodeGroupID) {
			m.UnregisterNodeGroup(nodeGroup)
			changed = true
		}
	}

	if changed {
		m.invalidateCache()
	}

	return nil
}

func (m *AzureManager) isExplicitlyConfigured(id string) bool {
	for name, configured := range m.explicitlyConfigured {
		if configured && strings.EqualFold(name, id) {
			return true
		}
	}
	return false
}

func (m *AzureManager) getNodeGroups() []cloudprovider.NodeGroup {
	return m.azureCache.getRegisteredNodeGroups()
}

// RegisterNodeGroup registers an a NodeGroup.
func (m *AzureManager) RegisterNodeGroup(nodeGroup cloudprovider.NodeGroup) bool {
	return m.azureCache.Register(nodeGroup)
}

// UnregisterNodeGroup unregisters a NodeGroup.
func (m *AzureManager) UnregisterNodeGroup(nodeGroup cloudprovider.NodeGroup) bool {
	return m.azureCache.Unregister(nodeGroup)
}

// GetNodeGroupForInstance returns the NodeGroup of the given Instance
func (m *AzureManager) GetNodeGroupForInstance(instance *azureRef) (cloudprovider.NodeGroup, error) {
	return m.azureCache.FindForInstance(instance, m.config.VMType)
}

// GetScaleSetOptions parse options extracted from VMSS tags and merges them with provided defaults
func (m *AzureManager) GetScaleSetOptions(scaleSetName string, defaults config.NodeGroupAutoscalingOptions) *config.NodeGroupAutoscalingOptions {
	options := m.azureCache.getAutoscalingOptions(azureRef{Name: scaleSetName})
	if options == nil || len(options) == 0 {
		return &defaults
	}

	if opt, ok := getFloat64Option(options, scaleSetName, config.DefaultScaleDownUtilizationThresholdKey); ok {
		defaults.ScaleDownUtilizationThreshold = opt
	}
	if opt, ok := getFloat64Option(options, scaleSetName, config.DefaultScaleDownGpuUtilizationThresholdKey); ok {
		defaults.ScaleDownGpuUtilizationThreshold = opt
	}
	if opt, ok := getDurationOption(options, scaleSetName, config.DefaultScaleDownUnneededTimeKey); ok {
		defaults.ScaleDownUnneededTime = opt
	}
	if opt, ok := getDurationOption(options, scaleSetName, config.DefaultScaleDownUnreadyTimeKey); ok {
		defaults.ScaleDownUnreadyTime = opt
	}

	return &defaults
}

// Cleanup the cache.
func (m *AzureManager) Cleanup() {
	m.azureCache.Cleanup()
}

func (m *AzureManager) getFilteredNodeGroups(filter []labelAutoDiscoveryConfig) (nodeGroups []cloudprovider.NodeGroup, err error) {
	if len(filter) == 0 {
		return nil, nil
	}

	if m.config.VMType == providerazureconsts.VMTypeVMSS {
		return m.getFilteredScaleSets(filter)
	}

	return nil, fmt.Errorf("vmType %q does not support autodiscovery", m.config.VMType)
}

// getFilteredScaleSets gets a list of scale sets and instanceIDs.
func (m *AzureManager) getFilteredScaleSets(filter []labelAutoDiscoveryConfig) ([]cloudprovider.NodeGroup, error) {
	vmssList := m.azureCache.getScaleSets()

	var nodeGroups []cloudprovider.NodeGroup
	for _, scaleSet := range vmssList {
		var cfgSizes *autoDiscoveryConfigSizes
		if len(filter) > 0 {
			if scaleSet.Tags == nil || len(scaleSet.Tags) == 0 {
				continue
			}

			if cfgSizes = matchDiscoveryConfig(scaleSet.Tags, filter); cfgSizes == nil {
				continue
			}
		}
		spec := &dynamic.NodeGroupSpec{
			Name:               *scaleSet.Name,
			MinSize:            1,
			MaxSize:            -1,
			SupportScaleToZero: scaleToZeroSupportedVMSS,
		}
		m.retentionMutex.Lock()
		retention := m.retentionGroups[strings.ToLower(spec.Name)] != nil
		m.retentionMutex.Unlock()
		retention = retention || m.config.deallocates(spec.Name)

		if val, ok := scaleSet.Tags["min"]; ok {
			if minSize, err := strconv.Atoi(ptr.Deref(val, "")); err == nil {
				spec.MinSize = minSize
			} else {
				if retention {
					return nil, fmt.Errorf("invalid minimum size for Deallocate vmss %q: %w", spec.Name, err)
				}
				klog.Warningf("ignoring vmss %q because of invalid minimum size specified for vmss: %s", *scaleSet.Name, err)
				continue
			}
		} else if cfgSizes.Min >= 0 {
			spec.MinSize = cfgSizes.Min
		} else {
			if retention {
				return nil, fmt.Errorf("no minimum size specified for Deallocate vmss %q", spec.Name)
			}
			klog.Warningf("ignoring vmss %q because of no minimum size specified for vmss", *scaleSet.Name)
			continue
		}
		if spec.MinSize < 0 {
			if retention {
				return nil, fmt.Errorf("minimum size must be non-negative for Deallocate vmss %q", spec.Name)
			}
			klog.Warningf("ignoring vmss %q because of minimum size must be a non-negative number of nodes", *scaleSet.Name)
			continue
		}
		if val, ok := scaleSet.Tags["max"]; ok {
			if maxSize, err := strconv.Atoi(ptr.Deref(val, "")); err == nil {
				spec.MaxSize = maxSize
			} else {
				if retention {
					return nil, fmt.Errorf("invalid maximum size for Deallocate vmss %q: %w", spec.Name, err)
				}
				klog.Warningf("ignoring vmss %q because of invalid maximum size specified for vmss: %s", *scaleSet.Name, err)
				continue
			}
		} else if cfgSizes.Max >= 0 {
			spec.MaxSize = cfgSizes.Max
		} else {
			if retention {
				return nil, fmt.Errorf("no maximum size specified for Deallocate vmss %q", spec.Name)
			}
			klog.Warningf("ignoring vmss %q because of no maximum size specified for vmss", *scaleSet.Name)
			continue
		}
		if spec.MaxSize < spec.MinSize {
			if retention {
				return nil, fmt.Errorf("maximum size must be at least minimum size for Deallocate vmss %q: max=%d < min=%d", spec.Name, spec.MaxSize, spec.MinSize)
			}
			klog.Warningf("ignoring vmss %q because of maximum size must be greater than or equal to minimum size: max=%d < min=%d", *scaleSet.Name, spec.MaxSize, spec.MinSize)
			continue
		}

		curSize := int64(-1)
		if scaleSet.SKU != nil && scaleSet.SKU.Capacity != nil {
			curSize = *scaleSet.SKU.Capacity
		}

		dedicatedHost := scaleSet.Properties != nil && scaleSet.Properties.HostGroup != nil

		vmss, err := NewScaleSet(spec, m, curSize, dedicatedHost)
		if err != nil {
			if retention {
				return nil, err
			}
			klog.Warningf("ignoring vmss %q %s", *scaleSet.Name, err)
			continue
		}
		nodeGroups = append(nodeGroups, vmss)
	}

	return nodeGroups, nil
}
