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
	"errors"
	"flag"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider/azure/test/pkg/environment"
)

const (
	pollInterval = 10 * time.Second
	waitTimeout  = 15 * time.Minute
)

var configPath string

func init() {
	flag.StringVar(&configPath, "environment", "", "explicit non-secret JSON environment binding (required)")
}

func TestScalePhase(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Azure operator-prepared phased E2E suite")
}

type fixture struct {
	env       *environment.Environment
	namespace *corev1.Namespace
}

func setup(ctx SpecContext, phase, caseID string) *fixture {
	Expect(configPath).NotTo(BeEmpty(), "-environment is required")
	cfg, err := environment.LoadConfig(configPath)
	Expect(err).NotTo(HaveOccurred())
	if cfg.Phase != phase {
		Skip(fmt.Sprintf("%s requires the %s fixture phase", caseID, phase))
	}
	Expect(GinkgoParallelProcess()).To(Equal(1))
	conf, _ := GinkgoConfiguration()
	Expect(conf.ParallelTotal).To(Equal(1))
	e, err := environment.NewEnvironment(ctx, cfg)
	Expect(err).NotTo(HaveOccurred())
	Expect(e.CheckPhaseMarker(ctx, phase, caseID)).To(Succeed())
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		GenerateName: "azure-e2e-", Labels: map[string]string{environment.RunLabel: cfg.RunID},
	}}
	Expect(e.K8s.Create(ctx, ns)).To(Succeed())
	owned := ns.DeepCopy()
	DeferCleanup(func(ctx SpecContext) {
		Expect(e.Authorize(ctx)).To(Succeed())
		var current corev1.Namespace
		err := e.K8s.Get(ctx, client.ObjectKeyFromObject(owned), &current)
		if !apierrors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred())
			Expect(current.UID).To(Equal(owned.UID))
			Expect(current.Labels[environment.RunLabel]).To(Equal(cfg.RunID))
			Expect(e.K8s.Delete(ctx, &current, client.Preconditions{UID: &owned.UID})).To(Succeed())
		}
		Eventually(ctx, func() error {
			err := e.K8s.Get(ctx, client.ObjectKeyFromObject(owned), &corev1.Namespace{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			return fmt.Errorf("test namespace is still terminating")
		}, 4*time.Minute, pollInterval).Should(Succeed())
	}, NodeTimeout(6*time.Minute))
	AddReportEntry("runtime-image", cfg.ExpectedImage)
	return &fixture{env: e, namespace: ns}
}

func (f *fixture) read(ctx context.Context) (environment.Snapshot, error) {
	snapshot, err := f.env.Read(ctx)
	if errors.Is(err, environment.ErrBounds) {
		StopTrying("observed resource bounds breach").Wrap(err).Now()
	}
	return snapshot, err
}

func (f *fixture) active(ctx context.Context) (environment.Snapshot, error) {
	if err := f.env.Controller(ctx); err != nil {
		return environment.Snapshot{}, err
	}
	return f.read(ctx)
}

func (f *fixture) stable(ctx SpecContext, counts map[string]int) environment.Snapshot {
	var snapshot environment.Snapshot
	Eventually(ctx, func() error {
		var err error
		snapshot, err = f.read(ctx)
		if err != nil {
			return err
		}
		return snapshot.StablePools(f.env.Config, counts)
	}, waitTimeout, pollInterval).Should(Succeed())
	AddReportEntry("pool-capacities", counts)
	return snapshot
}

func (f *fixture) signalStart(ctx context.Context) {
	signal := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "start-controller", Namespace: f.namespace.Name,
		Labels: map[string]string{environment.RunLabel: f.env.Config.RunID},
	}}
	Expect(f.env.K8s.Create(ctx, signal)).To(Succeed())
	Eventually(ctx, f.env.Controller, 5*time.Minute, pollInterval).Should(Succeed())
}

