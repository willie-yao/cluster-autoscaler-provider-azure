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

var (
	configPath string

	balancedPlanRE = regexp.MustCompile(`^\[\{([^{}]+) 0->1 \(max: 2\)\} \{([^{}]+) 0->1 \(max: 2\)\}\]$`)
)

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

// setup skips the spec unless the binding selects phase. Otherwise it checks
// the operator marker for caseID and creates a run-labeled test namespace
// that cleanup deletes.
func setup(ctx context.Context, phase, caseID string) *fixture {
	Expect(configPath).NotTo(BeEmpty(), "-environment is required")
	cfg, err := environment.LoadConfig(configPath)
	Expect(err).NotTo(HaveOccurred())
	if cfg.AKS() {
		Skip(fmt.Sprintf("%s needs custom VMSS from fixture.sh; AKS pools cannot provide the %s fixture", caseID, phase))
	}
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
	if phase == "deallocate" {
		ns.Labels["pod-security.kubernetes.io/enforce"] = "privileged"
	}
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

// read reads the environment and stops the enclosing poll at once when the
// resources exceed their bounds.
func (f *fixture) read(ctx context.Context) (environment.Snapshot, error) {
	snapshot, err := f.env.Read(ctx)
	if errors.Is(err, environment.ErrBounds) {
		StopTrying("observed resource bounds breach").Wrap(err).Now()
	}
	return snapshot, err
}

// active checks the running controller, then works like read.
func (f *fixture) active(ctx context.Context) (environment.Snapshot, error) {
	if err := f.env.Controller(ctx); err != nil {
		return environment.Snapshot{}, err
	}
	return f.read(ctx)
}

// stable waits until each pool in counts has that many Ready workers and
// matching Azure capacity.
func (f *fixture) stable(ctx context.Context, counts map[string]int) environment.Snapshot {
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

// signalStart asks the operator to start the paused controller and waits
// until it runs.
func (f *fixture) signalStart(ctx context.Context) {
	By("signaling the operator to start the controller")
	Expect(createPhaseSignal(ctx, f, "start-controller", nil)).To(Succeed())
	Eventually(ctx, f.env.Controller, 5*time.Minute, pollInterval).Should(Succeed())
}

var _ = Describe("Phased Azure VMSS cases", Serial, Label("Slow"), func() {
	It("AZ-P1-007 splits one two-node scale-up across similar pools", Label("AZ-P1-007", "balance"), func(ctx SpecContext) {
		f := setup(ctx, "balance", "AZ-P1-007")
		c := f.env.Config
		Expect(f.env.CheckPausedController(ctx)).To(Succeed())
		initial := f.stable(ctx, map[string]int{c.MainPool: 1, c.ZeroPool: 0, c.BalancePoolA: 0, c.BalancePoolB: 0})
		Expect(environment.CheckBalancePools(initial, c)).To(Succeed())
		Expect(f.env.CheckWorkerIsolation(ctx, initial, f.namespace.Name)).To(Succeed())

		By("creating two anti-affinity Pods for the balance pools")
		work := f.env.Deployment(f.namespace.Name, "balance-demand", c.BalanceLabel, c.DemandCPU, 2)
		work.Spec.Template.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				TopologyKey: corev1.LabelHostname, LabelSelector: work.Spec.Selector.DeepCopy(),
			}},
		}}
		Expect(f.env.K8s.Create(ctx, work)).To(Succeed())
		Eventually(ctx, func() error {
			return pendingBalancePods(ctx, f, work)
		}, 3*time.Minute, pollInterval).Should(Succeed())
		Expect(f.env.CheckPausedController(ctx)).To(Succeed())
		started := time.Now().Add(-time.Second)
		f.signalStart(ctx)

		By("waiting for one new worker in each balance pool")
		var grown environment.Snapshot
		Eventually(ctx, func() error {
			var err error
			grown, err = f.active(ctx)
			if err != nil {
				return err
			}
			for _, pool := range []string{c.BalancePoolA, c.BalancePoolB} {
				if grown.Pools[pool].Capacity > 1 || len(grown.Pools[pool].Instances) > 1 {
					StopTrying(fmt.Sprintf("balance pool %s grew by more than one, capacity %d instances %d",
						pool, grown.Pools[pool].Capacity, len(grown.Pools[pool].Instances))).Now()
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

		By("removing the demand and waiting for both balance workers to be deleted")
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
		By("creating zero-pool demand for a VM that never joins")
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
				return fmt.Errorf("waiting for one unregistered zero-pool VM, capacity %d instances %d", pool.Capacity, len(pool.Instances))
			}
			return nil
		}, waitTimeout, pollInterval).Should(Succeed())
		AddReportEntry("unregistered-vm", created.Pools[c.ZeroPool])

		var first environment.Instance
		for _, instance := range created.Pools[c.ZeroPool].Instances {
			first = instance
		}
		seen := map[string]environment.Instance{}
		record := func(snapshot environment.Snapshot) {
			for _, instance := range snapshot.Pools[c.ZeroPool].Instances {
				seen[instance.VMID] = instance
			}
		}
		record(created)
		By("waiting for the unregistered VM to run and then be deleted")
		running := false
		Eventually(ctx, func() error {
			current, err := f.active(ctx)
			if err != nil {
				return err
			}
			record(current)
			if len(environment.PoolNodes(current.Nodes, c.PoolID(c.ZeroPool))) != 0 {
				StopTrying("unregistered VM became a Kubernetes Node").Now()
			}
			var events corev1.EventList
			if err := f.env.K8s.List(ctx, &events, client.InNamespace(c.AutoscalerNamespace)); err != nil {
				return err
			}
			deletedEvent := unregisteredDeletionEvent(events.Items, first.ID)
			if instance, exists := current.Pools[c.ZeroPool].Instances[first.ID]; exists &&
				instance.VMID == first.VMID && current.Pools[c.ZeroPool].Capacity == 1 && !deletedEvent {
				nowRunning, err := f.env.InstanceRunning(ctx, c.ZeroPool, first.ID)
				if err != nil {
					return err
				}
				if running && !nowRunning {
					StopTrying("unregistered VM stopped before the deletion event").Now()
				}
				running = nowRunning
			}
			if !running {
				return fmt.Errorf("waiting for the unregistered VM to reach Running")
			}
			if !deletedEvent {
				return fmt.Errorf("waiting for deletion of the unregistered VM")
			}
			return f.env.DeletedGeneration(ctx, first, current, c.ZeroPool)
		}, waitTimeout, pollInterval).Should(Succeed())
		AddReportEntry("unregistered-power", "captured VM ran without a Node, then CA deleted its VM and NIC")
		By("removing the demand and waiting for every captured VM to be deleted")
		Expect(f.env.K8s.Delete(ctx, demand)).To(Succeed())
		Eventually(ctx, func() error {
			current, err := f.active(ctx)
			if err != nil {
				return err
			}
			record(current)
			if len(environment.PoolNodes(current.Nodes, c.PoolID(c.ZeroPool))) != 0 {
				StopTrying("unregistered VM became a Kubernetes Node").Now()
			}
			return workloadPodsGone(ctx, f, demand)
		}, 4*time.Minute, pollInterval).Should(Succeed())

		Eventually(ctx, func() error {
			after, err := f.active(ctx)
			if err != nil {
				return err
			}
			record(after)
			if len(environment.PoolNodes(after.Nodes, c.PoolID(c.ZeroPool))) != 0 {
				StopTrying("unregistered VM became a Kubernetes Node").Now()
			}
			if err := after.StablePools(c, map[string]int{c.MainPool: 1, c.ZeroPool: 0}); err != nil {
				return err
			}
			for _, instance := range seen {
				if err := f.env.DeletedGeneration(ctx, instance, after, c.ZeroPool); err != nil {
					return err
				}
			}
			return nil
		}, waitTimeout, pollInterval).Should(Succeed())
		AddReportEntry("physical-delete", fmt.Sprintf("%d captured unregistered VMs, Nodes and NICs were removed", len(seen)))
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
		By("waiting for main to grow to its minimum without pending demand")
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
					return fmt.Errorf("original main worker %s was replaced instead of growing to the minimum", id)
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
		By("checking that main stays at its minimum")
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

// pendingBalancePods requires both balance Pods to be pending and rejected
// by the scheduler.
func pendingBalancePods(ctx context.Context, f *fixture, work *appsv1.Deployment) error {
	var pods corev1.PodList
	if err := f.env.K8s.List(ctx, &pods, client.InNamespace(work.Namespace), client.MatchingLabels(work.Spec.Selector.MatchLabels)); err != nil {
		return fmt.Errorf("list balance Pods: %w", err)
	}
	if len(pods.Items) != 2 {
		return fmt.Errorf("waiting for two balance demand Pods, got %d", len(pods.Items))
	}
	for _, pod := range pods.Items {
		if pod.Spec.NodeName != "" || pod.Status.Phase != corev1.PodPending {
			return fmt.Errorf("balance Pod %s was scheduled to %q with phase %s before the two-node plan", pod.Name, pod.Spec.NodeName, pod.Status.Phase)
		}
		scheduled := false
		for _, condition := range pod.Status.Conditions {
			scheduled = scheduled || condition.Type == corev1.PodScheduled &&
				condition.Status == corev1.ConditionFalse && condition.Reason == corev1.PodReasonUnschedulable
		}
		if !scheduled {
			return fmt.Errorf("waiting for scheduler rejection of balance Pod %s", pod.Name)
		}
	}
	return nil
}

// readyBalancePods requires the two balance Pods to be Ready, one on a
// worker of each balance pool.
func readyBalancePods(ctx context.Context, f *fixture, work *appsv1.Deployment, snapshot environment.Snapshot) error {
	var pods corev1.PodList
	if err := f.env.K8s.List(ctx, &pods, client.InNamespace(work.Namespace), client.MatchingLabels(work.Spec.Selector.MatchLabels)); err != nil {
		return fmt.Errorf("list balance Pods: %w", err)
	}
	if len(pods.Items) != 2 {
		return fmt.Errorf("waiting for two balance Pods, got %d", len(pods.Items))
	}
	pools := map[string]bool{}
	for _, pod := range pods.Items {
		if !environment.PodReady(pod) {
			return fmt.Errorf("balance Pod %s is not Ready, phase %s", pod.Name, pod.Status.Phase)
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
		return fmt.Errorf("balance Pods must run on both new pool Nodes, got pools %v", pools)
	}
	return nil
}

// checkBalancedPlan requires one "Final scale-up plan" log line that adds one
// Node to each of pools a and b.
func checkBalancedPlan(logs, a, b string) error {
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
		groups := balancedPlanRE.FindStringSubmatch(raw)
		if groups != nil && ((groups[1] == a && groups[2] == b) || (groups[1] == b && groups[2] == a)) {
			return nil
		}
	}
	return fmt.Errorf("no single autoscaler plan split two nodes between the balance pools")
}

// unregisteredDeletionEvent reports whether events hold a DeleteUnregistered
// event for the VMSS instance instanceID.
func unregisteredDeletionEvent(events []corev1.Event, instanceID string) bool {
	for _, event := range events {
		if event.Reason == "DeleteUnregistered" &&
			strings.HasSuffix(strings.ToLower(strings.TrimSpace(event.Message)), strings.ToLower(instanceID)) {
			return true
		}
	}
	return false
}

func TestUnregisteredDeletionEvent(t *testing.T) {
	id := "/subscriptions/s/resourceGroups/owned/providers/Microsoft.Compute/virtualMachineScaleSets/zero/virtualMachines/0"
	event := corev1.Event{Reason: "DeleteUnregistered", Message: "Removed unregistered node azure://" + id}
	if !unregisteredDeletionEvent([]corev1.Event{event}, id) {
		t.Fatal("exact deleted instance event was missed")
	}
	event.Message = strings.Replace(event.Message, "/virtualMachines/0", "/virtualMachines/1", 1)
	if unregisteredDeletionEvent([]corev1.Event{event}, id) {
		t.Fatal("accepted a deletion for a different instance")
	}
	event.Reason = "ScaleUpTimedOut"
	if unregisteredDeletionEvent([]corev1.Event{event}, id) {
		t.Fatal("accepted a timeout rather than physical deletion")
	}
}

// checkMinimumPlan requires one minimum-size scale-up plan log line that
// grows pool from one to two Nodes.
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
