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
	"encoding/json"
	"fmt"
	"os"
	"slices"

	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/cluster-autoscaler/pkg/config"
)

const (
	scaleDownPolicyDelete     = "Delete"
	scaleDownPolicyDeallocate = "Deallocate"
)

type aksNodeGroup struct {
	Name            string            `json:"name"`
	MinSize         int               `json:"minSize"`
	MaxSize         int               `json:"maxSize"`
	ScaleDownPolicy string            `json:"scaleDownPolicy"`
	Labels          map[string]string `json:"labels"`
	Taints          string            `json:"taints"`
}

type aksSettings struct {
	NodeGroups []aksNodeGroup `json:"nodeGroups"`
}

func decodeAKSSettings(data []byte) (aksSettings, error) {
	var settings aksSettings
	if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096).Decode(&settings); err != nil {
		return aksSettings{}, fmt.Errorf("failed to decode autoscaler settings: %w", err)
	}

	for i := range settings.NodeGroups {
		group := &settings.NodeGroups[i]
		if group.ScaleDownPolicy == "" {
			group.ScaleDownPolicy = scaleDownPolicyDelete
		}
		if group.Name == "" {
			return aksSettings{}, fmt.Errorf("invalid node group: name must not be blank")
		}
		if group.MinSize < 0 {
			return aksSettings{}, fmt.Errorf("invalid node group %q: min size must be at least 0", group.Name)
		}
		if group.MaxSize < group.MinSize {
			return aksSettings{}, fmt.Errorf("invalid node group %q: max size must be at least min size", group.Name)
		}
		if group.ScaleDownPolicy != scaleDownPolicyDelete && group.ScaleDownPolicy != scaleDownPolicyDeallocate {
			return aksSettings{}, fmt.Errorf("invalid node group %q: scaleDownPolicy %q is not supported; use Delete or Deallocate", group.Name, group.ScaleDownPolicy)
		}
	}
	return settings, nil
}

func (s aksSettings) nodeGroupSpecs() ([]string, error) {
	result := make([]string, 0, len(s.NodeGroups))
	for _, group := range s.NodeGroups {
		labels := group.Labels
		if len(labels) == 0 {
			labels = map[string]string{}
		}
		encodedLabels, err := json.Marshal(labels)
		if err != nil {
			return nil, fmt.Errorf("failed to encode labels for node group %q: %w", group.Name, err)
		}
		result = append(result, fmt.Sprintf("%d:%d:%s:%s:%s|%s",
			group.MinSize, group.MaxSize, group.ScaleDownPolicy, group.Name, encodedLabels, group.Taints))
	}
	return result, nil
}

type aksSettingsFetcher struct {
	path string
	// previous holds the node group specs from the last successful read.
	previous []string
}

func newAKSSettingsFetcher(path string) *aksSettingsFetcher {
	return &aksSettingsFetcher{path: path}
}

// fetchIfChanged reads the settings file and returns its node group specs and
// true when they differ from the last successful read. It returns nil and
// false when they are unchanged or the file can't be used.
func (f *aksSettingsFetcher) fetchIfChanged() ([]string, bool, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		return nil, false, fmt.Errorf("failed to read settings file %q: %w", f.path, err)
	}
	settings, err := decodeAKSSettings(data)
	if err != nil {
		return nil, false, fmt.Errorf("failed to load settings file %q: %w", f.path, err)
	}
	specs, err := settings.nodeGroupSpecs()
	if err != nil {
		return nil, false, err
	}
	if slices.Equal(f.previous, specs) {
		return nil, false, nil
	}
	f.previous = specs
	return specs, true, nil
}

// useAKSSettings replaces the --nodes groups in opts with the groups from the
// settings file at path, and returns a fetcher that watches the file for
// changes. An empty path leaves opts unchanged and returns a nil fetcher.
// The fetcher is returned even when the first read fails, so a later fix to
// the file still restarts the pod.
func useAKSSettings(opts *config.AutoscalingOptions, path string) (*aksSettingsFetcher, error) {
	if path == "" {
		return nil, nil
	}

	fetcher := newAKSSettingsFetcher(path)
	opts.NodeGroups = []string{}
	specs, changed, err := fetcher.fetchIfChanged()
	if err != nil {
		return fetcher, err
	}
	if changed {
		opts.NodeGroups = specs
	}
	return fetcher, nil
}
