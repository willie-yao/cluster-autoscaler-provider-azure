//go:build helm

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

package charts

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// TestAzureCompatibility compares whole resources with frozen upstream renders.
func TestAzureCompatibility(t *testing.T) {
	_, err := exec.LookPath("helm")
	require.NoError(t, err, "make test-chart requires Helm")
	for _, name := range []string{
		"explicit-sp",
		"discovery-managed",
		"explicit-existing-secret",
		"discovery-managed-existing",
		"discovery-workload-vpa",
		"explicit-namespaced-rbac",
	} {
		t.Run(name, func(t *testing.T) {
			fixture := filepath.Join("testdata", "azure-compatibility", name)
			common := filepath.Join("testdata", "azure-compatibility", "common.yaml")
			lint, err := exec.Command("helm", "lint", "cluster-autoscaler", "--strict",
				"-f", common, "-f", fixture+".values.yaml").CombinedOutput()
			require.NoError(t, err, "%s", lint)
			cmd := exec.Command("helm", "template", "migration", "cluster-autoscaler",
				"--namespace", "kube-system", "--kube-version", "1.37.0-rc.1",
				"-f", common, "-f", fixture+".values.yaml")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			rendered, err := cmd.Output()
			require.NoError(t, err, "%s", stderr.String())
			baseline, err := os.ReadFile(fixture + ".upstream.yaml")
			require.NoError(t, err)
			want := decodeResources(t, baseline)
			got := decodeResources(t, rendered)

			// The managed-identity Secret override is an intentional bootstrap fix.
			if name == "discovery-managed-existing" {
				deployment := resourceOfKind(t, want, "Deployment")
				spec := deployment["spec"].(map[string]interface{})
				pod := spec["template"].(map[string]interface{})["spec"].(map[string]interface{})
				container := pod["containers"].([]interface{})[0].(map[string]interface{})
				matches := 0
				for _, value := range container["env"].([]interface{}) {
					env := value.(map[string]interface{})
					if env["name"] == "ARM_USER_ASSIGNED_IDENTITY_ID" {
						ref := env["valueFrom"].(map[string]interface{})["secretKeyRef"].(map[string]interface{})
						require.Equal(t, "migration-azure-cluster-autoscaler", ref["name"])
						ref["name"] = "existing-azure-config"
						matches++
					}
				}
				require.Equal(t, 1, matches)
			}

			normalizeChartVersionLabel(t, want)
			resourceOfKind(t, want, "Deployment")
			resourceOfKind(t, got, "Deployment")
			if name == "discovery-workload-vpa" {
				resourceOfKind(t, want, "VerticalPodAutoscaler")
				resourceOfKind(t, got, "VerticalPodAutoscaler")
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("Azure chart differs from pinned upstream (-upstream +local):\n%s", diff)
			}
		})
	}
}

