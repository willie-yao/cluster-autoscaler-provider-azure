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
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	azcorepolicy "github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/stretchr/testify/require"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-autoscaler/pkg/cloudprovider"
	"sigs.k8s.io/cluster-autoscaler/pkg/core"
	"sigs.k8s.io/cluster-autoscaler/pkg/loop"
	"sigs.k8s.io/cluster-autoscaler/pkg/metrics"
	"sigs.k8s.io/cluster-autoscaler/pkg/test/integration"
	synctestutils "sigs.k8s.io/cluster-autoscaler/pkg/test/integration/synctest"
	catest "sigs.k8s.io/cluster-autoscaler/pkg/utils/test"
)

const (
	survivingStartAPath  = "/operations/surviving-start-a"
	survivingStartBPath  = "/operations/surviving-start-b"
	survivingCleanupPath = "/operations/surviving-cleanup-a"
)

type survivingRequestTransport struct {
	mu                sync.Mutex
	world             *parkingWorld
	trace             []slowStartTrace
	failA             bool
	cleanupCompletion string
	startAPolled      chan struct{}
	startBPolled      chan struct{}
	aFailed           chan struct{}
	cleanupAccepted   chan struct{}
	cleanupDone       chan struct{}
	cleanupResult     chan struct{}
	startAOnce        sync.Once
	startBOnce        sync.Once
	aFailedOnce       sync.Once
	cleanupOnce       sync.Once
	cleanupDoneOnce   sync.Once
	cleanupResultOnce sync.Once
}

func newSurvivingRequestTransport(world *parkingWorld) *survivingRequestTransport {
	return &survivingRequestTransport{
		world:             world,
		cleanupCompletion: "InProgress",
		startAPolled:      make(chan struct{}),
		startBPolled:      make(chan struct{}),
		aFailed:           make(chan struct{}),
		cleanupAccepted:   make(chan struct{}),
		cleanupDone:       make(chan struct{}),
		cleanupResult:     make(chan struct{}),
	}
}

func (t *survivingRequestTransport) setAFailed() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failA = true
}

func (t *survivingRequestTransport) setCleanupCompletion(status string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cleanupCompletion = status
}

func (t *survivingRequestTransport) snapshot() ([]slowStartTrace, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]slowStartTrace(nil), t.trace...), t.failA
}

func (t *survivingRequestTransport) Do(request *http.Request) (*http.Response, error) {
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.trace = append(t.trace, slowStartTrace{at: time.Now(), method: request.Method, path: request.URL.Path})
	t.mu.Unlock()

	switch {
	case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/start"):
		parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
		instanceID := parts[len(parts)-2]
		instance := 0
		pollPath := ""
		event := ""
		switch instanceID {
		case "1":
			instance = 1
			pollPath = survivingStartAPath
			event = "start-a-accepted:1"
		case "2":
			instance = 2
			pollPath = survivingStartBPath
			event = "start-b-accepted:2"
		default:
			return nil, fmt.Errorf("unexpected surviving-request Start instance %s", instanceID)
		}
		t.world.mu.Lock()
		t.world.states[instance] = vmPowerStateStarting
		t.world.mu.Unlock()
		t.world.record(event)
		return survivingAcceptedResponse(request, pollPath), nil

	case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/deallocate"):
		if request.URL.Path != strings.TrimSuffix(slowStartActionPath, "/start")+"/deallocate" {
			return nil, fmt.Errorf("unexpected surviving-request Deallocate path %s", request.URL.Path)
		}
		t.world.mu.Lock()
		t.world.states[1] = vmPowerStateDeallocating
		t.world.mu.Unlock()
		t.world.record("cleanup-a-accepted:1")
		t.cleanupOnce.Do(func() { close(t.cleanupAccepted) })
		return survivingAcceptedResponse(request, survivingCleanupPath), nil

	case request.Method == http.MethodGet && request.URL.Path == survivingStartAPath:
		t.startAOnce.Do(func() { close(t.startAPolled) })
		t.mu.Lock()
		failed := t.failA
		t.mu.Unlock()
		if !failed {
			return survivingPollingResponse(request, `{"status":"InProgress"}`), nil
		}
		t.world.mu.Lock()
		if t.world.provisioning == nil {
			t.world.provisioning = make(map[int]string)
		}
		t.world.states[1] = vmPowerStateDeallocated
		t.world.provisioning[1] = VMProvisioningStateFailed
		t.world.mu.Unlock()
		t.aFailedOnce.Do(func() { close(t.aFailed) })
		return survivingPollingResponse(
			request,
			`{"status":"Failed","error":{"code":"StartAFailed","message":"controlled terminal failure for A"}}`,
		), nil

	case request.Method == http.MethodGet && request.URL.Path == survivingStartBPath:
		t.startBOnce.Do(func() { close(t.startBPolled) })
		return survivingPollingResponse(request, `{"status":"InProgress"}`), nil

	case request.Method == http.MethodGet && request.URL.Path == survivingCleanupPath:
		t.mu.Lock()
		completion := t.cleanupCompletion
		t.mu.Unlock()
		if completion == "Succeeded" {
			t.world.mu.Lock()
			t.world.states[1] = vmPowerStateDeallocated
			t.world.provisioning[1] = provisioningStateSucceeded
			t.world.mu.Unlock()
			t.cleanupDoneOnce.Do(func() {
				t.world.record("cleanup-a-complete:1")
				close(t.cleanupDone)
			})
		}
		return survivingPollingResponse(request, fmt.Sprintf(`{"status":%q}`, completion)), nil

	case request.Method == http.MethodGet && request.URL.Path == survivingCleanupPath+"/result":
		t.cleanupResultOnce.Do(func() { close(t.cleanupResult) })
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    request,
		}, nil

	default:
		return nil, fmt.Errorf("unexpected surviving-request operation %s %s", request.Method, request.URL.Path)
	}
}

