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
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ZeroPoolTaintTag is the VMSS tag that gives zero-pool Nodes the run taint.
	ZeroPoolTaintTag = "k8s.io_cluster-autoscaler_node-template_taint_" + RunLabel
	// DiskCSIDriver is the Azure Disk CSI driver name.
	DiskCSIDriver = "disk.csi.azure.com"
)

// CheckZeroPoolTaint requires the zero pool's node-template taint tag to be
// the run ID with the NoSchedule effect.
func CheckZeroPoolTaint(snapshot Snapshot, c Config) error {
	if taint := snapshot.Pools[c.ZeroPool].TemplateTaint; taint != c.RunID+":NoSchedule" {
		return fmt.Errorf("zero pool must have the run's NoSchedule node-template taint tag, got %q", taint)
	}
	return nil
}

// CheckZeroNodeTaint requires the one zero-pool Node to have the run taint
// with the NoSchedule effect.
func CheckZeroNodeTaint(snapshot Snapshot, c Config) error {
	nodes := PoolNodes(snapshot.Nodes, c.PoolID(c.ZeroPool))
	if len(nodes) != 1 {
		return fmt.Errorf("expected one registered zero-pool Node, got %d", len(nodes))
	}
	for _, taint := range nodes[0].Spec.Taints {
		if taint.Key == RunLabel && taint.Value == c.RunID && taint.Effect == corev1.TaintEffectNoSchedule {
			return nil
		}
	}
	return fmt.Errorf("zero-pool Node has no matching NoSchedule run taint")
}

// CheckDiskFixture checks that the disk StorageClass uses the Azure Disk
// CSI driver with WaitForFirstConsumer binding and that baseline has one main
// worker, then runs CheckDiskWorkers on baseline.
func (e *Environment) CheckDiskFixture(ctx context.Context, baseline Snapshot) error {
	c := e.Config
	if c.DiskStorageClass == "" {
		return fmt.Errorf("diskStorageClass is required for the disk case")
	}
	var class storagev1.StorageClass
	if err := e.K8s.Get(ctx, client.ObjectKey{Name: c.DiskStorageClass}, &class); err != nil {
		return fmt.Errorf("read disk StorageClass: %w", err)
	}
	if class.Provisioner != DiskCSIDriver ||
		class.VolumeBindingMode == nil || *class.VolumeBindingMode != storagev1.VolumeBindingWaitForFirstConsumer {
		return fmt.Errorf("disk StorageClass must use %s with WaitForFirstConsumer binding", DiskCSIDriver)
	}
	if len(PoolNodes(baseline.Nodes, c.PoolID(c.MainPool))) != 1 {
		return fmt.Errorf("disk case requires one baseline main worker")
	}
	return e.CheckDiskWorkers(ctx, baseline)
}

// CheckDiskWorkers requires every main worker to be in the same zone and to
// have the Azure Disk CSI driver registered.
func (e *Environment) CheckDiskWorkers(ctx context.Context, snapshot Snapshot) error {
	nodes := PoolNodes(snapshot.Nodes, e.Config.PoolID(e.Config.MainPool))
	if len(nodes) == 0 {
		return fmt.Errorf("disk case requires a main worker")
	}
	zone := nodes[0].Labels[corev1.LabelTopologyZone]
	if zone == "" {
		return fmt.Errorf("main worker has no disk topology zone")
	}
	for _, node := range nodes {
		if node.Labels[corev1.LabelTopologyZone] != zone {
			return fmt.Errorf("main workers have different disk topology zones")
		}
		var csiNode storagev1.CSINode
		if err := e.K8s.Get(ctx, client.ObjectKey{Name: node.Name}, &csiNode); err != nil {
			return fmt.Errorf("read main worker CSI registration: %w", err)
		}
		registered := false
		for _, driver := range csiNode.Spec.Drivers {
			registered = registered || driver.Name == DiskCSIDriver
		}
		if !registered {
			return fmt.Errorf("Azure Disk CSI driver is not registered on main worker %s", node.Name)
		}
	}
	return nil
}

// ReadDiskIdentity returns the contents of /data/identity in the work
// container of pod.
func (e *Environment) ReadDiskIdentity(ctx context.Context, namespace, pod string) (string, error) {
	config := e.restConfig
	if config == nil {
		return "", fmt.Errorf("reading the disk test file requires the client from NewEnvironment")
	}
	pods, err := kubernetes.NewForConfig(config)
	if err != nil {
		return "", fmt.Errorf("create disk test Pod client: %w", err)
	}
	request := pods.CoreV1().RESTClient().Post().Namespace(namespace).Resource("pods").Name(pod).
		SubResource("exec").VersionedParams(&corev1.PodExecOptions{
		Container: "work", Command: []string{"cat", "/data/identity"}, Stdout: true, Stderr: true,
	}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(config, "POST", request.URL())
	if err != nil {
		return "", fmt.Errorf("connect to disk test Pod: %w", err)
	}
	var output, stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &output, Stderr: &stderr}); err != nil {
		return "", fmt.Errorf("read disk test file from Pod %s: %w, stderr: %q", pod, err, stderr.String())
	}
	return output.String(), nil
}
