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
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/stretchr/testify/require"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/cluster-autoscaler/pkg/cloudprovider"
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

	slowStartMaxNodeProvisionTime = 15 * time.Minute
	slowStartPollInterval         = 37 * time.Second
	slowStartActionPath           = "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/" +
		"virtualMachineScaleSets/pool/virtualMachines/1/start"
	slowDeallocateActionPath = "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/" +
		"virtualMachineScaleSets/pool/virtualMachines/1/deallocate"
	slowStartPollPath        = "/operations/slow-start"
	slowStartResultPath      = "/operations/slow-start/result"
	slowRetryStartPollPath   = "/operations/slow-start-retry"
	slowRetryStartResultPath = "/operations/slow-start-retry/result"
	slowDeallocatePollPath   = "/operations/slow-deallocate"
	slowDeallocateResultPath = "/operations/slow-deallocate/result"
)

type slowStartTrace struct {
	at     time.Time
	method string
	path   string
}

type nonterminalStartTransport struct {
	mu                   sync.Mutex
	world                *parkingWorld
	trace                []slowStartTrace
	accepted             bool
	completion           string
	retryCompletion      string
	deallocateCompletion string
	starts               int
	firstPoll            chan struct{}
	terminal             chan struct{}
	result               chan struct{}
	retryStart           chan struct{}
	retryResult          chan struct{}
	deallocate           chan struct{}
	deallocated          chan struct{}
	pollOnce             sync.Once
	termOnce             sync.Once
	resultOnce           sync.Once
	retryOnce            sync.Once
	retryResultOnce      sync.Once
	deallocateOnce       sync.Once
	deallocatedOnce      sync.Once
}

func newNonterminalStartTransport(world *parkingWorld) *nonterminalStartTransport {
	return &nonterminalStartTransport{
		world:                world,
		completion:           "InProgress",
		retryCompletion:      "InProgress",
		deallocateCompletion: "InProgress",
		firstPoll:            make(chan struct{}),
		terminal:             make(chan struct{}),
		result:               make(chan struct{}),
		retryStart:           make(chan struct{}),
		retryResult:          make(chan struct{}),
		deallocate:           make(chan struct{}),
		deallocated:          make(chan struct{}),
	}
}

func (t *nonterminalStartTransport) setCompletion(status string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.completion = status
}

func (t *nonterminalStartTransport) setRetryCompletion(status string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.retryCompletion = status
}

