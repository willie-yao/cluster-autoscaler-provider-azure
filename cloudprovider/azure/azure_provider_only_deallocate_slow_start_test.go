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

package azure

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/cluster-autoscaler/pkg/cloudprovider"
	statusutils "sigs.k8s.io/cluster-autoscaler/pkg/clusterstate/utils"
	"sigs.k8s.io/cluster-autoscaler/pkg/core"
	"sigs.k8s.io/cluster-autoscaler/pkg/loop"
	"sigs.k8s.io/cluster-autoscaler/pkg/metrics"
	"sigs.k8s.io/cluster-autoscaler/pkg/test/integration"
	synctestutils "sigs.k8s.io/cluster-autoscaler/pkg/test/integration/synctest"
	catest "sigs.k8s.io/cluster-autoscaler/pkg/utils/test"
)

const (
	// Defaults from the selected stock core flags.
	stockMaxInactivity  = 10 * time.Minute
	stockMaxFailingTime = 15 * time.Minute
	stockMaxStartupTime = 20 * time.Minute

	slowStartPollInterval = 37 * time.Second
	slowStartActionPath   = "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/" +
		"virtualMachineScaleSets/pool/virtualMachines/1/start"
	slowStartPollPath = "/operations/slow-start"
)

type slowStartTrace struct {
	at     time.Time
	method string
	path   string
}

type nonterminalStartTransport struct {
	mu        sync.Mutex
	world     *parkingWorld
	trace     []slowStartTrace
	accepted  bool
	firstPoll chan struct{}
	pollOnce  sync.Once
}

func newNonterminalStartTransport(world *parkingWorld) *nonterminalStartTransport {
	return &nonterminalStartTransport{
		world:     world,
		firstPoll: make(chan struct{}),
	}
}

func (t *nonterminalStartTransport) Do(request *http.Request) (*http.Response, error) {
	if err := request.Context().Err(); err != nil {
		return nil, err
	}

	t.mu.Lock()
	t.trace = append(t.trace, slowStartTrace{
		at:     time.Now(),
		method: request.Method,
		path:   request.URL.Path,
	})
	t.mu.Unlock()

	switch {
	case request.Method == http.MethodPost && request.URL.Path == slowStartActionPath:
		// Azure accepted the operation and exposes the VM as starting while its LRO remains nonterminal.
		t.world.mu.Lock()
		if len(t.world.states) != 2 || t.world.states[1] != vmPowerStateDeallocated {
			t.world.mu.Unlock()
			return nil, fmt.Errorf("accepted Start requires one retained deallocated spare")
		}
		t.world.states[1] = vmPowerStateStarting
		t.world.mu.Unlock()
		t.world.record("start-accepted:1")

		t.mu.Lock()
		t.accepted = true
		t.mu.Unlock()
		header := make(http.Header)
		header.Set("Azure-AsyncOperation", "https://management.test"+slowStartPollPath)
		header.Set("Content-Type", "application/json")
		header.Set("Location", "https://management.test"+slowStartPollPath)
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    request,
		}, nil

	case request.Method == http.MethodGet && request.URL.Path == slowStartPollPath:
		t.pollOnce.Do(func() { close(t.firstPoll) })
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
				"Retry-After":  []string{"37"},
			},
			Body:    io.NopCloser(strings.NewReader(`{"status":"InProgress"}`)),
			Request: request,
		}, nil

	default:
		return nil, fmt.Errorf("unexpected slow Start request %s %s", request.Method, request.URL.Path)
	}
}

func (t *nonterminalStartTransport) snapshot() ([]slowStartTrace, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]slowStartTrace(nil), t.trace...), t.accepted
}

type slowStartLoopMoment struct {
	iteration int
	at        time.Time
}

type slowStartLoopDriver struct {
	started  chan slowStartLoopMoment
	returned chan slowStartLoopMoment
	advance  chan struct{}
}

