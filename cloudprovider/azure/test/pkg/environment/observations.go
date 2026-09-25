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
	resourcehelper "k8s.io/kubectl/pkg/util/resource"
	"sigs.k8s.io/yaml"
)

// ErrBounds identifies positively observed resource-envelope or per-pool breaches.
var ErrBounds = errors.New("resource bounds violated")

// Instance records only identifiers needed to prove physical deletion.
type Instance struct {
	ID   string
	NICs []string
}

// PoolState separates requested capacity from actual cloud instances.
type PoolState struct {
	Capacity      int
	Instances     map[string]Instance
	TemplateTaint string
}

// Snapshot contains no credentials, bootstrap settings, Pod specs or logs.
type Snapshot struct {
	Pools map[string]PoolState
	Nodes []corev1.Node
	VMs   int
	VCPUs int
}

// Ready requires the Ready condition to be explicitly true.
func Ready(node corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// PoolNodes selects by cloud identity, never by cluster-wide node count.
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

// WorkerNodes returns only instances of the two explicitly bound pools.
func WorkerNodes(nodes []corev1.Node, c Config) []corev1.Node {
	return append(PoolNodes(nodes, c.PoolID(c.MainPool)), PoolNodes(nodes, c.PoolID(c.ZeroPool))...)
}

// CheckBounds checks observed pools before any convergence checks.
func (s Snapshot) CheckBounds(c Config) error {
	if s.VMs > MaxVMs || s.VCPUs > MaxVCPUs {
		return fmt.Errorf("%w: %d VMs, %d vCPUs", ErrBounds, s.VMs, s.VCPUs)
	}
	for _, name := range []string{c.MainPool, c.ZeroPool} {
		pool, ok := s.Pools[name]
		if !ok {
			continue
		}
		minimum, maximum := 1, 2
		if name == c.ZeroPool {
			minimum, maximum = 0, 1
		}
		if err := checkCapacity(name, pool.Capacity, minimum, maximum); err != nil {
			return err
		}
		if len(pool.Instances) < minimum || len(pool.Instances) > maximum {
			return fmt.Errorf("%w: pool %s actual=%d, bounds %d..%d",
				ErrBounds, name, len(pool.Instances), minimum, maximum)
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

// Stable requires a one-to-one mapping between Azure instances and Ready Nodes.
func (s Snapshot) Stable(c Config, main, zero int) error {
	if err := s.CheckBounds(c); err != nil {
		return err
	}
	for name, expected := range map[string]int{c.MainPool: main, c.ZeroPool: zero} {
		pool, ok := s.Pools[name]
		minimum, maximum := 1, 2
		if name == c.ZeroPool {
			minimum, maximum = 0, 1
		}
		if expected < minimum || expected > maximum {
			return fmt.Errorf("pool %s: expected=%d violates bounds %d..%d", name, expected, minimum, maximum)
		}
		if !ok || pool.Capacity != expected || len(pool.Instances) != expected {
			return fmt.Errorf("pool %s: desired=%d actual=%d, want %d", name, pool.Capacity, len(pool.Instances), expected)
		}
		nodes := PoolNodes(s.Nodes, c.PoolID(name))
		if len(nodes) != expected {
			return fmt.Errorf("pool %s: %d registered Nodes, want %d", name, len(nodes), expected)
		}
		seen := map[string]bool{}
		label := c.MainLabel
		if name == c.ZeroPool {
			label = c.ZeroLabel
		}
		for _, node := range nodes {
			id := normalizeID(node.Spec.ProviderID)
			if _, ok := pool.Instances[id]; !ok || seen[id] || !Ready(node) ||
				node.Spec.Unschedulable || node.DeletionTimestamp != nil || node.Labels[c.PoolLabel] != label {
				return fmt.Errorf("pool %s: Node %s is not a unique Ready schedulable instance with expected label", name, node.Name)
			}
			seen[id] = true
		}
	}
	return nil
}

// ValidateDemand proves that one demand Pod fits, but two cannot share a worker.
func ValidateDemand(node corev1.Node, pods []corev1.Pod, demandMilliCPU int64) error {
	occupied, err := NodeRequests(node, pods)
	if err != nil {
		return err
	}
	available := node.Status.Allocatable.Cpu().MilliValue() - occupied.Cpu().MilliValue()
	if available < demandMilliCPU || available >= 2*demandMilliCPU {
		return fmt.Errorf("Node %s has %dm free CPU; require demand <= free < 2*demand (%dm)", node.Name, available, demandMilliCPU)
	}
	return nil
}

// NodeRequests includes Kubernetes init-container, sidecar and overhead accounting.
func NodeRequests(node corev1.Node, pods []corev1.Pod) (corev1.ResourceList, error) {
	occupied := corev1.ResourceList{}
	for _, pod := range pods {
		if pod.Spec.NodeName != node.Name || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		// The retained kubectl helper predates Pod-level request accounting.
		if pod.Spec.Resources != nil {
			return nil, fmt.Errorf("Node %s has Pod %s/%s with unsupported Pod-level requests", node.Name, pod.Namespace, pod.Name)
		}
		requests, _ := resourcehelper.PodRequestsAndLimits(&pod)
		for name, request := range requests {
			sum := occupied[name]
			sum.Add(request)
			occupied[name] = sum
		}
	}
	return occupied, nil
}

// CheckStatus reads the controller's reported group inventory, not just Azure tags.
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
	if err != nil || now.Sub(at) > 2*time.Minute || at.After(now.Add(10*time.Second)) {
		return fmt.Errorf("autoscaler status is missing or stale")
	}
	if parsed.AutoscalerStatus != "Running" || len(parsed.NodeGroups) != len(expected) {
		return fmt.Errorf("autoscaler must be Running with exactly %d groups", len(expected))
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
