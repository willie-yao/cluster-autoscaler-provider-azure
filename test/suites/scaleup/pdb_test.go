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
	"sort"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/cluster-autoscaler-provider-azure/test/pkg/environment"
)

const pdbDiagnosticTimeout = 45 * time.Second

// newProtectedPDBFixture returns a two-replica main Deployment spread across
// workers and a PDB that requires both replicas to stay available.
func newProtectedPDBFixture(e *environment.Environment, namespace string) (*appsv1.Deployment, *policyv1.PodDisruptionBudget) {
	protected := e.Deployment(namespace, "protected", e.Config.MainLabel, "50m", 2)
	nodeTaintsPolicy := corev1.NodeInclusionPolicyHonor
	protected.Spec.Template.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
		MaxSkew: 1, TopologyKey: corev1.LabelHostname, WhenUnsatisfiable: corev1.DoNotSchedule,
		LabelSelector: protected.Spec.Selector.DeepCopy(), NodeTaintsPolicy: &nodeTaintsPolicy,
	}}
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: protected.Name, Namespace: namespace, Labels: protected.Labels},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: ptr.To(intstr.FromInt32(2)),
			Selector:     protected.Spec.Selector.DeepCopy(),
		},
	}
	return protected, pdb
}

// reportPDBDiagnostics adds the PDB status and the placement of its Pods to
// the report. It records read errors instead of failing the spec.
func reportPDBDiagnostics(ctx context.Context, stage string, pdb *policyv1.PodDisruptionBudget, podLabels map[string]string) {
	ctx, cancel := context.WithTimeout(ctx, pdbDiagnosticTimeout)
	defer cancel()
	var report struct {
		Generation int64
		Status     policyv1.PodDisruptionBudgetStatus
		Pods       []string
		Errors     []string
	}
	var current policyv1.PodDisruptionBudget
	if err := env.K8s.Get(ctx, client.ObjectKeyFromObject(pdb), &current); err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("read PDB: %v", err))
	} else {
		report.Generation, report.Status = current.Generation, current.Status
	}
	var pods corev1.PodList
	if err := env.K8s.List(ctx, &pods, client.InNamespace(pdb.Namespace), client.MatchingLabels(podLabels)); err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("list protected Pods: %v", err))
	}
	for _, pod := range pods.Items {
		report.Pods = append(report.Pods, fmt.Sprintf("%s node=%s phase=%s ready=%t deleting=%t",
			pod.Name, pod.Spec.NodeName, pod.Status.Phase, environment.PodReady(pod), pod.DeletionTimestamp != nil))
	}
	sort.Strings(report.Pods)
	AddReportEntry("pdb-"+stage, report)
}

func intOrString(value *intstr.IntOrString) string {
	if value == nil {
		return ""
	}
	return value.String()
}

func TestPDBFixture(t *testing.T) {
	t.Run("fixture honors drain taint and preserves hard spread", func(t *testing.T) {
		e := &environment.Environment{Config: environment.Config{RunID: "run", MainLabel: "main", PoolLabel: "pool"}}
		protected, budget := newProtectedPDBFixture(e, "fixture")
		if len(protected.Spec.Template.Spec.TopologySpreadConstraints) != 1 {
			t.Fatalf("topology constraints=%d, want 1", len(protected.Spec.Template.Spec.TopologySpreadConstraints))
		}
		constraint := protected.Spec.Template.Spec.TopologySpreadConstraints[0]
		if constraint.MaxSkew != 1 || constraint.TopologyKey != corev1.LabelHostname ||
			constraint.WhenUnsatisfiable != corev1.DoNotSchedule {
			t.Fatalf("unexpected hard topology spread constraint: %#v", constraint)
		}
		if constraint.NodeTaintsPolicy == nil || *constraint.NodeTaintsPolicy != corev1.NodeInclusionPolicyHonor {
			t.Fatalf("nodeTaintsPolicy=%v, want Honor", constraint.NodeTaintsPolicy)
		}
		if protected.Spec.Replicas == nil || *protected.Spec.Replicas != 2 ||
			budget.Spec.MinAvailable == nil || budget.Spec.MinAvailable.IntVal != 2 {
			t.Fatalf("replicas=%v minAvailable=%v, want 2 and 2", protected.Spec.Replicas, budget.Spec.MinAvailable)
		}
	})

	t.Run("acknowledges the processed relaxed budget through eviction", func(t *testing.T) {
		const blockedGeneration = int64(3)
		for _, tt := range []struct {
			name   string
			change func(*policyv1.PodDisruptionBudget)
			valid  bool
		}{
			{name: "relaxed before eviction", valid: true},
			{name: "eviction decremented allowance before first sample", valid: true, change: func(pdb *policyv1.PodDisruptionBudget) {
				pdb.Status.CurrentHealthy = 2
				pdb.Status.DisruptionsAllowed = 0
			}},
			{name: "one healthy replacement in flight before first sample", valid: true, change: func(pdb *policyv1.PodDisruptionBudget) {
				pdb.Status.CurrentHealthy = 1
				pdb.Status.DisruptionsAllowed = 0
			}},
			{name: "replaced UID", change: func(pdb *policyv1.PodDisruptionBudget) { pdb.UID = "replacement" }},
			{name: "unrelaxed generation", change: func(pdb *policyv1.PodDisruptionBudget) { pdb.Generation = blockedGeneration }},
			{name: "unprocessed generation", change: func(pdb *policyv1.PodDisruptionBudget) { pdb.Status.ObservedGeneration = blockedGeneration }},
			{name: "unrelaxed minimum", change: func(pdb *policyv1.PodDisruptionBudget) {
				pdb.Spec.MinAvailable = ptr.To(intstr.FromInt32(2))
			}},
			{name: "unprocessed desired health", change: func(pdb *policyv1.PodDisruptionBudget) { pdb.Status.DesiredHealthy = 2 }},
			{name: "no healthy Pod", change: func(pdb *policyv1.PodDisruptionBudget) { pdb.Status.CurrentHealthy = 0 }},
			{name: "excess allowance", change: func(pdb *policyv1.PodDisruptionBudget) { pdb.Status.DisruptionsAllowed = 2 }},
		} {
			t.Run(tt.name, func(t *testing.T) {
				pdb := policyv1.PodDisruptionBudget{
					ObjectMeta: metav1.ObjectMeta{UID: "expected", Generation: blockedGeneration + 1},
					Spec:       policyv1.PodDisruptionBudgetSpec{MinAvailable: ptr.To(intstr.FromInt32(1))},
					Status: policyv1.PodDisruptionBudgetStatus{
						ObservedGeneration: blockedGeneration + 1, CurrentHealthy: 2,
						DesiredHealthy: 1, DisruptionsAllowed: 1,
					},
				}
				if tt.change != nil {
					tt.change(&pdb)
				}
				if err := relaxedPDBProcessed(pdb, "expected", blockedGeneration); (err == nil) != tt.valid {
					t.Fatalf("relaxedPDBProcessed() error=%v, want valid=%t; budget=%#v", err, tt.valid, pdb)
				}
			})
		}
	})
}