func survivingAcceptedResponse(request *http.Request, pollPath string) *http.Response {
	header := make(http.Header)
	header.Set("Azure-AsyncOperation", "https://management.test"+pollPath)
	header.Set("Content-Type", "application/json")
	header.Set("Location", "https://management.test"+pollPath+"/result")
	return &http.Response{
		StatusCode: http.StatusAccepted,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(`{}`)),
		Request:    request,
	}
}

func survivingPollingResponse(request *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Retry-After":  []string{"37"},
		},
		Body:    io.NopCloser(strings.NewReader(body)),
		Request: request,
	}
}

func TestProviderOnlyDeallocateSurvivingRequestAccounting(t *testing.T) {
	infra := integration.SetupInfrastructure(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer synctestutils.TearDown(cancel)

		world := &parkingWorld{
			states: []string{
				vmPowerStateRunning,
				vmPowerStateDeallocated,
				vmPowerStateDeallocated,
			},
		}
		client := infra.Fakes.KubeClient
		provider, group := newParkingProvider(t, world, client, 1, 3, true)
		transport := newSurvivingRequestTransport(world)
		provider.azureManager.azClient.vmssPowerClient = newTestVMSSPowerClient(t, transport)
		beforeVMIDs := slowStartWorldVMIDs(world)
		beforeDisks := slowStartWorldDiskIDs(world)

		node := parkingNode(0, "active", true)
		_, err := client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
		require.NoError(t, err)
		busy := catest.BuildScheduledTestPod("busy", 3500, 100, node.Name)
		_, err = client.CoreV1().Pods(busy.Namespace).Create(ctx, busy, metav1.CreateOptions{})
		require.NoError(t, err)
		autoscaler := slowStartAutoscaler(t, ctx, infra, provider)
		health := metrics.NewHealthCheck(stockMaxInactivity, stockMaxFailingTime, stockMaxStartupTime)
		health.StartMonitoring()
		loop.RunAutoscalerOnce(ctx, autoscaler, health, time.Now(), 0)

		demandA := catest.BuildTestPod("demand-a", 3000, 100, catest.MarkUnschedulable())
		demandA.Spec.NodeSelector = map[string]string{"pool": "workers"}
		demandB := catest.BuildTestPod("demand-b", 3000, 100, catest.MarkUnschedulable())
		demandB.Spec.NodeSelector = map[string]string{"pool": "workers"}
		for _, pod := range []*apiv1.Pod{demandA, demandB} {
			_, err = client.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
			require.NoError(t, err)
		}
		synctest.Wait()
		requireSlowStartTwoWorkerDemand(t, ctx, autoscaler, group, demandA, demandB)

		driver := startSlowStartLoopDriver(ctx, autoscaler, health, 1, 12)
		initialStarted := <-driver.started
		initialReturned := <-driver.returned
		require.Equal(t, initialStarted.at, initialReturned.at)
		synctest.Wait()
		<-transport.startAPolled
		<-transport.startBPolled

		require.Equal(t, []string{"start-a-accepted:1", "start-b-accepted:2"}, world.history())
		require.Equal(t, beforeVMIDs, slowStartWorldVMIDs(world))
		require.Equal(t, beforeDisks, slowStartWorldDiskIDs(world))
		require.Len(t, world.vms(), 3)
		require.Equal(t, 3, slowStartTargetSize(t, group))
		require.Equal(t, map[string]bool{
			beforeVMIDs[1]: false,
			beforeVMIDs[2]: false,
		}, slowStartPowerOverrides(group))
		require.True(t, autoscaler.ClusterStateRegistry.HasNodeGroupStartedScaleUp(group.Id()))
		requestTime, err := autoscaler.ClusterStateRegistry.NodeGroupScaleUpTime(group)
		require.NoError(t, err)
		require.Equal(t, initialStarted.at, requestTime)
		upcoming, _ := autoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
		require.Equal(t, 2, upcoming[group.Id()])
		currentSize, targetSize := autoscaler.ClusterStateRegistry.GetAutoscaledNodesCount()
		require.Equal(t, 1, currentSize)
		require.Equal(t, 3, targetSize)

		transport.setAFailed()
		<-transport.aFailed
		synctest.Wait()
		instances, err := group.Nodes(ctx)
		require.NoError(t, err)
		require.Len(t, instances, 3)
		require.Equal(t, cloudprovider.InstanceCreating, instances[1].Status.State)
		require.NotNil(t, instances[1].Status.ErrorInfo)
		require.Equal(t, "start-deallocated-failed", instances[1].Status.ErrorInfo.ErrorCode)
		require.Equal(t, cloudprovider.InstanceCreating, instances[2].Status.State)
		require.Nil(t, instances[2].Status.ErrorInfo)

		failureLoop := advanceSlowStartLoop(t, driver, time.Minute, 2)
		<-transport.cleanupAccepted
		require.Equal(t, time.Minute+37*time.Second, failureLoop.at.Sub(initialStarted.at))
		require.Equal(t, []string{
			"start-a-accepted:1",
			"start-b-accepted:2",
			"cleanup-a-accepted:1",
		}, world.history())
		failures := autoscaler.ClusterStateRegistry.GetScaleUpFailures()
		require.Len(t, failures[group.Id()], 1)
		require.Equal(t, 1, failures[group.Id()][0].Delta)
		require.Equal(t, "start-deallocated-failed", failures[group.Id()][0].ErrorInfo.ErrorCode)
		require.True(t, autoscaler.ClusterStateRegistry.HasNodeGroupStartedScaleUp(group.Id()))
		requestTime, err = autoscaler.ClusterStateRegistry.NodeGroupScaleUpTime(group)
		require.NoError(t, err)
		require.Equal(t, initialStarted.at, requestTime)
		require.True(t, autoscaler.ClusterStateRegistry.BackoffStatusForNodeGroup(ctx, group, time.Now()).IsBackedOff)
		upcoming, _ = autoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
		require.Zero(t, upcoming[group.Id()])
		require.Equal(t, 2, slowStartTargetSize(t, group))
		require.Equal(t, map[string]bool{beforeVMIDs[1]: true}, slowStartSyntheticDeallocating(group))
		instances, err = group.Nodes(ctx)
		require.NoError(t, err)
		require.Equal(t, cloudprovider.InstanceDeleting, instances[1].Status.State)
		require.Equal(t, cloudprovider.InstanceCreating, instances[2].Status.State)
		traceAfterFailure, failed := transport.snapshot()
		require.True(t, failed)
		require.Equal(t, 3, slowStartCountMethod(traceAfterFailure, http.MethodPost))
		postsAfterFailure := survivingPostPaths(traceAfterFailure)

		for iteration := 3; iteration <= 7; iteration++ {
			advanceSlowStartLoop(t, driver, time.Minute, iteration)
			require.Equal(t, []string{
				"start-a-accepted:1",
				"start-b-accepted:2",
				"cleanup-a-accepted:1",
			}, world.history())
		}

		require.Equal(t, 6*time.Minute+37*time.Second, time.Now().Sub(initialStarted.at))
		time.Sleep(time.Nanosecond)
		require.True(t, time.Now().Before(requestTime.Add(slowStartMaxNodeProvisionTime)))
		require.False(t, autoscaler.ClusterStateRegistry.BackoffStatusForNodeGroup(ctx, group, time.Now()).IsBackedOff)
		require.True(t, autoscaler.ClusterStateRegistry.HasNodeGroupStartedScaleUp(group.Id()))
		requestTimeAfterBackoff, err := autoscaler.ClusterStateRegistry.NodeGroupScaleUpTime(group)
		require.NoError(t, err)
		require.Equal(t, requestTime, requestTimeAfterBackoff)
		upcoming, _ = autoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
		const expectedSurvivingIncoming = 1
		require.Equal(t, expectedSurvivingIncoming, upcoming[group.Id()])
		require.Equal(t, 2, slowStartTargetSize(t, group))
		require.Len(t, world.vms(), 3)
		require.Equal(t, beforeVMIDs, slowStartWorldVMIDs(world))
		require.Equal(t, beforeDisks, slowStartWorldDiskIDs(world))
		require.Equal(t, map[string]bool{beforeVMIDs[1]: true}, slowStartSyntheticDeallocating(group))
		instances, err = group.Nodes(ctx)
		require.NoError(t, err)
		require.Equal(t, cloudprovider.InstanceDeleting, instances[1].Status.State)
		require.Equal(t, cloudprovider.InstanceCreating, instances[2].Status.State)
		statusCode, body := slowStartHealthStatus(health)
		require.Equal(t, http.StatusOK, statusCode)
		require.Equal(t, "OK", body)

		oneIncomingTrace, failed := transport.snapshot()
		require.True(t, failed)
		require.Equal(t, postsAfterFailure, survivingPostPaths(oneIncomingTrace), "one-incoming checkpoint must precede any new accepted action")
		require.Equal(t, 2, survivingCountAction(oneIncomingTrace, "/start"))
		require.Equal(t, 1, slowStartCountPath(oneIncomingTrace, slowDeallocateActionPath))

		t.Logf(
			"surviving request checkpoint: initial request accepted 2 at 0s, A failed and cleanup was accepted at %s, post-backoff incoming=%d at %s",
			failureLoop.at.Sub(initialStarted.at),
			upcoming[group.Id()],
			time.Now().Sub(initialStarted.at),
		)

		cAccepted := advanceSlowStartLoop(t, driver, 0, 8)
		require.Equal(t, []string{
			"start-a-accepted:1",
			"start-b-accepted:2",
			"cleanup-a-accepted:1",
			"grow:4",
		}, world.history())
		require.Len(t, world.vms(), 4)
		require.Equal(t, []string{"vm-identity-0", "vm-identity-1", "vm-identity-2", "vm-identity-3"}, slowStartWorldVMIDs(world))
		require.Equal(t, []string{"disk-identity-0", "disk-identity-1", "disk-identity-2", "disk-identity-3"}, slowStartWorldDiskIDs(world))
		require.Equal(t, 3, slowStartTargetSize(t, group))
		require.True(t, autoscaler.ClusterStateRegistry.HasNodeGroupStartedScaleUp(group.Id()))
		requestTimeAfterC, err := autoscaler.ClusterStateRegistry.NodeGroupScaleUpTime(group)
		require.NoError(t, err)
		require.Equal(t, cAccepted.at, requestTimeAfterC)
		require.False(t, autoscaler.ClusterStateRegistry.BackoffStatusForNodeGroup(ctx, group, time.Now()).IsBackedOff)
		upcoming, _ = autoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
		require.Equal(t, 2, upcoming[group.Id()])
		traceAfterC, failed := transport.snapshot()
		require.True(t, failed)
		require.Equal(t, postsAfterFailure, survivingPostPaths(traceAfterC), "C is physical growth, not another power operation")
		require.Equal(t, 2, survivingCountAction(traceAfterC, "/start"), "cleanup VM A must not be started before parking")

		transport.setCleanupCompletion("Succeeded")
		<-transport.cleanupDone
		<-transport.cleanupResult
		synctest.Wait()
		require.Equal(t, []string{
			"start-a-accepted:1",
			"start-b-accepted:2",
			"cleanup-a-accepted:1",
			"grow:4",
			"cleanup-a-complete:1",
		}, world.history())
		checkParkingCounts(t, group, 4, 3, 1, 1)
		require.Equal(t, 3, slowStartTargetSize(t, group), "accepted cleanup and confirmed parking exclude A exactly once")
		require.Empty(t, slowStartSyntheticDeallocating(group))
		require.Equal(t, map[string]bool{beforeVMIDs[2]: false}, slowStartPowerOverrides(group))
		upcoming, _ = autoscaler.ClusterStateRegistry.GetUpcomingNodes(ctx)
		require.Equal(t, 2, upcoming[group.Id()])
		finalTrace, failed := transport.snapshot()
		require.True(t, failed)
		require.Equal(t, postsAfterFailure, survivingPostPaths(finalTrace))
		require.Equal(t, 2, survivingCountAction(finalTrace, "/start"))
		require.Equal(t, 1, slowStartCountPath(finalTrace, slowDeallocateActionPath))

		t.Logf(
			"adjusted-target timeline: C accepted at %s with physical=4 target=3 incoming=2; cleanup completion kept target=3",
			cAccepted.at.Sub(initialStarted.at),
		)

		time.Sleep(asyncContextTimeout)
		synctest.Wait()
	})
}