func (t *nonterminalStartTransport) setDeallocateCompletion(status string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.deallocateCompletion = status
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
		t.mu.Lock()
		t.starts++
		attempt := t.starts
		t.mu.Unlock()
		t.world.mu.Lock()
		if len(t.world.states) < 2 || t.world.states[1] != vmPowerStateDeallocated {
			t.world.mu.Unlock()
			return nil, fmt.Errorf("accepted Start requires one retained deallocated spare")
		}
		t.world.states[1] = vmPowerStateStarting
		t.world.mu.Unlock()
		pollPath := slowStartPollPath
		resultPath := slowStartResultPath
		event := "start-accepted:1"
		if attempt == 2 {
			pollPath = slowRetryStartPollPath
			resultPath = slowRetryStartResultPath
			event = "start-accepted:1:retry"
			t.retryOnce.Do(func() { close(t.retryStart) })
		} else if attempt != 1 {
			return nil, fmt.Errorf("unexpected Start attempt %d", attempt)
		}
		t.world.record(event)
		t.mu.Lock()
		t.accepted = true
		t.mu.Unlock()
		header := make(http.Header)
		header.Set("Azure-AsyncOperation", "https://management.test"+pollPath)
		header.Set("Content-Type", "application/json")
		header.Set("Location", "https://management.test"+resultPath)
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    request,
		}, nil

	case request.Method == http.MethodGet && request.URL.Path == slowStartPollPath:
		t.pollOnce.Do(func() { close(t.firstPoll) })
		t.mu.Lock()
		completion := t.completion
		t.mu.Unlock()
		if completion != "InProgress" {
			t.world.mu.Lock()
			if t.world.provisioning == nil {
				t.world.provisioning = make(map[int]string)
			}
			switch completion {
			case "Succeeded":
				t.world.states[1] = vmPowerStateRunning
				t.world.provisioning[1] = provisioningStateSucceeded
			case "Failed":
				t.world.states[1] = vmPowerStateDeallocated
				t.world.provisioning[1] = VMProvisioningStateFailed
			}
			t.world.mu.Unlock()
			t.termOnce.Do(func() { close(t.terminal) })
		}
		body := fmt.Sprintf(`{"status":%q}`, completion)
		if completion == "Failed" {
			body = `{"status":"Failed","error":{"code":"StartFailed","message":"controlled terminal Start failure"}}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
				"Retry-After":  []string{"37"},
			},
			Body:    io.NopCloser(strings.NewReader(body)),
			Request: request,
		}, nil

	case request.Method == http.MethodGet && request.URL.Path == slowStartResultPath:
		t.resultOnce.Do(func() { close(t.result) })
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    request,
		}, nil

	case request.Method == http.MethodGet && request.URL.Path == slowRetryStartPollPath:
		t.mu.Lock()
		completion := t.retryCompletion
		t.mu.Unlock()
		if completion == "Succeeded" {
			t.world.mu.Lock()
			t.world.states[1] = vmPowerStateRunning
			t.world.provisioning[1] = provisioningStateSucceeded
			t.world.mu.Unlock()
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
				"Retry-After":  []string{"37"},
			},
			Body:    io.NopCloser(strings.NewReader(fmt.Sprintf(`{"status":%q}`, completion))),
			Request: request,
		}, nil

	case request.Method == http.MethodGet && request.URL.Path == slowRetryStartResultPath:
		t.retryResultOnce.Do(func() { close(t.retryResult) })
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    request,
		}, nil

	case request.Method == http.MethodPost && request.URL.Path == slowDeallocateActionPath:
		t.world.mu.Lock()
		t.world.states[1] = vmPowerStateDeallocating
		t.world.mu.Unlock()
		t.world.record("deallocate-accepted:1")
		t.deallocateOnce.Do(func() { close(t.deallocate) })
		header := make(http.Header)
		header.Set("Azure-AsyncOperation", "https://management.test"+slowDeallocatePollPath)
		header.Set("Content-Type", "application/json")
		header.Set("Location", "https://management.test"+slowDeallocateResultPath)
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    request,
		}, nil

	case request.Method == http.MethodGet && request.URL.Path == slowDeallocatePollPath:
		t.mu.Lock()
		completion := t.deallocateCompletion
		t.mu.Unlock()
		if completion == "Succeeded" {
			t.world.mu.Lock()
			t.world.states[1] = vmPowerStateDeallocated
			if t.world.provisioning == nil {
				t.world.provisioning = make(map[int]string)
			}
			t.world.provisioning[1] = provisioningStateSucceeded
			t.world.mu.Unlock()
			t.deallocatedOnce.Do(func() {
				t.world.record("deallocate-complete:1")
				close(t.deallocated)
			})
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
				"Retry-After":  []string{"37"},
			},
			Body:    io.NopCloser(strings.NewReader(fmt.Sprintf(`{"status":%q}`, completion))),
			Request: request,
		}, nil

	case request.Method == http.MethodGet && request.URL.Path == slowDeallocateResultPath:
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    request,
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

func slowStartAutoscaler(
	t *testing.T,
	ctx context.Context,
	infra *integration.TestInfrastructure,
	provider *AzureCloudProvider,
) *core.StaticAutoscaler {
	t.Helper()
	opts := integration.NewTestConfig().ResolveOptions()
	opts.CloudProviderName = "azure"
	opts.InitialNodeGroupBackoffDuration = 5 * time.Minute
	opts.MaxNodeGroupBackoffDuration = 30 * time.Minute
	opts.NodeGroupBackoffResetTimeout = 3 * time.Hour
	opts.CordonNodeBeforeTerminate = true
	opts.NodeGroupDefaults.ScaleDownUnneededTime = time.Second
	opts.NodeGroupDefaults.MaxNodeProvisionTime = slowStartMaxNodeProvisionTime
	opts.MaxGracefulTerminationSec = 30
	opts.MaxPodEvictionTime = 30 * time.Second
	opts.UnremovableNodeRecheckTimeout = time.Second
	autoscaler, _, err := integration.DefaultAutoscalingBuilder(opts, infra).WithCloudProvider(provider).Build(ctx)
	require.NoError(t, err)
	require.NoError(t, autoscaler.Start())
	static := autoscaler.(*core.StaticAutoscaler)
	require.Equal(t, slowStartMaxNodeProvisionTime, static.NodeGroupDefaults.MaxNodeProvisionTime)
	go func() {
		<-ctx.Done()
		static.ClusterStateRegistry.Stop()
	}()
	return static
}

type slowStartFixture struct {
	world      *parkingWorld
	group      *ScaleSet
	autoscaler *core.StaticAutoscaler
	health     *metrics.HealthCheck
	transport  *nonterminalStartTransport
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

	autoscaler := slowStartAutoscaler(t, ctx, infra, provider)

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
		group:      group,
		autoscaler: autoscaler,
		health:     healthCheck,
		transport:  transport,
		spareVMID:  *world.vms()[1].Properties.VMID,
		demandPods: []string{demand.Name},
	}
}

func startAcceptedSlowStart(
	t *testing.T,
	ctx context.Context,
	fixture *slowStartFixture,
	iterations int,
) (*slowStartLoopDriver, slowStartLoopMoment) {
	t.Helper()
	driver := startSlowStartLoopDriver(ctx, fixture.autoscaler, fixture.health, 1, iterations)
	started := <-driver.started
	require.Equal(t, 1, started.iteration)
	returned := <-driver.returned
	require.Equal(t, started.at, returned.at)
	synctest.Wait()
	<-fixture.transport.firstPoll

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
	require.True(t, fixture.autoscaler.ClusterStateRegistry.IsNodeGroupScalingUp(ctx, fixture.group.Id()))
	require.True(t, fixture.autoscaler.ClusterStateRegistry.HasNodeGroupStartedScaleUp(fixture.group.Id()))
	require.False(t,
		fixture.autoscaler.ClusterStateRegistry.BackoffStatusForNodeGroup(ctx, fixture.group, time.Now()).IsBackedOff,
	)
	upcoming, _ := fixture.autoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
	require.Equal(t, 1, upcoming[fixture.group.Id()])
	currentSize, targetSize := fixture.autoscaler.ClusterStateRegistry.GetAutoscaledNodesCount()
	require.Equal(t, 1, currentSize)
	require.Equal(t, 2, targetSize)
	statusCode, body := slowStartHealthStatus(fixture.health)
	require.Equal(t, http.StatusOK, statusCode)
	require.Equal(t, "OK", body)
	require.Equal(t, []string{"start-accepted:1"}, fixture.world.history())

	trace, accepted := fixture.transport.snapshot()
	require.True(t, accepted)
	slowStartValidateTrace(t, trace, started.at)
	return driver, started
}

func addSlowStartLateDemand(
	t *testing.T,
	ctx context.Context,
	infra *integration.TestInfrastructure,
	fixture *slowStartFixture,
) {
	t.Helper()
	lateDemand := catest.BuildTestPod("demand-created-while-start-blocked", 1000, 100, catest.MarkUnschedulable())
	lateDemand.Spec.NodeSelector = map[string]string{"pool": "workers"}
	_, err := infra.Fakes.KubeClient.CoreV1().Pods(lateDemand.Namespace).Create(ctx, lateDemand, metav1.CreateOptions{})
	require.NoError(t, err)
	fixture.demandPods = append(fixture.demandPods, lateDemand.Name)
	synctest.Wait()
	slowStartRequirePendingPods(t, ctx, infra.Fakes.KubeClient, fixture.demandPods)
	slowStartRequireDemandFitsReturningWorker(t, ctx, infra.Fakes.KubeClient, fixture)
}

func advanceSlowStartLoop(
	t *testing.T,
	driver *slowStartLoopDriver,
	after time.Duration,
	iteration int,
) slowStartLoopMoment {
	t.Helper()
	time.Sleep(after)
	driver.advance <- struct{}{}
	started := <-driver.started
	returned := <-driver.returned
	require.Equal(t, iteration, started.iteration)
	require.Equal(t, started.at, returned.at)
	synctest.Wait()
	return returned
}

func TestProviderOnlyDeallocateSlowAcceptedStartCharacterization(t *testing.T) {
	t.Run("accepted Start keeps serial loop healthy through readiness", func(t *testing.T) {
		infra := integration.SetupInfrastructure(t)
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer synctestutils.TearDown(cancel)

			fixture := newSlowStartFixture(t, ctx, infra)
			driver, acceptedAt := startAcceptedSlowStart(t, ctx, fixture, 14)
			addSlowStartLateDemand(t, ctx, infra, fixture)

			for iteration := 2; iteration <= 12; iteration++ {
				advanceSlowStartLoop(t, driver, time.Minute, iteration)
				require.Equal(t, []string{"start-accepted:1"}, fixture.world.history())
				require.Equal(t, 2, slowStartTargetSize(t, fixture.group))
				require.True(t, fixture.autoscaler.ClusterStateRegistry.HasNodeGroupStartedScaleUp(fixture.group.Id()))
				upcoming, _ := fixture.autoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
				require.Equal(t, 1, upcoming[fixture.group.Id()])
			}
			require.Equal(t, 11*time.Minute, time.Now().Sub(acceptedAt.at))
			require.Less(t, time.Now().Sub(acceptedAt.at), slowStartMaxNodeProvisionTime)
			statusCode, body := slowStartHealthStatus(fixture.health)
			require.Equal(t, http.StatusOK, statusCode)
			require.Equal(t, "OK", body)
			slowStartRequirePendingPods(t, ctx, infra.Fakes.KubeClient, fixture.demandPods)

			fixture.transport.setCompletion("Succeeded")
			<-fixture.transport.terminal
			<-fixture.transport.result
			synctest.Wait()
			require.Equal(t,
				[]string{vmPowerStateRunning, vmPowerStateRunning},
				slowStartWorldStates(fixture.world),
			)
			require.Equal(t, 2, slowStartTargetSize(t, fixture.group))
			require.Empty(t, slowStartPowerOverrides(fixture.group))
			instances, err := fixture.group.Nodes(ctx)
			require.NoError(t, err)
			require.Len(t, instances, 2)
			require.Equal(t, cloudprovider.InstanceRunning, instances[1].Status.State)

			advanceSlowStartLoop(t, driver, 0, 13)
			readiness := fixture.autoscaler.ClusterStateRegistry.GetClusterReadiness()
			require.Contains(t, readiness.Ready, "node-0")
			require.NotContains(t, readiness.Ready, "node-1")
			require.True(t, fixture.autoscaler.ClusterStateRegistry.HasNodeGroupStartedScaleUp(fixture.group.Id()))
			upcoming, _ := fixture.autoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
			require.Equal(t, 1, upcoming[fixture.group.Id()], "Azure completion alone is not Node registration")
			require.Equal(t, []string{"start-accepted:1"}, fixture.world.history())

			registered := parkingNode(1, "registered-after-start", true)
			_, err = infra.Fakes.KubeClient.CoreV1().Nodes().Create(ctx, registered, metav1.CreateOptions{})
			require.NoError(t, err)
			synctest.Wait()
			advanceSlowStartLoop(t, driver, 0, 14)
			readiness = fixture.autoscaler.ClusterStateRegistry.GetClusterReadiness()
			require.Contains(t, readiness.Ready, "node-0")
			require.Contains(t, readiness.Ready, "node-1")
			upcoming, _ = fixture.autoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
			require.Zero(t, upcoming[fixture.group.Id()])
			require.False(t, fixture.autoscaler.ClusterStateRegistry.HasNodeGroupStartedScaleUp(fixture.group.Id()))
			currentSize, targetSize := fixture.autoscaler.ClusterStateRegistry.GetAutoscaledNodesCount()
			require.Equal(t, 2, currentSize)
			require.Equal(t, 2, targetSize)
			require.Equal(t, []string{"start-accepted:1"}, fixture.world.history())
			statusCode, body = slowStartHealthStatus(fixture.health)
			require.Equal(t, http.StatusOK, statusCode)
			require.Equal(t, "OK", body)

			trace, accepted := fixture.transport.snapshot()
			require.True(t, accepted)
			slowStartValidateTrace(t, trace, acceptedAt.at)
			require.Equal(t, 1, slowStartCountMethod(trace, http.MethodPost))
			t.Logf(
				"accepted Start timeline: first loop returned at %s, health remained 200 through %s, Azure completed before iteration 13, Node became Ready in iteration 14; Start POSTs=%d polls=%d",
				time.Duration(0),
				time.Now().Sub(acceptedAt.at),
				slowStartCountMethod(trace, http.MethodPost),
				slowStartCountPath(trace, slowStartPollPath),
			)
		})
	})

	t.Run("terminal accepted failure deallocates before later reuse", func(t *testing.T) {
		infra := integration.SetupInfrastructure(t)
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer synctestutils.TearDown(cancel)

			fixture := newSlowStartFixture(t, ctx, infra)
			require.False(t, fixture.group.enableFastDeleteOnFailedProvisioning)
			driver, acceptedAt := startAcceptedSlowStart(t, ctx, fixture, 8)
			addSlowStartLateDemand(t, ctx, infra, fixture)
			beforeVMIDs := slowStartWorldVMIDs(fixture.world)
			beforeDisks := slowStartWorldDiskIDs(fixture.world)

			fixture.transport.setCompletion("Failed")
			<-fixture.transport.terminal
			synctest.Wait()
			instances, err := fixture.group.Nodes(ctx)
			require.NoError(t, err)
			require.Len(t, instances, 2)
			require.Equal(t, cloudprovider.InstanceCreating, instances[1].Status.State)
			require.NotNil(t, instances[1].Status.ErrorInfo)
			require.Equal(t, "start-deallocated-failed", instances[1].Status.ErrorInfo.ErrorCode)
			require.Equal(t, 2, slowStartTargetSize(t, fixture.group))
			require.Equal(t, map[string]bool{fixture.spareVMID: false}, slowStartPowerOverrides(fixture.group))
			require.True(t, fixture.autoscaler.ClusterStateRegistry.HasNodeGroupStartedScaleUp(fixture.group.Id()))

			advanceSlowStartLoop(t, driver, time.Minute, 2)
			<-fixture.transport.deallocate
			require.Equal(t, []string{"start-accepted:1", "deallocate-accepted:1"}, fixture.world.history())
			require.Equal(t, 1, slowStartTargetSize(t, fixture.group))
			require.Equal(t, map[string]bool{fixture.spareVMID: true}, slowStartSyntheticDeallocating(fixture.group))
			instances, err = fixture.group.Nodes(ctx)
			require.NoError(t, err)
			require.Len(t, instances, 2)
			require.Equal(t, cloudprovider.InstanceDeleting, instances[1].Status.State)
			upcoming, _ := fixture.autoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
			require.Zero(t, upcoming[fixture.group.Id()])

			fixture.transport.setDeallocateCompletion("Succeeded")
			<-fixture.transport.deallocated
			synctest.Wait()
			history := fixture.world.history()
			require.Equal(t, []string{"start-accepted:1", "deallocate-accepted:1", "deallocate-complete:1"}, history)
			require.Equal(t, beforeVMIDs, slowStartWorldVMIDs(fixture.world))
			require.Equal(t, beforeDisks, slowStartWorldDiskIDs(fixture.world))
			require.Len(t, fixture.world.vms(), 2)
			require.Equal(t, 1, slowStartTargetSize(t, fixture.group))
			require.Empty(t, slowStartPowerOverrides(fixture.group))
			require.Empty(t, slowStartSyntheticDeallocating(fixture.group))
			require.False(t, fixture.autoscaler.ClusterStateRegistry.HasNodeGroupStartedScaleUp(fixture.group.Id()))
			require.True(t,
				fixture.autoscaler.ClusterStateRegistry.BackoffStatusForNodeGroup(ctx, fixture.group, time.Now()).IsBackedOff,
			)
			upcoming, _ = fixture.autoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
			require.Zero(t, upcoming[fixture.group.Id()])

			for iteration := 3; iteration <= 8 && !slices.Contains(history, "start-accepted:1:retry"); iteration++ {
				advanceSlowStartLoop(t, driver, time.Minute, iteration)
				history = fixture.world.history()
			}
			require.Equal(t, []string{
				"start-accepted:1",
				"deallocate-accepted:1",
				"deallocate-complete:1",
				"start-accepted:1:retry",
			}, history)
			trace, accepted := fixture.transport.snapshot()
			require.True(t, accepted)
			require.Equal(t, 3, slowStartCountMethod(trace, http.MethodPost))
			require.Equal(t, beforeVMIDs, slowStartWorldVMIDs(fixture.world))
			require.Equal(t, beforeDisks, slowStartWorldDiskIDs(fixture.world))
			require.Equal(t, 2, slowStartTargetSize(t, fixture.group))
			require.Equal(t, map[string]bool{fixture.spareVMID: false}, slowStartPowerOverrides(fixture.group))
			require.True(t, fixture.autoscaler.ClusterStateRegistry.HasNodeGroupStartedScaleUp(fixture.group.Id()))
			upcoming, _ = fixture.autoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
			require.Equal(t, 1, upcoming[fixture.group.Id()])
			currentSize, targetSize := fixture.autoscaler.ClusterStateRegistry.GetAutoscaledNodesCount()
			require.Equal(t, 1, currentSize)
			require.Equal(t, 2, targetSize)
			fixture.transport.setRetryCompletion("Succeeded")
			<-fixture.transport.retryResult
			synctest.Wait()

			t.Logf(
				"accepted failed Start timeline: terminal failure visible before iteration 2, cleanup retained and parked VM 1, later demand accepted a new Start of the same VM at %s",
				time.Now().Sub(acceptedAt.at),
			)
		})
	})

	t.Run("overlapping demand waits for cleanup then restarts retained VM", func(t *testing.T) {
		infra := integration.SetupInfrastructure(t)
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer synctestutils.TearDown(cancel)

			fixture := newSlowStartFixture(t, ctx, infra)
			require.False(t, fixture.group.enableFastDeleteOnFailedProvisioning)
			driver, acceptedAt := startAcceptedSlowStart(t, ctx, fixture, 8)
			addSlowStartLateDemand(t, ctx, infra, fixture)
			beforeVMIDs := slowStartWorldVMIDs(fixture.world)
			beforeDisks := slowStartWorldDiskIDs(fixture.world)

			fixture.transport.setCompletion("Failed")
			<-fixture.transport.terminal
			synctest.Wait()
			instances, err := fixture.group.Nodes(ctx)
			require.NoError(t, err)
			require.Len(t, instances, 2)
			require.Equal(t, cloudprovider.InstanceCreating, instances[1].Status.State)
			require.NotNil(t, instances[1].Status.ErrorInfo)
			require.Equal(t, "start-deallocated-failed", instances[1].Status.ErrorInfo.ErrorCode)
			addSlowStartAdditionalDemand(t, ctx, infra, fixture)

			cleanupAt := advanceSlowStartLoop(t, driver, time.Minute, 2)
			<-fixture.transport.deallocate
			require.Equal(t, []string{"start-accepted:1", "deallocate-accepted:1"}, fixture.world.history())
			require.Equal(t, beforeVMIDs, slowStartWorldVMIDs(fixture.world))
			require.Equal(t, beforeDisks, slowStartWorldDiskIDs(fixture.world))
			require.Equal(t, 1, slowStartTargetSize(t, fixture.group))
			require.Equal(t, map[string]bool{fixture.spareVMID: true}, slowStartSyntheticDeallocating(fixture.group))
			instances, err = fixture.group.Nodes(ctx)
			require.NoError(t, err)
			require.Len(t, instances, 2)
			require.Equal(t, cloudprovider.InstanceDeleting, instances[1].Status.State)
			upcoming, _ := fixture.autoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
			require.Zero(t, upcoming[fixture.group.Id()])

			fixture.transport.setDeallocateCompletion("Succeeded")
			<-fixture.transport.deallocated
			synctest.Wait()
			require.Equal(t, time.Minute+37*time.Second, cleanupAt.at.Sub(acceptedAt.at))
			require.Equal(t, []string{"start-accepted:1", "deallocate-accepted:1", "deallocate-complete:1"}, fixture.world.history())
			require.Equal(t, beforeVMIDs, slowStartWorldVMIDs(fixture.world))
			require.Equal(t, beforeDisks, slowStartWorldDiskIDs(fixture.world))
			require.Len(t, fixture.world.vms(), 2)
			checkParkingCounts(t, fixture.group, 2, 1, 1, 0)
			require.Empty(t, slowStartPowerOverrides(fixture.group))
			require.Empty(t, slowStartSyntheticDeallocating(fixture.group))
			require.False(t, fixture.autoscaler.ClusterStateRegistry.HasNodeGroupStartedScaleUp(fixture.group.Id()))
			require.True(t,
				fixture.autoscaler.ClusterStateRegistry.BackoffStatusForNodeGroup(ctx, fixture.group, time.Now()).IsBackedOff,
			)
			upcoming, _ = fixture.autoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
			require.Zero(t, upcoming[fixture.group.Id()])

			history := fixture.world.history()
			var retryAccepted slowStartLoopMoment
			for iteration := 3; iteration <= 8 && !slices.Contains(history, "start-accepted:1:retry"); iteration++ {
				retryAccepted = advanceSlowStartLoop(t, driver, time.Minute, iteration)
				history = fixture.world.history()
			}
			require.Equal(t, []string{
				"start-accepted:1",
				"deallocate-accepted:1",
				"deallocate-complete:1",
				"start-accepted:1:retry",
				"grow:3",
			}, history)
			require.False(t, retryAccepted.at.IsZero())
			require.Equal(t, []string{"vm-identity-0", "vm-identity-1", "vm-identity-2"}, slowStartWorldVMIDs(fixture.world))
			require.Equal(t, []string{"disk-identity-0", "disk-identity-1", "disk-identity-2"}, slowStartWorldDiskIDs(fixture.world))
			require.Len(t, fixture.world.vms(), 3)
			require.Equal(t, 3, slowStartTargetSize(t, fixture.group))
			require.Equal(t, map[string]bool{fixture.spareVMID: false}, slowStartPowerOverrides(fixture.group))
			require.True(t, fixture.autoscaler.ClusterStateRegistry.HasNodeGroupStartedScaleUp(fixture.group.Id()))
			require.False(t,
				fixture.autoscaler.ClusterStateRegistry.BackoffStatusForNodeGroup(ctx, fixture.group, time.Now()).IsBackedOff,
			)
			upcoming, _ = fixture.autoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
			require.Equal(t, 2, upcoming[fixture.group.Id()])

			trace, accepted := fixture.transport.snapshot()
			require.True(t, accepted)
			require.Equal(t, 3, slowStartCountMethod(trace, http.MethodPost))
			require.Equal(t, 2, slowStartCountPath(trace, slowStartActionPath))
			require.Equal(t, 1, slowStartCountPath(trace, slowDeallocateActionPath))
			fixture.transport.mu.Lock()
			require.Equal(t, "Failed", fixture.transport.completion)
			require.Equal(t, 2, fixture.transport.starts)
			fixture.transport.mu.Unlock()
			fixture.transport.setRetryCompletion("Succeeded")
			<-fixture.transport.retryResult
			synctest.Wait()
			t.Logf(
				"retention overlap timeline: cleanup parked VM 1 at %s, later demand accepted a distinct Start of VM 1 and one physical growth at %s",
				cleanupAt.at.Sub(acceptedAt.at),
				retryAccepted.at.Sub(acceptedAt.at),
			)
		})
	})
}

func TestProviderOnlyDeallocateAcceptedStartObservationTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		world := &parkingWorld{states: []string{vmPowerStateRunning, vmPowerStateDeallocated}}
		provider, group := newParkingProvider(t, world, fake.NewClientset(), 0, 2, true)
		transport := newNonterminalStartTransport(world)
		provider.azureManager.azClient.vmssPowerClient = newTestVMSSPowerClient(t, transport)

		require.NoError(t, group.IncreaseSize(context.Background(), 1))
		<-transport.firstPoll
		time.Sleep(asyncContextTimeout)
		synctest.Wait()

		trace, accepted := transport.snapshot()
		require.True(t, accepted)
		slowStartValidateTrace(t, trace, trace[0].at)
		require.Equal(t, 1, slowStartCountMethod(trace, http.MethodPost))
		require.Equal(t, 49, slowStartCountPath(trace, slowStartPollPath))
		require.Equal(t, 2, slowStartTargetSize(t, group))
		require.Equal(t, map[string]bool{"vm-identity-1": false}, slowStartPowerOverrides(group))
		require.Equal(t,
			[]string{vmPowerStateRunning, vmPowerStateStarting},
			slowStartWorldStates(world),
		)
		require.Equal(t, []string{"start-accepted:1"}, world.history())
	})
}

func TestProviderOnlyDeallocateForceSyntheticCleanupRetainsVM(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		world := &parkingWorld{
			states:       []string{vmPowerStateRunning, vmPowerStateDeallocated},
			provisioning: map[int]string{1: VMProvisioningStateFailed},
		}
		provider, group := newParkingProvider(t, world, fake.NewClientset(), 2, 2, true)
		transport := newNonterminalStartTransport(world)
		provider.azureManager.azClient.vmssPowerClient = newTestVMSSPowerClient(t, transport)
		beforeVMIDs := slowStartWorldVMIDs(world)
		beforeDisks := slowStartWorldDiskIDs(world)
		group.setPowerOverride(beforeVMIDs[1], false)
		synthetic := parkingNode(1, "synthetic", false)
		synthetic.UID = ""
		synthetic.Annotations = map[string]string{
			cloudprovider.FakeNodeReasonAnnotation: cloudprovider.FakeNodeCreateError,
		}

		require.NoError(t, group.ForceDeleteNodes(context.Background(), []*apiv1.Node{synthetic}))
		<-transport.deallocate
		require.Equal(t, 1, slowStartTargetSize(t, group))
		require.Equal(t, map[string]bool{beforeVMIDs[1]: true}, slowStartSyntheticDeallocating(group))
		instances, err := group.Nodes(context.Background())
		require.NoError(t, err)
		require.Len(t, instances, 2)
		require.Equal(t, cloudprovider.InstanceDeleting, instances[1].Status.State)

		transport.setDeallocateCompletion("Succeeded")
		<-transport.deallocated
		synctest.Wait()
		checkParkingCounts(t, group, 2, 1, 1, 0)
		require.Equal(t, beforeVMIDs, slowStartWorldVMIDs(world))
		require.Equal(t, beforeDisks, slowStartWorldDiskIDs(world))
		require.Equal(t, []string{"deallocate-accepted:1", "deallocate-complete:1"}, world.history())
	})
}

func TestProviderOnlyDeallocateSyntheticCleanupObservationTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		world := &parkingWorld{
			states:       []string{vmPowerStateRunning, vmPowerStateDeallocated},
			provisioning: map[int]string{1: VMProvisioningStateFailed},
		}
		provider, group := newParkingProvider(t, world, fake.NewClientset(), 0, 2, true)
		transport := newNonterminalStartTransport(world)
		provider.azureManager.azClient.vmssPowerClient = newTestVMSSPowerClient(t, transport)
		vmID := *world.vms()[1].Properties.VMID
		group.setPowerOverride(vmID, false)
		synthetic := parkingNode(1, "synthetic", false)
		synthetic.UID = ""
		synthetic.Annotations = map[string]string{
			cloudprovider.FakeNodeReasonAnnotation: cloudprovider.FakeNodeCreateError,
		}

		require.NoError(t, group.ForceDeleteNodes(context.Background(), []*apiv1.Node{synthetic}))
		<-transport.deallocate
		time.Sleep(asyncContextTimeout)
		synctest.Wait()

		require.Equal(t, 1, slowStartTargetSize(t, group))
		require.Equal(t, map[string]bool{vmID: false}, slowStartPowerOverrides(group))
		require.Equal(t, map[string]bool{vmID: true}, slowStartSyntheticDeallocating(group))
		instances, err := group.Nodes(context.Background())
		require.NoError(t, err)
		require.Len(t, instances, 2)
		require.Equal(t, cloudprovider.InstanceDeleting, instances[1].Status.State)
		require.Equal(t, []string{"deallocate-accepted:1"}, world.history())
		trace, _ := transport.snapshot()
		require.Equal(t, 1, slowStartCountPath(trace, slowDeallocateActionPath))
		require.Equal(t, 49, slowStartCountPath(trace, slowDeallocatePollPath))
	})
}

func TestProviderOnlyDeallocateSyntheticCleanupRejectsRegisteredNode(t *testing.T) {
	world := &parkingWorld{
		states:       []string{vmPowerStateRunning, vmPowerStateDeallocated},
		provisioning: map[int]string{1: VMProvisioningStateFailed},
	}
	client := fake.NewClientset()
	provider, group := newParkingProvider(t, world, client, 0, 2, true)
	transport := newNonterminalStartTransport(world)
	provider.azureManager.azClient.vmssPowerClient = newTestVMSSPowerClient(t, transport)
	_, err := client.CoreV1().Nodes().Create(t.Context(), parkingNode(1, "registered", true), metav1.CreateOptions{})
	require.NoError(t, err)
	synthetic := parkingNode(1, "synthetic", false)
	synthetic.UID = ""
	synthetic.Annotations = map[string]string{
		cloudprovider.FakeNodeReasonAnnotation: cloudprovider.FakeNodeCreateError,
	}

	err = group.ForceDeleteNodes(t.Context(), []*apiv1.Node{synthetic})

	require.ErrorContains(t, err, "still has Node node-1")
	trace, _ := transport.snapshot()
	require.Empty(t, trace)
	require.Equal(t, []string{"vm-identity-0", "vm-identity-1"}, slowStartWorldVMIDs(world))
}

func TestProviderOnlyDeallocateSyntheticCleanupObserverOwnership(t *testing.T) {
	world := &parkingWorld{states: []string{vmPowerStateDeallocated}}
	_, group := newParkingProvider(t, world, fake.NewClientset(), 0, 1, true)
	vmID := *world.vms()[0].Properties.VMID
	group.setPowerOverride(vmID, false)
	oldToken := group.setSyntheticDeallocating(vmID)

	_, _, _, err := group.parkingInventory()
	require.NoError(t, err)
	require.Empty(t, slowStartSyntheticDeallocating(group))
	group.setPowerOverride(vmID, false)

	require.False(t, group.completeSyntheticDeallocate(vmID, oldToken))
	require.Equal(t, map[string]bool{vmID: false}, slowStartPowerOverrides(group))

	newToken := group.setSyntheticDeallocating(vmID)
	require.NotEqual(t, oldToken, newToken)
	require.False(t, group.clearSyntheticDeallocating(vmID, oldToken))
	require.Equal(t, map[string]bool{vmID: true}, slowStartSyntheticDeallocating(group))
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

func slowStartSyntheticDeallocating(group *ScaleSet) map[string]bool {
	group.powerMutex.Lock()
	defer group.powerMutex.Unlock()
	result := make(map[string]bool, len(group.syntheticDeallocating))
	for vmID := range group.syntheticDeallocating {
		result[vmID] = true
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

func slowStartWorldDiskIDs(world *parkingWorld) []string {
	vms := world.vms()
	result := make([]string, 0, len(vms))
	for _, vm := range vms {
		result = append(result, *vm.Properties.StorageProfile.OSDisk.ManagedDisk.ID)
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

func slowStartRequireDemandFitsReturningWorker(
	t *testing.T,
	ctx context.Context,
	client kubernetes.Interface,
	fixture *slowStartFixture,
) {
	t.Helper()
	template, err := fixture.group.TemplateNodeInfo(ctx)
	require.NoError(t, err)
	returning := template.DeepCopy()
	returning.Node().Name = "expected-returning-worker"

	fixture.autoscaler.ClusterSnapshot.Fork()
	defer fixture.autoscaler.ClusterSnapshot.Revert()
	require.NoError(t, fixture.autoscaler.ClusterSnapshot.AddNodeInfo(returning))
	for _, name := range fixture.demandPods {
		pod, err := client.CoreV1().Pods("default").Get(ctx, name, metav1.GetOptions{})
		require.NoError(t, err)
		candidate := pod.DeepCopy()
		candidate.Spec.NodeName = ""
		require.NoError(t, fixture.autoscaler.ClusterSnapshot.SchedulePod(candidate, returning.Node().Name))
	}
}

func addSlowStartAdditionalDemand(
	t *testing.T,
	ctx context.Context,
	infra *integration.TestInfrastructure,
	fixture *slowStartFixture,
) {
	t.Helper()
	additional := catest.BuildTestPod("overlapping-additional-demand", 2000, 100, catest.MarkUnschedulable())
	additional.Spec.NodeSelector = map[string]string{"pool": "workers"}
	_, err := infra.Fakes.KubeClient.CoreV1().Pods(additional.Namespace).Create(ctx, additional, metav1.CreateOptions{})
	require.NoError(t, err)
	synctest.Wait()

	template, err := fixture.group.TemplateNodeInfo(ctx)
	require.NoError(t, err)
	first := template.DeepCopy()
	first.Node().Name = "first-expected-worker"
	second := template.DeepCopy()
	second.Node().Name = "required-second-worker"

	fixture.autoscaler.ClusterSnapshot.Fork()
	defer fixture.autoscaler.ClusterSnapshot.Revert()
	require.NoError(t, fixture.autoscaler.ClusterSnapshot.AddNodeInfo(first))
	for _, name := range fixture.demandPods {
		pod, err := infra.Fakes.KubeClient.CoreV1().Pods("default").Get(ctx, name, metav1.GetOptions{})
		require.NoError(t, err)
		candidate := pod.DeepCopy()
		candidate.Name += "-overlap-fit"
		candidate.Spec.NodeName = ""
		require.NoError(t, fixture.autoscaler.ClusterSnapshot.SchedulePod(candidate, first.Node().Name))
	}
	candidate := additional.DeepCopy()
	candidate.Name += "-overlap-fit"
	require.Error(t, fixture.autoscaler.ClusterSnapshot.SchedulePod(candidate, first.Node().Name))
	require.NoError(t, fixture.autoscaler.ClusterSnapshot.AddNodeInfo(second))
	require.NoError(t, fixture.autoscaler.ClusterSnapshot.SchedulePod(candidate, second.Node().Name))

	fixture.demandPods = append(fixture.demandPods, additional.Name)
	slowStartRequirePendingPods(t, ctx, infra.Fakes.KubeClient, fixture.demandPods)
}

func slowStartValidateTrace(t *testing.T, trace []slowStartTrace, start time.Time) {
	t.Helper()
	require.GreaterOrEqual(t, len(trace), 2)
	require.Equal(t, slowStartTrace{at: start, method: http.MethodPost, path: slowStartActionPath}, trace[0])
	var lastPoll time.Time
	for i, request := range trace[1:] {
		require.Equal(t, http.MethodGet, request.method)
		require.False(t, request.at.Before(start))
		if request.path == slowStartResultPath {
			require.Equal(t, len(trace)-2, i)
			require.Equal(t, lastPoll, request.at)
			continue
		}
		require.Equal(t, slowStartPollPath, request.path)
		if lastPoll.IsZero() {
			require.Equal(t, start, request.at)
		} else {
			require.Equal(t, slowStartPollInterval, request.at.Sub(lastPoll))
		}
		lastPoll = request.at
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

func slowStartCountPath(trace []slowStartTrace, path string) int {
	count := 0
	for _, request := range trace {
		if request.path == path {
			count++
		}
	}
	return count
}

var _ policy.Transporter = (*nonterminalStartTransport)(nil)
