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
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcehelper "k8s.io/component-helpers/resource"
	"sigs.k8s.io/yaml"
)

// ErrBounds marks a pool that has more VMs or requested capacity than its
// max tag allows.
var ErrBounds = errors.New("pool maximum exceeded")

// Instance holds the identifiers of one VMSS VM that prove its deletion.
type Instance struct {
	ID   string
	NICs []string
}

// PoolState holds the requested capacity, max tag, provisioning state and
// actual VMs of one VMSS.
type PoolState struct {
	Capacity          int
	Max               int
	ProvisioningState string
	TemplateTaint     string
	Instances         map[string]Instance
}

// Snapshot is one reading of the pools and Nodes. It holds no credentials,
// bootstrap settings, Pod specs or logs.
type Snapshot struct {
	Pools map[string]PoolState
	Nodes []corev1.Node
}

// Ready reports whether node has a true Ready condition.
func Ready(node corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// PoolNodes returns the Nodes whose provider ID is a VM of the VMSS with
// poolID.
func PoolNodes(nodes []corev1.Node, poolID string) []corev1.Node {
	var result []corev1.Node
	prefix := normalizeID(poolID) + "/virtualmachines/"
	for _, node := range nodes {
		if strings.HasPrefix(normalizeID(node.Spec.ProviderID), prefix) {
			result = append(result, node)
		}
	}
	return result
}

// WorkerNodes returns the Nodes of the main and zero pools.
func WorkerNodes(nodes []corev1.Node, c Config) []corev1.Node {
	return append(PoolNodes(nodes, c.PoolID(c.MainPool)), PoolNodes(nodes, c.PoolID(c.ZeroPool))...)
}

// checkMax returns an ErrBounds error when a pool's capacity or VM count is
// above its max tag.
func (s Snapshot) checkMax() error {
	for name, pool := range s.Pools {
		if pool.Capacity > pool.Max || len(pool.Instances) > pool.Max {
			return fmt.Errorf("%w: pool %s desired=%d actual=%d max=%d",
				ErrBounds, name, pool.Capacity, len(pool.Instances), pool.Max)
		}
	}
	return nil
}

// Stable requires main and zero to have exactly the given counts for their
// capacity, VMs and Ready, schedulable Nodes with the pool label, one Node
// for each VM, and a Succeeded VMSS.
func (s Snapshot) Stable(c Config, main, zero int) error {
	for _, want := range []struct {
		name, label string
		count       int
	}{{c.MainPool, c.MainLabel, main}, {c.ZeroPool, c.ZeroLabel, zero}} {
		pool, ok := s.Pools[want.name]
		if !ok {
			return fmt.Errorf("pool %s is missing from the observation", want.name)
		}
		if pool.ProvisioningState != "Succeeded" {
			return fmt.Errorf("pool %s is %s, want Succeeded", want.name, pool.ProvisioningState)
		}
		if pool.Capacity != want.count || len(pool.Instances) != want.count {
			return fmt.Errorf("pool %s: desired=%d actual=%d, want %d", want.name, pool.Capacity, len(pool.Instances), want.count)
		}
		nodes := PoolNodes(s.Nodes, c.PoolID(want.name))
		if len(nodes) != want.count {
			return fmt.Errorf("pool %s: %d registered Nodes, want %d", want.name, len(nodes), want.count)
		}
		seen := map[string]bool{}
		for _, node := range nodes {
			id := normalizeID(node.Spec.ProviderID)
			if _, ok := pool.Instances[id]; !ok || seen[id] || !Ready(node) ||
				node.Spec.Unschedulable || node.DeletionTimestamp != nil || node.Labels[c.PoolLabel] != want.label {
				return fmt.Errorf("pool %s: Node %s is not a unique Ready schedulable instance with the pool label", want.name, node.Name)
			}
			seen[id] = true
		}
	}
	return nil
}

// ValidateDemand requires node to have room for one Pod that requests
// demandMilliCPU, but not for two.
func ValidateDemand(node corev1.Node, pods []corev1.Pod, demandMilliCPU int64) error {
	occupied, err := NodeRequests(node, pods)
	if err != nil {
		return err
	}
	available := node.Status.Allocatable.Cpu().MilliValue() - occupied.Cpu().MilliValue()
	if available < demandMilliCPU || available >= 2*demandMilliCPU {
		return fmt.Errorf("node %s has %dm free CPU; require demand <= free < 2*demand (%dm)", node.Name, available, demandMilliCPU)
	}
	return nil
}

// NodeRequests sums the requests of the active Pods on node, including init
// containers, sidecars and overhead.
func NodeRequests(node corev1.Node, pods []corev1.Pod) (corev1.ResourceList, error) {
	occupied := corev1.ResourceList{}
	for _, pod := range pods {
		if pod.Spec.NodeName != node.Name || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		// This fixture does not use Pod-level request accounting.
		if pod.Spec.Resources != nil {
			return nil, fmt.Errorf("node %s has Pod %s/%s with unsupported Pod-level requests", node.Name, pod.Namespace, pod.Name)
		}
		requests := resourcehelper.PodRequests(&pod, resourcehelper.PodResourcesOptions{})
		for name, request := range requests {
			sum := occupied[name]
			sum.Add(request)
			occupied[name] = sum
		}
	}
	return occupied, nil
}

// CheckStatus requires the autoscaler status to be less than two minutes old,
// Running and listing exactly the expected node groups.
func CheckStatus(status string, expected []string, now time.Time) error {
	var parsed struct {
		Time             string `json:"time"`
		AutoscalerStatus string `json:"autoscalerStatus"`
		NodeGroups       []struct {
			Name string `json:"name"`
		} `json:"nodeGroups"`
	}
	if err := yaml.Unmarshal([]byte(status), &parsed); err != nil {
		return fmt.Errorf("decode autoscaler status: %w", err)
	}
	at, err := time.Parse(time.RFC3339, parsed.Time)
	if err != nil {
		at, err = time.Parse("2006-01-02 15:04:05.999999999 -0700 MST", parsed.Time)
	}
	if err != nil {
		return fmt.Errorf("autoscaler status time %q is missing or invalid: %w", parsed.Time, err)
	}
	if now.Sub(at) > 2*time.Minute || at.After(now.Add(10*time.Second)) {
		return fmt.Errorf("autoscaler status time %s is stale at %s", at.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	if parsed.AutoscalerStatus != "Running" || len(parsed.NodeGroups) != len(expected) {
		return fmt.Errorf("autoscaler must be Running with exactly %d groups, got %s with %d groups",
			len(expected), parsed.AutoscalerStatus, len(parsed.NodeGroups))
	}
	names := map[string]bool{}
	for _, group := range parsed.NodeGroups {
		names[group.Name] = true
	}
	for _, name := range expected {
		if !names[name] {
			return fmt.Errorf("autoscaler did not report expected group %s", name)
		}
	}
	return nil
}