var _ = Describe("Phased Azure VMSS cases", Serial, func() {
	It("AZ-P1-007 splits one two-node scale-up across similar pools", Label("AZ-P1-007", "balance"), func(ctx SpecContext) {
		f := setup(ctx, "balance", "AZ-P1-007")
		c := f.env.Config
		Expect(f.env.CheckPausedController(ctx)).To(Succeed())
		initial := f.stable(ctx, map[string]int{c.MainPool: 1, c.ZeroPool: 0, c.BalancePoolA: 0, c.BalancePoolB: 0})
		Expect(environment.CheckBalancePools(initial, c)).To(Succeed())
		Expect(f.env.CheckWorkerIsolation(ctx, initial, f.namespace.Name)).To(Succeed())

		work := f.env.Deployment(f.namespace.Name, "balance-demand", c.BalanceLabel, c.DemandCPU, 2)
		work.Spec.Template.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				TopologyKey: "kubernetes.io/hostname", LabelSelector: work.Spec.Selector.DeepCopy(),
			}},
		}}
		Expect(f.env.K8s.Create(ctx, work)).To(Succeed())
		Eventually(ctx, func() error {
			return pendingBalancePods(ctx, f, work)
		}, 3*time.Minute, pollInterval).Should(Succeed())
		Expect(f.env.CheckPausedController(ctx)).To(Succeed())
		started := time.Now().Add(-time.Second)
		f.signalStart(ctx)

		var grown environment.Snapshot
		Eventually(ctx, func() error {
			var err error
			grown, err = f.active(ctx)
			if err != nil {
				return err
			}
			for _, pool := range []string{c.BalancePoolA, c.BalancePoolB} {
				if grown.Pools[pool].Capacity > 1 || len(grown.Pools[pool].Instances) > 1 {
					StopTrying("one balance pool grew by more than one").Now()
				}
			}
			if err := grown.StablePools(c, map[string]int{
				c.MainPool: 1, c.ZeroPool: 0, c.BalancePoolA: 1, c.BalancePoolB: 1,
			}); err != nil {
				return err
			}
			return readyBalancePods(ctx, f, work, grown)
		}, waitTimeout, pollInterval).Should(Succeed())
		Eventually(ctx, func() error {
			logs, err := f.env.ReadControllerLogsSince(ctx, started)
			if err != nil {
				return err
			}
			return checkBalancedPlan(logs, c.BalancePoolA, c.BalancePoolB)
		}, 2*time.Minute, pollInterval).Should(Succeed())
		AddReportEntry("balance-plan", "one plan added one node to each pool")

		Expect(f.env.K8s.Delete(ctx, work)).To(Succeed())
		Eventually(ctx, func() error {
			after, err := f.read(ctx)
			if err != nil {
				return err
			}
			if err := after.StablePools(c, map[string]int{
				c.MainPool: 1, c.ZeroPool: 0, c.BalancePoolA: 0, c.BalancePoolB: 0,
			}); err != nil {
				return err
			}
			for _, pool := range []string{c.BalancePoolA, c.BalancePoolB} {
				if err := f.env.Deleted(ctx, grown, after, pool, 1); err != nil {
					return err
				}
			}
			return nil
		}, waitTimeout, pollInterval).Should(Succeed())
		AddReportEntry("physical-delete", "both balance pool VMs, Nodes and NICs were removed")
	}, NodeTimeout(55*time.Minute))

	It("AZ-P1-008 deletes a zero-pool VM that never registers", Label("AZ-P1-008", "no-join", "delete"), func(ctx SpecContext) {
		f := setup(ctx, "no-join", "AZ-P1-008")
		c := f.env.Config
		Expect(f.env.Controller(ctx)).To(Succeed())
		f.stable(ctx, map[string]int{c.MainPool: 1, c.ZeroPool: 0})
		demand := f.env.Deployment(f.namespace.Name, "unregistered-demand", c.ZeroLabel, "50m", 1)
		Expect(f.env.K8s.Create(ctx, demand)).To(Succeed())
		Eventually(ctx, func() error {
			_, err := f.env.WorkloadState(ctx, f.namespace.Name, demand.Name, c.ZeroPool, 0, 1)
			return err
		}, 3*time.Minute, pollInterval).Should(Succeed())

		var created environment.Snapshot
		Eventually(ctx, func() error {
			var err error
			created, err = f.active(ctx)
			if err != nil {
				return err
			}
			if len(environment.PoolNodes(created.Nodes, c.PoolID(c.ZeroPool))) != 0 {
				StopTrying("unregistered VM became a Kubernetes Node").Now()
			}
			pool := created.Pools[c.ZeroPool]
			if pool.Capacity != 1 || len(pool.Instances) != 1 {
				return fmt.Errorf("waiting for one unregistered zero-pool VM")
			}
			return nil
		}, waitTimeout, pollInterval).Should(Succeed())
		AddReportEntry("unregistered-vm", created.Pools[c.ZeroPool])

		var instanceID string
		for id := range created.Pools[c.ZeroPool].Instances {
			instanceID = id
		}
		running := false
		Eventually(ctx, func() error {
			current, err := f.active(ctx)
			if err != nil {
				return err
			}
			if len(environment.PoolNodes(current.Nodes, c.PoolID(c.ZeroPool))) != 0 {
				StopTrying("unregistered VM became a Kubernetes Node").Now()
			}
			backoffErr := f.env.CheckTimeoutBackoff(ctx, c.ZeroPool)
			if _, exists := current.Pools[c.ZeroPool].Instances[instanceID]; exists &&
				current.Pools[c.ZeroPool].Capacity == 1 && backoffErr != nil {
				nowRunning, err := f.env.InstanceRunning(ctx, c.ZeroPool, instanceID)
				if err != nil {
					return err
				}
				if running && !nowRunning {
					StopTrying("unregistered VM stopped before timeout backoff").Now()
				}
				running = nowRunning
			}
			if !running {
				return fmt.Errorf("waiting for the unregistered VM to reach Running")
			}
			return backoffErr
		}, 12*time.Minute, pollInterval).Should(Succeed())
		AddReportEntry("unregistered-power", "captured VM reached Running before the provision timeout")
		Eventually(ctx, func() error {
			var events corev1.EventList
			if err := f.env.K8s.List(ctx, &events, client.InNamespace(c.AutoscalerNamespace)); err != nil {
				return err
			}
			for _, event := range events.Items {
				if event.Reason == "ScaleUpTimedOut" && strings.Contains(event.Message, c.ZeroPool) {
					return nil
				}
			}
			return fmt.Errorf("no timeout event for the unregistered pool")
		}, time.Minute, pollInterval).Should(Succeed())

		Eventually(ctx, func() error {
			after, err := f.active(ctx)
			if err != nil {
				return err
			}
			if err := after.StablePools(c, map[string]int{c.MainPool: 1, c.ZeroPool: 0}); err != nil {
				return err
			}
			if err := f.env.Deleted(ctx, created, after, c.ZeroPool, 1); err != nil {
				return err
			}
			_, err = f.env.WorkloadState(ctx, f.namespace.Name, demand.Name, c.ZeroPool, 0, 1)
			return err
		}, waitTimeout, pollInterval).Should(Succeed())
		AddReportEntry("physical-delete", "timed-out VM, Node and NIC were removed; the group was in timeout backoff")
	}, NodeTimeout(50*time.Minute))

	It("AZ-P1-009 grows a pool from one worker to its configured minimum of two", Label("AZ-P1-009", "minimum"), func(ctx SpecContext) {
		f := setup(ctx, "minimum", "AZ-P1-009")
		c := f.env.Config
		Expect(f.env.CheckPausedController(ctx)).To(Succeed())
		before := f.stable(ctx, map[string]int{c.MainPool: 1, c.ZeroPool: 0})
		Expect(f.env.CheckWorkerIsolation(ctx, before, f.namespace.Name)).To(Succeed())
		Expect(f.env.CheckNoPendingDemand(ctx)).To(Succeed())
		AddReportEntry("below-minimum", "main tag minimum was two while Azure capacity and Node count were one")
		started := time.Now().Add(-time.Second)
		f.signalStart(ctx)
		var grown environment.Snapshot
		Eventually(ctx, func() error {
			var err error
			grown, err = f.active(ctx)
			if err != nil {
				return err
			}
			if err := f.env.CheckNoPendingDemand(ctx); err != nil {
				StopTrying("unscheduled demand invalidates the minimum-size case").Wrap(err).Now()
			}
			if err := grown.StablePools(c, map[string]int{c.MainPool: 2, c.ZeroPool: 0}); err != nil {
				return err
			}
			for id := range before.Pools[c.MainPool].Instances {
				if _, ok := grown.Pools[c.MainPool].Instances[id]; !ok {
					return fmt.Errorf("original main worker was replaced instead of growing to the minimum")
				}
			}
			return nil
		}, waitTimeout, pollInterval).Should(Succeed())
		Eventually(ctx, func() error {
			logs, err := f.env.ReadControllerLogsSince(ctx, started)
			if err != nil {
				return err
			}
			return checkMinimumPlan(logs, c.MainPool)
		}, 2*time.Minute, pollInterval).Should(Succeed())
		Consistently(ctx, func() error {
			current, err := f.active(ctx)
			if err != nil {
				return err
			}
			if err := f.env.CheckNoPendingDemand(ctx); err != nil {
				return err
			}
			return current.StablePools(c, map[string]int{c.MainPool: 2, c.ZeroPool: 0})
		}, 2*time.Minute, pollInterval).Should(Succeed())
		AddReportEntry("minimum", "the minimum-size plan grew main from one to two without unscheduled demand")
	}, NodeTimeout(35*time.Minute))
})

