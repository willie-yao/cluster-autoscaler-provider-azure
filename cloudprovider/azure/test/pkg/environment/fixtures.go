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
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// CheckPhaseMarker requires the configured phase to be phase and the
// operator marker to allow that phase's fixture for caseID.
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

// CheckBalancePools requires both balance pools to be empty and to share the
// SKU, image, zone and template taint.
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

// CheckPausedController requires the autoscaler Deployment to have zero
// replicas and no Pods, with the image, environment and flags that Controller
// checks once it runs.
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
		return fmt.Errorf("parse paused autoscaler selector: %w", err)
	}
	var pods corev1.PodList
	if err := e.K8s.List(ctx, &pods, client.InNamespace(c.AutoscalerNamespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return fmt.Errorf("list paused autoscaler Pods: %w", err)
	}
	if len(pods.Items) != 0 {
		return fmt.Errorf("autoscaler Pods must stop before recording the starting state, got %d Pods", len(pods.Items))
	}
	found := 0
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name != c.AutoscalerContainer {
			continue
		}
		found++
		if container.Image != c.ExpectedImage {
			return fmt.Errorf("paused autoscaler image %q differs from the candidate %q", container.Image, c.ExpectedImage)
		}
		if err := checkControllerScope(container.Env, c); err != nil {
			return err
		}
		if err := checkPhaseControllerEnv(container.Env, c); err != nil {
			return err
		}
		args := slices.Concat(container.Command, container.Args)
		if err := checkControllerArguments(args, c.DiscoveryValue, c.Phase == "local-storage" && c.SkipLocalStorage != nil && !*c.SkipLocalStorage); err != nil {
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

// ReadControllerLogsSince returns the autoscaler container log since the
// given time. It returns an error if the log is larger than 8 MiB.
func (e *Environment) ReadControllerLogsSince(ctx context.Context, since time.Time) (string, error) {
	c := e.Config
	if e.restConfig == nil {
		return "", fmt.Errorf("reading autoscaler logs requires the client from NewEnvironment")
	}
	k8s, err := kubernetes.NewForConfig(e.restConfig)
	if err != nil {
		return "", fmt.Errorf("create autoscaler log client: %w", err)
	}
	var deployment appsv1.Deployment
	if err := e.K8s.Get(ctx, client.ObjectKey{Namespace: c.AutoscalerNamespace, Name: c.AutoscalerDeployment}, &deployment); err != nil {
		return "", fmt.Errorf("get autoscaler Deployment: %w", err)
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return "", fmt.Errorf("parse autoscaler selector: %w", err)
	}
	var pods corev1.PodList
	if err := e.K8s.List(ctx, &pods, client.InNamespace(c.AutoscalerNamespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return "", fmt.Errorf("list autoscaler Pods: %w", err)
	}
	if len(pods.Items) != 1 {
		return "", fmt.Errorf("expected one autoscaler Pod for plan evidence, got %d", len(pods.Items))
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
		return "", fmt.Errorf("autoscaler plan log exceeds %d bytes", maxLogBytes)
	}
	return string(raw), nil
}

// CheckZeroPoolTaint requires the zero pool's template taint tag to be the
// run ID with the NoSchedule effect.
func CheckZeroPoolTaint(snapshot Snapshot, c Config) error {
	if taint := snapshot.Pools[c.ZeroPool].TemplateTaint; taint != c.RunID+":NoSchedule" {
		return fmt.Errorf("zero pool must have the run-owned NoSchedule node-template taint tag, got %q", taint)
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
	return fmt.Errorf("zero-pool Node has no matching run-owned NoSchedule taint")
}

// CheckDiskFixture checks the operator marker, the run-owned StorageClass and
// the Azure Disk CSI driver, then runs CheckDiskWorkers on baseline.
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
		!strings.EqualFold(class.Parameters["resourceGroup"], c.ResourceGroup) ||
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
