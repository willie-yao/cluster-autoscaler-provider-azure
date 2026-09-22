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
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// DRADevices maps synthetic device identities to the Nodes advertising them.
func DRADevices(slices []unstructured.Unstructured, driver string) (map[string]string, error) {
	result := map[string]string{}
	for _, slice := range slices {
		sliceDriver, _, err := unstructured.NestedString(slice.Object, "spec", "driver")
		if err != nil {
			return nil, err
		}
		if sliceDriver != driver {
			continue
		}
		node, _, err := unstructured.NestedString(slice.Object, "spec", "nodeName")
		if err != nil || node == "" {
			return nil, fmt.Errorf("synthetic DRA fixture requires node-local ResourceSlices")
		}
		pool, _, err := unstructured.NestedString(slice.Object, "spec", "pool", "name")
		if err != nil || pool == "" {
			return nil, fmt.Errorf("DRA slice has no pool identity")
		}
		devices, found, err := unstructured.NestedSlice(slice.Object, "spec", "devices")
		if err != nil || !found || len(devices) == 0 {
			return nil, fmt.Errorf("DRA slice has no devices")
		}
		for _, entry := range devices {
			device, ok := entry.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("invalid DRA device object")
			}
			name, _, err := unstructured.NestedString(device, "name")
			if err != nil || name == "" {
				return nil, fmt.Errorf("DRA device has no name")
			}
			key := driver + "/" + pool + "/" + name
			if _, exists := result[key]; exists {
				return nil, fmt.Errorf("duplicate DRA device identity %s", key)
			}
			result[key] = node
		}
	}
	return result, nil
}

// DRAAllocation requires actual allocation results, not merely a submitted claim.
func DRAAllocation(claim unstructured.Unstructured, driver string, expected int) ([]string, error) {
	results, found, err := unstructured.NestedSlice(claim.Object, "status", "allocation", "devices", "results")
	if err != nil || !found || len(results) != expected {
		return nil, fmt.Errorf("claim has %d allocated devices, want %d", len(results), expected)
	}
	var keys []string
	seen := map[string]bool{}
	for _, entry := range results {
		result, ok := entry.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("invalid claim allocation result")
		}
		d, _, err := unstructured.NestedString(result, "driver")
		if err != nil || d != driver {
			return nil, fmt.Errorf("claim allocation uses unexpected driver")
		}
		pool, _, err := unstructured.NestedString(result, "pool")
		if err != nil || pool == "" {
			return nil, fmt.Errorf("claim allocation has no pool")
		}
		device, _, err := unstructured.NestedString(result, "device")
		if err != nil || device == "" {
			return nil, fmt.Errorf("claim allocation has no device")
		}
		key := driver + "/" + pool + "/" + device
		if seen[key] {
			return nil, fmt.Errorf("claim allocated the same device twice")
		}
		seen[key] = true
		keys = append(keys, key)
	}
	return keys, nil
}
