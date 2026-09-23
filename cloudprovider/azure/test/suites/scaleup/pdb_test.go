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
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

const pdbDiagnosticTimeout = 45 * time.Second

type pdbDiagnostics struct {
	Stage  string
	Budget *pdbBudgetDiagnostics
	Pods   []pdbPodDiagnostics
	Errors []string
}

type pdbBudgetDiagnostics struct {
	Name                             string
	UID                              types.UID
	Generation, ObservedGeneration   int64
	MinAvailable, MaxUnavailable     string
	Selector                         map[string]string
	CurrentHealthy, DesiredHealthy   int32
	ExpectedPods, DisruptionsAllowed int32
	DisruptedPods                    int
}

type pdbPodDiagnostics struct {
	Name                       string
	UID                        types.UID
	NodeName                   string
	Phase                      corev1.PodPhase
	Ready                      bool
	DeletionTimestamp          *metav1.Time
	DeletionGracePeriodSeconds *int64
}

func newProtectedPDBFixture(e *environment.Environment, namespace string) (*appsv1.Deployment, *policyv1.PodDisruptionBudget) {
	protected := e.Deployment(namespace, "protected", e.Config.MainLabel, "50m", 2)
	nodeTaintsPolicy := corev1.NodeInclusionPolicyHonor
	protected.Spec.Template.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
		MaxSkew: 1, TopologyKey: "kubernetes.io/hostname", WhenUnsatisfiable: corev1.DoNotSchedule,
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

func reportPDBDiagnostics(ctx context.Context, stage string, expected *policyv1.PodDisruptionBudget, podLabels map[string]string) {
	ctx, cancel := context.WithTimeout(ctx, pdbDiagnosticTimeout)
	defer cancel()
	report := pdbDiagnostics{Stage: stage}
	switch {
	case env == nil:
		report.Errors = append(report.Errors, "environment is unavailable")
	case env.K8s == nil:
		report.Errors = append(report.Errors, "Kubernetes client is unavailable")
	default:
		if err := env.Authorize(ctx); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("authorization failed: %v", err))
		} else {
			report = readPDBDiagnostics(ctx, env.K8s, stage, env.Config.RunID, expected, podLabels)
		}
	}
	AddReportEntry("pdb-"+stage, report)
}

func readPDBDiagnostics(ctx context.Context, k8s client.Client, stage, runID string, expected *policyv1.PodDisruptionBudget, podLabels map[string]string) pdbDiagnostics {
	report := pdbDiagnostics{Stage: stage}
	if k8s == nil {
		report.Errors = append(report.Errors, "Kubernetes client is unavailable")
		return report
	}
	if expected == nil || expected.Namespace == "" || expected.Name == "" {
		report.Errors = append(report.Errors, "expected PDB identity is unavailable")
		return report
	}
	if expected.UID == "" {
		report.Errors = append(report.Errors, "expected PDB UID is unavailable")
	}
	if runID == "" || podLabels[environment.RunLabel] != runID {
		report.Errors = append(report.Errors, "protected Pod ownership labels are invalid")
		return report
	}

	var current policyv1.PodDisruptionBudget
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(expected), &current); err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("read PDB: %v", err))
	} else if expected.UID == "" || current.UID != expected.UID || current.Labels[environment.RunLabel] != runID {
		report.Errors = append(report.Errors, "PDB ownership does not match the created fixture")
	} else {
		report.Budget = projectPDBDiagnostics(current)
	}

	var pods corev1.PodList
	if err := k8s.List(ctx, &pods, client.InNamespace(expected.Namespace), client.MatchingLabels(podLabels)); err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("list protected Pods: %v", err))
		return report
	}
	for _, pod := range pods.Items {
		if pod.Namespace != expected.Namespace || pod.UID == "" || pod.Labels[environment.RunLabel] != runID {
			report.Errors = append(report.Errors, fmt.Sprintf("skip Pod %q with incomplete ownership", pod.Name))
			continue
		}
		report.Pods = append(report.Pods, projectPDBPodDiagnostics(pod))
	}
	sort.Slice(report.Pods, func(i, j int) bool {
		return report.Pods[i].Name < report.Pods[j].Name
	})
	return report
}

func projectPDBDiagnostics(pdb policyv1.PodDisruptionBudget) *pdbBudgetDiagnostics {
	selector := map[string]string{}
	if pdb.Spec.Selector != nil {
		for key, value := range pdb.Spec.Selector.MatchLabels {
			selector[key] = value
		}
	}
	return &pdbBudgetDiagnostics{
		Name: pdb.Name, UID: pdb.UID, Generation: pdb.Generation, ObservedGeneration: pdb.Status.ObservedGeneration,
		MinAvailable: intOrString(pdb.Spec.MinAvailable), MaxUnavailable: intOrString(pdb.Spec.MaxUnavailable),
		Selector: selector, CurrentHealthy: pdb.Status.CurrentHealthy, DesiredHealthy: pdb.Status.DesiredHealthy,
		ExpectedPods: pdb.Status.ExpectedPods, DisruptionsAllowed: pdb.Status.DisruptionsAllowed,
		DisruptedPods: len(pdb.Status.DisruptedPods),
	}
}

