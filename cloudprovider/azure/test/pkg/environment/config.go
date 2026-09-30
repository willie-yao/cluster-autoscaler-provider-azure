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

// Package environment observes a prepared, disposable Azure cluster.
package environment

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// RunLabel marks the Kubernetes objects that one run creates.
const RunLabel = "autoscaler-e2e-run"

// Config is the JSON binding to one prepared cluster. The test README
// describes each field.
type Config struct {
	// Kubeconfig is the absolute path of the cluster's kubeconfig.
	Kubeconfig string `json:"kubeconfig"`
	// RunID names the run's labels, taint and PriorityClasses.
	RunID string `json:"runID"`
	// SubscriptionID and ResourceGroup select the VMSS of the two pools.
	SubscriptionID string `json:"subscriptionID"`
	ResourceGroup  string `json:"resourceGroup"`
	// The Autoscaler fields and ExpectedImage identify the one expected
	// controller.
	AutoscalerNamespace  string `json:"autoscalerNamespace"`
	AutoscalerDeployment string `json:"autoscalerDeployment"`
	AutoscalerContainer  string `json:"autoscalerContainer"`
	ExpectedImage        string `json:"expectedImage"`
	// MainPool, ZeroPool and the label fields name the two VMSS and the
	// Node label that selects them.
	MainPool  string `json:"mainPool"`
	ZeroPool  string `json:"zeroPool"`
	PoolLabel string `json:"poolLabel"`
	MainLabel string `json:"mainLabel"`
	ZeroLabel string `json:"zeroLabel"`
	// DemandCPU is the CPU request that fills one worker, and WorkloadImage
	// runs every test Pod.
	DemandCPU        string `json:"demandCPU"`
	WorkloadImage    string `json:"workloadImage"`
	DiskStorageClass string `json:"diskStorageClass,omitempty"`
}

// LoadConfig reads one JSON object from path, rejects unknown fields and
// validates the result.
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

// Validate returns an error for a missing or malformed field.
func (c Config) Validate() error {
	required := map[string]string{
		"kubeconfig": c.Kubeconfig, "runID": c.RunID, "subscriptionID": c.SubscriptionID,
		"resourceGroup": c.ResourceGroup, "autoscalerNamespace": c.AutoscalerNamespace,
		"autoscalerDeployment": c.AutoscalerDeployment, "autoscalerContainer": c.AutoscalerContainer,
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
		return fmt.Errorf("kubeconfig must be an absolute file path")
	}
	if strings.EqualFold(c.MainPool, c.ZeroPool) || c.MainLabel == c.ZeroLabel {
		return fmt.Errorf("main and zero pools and their labels must be distinct")
	}
	cpu, err := resource.ParseQuantity(c.DemandCPU)
	if err != nil || cpu.MilliValue() <= 0 {
		return fmt.Errorf("demandCPU must be a positive CPU quantity")
	}
	return nil
}

// PoolID returns the Azure resource ID of the VMSS name.
func (c Config) PoolID(name string) string {
	return "/subscriptions/" + c.SubscriptionID + "/resourceGroups/" + c.ResourceGroup +
		"/providers/Microsoft.Compute/virtualMachineScaleSets/" + name
}

func normalizeID(id string) string {
	return strings.ToLower(strings.TrimPrefix(id, "azure://"))
}