func startSlowStartLoopDriver(
	ctx context.Context,
	autoscaler *core.StaticAutoscaler,
	healthCheck *metrics.HealthCheck,
	firstIteration int,
	iterations int,
) *slowStartLoopDriver {
	driver := &slowStartLoopDriver{
		started:  make(chan slowStartLoopMoment, iterations),
		returned: make(chan slowStartLoopMoment, iterations),
		advance:  make(chan struct{}),
	}
	go func() {
		for offset := 0; offset < iterations; offset++ {
			if offset > 0 {
				select {
				case <-ctx.Done():
					return
				case <-driver.advance:
				}
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
			started := slowStartLoopMoment{iteration: firstIteration + offset, at: time.Now()}
			driver.started <- started
			loop.RunAutoscalerOnce(ctx, autoscaler, healthCheck, started.at, started.iteration)
			driver.returned <- slowStartLoopMoment{iteration: started.iteration, at: time.Now()}
		}
	}()
	return driver
}

type slowStartFixture struct {
	world      *parkingWorld
	provider   *AzureCloudProvider
	group      *ScaleSet
	autoscaler *core.StaticAutoscaler
	health     *metrics.HealthCheck
	transport  *nonterminalStartTransport
	events     *record.FakeRecorder
	spareVMID  string
	demandPods []string
}

func newSlowStartFixture(
	t *testing.T,
	ctx context.Context,
	infra *integration.TestInfrastructure,
) *slowStartFixture {
	t.Helper()
	client := infra.Fakes.KubeClient
	world := &parkingWorld{states: []string{vmPowerStateRunning, vmPowerStateDeallocated}}
	provider, group := newParkingProvider(t, world, client, 1, 3, true)
	transport := newNonterminalStartTransport(world)
	provider.azureManager.azClient.vmssPowerClient = newTestVMSSPowerClient(t, transport)

	node := parkingNode(0, "active", true)
	_, err := client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	require.NoError(t, err)
	busy := catest.BuildScheduledTestPod("busy", 3500, 100, node.Name)
	_, err = client.CoreV1().Pods(busy.Namespace).Create(ctx, busy, metav1.CreateOptions{})
	require.NoError(t, err)

	autoscaler := parkingAutoscaler(t, ctx, infra, provider)
	events := record.NewFakeRecorder(100)
	autoscaler.LogRecorder, err = statusutils.NewStatusMapRecorder(
		client,
		"kube-system",
		events,
		true,
		"test-ca-status",
	)
	require.NoError(t, err)

	healthCheck := metrics.NewHealthCheck(stockMaxInactivity, stockMaxFailingTime, stockMaxStartupTime)
	healthCheck.StartMonitoring()
	warmupStart := time.Now()
	loop.RunAutoscalerOnce(ctx, autoscaler, healthCheck, warmupStart, 0)
	statusCode, body := slowStartHealthStatus(healthCheck)
	require.Equal(t, http.StatusOK, statusCode)
	require.Equal(t, "OK", body)
	require.Empty(t, world.history())
	trace, accepted := transport.snapshot()
	require.Empty(t, trace)
	require.False(t, accepted)

	demand := catest.BuildTestPod("persistent-demand", 2000, 100, catest.MarkUnschedulable())
	demand.Spec.NodeSelector = map[string]string{"pool": "workers"}
	_, err = client.CoreV1().Pods(demand.Namespace).Create(ctx, demand, metav1.CreateOptions{})
	require.NoError(t, err)
	synctest.Wait()

	return &slowStartFixture{
		world:      world,
		provider:   provider,
		group:      group,
		autoscaler: autoscaler,
		health:     healthCheck,
		transport:  transport,
		events:     events,
		spareVMID:  *world.vms()[1].Properties.VMID,
		demandPods: []string{demand.Name},
	}
}

func blockSlowStartPastHealthDeadline(
	t *testing.T,
	ctx context.Context,
	infra *integration.TestInfrastructure,
	fixture *slowStartFixture,
	iterations int,
) (*slowStartLoopDriver, slowStartLoopMoment) {
	t.Helper()
	driver := startSlowStartLoopDriver(ctx, fixture.autoscaler, fixture.health, 1, iterations)
	started := <-driver.started
	require.Equal(t, 1, started.iteration)
	select {
	case <-fixture.transport.firstPoll:
	case returned := <-driver.returned:
		t.Fatalf("loop iteration %d returned at %s before the first nonterminal poll", returned.iteration, returned.at)
	}

	require.Equal(t, map[string]bool{fixture.spareVMID: false}, slowStartPowerOverrides(fixture.group))
	require.Equal(t,
		[]string{vmPowerStateRunning, vmPowerStateStarting},
		slowStartWorldStates(fixture.world),
	)
	require.Equal(t, 2, slowStartTargetSize(t, fixture.group))
	require.Equal(t, 2, len(fixture.world.vms()))
	require.Equal(t, map[string]bool{fixture.spareVMID: false}, slowStartPowerOverrides(fixture.group))

	instances, err := fixture.group.Nodes(ctx)
	require.NoError(t, err)
	require.Len(t, instances, 2)
	require.Equal(t, cloudprovider.InstanceRunning, instances[0].Status.State)
	require.Equal(t, cloudprovider.InstanceCreating, instances[1].Status.State)

	readiness := fixture.autoscaler.ClusterStateRegistry.GetClusterReadiness()
	require.Contains(t, readiness.Ready, "node-0")
	require.False(t, fixture.autoscaler.ClusterStateRegistry.IsNodeGroupScalingUp(ctx, fixture.group.Id()))
	require.False(t, fixture.autoscaler.ClusterStateRegistry.HasNodeGroupStartedScaleUp(fixture.group.Id()))
	require.False(t,
		fixture.autoscaler.ClusterStateRegistry.BackoffStatusForNodeGroup(ctx, fixture.group, time.Now()).IsBackedOff,
	)
	upcoming, _ := fixture.autoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
	require.Zero(t, upcoming[fixture.group.Id()])
	currentSize, targetSize := fixture.autoscaler.ClusterStateRegistry.GetAutoscaledNodesCount()
	require.Equal(t, 1, currentSize)
	require.Equal(t, 1, targetSize, "the blocked iteration has not published the accepted target")

	lateDemand := catest.BuildTestPod("demand-created-while-start-blocked", 1000, 100, catest.MarkUnschedulable())
	lateDemand.Spec.NodeSelector = map[string]string{"pool": "workers"}
	_, err = infra.Fakes.KubeClient.CoreV1().Pods(lateDemand.Namespace).Create(ctx, lateDemand, metav1.CreateOptions{})
	require.NoError(t, err)
	fixture.demandPods = append(fixture.demandPods, lateDemand.Name)
	synctest.Wait()
	slowStartRequirePendingPods(t, ctx, infra.Fakes.KubeClient, fixture.demandPods)

	time.Sleep(stockMaxInactivity - time.Nanosecond)
	require.Equal(t, stockMaxInactivity-time.Nanosecond, time.Now().Sub(started.at))
	statusCode, body := slowStartHealthStatus(fixture.health)
	require.Equal(t, http.StatusOK, statusCode)
	require.Equal(t, "OK", body)
	slowStartRequireNoLoopMoment(t, driver.returned, "blocked loop returned before inactivity deadline")
	slowStartRequireNoLoopMoment(t, driver.started, "next loop started while IncreaseSize was blocked")

	time.Sleep(2 * time.Nanosecond)
	require.Equal(t, stockMaxInactivity+time.Nanosecond, time.Now().Sub(started.at))
	statusCode, body = slowStartHealthStatus(fixture.health)
	require.Equal(t, http.StatusInternalServerError, statusCode)
	require.Contains(t, body, "last activity more 10m0.000000001s ago")
	require.Equal(t, []string{"start-accepted:1"}, fixture.world.history())
	slowStartRequirePendingPods(t, ctx, infra.Fakes.KubeClient, fixture.demandPods)
	slowStartRequireNoLoopMoment(t, driver.returned, "blocked loop returned at inactivity failure")
	slowStartRequireNoLoopMoment(t, driver.started, "next loop started at inactivity failure")

	trace, accepted := fixture.transport.snapshot()
	require.True(t, accepted)
	slowStartValidateTrace(t, trace, started.at)
	return driver, started
}

func TestProviderOnlyDeallocateSlowAcceptedStartCharacterization(t *testing.T) {
	t.Run("simulated restart after health deadline", func(t *testing.T) {
		infra := integration.SetupInfrastructure(t)
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer synctestutils.TearDown(cancel)
			firstCtx, stopFirst := context.WithCancel(ctx)
			defer stopFirst()

			fixture := newSlowStartFixture(t, firstCtx, infra)
			driver, blockedStart := blockSlowStartPastHealthDeadline(t, firstCtx, infra, fixture, 2)
			beforeIDs := slowStartWorldVMIDs(fixture.world)

			stopFirst()
			returned := <-driver.returned
			require.Equal(t, 1, returned.iteration)
			require.Equal(t, stockMaxInactivity+time.Nanosecond, returned.at.Sub(blockedStart.at))
			synctest.Wait()
			slowStartRequireNoLoopMoment(t, driver.started, "cancelled process started another iteration")
			require.Equal(t,
				[]string{vmPowerStateRunning, vmPowerStateStarting},
				slowStartWorldStates(fixture.world),
			)
			require.Equal(t, []string{"start-accepted:1"}, fixture.world.history())

			infra.Fakes.InformerFactory = informers.NewSharedInformerFactory(infra.Fakes.KubeClient, 0)
			recreatedProvider, recreatedGroup := newParkingProvider(t, fixture.world, infra.Fakes.KubeClient, 1, 3, true)
			recreatedProvider.azureManager.azClient.vmssPowerClient = newTestVMSSPowerClient(t, fixture.transport)
			recreatedAutoscaler := parkingAutoscaler(t, ctx, infra, recreatedProvider)
			recreatedHealth := metrics.NewHealthCheck(stockMaxInactivity, stockMaxFailingTime, stockMaxStartupTime)
			recreatedHealth.StartMonitoring()

			require.NotSame(t, fixture.provider, recreatedProvider)
			require.NotSame(t, fixture.group, recreatedGroup)
			require.NotSame(t, fixture.autoscaler, recreatedAutoscaler)
			require.Empty(t, slowStartPowerOverrides(recreatedGroup))
			require.Equal(t, 2, slowStartTargetSize(t, recreatedGroup))
			require.False(t, recreatedAutoscaler.ClusterStateRegistry.IsNodeGroupScalingUp(ctx, recreatedGroup.Id()))
			require.False(t, recreatedAutoscaler.ClusterStateRegistry.HasNodeGroupStartedScaleUp(recreatedGroup.Id()))

			recreatedDriver := startSlowStartLoopDriver(ctx, recreatedAutoscaler, recreatedHealth, 0, 1)
			recreatedStarted := <-recreatedDriver.started
			recreatedReturned := <-recreatedDriver.returned
			require.Equal(t, recreatedStarted.at, recreatedReturned.at)

			trace, accepted := fixture.transport.snapshot()
			require.True(t, accepted)
			slowStartValidateTrace(t, trace, blockedStart.at)
			require.Equal(t, 1, slowStartCountMethod(trace, http.MethodPost))
			require.Equal(t, 17, slowStartCountMethod(trace, http.MethodGet))
			require.Equal(t, []string{"start-accepted:1", "grow:3"}, fixture.world.history())
			require.Equal(t, 3, slowStartTargetSize(t, recreatedGroup))
			require.Equal(t,
				[]string{vmPowerStateRunning, vmPowerStateStarting, vmPowerStateStarting},
				slowStartWorldStates(fixture.world),
			)
			afterIDs := slowStartWorldVMIDs(fixture.world)
			require.Equal(t, beforeIDs, afterIDs[:2])
			require.Equal(t, "vm-identity-2", afterIDs[2])
			require.Empty(t, slowStartPowerOverrides(recreatedGroup))

			readiness := recreatedAutoscaler.ClusterStateRegistry.GetClusterReadiness()
			require.Contains(t, readiness.Ready, "node-0")
			require.True(t, recreatedAutoscaler.ClusterStateRegistry.IsNodeGroupScalingUp(ctx, recreatedGroup.Id()))
			require.True(t, recreatedAutoscaler.ClusterStateRegistry.HasNodeGroupStartedScaleUp(recreatedGroup.Id()))
			require.False(t,
				recreatedAutoscaler.ClusterStateRegistry.BackoffStatusForNodeGroup(ctx, recreatedGroup, time.Now()).IsBackedOff,
			)
			upcoming, _ := recreatedAutoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
			require.Equal(t, 2, upcoming[recreatedGroup.Id()])
			currentSize, targetSize := recreatedAutoscaler.ClusterStateRegistry.GetAutoscaledNodesCount()
			require.Equal(t, 1, currentSize)
			require.Equal(t, 3, targetSize)
			statusCode, body := slowStartHealthStatus(recreatedHealth)
			require.Equal(t, http.StatusOK, statusCode)
			require.Equal(t, "OK", body)
			slowStartRequirePendingPods(t, ctx, infra.Fakes.KubeClient, fixture.demandPods)

			t.Logf(
				"simulated restart timeline: health failed at %s, old loop returned at %s, recreated loop grew at %s; Start POSTs=%d polls=%d",
				stockMaxInactivity+time.Nanosecond,
				returned.at.Sub(blockedStart.at),
				recreatedStarted.at.Sub(blockedStart.at),
				slowStartCountMethod(trace, http.MethodPost),
				slowStartCountMethod(trace, http.MethodGet),
			)
		})
	})

	t.Run("provider timeout restores loop but not accepted request tracking", func(t *testing.T) {
		infra := integration.SetupInfrastructure(t)
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer synctestutils.TearDown(cancel)

			fixture := newSlowStartFixture(t, ctx, infra)
			driver, blockedStart := blockSlowStartPastHealthDeadline(t, ctx, infra, fixture, 2)
			beforeIDs := slowStartWorldVMIDs(fixture.world)

			returned := <-driver.returned
			require.Equal(t, 1, returned.iteration)
			require.Equal(t, asyncContextTimeout, returned.at.Sub(blockedStart.at))
			require.Contains(t, slowStartDrainEvents(fixture.events), "context deadline exceeded")

			statusCode, body := slowStartHealthStatus(fixture.health)
			require.Equal(t, http.StatusOK, statusCode, "the wrapper records the overall RunOnce as successful")
			require.Equal(t, "OK", body)
			require.Equal(t, []string{"start-accepted:1"}, fixture.world.history())
			require.Equal(t, 2, slowStartTargetSize(t, fixture.group))
			require.Equal(t,
				[]string{vmPowerStateRunning, vmPowerStateStarting},
				slowStartWorldStates(fixture.world),
			)
			require.Equal(t, map[string]bool{fixture.spareVMID: false}, slowStartPowerOverrides(fixture.group))
			require.False(t, fixture.autoscaler.ClusterStateRegistry.IsNodeGroupScalingUp(ctx, fixture.group.Id()))
			require.False(t, fixture.autoscaler.ClusterStateRegistry.HasNodeGroupStartedScaleUp(fixture.group.Id()))
			require.False(t,
				fixture.autoscaler.ClusterStateRegistry.BackoffStatusForNodeGroup(ctx, fixture.group, time.Now()).IsBackedOff,
				"the five-minute failure backoff is timestamped at the 30-minute-old loop start",
			)
			upcoming, _ := fixture.autoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
			require.Zero(t, upcoming[fixture.group.Id()])
			currentSize, targetSize := fixture.autoscaler.ClusterStateRegistry.GetAutoscaledNodesCount()
			require.Equal(t, 1, currentSize)
			require.Equal(t, 1, targetSize, "the timed-out loop retains its pre-Start core snapshot")
			slowStartRequirePendingPods(t, ctx, infra.Fakes.KubeClient, fixture.demandPods)

			traceAtTimeout, accepted := fixture.transport.snapshot()
			require.True(t, accepted)
			slowStartValidateTrace(t, traceAtTimeout, blockedStart.at)
			require.Equal(t, 1, slowStartCountMethod(traceAtTimeout, http.MethodPost))
			require.Equal(t, 49, slowStartCountMethod(traceAtTimeout, http.MethodGet))

			driver.advance <- struct{}{}
			nextStarted := <-driver.started
			nextReturned := <-driver.returned
			require.Equal(t, 2, nextStarted.iteration)
			require.Equal(t, nextStarted.at, nextReturned.at)

			trace, accepted := fixture.transport.snapshot()
			require.True(t, accepted)
			require.Equal(t, traceAtTimeout, trace, "later planning must not poll or restart the old operation")
			require.Equal(t, 1, slowStartCountMethod(trace, http.MethodPost))
			require.Equal(t, []string{"start-accepted:1", "grow:3"}, fixture.world.history())
			require.Equal(t, 3, slowStartTargetSize(t, fixture.group))
			require.Equal(t,
				[]string{vmPowerStateRunning, vmPowerStateStarting, vmPowerStateStarting},
				slowStartWorldStates(fixture.world),
			)
			afterIDs := slowStartWorldVMIDs(fixture.world)
			require.Equal(t, beforeIDs, afterIDs[:2])
			require.Equal(t, "vm-identity-2", afterIDs[2])
			require.Equal(t, map[string]bool{fixture.spareVMID: false}, slowStartPowerOverrides(fixture.group))

			readiness := fixture.autoscaler.ClusterStateRegistry.GetClusterReadiness()
			require.Contains(t, readiness.Ready, "node-0")
			require.True(t, fixture.autoscaler.ClusterStateRegistry.IsNodeGroupScalingUp(ctx, fixture.group.Id()))
			require.True(t, fixture.autoscaler.ClusterStateRegistry.HasNodeGroupStartedScaleUp(fixture.group.Id()))
			require.False(t,
				fixture.autoscaler.ClusterStateRegistry.BackoffStatusForNodeGroup(ctx, fixture.group, time.Now()).IsBackedOff,
			)
			upcoming, _ = fixture.autoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
			require.Equal(t, 2, upcoming[fixture.group.Id()])
			currentSize, targetSize = fixture.autoscaler.ClusterStateRegistry.GetAutoscaledNodesCount()
			require.Equal(t, 1, currentSize)
			require.Equal(t, 3, targetSize)
			statusCode, body = slowStartHealthStatus(fixture.health)
			require.Equal(t, http.StatusOK, statusCode)
			require.Equal(t, "OK", body)
			slowStartRequirePendingPods(t, ctx, infra.Fakes.KubeClient, fixture.demandPods)

			t.Logf(
				"provider timeout timeline: health failed at %s, loop returned at %s, next iteration started at %s; Start POSTs=%d polls=%d",
				stockMaxInactivity+time.Nanosecond,
				returned.at.Sub(blockedStart.at),
				nextStarted.at.Sub(blockedStart.at),
				slowStartCountMethod(trace, http.MethodPost),
				slowStartCountMethod(trace, http.MethodGet),
			)
		})
	})
}

