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
	"io"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// CheckPhaseMarker requires an operator opt-in for each optional fixture.
func (e *Environment) CheckPhaseMarker(ctx context.Context, phase, caseID string) error {
	if e.Config.Phase != phase {
		return fmt.Errorf("%s requires the %s phase", caseID, phase)
	}
	var marker corev1.ConfigMap
	if err := e.K8s.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: MarkerName}, &marker); err != nil {
		return fmt.Errorf("read %s fixture marker: %w", phase, err)
	}
	if marker.Data["allow-"+phase+"-fixture"] != caseID {
		return fmt.Errorf("operator marker must allow the %s fixture for %s", phase, caseID)
	}
	return nil
}

// CheckBalancePools requires two matching empty groups for the balance case.
func CheckBalancePools(snapshot Snapshot, c Config) error {
	a, aFound := snapshot.Pools[c.BalancePoolA]
	b, bFound := snapshot.Pools[c.BalancePoolB]
	if c.Phase != "balance" || !aFound || !bFound ||
		a.Capacity != 0 || b.Capacity != 0 || len(a.Instances) != 0 || len(b.Instances) != 0 ||
		a.SKU == "" || a.SKU != b.SKU || a.Image == "" || a.Image != b.Image ||
		a.Zone == "" || a.Zone != b.Zone || a.TemplateTaint != b.TemplateTaint {
		return fmt.Errorf("balance pools must be empty and share the SKU, image, zone and taints")
	}
	return nil
}

// CheckPausedController checks the planned flags before the operator starts the controller.
func (e *Environment) CheckPausedController(ctx context.Context) error {
	c := e.Config
	var deployment appsv1.Deployment
	if err := e.K8s.Get(ctx, client.ObjectKey{Namespace: c.AutoscalerNamespace, Name: c.AutoscalerDeployment}, &deployment); err != nil {
		return fmt.Errorf("read paused autoscaler: %w", err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 || deployment.Status.ReadyReplicas != 0 {
		return fmt.Errorf("autoscaler must remain paused until the case records its starting state")
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return err
	}
	var pods corev1.PodList
	if err := e.K8s.List(ctx, &pods, client.InNamespace(c.AutoscalerNamespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return err
	}
	if len(pods.Items) != 0 {
		return fmt.Errorf("autoscaler Pods must stop before recording the starting state")
	}
	found := 0
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name != c.AutoscalerContainer {
			continue
		}
		found++
		if container.Image != c.ExpectedImage {
			return fmt.Errorf("paused autoscaler image differs from the candidate")
		}
		if err := checkControllerScope(container.Env, c); err != nil {
			return err
		}
		args := append(append([]string{}, container.Command...), container.Args...)
		if err := CheckControllerArguments(args, c.DiscoveryValue); err != nil {
			return err
		}
		if err := CheckPhaseArguments(args, c); err != nil {
			return err
		}
	}
	if found != 1 {
		return fmt.Errorf("paused autoscaler must have one expected container")
	}
	return nil
}

// ReadControllerLogsSince returns bounded, private log text for the balance-plan assertion.
func (e *Environment) ReadControllerLogsSince(ctx context.Context, since time.Time) (string, error) {
	c := e.Config
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: c.Kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: c.Context},
	).ClientConfig()
	if err != nil {
		return "", fmt.Errorf("load autoscaler log kubeconfig: %w", err)
	}
	config.Timeout = 30 * time.Second
	k8s, err := kubernetes.NewForConfig(config)
	if err != nil {
		return "", fmt.Errorf("create autoscaler log client: %w", err)
	}
	var deployment appsv1.Deployment
	if err := e.K8s.Get(ctx, client.ObjectKey{Namespace: c.AutoscalerNamespace, Name: c.AutoscalerDeployment}, &deployment); err != nil {
		return "", err
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return "", err
	}
	var pods corev1.PodList
	if err := e.K8s.List(ctx, &pods, client.InNamespace(c.AutoscalerNamespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return "", err
	}
	if len(pods.Items) != 1 {
		return "", fmt.Errorf("expected one autoscaler Pod for plan evidence")
	}
	timestamp := metav1.NewTime(since)
	stream, err := k8s.CoreV1().Pods(c.AutoscalerNamespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{
		Container: c.AutoscalerContainer, SinceTime: &timestamp,
	}).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("read autoscaler plan log: %w", err)
	}
	defer stream.Close()
	const maxLogBytes = 8 << 20
	raw, err := io.ReadAll(io.LimitReader(stream, maxLogBytes+1))
	if err != nil {
		return "", fmt.Errorf("read autoscaler plan log: %w", err)
	}
	if len(raw) > maxLogBytes {
		return "", fmt.Errorf("autoscaler plan log exceeds the allowed size")
	}
	return string(raw), nil
}

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
