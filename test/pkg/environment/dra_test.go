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
	"testing"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"
)

func TestDRADevices(t *testing.T) {
	t.Parallel()
	slice := resourcev1.ResourceSlice{Spec: resourcev1.ResourceSliceSpec{
		Driver: "synthetic", NodeName: ptr.To("worker"), Pool: resourcev1.ResourcePool{Name: "pool"},
		Devices: []resourcev1.Device{{Name: "device-0"}},
	}}
	devices, err := DRADevices([]resourcev1.ResourceSlice{slice}, "synthetic")
	if err != nil || devices["synthetic/pool/device-0"] != "worker" {
		t.Fatalf("device-to-Node mapping = %v, error %v", devices, err)
	}
	if _, err := DRADevices([]resourcev1.ResourceSlice{slice, slice}, "synthetic"); err == nil {
		t.Fatal("duplicate slice devices accepted")
	}
	slice.Spec.NodeName = nil
	if _, err := DRADevices([]resourcev1.ResourceSlice{slice}, "synthetic"); err == nil {
		t.Fatal("missing Node mapping accepted")
	}
}

func TestDRAAllocation(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		allocation *resourcev1.AllocationResult
		valid      bool
	}{
		{name: "allocated", valid: true, allocation: &resourcev1.AllocationResult{Devices: resourcev1.DeviceAllocationResult{
			Results: []resourcev1.DeviceRequestAllocationResult{{Driver: "synthetic", Pool: "pool", Device: "device-0"}},
		}}},
		{name: "requested but unallocated"},
		{name: "wrong driver", allocation: &resourcev1.AllocationResult{Devices: resourcev1.DeviceAllocationResult{
			Results: []resourcev1.DeviceRequestAllocationResult{{Driver: "other", Pool: "pool", Device: "device-0"}},
		}}},
		{name: "allocation without a device", allocation: &resourcev1.AllocationResult{Devices: resourcev1.DeviceAllocationResult{
			Results: []resourcev1.DeviceRequestAllocationResult{{Driver: "synthetic", Pool: "pool"}},
		}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			claim := resourcev1.ResourceClaim{Status: resourcev1.ResourceClaimStatus{Allocation: tt.allocation}}
			_, err := DRAAllocation(claim, "synthetic", 1)
			if (err == nil) != tt.valid {
				t.Fatalf("allocation error=%v, valid=%v", err, tt.valid)
			}
		})
	}
}