func projectPDBPodDiagnostics(pod corev1.Pod) pdbPodDiagnostics {
	var deletionTimestamp *metav1.Time
	if pod.DeletionTimestamp != nil {
		deletionTimestamp = pod.DeletionTimestamp.DeepCopy()
	}
	var deletionGracePeriodSeconds *int64
	if pod.DeletionGracePeriodSeconds != nil {
		deletionGracePeriodSeconds = ptr.To(*pod.DeletionGracePeriodSeconds)
	}
	return pdbPodDiagnostics{
		Name: pod.Name, UID: pod.UID, NodeName: pod.Spec.NodeName, Phase: pod.Status.Phase,
		Ready: environment.PodReady(pod), DeletionTimestamp: deletionTimestamp,
		DeletionGracePeriodSeconds: deletionGracePeriodSeconds,
	}
}

func intOrString(value *intstr.IntOrString) string {
	if value == nil {
		return ""
	}
	return value.String()
}

func TestPDBFixtureDiagnostics(t *testing.T) {
	t.Run("fixture honors drain taint and preserves hard spread", func(t *testing.T) {
		e := &environment.Environment{Config: environment.Config{RunID: "run", MainLabel: "main", PoolLabel: "pool"}}
		protected, budget := newProtectedPDBFixture(e, "fixture")
		if len(protected.Spec.Template.Spec.TopologySpreadConstraints) != 1 {
			t.Fatalf("topology constraints=%d, want 1", len(protected.Spec.Template.Spec.TopologySpreadConstraints))
		}
		constraint := protected.Spec.Template.Spec.TopologySpreadConstraints[0]
		if constraint.MaxSkew != 1 || constraint.TopologyKey != "kubernetes.io/hostname" ||
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
		if budget.Labels[environment.RunLabel] != "run" ||
			budget.Spec.Selector.MatchLabels[environment.RunLabel] != "run" ||
			constraint.LabelSelector.MatchLabels[environment.RunLabel] != "run" {
			t.Fatal("fixture ownership selector was not preserved")
		}
	})

	t.Run("projects bounded status and owned Pod placement", func(t *testing.T) {
		now := metav1.NewTime(time.Date(2026, 9, 22, 0, 5, 0, 0, time.UTC))
		grace := int64(30)
		labels := map[string]string{environment.RunLabel: "run", "app": "protected"}
		budget := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: "protected", Namespace: "fixture", UID: "pdb-uid", Generation: 4, Labels: labels},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MinAvailable: ptr.To(intstr.FromInt32(1)),
				Selector:     &metav1.LabelSelector{MatchLabels: labels},
			},
			Status: policyv1.PodDisruptionBudgetStatus{
				ObservedGeneration: 4, CurrentHealthy: 2, DesiredHealthy: 1,
				ExpectedPods: 2, DisruptionsAllowed: 1,
			},
		}
		pods := []client.Object{
			&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "protected-b", Namespace: "fixture", UID: "pod-b", Labels: labels},
				Spec:       corev1.PodSpec{NodeName: "worker-b"},
				Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{
					Type: corev1.PodReady, Status: corev1.ConditionTrue,
				}}},
			},
			&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "protected-a", Namespace: "fixture", UID: "pod-a", Labels: labels,
					DeletionTimestamp: &now, DeletionGracePeriodSeconds: &grace, Finalizers: []string{"example.test/finalizer"},
				},
				Spec:   corev1.PodSpec{NodeName: "worker-a"},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			},
		}
		k8s := fake.NewClientBuilder().WithObjects(append([]client.Object{budget}, pods...)...).Build()
		report := readPDBDiagnostics(context.Background(), k8s, "relaxed", "run", budget, labels)
		if len(report.Errors) != 0 || report.Budget == nil {
			t.Fatalf("report errors=%v budget=%v", report.Errors, report.Budget)
		}
		if report.Budget.Generation != 4 || report.Budget.ObservedGeneration != 4 ||
			report.Budget.MinAvailable != "1" || report.Budget.DisruptionsAllowed != 1 {
			t.Fatalf("unexpected PDB projection: %#v", report.Budget)
		}
		if len(report.Pods) != 2 || report.Pods[0].Name != "protected-a" ||
			report.Pods[0].DeletionTimestamp == nil || report.Pods[0].DeletionGracePeriodSeconds == nil ||
			report.Pods[1].Name != "protected-b" || !report.Pods[1].Ready {
			t.Fatalf("unexpected Pod projection: %#v", report.Pods)
		}
	})

	t.Run("surfaces diagnostic read failure without fabricating evidence", func(t *testing.T) {
		labels := map[string]string{environment.RunLabel: "run", "app": "protected"}
		budget := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: "protected", Namespace: "fixture", UID: "pdb-uid", Labels: labels},
		}
		k8s := fake.NewClientBuilder().WithObjects(budget).WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return fmt.Errorf("synthetic list failure")
			},
		}).Build()
		report := readPDBDiagnostics(context.Background(), k8s, "final", "run", budget, labels)
		if report.Budget == nil || len(report.Pods) != 0 || len(report.Errors) != 1 ||
			!strings.Contains(report.Errors[0], "synthetic list failure") {
			t.Fatalf("unexpected failed-read report: %#v", report)
		}
	})

	t.Run("rejects a replaced PDB", func(t *testing.T) {
		labels := map[string]string{environment.RunLabel: "run", "app": "protected"}
		expected := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: "protected", Namespace: "fixture", UID: "expected", Labels: labels},
		}
		current := expected.DeepCopy()
		current.UID = "replacement"
		k8s := fake.NewClientBuilder().WithObjects(current).Build()
		report := readPDBDiagnostics(context.Background(), k8s, "final", "run", expected, labels)
		if report.Budget != nil || len(report.Errors) != 1 ||
			!strings.Contains(report.Errors[0], "ownership") {
			t.Fatalf("unexpected replacement report: %#v", report)
		}
	})
}