func pendingBalancePods(ctx context.Context, f *fixture, work *appsv1.Deployment) error {
	var pods corev1.PodList
	if err := f.env.K8s.List(ctx, &pods, client.InNamespace(work.Namespace), client.MatchingLabels(work.Spec.Selector.MatchLabels)); err != nil {
		return err
	}
	if len(pods.Items) != 2 {
		return fmt.Errorf("waiting for two balance demand Pods")
	}
	for _, pod := range pods.Items {
		if pod.Spec.NodeName != "" || pod.Status.Phase != corev1.PodPending {
			return fmt.Errorf("balance demand scheduled before the two-node plan")
		}
		scheduled := false
		for _, condition := range pod.Status.Conditions {
			scheduled = scheduled || condition.Type == corev1.PodScheduled &&
				condition.Status == corev1.ConditionFalse && condition.Reason == corev1.PodReasonUnschedulable
		}
		if !scheduled {
			return fmt.Errorf("waiting for scheduler rejection of both balance Pods")
		}
	}
	return nil
}

func readyBalancePods(ctx context.Context, f *fixture, work *appsv1.Deployment, snapshot environment.Snapshot) error {
	var pods corev1.PodList
	if err := f.env.K8s.List(ctx, &pods, client.InNamespace(work.Namespace), client.MatchingLabels(work.Spec.Selector.MatchLabels)); err != nil {
		return err
	}
	if len(pods.Items) != 2 {
		return fmt.Errorf("waiting for two balance Pods")
	}
	pools := map[string]bool{}
	for _, pod := range pods.Items {
		if !environment.PodReady(pod) {
			return fmt.Errorf("balance Pod is not Ready")
		}
		for _, pool := range []string{f.env.Config.BalancePoolA, f.env.Config.BalancePoolB} {
			for _, node := range environment.PoolNodes(snapshot.Nodes, f.env.Config.PoolID(pool)) {
				if pod.Spec.NodeName == node.Name {
					pools[pool] = true
				}
			}
		}
	}
	if len(pools) != 2 {
		return fmt.Errorf("balance Pods must run on both new pool Nodes")
	}
	return nil
}

