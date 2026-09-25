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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ZeroPoolTaintTag = "k8s.io_cluster-autoscaler_node-template_taint_" + RunLabel
	DiskCSIDriver    = "disk.csi.azure.com"
)

// CheckZeroPoolTaint requires the exact run-owned taint on the empty pool template.
func CheckZeroPoolTaint(snapshot Snapshot, c Config) error {
	if snapshot.Pools[c.ZeroPool].TemplateTaint != c.RunID+":NoSchedule" {
		return fmt.Errorf("zero pool must have the run-owned NoSchedule node-template taint tag")
	}
	return nil
}

// CheckZeroNodeTaint requires the template taint on the actual zero-pool Node.
func CheckZeroNodeTaint(snapshot Snapshot, c Config) error {
	nodes := PoolNodes(snapshot.Nodes, c.PoolID(c.ZeroPool))
	if len(nodes) != 1 {
		return fmt.Errorf("expected one registered zero-pool Node")
	}
	for _, taint := range nodes[0].Spec.Taints {
		if taint.Key == RunLabel && taint.Value == c.RunID && taint.Effect == corev1.TaintEffectNoSchedule {
			return nil
		}
	}
	return fmt.Errorf("zero-pool Node has no matching run-owned NoSchedule taint")
}

// CheckDiskFixture verifies the operator's disk setup before a workload is created.
func (e *Environment) CheckDiskFixture(ctx context.Context, baseline Snapshot) error {
	c := e.Config
	if c.DiskStorageClass == "" {
		return fmt.Errorf("diskStorageClass is required for the disk case")
	}
	var marker corev1.ConfigMap
	if err := e.K8s.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: MarkerName}, &marker); err != nil {
		return fmt.Errorf("read disk fixture marker: %w", err)
	}
	if marker.Data["allow-disk-fixture"] != "AZ-P1-006" {
		return fmt.Errorf("operator marker must allow the disk fixture for AZ-P1-006")
	}
	var class storagev1.StorageClass
	if err := e.K8s.Get(ctx, client.ObjectKey{Name: c.DiskStorageClass}, &class); err != nil {
		return fmt.Errorf("read disk StorageClass: %w", err)
	}
	if class.Labels[RunLabel] != c.RunID || class.Provisioner != DiskCSIDriver ||
		class.VolumeBindingMode == nil || *class.VolumeBindingMode != storagev1.VolumeBindingWaitForFirstConsumer ||
		class.ReclaimPolicy == nil || *class.ReclaimPolicy != corev1.PersistentVolumeReclaimDelete ||
		class.Parameters["resourceGroup"] != c.ResourceGroup ||
		class.Parameters["skuName"] != "StandardSSD_LRS" ||
		!strings.EqualFold(class.Parameters["subscriptionID"], c.SubscriptionID) ||
		!hasRunTag(class.Parameters["tags"], c.RunID) {
		return fmt.Errorf("disk StorageClass must use run-owned Standard SSD disks in the authorized subscription and resource group")
	}
	var driver storagev1.CSIDriver
	if err := e.K8s.Get(ctx, client.ObjectKey{Name: DiskCSIDriver}, &driver); err != nil {
		return fmt.Errorf("read Azure Disk CSI driver: %w", err)
	}
	if driver.Spec.AttachRequired != nil && !*driver.Spec.AttachRequired {
		return fmt.Errorf("Azure Disk CSI driver must attach disks")
	}
	if len(PoolNodes(baseline.Nodes, c.PoolID(c.MainPool))) != 1 {
		return fmt.Errorf("disk case requires one baseline main worker")
	}
	return e.CheckDiskWorkers(ctx, baseline)
}

// CheckDiskWorkers requires the CSI driver and one disk zone on every main worker.
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

func hasRunTag(tags, runID string) bool {
	found := 0
	for _, tag := range strings.Split(tags, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(tag), "=")
		if ok && key == RunLabel {
			if value != runID {
				return false
			}
			found++
		}
	}
	return found == 1
}

// ReadDiskIdentity reads only the test file through the selected kubeconfig context.
func (e *Environment) ReadDiskIdentity(ctx context.Context, namespace, pod string) (string, error) {
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: e.Config.Kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: e.Config.Context},
	).ClientConfig()
	if err != nil {
		return "", fmt.Errorf("load disk test kubeconfig: %w", err)
	}
	config.Timeout = 30 * time.Second
	pods, err := kubernetes.NewForConfig(config)
	if err != nil {
		return "", fmt.Errorf("create disk test Pod client: %w", err)
	}
	request := pods.CoreV1().RESTClient().Post().Namespace(namespace).Resource("pods").Name(pod).
		SubResource("exec").VersionedParams(&corev1.PodExecOptions{
		Container: "work", Command: []string{"cat", "/data/identity"}, Stdout: true,
	}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(config, "POST", request.URL())
	if err != nil {
		return "", fmt.Errorf("connect to disk test Pod: %w", err)
	}
	var output bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &output}); err != nil {
		return "", fmt.Errorf("read disk test file from Pod %s: %w", pod, err)
	}
	return output.String(), nil
}