func TestAzureCompatibilityChartVersionLabel(t *testing.T) {
	const resource = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: migration
  labels:
    app.kubernetes.io/name: azure-cluster-autoscaler
    helm.sh/chart: cluster-autoscaler-9.59.0
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: azure-cluster-autoscaler
      helm.sh/chart: cluster-autoscaler-9.59.0
`
	tests := []struct {
		name     string
		change   func(map[string]interface{})
		wantDiff bool
	}{
		{name: "chart version only"},
		{
			name: "wrong chart label",
			change: func(object map[string]interface{}) {
				object["metadata"].(map[string]interface{})["labels"].(map[string]interface{})["helm.sh/chart"] = "cluster-autoscaler-9.59.2"
			},
			wantDiff: true,
		},
		{
			name: "unrelated label",
			change: func(object map[string]interface{}) {
				object["metadata"].(map[string]interface{})["labels"].(map[string]interface{})["app.kubernetes.io/name"] = "other"
			},
			wantDiff: true,
		},
		{
			name: "selector chart label",
			change: func(object map[string]interface{}) {
				object["spec"].(map[string]interface{})["selector"].(map[string]interface{})["matchLabels"].(map[string]interface{})["helm.sh/chart"] = "cluster-autoscaler-9.59.1"
			},
			wantDiff: true,
		},
		{
			name: "deployment spec",
			change: func(object map[string]interface{}) {
				object["spec"].(map[string]interface{})["replicas"] = float64(2)
			},
			wantDiff: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := decodeResources(t, []byte(resource))
			got := decodeResources(t, []byte(resource))
			deployment := resourceOfKind(t, got, "Deployment")
			deployment["metadata"].(map[string]interface{})["labels"].(map[string]interface{})["helm.sh/chart"] = "cluster-autoscaler-9.59.1"
			if tt.change != nil {
				tt.change(deployment)
			}
			normalizeChartVersionLabel(t, want)
			diff := cmp.Diff(want, got)
			if tt.wantDiff {
				require.NotEmpty(t, diff)
			} else {
				require.Empty(t, diff)
			}
		})
	}
}

func TestAzureNotes(t *testing.T) {
	_, err := exec.LookPath("helm")
	require.NoError(t, err, "make test-chart requires Helm")
	chart := t.TempDir()
	require.NoError(t, os.CopyFS(chart, os.DirFS("cluster-autoscaler")))
	notes, err := os.ReadFile(filepath.Join(chart, "templates", "NOTES.txt"))
	require.NoError(t, err)
	// helm template omits NOTES.txt, so render its source in a test-only ConfigMap.
	probe := "{{- define \"test.azureNotes\" -}}\n" + string(notes) + "\n{{- end -}}\n" +
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: rendered-notes\n" +
		"data:\n  notes: |-\n{{ include \"test.azureNotes\" . | indent 4 }}\n"
	require.NoError(t, os.WriteFile(filepath.Join(chart, "templates", "rendered-notes.yaml"), []byte(probe), 0o644))

	tests := []struct {
		name           string
		values         []string
		wantDeployment bool
	}{
		{name: "no selector"},
		{name: "labels only", values: []string{"autoDiscovery.labels.team=workers"}},
		{name: "namespace only", values: []string{"autoDiscovery.namespace=workers"}},
		{name: "cluster name", values: []string{"autoDiscovery.clusterName=sample-cluster"}, wantDeployment: true},
		{
			name: "explicit groups",
			values: []string{
				"autoscalingGroups[0].name=pool-a",
				"autoscalingGroups[0].minSize=0",
				"autoscalingGroups[0].maxSize=2",
			},
			wantDeployment: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"template", "migration", chart,
				"--namespace", "kube-system", "--kubeconfig", os.DevNull}
			for _, value := range tt.values {
				args = append(args, "--set", value)
			}
			cmd := exec.Command("helm", args...)
			cmd.Env = append(os.Environ(), "KUBECONFIG="+os.DevNull, "HELM_KUBECONFIG="+os.DevNull)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			output, err := cmd.Output()
			require.NoError(t, err, "%s", stderr.String())
			resources := decodeResources(t, output)
			require.Contains(t, resources, "v1/ConfigMap//rendered-notes")
			renderedNotes := resources["v1/ConfigMap//rendered-notes"]["data"].(map[string]interface{})["notes"].(string)
			hasDeployment := false
			for _, resource := range resources {
				if resource["kind"] == "Deployment" {
					require.False(t, hasDeployment, "expected at most one Deployment")
					hasDeployment = true
				}
			}
			require.Equal(t, tt.wantDeployment, hasDeployment)
			require.Equal(t, tt.wantDeployment, strings.Contains(renderedNotes, "To verify that cluster-autoscaler has started"))
			if !tt.wantDeployment {
				require.Contains(t, renderedNotes, "Set autoDiscovery.clusterName or autoscalingGroups[]")
				require.Contains(t, renderedNotes, "https://github.com/willie-yao/cluster-autoscaler-provider-azure")
			}
		})
	}
}

func normalizeChartVersionLabel(t *testing.T, resources map[string]map[string]interface{}) {
	t.Helper()
	for _, object := range resources {
		metadata := object["metadata"].(map[string]interface{})
		labels, ok := metadata["labels"].(map[string]interface{})
		if !ok {
			continue
		}
		if label, ok := labels["helm.sh/chart"]; ok {
			require.Equal(t, "cluster-autoscaler-9.59.0", label)
			labels["helm.sh/chart"] = "cluster-autoscaler-9.59.1"
		}
	}
}

func decodeResources(t *testing.T, data []byte) map[string]map[string]interface{} {
	t.Helper()
	resources := make(map[string]map[string]interface{})
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var object map[string]interface{}
		err := decoder.Decode(&object)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if len(object) == 0 {
			continue
		}
		meta := object["metadata"].(map[string]interface{})
		namespace, _ := meta["namespace"].(string)
		key := object["apiVersion"].(string) + "/" + object["kind"].(string) + "/" + namespace + "/" + meta["name"].(string)
		require.NotContains(t, resources, key, "duplicate resource")
		resources[key] = object
	}
	return resources
}

func resourceOfKind(t *testing.T, resources map[string]map[string]interface{}, kind string) map[string]interface{} {
	t.Helper()
	var found map[string]interface{}
	for _, object := range resources {
		if object["kind"] == kind {
			require.Nil(t, found, "expected exactly one %s", kind)
			found = object
		}
	}
	require.NotNil(t, found, "expected exactly one %s", kind)
	return found
}
