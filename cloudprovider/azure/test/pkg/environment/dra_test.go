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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestDRADevices(t *testing.T) {
	t.Parallel()
	slice := unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{
		"driver": "synthetic", "nodeName": "worker", "pool": map[string]interface{}{"name": "pool"},
		"devices": []interface{}{map[string]interface{}{"name": "device-0"}},
	}}}
	devices, err := DRADevices([]unstructured.Unstructured{slice}, "synthetic")
	if err != nil || devices["synthetic/pool/device-0"] != "worker" {
		t.Fatalf("device-to-Node mapping = %v, error %v", devices, err)
	}
	if _, err := DRADevices([]unstructured.Unstructured{slice, slice}, "synthetic"); err == nil {
		t.Fatal("duplicate slice devices accepted")
	}
	unstructured.RemoveNestedField(slice.Object, "spec", "nodeName")
	if _, err := DRADevices([]unstructured.Unstructured{slice}, "synthetic"); err == nil {
		t.Fatal("missing Node mapping accepted")
	}
}

func TestDRAAllocation(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		results []interface{}
		valid   bool
	}{
		{name: "allocated", results: []interface{}{map[string]interface{}{"driver": "synthetic", "pool": "pool", "device": "device-0"}}, valid: true},
		{name: "requested but unallocated"},
		{name: "wrong driver", results: []interface{}{map[string]interface{}{"driver": "other", "pool": "pool", "device": "device-0"}}},
		{name: "malformed allocation", results: []interface{}{"device-0"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			claim := unstructured.Unstructured{Object: map[string]interface{}{"status": map[string]interface{}{
				"allocation": map[string]interface{}{"devices": map[string]interface{}{"results": tt.results}},
			}}}
			_, err := DRAAllocation(claim, "synthetic", 1)
			if (err == nil) != tt.valid {
				t.Fatalf("allocation error=%v, valid=%v", err, tt.valid)
			}
		})
	}
}
