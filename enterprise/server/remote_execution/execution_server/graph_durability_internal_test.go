package execution_server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/buildbuddy-io/buildbuddy/enterprise/server/testutil/testredis"
	graphpb "github.com/buildbuddy-io/buildbuddy/proto/graph_execution"
	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
	rspb "github.com/buildbuddy-io/buildbuddy/proto/resource"
	scpb "github.com/buildbuddy-io/buildbuddy/proto/scheduler"
	"github.com/buildbuddy-io/buildbuddy/server/remote_cache/digest"
	"github.com/buildbuddy-io/buildbuddy/server/util/proto"
	"github.com/buildbuddy-io/buildbuddy/server/util/status"
	"github.com/go-redis/redis/v8"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	gstatus "google.golang.org/grpc/status"
)

type durabilityGraphStream struct {
	graphpb.GraphExecution_GraphExecuteServer
	ctx  context.Context
	recv func() (*graphpb.GraphExecuteRequest, error)
	sent []*graphpb.GraphExecuteResponse
}

type graphMissingBlobFinderFunc func(
	context.Context, []*rspb.ResourceName,
) ([]*repb.Digest, error)

func (f graphMissingBlobFinderFunc) FindMissing(
	ctx context.Context, resources []*rspb.ResourceName,
) ([]*repb.Digest, error) {
	return f(ctx, resources)
}

func (s *durabilityGraphStream) Send(response *graphpb.GraphExecuteResponse) error {
	s.sent = append(s.sent, proto.Clone(response).(*graphpb.GraphExecuteResponse))
	return nil
}

func (s *durabilityGraphStream) Context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

func (s *durabilityGraphStream) Recv() (*graphpb.GraphExecuteRequest, error) {
	if s.recv != nil {
		return s.recv()
	}
	<-s.Context().Done()
	return nil, s.Context().Err()
}

func durableTestGraph(t *testing.T, s *ExecutionServer, owner, session string, token []byte) *graphExecution {
	t.Helper()
	g := newGraphExecution(s, nil, context.Background(), owner)
	g.sessionID = session
	g.resumeToken = append([]byte(nil), token...)
	g.durableKey = graphDurableSessionKey(owner, session)
	g.durableLeaseKey = g.durableKey + ":lease"
	g.durableTenantIndexKey = graphDurableTenantIndexKey(owner)
	return g
}

func createDurableTestGraphSession(
	t *testing.T, g *graphExecution, begin *graphpb.BeginGraph,
) (bool, error) {
	t.Helper()
	request := &graphpb.GraphExecuteRequest{
		SessionId:      g.sessionID,
		SequenceNumber: 1,
		Payload:        &graphpb.GraphExecuteRequest_Begin{Begin: begin},
	}
	data, err := proto.Marshal(request)
	require.NoError(t, err)
	return g.createDurableGraphSession(begin, data)
}

func TestDurableGraphLeaseExpiryTakeoverFencesOldCoordinator(t *testing.T) {
	r := testredis.Start(t)
	s1 := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
	s2 := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
	token := []byte(strings.Repeat("t", 32))
	g1 := durableTestGraph(t, s1, "tenant-a", "session-a", token)
	created, err := createDurableTestGraphSession(t, g1, &graphpb.BeginGraph{ResumeToken: token})
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, g1.acquireDurableLease())
	_, err = g1.appendDurableRequest(2, []byte("request-two"))
	require.NoError(t, err)

	// Simulate abrupt app loss. The old lease is not released; it expires.
	require.NoError(t, r.Client().PExpire(context.Background(), g1.durableLeaseKey, 20*time.Millisecond).Err())
	require.Eventually(t, func() bool {
		return r.Client().Exists(context.Background(), g1.durableLeaseKey).Val() == 0
	}, time.Second, 5*time.Millisecond)

	g2 := durableTestGraph(t, s2, "tenant-a", "session-a", token)
	require.NoError(t, g2.acquireDurableLease())
	_, err = g1.appendDurableRequest(3, []byte("stale-owner"))
	require.Error(t, err)
	require.True(t, status.IsAbortedError(err))

	data, err := g2.loadDurableGraph()
	require.NoError(t, err)
	require.Equal(t, []byte("request-two"), data.requests[2])

	prepared := &scpb.EnsureTaskRequest{
		TaskId:         "stable-execution-id",
		Metadata:       &scpb.SchedulingMetadata{TaskSize: &scpb.TaskSize{EstimatedMilliCpu: 1}},
		SerializedTask: []byte("exact-task"),
	}
	require.NoError(t, g2.storeDurablePreparedTask("node", prepared))
	data, err = g2.loadDurableGraph()
	require.NoError(t, err)
	replayed := &scpb.EnsureTaskRequest{}
	require.NoError(t, proto.Unmarshal(data.taskData["node"], replayed))
	preparedData, err := (proto.MarshalOptions{Deterministic: true}).Marshal(prepared)
	require.NoError(t, err)
	replayedData, err := (proto.MarshalOptions{Deterministic: true}).Marshal(replayed)
	require.NoError(t, err)
	require.Equal(t, preparedData, replayedData)
}

func TestDurableGraphStaleLocalCoordinatorMustReconstructBeforeReacquire(t *testing.T) {
	r := testredis.Start(t)
	s1 := &ExecutionServer{
		rdb:           r.Client(),
		clock:         clockwork.NewRealClock(),
		graphSessions: make(map[string]*graphExecution),
	}
	s2 := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
	token := []byte(strings.Repeat("s", 32))
	g1 := durableTestGraph(t, s1, "tenant-stale", "session-stale", token)
	created, err := createDurableTestGraphSession(t, g1, &graphpb.BeginGraph{ResumeToken: token})
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, g1.acquireDurableLease())
	s1.graphSessions[graphSessionKey(g1.ownerPrefix, g1.sessionID)] = g1

	require.NoError(t, r.Client().Del(context.Background(), g1.durableLeaseKey).Err())
	g2 := durableTestGraph(t, s2, g1.ownerPrefix, g1.sessionID, token)
	require.NoError(t, g2.acquireDurableLease())
	g2.releaseDurableLease(context.Background())

	err = g1.serveResumedGraph(nil, &graphpb.GraphExecuteRequest{
		SessionId: g1.sessionID,
		Payload: &graphpb.GraphExecuteRequest_Resume{Resume: &graphpb.ResumeGraph{
			SessionId:   g1.sessionID,
			ResumeToken: token,
		}},
	}, 0)
	var transitionErr *graphLeaseTransitionError
	require.ErrorAs(t, err, &transitionErr)
	require.Zero(t, r.Client().Exists(context.Background(), g1.durableLeaseKey).Val(),
		"stale local state must not reacquire a new lease")
	require.NotContains(t, s1.graphSessions, graphSessionKey(g1.ownerPrefix, g1.sessionID))

	g3 := durableTestGraph(t, s1, g1.ownerPrefix, g1.sessionID, token)
	data, err := g3.loadDurableGraphForRecovery()
	require.NoError(t, err)
	require.False(t, data.finished)
	require.Greater(t, g3.durableLeaseEpoch, g1.durableLeaseEpoch)
	require.NoError(t, g3.stopRecoveryHeartbeat(false))
	g3.releaseDurableLease(context.Background())
}