func checkBalancedPlan(logs, a, b string) error {
	plan := regexp.MustCompile(`^\[\{([^{}]+) 0->1 \(max: 2\)\} \{([^{}]+) 0->1 \(max: 2\)\}\]$`)
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, "Final scale-up plan") {
			continue
		}
		_, raw, found := strings.Cut(line, `scaleUpInfos="`)
		if !found {
			continue
		}
		raw, _, found = strings.Cut(raw, `"`)
		if !found {
			continue
		}
		groups := plan.FindStringSubmatch(raw)
		if groups != nil && ((groups[1] == a && groups[2] == b) || (groups[1] == b && groups[2] == a)) {
			return nil
		}
	}
	return fmt.Errorf("no single autoscaler plan split two nodes between the balance pools")
}

func checkMinimumPlan(logs, pool string) error {
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, "ScaleUpToNodeGroupMinSize: final scale-up plan") {
			continue
		}
		_, raw, found := strings.Cut(line, `scaleUpInfos="`)
		if !found {
			continue
		}
		raw, _, found = strings.Cut(raw, `"`)
		if found && raw == fmt.Sprintf("[{%s 1->2 (max: 2)}]", pool) {
			return nil
		}
	}
	return fmt.Errorf("no minimum-size plan grew main from one to two")
}

