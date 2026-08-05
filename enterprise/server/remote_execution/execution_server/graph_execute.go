package execution_server

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"maps"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/operation"
	"github.com/buildbuddy-io/buildbuddy/server/metrics"
	"github.com/buildbuddy-io/buildbuddy/server/remote_cache/cachetools"
	"github.com/buildbuddy-io/buildbuddy/server/remote_cache/digest"
	"github.com/buildbuddy-io/buildbuddy/server/util/bazel_request"
	"github.com/buildbuddy-io/buildbuddy/server/util/prefix"
	"github.com/buildbuddy-io/buildbuddy/server/util/proto"
	"github.com/buildbuddy-io/buildbuddy/server/util/status"
	"github.com/jonboulle/clockwork"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	gstatus "google.golang.org/grpc/status"

	graphpb "github.com/buildbuddy-io/buildbuddy/proto/graph_execution"
	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
	rspb "github.com/buildbuddy-io/buildbuddy/proto/resource"
	scpb "github.com/buildbuddy-io/buildbuddy/proto/scheduler"
)

const (
	graphExecutionProtocolVersion              = 1
	graphExecutionFinishedSessionLifetime      = 30 * time.Minute
	graphExecutionDisconnectedSessionLifetime  = 5 * time.Minute
	graphExecutionMaxActiveSessionsPerTenant   = 8
	graphExecutionMaxActiveSessionsGlobal      = 4096
	graphExecutionMaxFinishedSessionsPerTenant = 32
	graphExecutionMaxFinishedSessionsGlobal    = 1024
	graphExecutionMaxFinishedBytesPerTenant    = 16 << 20
	graphExecutionMaxFinishedBytesGlobal       = 256 << 20
	graphExecutionMaxNodes                     = 100_000
	graphExecutionMaxReplayRequests            = 200_000
	graphExecutionMaxReplayResponses           = 200_000
	graphExecutionMaxRequestReplayBytes        = 64 << 20
	graphExecutionMaxResponseReplayBytes       = 64 << 20
	graphExecutionTerminalResponseHeadroom     = 1 << 20
	graphExecutionMaxRootDeclarationBytes      = 256 << 10
	graphExecutionMaxRoots                     = 2_048
	graphExecutionMaxReceivedRequests          = 400_000
	graphExecutionMaxReceivedRequestBytes      = 128 << 20
	graphExecutionMaxInFlightActions           = 256
	graphExecutionMaxInputBindingsPerAction    = 100_000
	graphExecutionMaxInputBindings             = 1_000_000
	graphExecutionMaxInputPathBytes            = 4096
	graphExecutionMaxInputPathDepth            = 256
	graphExecutionMaxInputPathComponentBytes   = 255
	graphExecutionMaxInputPathBytesPerGraph    = 64 << 20
	graphExecutionMaxInputDirectoriesPerAction = 10_000
	graphExecutionMaxInputDirectoriesPerGraph  = 100_000
	graphExecutionDeclarationAckInterval       = 64
	graphExecutionMaxNodeIDBytes               = 1_024
	graphExecutionMaxNodeIDBytesPerGraph       = 3 << 20
	graphExecutionMaxSessionIDBytes            = 1_024
	graphExecutionMaxInstanceNameBytes         = 4 << 10
	graphExecutionMaxInvocationIDBytes         = 4 << 10
)

type graphMissingBlobFinder interface {
	FindMissing(context.Context, []*rspb.ResourceName) ([]*repb.Digest, error)
}

type graphArtifact struct {
	digest       *repb.Digest
	isExecutable bool
}

type graphNodeState struct {
	definition              *graphpb.ActionNode
	definitionData          []byte
	unresolvedPrerequisites int
	inputsMaterialized      bool
	materializedInputs      []graphInputFile
	running                 bool
	completed               bool
	actionDigest            *repb.Digest
	executionID             string
	preparedTask            *scpb.EnsureTaskRequest
	response                *repb.ExecuteResponse
}

type graphProducedConsumer struct {
	nodeID     string
	outputPath string
}

type graphActionCompletion struct {
	nodeID       string
	actionDigest *repb.Digest
	response     *repb.ExecuteResponse
	err          error
}

type graphActionStream struct {
	ctx        context.Context
	nodeID     string
	action     *repb.Digest
	completion chan<- graphActionCompletion
	sent       bool
}

func (s *graphActionStream) Context() context.Context {
	return s.ctx
}