func TestDurableGraphRecoveryReloadsFinishedSessionAfterLeaseAcquireRace(t *testing.T) {
	r := testredis.Start(t)
	s := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
	token := []byte(strings.Repeat("w", 32))
	original := durableTestGraph(t, s, "race-tenant", "race-session", token)
	_, err := createDurableTestGraphSession(t, original, &graphpb.BeginGraph{ResumeToken: token})
	require.NoError(t, err)
	require.NoError(t, original.acquireDurableLease())
	terminal := &graphpb.GraphExecuteResponse{
		SequenceNumber: 1,
		Payload: &graphpb.GraphExecuteResponse_Error{Error: &graphpb.GraphStreamError{
			Terminal: true,
		}},
	}
	terminalData, err := proto.Marshal(terminal)
	require.NoError(t, err)
	require.NoError(t, original.appendDurableResponse(terminal, terminalData))

	recovered := durableTestGraph(t, s, original.ownerPrefix, original.sessionID, token)
	hookCalls := 0
	recovered.beforeRecoveryLeaseAcquireForTesting = func() {
		hookCalls++
		require.NoError(t, original.finishDurableSession())
	}
	data, err := recovered.loadDurableGraphForRecovery()
	require.NoError(t, err)
	require.Equal(t, 1, hookCalls)
	require.True(t, data.finished)
	require.True(t, recovered.isDurableFinished())
	require.Empty(t, recovered.currentDurableLeaseValue())
	require.NoError(t, recovered.reconstructDurableGraph(data))
	require.True(t, recovered.finished.Load())
	require.Len(t, recovered.responses, 1)
	require.True(t, recovered.responses[0].GetError().GetTerminal())
}

func TestDurableGraphRecoveryHeartbeatKeepsLeaseAcrossSlowLoad(t *testing.T) {
	r := testredis.Start(t)
	s := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
	token := []byte(strings.Repeat("k", 32))
	original := durableTestGraph(t, s, "heartbeat-tenant", "slow-load", token)
	begin := &graphpb.BeginGraph{
		ProtocolVersion: 1,
		DigestFunction:  repb.DigestFunction_SHA256,
		ResumeToken:     token,
	}
	_, err := createDurableTestGraphSession(t, original, begin)
	require.NoError(t, err)
	require.NoError(t, original.acquireDurableLease())
	terminal := &graphpb.GraphExecuteResponse{
		SequenceNumber: 1,
		Payload: &graphpb.GraphExecuteResponse_Error{Error: &graphpb.GraphStreamError{
			Terminal: true,
		}},
	}
	terminalData, err := proto.Marshal(terminal)
	require.NoError(t, err)
	require.NoError(t, original.appendDurableResponse(terminal, terminalData))
	original.releaseDurableLease(context.Background())

	recovered := durableTestGraph(t, s, original.ownerPrefix, original.sessionID, token)
	recovered.afterRecoveryLeaseAcquireForTesting = func() {
		time.Sleep(2*graphDurableLeaseLifetime + graphDurableLeaseRefresh)
	}
	require.NoError(t, recovered.loadAndReconstructDurableGraph())
	require.Greater(t, r.Client().PTTL(
		context.Background(), recovered.durableLeaseKey).Val(), time.Duration(0))
	recovered.releaseDurableLease(context.Background())
}

func TestDurableGraphRecoveryHeartbeatKeepsLeaseDuringSlowAdmissionRefresh(t *testing.T) {
	r := testredis.Start(t)
	s := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
	token := []byte(strings.Repeat("a", 32))
	original := durableTestGraph(t, s, "heartbeat-tenant", "slow-admission", token)
	_, err := createDurableTestGraphSession(
		t, original, &graphpb.BeginGraph{ResumeToken: token})
	require.NoError(t, err)
	require.NoError(t, original.acquireDurableLease())
	original.releaseDurableLease(context.Background())

	recovered := durableTestGraph(t, s, original.ownerPrefix, original.sessionID, token)
	refreshStarted := make(chan struct{})
	unblockRefresh := make(chan struct{})
	refreshCalls := 0
	recovered.recoveryAdmissionRefreshForTesting = func() error {
		refreshCalls++
		if refreshCalls == 1 {
			close(refreshStarted)
			<-unblockRefresh
		}
		return recovered.refreshDurableAdmission()
	}
	recovered.afterRecoveryLeaseAcquireForTesting = func() {
		<-refreshStarted
		// Shorten all four reservation deadlines without expiring them. The
		// blocked initial refresh must restore them after the independent lease
		// heartbeat has kept the coordinator alive for multiple lease periods.
		shortDeadline := time.Now().Add(10 * time.Second).UnixMilli()
		for _, key := range []string{
			"graph-execution:{admission}:active:global",
			recovered.durableTenantIndexKey,
			"graph-execution:{admission}:retained:global",
			graphDurableRetainedTenantIndexKey(recovered.ownerPrefix),
		} {
			require.NoError(t, r.Client().ZAdd(context.Background(), key, &redis.Z{
				Score:  float64(shortDeadline),
				Member: recovered.durableKey,
			}).Err())
		}
		time.Sleep(2*graphDurableLeaseLifetime + graphDurableLeaseRefresh)
		require.Positive(t, r.Client().PTTL(
			context.Background(), recovered.durableLeaseKey).Val())
		close(unblockRefresh)
	}

	require.NoError(t, recovered.loadAndReconstructDurableGraph())
	require.Equal(t, 1, refreshCalls)
	minActiveDeadline := float64(time.Now().Add(graphDurableActiveLifetime / 2).UnixMilli())
	for _, key := range []string{
		"graph-execution:{admission}:active:global",
		recovered.durableTenantIndexKey,
	} {
		score, err := r.Client().ZScore(context.Background(), key, recovered.durableKey).Result()
		require.NoError(t, err)
		require.Greater(t, score, minActiveDeadline)
	}
	minRetainedDeadline := float64(time.Now().Add(graphDurableActiveLifetime / 2).UnixMilli())
	for _, key := range []string{
		"graph-execution:{admission}:retained:global",
		graphDurableRetainedTenantIndexKey(recovered.ownerPrefix),
	} {
		score, err := r.Client().ZScore(context.Background(), key, recovered.durableKey).Result()
		require.NoError(t, err)
		require.Greater(t, score, minRetainedDeadline)
	}
	recovered.releaseDurableLease(context.Background())
}

func TestDurableGraphRecoveryHeartbeatSurfacesFence(t *testing.T) {
	r := testredis.Start(t)
	s := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
	token := []byte(strings.Repeat("j", 32))
	original := durableTestGraph(t, s, "heartbeat-tenant", "heartbeat-fence", token)
	begin := &graphpb.BeginGraph{
		ProtocolVersion: 1,
		DigestFunction:  repb.DigestFunction_SHA256,
		ResumeToken:     token,
	}
	_, err := createDurableTestGraphSession(t, original, begin)
	require.NoError(t, err)

	recovered := durableTestGraph(t, s, original.ownerPrefix, original.sessionID, token)
	recovered.afterRecoveryLeaseAcquireForTesting = func() {
		time.Sleep(graphDurableLeaseRefresh + 100*time.Millisecond)
		require.NoError(t, r.Client().Set(
			context.Background(), recovered.durableLeaseKey, "replacement",
			graphDurableLeaseLifetime).Err())
		time.Sleep(graphDurableLeaseRefresh + 100*time.Millisecond)
	}
	err = recovered.loadAndReconstructDurableGraph()
	require.Error(t, err)
	require.True(t, status.IsAbortedError(err))
	require.Equal(t, "replacement", r.Client().Get(
		context.Background(), recovered.durableLeaseKey).Val())
}

func TestDurableGraphRecoveryFinalFenceCheckClosesHeartbeatStopRace(t *testing.T) {
	r := testredis.Start(t)
	s := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
	token := []byte(strings.Repeat("i", 32))
	original := durableTestGraph(t, s, "heartbeat-tenant", "final-fence", token)
	begin := &graphpb.BeginGraph{
		ProtocolVersion: 1,
		DigestFunction:  repb.DigestFunction_SHA256,
		ResumeToken:     token,
	}
	_, err := createDurableTestGraphSession(t, original, begin)
	require.NoError(t, err)

	recovered := durableTestGraph(t, s, original.ownerPrefix, original.sessionID, token)
	recovered.beforeRecoveryFinalFenceCheckForTesting = func() {
		require.NoError(t, r.Client().Set(
			context.Background(), recovered.durableLeaseKey, "replacement",
			graphDurableLeaseLifetime).Err())
	}
	err = recovered.loadAndReconstructDurableGraph()
	require.Error(t, err)
	require.True(t, status.IsAbortedError(err))
	require.Equal(t, "replacement", r.Client().Get(
		context.Background(), recovered.durableLeaseKey).Val())
}

