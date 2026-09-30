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

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"
)

// DRADevices maps each device of driver, keyed as driver/pool/device, to the
// Node whose ResourceSlice lists it. It returns an error for a slice that is
// not node-local, has no pool or devices, or repeats a device.
func DRADevices(resourceSlices []resourcev1.ResourceSlice, driver string) (map[string]string, error) {
	result := map[string]string{}
	for _, slice := range resourceSlices {
		if slice.Spec.Driver != driver {
			continue
		}
		node := ptr.Deref(slice.Spec.NodeName, "")
		if node == "" {
			return nil, fmt.Errorf("synthetic DRA fixture requires node-local ResourceSlices, slice %s has no Node", slice.Name)
		}
		pool := slice.Spec.Pool.Name
		if pool == "" {
			return nil, fmt.Errorf("DRA slice %s has no pool identity", slice.Name)
		}
		if len(slice.Spec.Devices) == 0 {
			return nil, fmt.Errorf("DRA slice %s has no devices", slice.Name)
		}
		for _, device := range slice.Spec.Devices {
			if device.Name == "" {
				return nil, fmt.Errorf("DRA slice %s has a device with no name", slice.Name)
			}
			key := driver + "/" + pool + "/" + device.Name
			if _, exists := result[key]; exists {
				return nil, fmt.Errorf("duplicate DRA device identity %s", key)
			}
			result[key] = node
		}
	}
	return result, nil
}

// DRAAllocation returns the driver/pool/device keys that claim has allocated.
// It returns an error unless the claim status holds exactly expected distinct
// results from driver, so a claim that is only requested does not count.
func DRAAllocation(claim resourcev1.ResourceClaim, driver string, expected int) ([]string, error) {
	var results []resourcev1.DeviceRequestAllocationResult
	if claim.Status.Allocation != nil {
		results = claim.Status.Allocation.Devices.Results
	}
	if claim.Status.Allocation == nil || len(results) != expected {
		return nil, fmt.Errorf("claim %s has %d allocated devices, want %d", claim.Name, len(results), expected)
	}
	keys := make([]string, 0, len(results))
	seen := map[string]bool{}
	for _, result := range results {
		if result.Driver != driver {
			return nil, fmt.Errorf("claim %s allocation uses driver %q, want %q", claim.Name, result.Driver, driver)
		}
		if result.Pool == "" || result.Device == "" {
			return nil, fmt.Errorf("claim %s allocation has no pool or device", claim.Name)
		}
		key := driver + "/" + result.Pool + "/" + result.Device
		if seen[key] {
			return nil, fmt.Errorf("claim %s allocated device %s twice", claim.Name, key)
		}
		seen[key] = true
		keys = append(keys, key)
	}
	return keys, nil
}
