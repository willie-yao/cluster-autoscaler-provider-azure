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

package azure

import (
	"fmt"
	"strings"

	"sigs.k8s.io/cluster-autoscaler/pkg/config/dynamic"
)

const (
	scaleDownPolicyDelete     = "Delete"
	scaleDownPolicyDeallocate = "Deallocate"
)

type azureNodeGroupSpec struct {
	*dynamic.NodeGroupSpec
	policy string
}

// parseAzureNodeGroupSpec accepts min:max:name and min:max:policy:name.
func parseAzureNodeGroupSpec(value string, scaleToZeroSupported bool) (azureNodeGroupSpec, error) {
	parts := strings.Split(value, ":")
	if len(parts) > 4 {
		return azureNodeGroupSpec{}, fmt.Errorf("invalid node group spec %q: want three or four fields", value)
	}
	if len(parts) <= 3 {
		spec, err := dynamic.SpecFromString(value, scaleToZeroSupported)
		if err != nil {
			return azureNodeGroupSpec{}, err
		}
		return azureNodeGroupSpec{NodeGroupSpec: spec, policy: scaleDownPolicyDelete}, nil
	}
	if parts[2] != scaleDownPolicyDelete && parts[2] != scaleDownPolicyDeallocate {
		return azureNodeGroupSpec{}, fmt.Errorf("invalid scale down policy %q: want Delete or Deallocate", parts[2])
	}
	spec, err := dynamic.SpecFromString(parts[0]+":"+parts[1]+":"+parts[3], scaleToZeroSupported)
	if err != nil {
		return azureNodeGroupSpec{}, err
	}
	return azureNodeGroupSpec{NodeGroupSpec: spec, policy: parts[2]}, nil
}

func hasExplicitDeallocatePolicy(specs []string) bool {
	for _, spec := range specs {
		parsed, err := parseAzureNodeGroupSpec(spec, true)
		if err == nil && parsed.policy == scaleDownPolicyDeallocate {
			return true
		}
	}
	return false
}