func TestDurableGraphReconstructsPartialCompletion(t *testing.T) {
	s := &ExecutionServer{clock: clockwork.NewRealClock()}
	token := []byte(strings.Repeat("r", 32))
	g := durableTestGraph(t, s, "tenant", "partial", token)
	begin := &graphpb.GraphExecuteRequest{
		SessionId:      "partial",
		SequenceNumber: 1,
		Payload: &graphpb.GraphExecuteRequest_Begin{Begin: &graphpb.BeginGraph{
			ProtocolVersion: 1,
			DigestFunction:  repb.DigestFunction_SHA256,
			ResumeToken:     token,
		}},
	}
	action := &graphpb.GraphExecuteRequest{
		SessionId:      "partial",
		SequenceNumber: 2,
		Payload: &graphpb.GraphExecuteRequest_Action{Action: &graphpb.ActionNode{
			NodeId:  "done",
			Command: &repb.Command{Arguments: []string{"true"}},
		}},
	}
	response := &graphpb.GraphExecuteResponse{
		SequenceNumber: 1,
		Payload: &graphpb.GraphExecuteResponse_NodeResult{NodeResult: &graphpb.NodeResult{
			NodeId:       "done",
			ActionDigest: &repb.Digest{Hash: strings.Repeat("a", 64), SizeBytes: 1},
			ExecuteResponse: &repb.ExecuteResponse{
				Result: &repb.ActionResult{ExitCode: 0},
			},
		}},
	}
	beginData, err := proto.Marshal(begin)
	require.NoError(t, err)
	actionData, err := proto.Marshal(action)
	require.NoError(t, err)
	responseData, err := proto.Marshal(response)
	require.NoError(t, err)
	require.NoError(t, g.reconstructDurableGraph(&durableGraphData{
		requests:  map[uint64][]byte{1: beginData, 2: actionData},
		responses: map[uint64][]byte{1: responseData},
	}))
	require.True(t, g.nodes["done"].completed)
	require.Equal(t, 1, g.completedNodes)
}

func TestDurableFinishedGraphDoesNotReapplyTerminalInvalidRequest(t *testing.T) {
	s := &ExecutionServer{clock: clockwork.NewRealClock()}
	g := durableTestGraph(t, s, "tenant", "terminal", []byte(strings.Repeat("x", 32)))
	invalid := &graphpb.GraphExecuteRequest{
		SessionId:      "terminal",
		SequenceNumber: 1,
		Payload: &graphpb.GraphExecuteRequest_Action{Action: &graphpb.ActionNode{
			NodeId:  "invalid-before-begin",
			Command: &repb.Command{Arguments: []string{"false"}},
		}},
	}
	terminal := &graphpb.GraphExecuteResponse{
		SequenceNumber: 1,
		Payload: &graphpb.GraphExecuteResponse_Error{Error: &graphpb.GraphStreamError{
			Terminal: true,
		}},
	}
	invalidData, err := proto.Marshal(invalid)
	require.NoError(t, err)
	terminalData, err := proto.Marshal(terminal)
	require.NoError(t, err)
	require.NoError(t, g.reconstructDurableGraph(&durableGraphData{
		requests:  map[uint64][]byte{1: invalidData},
		responses: map[uint64][]byte{1: terminalData},
		finished:  true,
	}))
	require.True(t, g.finished.Load())
	require.Len(t, g.responses, 1)
}

func TestDurableActiveGraphRecoversJournaledTerminalValidationError(t *testing.T) {
	s := &ExecutionServer{clock: clockwork.NewRealClock()}
	token := []byte(strings.Repeat("v", 32))
	g := durableTestGraph(t, s, "tenant", "invalid-active", token)
	begin := &graphpb.GraphExecuteRequest{
		SessionId:      "invalid-active",
		SequenceNumber: 1,
		Payload: &graphpb.GraphExecuteRequest_Begin{Begin: &graphpb.BeginGraph{
			ProtocolVersion: 1,
			DigestFunction:  repb.DigestFunction_SHA256,
			ResumeToken:     token,
		}},
	}
	invalidCommit := &graphpb.GraphExecuteRequest{
		SessionId:      "invalid-active",
		SequenceNumber: 2,
		Payload: &graphpb.GraphExecuteRequest_Commit{Commit: &graphpb.CommitGraph{
			ExpectedActionCount: 1,
		}},
	}
	beginData, err := proto.Marshal(begin)
	require.NoError(t, err)
	invalidCommitData, err := proto.Marshal(invalidCommit)
	require.NoError(t, err)
	require.NoError(t, g.reconstructDurableGraph(&durableGraphData{
		requests: map[uint64][]byte{1: beginData, 2: invalidCommitData},
	}))
	require.False(t, g.finished.Load())
	require.Error(t, g.recoveredTerminalErr)
	require.True(t, status.IsInvalidArgumentError(g.recoveredTerminalErr))
	require.Contains(t, g.recoveredTerminalErr.Error(), "expected 1 actions but received 0")
	require.Equal(t, uint64(2), g.lastRequestSequence)
}

func TestDurableGraphRedisKeysUseValidScriptSlots(t *testing.T) {
	tag := func(key string) string {
		start := strings.IndexByte(key, '{')
		end := strings.IndexByte(key, '}')
		require.GreaterOrEqual(t, start, 0)
		require.Greater(t, end, start)
		return key[start+1 : end]
	}
	state := graphDurableSessionKey("tenant", "session")
	lease := state + ":lease"
	globalAdmission := "graph-execution:{admission}:active:global"
	tenantAdmission := graphDurableTenantIndexKey("tenant")
	retainedAdmission := graphDurableRetainedTenantIndexKey("tenant")
	require.Equal(t, tag(state), tag(lease))
	require.Equal(t, tag(globalAdmission), tag(tenantAdmission))
	require.Equal(t, tag(globalAdmission), tag(retainedAdmission))
	require.NotEqual(t, tag(state), tag(globalAdmission))
}

func TestDurablePreparedTaskBytesAreBounded(t *testing.T) {
	r := testredis.Start(t)
	s := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
	token := []byte(strings.Repeat("b", 32))
	g := durableTestGraph(t, s, "tenant", "bounded-task", token)
	_, err := createDurableTestGraphSession(t, g, &graphpb.BeginGraph{ResumeToken: token})
	require.NoError(t, err)
	require.NoError(t, g.acquireDurableLease())
	require.NoError(t, r.Client().HSet(
		context.Background(), g.durableKey, "prepared_task_bytes", graphDurableMaxPreparedTaskBytes,
	).Err())
	err = g.storeDurablePreparedTask("too-large", &scpb.EnsureTaskRequest{
		TaskId:         "id",
		SerializedTask: []byte("x"),
	})
	require.Error(t, err)
	require.True(t, status.IsResourceExhaustedError(err))
}

func TestDurableGraphAdmissionIsBoundedAcrossServers(t *testing.T) {
	r := testredis.Start(t)
	token := []byte(strings.Repeat("a", 32))
	byteLimitedCapacity := graphDurableMaxActiveBytesPerTenant / graphDurableActiveReservationBytes
	require.Less(t, byteLimitedCapacity, graphExecutionMaxActiveSessionsPerTenant)
	for i := 0; i < byteLimitedCapacity; i++ {
		s := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
		g := durableTestGraph(t, s, "bounded-tenant", "session-"+strings.Repeat("x", i), token)
		_, err := createDurableTestGraphSession(t, g, &graphpb.BeginGraph{ResumeToken: token})
		require.NoError(t, err)
	}
	s := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
	g := durableTestGraph(t, s, "bounded-tenant", "one-too-many", token)
	_, err := createDurableTestGraphSession(t, g, &graphpb.BeginGraph{ResumeToken: token})
	require.Error(t, err)
	require.True(t, status.IsResourceExhaustedError(err))
}

