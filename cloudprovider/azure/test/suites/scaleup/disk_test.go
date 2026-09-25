//go:build e2e

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

package scaleup_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

type diskPodState struct {
	PodUID       types.UID
	Node         string
	ClaimUID     types.UID
	VolumeUID    types.UID
	VolumeName   string
	VolumeHandle string
	Identity     string
}

var _ = Describe("Azure Disk StatefulSet", Serial, Label("uniform", "disk"), func() {
	It("AZ-P1-006 reattaches StatefulSet disks after a worker is physically deleted", Label("AZ-P1-006", "delete"), func(ctx SpecContext) {
		baseline := waitStable(ctx, 1, 0)
		Expect(env.CheckDiskFixture(ctx, baseline)).To(Succeed())
		Expect(env.DemandFits(ctx, baseline, env.Config.MainPool)).To(Succeed())

		growth := env.Deployment(namespace.Name, "disk-growth", env.Config.MainLabel, env.Config.DemandCPU, 2)
		Expect(env.K8s.Create(ctx, growth)).To(Succeed())
		grown := waitStable(ctx, 2, 0)
		assertDistinctNodes(waitWorkload(ctx, growth.Name, env.Config.MainPool, 2, 0), 2)
		Eventually(ctx, func() error {
			return env.CheckDiskWorkers(ctx, grown)
		}, 3*time.Minute, pollInterval).Should(Succeed())

		stateful := diskStatefulSet("disk-data")
		service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: stateful.Name, Namespace: namespace.Name, Labels: stateful.Labels},
			Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone, Selector: stateful.Spec.Selector.MatchLabels,
				Ports: []corev1.ServicePort{{Name: "data", Port: 80}}}}
		Expect(env.K8s.Create(ctx, service)).To(Succeed())
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: stateful.Name, Namespace: namespace.Name, Labels: stateful.Labels},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MinAvailable: ptr.To(intstr.FromInt32(1)),
				Selector:     stateful.Spec.Selector.DeepCopy(),
			},
		}
		Expect(env.K8s.Create(ctx, pdb)).To(Succeed())
		Expect(env.K8s.Create(ctx, stateful)).To(Succeed())

		var before map[string]diskPodState
		Eventually(ctx, func() error {
			var err error
			before, err = diskPods(ctx, stateful)
			if err != nil {
				return err
			}
			placed := map[string]bool{}
			for _, pod := range before {
				placed[pod.Node] = true
				if pod.Identity != string(pod.PodUID) {
					return fmt.Errorf("StatefulSet Pod did not write its original UID to the disk")
				}
			}
			if len(placed) != 2 {
				return fmt.Errorf("StatefulSet Pods must start on separate main workers")
			}
			return nil
		}, settleTimeout, pollInterval).Should(Succeed())
		Eventually(ctx, func(g Gomega) {
			var current policyv1.PodDisruptionBudget
			g.Expect(env.K8s.Get(ctx, client.ObjectKeyFromObject(pdb), &current)).To(Succeed())
			g.Expect(current.Status.ObservedGeneration).To(Equal(current.Generation))
			g.Expect(current.Status.CurrentHealthy).To(Equal(int32(2)))
			g.Expect(current.Status.DisruptionsAllowed).To(Equal(int32(1)))
		}, time.Minute, pollInterval).Should(Succeed())

		Expect(env.K8s.Delete(ctx, growth)).To(Succeed())
		var deleted environment.Snapshot
		Eventually(ctx, func(g Gomega) {
			healthy, err := protectedReadyPods(ctx, namespace.Name, stateful.Spec.Selector.MatchLabels)
			g.Expect(err).NotTo(HaveOccurred())
			if err == nil {
				Expect(healthy).To(BeNumerically(">=", 1), "one StatefulSet Pod must remain Ready during drain")
			}
			after, err := readSnapshot(ctx)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(after.Stable(env.Config, 1, 0)).To(Succeed())
			g.Expect(env.Deleted(ctx, grown, after, env.Config.MainPool, 1)).To(Succeed())
			deleted = after
		}, settleTimeout, pollInterval).Should(Succeed())
		waitDeleted(ctx, grown, env.Config.MainPool, 1, 1, 0)

		var after map[string]diskPodState
		survivor := environment.PoolNodes(deleted.Nodes, env.Config.PoolID(env.Config.MainPool))[0].Name
		Eventually(ctx, func() error {
			var err error
			after, err = diskPods(ctx, stateful)
			if err != nil {
				return err
			}
			moved := 0
			for name, original := range before {
				current, ok := after[name]
				if !ok || current.Node != survivor || current.ClaimUID != original.ClaimUID ||
					current.VolumeUID != original.VolumeUID || current.VolumeName != original.VolumeName ||
					current.VolumeHandle != original.VolumeHandle || current.Identity != original.Identity {
					return fmt.Errorf("StatefulSet Pod %s did not keep its attached disk and data on the surviving worker", name)
				}
				if current.Node != original.Node {
					if current.PodUID == original.PodUID {
						return fmt.Errorf("moved StatefulSet Pod %s still has its original Pod UID", name)
					}
					moved++
				}
			}
			if moved == 0 {
				return fmt.Errorf("no StatefulSet Pod and disk moved to the surviving worker")
			}
			return nil
		}, settleTimeout, pollInterval).Should(Succeed())
		AddReportEntry("azure-disks", fmt.Sprintf("%d PVCs kept their PVs, Azure disk handles and file contents; at least one disk reattached after physical deletion", len(after)))
	}, NodeTimeout(65*time.Minute))
})

