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

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCompareInstanceTypes(t *testing.T) {
	committed := instanceTypes{
		"unchanged": {"InstanceType": "unchanged", "SkuFamily": "A", "VCPU": "1", "MemoryMb": "1024", "GPU": "0", "Architecture": "amd64"},
		"changed":   {"InstanceType": "changed", "SkuFamily": "A", "VCPU": "1", "MemoryMb": "1024", "GPU": "0", "Architecture": "amd64"},
		"removed":   {"InstanceType": "removed"},
	}
	live := instanceTypes{
		"unchanged": {"InstanceType": "unchanged", "SkuFamily": "A", "VCPU": "1", "MemoryMb": "1024", "GPU": "0", "Architecture": "amd64"},
		"changed":   {"InstanceType": "different", "SkuFamily": "B", "VCPU": "2", "MemoryMb": "2048", "GPU": "1", "Architecture": "arm64"},
		"added":     {"InstanceType": "added"},
	}
	diff := compareInstanceTypes(committed, live)
	want := comparison{
		added:   []string{"added"},
		removed: []string{"removed"},
		changed: []skuChange{{name: "changed", fields: []fieldChange{
			{"InstanceType", "changed", "different"},
			{"SkuFamily", "A", "B"},
			{"VCPU", "1", "2"},
			{"MemoryMb", "1024", "2048"},
			{"GPU", "0", "1"},
			{"Architecture", "amd64", "arm64"},
		}}},
	}
	if !reflect.DeepEqual(diff, want) || !diff.hasChanges() {
		t.Fatalf("comparison = %#v, want %#v", diff, want)
	}
	var output bytes.Buffer
	diff.write(&output)
	for _, line := range []string{
		"Summary: 1 added, 1 removed, 1 changed",
		"Added: added",
		"Removed: removed",
		"Changed: changed",
		"  GPU: \"0\" -> \"1\"",
	} {
		if !strings.Contains(output.String(), line) {
			t.Errorf("report missing %q: %s", line, output.String())
		}
	}
	if compareInstanceTypes(committed, committed).hasChanges() {
		t.Error("identical lists differ")
	}
}

func TestReadInstanceTypes(t *testing.T) {
	const source = `package azure
var InstanceTypes = map[string]*InstanceType{
	"Standard_A": {
		InstanceType: "Standard_A", SkuFamily: "A", VCPU: 2,
		MemoryMb: 4096, GPU: 0, Architecture: "amd64",
	},
}`
	path := filepath.Join(t.TempDir(), "azure_instance_types.go")
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readInstanceTypes(path)
	if err != nil {
		t.Fatal(err)
	}
	want := instanceTypes{"Standard_A": {
		"InstanceType": "Standard_A", "SkuFamily": "A", "VCPU": "2",
		"MemoryMb": "4096", "GPU": "0", "Architecture": "amd64",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed = %#v, want %#v", got, want)
	}
}

func TestReadInstanceTypesRejectsMissingField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "azure_instance_types.go")
	source := `package azure
var InstanceTypes = map[string]*InstanceType{
	"Standard_A": {InstanceType: "Standard_A"},
}`
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := readInstanceTypes(path)
	if err == nil || !strings.Contains(err.Error(), "missing SkuFamily") {
		t.Fatalf("expected missing field error, got %v", err)
	}
}