func survivingPostPaths(trace []slowStartTrace) []string {
	var result []string
	for _, request := range trace {
		if request.method == http.MethodPost {
			result = append(result, request.path)
		}
	}
	return result
}

func survivingCountAction(trace []slowStartTrace, suffix string) int {
	count := 0
	for _, path := range survivingPostPaths(trace) {
		if strings.HasSuffix(path, suffix) {
			count++
		}
	}
	return count
}

func requireSlowStartTwoWorkerDemand(
	t *testing.T,
	ctx context.Context,
	autoscaler *core.StaticAutoscaler,
	group *ScaleSet,
	firstPod *apiv1.Pod,
	secondPod *apiv1.Pod,
) {
	t.Helper()
	template, err := group.TemplateNodeInfo(ctx)
	require.NoError(t, err)
	firstWorker := template.DeepCopy()
	firstWorker.Node().Name = "required-worker-a"
	secondWorker := template.DeepCopy()
	secondWorker.Node().Name = "required-worker-b"

	autoscaler.ClusterSnapshot.Fork()
	defer autoscaler.ClusterSnapshot.Revert()
	require.NoError(t, autoscaler.ClusterSnapshot.AddNodeInfo(firstWorker))
	firstCandidate := firstPod.DeepCopy()
	firstCandidate.Name += "-fit"
	require.NoError(t, autoscaler.ClusterSnapshot.SchedulePod(firstCandidate, firstWorker.Node().Name))
	secondCandidate := secondPod.DeepCopy()
	secondCandidate.Name += "-fit"
	require.Error(t, autoscaler.ClusterSnapshot.SchedulePod(secondCandidate, firstWorker.Node().Name))
	require.NoError(t, autoscaler.ClusterSnapshot.AddNodeInfo(secondWorker))
	require.NoError(t, autoscaler.ClusterSnapshot.SchedulePod(secondCandidate, secondWorker.Node().Name))
}

var _ azcorepolicy.Transporter = (*survivingRequestTransport)(nil)
