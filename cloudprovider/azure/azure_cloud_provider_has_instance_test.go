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
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/cluster-autoscaler/pkg/cloudprovider"
	"sigs.k8s.io/cluster-autoscaler/pkg/test/integration"
	synctestutils "sigs.k8s.io/cluster-autoscaler/pkg/test/integration/synctest"
)

func TestProviderOnlyHasInstancePreservesUnmanagedResult(t *testing.T) {
	testCases := []struct {
		name       string
		providerID string
		wantError  error
	}{
		{
			name:       "unsupported instance",
			providerID: azurePrefix + "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/other-pool/virtualMachines/0",
			wantError:  cloudprovider.ErrNotImplemented,
		},
		{
			name:       "invalid resource ID",
			providerID: azurePrefix + "not-a-resource-id",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			world := &parkingWorld{states: []string{vmPowerStateRunning}}
			ordinaryProvider, _ := newParkingProvider(t, world, fake.NewClientset(), 0, 1, false)
			providerOnly, _ := newParkingProvider(t, world, fake.NewClientset(), 0, 1, true)
			node := &apiv1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "unmanaged"},
				Spec:       apiv1.NodeSpec{ProviderID: tc.providerID},
			}

			wantExists, wantErr := ordinaryProvider.HasInstance(t.Context(), node)
			gotExists, gotErr := providerOnly.HasInstance(t.Context(), node)

			require.Equal(t, wantExists, gotExists)
			if wantErr == nil {
				require.NoError(t, gotErr)
			} else {
				require.EqualError(t, gotErr, wantErr.Error())
			}
			if tc.wantError != nil {
				require.ErrorIs(t, wantErr, tc.wantError)
				require.ErrorIs(t, gotErr, tc.wantError)
			} else {
				require.Error(t, wantErr)
				require.Error(t, gotErr)
			}
		})
	}
}

func TestProviderOnlyHasInstanceManagedStates(t *testing.T) {
	testCases := []struct {
		name         string
		instanceID   int
		powerState   string
		provisioning string
		wantExists   bool
		wantError    string
	}{
		{
			name:       "active",
			powerState: vmPowerStateRunning,
			wantExists: true,
		},
		{
			name:       "parked",
			powerState: vmPowerStateDeallocated,
		},
		{
			name:         "deleting",
			powerState:   vmPowerStateRunning,
			provisioning: VMProvisioningStateDeleting,
		},
		{
			name:       "absent",
			instanceID: 1,
			powerState: vmPowerStateRunning,
		},
		{
			name:       "inventory error",
			powerState: vmPowerStateUnknown,
			wantError:  "unknown VM power state",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			world := &parkingWorld{states: []string{vmPowerStateRunning}}
			provider, _ := newParkingProvider(t, world, fake.NewClientset(), 0, 1, true)
			world.mu.Lock()
			world.states[0] = tc.powerState
			world.provisioning = map[int]string{0: tc.provisioning}
			world.mu.Unlock()

			exists, err := provider.HasInstance(t.Context(), parkingNode(tc.instanceID, "managed", true))

			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				require.False(t, exists)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantExists, exists)
		})
	}
}

func TestProviderOnlyUnmanagedNodeIsNotClassifiedAsDeleted(t *testing.T) {
	infra := integration.SetupInfrastructure(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer synctestutils.TearDown(cancel)

		client := infra.Fakes.KubeClient
		world := &parkingWorld{states: []string{vmPowerStateRunning}}
		provider, _ := newParkingProvider(t, world, client, 0, 1, true)
		node := parkingNode(0, "control-plane", true)
		node.Name = "control-plane"
		node.Labels[apiv1.LabelHostname] = node.Name
		node.Spec.ProviderID = strings.Replace(node.Spec.ProviderID, "/pool/", "/control-plane/", 1)
		_, err := client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
		require.NoError(t, err)

		autoscaler := parkingAutoscaler(t, ctx, infra, provider)
		synctestutils.MustRunOnceAfter(t, autoscaler, time.Second)

		readiness := autoscaler.ClusterStateRegistry.GetClusterReadiness()
		require.Contains(t, readiness.Ready, node.Name)
		require.NotContains(t, readiness.Deleted, node.Name)
		status := autoscaler.ClusterStateRegistry.GetStatus(ctx, time.Now())
		require.Zero(t, status.ClusterWide.Health.NodeCounts.Registered.BeingDeleted)
	})
}
