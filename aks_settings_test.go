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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/cluster-autoscaler/pkg/config"
)

func TestDecodeAKSSettings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    string
		want    []string
		wantErr string
	}{
		{
			name: "default policy and empty labels",
			data: `{"nodeGroups":[{"name":"pool-vmss","minSize":0,"maxSize":10}]}`,
			want: []string{"0:10:Delete:pool-vmss:{}|"},
		},
		{
			name: "labels and taints",
			data: `{"nodeGroups":[{"name":"pool/Standard_D2s_v3","minSize":1,"maxSize":50,"scaleDownPolicy":"Delete","labels":{"team":"prod","rack":"east"},"taints":"special=true:NoSchedule"}]}`,
			want: []string{`1:50:Delete:pool/Standard_D2s_v3:{"rack":"east","team":"prod"}|special=true:NoSchedule`},
		},
		{
			name: "yaml settings",
			data: "nodeGroups:\n- name: pool-vmss\n  minSize: 2\n  maxSize: 5\n  labels:\n    team: prod\n",
			want: []string{`2:5:Delete:pool-vmss:{"team":"prod"}|`},
		},
		{
			name: "unrelated profile",
			data: `{"autoScalerProfile":{"scale-down-unneeded-time":"10m"},"nodeGroups":[]}`,
			want: []string{},
		},
		{
			name:    "invalid node groups type",
			data:    `{"nodeGroups":"pool"}`,
			wantErr: "cannot unmarshal",
		},
		{
			name:    "blank name",
			data:    `{"nodeGroups":[{"name":"","minSize":1,"maxSize":5}]}`,
			wantErr: "name must not be blank",
		},
		{
			name:    "negative minimum",
			data:    `{"nodeGroups":[{"name":"pool","minSize":-1,"maxSize":5}]}`,
			wantErr: "min size must be at least 0",
		},
		{
			name:    "maximum below minimum",
			data:    `{"nodeGroups":[{"name":"pool","minSize":10,"maxSize":5}]}`,
			wantErr: "max size must be at least min size",
		},
		{
			name: "mixed policies",
			data: `{"nodeGroups":[{"name":"paused","minSize":1,"maxSize":5,"scaleDownPolicy":"Deallocate"},
				{"name":"active","minSize":0,"maxSize":10,"scaleDownPolicy":"Delete"}]}`,
			want: []string{"1:5:Deallocate:paused:{}|", "0:10:Delete:active:{}|"},
		},
		{
			name:    "unknown policy",
			data:    `{"nodeGroups":[{"name":"pool","minSize":1,"maxSize":5,"scaleDownPolicy":"Keep"}]}`,
			wantErr: "scaleDownPolicy \"Keep\" is not supported; use Delete or Deallocate",
		},
		{
			name:    "invalid deallocate sizes",
			data:    `{"nodeGroups":[{"name":"pool","minSize":5,"maxSize":1,"scaleDownPolicy":"Deallocate"}]}`,
			wantErr: "max size must be at least min size",
		},
		{
			name: "numeric label converted by yaml decoder",
			data: `{"nodeGroups":[{"name":"pool","minSize":1,"maxSize":5,"labels":{"team":42}}]}`,
			want: []string{`1:5:Delete:pool:{"team":"42"}|`},
		},
		{
			name:    "invalid label map",
			data:    `{"nodeGroups":[{"name":"pool","minSize":1,"maxSize":5,"labels":["team"]}]}`,
			wantErr: "cannot unmarshal",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, err := decodeAKSSettings([]byte(tc.data))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("decodeAKSSettings() error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := settings.nodeGroupSpecs()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("nodeGroupSpecs() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUseAKSSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := []string{"1:5:cli-pool"}
	discovery := []string{"label:cluster-autoscaler-enabled=true"}

	opts := config.AutoscalingOptions{NodeGroups: original, NodeGroupAutoDiscovery: discovery}
	fetcher, err := useAKSSettings(&opts, "")
	if err != nil || fetcher != nil {
		t.Fatalf("static mode returned fetcher %v, error %v", fetcher, err)
	}
	if !reflect.DeepEqual(opts.NodeGroups, original) || !reflect.DeepEqual(opts.NodeGroupAutoDiscovery, discovery) {
		t.Fatalf("static mode changed options: %+v", opts)
	}

	fetcher, err = useAKSSettings(&opts, path)
	if err == nil || fetcher == nil {
		t.Fatalf("missing settings returned fetcher %v, error %v", fetcher, err)
	}
	if len(opts.NodeGroups) != 0 || !reflect.DeepEqual(opts.NodeGroupAutoDiscovery, discovery) {
		t.Fatalf("missing file should clear only explicit node groups: %+v", opts)
	}

	writeAKSSettings(t, path, `{"nodeGroups":[{"name":"file-pool","minSize":2,"maxSize":8}]}`)
	opts.NodeGroups = original
	fetcher, err = useAKSSettings(&opts, path)
	if err != nil || fetcher == nil {
		t.Fatalf("valid settings returned fetcher %v, error %v", fetcher, err)
	}
	if !reflect.DeepEqual(opts.NodeGroups, []string{"2:8:Delete:file-pool:{}|"}) ||
		!reflect.DeepEqual(opts.NodeGroupAutoDiscovery, discovery) {
		t.Fatalf("file should replace --nodes but not auto-discovery: %+v", opts)
	}

	writeAKSSettings(t, path, `{"nodeGroups":[{"name":"paused","minSize":1,"maxSize":5,"scaleDownPolicy":"Deallocate"},
		{"name":"file-pool","minSize":2,"maxSize":8}]}`)
	opts.NodeGroups = original
	fetcher, err = useAKSSettings(&opts, path)
	if err != nil || fetcher == nil {
		t.Fatalf("mixed settings returned fetcher %v, error %v", fetcher, err)
	}
	if !reflect.DeepEqual(opts.NodeGroups, []string{"1:5:Deallocate:paused:{}|", "2:8:Delete:file-pool:{}|"}) ||
		!reflect.DeepEqual(opts.NodeGroupAutoDiscovery, discovery) {
		t.Fatalf("file should pass both groups to the provider: %+v", opts)
	}
}

func TestAKSSettingsFetcher(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	fetcher := newAKSSettingsFetcher(path)
	if _, changed, err := fetcher.fetchIfChanged(); err == nil || changed {
		t.Fatalf("missing file returned changed %t, error %v", changed, err)
	}

	writeAKSSettings(t, path, `{"nodeGroups":[]}`)
	if specs, changed, err := fetcher.fetchIfChanged(); err != nil || changed || len(specs) != 0 {
		t.Fatalf("empty groups returned %q, changed %t, error %v", specs, changed, err)
	}
	writeAKSSettings(t, path, `{}`)
	if specs, changed, err := fetcher.fetchIfChanged(); err != nil || changed || len(specs) != 0 {
		t.Fatalf("missing groups returned %q, changed %t, error %v", specs, changed, err)
	}

	base := `{"nodeGroups":[{"name":"pool","minSize":1,"maxSize":5}]}`
	writeAKSSettings(t, path, base)
	if specs, changed, err := fetcher.fetchIfChanged(); err != nil || !changed ||
		!reflect.DeepEqual(specs, []string{"1:5:Delete:pool:{}|"}) {
		t.Fatalf("new group returned %q, changed %t, error %v", specs, changed, err)
	}
	if specs, changed, err := fetcher.fetchIfChanged(); err != nil || changed || specs != nil {
		t.Fatalf("same groups returned %q, changed %t, error %v", specs, changed, err)
	}
	writeAKSSettings(t, path, `{"autoScalerProfile":{"scan-interval":"15s"},"nodeGroups":[{"name":"pool","minSize":1,"maxSize":5}]}`)
	if _, changed, err := fetcher.fetchIfChanged(); err != nil || changed {
		t.Fatalf("profile-only change returned changed %t, error %v", changed, err)
	}
	writeAKSSettings(t, path, `{"nodeGroups":[{"name":"pool","minSize":1,"maxSize":5,"labels":{}}]}`)
	if _, changed, err := fetcher.fetchIfChanged(); err != nil || changed {
		t.Fatalf("empty labels returned changed %t, error %v", changed, err)
	}

	updated := `{"nodeGroups":[{"name":"pool","minSize":1,"maxSize":5,"labels":{"env":"prod"}}]}`
	writeAKSSettings(t, path, updated)
	if specs, changed, err := fetcher.fetchIfChanged(); err != nil || !changed ||
		!reflect.DeepEqual(specs, []string{`1:5:Delete:pool:{"env":"prod"}|`}) {
		t.Fatalf("label change returned %q, changed %t, error %v", specs, changed, err)
	}

	writeAKSSettings(t, path, `{"nodeGroups":"invalid"}`)
	if _, changed, err := fetcher.fetchIfChanged(); err == nil || changed {
		t.Fatalf("invalid update returned changed %t, error %v", changed, err)
	}
	writeAKSSettings(t, path, updated)
	if _, changed, err := fetcher.fetchIfChanged(); err != nil || changed {
		t.Fatalf("restored config returned changed %t, error %v", changed, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := fetcher.fetchIfChanged(); err == nil || changed {
		t.Fatalf("removed file returned changed %t, error %v", changed, err)
	}
}

func writeAKSSettings(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