func (s *graphActionStream) Send(op *longrunningpb.Operation) error {
	if operation.ExtractStage(op) != repb.ExecutionStage_COMPLETED || s.sent {
		return nil
	}
	s.sent = true
	executeResponse := operation.ExtractExecuteResponse(op)
	completion := graphActionCompletion{
		nodeID:       s.nodeID,
		actionDigest: s.action,
		response:     executeResponse,
	}
	if isSyntheticGraphWaitSubscriptionFailure(executeResponse) {
		completion.response = nil
		completion.err = &graphRetriableError{err: status.UnavailableErrorf(
			"durable execution status subscription was lost: %s",
			executeResponse.GetStatus().GetMessage())}
	}
	select {
	case s.completion <- completion:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func isSyntheticGraphWaitSubscriptionFailure(response *repb.ExecuteResponse) bool {
	if response == nil || response.GetStatus().GetCode() != int32(codes.NotFound) {
		return false
	}
	// waitExecution synthesizes this exact provenance when its Redis
	// subscription fails. Ordinary action-level NOT_FOUND statuses remain
	// semantic terminal failures.
	return strings.HasPrefix(response.GetStatus().GetMessage(), "receive execution update:")
}

type graphRecvResult struct {
	request *graphpb.GraphExecuteRequest
	err     error
}

type graphStreamSendError struct {
	err error
}

func (e *graphStreamSendError) Error() string {
	return e.err.Error()
}

func (e *graphStreamSendError) Unwrap() error {
	return e.err
}

type graphExecution struct {
	server *ExecutionServer
	stream graphpb.GraphExecution_GraphExecuteServer
	ctx    context.Context
	cancel context.CancelFunc

	sessionID      string
	resumeToken    []byte
	ownerPrefix    string
	instanceName   string
	digestFunction repb.DigestFunction_Value
	invocationID   string
	roots          []*graphpb.RootOutput

	lastRequestSequence   uint64
	requestData           map[uint64][]byte
	requestReplayBytes    int64
	receivedRequests      int
	receivedRequestBytes  int64
	lastRequestApplied    bool
	responseSequence      uint64
	responses             []*graphpb.GraphExecuteResponse
	responseReplayBytes   int64
	committed             bool
	finished              atomic.Bool
	preserveLeaseOnCrash  atomic.Bool
	connected             bool
	connectionNumber      uint64
	disconnectTimer       clockwork.Timer
	retainedAt            time.Time
	retainedOrder         uint64
	retainedBytes         int64
	durableKey            string
	durableLeaseKey       string
	durableTenantIndexKey string
	durableLeaseValue     string
	durableLeaseEpoch     uint64
	durableFinished       bool
	lastAdmissionRefresh  time.Time
	reconstructing        bool
	needsRecoveredWaiters bool
	recoveredTerminalErr  error
	missingBlobFinder     graphMissingBlobFinder
	// Test-only deterministic interleaving seam between the optimistic state
	// scan and lease acquisition during recovery.
	beforeRecoveryLeaseAcquireForTesting    func()
	afterRecoveryLeaseAcquireForTesting     func()
	beforeRecoveryFinalFenceCheckForTesting func()
	recoveryAdmissionRefreshForTesting      func() error
	recoveryAdmissionIntervalForTesting     time.Duration
	startNodeForTesting                     func(string, *graphNodeState, []graphInputFile) error
	durableMu                               sync.Mutex
	finishLocalOnce                         sync.Once

	runMu sync.Mutex

	nodes                 map[string]*graphNodeState
	availableBlobs        map[digest.Key]struct{}
	outputs               map[string]map[string]*graphArtifact
	completions           chan graphActionCompletion
	inFlight              int
	completedNodes        int
	totalInputBindings    int
	totalInputPathBytes   int64
	totalInputDirectories int
	totalNodeIDBytes      int64

	recoveryHeartbeatMu    sync.Mutex
	recoveryHeartbeatStop  chan struct{}
	recoveryHeartbeatDone  chan struct{}
	recoveryHeartbeatErr   error
	recoveryAdmissionStop  chan struct{}
	recoveryAdmissionDone  chan struct{}
	recoveryAdmissionReady chan struct{}

	dirtyNodes           []string
	dirtyNodeSet         map[string]struct{}
	sourceConsumers      map[digest.Key]map[string]int
	producerConsumers    map[string][]graphProducedConsumer
	schedulingDependents map[string]map[string]int

	beginTime          time.Time
	firstReadyObserved bool
	terminalObserved   bool
}

type graphDurabilityError struct {
	err error
}

func (e *graphDurabilityError) Error() string {
	return e.err.Error()
}

func (e *graphDurabilityError) Unwrap() error {
	return e.err
}

type graphLeaseTransitionError struct {
	err error
}

func (e *graphLeaseTransitionError) Error() string {
	return e.err.Error()
}

func (e *graphLeaseTransitionError) Unwrap() error {
	return e.err
}

type graphRetriableError struct {
	err error
}

func (e *graphRetriableError) Error() string {
	return e.err.Error()
}

func (e *graphRetriableError) Unwrap() error {
	return e.err
}

func (s *ExecutionServer) GraphExecute(stream graphpb.GraphExecution_GraphExecuteServer) error {
	ctx, err := prefix.AttachUserPrefixToContext(stream.Context(), s.authenticator)
	if err != nil {
		return err
	}
	ownerPrefix, err := prefix.UserPrefixFromContext(ctx)
	if err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if !s.beginGraphHandler() {
		return status.UnavailableError("graph execution server is shutting down")
	}
	defer s.graphHandlers.Done()
	recordGraphRequestMessage(first)
	if resume := first.GetResume(); resume != nil {
		if first.GetSequenceNumber() != 0 {
			return status.InvalidArgumentError("ResumeGraph must use sequence_number 0")
		}
		g, err := s.resumeGraphExecution(first.GetSessionId(), resume, ownerPrefix)
		if status.IsNotFoundError(err) {
			g, err = s.recoverDurableGraphExecution(
				stream, ctx, ownerPrefix, first.GetSessionId(), resume.GetResumeToken())
		}
		if err != nil {
			return err
		}
		err = g.serveResumedGraph(stream, first, resume.GetLastResponseSequence())
		var transitionErr *graphLeaseTransitionError
		if !errors.As(err, &transitionErr) {
			return err
		}
		g, err = s.recoverDurableGraphExecution(
			stream, ctx, ownerPrefix, first.GetSessionId(), resume.GetResumeToken())
		if err != nil {
			return err
		}
		return g.serveResumedGraph(stream, first, resume.GetLastResponseSequence())
	}
	begin := first.GetBegin()
	if begin == nil {
		return status.FailedPreconditionError("first graph request must contain BeginGraph or ResumeGraph")
	}
	if first.GetSessionId() == "" {
		return status.InvalidArgumentError("session_id is required")
	}
	if len(first.GetSessionId()) > graphExecutionMaxSessionIDBytes {
		return status.ResourceExhaustedErrorf(
			"session_id exceeds the limit of %d bytes",
			graphExecutionMaxSessionIDBytes)
	}
	g := newGraphExecution(s, stream, ctx, ownerPrefix)
	g.sessionID = first.GetSessionId()
	beginRequest, err := proto.Marshal(first)
	if err != nil {
		return status.WrapError(err, "marshal initial GraphExecute request")
	}
	created, err := g.createDurableGraphSession(begin, beginRequest)
	if err != nil {
		if status.IsResourceExhaustedError(err) {
			return sendRejectedGraphBegin(stream, first, err)
		}
		return err
	}
	if !created {
		if err := g.loadAndReconstructDurableGraph(); err != nil {
			s.discardUnpublishedGraphExecution(g)
			return err
		}
		if err := s.publishGraphExecution(g); err != nil {
			if !g.preserveLeaseOnCrash.Load() {
				g.releaseDurableLease(context.Background())
			}
			return err
		}
		// A repeated BeginGraph is the lost-BeginAck recovery path. Replay the
		// complete durable response prefix before accepting further requests.
		return g.serveResumedGraph(stream, first, 0)
	}
	if err := g.acquireDurableLease(); err != nil {
		s.discardUnpublishedGraphExecution(g)
		return err
	}
	// handleBegin performs the normal validation and local registration. The
	// durable identity was created above and must remain available to it.
	g.sessionID = ""
	g.runMu.Lock()
	g.attachStreamLocked(stream)
	err = g.run(stream, first)
	if err != nil && !g.finished.Load() {
		g.markDisconnectedLocked()
	}
	g.runMu.Unlock()
	return err
}

func sendRejectedGraphBegin(
	stream graphpb.GraphExecution_GraphExecuteServer,
	request *graphpb.GraphExecuteRequest,
	err error,
) error {
	response := &graphpb.GraphExecuteResponse{
		SequenceNumber:     1,
		AckRequestSequence: request.GetSequenceNumber(),
		Payload: &graphpb.GraphExecuteResponse_Error{Error: &graphpb.GraphStreamError{
			Status:   gstatus.Convert(err).Proto(),
			Terminal: true,
		}},
	}
	if sendErr := stream.Send(response); sendErr != nil {
		return sendErr
	}
	recordGraphResponseMessage(response)
	return nil
}

func (g *graphExecution) serveResumedGraph(
	stream graphpb.GraphExecution_GraphExecuteServer,
	first *graphpb.GraphExecuteRequest,
	lastResponseSequence uint64,
) error {
	if !g.runMu.TryLock() {
		return status.AlreadyExistsErrorf("graph session %q already has an active stream", first.GetSessionId())
	}
	if !g.finished.Load() {
		if err := g.renewDurableLease(); err != nil {
			if status.IsAbortedError(err) {
				g.discardLocalCoordinatorLocked()
				g.runMu.Unlock()
				return &graphLeaseTransitionError{err: err}
			}
			g.runMu.Unlock()
			return err
		}
	} else if !g.isDurableFinished() {
		if err := g.finishDurableSession(); err != nil {
			if status.IsAbortedError(err) {
				g.discardLocalCoordinatorLocked()
				g.runMu.Unlock()
				return &graphLeaseTransitionError{err: err}
			}
			// The terminal response is already durable and safe to replay.
			// Retention admission or Redis availability can delay compaction,
			// but must not hide the committed outcome from the client.
		}
	}
	if err := g.recordReceivedRequest(first); err != nil {
		g.runMu.Unlock()
		return err
	}
	if first.GetBegin() != nil && !g.finished.Load() {
		if err := g.handleRequest(first); err != nil {
			g.runMu.Unlock()
			return err
		}
	}
	if lastResponseSequence > g.responseSequence {
		g.runMu.Unlock()
		return status.InvalidArgumentErrorf(
			"last_response_sequence %d exceeds server sequence %d",
			lastResponseSequence, g.responseSequence)
	}
	g.attachStreamLocked(stream)
	for _, response := range g.responses {
		if response.GetSequenceNumber() <= lastResponseSequence {
			continue
		}
		if err := stream.Send(response); err != nil {
			g.markDisconnectedLocked()
			g.runMu.Unlock()
			return err
		}
		recordGraphResponseMessage(response)
	}
	if g.finished.Load() {
		g.finish()
		g.stream = nil
		g.connected = false
		g.runMu.Unlock()
		return nil
	}
	if g.recoveredTerminalErr != nil {
		err := g.sendTerminalError(g.recoveredTerminalErr)
		g.runMu.Unlock()
		return err
	}
	g.launchRecoveredNodeExecutions()
	if err := g.scheduleReadyNodes(); err != nil {
		err = g.terminalOrDisconnect(err)
		g.runMu.Unlock()
		return err
	}
	if err := g.checkCommittedQuiescence(); err != nil {
		err = g.terminalOrDisconnect(err)
		g.runMu.Unlock()
		return err
	}
	if g.committed && g.allNodesCompleted() {
		err := g.sendGraphResult()
		g.runMu.Unlock()
		return err
	}
	err := g.run(stream, nil)
	if err != nil && !g.finished.Load() {
		g.markDisconnectedLocked()
	}
	g.runMu.Unlock()
	return err
}

func (g *graphExecution) discardLocalCoordinatorLocked() {
	if g.disconnectTimer != nil {
		g.disconnectTimer.Stop()
		g.disconnectTimer = nil
	}
	g.connected = false
	g.stream = nil
	g.cancelGraphWaiters()
	if !g.preserveLeaseOnCrash.Load() {
		g.releaseDurableLease(context.Background())
	}
	g.removeSession()
}

func (s *ExecutionServer) resumeGraphExecution(sessionID string, resume *graphpb.ResumeGraph, ownerPrefix string) (*graphExecution, error) {
	if sessionID == "" || resume.GetSessionId() != sessionID {
		return nil, status.InvalidArgumentError("ResumeGraph session ID does not match request session ID")
	}
	s.graphSessionsMu.Lock()
	defer s.graphSessionsMu.Unlock()
	g := s.graphSessions[graphSessionKey(ownerPrefix, sessionID)]
	if g == nil {
		return nil, status.NotFoundErrorf("graph session %q was not found", sessionID)
	}
	if subtle.ConstantTimeCompare(g.resumeToken, resume.GetResumeToken()) != 1 {
		return nil, status.PermissionDeniedError("invalid graph resume token")
	}
	return g, nil
}

func (g *graphExecution) attachStreamLocked(stream graphpb.GraphExecution_GraphExecuteServer) {
	if g.disconnectTimer != nil {
		g.disconnectTimer.Stop()
		g.disconnectTimer = nil
	}
	g.stream = stream
	g.connected = true
	g.connectionNumber++
}

func (g *graphExecution) markDisconnectedLocked() {
	if g.finished.Load() || !g.connected {
		return
	}
	g.connected = false
	connectionNumber := g.connectionNumber
	if g.disconnectTimer != nil {
		g.disconnectTimer.Stop()
	}
	g.disconnectTimer = g.server.clock.AfterFunc(graphExecutionDisconnectedSessionLifetime, func() {
		g.expireDisconnected(connectionNumber)
	})
}

func (g *graphExecution) expireDisconnected(connectionNumber uint64) {
	g.runMu.Lock()
	defer g.runMu.Unlock()
	if g.finished.Load() || g.connected || g.connectionNumber != connectionNumber {
		return
	}
	g.cancelGraphWaiters()
	g.releaseDurableLease(context.Background())
	g.removeSession()
}

func (g *graphExecution) run(stream graphpb.GraphExecution_GraphExecuteServer, initial *graphpb.GraphExecuteRequest) error {
	leaseTicker := time.NewTicker(graphDurableLeaseRefresh)
	defer leaseTicker.Stop()
	recv := make(chan graphRecvResult, 1)
	go func() {
		for {
			req, err := stream.Recv()
			if err == nil {
				recordGraphRequestMessage(req)
			}
			select {
			case recv <- graphRecvResult{request: req, err: err}:
			case <-stream.Context().Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	if initial != nil {
		if err := g.handleRequest(initial); err != nil {
			return g.terminalOrDisconnect(err)
		}
		if g.lastRequestApplied {
			if err := g.scheduleReadyNodes(); err != nil {
				return g.terminalOrDisconnect(err)
			}
		}
		if err := g.checkCommittedQuiescence(); err != nil {
			return g.terminalOrDisconnect(err)
		}
	}

	for {
		select {
		case <-leaseTicker.C:
			if err := g.renewDurableLease(); err != nil {
				return g.terminalOrDisconnect(err)
			}

		case <-g.ctx.Done():
			return g.ctx.Err()

		case <-stream.Context().Done():
			return stream.Context().Err()

		case result := <-recv:
			if result.err == io.EOF {
				if !g.committed {
					return g.sendTerminalError(status.FailedPreconditionError("graph stream closed before CommitGraph"))
				}
				recv = nil
				if g.allNodesCompleted() {
					return g.sendGraphResult()
				}
				continue
			}
			if result.err != nil {
				return result.err
			}
			if err := g.handleRequest(result.request); err != nil {
				return g.terminalOrDisconnect(err)
			}
			if g.lastRequestApplied {
				if err := g.scheduleReadyNodes(); err != nil {
					return g.terminalOrDisconnect(err)
				}
			}
			if err := g.checkCommittedQuiescence(); err != nil {
				return g.terminalOrDisconnect(err)
			}
			if g.committed && g.allNodesCompleted() {
				return g.sendGraphResult()
			}

		case completion := <-g.completions:
			if err := g.handleCompletion(completion); err != nil {
				return g.terminalOrDisconnect(err)
			}
			if g.finished.Load() {
				return nil
			}
			if err := g.scheduleReadyNodes(); err != nil {
				return g.terminalOrDisconnect(err)
			}
			if err := g.checkCommittedQuiescence(); err != nil {
				return g.terminalOrDisconnect(err)
			}
			if g.committed && g.allNodesCompleted() {
				return g.sendGraphResult()
			}
		}
	}
}

func (g *graphExecution) terminalOrDisconnect(err error) error {
	var sendErr *graphStreamSendError
	if errors.As(err, &sendErr) {
		return sendErr.err
	}
	var durabilityErr *graphDurabilityError
	if errors.As(err, &durabilityErr) {
		switch gstatus.Code(durabilityErr.err) {
		case codes.Aborted, codes.Unavailable:
			g.discardLocalCoordinatorLocked()
		default:
		}
		return durabilityErr.err
	}
	var retriableErr *graphRetriableError
	if errors.As(err, &retriableErr) {
		g.discardLocalCoordinatorLocked()
		return retriableErr.err
	}
	// Synchronous cache, CAS, and scheduler calls can return infrastructure
	// status errors directly. They are safe to retry from the durable graph
	// journal; validation and action failures use semantic status codes.
	switch gstatus.Code(err) {
	case codes.Aborted, codes.Unavailable:
		g.discardLocalCoordinatorLocked()
		return err
	default:
	}
	return g.sendTerminalError(err)
}

func retriableGraphInfrastructureError(err error) error {
	switch gstatus.Code(err) {
	case codes.Aborted, codes.Unavailable:
		return &graphRetriableError{err: err}
	default:
		return err
	}
}

func isRetriableGraphError(err error) bool {
	var retriableErr *graphRetriableError
	if errors.As(err, &retriableErr) {
		return true
	}
	switch gstatus.Code(err) {
	case codes.Aborted, codes.Unavailable:
		return true
	default:
		return false
	}
}

func (g *graphExecution) handleRequest(req *graphpb.GraphExecuteRequest) error {
	g.lastRequestApplied = false
	if req == nil {
		return status.InvalidArgumentError("nil GraphExecuteRequest")
	}
	if err := g.recordReceivedRequest(req); err != nil {
		return err
	}
	data, err := proto.Marshal(req)
	if err != nil {
		return status.WrapError(err, "marshal GraphExecuteRequest")
	}
	if _, err := g.appendDurableRequest(req.GetSequenceNumber(), data); err != nil {
		return err
	}
	if req.GetSequenceNumber() <= g.lastRequestSequence {
		previous := g.requestData[req.GetSequenceNumber()]
		if !slices.Equal(previous, data) {
			return status.InvalidArgumentErrorf("sequence %d was retransmitted with different contents", req.GetSequenceNumber())
		}
		return nil
	}
	if req.GetSequenceNumber() != g.lastRequestSequence+1 {
		return status.InvalidArgumentErrorf("expected request sequence %d, got %d", g.lastRequestSequence+1, req.GetSequenceNumber())
	}
	if g.sessionID != "" && req.GetSessionId() != g.sessionID {
		return status.InvalidArgumentErrorf("request session ID %q does not match %q", req.GetSessionId(), g.sessionID)
	}
	if g.requestReplayBytes+int64(len(data)) > graphExecutionMaxRequestReplayBytes {
		return status.ResourceExhaustedErrorf(
			"graph request replay data exceeds %d bytes", graphExecutionMaxRequestReplayBytes)
	}
	if len(g.requestData) >= graphExecutionMaxReplayRequests {
		return status.ResourceExhaustedErrorf(
			"graph exceeds the limit of %d retained requests", graphExecutionMaxReplayRequests)
	}
	g.lastRequestSequence = req.GetSequenceNumber()
	g.requestData[req.GetSequenceNumber()] = data
	g.requestReplayBytes += int64(len(data))
	g.lastRequestApplied = true

	if begin := req.GetBegin(); begin != nil {
		return g.handleBegin(req.GetSessionId(), begin)
	}
	if req.GetResume() != nil {
		return status.UnimplementedError("resuming GraphExecute streams is not implemented")
	}
	if g.sessionID == "" {
		return status.FailedPreconditionError("first graph request must contain BeginGraph")
	}
	if g.committed {
		return status.FailedPreconditionError("graph declarations are immutable after CommitGraph")
	}
	if uploaded := req.GetUploadedBlobs(); uploaded != nil {
		if err := g.handleUploadedBlobs(uploaded); err != nil {
			return err
		}
		return g.maybeSendDeclarationAck()
	}
	if action := req.GetAction(); action != nil {
		if err := g.handleAction(action); err != nil {
			return err
		}
		return g.maybeSendDeclarationAck()
	}
	if commit := req.GetCommit(); commit != nil {
		return g.handleCommit(commit)
	}
	return status.InvalidArgumentError("GraphExecuteRequest payload is required")
}

func (g *graphExecution) maybeSendDeclarationAck() error {
	if g.lastRequestSequence%graphExecutionDeclarationAckInterval != 0 {
		return nil
	}
	// An empty payload is an ack-only response. Keeping the cadence well below
	// the client's declaration window advances flow control for large graphs
	// without restoring per-action request/response chatter.
	return g.send(&graphpb.GraphExecuteResponse{})
}

func (g *graphExecution) recordReceivedRequest(req *graphpb.GraphExecuteRequest) error {
	requestBytes := int64(proto.Size(req))
	if g.receivedRequests >= graphExecutionMaxReceivedRequests {
		return status.ResourceExhaustedErrorf(
			"graph exceeds the limit of %d received requests", graphExecutionMaxReceivedRequests)
	}
	if g.receivedRequestBytes+requestBytes > graphExecutionMaxReceivedRequestBytes {
		return status.ResourceExhaustedErrorf(
			"graph received request traffic exceeds %d bytes", graphExecutionMaxReceivedRequestBytes)
	}
	g.receivedRequests++
	g.receivedRequestBytes += requestBytes
	return nil
}

func (g *graphExecution) handleBegin(sessionID string, begin *graphpb.BeginGraph) error {
	if g.sessionID != "" {
		return status.FailedPreconditionError("BeginGraph was already received")
	}
	if sessionID == "" {
		return status.InvalidArgumentError("session_id is required")
	}
	if len(sessionID) > graphExecutionMaxSessionIDBytes {
		return status.ResourceExhaustedErrorf(
			"session_id exceeds the limit of %d bytes",
			graphExecutionMaxSessionIDBytes)
	}
	if begin.GetProtocolVersion() != graphExecutionProtocolVersion {
		return status.InvalidArgumentErrorf("unsupported graph protocol version %d", begin.GetProtocolVersion())
	}
	if begin.GetDigestFunction() == repb.DigestFunction_UNKNOWN {
		return status.InvalidArgumentError("digest_function is required")
	}
	if len(begin.GetResumeToken()) != 32 {
		return status.InvalidArgumentError("BeginGraph resume_token must contain exactly 32 bytes")
	}
	if len(begin.GetInstanceName()) > graphExecutionMaxInstanceNameBytes {
		return status.ResourceExhaustedErrorf(
			"instance_name exceeds the limit of %d bytes",
			graphExecutionMaxInstanceNameBytes)
	}
	if len(begin.GetInvocationId()) > graphExecutionMaxInvocationIDBytes {
		return status.ResourceExhaustedErrorf(
			"invocation_id exceeds the limit of %d bytes",
			graphExecutionMaxInvocationIDBytes)
	}
	if len(begin.GetRoots()) > graphExecutionMaxRoots {
		return status.ResourceExhaustedErrorf(
			"graph exceeds the limit of %d root outputs", graphExecutionMaxRoots)
	}
	rootBytes := 0
	for _, root := range begin.GetRoots() {
		rootBytes += proto.Size(root)
	}
	if rootBytes > graphExecutionMaxRootDeclarationBytes {
		return status.ResourceExhaustedErrorf(
			"graph root declarations exceed %d bytes", graphExecutionMaxRootDeclarationBytes)
	}
	if len(g.resumeToken) != 0 && subtle.ConstantTimeCompare(g.resumeToken, begin.GetResumeToken()) != 1 {
		return status.PermissionDeniedError("BeginGraph resume token does not match durable session")
	}
	g.sessionID = sessionID
	g.resumeToken = slices.Clone(begin.GetResumeToken())
	g.instanceName = begin.GetInstanceName()
	g.digestFunction = begin.GetDigestFunction()
	g.invocationID = begin.GetInvocationId()
	g.roots = begin.GetRoots()
	g.beginTime = g.server.clock.Now()
	if err := g.server.publishGraphExecution(g); err != nil {
		return err
	}
	return g.send(&graphpb.GraphExecuteResponse{
		Payload: &graphpb.GraphExecuteResponse_Begin{
			Begin: &graphpb.BeginAck{
				SessionId:       sessionID,
				ResumeToken:     slices.Clone(g.resumeToken),
				ProtocolVersion: graphExecutionProtocolVersion,
			},
		},
	})
}

func (g *graphExecution) handleUploadedBlobs(uploaded *graphpb.UploadedBlobs) error {
	resources := make([]*rspb.ResourceName, 0, len(uploaded.GetDigests()))
	for _, d := range uploaded.GetDigests() {
		r := digest.NewCASResourceName(d, g.instanceName, g.digestFunction)
		if err := r.Validate(); err != nil {
			return err
		}
		if r.IsEmpty() {
			g.availableBlobs[digest.NewKey(d)] = struct{}{}
			if err := g.resolveSourceConsumers(digest.NewKey(d)); err != nil {
				return err
			}
			continue
		}
		resources = append(resources, r.ToProto())
	}
	missing, err := g.missingBlobFinder.FindMissing(g.ctx, resources)
	if err != nil {
		return retriableGraphInfrastructureError(
			status.WrapError(err, "find uploaded graph blobs"))
	}
	missingSet := make(map[digest.Key]struct{}, len(missing))
	for _, d := range missing {
		missingSet[digest.NewKey(d)] = struct{}{}
	}
	for _, d := range uploaded.GetDigests() {
		if _, absent := missingSet[digest.NewKey(d)]; !absent {
			g.availableBlobs[digest.NewKey(d)] = struct{}{}
			if err := g.resolveSourceConsumers(digest.NewKey(d)); err != nil {
				return err
			}
		}
	}
	if len(missing) > 0 {
		return g.send(&graphpb.GraphExecuteResponse{
			Payload: &graphpb.GraphExecuteResponse_MissingBlobs{
				MissingBlobs: &graphpb.MissingBlobs{Digests: missing},
			},
		})
	}
	return nil
}

func (g *graphExecution) handleAction(action *graphpb.ActionNode) error {
	if action.GetNodeId() == "" {
		return status.InvalidArgumentError("action node_id is required")
	}
	if len(action.GetNodeId()) > graphExecutionMaxNodeIDBytes {
		return status.ResourceExhaustedErrorf(
			"action node_id exceeds the limit of %d bytes",
			graphExecutionMaxNodeIDBytes)
	}
	if action.GetCommand() == nil {
		return status.InvalidArgumentErrorf("action %q has no Command", action.GetNodeId())
	}
	if len(action.GetInputs()) > graphExecutionMaxInputBindingsPerAction {
		return status.ResourceExhaustedErrorf(
			"action %q exceeds the limit of %d input bindings",
			action.GetNodeId(), graphExecutionMaxInputBindingsPerAction)
	}
	if inputRootDigest := action.GetInputRootDigest(); inputRootDigest != nil {
		if len(action.GetInputs()) != 0 {
			return status.InvalidArgumentErrorf("action %q specifies both inputs and input_root_digest", action.GetNodeId())
		}
		if err := digest.Validate(inputRootDigest, g.digestFunction); err != nil {
			return status.WrapErrorf(err, "action %q input_root_digest", action.GetNodeId())
		}
		if _, ok := g.availableBlobs[digest.NewKey(inputRootDigest)]; !ok {
			return status.FailedPreconditionErrorf(
				"action %q input_root_digest was not announced in UploadedBlobs", action.GetNodeId())
		}
	}
	seenPaths := make(map[string]struct{}, len(action.GetInputs()))
	var inputPathBytes int64
	for _, input := range action.GetInputs() {
		if err := validateGraphInputPath(input.GetExecPath()); err != nil {
			return status.WrapErrorf(err, "action %q input", action.GetNodeId())
		}
		inputPathBytes += int64(len(input.GetExecPath()))
		if _, ok := seenPaths[input.GetExecPath()]; ok {
			return status.InvalidArgumentErrorf("action %q has duplicate input path %q", action.GetNodeId(), input.GetExecPath())
		}
		seenPaths[input.GetExecPath()] = struct{}{}
		switch input.Value.(type) {
		case *graphpb.InputBinding_SourceDigest:
			if err := digest.Validate(input.GetSourceDigest(), g.digestFunction); err != nil {
				return status.WrapErrorf(err, "action %q input %q", action.GetNodeId(), input.GetExecPath())
			}
		case *graphpb.InputBinding_Produced:
			if input.GetProduced().GetProducerNodeId() == "" || input.GetProduced().GetOutputPath() == "" {
				return status.InvalidArgumentErrorf("action %q input %q has an incomplete producer reference", action.GetNodeId(), input.GetExecPath())
			}
			if err := validateGraphInputPath(input.GetProduced().GetOutputPath()); err != nil {
				return status.WrapErrorf(err, "action %q input %q producer output", action.GetNodeId(), input.GetExecPath())
			}
			inputPathBytes += int64(len(input.GetProduced().GetOutputPath()))
		default:
			return status.InvalidArgumentErrorf("action %q input %q has no value", action.GetNodeId(), input.GetExecPath())
		}
	}

	data, err := proto.Marshal(action)
	if err != nil {
		return status.WrapError(err, "marshal ActionNode")
	}
	if existing := g.nodes[action.GetNodeId()]; existing != nil {
		if !slices.Equal(existing.definitionData, data) {
			return status.InvalidArgumentErrorf("action node %q was redeclared with different contents", action.GetNodeId())
		}
		return nil
	}
	if len(g.nodes) >= graphExecutionMaxNodes {
		return status.ResourceExhaustedErrorf("graph exceeds the limit of %d action nodes", graphExecutionMaxNodes)
	}
	if g.totalNodeIDBytes+int64(len(action.GetNodeId())) > graphExecutionMaxNodeIDBytesPerGraph {
		return status.ResourceExhaustedErrorf(
			"graph action node IDs exceed the limit of %d bytes",
			graphExecutionMaxNodeIDBytesPerGraph)
	}
	if g.totalInputBindings+len(action.GetInputs()) > graphExecutionMaxInputBindings {
		return status.ResourceExhaustedErrorf(
			"graph exceeds the limit of %d input bindings", graphExecutionMaxInputBindings)
	}
	if g.totalInputPathBytes+inputPathBytes > graphExecutionMaxInputPathBytesPerGraph {
		return status.ResourceExhaustedErrorf(
			"graph input paths exceed the limit of %d bytes", graphExecutionMaxInputPathBytesPerGraph)
	}
	node := &graphNodeState{
		definition:     proto.Clone(action).(*graphpb.ActionNode),
		definitionData: data,
	}
	g.nodes[action.GetNodeId()] = node
	g.totalNodeIDBytes += int64(len(action.GetNodeId()))
	g.totalInputBindings += len(action.GetInputs())
	g.totalInputPathBytes += inputPathBytes
	for _, input := range action.GetInputs() {
		if source := input.GetSourceDigest(); source != nil {
			if digest.IsEmptyHash(source, g.digestFunction) {
				continue
			}
			if _, available := g.availableBlobs[digest.NewKey(source)]; !available {
				node.unresolvedPrerequisites++
				addGraphNodeCount(g.sourceConsumers, digest.NewKey(source), action.GetNodeId(), 1)
			}
		}
		if produced := input.GetProduced(); produced != nil {
			producer := g.nodes[produced.GetProducerNodeId()]
			if producer == nil || !producer.completed {
				node.unresolvedPrerequisites++
				g.producerConsumers[produced.GetProducerNodeId()] = append(
					g.producerConsumers[produced.GetProducerNodeId()],
					graphProducedConsumer{nodeID: action.GetNodeId(), outputPath: produced.GetOutputPath()},
				)
			} else if g.outputs[produced.GetProducerNodeId()][produced.GetOutputPath()] == nil {
				// The producer is already terminal and omitted this output, so
				// this prerequisite can never resolve. Commit quiescence will
				// report the malformed graph.
				node.unresolvedPrerequisites++
			}
		}
	}
	for _, dependency := range action.GetSchedulingDependencies() {
		dep := g.nodes[dependency]
		if dep == nil || !dep.completed {
			node.unresolvedPrerequisites++
			addGraphNodeCount(g.schedulingDependents, dependency, action.GetNodeId(), 1)
		}
	}
	if node.unresolvedPrerequisites == 0 {
		if err := g.materializeNodeInputs(action.GetNodeId(), node); err != nil {
			return err
		}
		g.markNodeDirty(action.GetNodeId())
	}
	return nil
}

func (g *graphExecution) handleCommit(commit *graphpb.CommitGraph) error {
	if commit.GetExpectedActionCount() != uint64(len(g.nodes)) {
		return status.InvalidArgumentErrorf("CommitGraph expected %d actions but received %d", commit.GetExpectedActionCount(), len(g.nodes))
	}
	if err := g.validateCompleteGraph(); err != nil {
		return err
	}
	g.committed = true
	for nodeID, node := range g.nodes {
		if node.definition.GetDoNotCache() {
			g.markNodeDirty(nodeID)
		}
	}
	return g.sendProgress()
}

func (g *graphExecution) validateCompleteGraph() error {
	for _, root := range g.roots {
		node := g.nodes[root.GetNodeId()]
		if node == nil {
			return status.InvalidArgumentErrorf("root references unknown action %q", root.GetNodeId())
		}
		if root.GetOutputPath() == "" {
			return status.InvalidArgumentErrorf("root for action %q has an empty output path", root.GetNodeId())
		}
		if err := validateGraphPath(root.GetOutputPath()); err != nil {
			return status.WrapErrorf(err, "root for action %q", root.GetNodeId())
		}
	}
	for id, node := range g.nodes {
		for _, input := range node.definition.GetInputs() {
			if source := input.GetSourceDigest(); source != nil {
				if !digest.IsEmptyHash(source, g.digestFunction) {
					if _, ok := g.availableBlobs[digest.NewKey(source)]; !ok {
						return status.FailedPreconditionErrorf(
							"action %q source input %q was not announced in UploadedBlobs", id, input.GetExecPath())
					}
				}
			}
			if produced := input.GetProduced(); produced != nil {
				if g.nodes[produced.GetProducerNodeId()] == nil {
					return status.InvalidArgumentErrorf("action %q references unknown producer %q", id, produced.GetProducerNodeId())
				}
				if !graphCommandDeclaresFileOutput(
					g.nodes[produced.GetProducerNodeId()].definition.GetCommand(),
					produced.GetOutputPath(),
				) {
					return status.InvalidArgumentErrorf(
						"action %q references undeclared file output %q of producer %q",
						id, produced.GetOutputPath(), produced.GetProducerNodeId())
				}
			}
		}
		for _, dep := range node.definition.GetSchedulingDependencies() {
			if g.nodes[dep] == nil {
				return status.InvalidArgumentErrorf("action %q references unknown scheduling dependency %q", id, dep)
			}
		}
	}

	visiting := make(map[string]bool, len(g.nodes))
	visited := make(map[string]bool, len(g.nodes))
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return status.InvalidArgumentErrorf("graph contains a cycle involving action %q", id)
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		node := g.nodes[id]
		for _, input := range node.definition.GetInputs() {
			if produced := input.GetProduced(); produced != nil {
				if err := visit(produced.GetProducerNodeId()); err != nil {
					return err
				}
			}
		}
		for _, dep := range node.definition.GetSchedulingDependencies() {
			if err := visit(dep); err != nil {
				return err
			}
		}
		visiting[id] = false
		visited[id] = true
		return nil
	}
	for id := range g.nodes {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func graphCommandDeclaresFileOutput(command *repb.Command, outputPath string) bool {
	if slices.Contains(command.GetOutputFiles(), outputPath) {
		return true
	}
	// Bazel may use the unified output_paths field. The graph prototype treats
	// a matching ActionResult.OutputFile as a file and detects quiescence if the
	// executor instead returns a directory or no output at this path.
	return slices.Contains(command.GetOutputPaths(), outputPath)
}

func (g *graphExecution) checkCommittedQuiescence() error {
	if !g.committed || g.finished.Load() || g.allNodesCompleted() || g.inFlight > 0 {
		return nil
	}
	return status.FailedPreconditionError("committed graph has unresolved inputs and no runnable or in-flight actions")
}

func (g *graphExecution) scheduleReadyNodes() error {
	for len(g.dirtyNodes) > 0 && g.inFlight < graphExecutionMaxInFlightActions {
		nodeID := g.dirtyNodes[0]
		g.dirtyNodes = g.dirtyNodes[1:]
		delete(g.dirtyNodeSet, nodeID)
		node := g.nodes[nodeID]
		if node == nil {
			continue
		}
		if node.running || node.completed {
			continue
		}
		// do_not_cache work cannot be merged or recovered from the AC, so don't
		// spend executor capacity until the complete graph has been validated.
		if node.definition.GetDoNotCache() && !g.committed {
			continue
		}
		if node.unresolvedPrerequisites != 0 {
			continue
		}
		if err := g.materializeNodeInputs(nodeID, node); err != nil {
			g.markNodeDirty(nodeID)
			return err
		}
		g.observeFirstReadyNode()
		metrics.GraphExecutionReadyNodesCount.Inc()
		startNode := g.startNode
		if g.startNodeForTesting != nil {
			startNode = g.startNodeForTesting
		}
		if err := startNode(nodeID, node, node.materializedInputs); err != nil {
			g.markNodeDirty(nodeID)
			return err
		}
	}
	return nil
}

func (g *graphExecution) materializeNodeInputs(nodeID string, node *graphNodeState) error {
	if node.inputsMaterialized {
		return nil
	}
	inputs := make([]graphInputFile, 0, len(node.definition.GetInputs()))
	for _, binding := range node.definition.GetInputs() {
		var artifact *graphArtifact
		inputExecutable := binding.GetIsExecutable()
		if source := binding.GetSourceDigest(); source != nil {
			if digest.IsEmptyHash(source, g.digestFunction) {
				artifact = &graphArtifact{digest: source, isExecutable: binding.GetIsExecutable()}
			} else if _, ok := g.availableBlobs[digest.NewKey(source)]; ok {
				artifact = &graphArtifact{digest: source, isExecutable: binding.GetIsExecutable()}
			}
		} else if produced := binding.GetProduced(); produced != nil {
			artifact = g.outputs[produced.GetProducerNodeId()][produced.GetOutputPath()]
			// Bazel's ordinary remote execution Merkle tree currently marks
			// every input FileNode executable, including generated inputs.
			// Match that behavior so graph and Execute modes compute the
			// same downstream Action digest.
			inputExecutable = true
		}
		if artifact == nil {
			return status.InternalErrorf(
				"action %q reached zero unresolved prerequisites with unavailable input %q",
				nodeID, binding.GetExecPath())
		}
		inputs = append(inputs, graphInputFile{
			path:         binding.GetExecPath(),
			digest:       artifact.digest,
			isExecutable: inputExecutable,
		})
	}
	node.materializedInputs = inputs
	node.inputsMaterialized = true
	return nil
}

func (g *graphExecution) markNodeDirty(nodeID string) {
	if _, exists := g.dirtyNodeSet[nodeID]; exists {
		return
	}
	g.dirtyNodeSet[nodeID] = struct{}{}
	g.dirtyNodes = append(g.dirtyNodes, nodeID)
}

func addGraphNodeCount[K comparable](index map[K]map[string]int, key K, nodeID string, count int) {
	if index[key] == nil {
		index[key] = make(map[string]int)
	}
	index[key][nodeID] += count
}

func (g *graphExecution) resolveNodePrerequisites(nodeID string, count int) error {
	node := g.nodes[nodeID]
	if node == nil {
		return status.InternalErrorf("resolve prerequisites for unknown action %q", nodeID)
	}
	if count < 0 || count > node.unresolvedPrerequisites {
		return status.InternalErrorf(
			"resolve %d prerequisites for action %q with %d unresolved",
			count, nodeID, node.unresolvedPrerequisites)
	}
	node.unresolvedPrerequisites -= count
	if node.unresolvedPrerequisites != 0 {
		return nil
	}
	if err := g.materializeNodeInputs(nodeID, node); err != nil {
		return err
	}
	g.markNodeDirty(nodeID)
	return nil
}

func (g *graphExecution) resolveSourceConsumers(key digest.Key) error {
	consumers := g.sourceConsumers[key]
	delete(g.sourceConsumers, key)
	for nodeID, count := range consumers {
		if err := g.resolveNodePrerequisites(nodeID, count); err != nil {
			return err
		}
	}
	return nil
}

func (g *graphExecution) resolveDownstreamPrerequisites(nodeID string) error {
	consumers := g.producerConsumers[nodeID]
	delete(g.producerConsumers, nodeID)
	for _, consumer := range consumers {
		if g.outputs[nodeID][consumer.outputPath] == nil {
			continue
		}
		if err := g.resolveNodePrerequisites(consumer.nodeID, 1); err != nil {
			return err
		}
	}
	dependents := g.schedulingDependents[nodeID]
	delete(g.schedulingDependents, nodeID)
	for dependentID, count := range dependents {
		if err := g.resolveNodePrerequisites(dependentID, count); err != nil {
			return err
		}
	}
	return nil
}

func (g *graphExecution) startNode(nodeID string, node *graphNodeState, inputs []graphInputFile) error {
	commandDigest, err := cachetools.UploadProtoToCAS(g.ctx, g.server.cache, g.instanceName, g.digestFunction, node.definition.GetCommand())
	if err != nil {
		return retriableGraphInfrastructureError(
			status.WrapErrorf(err, "upload command for action %q", nodeID))
	}
	inputRootDigest := node.definition.GetInputRootDigest()
	if inputRootDigest == nil {
		inputRootDigest, err = g.uploadInputRoot(inputs)
		if err != nil {
			return retriableGraphInfrastructureError(
				status.WrapErrorf(err, "upload input root for action %q", nodeID))
		}
	}
	action := &repb.Action{
		CommandDigest:   commandDigest,
		InputRootDigest: inputRootDigest,
		Timeout:         node.definition.GetTimeout(),
		DoNotCache:      node.definition.GetDoNotCache(),
		Salt:            node.definition.GetSalt(),
		Platform:        node.definition.GetPlatform(),
	}
	actionDigest, err := cachetools.UploadProtoToCAS(g.ctx, g.server.cache, g.instanceName, g.digestFunction, action)
	if err != nil {
		return retriableGraphInfrastructureError(
			status.WrapErrorf(err, "upload action %q", nodeID))
	}
	executionID, _, err := g.claimDurableExecution(nodeID, actionDigest)
	if err != nil {
		return err
	}
	node.running = true
	node.actionDigest = actionDigest
	node.executionID = executionID
	g.inFlight++
	metrics.GraphExecutionAvoidedClientExecuteRPCsCount.Inc()
	g.launchNodeExecution(nodeID, node)
	return nil
}

func (g *graphExecution) launchNodeExecution(nodeID string, node *graphNodeState) {
	requestMetadata := node.definition.GetRequestMetadata()
	if requestMetadata == nil {
		requestMetadata = &repb.RequestMetadata{}
	} else {
		requestMetadata = proto.Clone(requestMetadata).(*repb.RequestMetadata)
	}
	if requestMetadata.GetToolInvocationId() == "" {
		requestMetadata.ToolInvocationId = g.invocationID
	}
	actionCtx := bazel_request.OverrideRequestMetadata(g.ctx, requestMetadata)
	internalStream := &graphActionStream{
		ctx:        actionCtx,
		nodeID:     nodeID,
		action:     node.actionDigest,
		completion: g.completions,
	}
	executeRequest := &repb.ExecuteRequest{
		InstanceName:       g.instanceName,
		ActionDigest:       node.actionDigest,
		SkipCacheLookup:    node.definition.GetSkipCacheLookup(),
		ExecutionPolicy:    node.definition.GetExecutionPolicy(),
		ResultsCachePolicy: node.definition.GetResultsCachePolicy(),
		DigestFunction:     g.digestFunction,
	}
	go func() {
		err := g.server.executeGraphNode(
			executeRequest,
			node.executionID,
			node.preparedTask,
			func(request *scpb.EnsureTaskRequest) error {
				return g.storeDurablePreparedTask(nodeID, request)
			},
			internalStream,
		)
		if err != nil && !internalStream.sent {
			completion := graphActionCompletion{
				nodeID:       nodeID,
				actionDigest: node.actionDigest,
				err:          err,
			}
			select {
			case g.completions <- completion:
			case <-actionCtx.Done():
			}
		}
	}()
}

func (g *graphExecution) launchRecoveredNodeExecutions() {
	if !g.needsRecoveredWaiters {
		return
	}
	g.needsRecoveredWaiters = false
	for nodeID, node := range g.nodes {
		if node.running && !node.completed && node.executionID != "" {
			g.launchNodeExecution(nodeID, node)
		}
	}
}

func (g *graphExecution) handleCompletion(completion graphActionCompletion) error {
	node := g.nodes[completion.nodeID]
	if node == nil || !node.running || node.completed {
		return status.InternalErrorf("unexpected completion for action %q", completion.nodeID)
	}
	if completion.err != nil {
		var durabilityErr *graphDurabilityError
		if errors.As(completion.err, &durabilityErr) {
			return completion.err
		}
		var retriableErr *graphRetriableError
		if errors.As(completion.err, &retriableErr) {
			g.needsRecoveredWaiters = true
			return completion.err
		}
		err := status.WrapErrorf(completion.err, "execute action %q", completion.nodeID)
		if _, ok := retriableGraphInfrastructureError(err).(*graphRetriableError); ok {
			g.needsRecoveredWaiters = true
			return &graphRetriableError{err: err}
		}
		node.running = false
		g.inFlight--
		return err
	}
	if completion.response == nil {
		node.running = false
		g.inFlight--
		return status.InternalErrorf("action %q completed without an ExecuteResponse", completion.nodeID)
	}
	node.running = false
	g.inFlight--
	node.completed = true
	g.completedNodes++
	node.response = completion.response
	if completion.response.GetCachedResult() {
		metrics.GraphExecutionCacheHitNodesCount.Inc()
	} else {
		metrics.GraphExecutionExecutedNodesCount.Inc()
	}
	for _, output := range completion.response.GetResult().GetOutputFiles() {
		if output.GetDigest() == nil {
			continue
		}
		if g.outputs[completion.nodeID] == nil {
			g.outputs[completion.nodeID] = make(map[string]*graphArtifact)
		}
		g.outputs[completion.nodeID][output.GetPath()] = &graphArtifact{
			digest:       output.GetDigest(),
			isExecutable: output.GetIsExecutable(),
		}
	}
	succeeded := executeResponseSucceeded(completion.response)
	if succeeded {
		if err := g.resolveDownstreamPrerequisites(completion.nodeID); err != nil {
			return err
		}
	}
	if err := g.send(&graphpb.GraphExecuteResponse{
		Payload: &graphpb.GraphExecuteResponse_NodeResult{
			NodeResult: &graphpb.NodeResult{
				NodeId:          completion.nodeID,
				ActionDigest:    completion.actionDigest,
				ExecuteResponse: completion.response,
			},
		},
	}); err != nil {
		return err
	}
	if !succeeded {
		graphStatus := completion.response.GetStatus()
		if graphStatus.GetCode() == 0 {
			graphStatus = gstatus.Newf(
				codes.FailedPrecondition,
				"action %q exited with code %d",
				completion.nodeID,
				completion.response.GetResult().GetExitCode(),
			).Proto()
		}
		return g.sendFailedGraphResult(graphStatus, completion.nodeID)
	}
	return nil
}

func executeResponseSucceeded(response *repb.ExecuteResponse) bool {
	return response != nil &&
		response.GetStatus().GetCode() == 0 &&
		response.GetResult() != nil &&
		response.GetResult().GetExitCode() == 0
}

func (g *graphExecution) allNodesCompleted() bool {
	return g.completedNodes == len(g.nodes)
}

func (g *graphExecution) completedNodeCount() uint64 {
	return uint64(g.completedNodes)
}

func (g *graphExecution) sendGraphResult() error {
	roots := make([]*graphpb.RootResult, 0, len(g.roots))
	for _, root := range g.roots {
		artifact := g.outputs[root.GetNodeId()][root.GetOutputPath()]
		if artifact == nil {
			return g.sendFailedGraphResult(
				gstatus.Newf(
					codes.FailedPrecondition,
					"root output %q was not produced by action %q",
					root.GetOutputPath(),
					root.GetNodeId(),
				).Proto(),
				"",
			)
		}
		roots = append(roots, &graphpb.RootResult{
			Root:         root,
			Digest:       artifact.digest,
			IsExecutable: artifact.isExecutable,
		})
	}
	return g.sendTerminalResponse(&graphpb.GraphExecuteResponse{
		Payload: &graphpb.GraphExecuteResponse_Result{
			Result: &graphpb.GraphResult{
				Status:         gstatus.New(0, "").Proto(),
				CompletedNodes: uint64(len(g.nodes)),
				Roots:          roots,
			},
		},
	})
}

func (g *graphExecution) sendFailedGraphResult(graphStatus *statuspb.Status, failedNodeID string) error {
	return g.sendTerminalResponse(&graphpb.GraphExecuteResponse{
		Payload: &graphpb.GraphExecuteResponse_Result{
			Result: &graphpb.GraphResult{
				Status:         graphStatus,
				CompletedNodes: g.completedNodeCount(),
				FailedNodeId:   failedNodeID,
			},
		},
	})
}

func (g *graphExecution) sendProgress() error {
	var ready, running, completed uint64
	for _, node := range g.nodes {
		if node.running && !node.completed {
			running++
		}
		if node.completed {
			completed++
		}
		if node.running || node.completed || (node.definition.GetDoNotCache() && !g.committed) {
			continue
		}
		if node.unresolvedPrerequisites == 0 {
			ready++
		}
	}
	return g.send(&graphpb.GraphExecuteResponse{
		Payload: &graphpb.GraphExecuteResponse_Progress{
			Progress: &graphpb.GraphProgress{
				DeclaredNodes:  uint64(len(g.nodes)),
				ReadyNodes:     ready,
				RunningNodes:   running,
				CompletedNodes: completed,
			},
		},
	})
}

func (g *graphExecution) sendTerminalError(err error) error {
	return g.sendTerminalResponse(&graphpb.GraphExecuteResponse{
		Payload: &graphpb.GraphExecuteResponse_Error{
			Error: &graphpb.GraphStreamError{
				Status:   gstatus.Convert(err).Proto(),
				Terminal: true,
			},
		},
	})
}

func (g *graphExecution) sendTerminalResponse(response *graphpb.GraphExecuteResponse) error {
	// Defensively collapse unexpectedly large executor status/result payloads
	// into a bounded terminal error. BeginGraph root limits make successful
	// GraphResult payloads fit the reserved slot, but an executor-supplied
	// status message is not otherwise under this server's control.
	if proto.Size(response) > graphExecutionTerminalResponseHeadroom-256 {
		response = &graphpb.GraphExecuteResponse{
			Payload: &graphpb.GraphExecuteResponse_Error{
				Error: &graphpb.GraphStreamError{
					Status: gstatus.Newf(
						codes.ResourceExhausted,
						"terminal graph result exceeded the reserved %d-byte replay slot",
						graphExecutionTerminalResponseHeadroom,
					).Proto(),
					Terminal: true,
				},
			},
		}
	}
	err := g.send(response)
	var streamErr *graphStreamSendError
	if err != nil && !errors.As(err, &streamErr) {
		// Local replay-capacity checks and durable append failures happen before
		// a terminal response is committed. Keep the session active so a
		// reconnect can retry instead of marking unreplayable state finished.
		return err
	}
	g.finished.Store(true)
	g.observeTerminalCompletion()
	g.finish()
	if streamErr != nil {
		return streamErr.err
	}
	return err
}

func (g *graphExecution) observeFirstReadyNode() {
	if g.firstReadyObserved || g.beginTime.IsZero() {
		return
	}
	g.firstReadyObserved = true
	elapsed := g.server.clock.Since(g.beginTime).Microseconds()
	if elapsed < 0 {
		elapsed = 0
	}
	metrics.GraphExecutionBeginToFirstReadyUsecCount.Add(float64(elapsed))
}

func (g *graphExecution) observeTerminalCompletion() {
	if g.terminalObserved || g.beginTime.IsZero() {
		return
	}
	g.terminalObserved = true
	elapsed := g.server.clock.Since(g.beginTime).Microseconds()
	if elapsed < 0 {
		elapsed = 0
	}
	metrics.GraphExecutionBeginToTerminalCompletionUsecCount.Add(float64(elapsed))
}

func (g *graphExecution) finish() {
	if err := g.finishDurableSession(); err != nil {
		// A terminal response is already committed before finish is called.
		// Relinquish the coordinator lease so another app can retry the
		// conservative retained-state transition immediately.
		g.releaseDurableLease(context.Background())
	}
	g.finishLocalOnce.Do(func() {
		if g.disconnectTimer != nil {
			g.disconnectTimer.Stop()
			g.disconnectTimer = nil
		}
		g.cancelGraphWaiters()
		g.compactTerminalState()
		g.server.clock.AfterFunc(graphExecutionFinishedSessionLifetime, func() {
			g.removeSession()
		})
	})
}

func (g *graphExecution) compactTerminalState() {
	if !g.retainedAt.IsZero() {
		return
	}
	// Preserve the complete response stream while the terminal session remains
	// retained. Dropping intermediate responses would leave sequence-number
	// gaps and make a post-completion resume unable to reconstruct the result.
	// The aggregate retained-session budgets below evict whole sessions instead
	// of weakening replay semantics.
	g.requestData = nil
	g.requestReplayBytes = 0
	g.roots = nil
	g.nodes = nil
	g.availableBlobs = nil
	g.outputs = nil
	g.completions = nil
	g.dirtyNodes = nil
	g.dirtyNodeSet = nil
	g.sourceConsumers = nil
	g.producerConsumers = nil
	g.schedulingDependents = nil
	g.instanceName = ""
	g.invocationID = ""
	g.stream = nil
	g.connected = false
	g.retainedAt = g.server.clock.Now()
	g.retainedBytes = int64(
		len(g.sessionID)+
			len(g.resumeToken)+
			len(g.ownerPrefix)+
			len(g.responses)*64,
	) + g.responseReplayBytes

	g.server.graphSessionsMu.Lock()
	defer g.server.graphSessionsMu.Unlock()
	if g.server.graphSessions[graphSessionKey(g.ownerPrefix, g.sessionID)] == g {
		g.server.graphRetentionSequence++
		g.retainedOrder = g.server.graphRetentionSequence
		g.server.compactFinishedGraphSessionsLocked()
	}
}

func (s *ExecutionServer) compactFinishedGraphSessionsLocked() {
	type retainedStats struct {
		count int
		bytes int64
	}
	for {
		global := retainedStats{}
		tenants := make(map[string]retainedStats)
		var oldestGlobalKey string
		var oldestGlobal *graphExecution
		oldestTenantKeys := make(map[string]string)
		oldestTenants := make(map[string]*graphExecution)
		for key, session := range s.graphSessions {
			if !session.finished.Load() || session.retainedAt.IsZero() {
				continue
			}
			global.count++
			global.bytes += session.retainedBytes
			tenant := tenants[session.ownerPrefix]
			tenant.count++
			tenant.bytes += session.retainedBytes
			tenants[session.ownerPrefix] = tenant
			if oldestGlobal == nil || session.retainedOrder < oldestGlobal.retainedOrder {
				oldestGlobalKey, oldestGlobal = key, session
			}
			oldestTenant := oldestTenants[session.ownerPrefix]
			if oldestTenant == nil || session.retainedOrder < oldestTenant.retainedOrder {
				oldestTenantKeys[session.ownerPrefix] = key
				oldestTenants[session.ownerPrefix] = session
			}
		}

		evictKey := ""
		for owner, stats := range tenants {
			if stats.count > graphExecutionMaxFinishedSessionsPerTenant ||
				stats.bytes > graphExecutionMaxFinishedBytesPerTenant {
				evictKey = oldestTenantKeys[owner]
				break
			}
		}
		if evictKey == "" &&
			(global.count > graphExecutionMaxFinishedSessionsGlobal ||
				global.bytes > graphExecutionMaxFinishedBytesGlobal) {
			evictKey = oldestGlobalKey
		}
		if evictKey == "" {
			return
		}
		delete(s.graphSessions, evictKey)
	}
}

func (g *graphExecution) cancelGraphWaiters() {
	// Cancel the graph's WaitExecution observers, but deliberately do not call
	// Scheduler.CancelTask. Cacheable actions may be shared by another Execute
	// caller, and checking merge state before cancellation is inherently racy
	// without an atomic "release this waiter and cancel only if unreferenced"
	// scheduler primitive.
	g.cancel()
}

func (g *graphExecution) removeSession() {
	g.server.graphSessionsMu.Lock()
	defer g.server.graphSessionsMu.Unlock()
	sessionKey := graphSessionKey(g.ownerPrefix, g.sessionID)
	if g.server.graphSessions[sessionKey] == g {
		delete(g.server.graphSessions, sessionKey)
	}
}

func (g *graphExecution) send(response *graphpb.GraphExecuteResponse) error {
	if g.reconstructing {
		return nil
	}
	response.SequenceNumber = g.responseSequence + 1
	response.AckRequestSequence = g.lastRequestSequence
	terminal := response.GetResult() != nil || response.GetError().GetTerminal()
	maxResponses := graphExecutionMaxReplayResponses
	maxResponseBytes := int64(graphExecutionMaxResponseReplayBytes)
	if !terminal {
		maxResponses--
		maxResponseBytes -= graphExecutionTerminalResponseHeadroom
	}
	if len(g.responses) >= maxResponses {
		return status.ResourceExhaustedErrorf(
			"graph exceeds the limit of %d non-terminal retained responses", maxResponses)
	}
	responseSize := int64(proto.Size(response))
	if terminal && responseSize > graphExecutionTerminalResponseHeadroom {
		return status.ResourceExhaustedErrorf(
			"terminal graph response exceeds reserved headroom of %d bytes",
			graphExecutionTerminalResponseHeadroom)
	}
	if g.responseReplayBytes+responseSize > maxResponseBytes {
		return status.ResourceExhaustedErrorf(
			"graph response replay data exceeds %d bytes", maxResponseBytes)
	}
	data, err := proto.Marshal(response)
	if err != nil {
		return status.WrapError(err, "marshal GraphExecuteResponse")
	}
	if err := g.appendDurableResponse(response, data); err != nil {
		return err
	}
	g.responseSequence++
	g.responses = append(g.responses, proto.Clone(response).(*graphpb.GraphExecuteResponse))
	g.responseReplayBytes += responseSize
	if err := g.stream.Send(response); err != nil {
		return &graphStreamSendError{err: err}
	}
	recordGraphResponseMessage(response)
	return nil
}

func graphSessionKey(ownerPrefix, sessionID string) string {
	return ownerPrefix + "\x00" + sessionID
}

func recordGraphRequestMessage(request *graphpb.GraphExecuteRequest) {
	metrics.GraphExecutionRequestMessagesCount.Inc()
	metrics.GraphExecutionRequestBytesCount.Add(float64(proto.Size(request)))
}

func recordGraphResponseMessage(response *graphpb.GraphExecuteResponse) {
	metrics.GraphExecutionResponseMessagesCount.Inc()
	metrics.GraphExecutionResponseBytesCount.Add(float64(proto.Size(response)))
}

func validateGraphPath(p string) error {
	if p == "" || p == "." || p[0] == '/' || path.Clean(p) != p {
		return status.InvalidArgumentErrorf("path %q must be normalized and relative", p)
	}
	return nil
}

func validateGraphInputPath(p string) error {
	if err := validateGraphPath(p); err != nil {
		return err
	}
	if len(p) > graphExecutionMaxInputPathBytes {
		return status.ResourceExhaustedErrorf(
			"path exceeds the limit of %d bytes", graphExecutionMaxInputPathBytes)
	}
	components := strings.Split(p, "/")
	if len(components) > graphExecutionMaxInputPathDepth {
		return status.ResourceExhaustedErrorf(
			"path exceeds the limit of %d components", graphExecutionMaxInputPathDepth)
	}
	for _, component := range components {
		if len(component) > graphExecutionMaxInputPathComponentBytes {
			return status.ResourceExhaustedErrorf(
				"path component exceeds the limit of %d bytes", graphExecutionMaxInputPathComponentBytes)
		}
	}
	return nil
}

type graphInputFile struct {
	path         string
	digest       *repb.Digest
	isExecutable bool
}

type graphInputDirectory struct {
	files       map[string]*repb.FileNode
	directories map[string]*graphInputDirectory
}

func newGraphInputDirectory() *graphInputDirectory {
	return &graphInputDirectory{
		files:       make(map[string]*repb.FileNode),
		directories: make(map[string]*graphInputDirectory),
	}
}

func (g *graphExecution) uploadInputRoot(inputs []graphInputFile) (*repb.Digest, error) {
	root := newGraphInputDirectory()
	directoryCount := 1
	for _, input := range inputs {
		parts := strings.Split(input.path, "/")
		current := root
		for _, part := range parts[:len(parts)-1] {
			if current.files[part] != nil {
				return nil, status.InvalidArgumentErrorf("input path %q conflicts with file %q", input.path, part)
			}
			if current.directories[part] == nil {
				current.directories[part] = newGraphInputDirectory()
				directoryCount++
				if directoryCount > graphExecutionMaxInputDirectoriesPerAction {
					return nil, status.ResourceExhaustedErrorf(
						"action input tree exceeds the limit of %d directories",
						graphExecutionMaxInputDirectoriesPerAction)
				}
			}
			current = current.directories[part]
		}
		name := parts[len(parts)-1]
		if current.directories[name] != nil || current.files[name] != nil {
			return nil, status.InvalidArgumentErrorf("input path %q conflicts with another input", input.path)
		}
		current.files[name] = &repb.FileNode{
			Name:         name,
			Digest:       input.digest,
			IsExecutable: input.isExecutable,
		}
	}
	if g.totalInputDirectories+directoryCount > graphExecutionMaxInputDirectoriesPerGraph {
		return nil, status.ResourceExhaustedErrorf(
			"graph input trees exceed the limit of %d directories",
			graphExecutionMaxInputDirectoriesPerGraph)
	}
	g.totalInputDirectories += directoryCount
	return g.uploadInputDirectory(root)
}

func (g *graphExecution) uploadInputDirectory(dir *graphInputDirectory) (*repb.Digest, error) {
	type frame struct {
		dir            *graphInputDirectory
		directoryNames []string
		nextChild      int
	}
	digests := make(map[*graphInputDirectory]*repb.Digest)
	stack := []frame{{
		dir:            dir,
		directoryNames: slices.Sorted(maps.Keys(dir.directories)),
	}}
	for len(stack) > 0 {
		current := &stack[len(stack)-1]
		if current.nextChild < len(current.directoryNames) {
			childName := current.directoryNames[current.nextChild]
			current.nextChild++
			child := current.dir.directories[childName]
			stack = append(stack, frame{
				dir:            child,
				directoryNames: slices.Sorted(maps.Keys(child.directories)),
			})
			continue
		}

		directory := &repb.Directory{}
		for _, name := range slices.Sorted(maps.Keys(current.dir.files)) {
			directory.Files = append(directory.Files, current.dir.files[name])
		}
		for _, name := range current.directoryNames {
			directory.Directories = append(directory.Directories, &repb.DirectoryNode{
				Name:   name,
				Digest: digests[current.dir.directories[name]],
			})
		}
		directoryDigest, err := cachetools.UploadProtoToCAS(
			g.ctx, g.server.cache, g.instanceName, g.digestFunction, directory)
		if err != nil {
			return nil, err
		}
		digests[current.dir] = directoryDigest
		stack = stack[:len(stack)-1]
	}
	return digests[dir], nil
}
