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
	"errors"
	"testing"

	"k8s.io/utils/ptr"
)

func TestCheckScaleDownTags(t *testing.T) {
	t.Parallel()
	const prefix = "k8s.io_cluster-autoscaler_node-template_autoscaling-options_"
	for _, tt := range []struct {
		name, unneeded, utilization string
		valid                       bool
	}{
		{name: "global settings", valid: true},
		{name: "bounded pool overrides", unneeded: "30s", utilization: "0.50", valid: true},
		{name: "provider lowercases duration", unneeded: "1M", valid: true},
		{name: "longer than negative observation", unneeded: "10m"},
		{name: "negative duration", unneeded: "-1m"},
		{name: "invalid duration", unneeded: "soon"},
		{name: "conflicting utilization", utilization: "0.1"},
		{name: "invalid utilization", utilization: "NaN"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tags := map[string]*string{}
			if tt.unneeded != "" {
				tags[prefix+"scaledownunneededtime"] = ptr.To(tt.unneeded)
			}
			if tt.utilization != "" {
				tags[prefix+"scaledownutilizationthreshold"] = ptr.To(tt.utilization)
			}
			if err := checkScaleDownTags(tags); (err == nil) != tt.valid {
				t.Fatalf("tag error=%v, valid=%v", err, tt.valid)
			}
		})
	}
}

func TestCheckPeakEnvelope(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name           string
		main, zero, cp int
		valid          bool
		bounds         bool
	}{
		{name: "four two-core VMs at maximum", main: 2, zero: 2, cp: 2, valid: true},
		{name: "idle zero pool hides sixteen-core SKU", main: 2, zero: 16, cp: 2, bounds: true},
		{name: "main peak exceeds current one-worker count", main: 4, zero: 2, cp: 2, bounds: true},
		{name: "control plane included", main: 2, zero: 2, cp: 4, bounds: true},
		{name: "unknown zero pool cores", main: 2, zero: 0, cp: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := checkPeakEnvelope(tt.main, tt.zero, tt.cp)
			if (err == nil) != tt.valid || errors.Is(err, ErrBounds) != tt.bounds {
				t.Fatalf("peak error=%v, valid=%v", err, tt.valid)
			}
		})
	}
}
