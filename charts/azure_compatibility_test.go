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