func slowStartHealthStatus(healthCheck *metrics.HealthCheck) (int, string) {
	response := httptest.NewRecorder()
	healthCheck.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health-check", nil))
	return response.Code, response.Body.String()
}

func slowStartPowerOverrides(group *ScaleSet) map[string]bool {
	group.powerMutex.Lock()
	defer group.powerMutex.Unlock()
	result := make(map[string]bool, len(group.powerOverrides))
	for vmID, parked := range group.powerOverrides {
		result[vmID] = parked
	}
	return result
}

func slowStartWorldStates(world *parkingWorld) []string {
	world.mu.Lock()
	defer world.mu.Unlock()
	return append([]string(nil), world.states...)
}

func slowStartWorldVMIDs(world *parkingWorld) []string {
	vms := world.vms()
	result := make([]string, 0, len(vms))
	for _, vm := range vms {
		result = append(result, *vm.Properties.VMID)
	}
	return result
}

func slowStartTargetSize(t *testing.T, group *ScaleSet) int {
	t.Helper()
	size, err := group.TargetSize(context.Background())
	require.NoError(t, err)
	return size
}

func slowStartRequirePendingPods(
	t *testing.T,
	ctx context.Context,
	client kubernetes.Interface,
	names []string,
) {
	t.Helper()
	for _, name := range names {
		pod, err := client.CoreV1().Pods("default").Get(ctx, name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Empty(t, pod.Spec.NodeName)
	}
}

func slowStartRequireNoLoopMoment(t *testing.T, moments <-chan slowStartLoopMoment, message string) {
	t.Helper()
	select {
	case moment := <-moments:
		t.Fatalf("%s: iteration %d at %s", message, moment.iteration, moment.at)
	default:
	}
}

func slowStartValidateTrace(t *testing.T, trace []slowStartTrace, start time.Time) {
	t.Helper()
	require.GreaterOrEqual(t, len(trace), 2)
	require.Equal(t, slowStartTrace{at: start, method: http.MethodPost, path: slowStartActionPath}, trace[0])
	for i, request := range trace[1:] {
		require.Equal(t, http.MethodGet, request.method)
		require.Equal(t, slowStartPollPath, request.path)
		require.False(t, request.at.Before(start))
		if i > 0 {
			require.Equal(t, slowStartPollInterval, request.at.Sub(trace[i].at))
		}
	}
}

func slowStartCountMethod(trace []slowStartTrace, method string) int {
	count := 0
	for _, request := range trace {
		if request.method == method {
			count++
		}
	}
	return count
}

func slowStartDrainEvents(events *record.FakeRecorder) string {
	var messages []string
	for len(events.Events) > 0 {
		messages = append(messages, <-events.Events)
	}
	return strings.Join(messages, "\n")
}

var _ policy.Transporter = (*nonterminalStartTransport)(nil)