func TestDurableGraphFinishedCommitPurgesAndRejectsFurtherMutation(t *testing.T) {
	r := testredis.Start(t)
	s := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
	token := []byte(strings.Repeat("p", 32))
	g := durableTestGraph(t, s, "tenant-finished", "finished", token)
	_, err := createDurableTestGraphSession(t, g, &graphpb.BeginGraph{ResumeToken: token})
	require.NoError(t, err)
	require.NoError(t, g.acquireDurableLease())
	_, err = g.appendDurableRequest(2, []byte("request"))
	require.NoError(t, err)
	require.NoError(t, g.storeDurablePreparedTask("node", &scpb.EnsureTaskRequest{
		TaskId:         "task",
		SerializedTask: []byte("serialized"),
	}))
	_, _, err = g.claimDurableExecution("node", &repb.Digest{
		Hash:      strings.Repeat("a", 64),
		SizeBytes: 1,
	})
	require.NoError(t, err)
	terminal := &graphpb.GraphExecuteResponse{
		SequenceNumber: 1,
		Payload: &graphpb.GraphExecuteResponse_Error{Error: &graphpb.GraphStreamError{
			Terminal: true,
		}},
	}
	terminalData, err := proto.Marshal(terminal)
	require.NoError(t, err)
	require.NoError(t, g.appendDurableResponse(terminal, terminalData))
	require.NoError(t, g.finishDurableSession())

	values := r.Client().HGetAll(context.Background(), g.durableKey).Val()
	require.Equal(t, "finished", values["state"])
	require.NotEmpty(t, values["response:1"])
	require.NotEmpty(t, values["terminal_response"])
	require.Equal(t, "0", values["request_bytes"])
	require.Equal(t, "0", values["prepared_task_bytes"])
	for field := range values {
		require.False(t,
			strings.HasPrefix(field, "request:") ||
				strings.HasPrefix(field, "execution_id:") ||
				strings.HasPrefix(field, "action_digest:") ||
				strings.HasPrefix(field, "prepared_task:"),
			"terminal payload field %q was not purged", field)
	}
	require.Zero(t, r.Client().ZCard(
		context.Background(), "graph-execution:{admission}:active:global").Val())
	require.Equal(t, int64(1), r.Client().ZCard(
		context.Background(), "graph-execution:{admission}:retained:global").Val())

	// Even a caller that somehow presents a matching lease cannot mutate state
	// once the durable terminal transition has committed.
	require.NoError(t, r.Client().Set(
		context.Background(), g.durableLeaseKey, "forged", time.Minute).Err())
	g.durableMu.Lock()
	g.durableLeaseValue = "forged"
	g.durableMu.Unlock()
	_, err = g.appendDurableRequest(2, []byte("late"))
	require.Error(t, err)
	require.True(t, status.IsAbortedError(err))
}

func TestDurableGraphAdmissionSeparatesActiveAndRetainedCategories(t *testing.T) {
	r := testredis.Start(t)
	s := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
	token := []byte(strings.Repeat("q", 32))
	finished := durableTestGraph(t, s, "separate-tenant", "retained", token)
	_, err := createDurableTestGraphSession(t, finished, &graphpb.BeginGraph{ResumeToken: token})
	require.NoError(t, err)
	require.NoError(t, finished.acquireDurableLease())
	terminal := &graphpb.GraphExecuteResponse{
		SequenceNumber: 1,
		Payload: &graphpb.GraphExecuteResponse_Error{Error: &graphpb.GraphStreamError{
			Terminal: true,
		}},
	}
	terminalData, err := proto.Marshal(terminal)
	require.NoError(t, err)
	require.NoError(t, finished.appendDurableResponse(terminal, terminalData))
	require.NoError(t, finished.finishDurableSession())

	byteLimitedCapacity := graphDurableMaxActiveBytesPerTenant / graphDurableActiveReservationBytes
	for i := 0; i < byteLimitedCapacity; i++ {
		active := durableTestGraph(
			t, s, "separate-tenant", "active-"+strings.Repeat("x", i), token)
		_, err := createDurableTestGraphSession(t, active, &graphpb.BeginGraph{ResumeToken: token})
		require.NoError(t, err)
	}
	excess := durableTestGraph(t, s, "separate-tenant", "active-excess", token)
	_, err = createDurableTestGraphSession(t, excess, &graphpb.BeginGraph{ResumeToken: token})
	require.Error(t, err)
	require.True(t, status.IsResourceExhaustedError(err))
	require.Equal(t, int64(byteLimitedCapacity), r.Client().ZCard(
		context.Background(), "graph-execution:{admission}:active:global").Val())
	// Active graphs also hold their guaranteed future terminal replay slots.
	require.Equal(t, int64(1+byteLimitedCapacity), r.Client().ZCard(
		context.Background(), "graph-execution:{admission}:retained:global").Val())
}

func TestDurableTerminalCapacityIsGuaranteedAcrossSequentialGraphs(t *testing.T) {
	r := testredis.Start(t)
	s := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
	token := []byte(strings.Repeat("g", 32))
	const completedGraphs = 12
	for i := 0; i < completedGraphs; i++ {
		g := durableTestGraph(t, s, "sequential-tenant", fmt.Sprintf("finished-%d", i), token)
		_, err := createDurableTestGraphSession(t, g, &graphpb.BeginGraph{ResumeToken: token})
		require.NoError(t, err)
		require.NoError(t, g.acquireDurableLease())
		terminal := &graphpb.GraphExecuteResponse{
			SequenceNumber: 1,
			Payload: &graphpb.GraphExecuteResponse_Error{Error: &graphpb.GraphStreamError{
				Terminal: true,
			}},
		}
		data, err := proto.Marshal(terminal)
		require.NoError(t, err)
		require.NoError(t, g.appendDurableResponse(terminal, data))
		require.NoError(t, g.finishDurableSession(),
			"terminal capacity reserved at Begin must make finish admission-infallible")
	}
	require.Zero(t, r.Client().ZCard(
		context.Background(), "graph-execution:{admission}:active:global").Val())
	require.Equal(t, int64(completedGraphs), r.Client().ZCard(
		context.Background(), "graph-execution:{admission}:retained:global").Val())

	next := durableTestGraph(t, s, "sequential-tenant", "next-active", token)
	_, err := createDurableTestGraphSession(t, next, &graphpb.BeginGraph{ResumeToken: token})
	require.NoError(t, err)
	require.Equal(t, int64(1), r.Client().ZCard(
		context.Background(), "graph-execution:{admission}:active:global").Val())
}

func TestDurableTerminalCapacityExhaustionRejectsAtBeginWithoutActiveLeak(t *testing.T) {
	r := testredis.Start(t)
	s := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
	token := []byte(strings.Repeat("h", 32))
	g := durableTestGraph(t, s, "full-retained-tenant", "rejected", token)
	capacity := graphDurableMaxRetainedBytesPerTenant / graphDurableRetainedReservationBytes
	require.Less(t, capacity, graphExecutionMaxFinishedSessionsPerTenant)
	expiresAt := float64(time.Now().Add(time.Hour).UnixMilli())
	for i := 0; i < capacity; i++ {
		require.NoError(t, r.Client().ZAdd(
			context.Background(),
			graphDurableRetainedTenantIndexKey(g.ownerPrefix),
			&redis.Z{Score: expiresAt, Member: fmt.Sprintf("occupied-%d", i)},
		).Err())
	}
	_, err := createDurableTestGraphSession(t, g, &graphpb.BeginGraph{ResumeToken: token})
	require.Error(t, err)
	require.True(t, status.IsResourceExhaustedError(err))
	require.Zero(t, r.Client().Exists(context.Background(), g.durableKey).Val())
	require.Zero(t, r.Client().ZCard(
		context.Background(), "graph-execution:{admission}:active:global").Val(),
		"terminal-capacity rejection must happen before active admission")
	require.Zero(t, r.Client().ZCard(
		context.Background(), g.durableTenantIndexKey).Val())
}

func TestDurableGraphFinishRequiresCommittedTerminalResponse(t *testing.T) {
	r := testredis.Start(t)
	s := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
	token := []byte(strings.Repeat("n", 32))
	g := durableTestGraph(t, s, "tenant", "no-terminal", token)
	_, err := createDurableTestGraphSession(t, g, &graphpb.BeginGraph{ResumeToken: token})
	require.NoError(t, err)
	require.NoError(t, g.acquireDurableLease())
	err = g.finishDurableSession()
	require.Error(t, err)
	require.True(t, status.IsAbortedError(err))
	require.Equal(t, "active", r.Client().HGet(
		context.Background(), g.durableKey, "state").Val())
}