func diskStatefulSet(name string) *appsv1.StatefulSet {
	workload := env.Deployment(namespace.Name, name, env.Config.MainLabel, "50m", 2)
	template := workload.Spec.Template
	template.Annotations = map[string]string{"cluster-autoscaler.kubernetes.io/safe-to-evict": "true"}
	template.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
		PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
			Weight: 100, PodAffinityTerm: corev1.PodAffinityTerm{
				TopologyKey: "kubernetes.io/hostname", LabelSelector: workload.Spec.Selector.DeepCopy(),
			},
		}},
	}}
	container := &template.Spec.Containers[0]
	container.Command = []string{"sh", "-c", `if [ ! -f /data/identity ]; then printf '%s' "$POD_UID" > /data/identity; fi; while true; do sleep 3600; done`}
	container.Env = []corev1.EnvVar{{Name: "POD_UID", ValueFrom: &corev1.EnvVarSource{
		FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"},
	}}}
	container.VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
	container.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
		Exec: &corev1.ExecAction{Command: []string{"sh", "-c", "test -s /data/identity"}},
	}, PeriodSeconds: 5}
	return &appsv1.StatefulSet{
		ObjectMeta: workload.ObjectMeta,
		Spec: appsv1.StatefulSetSpec{
			ServiceName: name, Replicas: ptr.To(int32(2)), Selector: workload.Spec.Selector,
			PodManagementPolicy: appsv1.ParallelPodManagement, Template: template,
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "data", Labels: workload.Labels},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					StorageClassName: ptr.To(env.Config.DiskStorageClass),
					Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("1Gi"),
					}},
				},
			}},
		},
	}
}

