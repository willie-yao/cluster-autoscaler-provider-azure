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

// Package environment observes an operator-prepared, disposable Azure cluster.
package environment

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// RunLabel binds Kubernetes objects and Azure resources to one disposable run.
	RunLabel = "autoscaler-e2e-run"
	// MarkerName is created by the operator, never by the test runner.
	MarkerName = "autoscaler-e2e-authorization"
	MaxVMs     = 4
	MaxVCPUs   = 8
)

// Config contains non-secret bindings to an existing environment.
type Config struct {
	Kubeconfig           string `json:"kubeconfig"`
	Context              string `json:"context"`
	ClusterUID           string `json:"clusterUID"`
	RunID                string `json:"runID"`
	SubscriptionID       string `json:"subscriptionID"`
	ResourceGroup        string `json:"resourceGroup"`
	ControlPlaneID       string `json:"controlPlaneID"`
	Location             string `json:"location"`
	DiscoveryValue       string `json:"discoveryValue"`
	AutoscalerNamespace  string `json:"autoscalerNamespace"`
	AutoscalerDeployment string `json:"autoscalerDeployment"`
	AutoscalerContainer  string `json:"autoscalerContainer"`
	LeaseName            string `json:"leaseName"`
	ExpectedImage        string `json:"expectedImage"`
	MainPool             string `json:"mainPool"`
	ZeroPool             string `json:"zeroPool"`
	PoolLabel            string `json:"poolLabel"`
	MainLabel            string `json:"mainLabel"`
	ZeroLabel            string `json:"zeroLabel"`
	DemandCPU            string `json:"demandCPU"`
	WorkloadImage        string `json:"workloadImage"`
	DiskStorageClass     string `json:"diskStorageClass,omitempty"`
}

// LoadConfig rejects misspelled inputs instead of selecting a default cluster.
func LoadConfig(path string) (Config, error) {
	var cfg Config
	f, err := os.Open(path)
	if err != nil {
		return cfg, fmt.Errorf("open environment file: %w", err)
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("decode environment file: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return cfg, fmt.Errorf("environment file must contain exactly one JSON object")
	}
	return cfg, cfg.Validate()
}

// Validate restricts this suite to its bounded two-pool contract.
func (c Config) Validate() error {
	required := map[string]string{
		"kubeconfig": c.Kubeconfig, "context": c.Context, "clusterUID": c.ClusterUID,
		"runID": c.RunID, "subscriptionID": c.SubscriptionID, "resourceGroup": c.ResourceGroup,
		"controlPlaneID": c.ControlPlaneID,
		"location":       c.Location, "discoveryValue": c.DiscoveryValue,
		"autoscalerNamespace": c.AutoscalerNamespace, "autoscalerDeployment": c.AutoscalerDeployment,
		"autoscalerContainer": c.AutoscalerContainer, "leaseName": c.LeaseName,
		"expectedImage": c.ExpectedImage, "mainPool": c.MainPool, "zeroPool": c.ZeroPool,
		"poolLabel": c.PoolLabel, "mainLabel": c.MainLabel, "zeroLabel": c.ZeroLabel,
		"demandCPU": c.DemandCPU, "workloadImage": c.WorkloadImage,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if !filepath.IsAbs(c.Kubeconfig) {
		return fmt.Errorf("kubeconfig must be an absolute, explicit file path")
	}
	if len(validation.IsDNS1123Label(c.RunID)) != 0 {
		return fmt.Errorf("runID must be a DNS label")
	}
	if !regexp.MustCompile(`^[0-9a-fA-F-]{36}$`).MatchString(c.SubscriptionID) {
		return fmt.Errorf("subscriptionID must be an explicit UUID")
	}
	if c.MainPool == c.ZeroPool || c.MainLabel == c.ZeroLabel {
		return fmt.Errorf("main and zero pools and their labels must be distinct")
	}
	cp := strings.Split(c.ControlPlaneID, "/")
	if len(cp) != 9 || !strings.EqualFold(cp[1], "subscriptions") ||
		!strings.EqualFold(cp[2], c.SubscriptionID) || !strings.EqualFold(cp[3], "resourceGroups") ||
		!strings.EqualFold(cp[5], "providers") || !strings.EqualFold(cp[6], "Microsoft.Compute") ||
		!strings.EqualFold(cp[7], "virtualMachines") || cp[4] == "" || cp[8] == "" {
		return fmt.Errorf("controlPlaneID must identify a VM in the selected subscription")
	}
	for _, name := range []string{c.ResourceGroup, c.MainPool, c.ZeroPool, c.Location} {
		if !regexp.MustCompile(`^[a-zA-Z0-9_.()-]+$`).MatchString(name) {
			return fmt.Errorf("invalid Azure resource name %q", name)
		}
	}
	if len(validation.IsQualifiedName(c.PoolLabel)) != 0 ||
		len(validation.IsValidLabelValue(c.MainLabel)) != 0 ||
		len(validation.IsValidLabelValue(c.ZeroLabel)) != 0 {
		return fmt.Errorf("invalid pool label")
	}
	cpu, err := resource.ParseQuantity(c.DemandCPU)
	if err != nil || cpu.MilliValue() <= 0 {
		return fmt.Errorf("demandCPU must be a positive CPU quantity")
	}
	if c.DiskStorageClass != "" && len(validation.IsDNS1123Subdomain(c.DiskStorageClass)) != 0 {
		return fmt.Errorf("diskStorageClass must be a valid StorageClass name")
	}
	return nil
}

func (c Config) resourcePrefix() string {
	return "/subscriptions/" + c.SubscriptionID + "/resourceGroups/" + c.ResourceGroup
}

// PoolID is the Azure resource ID, not a label or a Kubernetes Node name.
func (c Config) PoolID(name string) string {
	return c.resourcePrefix() + "/providers/Microsoft.Compute/virtualMachineScaleSets/" + name
}

func normalizeID(id string) string {
	return strings.ToLower(strings.TrimPrefix(id, "azure://"))
}