func TestDurableGraphRecoveryFinalizesTerminalResponseAfterCrash(t *testing.T) {
	r := testredis.Start(t)
	s := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
	token := []byte(strings.Repeat("z", 32))
	original := durableTestGraph(t, s, "tenant", "terminal-before-finish", token)
	_, err := createDurableTestGraphSession(t, original, &graphpb.BeginGraph{ResumeToken: token})
	require.NoError(t, err)
	require.NoError(t, original.acquireDurableLease())
	terminal := &graphpb.GraphExecuteResponse{
		SequenceNumber: 1,
		Payload: &graphpb.GraphExecuteResponse_Error{Error: &graphpb.GraphStreamError{
			Terminal: true,
		}},
	}
	terminalData, err := proto.Marshal(terminal)
	require.NoError(t, err)
	require.NoError(t, original.appendDurableResponse(terminal, terminalData))
	original.releaseDurableLease(context.Background())

	recovered := durableTestGraph(t, s, original.ownerPrefix, original.sessionID, token)
	data, err := recovered.loadDurableGraphForRecovery()
	require.NoError(t, err)
	require.False(t, recovered.isDurableFinished())
	require.NoError(t, recovered.reconstructDurableGraph(data))
	require.True(t, recovered.finished.Load())
	require.False(t, recovered.isDurableFinished())
	require.NoError(t, recovered.finishDurableSession())
	require.True(t, recovered.isDurableFinished())
	require.Equal(t, "finished", r.Client().HGet(
		context.Background(), recovered.durableKey, "state").Val())
}

func TestTerminalCapacityFailureDoesNotFinishSession(t *testing.T) {
	s := &ExecutionServer{clock: clockwork.NewRealClock()}
	g := newGraphExecution(s, nil, context.Background(), "tenant")
	g.responseReplayBytes = graphExecutionMaxResponseReplayBytes
	err := g.sendTerminalError(status.InvalidArgumentError("invalid graph"))
	require.Error(t, err)
	require.True(t, status.IsResourceExhaustedError(err))
	require.False(t, g.finished.Load())
	require.True(t, g.retainedAt.IsZero())
}

func TestTransientExecutionBackendFailureRemainsRetriable(t *testing.T) {
	s := &ExecutionServer{clock: clockwork.NewRealClock()}
	g := newGraphExecution(s, nil, context.Background(), "tenant")
	g.nodes["node"] = &graphNodeState{
		definition: &graphpb.ActionNode{NodeId: "node"},
		running:    true,
	}
	g.inFlight = 1
	err := g.handleCompletion(graphActionCompletion{
		nodeID: "node",
		err:    status.UnavailableError("scheduler unavailable"),
	})
	var retryErr *graphRetriableError
	require.ErrorAs(t, err, &retryErr)
	require.True(t, status.IsUnavailableError(g.terminalOrDisconnect(err)))
	require.False(t, g.finished.Load())
	require.True(t, g.nodes["node"].running)
	require.Equal(t, 1, g.inFlight)
	require.True(t, g.needsRecoveredWaiters)
}

func TestUploadedBlobInfrastructureFailureRemainsRetriable(t *testing.T) {
	for _, code := range []codes.Code{codes.Unavailable, codes.Aborted} {
		t.Run(code.String(), func(t *testing.T) {
			s := &ExecutionServer{clock: clockwork.NewRealClock()}
			g := newGraphExecution(s, nil, context.Background(), "tenant")
			g.digestFunction = repb.DigestFunction_SHA256
			g.missingBlobFinder = graphMissingBlobFinderFunc(func(
				context.Context, []*rspb.ResourceName,
			) ([]*repb.Digest, error) {
				return nil, gstatus.Error(code, "cache infrastructure failure")
			})
			err := g.handleUploadedBlobs(&graphpb.UploadedBlobs{Digests: []*repb.Digest{{
				Hash: strings.Repeat("a", 64), SizeBytes: 1,
			}}})
			var retriableErr *graphRetriableError
			require.ErrorAs(t, err, &retriableErr)
			require.Equal(t, code, gstatus.Code(g.terminalOrDisconnect(err)))
			require.False(t, g.finished.Load())
			require.Empty(t, g.responses)
		})
	}
}

func TestDurableUploadedBlobTransientEvictsAndSameAppRecoveryRetriesJournal(t *testing.T) {
	r := testredis.Start(t)
	s := &ExecutionServer{
		rdb:           r.Client(),
		clock:         clockwork.NewRealClock(),
		graphSessions: make(map[string]*graphExecution),
	}
	finderCalls := 0
	s.graphMissingBlobFinderForTesting = graphMissingBlobFinderFunc(func(
		context.Context, []*rspb.ResourceName,
	) ([]*repb.Digest, error) {
		finderCalls++
		if finderCalls == 1 {
			return nil, status.UnavailableError("transient FindMissing failure")
		}
		return nil, nil
	})
	token := []byte(strings.Repeat("u", 32))
	original := durableTestGraph(t, s, "retry-tenant", "uploaded-retry", token)
	begin := &graphpb.BeginGraph{
		ProtocolVersion: 1,
		DigestFunction:  repb.DigestFunction_SHA256,
		ResumeToken:     token,
	}
	_, err := createDurableTestGraphSession(t, original, begin)
	require.NoError(t, err)
	require.NoError(t, original.acquireDurableLease())
	blob := &repb.Digest{Hash: strings.Repeat("d", 64), SizeBytes: 1}
	uploadedRequest := &graphpb.GraphExecuteRequest{
		SessionId:      original.sessionID,
		SequenceNumber: 2,
		Payload: &graphpb.GraphExecuteRequest_UploadedBlobs{
			UploadedBlobs: &graphpb.UploadedBlobs{Digests: []*repb.Digest{blob}},
		},
	}
	uploadedData, err := proto.Marshal(uploadedRequest)
	require.NoError(t, err)
	_, err = original.appendDurableRequest(2, uploadedData)
	require.NoError(t, err)
	key := graphSessionKey(original.ownerPrefix, original.sessionID)
	original.connected = true
	original.stream = &durabilityGraphStream{}
	s.graphSessions[key] = original

	err = original.terminalOrDisconnect(
		&graphRetriableError{err: status.UnavailableError("initial synchronous failure")})
	require.True(t, status.IsUnavailableError(err))
	require.NotContains(t, s.graphSessions, key)
	require.False(t, original.connected)
	require.Nil(t, original.stream)
	require.Zero(t, r.Client().Exists(context.Background(), original.durableLeaseKey).Val())

	_, err = s.recoverDurableGraphExecution(
		nil, context.Background(), original.ownerPrefix, original.sessionID, token)
	require.Error(t, err)
	require.True(t, status.IsUnavailableError(err))
	require.Equal(t, 1, finderCalls)
	require.NotContains(t, s.graphSessions, key)
	require.False(t, r.Client().HExists(
		context.Background(), original.durableKey, "terminal_response").Val())
	require.Zero(t, r.Client().Exists(context.Background(), original.durableLeaseKey).Val())

	recovered, err := s.recoverDurableGraphExecution(
		nil, context.Background(), original.ownerPrefix, original.sessionID, token)
	require.NoError(t, err)
	require.Equal(t, 2, finderCalls)
	_, available := recovered.availableBlobs[digest.NewKey(blob)]
	require.True(t, available)
	require.Contains(t, s.graphSessions, key)
	recovered.releaseDurableLease(context.Background())
	recovered.removeSession()
}

