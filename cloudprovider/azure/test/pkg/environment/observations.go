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

// ErrBounds marks an observed breach of the VM, vCPU or per-pool limits.
var ErrBounds = errors.New("resource bounds violated")

// Instance holds the identifiers of one VMSS VM that prove its deletion.
type Instance struct {
	ID                string
	VMID              string
	ProvisioningState string
	NICs              []string
}

// PoolState holds the requested capacity and the actual VMs of one VMSS.
type PoolState struct {
	Capacity            int
	Instances           map[string]Instance
	TemplateTaint       string
	SKU                 string
	Image               string
	Zone                string
	FailedExtensionName string `json:"-"`
}

// Snapshot is one reading of the pools and Nodes. It holds no credentials,
// bootstrap settings, Pod specs or logs.
type Snapshot struct {
	Pools map[string]PoolState
	Nodes []corev1.Node
	VMs   int
	VCPUs int
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

// CheckBounds returns an ErrBounds error when the snapshot exceeds the VM or
// vCPU limit, or a pool's capacity or VM count is outside its bounds.
func (s Snapshot) CheckBounds(c Config) error {
	maxVMs, maxVCPUs := c.Limits()
	if s.VMs > maxVMs || s.VCPUs > maxVCPUs {
		return fmt.Errorf("%w: %d VMs, %d vCPUs", ErrBounds, s.VMs, s.VCPUs)
	}
	for name, bounds := range c.Pools() {
		pool, ok := s.Pools[name]
		if !ok {
			continue
		}
		if err := checkCapacity(name, pool.Capacity, bounds.ObservedMin, bounds.Max); err != nil {
			return err
		}
		if len(pool.Instances) < bounds.ObservedMin || len(pool.Instances) > bounds.Max {
			return fmt.Errorf("%w: pool %s actual=%d, bounds %d..%d",
				ErrBounds, name, len(pool.Instances), bounds.ObservedMin, bounds.Max)
		}
	}
	return nil
}

func checkCapacity(name string, capacity, minimum, maximum int) error {
	if capacity < minimum || capacity > maximum {
		return fmt.Errorf("%w: pool %s desired=%d, bounds %d..%d", ErrBounds, name, capacity, minimum, maximum)
	}
	return nil
}

// Stable works like StablePools for main and zero.
func (s Snapshot) Stable(c Config, main, zero int) error {
	return s.StablePools(c, map[string]int{c.MainPool: main, c.ZeroPool: zero})
}

// StableAfterMissing works like Stable after the missing-vmss phase deletes
// its empty group, and requires that group to be absent.
func (s Snapshot) StableAfterMissing(c Config, main, zero int) error {
	if c.Phase != "missing-vmss" {
		return fmt.Errorf("surviving-pool stability requires the missing-vmss phase")
	}
	if _, exists := s.Pools[c.MissingPool]; exists {
		return fmt.Errorf("deleted VMSS still exists")
	}
	// The default phase has the bounds of the two pools that remain.
	surviving := c
	surviving.Phase = ""
	return s.StablePools(surviving, map[string]int{c.MainPool: main, c.ZeroPool: zero})
}

// StablePools requires each pool of the phase to have exactly the count in
// expected for its capacity, VMs and Ready schedulable Nodes, with one Node
// for each VM.
func (s Snapshot) StablePools(c Config, expected map[string]int) error {
	if err := s.CheckBounds(c); err != nil {
		return err
	}
	bounds := c.Pools()
	if len(expected) != len(bounds) {
		return fmt.Errorf("expected capacity for every authorized pool")
	}
	for name, count := range expected {
		pool, ok := s.Pools[name]
		limit, authorized := bounds[name]
		if !authorized {
			return fmt.Errorf("unexpected pool %s in stable check", name)
		}
		if count < limit.ObservedMin || count > limit.Max {
			return fmt.Errorf("pool %s: expected=%d violates bounds %d..%d", name, count, limit.ObservedMin, limit.Max)
		}
		if !ok || pool.Capacity != count || len(pool.Instances) != count {
			return fmt.Errorf("pool %s: desired=%d actual=%d, want %d", name, pool.Capacity, len(pool.Instances), count)
		}
		nodes := PoolNodes(s.Nodes, c.PoolID(name))
		if len(nodes) != count {
			return fmt.Errorf("pool %s: %d registered Nodes, want %d", name, len(nodes), count)
		}
		seen := map[string]bool{}
		for _, node := range nodes {
			id := normalizeID(node.Spec.ProviderID)
			if _, ok := pool.Instances[id]; !ok || seen[id] || !Ready(node) ||
				node.Spec.Unschedulable || node.DeletionTimestamp != nil || node.Labels[c.PoolLabel] != limit.Label {
				return fmt.Errorf("pool %s: Node %s is not a unique Ready schedulable instance with expected label", name, node.Name)
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