func TestBalancedPlan(t *testing.T) {
	for _, tt := range []struct {
		name, logs string
		valid      bool
	}{
		{name: "single split", logs: `Final scale-up plan scaleUpInfos="[{pool-a 0->1 (max: 2)} {pool-b 0->1 (max: 2)}]"`, valid: true},
		{name: "opposite order", logs: `Final scale-up plan scaleUpInfos="[{pool-b 0->1 (max: 2)} {pool-a 0->1 (max: 2)}]"`, valid: true},
		{name: "two separate plans", logs: "Final scale-up plan scaleUpInfos=\"[{pool-a 0->1 (max: 2)}]\"\nFinal scale-up plan scaleUpInfos=\"[{pool-b 0->1 (max: 2)}]\""},
		{name: "unbalanced", logs: `Final scale-up plan scaleUpInfos="[{pool-a 0->2 (max: 2)}]"`},
		{name: "wrong pools", logs: `Final scale-up plan scaleUpInfos="[{foreign 0->1 (max: 2)} {pool-b 0->1 (max: 2)}]"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := checkBalancedPlan(tt.logs, "pool-a", "pool-b"); (err == nil) != tt.valid {
				t.Fatalf("checkBalancedPlan = %v, valid=%t", err, tt.valid)
			}
		})
	}
}

func TestMinimumPlan(t *testing.T) {
	for _, tt := range []struct {
		name, logs string
		valid      bool
	}{
		{name: "minimum-size growth", logs: `ScaleUpToNodeGroupMinSize: final scale-up plan scaleUpInfos="[{main 1->2 (max: 2)}]"`, valid: true},
		{name: "demand-based growth", logs: `Final scale-up plan scaleUpInfos="[{main 1->2 (max: 2)}]"`},
		{name: "wrong initial size", logs: `ScaleUpToNodeGroupMinSize: final scale-up plan scaleUpInfos="[{main 0->2 (max: 2)}]"`},
		{name: "wrong pool", logs: `ScaleUpToNodeGroupMinSize: final scale-up plan scaleUpInfos="[{other 1->2 (max: 2)}]"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := checkMinimumPlan(tt.logs, "main"); (err == nil) != tt.valid {
				t.Fatalf("checkMinimumPlan = %v, valid=%t", err, tt.valid)
			}
		})
	}
}