func TestDurableTransportLossPreservesLeaseButMutationFailureDiscards(t *testing.T) {
	t.Run("stream send error", func(t *testing.T) {
		r := testredis.Start(t)
		s := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
		token := []byte(strings.Repeat("x", 32))
		g := durableTestGraph(t, s, "transport-tenant", "send-error", token)
		_, err := createDurableTestGraphSession(t, g, &graphpb.BeginGraph{ResumeToken: token})
		require.NoError(t, err)
		require.NoError(t, g.acquireDurableLease())

		sendErr := status.UnavailableError("client stream disconnected")
		require.Same(t, sendErr, g.terminalOrDisconnect(&graphStreamSendError{err: sendErr}))
		require.Positive(t, r.Client().PTTL(context.Background(), g.durableLeaseKey).Val())
		g.releaseDurableLease(context.Background())
	})

	t.Run("stream context cancellation", func(t *testing.T) {
		r := testredis.Start(t)
		s := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
		token := []byte(strings.Repeat("y", 32))
		g := durableTestGraph(t, s, "transport-tenant", "context-canceled", token)
		_, err := createDurableTestGraphSession(t, g, &graphpb.BeginGraph{ResumeToken: token})
		require.NoError(t, err)
		require.NoError(t, g.acquireDurableLease())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err = g.run(&durabilityGraphStream{ctx: ctx}, nil)
		require.ErrorIs(t, err, context.Canceled)
		require.Positive(t, r.Client().PTTL(context.Background(), g.durableLeaseKey).Val())
		g.releaseDurableLease(context.Background())
	})

	t.Run("abrupt app loss racing waiter cancellation", func(t *testing.T) {
		r := testredis.Start(t)
		s := &ExecutionServer{
			rdb:           r.Client(),
			clock:         clockwork.NewRealClock(),
			graphSessions: make(map[string]*graphExecution),
		}
		token := []byte(strings.Repeat("z", 32))
		g := durableTestGraph(t, s, "transport-tenant", "app-crash", token)
		_, err := createDurableTestGraphSession(t, g, &graphpb.BeginGraph{ResumeToken: token})
		require.NoError(t, err)
		require.NoError(t, g.acquireDurableLease())
		s.graphSessions[graphSessionKey(g.ownerPrefix, g.sessionID)] = g

		s.stopGraphExecution(false)
		require.True(t, g.preserveLeaseOnCrash.Load())
		require.True(t, status.IsUnavailableError(g.terminalOrDisconnect(
			&graphRetriableError{err: status.UnavailableError("waiter canceled during app crash")})))
		require.Positive(t, r.Client().PTTL(context.Background(), g.durableLeaseKey).Val())
		g.releaseDurableLease(context.Background())
	})

	t.Run("synchronous mutation error", func(t *testing.T) {
		r := testredis.Start(t)
		s := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
		token := []byte(strings.Repeat("m", 32))
		g := durableTestGraph(t, s, "mutation-tenant", "mutation-error", token)
		_, err := createDurableTestGraphSession(t, g, &graphpb.BeginGraph{ResumeToken: token})
		require.NoError(t, err)
		require.NoError(t, g.acquireDurableLease())

		require.True(t, status.IsUnavailableError(g.terminalOrDisconnect(
			&graphRetriableError{err: status.UnavailableError("CAS mutation failed")})))
		require.Zero(t, r.Client().Exists(context.Background(), g.durableLeaseKey).Val())
	})
}

func TestGraphSessionPublicationAppliesShutdownLeasePolicy(t *testing.T) {
	for _, test := range []struct {
		name          string
		releaseLeases bool
	}{
		{name: "graceful shutdown", releaseLeases: true},
		{name: "abrupt shutdown", releaseLeases: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := testredis.Start(t)
			s := &ExecutionServer{
				rdb:           r.Client(),
				clock:         clockwork.NewRealClock(),
				graphSessions: make(map[string]*graphExecution),
			}
			token := []byte(strings.Repeat("p", 32))
			g := durableTestGraph(t, s, "publication-tenant", test.name, token)
			_, err := createDurableTestGraphSession(
				t, g, &graphpb.BeginGraph{ResumeToken: token})
			require.NoError(t, err)
			require.True(t, s.beginGraphHandler())
			require.NoError(t, g.acquireDurableLease())

			publishEntered := make(chan struct{})
			allowPublish := make(chan struct{})
			s.beforeGraphSessionPublishForTesting = func(got *graphExecution) {
				require.Same(t, g, got)
				close(publishEntered)
				<-allowPublish
			}
			publishResult := make(chan error, 1)
			go func() {
				defer s.graphHandlers.Done()
				publishResult <- s.publishGraphExecution(g)
			}()
			<-publishEntered

			shutdownDone := make(chan struct{})
			go func() {
				s.stopGraphExecution(test.releaseLeases)
				close(shutdownDone)
			}()
			require.Eventually(t, func() bool {
				s.graphSessionsMu.Lock()
				defer s.graphSessionsMu.Unlock()
				return s.graphShuttingDown
			}, time.Second, time.Millisecond)
			close(allowPublish)

			select {
			case err := <-publishResult:
				require.True(t, status.IsUnavailableError(err))
			case <-time.After(time.Second):
				t.Fatal("publication did not return after shutdown snapshot")
			}
			select {
			case <-shutdownDone:
			case <-time.After(time.Second):
				t.Fatal("shutdown remained blocked waiting for admitted graph handler")
			}
			require.Empty(t, s.graphSessions)
			select {
			case <-g.ctx.Done():
			default:
				t.Fatal("rejected local coordinator was not canceled")
			}
			if test.releaseLeases {
				require.Zero(t, r.Client().Exists(
					context.Background(), g.durableLeaseKey).Val())
			} else {
				require.True(t, g.preserveLeaseOnCrash.Load())
				require.Positive(t, r.Client().PTTL(
					context.Background(), g.durableLeaseKey).Val())
				g.releaseDurableLease(context.Background())
			}
		})
	}
}

func TestGraphLeaseAcquireAfterHandlerAdmissionObservesShutdown(t *testing.T) {
	r := testredis.Start(t)
	s := &ExecutionServer{
		rdb:           r.Client(),
		clock:         clockwork.NewRealClock(),
		graphSessions: make(map[string]*graphExecution),
	}
	token := []byte(strings.Repeat("q", 32))
	g := durableTestGraph(t, s, "publication-tenant", "acquire-race", token)
	_, err := createDurableTestGraphSession(
		t, g, &graphpb.BeginGraph{ResumeToken: token})
	require.NoError(t, err)
	require.True(t, s.beginGraphHandler())

	shutdownDone := make(chan struct{})
	go func() {
		s.stopGraphExecution(false)
		close(shutdownDone)
	}()
	require.Eventually(t, func() bool {
		s.graphSessionsMu.Lock()
		defer s.graphSessionsMu.Unlock()
		return s.graphShuttingDown
	}, time.Second, time.Millisecond)

	err = g.acquireDurableLease()
	require.True(t, status.IsUnavailableError(err))
	s.discardUnpublishedGraphExecution(g)
	s.graphHandlers.Done()
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown remained blocked after the admitted handler returned")
	}
	require.Empty(t, s.graphSessions)
	require.True(t, g.preserveLeaseOnCrash.Load())
	require.Zero(t, r.Client().Exists(context.Background(), g.durableLeaseKey).Val())
	select {
	case <-g.ctx.Done():
	default:
		t.Fatal("unpublished graph was not canceled after lease acquisition rejection")
	}
}