func diskPods(ctx context.Context, stateful *appsv1.StatefulSet) (map[string]diskPodState, error) {
	var current appsv1.StatefulSet
	if err := env.K8s.Get(ctx, client.ObjectKeyFromObject(stateful), &current); err != nil {
		return nil, err
	}
	if current.Status.ObservedGeneration < current.Generation || current.Status.ReadyReplicas != 2 {
		return nil, fmt.Errorf("StatefulSet has %d Ready replicas, want two", current.Status.ReadyReplicas)
	}
	pods, err := env.WorkloadState(ctx, stateful.Namespace, stateful.Name, env.Config.MainPool, 2, 0)
	if err != nil {
		return nil, err
	}
	var attachments storagev1.VolumeAttachmentList
	if err := env.K8s.List(ctx, &attachments); err != nil {
		return nil, err
	}
	result := map[string]diskPodState{}
	diskPrefix := strings.ToLower("/subscriptions/" + env.Config.SubscriptionID + "/resourceGroups/" +
		env.Config.ResourceGroup + "/providers/Microsoft.Compute/disks/")
	for _, pod := range pods {
		claimName := "data-" + pod.Name
		var claim corev1.PersistentVolumeClaim
		if err := env.K8s.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: claimName}, &claim); err != nil {
			return nil, err
		}
		if claim.UID == "" || claim.Labels[environment.RunLabel] != env.Config.RunID ||
			claim.Status.Phase != corev1.ClaimBound || claim.Spec.VolumeName == "" ||
			claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != env.Config.DiskStorageClass {
			return nil, fmt.Errorf("StatefulSet claim %s is not bound to the run-owned disk class", claimName)
		}
		var volume corev1.PersistentVolume
		if err := env.K8s.Get(ctx, client.ObjectKey{Name: claim.Spec.VolumeName}, &volume); err != nil {
			return nil, err
		}
		if volume.UID == "" || volume.Spec.ClaimRef == nil || volume.Spec.ClaimRef.UID != claim.UID ||
			volume.Spec.CSI == nil || volume.Spec.CSI.Driver != environment.DiskCSIDriver ||
			!strings.HasPrefix(strings.ToLower(volume.Spec.CSI.VolumeHandle), diskPrefix) {
			return nil, fmt.Errorf("StatefulSet claim %s does not own an Azure Disk in the selected resource group", claimName)
		}
		attached := false
		for _, attachment := range attachments.Items {
			if attachment.Spec.Source.PersistentVolumeName != nil &&
				*attachment.Spec.Source.PersistentVolumeName == volume.Name &&
				attachment.Spec.NodeName == pod.Spec.NodeName &&
				attachment.Spec.Attacher == environment.DiskCSIDriver && attachment.Status.Attached {
				attached = true
			}
		}
		if !attached {
			return nil, fmt.Errorf("Azure Disk %s is not attached to the Ready Pod worker", volume.Name)
		}
		identity, err := env.ReadDiskIdentity(ctx, pod.Namespace, pod.Name)
		if err != nil {
			return nil, err
		}
		if identity == "" {
			return nil, fmt.Errorf("StatefulSet Pod %s has an empty disk file", pod.Name)
		}
		result[pod.Name] = diskPodState{
			PodUID: pod.UID, Node: pod.Spec.NodeName, ClaimUID: claim.UID,
			VolumeUID: volume.UID, VolumeName: volume.Name, VolumeHandle: volume.Spec.CSI.VolumeHandle,
			Identity: identity,
		}
	}
	return result, nil
}

func TestDiskStatefulSetManifest(t *testing.T) {
	observationEnvironment(t)
	env.Config.DiskStorageClass = "run-disk"
	env.Config.WorkloadImage = "busybox@sha256:abc"
	previous := namespace
	namespace = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test"}}
	t.Cleanup(func() { namespace = previous })
	stateful := diskStatefulSet("disk-data")
	if *stateful.Spec.Replicas != 2 || stateful.Spec.ServiceName != stateful.Name ||
		stateful.Spec.Template.Spec.NodeSelector[env.Config.PoolLabel] != env.Config.MainLabel ||
		len(stateful.Spec.VolumeClaimTemplates) != 1 ||
		*stateful.Spec.VolumeClaimTemplates[0].Spec.StorageClassName != env.Config.DiskStorageClass ||
		stateful.Spec.VolumeClaimTemplates[0].Spec.AccessModes[0] != corev1.ReadWriteOnce ||
		stateful.Spec.Template.Spec.Containers[0].ReadinessProbe == nil ||
		stateful.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath != "/data" {
		t.Fatal("StatefulSet does not mount two run-owned Azure Disk claims on the main pool")
	}
}
