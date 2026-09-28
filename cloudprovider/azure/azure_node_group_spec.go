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
	"encoding/json"
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
	labels map[string]string
	taints string
}

// parseAzureNodeGroupSpec parses a node group spec. It accepts the core form
// <min>:<max>:<name>, the AKS form <min>:<max>:<policy>:<name>, and the AKS form
// with labels and taints, <min>:<max>:<policy>:<name>:<labels JSON>|<taints>.
// The policy is Delete or Deallocate, and the core form uses Delete.
func parseAzureNodeGroupSpec(value string, scaleToZeroSupported bool) (azureNodeGroupSpec, error) {
	parts := strings.SplitN(value, ":", 5)
	if len(parts) <= 3 {
		spec, err := dynamic.SpecFromString(value, scaleToZeroSupported)
		if err != nil {
			return azureNodeGroupSpec{}, err
		}
		return azureNodeGroupSpec{NodeGroupSpec: spec, policy: scaleDownPolicyDelete}, nil
	}

	if parts[2] != scaleDownPolicyDelete && parts[2] != scaleDownPolicyDeallocate {
		return azureNodeGroupSpec{}, fmt.Errorf("invalid scale down policy %q in node group spec %q: want Delete or Deallocate", parts[2], value)
	}
	spec, err := dynamic.SpecFromString(parts[0]+":"+parts[1]+":"+parts[3], scaleToZeroSupported)
	if err != nil {
		return azureNodeGroupSpec{}, fmt.Errorf("node group spec %q: %w", value, err)
	}

	result := azureNodeGroupSpec{NodeGroupSpec: spec, policy: parts[2]}
	if len(parts) == 5 {
		labelsAndTaints := strings.SplitN(parts[4], "|", 2)
		if err := json.Unmarshal([]byte(labelsAndTaints[0]), &result.labels); err != nil {
			return azureNodeGroupSpec{}, fmt.Errorf("invalid labels in node group spec %q: %w", value, err)
		}
		if len(labelsAndTaints) == 2 {
			result.taints = labelsAndTaints[1]
		}
	}
	return result, nil
}