func TestDurableStartNodeTransientRestoresDirtyNodeOnRecovery(t *testing.T) {
	r := testredis.Start(t)
	s := &ExecutionServer{
		rdb:           r.Client(),
		clock:         clockwork.NewRealClock(),
		graphSessions: make(map[string]*graphExecution),
		graphMissingBlobFinderForTesting: graphMissingBlobFinderFunc(func(
			context.Context, []*rspb.ResourceName,
		) ([]*repb.Digest, error) {
			return nil, nil
		}),
	}
	token := []byte(strings.Repeat("t", 32))
	original := durableTestGraph(t, s, "retry-tenant", "start-node-retry", token)
	begin := &graphpb.BeginGraph{
		ProtocolVersion: 1,
		DigestFunction:  repb.DigestFunction_SHA256,
		ResumeToken:     token,
	}
	_, err := createDurableTestGraphSession(t, original, begin)
	require.NoError(t, err)
	require.NoError(t, original.acquireDurableLease())
	emptyDigest := &repb.Digest{Hash: digest.EmptySha256, SizeBytes: 0}
	requests := []*graphpb.GraphExecuteRequest{
		{
			SessionId:      original.sessionID,
			SequenceNumber: 2,
			Payload: &graphpb.GraphExecuteRequest_UploadedBlobs{
				UploadedBlobs: &graphpb.UploadedBlobs{Digests: []*repb.Digest{emptyDigest}},
			},
		},
		{
			SessionId:      original.sessionID,
			SequenceNumber: 3,
			Payload: &graphpb.GraphExecuteRequest_Action{Action: &graphpb.ActionNode{
				NodeId:          "node",
				Command:         &repb.Command{},
				InputRootDigest: emptyDigest,
			}},
		},
		{
			SessionId:      original.sessionID,
			SequenceNumber: 4,
			Payload: &graphpb.GraphExecuteRequest_Commit{
				Commit: &graphpb.CommitGraph{ExpectedActionCount: 1},
			},
		},
	}
	for _, request := range requests {
		data, err := proto.Marshal(request)
		require.NoError(t, err)
		_, err = original.appendDurableRequest(request.GetSequenceNumber(), data)
		require.NoError(t, err)
	}
	original.releaseDurableLease(context.Background())

	first, err := s.recoverDurableGraphExecution(
		nil, context.Background(), original.ownerPrefix, original.sessionID, token)
	require.NoError(t, err)
	require.Contains(t, first.dirtyNodeSet, "node")
	first.startNodeForTesting = func(string, *graphNodeState, []graphInputFile) error {
		return &graphRetriableError{err: status.UnavailableError("CAS upload unavailable")}
	}
	err = first.scheduleReadyNodes()
	require.Error(t, err)
	require.Contains(t, first.dirtyNodeSet, "node")
	require.True(t, status.IsUnavailableError(first.terminalOrDisconnect(err)))
	require.NotContains(t, s.graphSessions, graphSessionKey(first.ownerPrefix, first.sessionID))
	require.False(t, r.Client().HExists(
		context.Background(), first.durableKey, "terminal_response").Val())

	second, err := s.recoverDurableGraphExecution(
		nil, context.Background(), original.ownerPrefix, original.sessionID, token)
	require.NoError(t, err)
	require.Contains(t, second.dirtyNodeSet, "node")
	require.False(t, second.finished.Load())
	second.releaseDurableLease(context.Background())
	second.removeSession()
}

func TestSyntheticWaitSubscriptionNotFoundIsRetriableButSemanticNotFoundIsTerminal(t *testing.T) {
	synthetic := &repb.ExecuteResponse{Status: &statuspb.Status{
		Code:    int32(codes.NotFound),
		Message: "receive execution update: Redis subscription lost",
	}}
	require.True(t, isSyntheticGraphWaitSubscriptionFailure(synthetic))

	s := &ExecutionServer{clock: clockwork.NewRealClock()}
	g := newGraphExecution(s, nil, context.Background(), "tenant")
	g.nodes["node"] = &graphNodeState{
		definition: &graphpb.ActionNode{NodeId: "node"},
		running:    true,
	}
	g.inFlight = 1
	err := g.handleCompletion(graphActionCompletion{
		nodeID: "node",
		err: &graphRetriableError{err: status.UnavailableError(
			"durable execution status subscription was lost")},
	})
	var retriableErr *graphRetriableError
	require.ErrorAs(t, err, &retriableErr)
	require.True(t, status.IsUnavailableError(g.terminalOrDisconnect(err)))
	require.False(t, g.finished.Load())
	require.True(t, g.nodes["node"].running)
	require.Equal(t, 1, g.inFlight)
	require.True(t, g.needsRecoveredWaiters)

	semantic := &repb.ExecuteResponse{Status: &statuspb.Status{
		Code:    int32(codes.NotFound),
		Message: "declared action input is missing",
	}}
	require.False(t, isSyntheticGraphWaitSubscriptionFailure(semantic))
	stream := &durabilityGraphStream{}
	semanticGraph := newGraphExecution(s, stream, context.Background(), "tenant")
	semanticGraph.nodes["node"] = &graphNodeState{
		definition: &graphpb.ActionNode{NodeId: "node"},
		running:    true,
	}
	semanticGraph.inFlight = 1
	require.NoError(t, semanticGraph.handleCompletion(graphActionCompletion{
		nodeID:       "node",
		actionDigest: &repb.Digest{Hash: strings.Repeat("b", 64), SizeBytes: 1},
		response:     semantic,
	}))
	require.True(t, semanticGraph.finished.Load())
	require.Len(t, stream.sent, 2)
	require.Equal(t, int32(codes.NotFound), stream.sent[1].GetResult().GetStatus().GetCode())
}

func TestDurableGeneratedStateAndNodeIDsAreBoundedByRecoveryAdmission(t *testing.T) {
	require.Equal(t, graphDurableActiveReservationBytes+4<<20, graphDurableMaxRecoveryBytes)

	s := &ExecutionServer{clock: clockwork.NewRealClock()}
	g := newGraphExecution(s, nil, context.Background(), "tenant")
	err := g.handleAction(&graphpb.ActionNode{
		NodeId:  strings.Repeat("n", graphExecutionMaxNodeIDBytes+1),
		Command: &repb.Command{},
	})
	require.Error(t, err)
	require.True(t, status.IsResourceExhaustedError(err))
	g.totalNodeIDBytes = graphExecutionMaxNodeIDBytesPerGraph
	err = g.handleAction(&graphpb.ActionNode{NodeId: "n", Command: &repb.Command{}})
	require.Error(t, err)
	require.True(t, status.IsResourceExhaustedError(err))

	r := testredis.Start(t)
	durableServer := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
	token := []byte(strings.Repeat("v", 32))
	durable := durableTestGraph(t, durableServer, "tenant", "generated-bound", token)
	_, err = createDurableTestGraphSession(t, durable, &graphpb.BeginGraph{ResumeToken: token})
	require.NoError(t, err)
	require.NoError(t, durable.acquireDurableLease())
	require.NoError(t, r.Client().HSet(
		context.Background(), durable.durableKey, "generated_bytes", graphDurableMaxGeneratedBytes,
	).Err())
	_, _, err = durable.claimDurableExecution("node", &repb.Digest{
		Hash: strings.Repeat("c", 64), SizeBytes: 1,
	})
	require.Error(t, err)
	require.True(t, status.IsResourceExhaustedError(err))
}

func TestRecoveredFinishedGraphCompactsExactlyOnce(t *testing.T) {
	fakeClock := clockwork.NewFakeClock()
	s := &ExecutionServer{
		clock:         fakeClock,
		graphSessions: make(map[string]*graphExecution),
	}
	g := durableTestGraph(t, s, "tenant", "recovered-finished", []byte(strings.Repeat("c", 32)))
	g.finished.Store(true)
	g.setDurableFinished()
	s.graphSessions[graphSessionKey(g.ownerPrefix, g.sessionID)] = g
	g.finish()
	firstRetainedAt := g.retainedAt
	require.False(t, firstRetainedAt.IsZero())
	require.Nil(t, g.nodes)
	g.finish()
	require.Equal(t, firstRetainedAt, g.retainedAt)
}

func TestDurableGraphRedisFailureIsRetriable(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 10 * time.Millisecond,
		MaxRetries:  0,
	})
	t.Cleanup(func() { _ = rdb.Close() })
	s := &ExecutionServer{rdb: rdb, clock: clockwork.NewRealClock()}
	token := []byte(strings.Repeat("f", 32))
	g := durableTestGraph(t, s, "tenant", "redis-down", token)
	_, err := createDurableTestGraphSession(t, g, &graphpb.BeginGraph{ResumeToken: token})
	require.Error(t, err)
	require.True(t, status.IsUnavailableError(err))
}

