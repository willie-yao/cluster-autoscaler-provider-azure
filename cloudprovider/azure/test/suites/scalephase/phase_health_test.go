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

package scalephase_test

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

// controllerWitness identifies one autoscaler Pod and its container restarts.
type controllerWitness struct {
	UID      types.UID
	Restarts int32
}

// readControllerWitness checks the controller, then returns the UID of its one
// Pod and the restart count of the autoscaler container. With afterMissing,
// it checks the controller as ControllerAfterMissing does.
func readControllerWitness(ctx context.Context, e *environment.Environment, afterMissing bool) (controllerWitness, error) {
	if afterMissing {
		if err := e.ControllerAfterMissing(ctx); err != nil {
			return controllerWitness{}, err
		}
	} else if err := e.Controller(ctx); err != nil {
		return controllerWitness{}, err
	}
	var deployment appsv1.Deployment
	if err := e.K8s.Get(ctx, client.ObjectKey{Namespace: e.Config.AutoscalerNamespace, Name: e.Config.AutoscalerDeployment},
		&deployment); err != nil {
		return controllerWitness{}, fmt.Errorf("get autoscaler Deployment: %w", err)
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return controllerWitness{}, fmt.Errorf("parse autoscaler selector: %w", err)
	}
	var pods corev1.PodList
	if err := e.K8s.List(ctx, &pods, client.InNamespace(e.Config.AutoscalerNamespace),
		client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return controllerWitness{}, fmt.Errorf("list autoscaler Pods: %w", err)
	}
	if len(pods.Items) != 1 || pods.Items[0].UID == "" {
		return controllerWitness{}, fmt.Errorf("expected one identified autoscaler Pod, got %d", len(pods.Items))
	}
	for _, status := range pods.Items[0].Status.ContainerStatuses {
		if status.Name == e.Config.AutoscalerContainer {
			return controllerWitness{UID: pods.Items[0].UID, Restarts: status.RestartCount}, nil
		}
	}
	return controllerWitness{}, fmt.Errorf("autoscaler container status is missing")
}

// sameController returns an error unless the controller still has the Pod
// and restart count in before.
func sameController(ctx context.Context, e *environment.Environment, before controllerWitness, afterMissing bool) error {
	current, err := readControllerWitness(ctx, e, afterMissing)
	if err != nil {
		return err
	}
	if current != before {
		return fmt.Errorf("autoscaler Pod changed or its container restarted, before %+v, now %+v", before, current)
	}
	return nil
}

// noControllerPanic returns an error if the autoscaler log since the given
// time holds a Go panic or fatal error.
func noControllerPanic(ctx context.Context, e *environment.Environment, since time.Time) error {
	logs, err := e.ReadControllerLogsSince(ctx, since)
	if err != nil {
		return err
	}
	if strings.Contains(logs, "panic:") || strings.Contains(logs, "fatal error:") {
		return fmt.Errorf("autoscaler logged a panic or fatal runtime error")
	}
	return nil
}

// createPhaseSignal creates a run-labeled ConfigMap in the test namespace that
// asks the operator's fixture script to act.
func createPhaseSignal(ctx context.Context, f *fixture, name string, data map[string]string) error {
	signal := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: f.namespace.Name,
		Labels: map[string]string{environment.RunLabel: f.env.Config.RunID},
	}, Data: data}
	return f.env.K8s.Create(ctx, signal)
}

// workloadPodsGone returns an error while Pods of work remain.
func workloadPodsGone(ctx context.Context, f *fixture, work *appsv1.Deployment) error {
	var pods corev1.PodList
	if err := f.env.K8s.List(ctx, &pods, client.InNamespace(f.namespace.Name),
		client.MatchingLabels(work.Spec.Selector.MatchLabels)); err != nil {
		return fmt.Errorf("list workload %s Pods: %w", work.Name, err)
	}
	if len(pods.Items) != 0 {
		return fmt.Errorf("waiting for %d Pods of workload %s to be removed", len(pods.Items), work.Name)
	}
	return nil
}