func TestGraphRetriableErrorPreservesStatus(t *testing.T) {
	err := retriableGraphInfrastructureError(status.UnavailableError("redis unavailable"))
	var retryErr *graphRetriableError
	require.True(t, errors.As(err, &retryErr))
	require.True(t, status.IsUnavailableError(err))
}

func TestCompletedOperationRetentionCoversDurableGraphRecovery(t *testing.T) {
	require.GreaterOrEqual(
		t,
		completedPubSubChanExpiration,
		graphDurableActiveLifetime+5*time.Minute)
}

func TestTerminalOrDisconnectClassifiesRawInfrastructureStatus(t *testing.T) {
	s := &ExecutionServer{clock: clockwork.NewRealClock()}
	stream := &durabilityGraphStream{}
	g := newGraphExecution(s, stream, context.Background(), "tenant")
	require.True(t, status.IsUnavailableError(
		g.terminalOrDisconnect(status.UnavailableError("CAS unavailable"))))
	require.Empty(t, stream.sent)
	require.False(t, g.finished.Load())

	semanticStream := &durabilityGraphStream{}
	semanticGraph := newGraphExecution(s, semanticStream, context.Background(), "tenant")
	require.NoError(t, semanticGraph.terminalOrDisconnect(status.InvalidArgumentError("bad action")))
	require.Len(t, semanticStream.sent, 1)
	require.True(t, semanticStream.sent[0].GetError().GetTerminal())
	require.True(t, semanticGraph.finished.Load())
}

func TestAcceptedRootEnvelopeFitsReservedTerminalHeadroom(t *testing.T) {
	stream := &durabilityGraphStream{}
	s := &ExecutionServer{
		clock:         clockwork.NewRealClock(),
		graphSessions: make(map[string]*graphExecution),
	}
	g := newGraphExecution(s, stream, context.Background(), "tenant")
	token := []byte(strings.Repeat("r", 32))
	roots := make([]*graphpb.RootOutput, 0, graphExecutionMaxRoots)
	for i := 0; i < graphExecutionMaxRoots; i++ {
		roots = append(roots, &graphpb.RootOutput{
			NodeId:     fmt.Sprintf("node-%04d", i),
			OutputPath: strings.Repeat("p", 80),
		})
	}
	begin := &graphpb.BeginGraph{
		ProtocolVersion: graphExecutionProtocolVersion,
		DigestFunction:  repb.DigestFunction_SHA256,
		ResumeToken:     token,
		Roots:           roots,
	}
	require.LessOrEqual(t, rootsProtoSize(roots), graphExecutionMaxRootDeclarationBytes)
	require.NoError(t, g.handleBegin("root-boundary", begin))

	results := make([]*graphpb.RootResult, 0, len(roots))
	for _, root := range roots {
		results = append(results, &graphpb.RootResult{
			Root:   root,
			Digest: &repb.Digest{Hash: strings.Repeat("a", 64), SizeBytes: 1},
		})
	}
	response := &graphpb.GraphExecuteResponse{
		Payload: &graphpb.GraphExecuteResponse_Result{Result: &graphpb.GraphResult{
			Roots: results,
		}},
	}
	require.Less(t, proto.Size(response), graphExecutionTerminalResponseHeadroom)
	require.NoError(t, g.sendTerminalResponse(response))
	require.True(t, g.finished.Load())
}

func TestOversizedExecutorTerminalPayloadUsesBoundedTerminalSlot(t *testing.T) {
	stream := &durabilityGraphStream{}
	s := &ExecutionServer{clock: clockwork.NewRealClock()}
	g := newGraphExecution(s, stream, context.Background(), "tenant")
	err := g.sendFailedGraphResult(&statuspb.Status{
		Code:    int32(codes.Internal),
		Message: strings.Repeat("x", graphExecutionTerminalResponseHeadroom),
	}, "node")
	require.NoError(t, err)
	require.Len(t, stream.sent, 1)
	require.True(t, stream.sent[0].GetError().GetTerminal())
	require.Equal(t, int32(codes.ResourceExhausted), stream.sent[0].GetError().GetStatus().GetCode())
	require.Less(t, proto.Size(stream.sent[0]), graphExecutionTerminalResponseHeadroom)
}

func rootsProtoSize(roots []*graphpb.RootOutput) int {
	n := 0
	for _, root := range roots {
		n += proto.Size(root)
	}
	return n
}

func TestDurableBeginCreationRejectsDifferentBytesAfterCrashWindow(t *testing.T) {
	r := testredis.Start(t)
	s := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
	token := []byte(strings.Repeat("i", 32))
	first := durableTestGraph(t, s, "tenant", "atomic-begin", token)
	begin := &graphpb.BeginGraph{ResumeToken: token, InstanceName: "first"}
	created, err := createDurableTestGraphSession(t, first, begin)
	require.NoError(t, err)
	require.True(t, created)
	require.NotEmpty(t, r.Client().HGet(
		context.Background(), first.durableKey, "request:1").Val())

	afterCrash := durableTestGraph(t, s, "tenant", "atomic-begin", token)
	created, err = createDurableTestGraphSession(
		t, afterCrash, &graphpb.BeginGraph{ResumeToken: token, InstanceName: "different"})
	require.Error(t, err)
	require.False(t, created)
	require.True(t, status.IsAlreadyExistsError(err))
}

func TestDurableRecoveryRejectsBytesIndependentOfFieldCount(t *testing.T) {
	r := testredis.Start(t)
	s := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
	token := []byte(strings.Repeat("m", 32))
	g := durableTestGraph(t, s, "tenant", "oversized-recovery", token)
	_, err := createDurableTestGraphSession(t, g, &graphpb.BeginGraph{ResumeToken: token})
	require.NoError(t, err)
	require.NoError(t, r.Client().HSet(
		context.Background(),
		g.durableKey,
		"response:1",
		strings.Repeat("x", graphExecutionMaxResponseReplayBytes+1),
	).Err())
	_, err = g.loadDurableGraph()
	require.Error(t, err)
	require.True(t, status.IsResourceExhaustedError(err))
}

func TestTerminalPurgeRenewsLeaseAcrossLargeSession(t *testing.T) {
	r := testredis.Start(t)
	s := &ExecutionServer{rdb: r.Client(), clock: clockwork.NewRealClock()}
	token := []byte(strings.Repeat("l", 32))
	g := durableTestGraph(t, s, "tenant", "large-purge", token)
	_, err := createDurableTestGraphSession(t, g, &graphpb.BeginGraph{ResumeToken: token})
	require.NoError(t, err)
	require.NoError(t, g.acquireDurableLease())
	terminal := &graphpb.GraphExecuteResponse{
		SequenceNumber: 1,
		Payload: &graphpb.GraphExecuteResponse_Error{Error: &graphpb.GraphStreamError{
			Terminal: true,
		}},
	}
	terminalData, err := proto.Marshal(terminal)
	require.NoError(t, err)
	require.NoError(t, g.appendDurableResponse(terminal, terminalData))
	fields := make(map[string]any, 4_000)
	for i := 0; i < 1_000; i++ {
		fields[fmt.Sprintf("request:%d", i+2)] = "r"
		fields[fmt.Sprintf("execution_id:n%d", i)] = "e"
		fields[fmt.Sprintf("action_digest:n%d", i)] = "a"
		fields[fmt.Sprintf("prepared_task:n%d", i)] = "p"
	}
	require.NoError(t, r.Client().HSet(context.Background(), g.durableKey, fields).Err())
	require.NoError(t, r.Client().PExpire(
		context.Background(), g.durableLeaseKey, time.Millisecond).Err())
	require.NoError(t, g.purgeDurableTerminalFields(g.currentDurableLeaseValue()))
	require.Greater(t, r.Client().PTTL(
		context.Background(), g.durableLeaseKey).Val(), time.Second)
	values := r.Client().HGetAll(context.Background(), g.durableKey).Val()
	for field := range values {
		require.False(t,
			strings.HasPrefix(field, "request:") ||
				strings.HasPrefix(field, "execution_id:") ||
				strings.HasPrefix(field, "action_digest:") ||
				strings.HasPrefix(field, "prepared_task:"))
	}
}
