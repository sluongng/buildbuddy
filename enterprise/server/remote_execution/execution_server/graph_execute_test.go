package execution_server_test

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/execution_server"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/operation"
	"github.com/buildbuddy-io/buildbuddy/server/metrics"
	"github.com/buildbuddy-io/buildbuddy/server/remote_cache/cachetools"
	"github.com/buildbuddy-io/buildbuddy/server/remote_cache/digest"
	"github.com/buildbuddy-io/buildbuddy/server/testutil/testauth"
	"github.com/buildbuddy-io/buildbuddy/server/testutil/testenv"
	"github.com/buildbuddy-io/buildbuddy/server/util/prefix"
	"github.com/buildbuddy-io/buildbuddy/server/util/proto"
	"github.com/buildbuddy-io/buildbuddy/server/util/usageutil"
	"github.com/jonboulle/clockwork"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	gstatus "google.golang.org/grpc/status"

	graphpb "github.com/buildbuddy-io/buildbuddy/proto/graph_execution"
	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
	rspb "github.com/buildbuddy-io/buildbuddy/proto/resource"
	scpb "github.com/buildbuddy-io/buildbuddy/proto/scheduler"
)

const (
	graphTestInstanceName = "graph-test"
	graphTestSessionID    = "graph-test-session"
)

func registerGraphExecutionServer(server grpc.ServiceRegistrar, executionServer *execution_server.ExecutionServer) {
	graphpb.RegisterGraphExecutionServer(server, executionServer)
}

func graphTestContexts(t *testing.T, env *testenv.TestEnv) (context.Context, context.Context) {
	t.Helper()
	auth := env.GetAuthenticator().(*testauth.TestAuthenticator)
	clientCtx, err := auth.WithAuthenticatedUser(context.Background(), "US1")
	require.NoError(t, err)
	clientCtx, cancel := context.WithTimeout(clientCtx, 10*time.Second)
	t.Cleanup(cancel)
	cacheCtx, err := prefix.AttachUserPrefixToContext(clientCtx, auth)
	require.NoError(t, err)
	return clientCtx, cacheCtx
}

func uploadGraphTestBlob(t *testing.T, env *testenv.TestEnv, ctx context.Context, data string) *repb.Digest {
	t.Helper()
	d, err := cachetools.UploadBytesToCache(
		ctx,
		env.GetCache(),
		rspb.CacheType_CAS,
		graphTestInstanceName,
		repb.DigestFunction_SHA256,
		bytes.NewReader([]byte(data)),
	)
	require.NoError(t, err)
	return d
}

func uploadGraphTestProto(t *testing.T, env *testenv.TestEnv, ctx context.Context, message proto.Message) *repb.Digest {
	t.Helper()
	d, err := cachetools.UploadProtoToCAS(
		ctx,
		env.GetCache(),
		graphTestInstanceName,
		repb.DigestFunction_SHA256,
		message,
	)
	require.NoError(t, err)
	return d
}

func cacheGraphTestActionResult(t *testing.T, env *testenv.TestEnv, ctx context.Context, actionDigest *repb.Digest, result *repb.ActionResult) {
	t.Helper()
	data, err := proto.Marshal(result)
	require.NoError(t, err)
	err = env.GetCache().Set(
		ctx,
		digest.NewACResourceName(actionDigest, graphTestInstanceName, repb.DigestFunction_SHA256).ToProto(),
		data,
	)
	require.NoError(t, err)
}

type graphTestMetricSnapshot struct {
	requestMessages               float64
	responseMessages              float64
	requestBytes                  float64
	responseBytes                 float64
	readyNodes                    float64
	beginToFirstReadyUsec         float64
	beginToTerminalCompletionUsec float64
	cacheHitNodes                 float64
	executedNodes                 float64
	avoidedExecuteRPCs            float64
}

func snapshotGraphTestMetrics() graphTestMetricSnapshot {
	return graphTestMetricSnapshot{
		requestMessages:               testutil.ToFloat64(metrics.GraphExecutionRequestMessagesCount),
		responseMessages:              testutil.ToFloat64(metrics.GraphExecutionResponseMessagesCount),
		requestBytes:                  testutil.ToFloat64(metrics.GraphExecutionRequestBytesCount),
		responseBytes:                 testutil.ToFloat64(metrics.GraphExecutionResponseBytesCount),
		readyNodes:                    testutil.ToFloat64(metrics.GraphExecutionReadyNodesCount),
		beginToFirstReadyUsec:         testutil.ToFloat64(metrics.GraphExecutionBeginToFirstReadyUsecCount),
		beginToTerminalCompletionUsec: testutil.ToFloat64(metrics.GraphExecutionBeginToTerminalCompletionUsecCount),
		cacheHitNodes:                 testutil.ToFloat64(metrics.GraphExecutionCacheHitNodesCount),
		executedNodes:                 testutil.ToFloat64(metrics.GraphExecutionExecutedNodesCount),
		avoidedExecuteRPCs:            testutil.ToFloat64(metrics.GraphExecutionAvoidedClientExecuteRPCsCount),
	}
}

func receiveGraphTestScheduledTask(t *testing.T, env *testenv.TestEnv) (*scpb.ScheduleTaskRequest, *repb.ExecutionTask) {
	t.Helper()
	scheduler := env.GetSchedulerService().(*schedulerServerMock)
	select {
	case request := <-scheduler.scheduleReqCh:
		task := &repb.ExecutionTask{}
		require.NoError(t, proto.Unmarshal(request.GetSerializedTask(), task))
		return request, task
	case <-time.After(5 * time.Second):
		require.FailNow(t, "timed out waiting for graph node to be scheduled")
		return nil, nil
	}
}

func publishGraphTestCompletion(
	t *testing.T,
	conn *grpc.ClientConn,
	ctx context.Context,
	request *scpb.ScheduleTaskRequest,
	task *repb.ExecutionTask,
	exitCode int32,
	outputs ...*repb.OutputFile,
) {
	t.Helper()
	executorCtx := metadata.AppendToOutgoingContext(ctx, usageutil.ClientHeaderName, "executor")
	stream, err := repb.NewExecutionClient(conn).PublishOperation(executorCtx)
	require.NoError(t, err)
	op, err := operation.Assemble(
		request.GetTaskId(),
		operation.Metadata(repb.ExecutionStage_COMPLETED, task.GetExecuteRequest().GetActionDigest()),
		&repb.ExecuteResponse{
			Result: &repb.ActionResult{
				ExitCode:    exitCode,
				OutputFiles: outputs,
			},
		},
	)
	require.NoError(t, err)
	require.NoError(t, stream.Send(op))
	_, err = stream.CloseAndRecv()
	require.NoError(t, err)
}

func prepareCachedGraphTestAction(
	t *testing.T,
	env *testenv.TestEnv,
	ctx context.Context,
	command *repb.Command,
	inputs []*repb.FileNode,
	outputPath string,
	outputContents string,
) (*repb.Digest, *repb.Digest) {
	t.Helper()
	slices.SortFunc(inputs, func(a, b *repb.FileNode) int {
		if a.GetName() < b.GetName() {
			return -1
		}
		if a.GetName() > b.GetName() {
			return 1
		}
		return 0
	})
	inputRootDigest := uploadGraphTestProto(t, env, ctx, &repb.Directory{Files: inputs})
	commandDigest := uploadGraphTestProto(t, env, ctx, command)
	actionDigest := uploadGraphTestProto(t, env, ctx, &repb.Action{
		CommandDigest:   commandDigest,
		InputRootDigest: inputRootDigest,
	})
	outputDigest := uploadGraphTestBlob(t, env, ctx, outputContents)
	cacheGraphTestActionResult(t, env, ctx, actionDigest, &repb.ActionResult{
		ExitCode: 0,
		OutputFiles: []*repb.OutputFile{{
			Path:   outputPath,
			Digest: outputDigest,
		}},
	})
	return actionDigest, outputDigest
}

func sendGraphTestRequest(
	t *testing.T,
	stream graphpb.GraphExecution_GraphExecuteClient,
	sequence uint64,
	payload any,
) {
	t.Helper()
	request := &graphpb.GraphExecuteRequest{
		SessionId:      graphTestSessionID,
		SequenceNumber: sequence,
	}
	switch payload := payload.(type) {
	case *graphpb.GraphExecuteRequest_Begin:
		request.Payload = payload
	case *graphpb.GraphExecuteRequest_UploadedBlobs:
		request.Payload = payload
	case *graphpb.GraphExecuteRequest_Action:
		request.Payload = payload
	case *graphpb.GraphExecuteRequest_Commit:
		request.Payload = payload
	default:
		require.FailNowf(t, "unsupported GraphExecute request payload", "%T", payload)
	}
	require.NoError(t, stream.Send(request))
}

func beginGraphTest(
	t *testing.T,
	stream graphpb.GraphExecution_GraphExecuteClient,
	roots ...*graphpb.RootOutput,
) *graphpb.GraphExecuteResponse {
	t.Helper()
	sendGraphTestRequest(t, stream, 1, &graphpb.GraphExecuteRequest_Begin{
		Begin: &graphpb.BeginGraph{
			ProtocolVersion: 1,
			InstanceName:    graphTestInstanceName,
			DigestFunction:  repb.DigestFunction_SHA256,
			Roots:           roots,
			InvocationId:    "graph-test-invocation",
			ResumeToken:     bytes.Repeat([]byte{0x42}, 32),
		},
	})
	response, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, graphTestSessionID, response.GetBegin().GetSessionId())
	require.NotEmpty(t, response.GetBegin().GetResumeToken())
	require.Equal(t, uint32(1), response.GetBegin().GetProtocolVersion())
	return response
}

func resumeGraphTest(
	t *testing.T,
	conn *grpc.ClientConn,
	ctx context.Context,
	resumeToken []byte,
) (graphpb.GraphExecution_GraphExecuteClient, *graphpb.GraphExecuteResponse) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&graphpb.GraphExecuteRequest{
			SessionId:      graphTestSessionID,
			SequenceNumber: 0,
			Payload: &graphpb.GraphExecuteRequest_Resume{
				Resume: &graphpb.ResumeGraph{
					SessionId:            graphTestSessionID,
					ResumeToken:          resumeToken,
					LastResponseSequence: 0,
				},
			},
		}))
		response, err := stream.Recv()
		if gstatus.Code(err) == codes.AlreadyExists && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		require.NoError(t, err)
		require.NotNil(t, response.GetBegin())
		return stream, response
	}
}

func receiveGraphTestResult(
	t *testing.T,
	stream graphpb.GraphExecution_GraphExecuteClient,
) ([]string, *graphpb.GraphResult) {
	t.Helper()
	var completed []string
	for {
		response, err := stream.Recv()
		require.NoError(t, err)
		if streamError := response.GetError(); streamError != nil {
			require.FailNowf(t, "GraphExecute failed", "%s", streamError.GetStatus())
		}
		if node := response.GetNodeResult(); node != nil {
			completed = append(completed, node.GetNodeId())
		}
		if result := response.GetResult(); result != nil {
			return completed, result
		}
	}
}

func TestGraphExecute_OneNodeCacheHit(t *testing.T) {
	before := snapshotGraphTestMetrics()
	env, conn, _ := setupEnv(t)
	clientCtx, cacheCtx := graphTestContexts(t, env)
	sourceDigest := uploadGraphTestBlob(t, env, cacheCtx, "source")
	command := &repb.Command{
		Arguments:   []string{"/bin/cp", "src.txt", "out.txt"},
		OutputFiles: []string{"out.txt"},
	}
	actionDigest, outputDigest := prepareCachedGraphTestAction(
		t,
		env,
		cacheCtx,
		command,
		[]*repb.FileNode{{Name: "src.txt", Digest: sourceDigest}},
		"out.txt",
		"output",
	)

	stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(clientCtx)
	require.NoError(t, err)
	_ = beginGraphTest(t, stream, &graphpb.RootOutput{NodeId: "copy", OutputPath: "out.txt"})
	sendGraphTestRequest(t, stream, 2, &graphpb.GraphExecuteRequest_UploadedBlobs{
		UploadedBlobs: &graphpb.UploadedBlobs{Digests: []*repb.Digest{sourceDigest}},
	})
	sendGraphTestRequest(t, stream, 3, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:  "copy",
			Command: command,
			Inputs: []*graphpb.InputBinding{{
				ExecPath: "src.txt",
				Value:    &graphpb.InputBinding_SourceDigest{SourceDigest: sourceDigest},
			}},
		},
	})
	nodeResponse, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, "copy", nodeResponse.GetNodeResult().GetNodeId())
	require.True(t, digest.Equal(actionDigest, nodeResponse.GetNodeResult().GetActionDigest()))
	sendGraphTestRequest(t, stream, 4, &graphpb.GraphExecuteRequest_Commit{
		Commit: &graphpb.CommitGraph{ExpectedActionCount: 1},
	})
	require.NoError(t, stream.CloseSend())

	completed, result := receiveGraphTestResult(t, stream)
	require.Empty(t, completed)
	require.Equal(t, uint64(1), result.GetCompletedNodes())
	require.Len(t, result.GetRoots(), 1)
	require.True(t, digest.Equal(outputDigest, result.GetRoots()[0].GetDigest()))
	after := snapshotGraphTestMetrics()
	require.Equal(t, float64(4), after.requestMessages-before.requestMessages)
	require.Equal(t, float64(4), after.responseMessages-before.responseMessages)
	require.Greater(t, after.requestBytes, before.requestBytes)
	require.Greater(t, after.responseBytes, before.responseBytes)
	require.Equal(t, float64(1), after.readyNodes-before.readyNodes)
	require.Equal(t, float64(1), after.cacheHitNodes-before.cacheHitNodes)
	require.Equal(t, float64(0), after.executedNodes-before.executedNodes)
	require.Equal(t, float64(1), after.avoidedExecuteRPCs-before.avoidedExecuteRPCs)
}

func TestGraphExecute_HighFanInConsumerDeclaredBeforeProducers(t *testing.T) {
	env, conn, _ := setupEnv(t)
	clientCtx, cacheCtx := graphTestContexts(t, env)

	const producerCount = 128
	producerCommand := &repb.Command{
		Arguments:   []string{"/bin/sh", "-c", "printf output > out"},
		OutputFiles: []string{"out"},
	}
	_, producedDigest := prepareCachedGraphTestAction(
		t, env, cacheCtx, producerCommand, nil, "out", "output",
	)
	consumerCommand := &repb.Command{
		Arguments:   []string{"/bin/sh", "-c", "printf final > final"},
		OutputFiles: []string{"final"},
	}
	consumerInputFiles := make([]*repb.FileNode, 0, producerCount)
	consumerBindings := make([]*graphpb.InputBinding, 0, producerCount)
	for i := 0; i < producerCount; i++ {
		inputPath := "input-" + strconv.Itoa(i)
		producerID := "producer-" + strconv.Itoa(i)
		consumerInputFiles = append(consumerInputFiles, &repb.FileNode{
			Name:         inputPath,
			Digest:       producedDigest,
			IsExecutable: true,
		})
		consumerBindings = append(consumerBindings, &graphpb.InputBinding{
			ExecPath: inputPath,
			Value: &graphpb.InputBinding_Produced{
				Produced: &graphpb.ProducedInput{
					ProducerNodeId: producerID,
					OutputPath:     "out",
				},
			},
		})
	}
	_, finalDigest := prepareCachedGraphTestAction(
		t, env, cacheCtx, consumerCommand, consumerInputFiles, "final", "final",
	)

	stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(clientCtx)
	require.NoError(t, err)
	_ = beginGraphTest(t, stream, &graphpb.RootOutput{
		NodeId: "consumer", OutputPath: "final",
	})
	sendGraphTestRequest(t, stream, 2, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:  "consumer",
			Command: consumerCommand,
			Inputs:  consumerBindings,
		},
	})
	for i := 0; i < producerCount; i++ {
		sendGraphTestRequest(t, stream, uint64(i+3), &graphpb.GraphExecuteRequest_Action{
			Action: &graphpb.ActionNode{
				NodeId:  "producer-" + strconv.Itoa(i),
				Command: producerCommand,
			},
		})
	}
	sendGraphTestRequest(t, stream, producerCount+3, &graphpb.GraphExecuteRequest_Commit{
		Commit: &graphpb.CommitGraph{ExpectedActionCount: producerCount + 1},
	})
	require.NoError(t, stream.CloseSend())

	completed, result := receiveGraphTestResult(t, stream)
	require.Len(t, completed, producerCount+1)
	require.Equal(t, uint64(producerCount+1), result.GetCompletedNodes())
	require.Len(t, result.GetRoots(), 1)
	require.True(t, digest.Equal(finalDigest, result.GetRoots()[0].GetDigest()))
}

func TestGraphExecute_PeriodicDeclarationAcksAdvanceLargeGraphWindow(t *testing.T) {
	env, conn, _ := setupEnv(t)
	clientCtx, _ := graphTestContexts(t, env)
	streamCtx, cancel := context.WithCancel(clientCtx)
	defer cancel()
	stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(streamCtx)
	require.NoError(t, err)
	begin := beginGraphTest(t, stream)
	require.Equal(t, uint64(1), begin.GetAckRequestSequence())

	const actionCount = 1100
	var lastResponseSequence = begin.GetSequenceNumber()
	for i := 0; i < actionCount; i++ {
		sequence := uint64(i + 2)
		sendGraphTestRequest(t, stream, sequence, &graphpb.GraphExecuteRequest_Action{
			Action: &graphpb.ActionNode{
				NodeId:     "deferred-" + strconv.Itoa(i),
				Command:    &repb.Command{Arguments: []string{"true"}},
				DoNotCache: true,
			},
		})
		if sequence%64 != 0 {
			continue
		}
		ack, err := stream.Recv()
		require.NoError(t, err)
		require.Nil(t, ack.GetPayload(), "periodic declaration response must be ack-only")
		require.Equal(t, sequence, ack.GetAckRequestSequence())
		require.Equal(t, lastResponseSequence+1, ack.GetSequenceNumber())
		lastResponseSequence = ack.GetSequenceNumber()
	}
	require.Greater(t, uint64(actionCount), uint64(1024))
	cancel()
}

func TestGraphExecute_RejectsExcessiveInputDirectories(t *testing.T) {
	env, conn, _ := setupEnv(t)
	clientCtx, cacheCtx := graphTestContexts(t, env)
	sourceDigest := uploadGraphTestBlob(t, env, cacheCtx, "source")

	stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(clientCtx)
	require.NoError(t, err)
	_ = beginGraphTest(t, stream)
	sendGraphTestRequest(t, stream, 2, &graphpb.GraphExecuteRequest_UploadedBlobs{
		UploadedBlobs: &graphpb.UploadedBlobs{Digests: []*repb.Digest{sourceDigest}},
	})
	inputs := make([]*graphpb.InputBinding, 0, 10_001)
	for i := 0; i < 10_001; i++ {
		inputs = append(inputs, &graphpb.InputBinding{
			ExecPath: "directory-" + strconv.Itoa(i) + "/input",
			Value:    &graphpb.InputBinding_SourceDigest{SourceDigest: sourceDigest},
		})
	}
	sendGraphTestRequest(t, stream, 3, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:  "too-many-directories",
			Command: &repb.Command{Arguments: []string{"/bin/true"}},
			Inputs:  inputs,
		},
	})

	for {
		response, err := stream.Recv()
		require.NoError(t, err)
		if response.GetError() == nil {
			continue
		}
		require.True(t, response.GetError().GetTerminal())
		require.Equal(t, int32(codes.ResourceExhausted), response.GetError().GetStatus().GetCode())
		require.Contains(t, response.GetError().GetStatus().GetMessage(), "directories")
		break
	}
}

func TestGraphExecute_PrecomputedInputRootCacheHit(t *testing.T) {
	env, conn, _ := setupEnv(t)
	clientCtx, cacheCtx := graphTestContexts(t, env)
	sourceDigest := uploadGraphTestBlob(t, env, cacheCtx, "source")
	inputRootDigest := uploadGraphTestProto(t, env, cacheCtx, &repb.Directory{
		Files: []*repb.FileNode{{
			Name:         "src.txt",
			Digest:       sourceDigest,
			IsExecutable: true,
		}},
	})
	command := &repb.Command{
		Arguments:   []string{"/bin/cp", "src.txt", "out.txt"},
		OutputFiles: []string{"out.txt"},
	}
	commandDigest := uploadGraphTestProto(t, env, cacheCtx, command)
	actionDigest := uploadGraphTestProto(t, env, cacheCtx, &repb.Action{
		CommandDigest:   commandDigest,
		InputRootDigest: inputRootDigest,
	})
	outputDigest := uploadGraphTestBlob(t, env, cacheCtx, "output")
	cacheGraphTestActionResult(t, env, cacheCtx, actionDigest, &repb.ActionResult{
		ExitCode: 0,
		OutputFiles: []*repb.OutputFile{{
			Path:   "out.txt",
			Digest: outputDigest,
		}},
	})

	stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(clientCtx)
	require.NoError(t, err)
	_ = beginGraphTest(t, stream, &graphpb.RootOutput{NodeId: "copy", OutputPath: "out.txt"})
	sendGraphTestRequest(t, stream, 2, &graphpb.GraphExecuteRequest_UploadedBlobs{
		UploadedBlobs: &graphpb.UploadedBlobs{Digests: []*repb.Digest{inputRootDigest}},
	})
	sendGraphTestRequest(t, stream, 3, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:          "copy",
			Command:         command,
			InputRootDigest: inputRootDigest,
		},
	})

	// A cache hit on this exact digest proves the graph server reused the
	// ordinary REAPI Merkle root rather than reconstructing a different one.
	nodeResponse, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, "copy", nodeResponse.GetNodeResult().GetNodeId())
	require.True(t, digest.Equal(actionDigest, nodeResponse.GetNodeResult().GetActionDigest()))
	sendGraphTestRequest(t, stream, 4, &graphpb.GraphExecuteRequest_Commit{
		Commit: &graphpb.CommitGraph{ExpectedActionCount: 1},
	})
	require.NoError(t, stream.CloseSend())

	completed, result := receiveGraphTestResult(t, stream)
	require.Empty(t, completed)
	require.Len(t, result.GetRoots(), 1)
	require.True(t, digest.Equal(outputDigest, result.GetRoots()[0].GetDigest()))
}

func TestGraphExecute_PrecomputedInputRootValidation(t *testing.T) {
	for _, test := range []struct {
		name           string
		announceRoot   bool
		includeBinding bool
		errorMessage   string
	}{
		{
			name:           "mutually_exclusive_with_bindings",
			announceRoot:   true,
			includeBinding: true,
			errorMessage:   "both inputs and input_root_digest",
		},
		{
			name:         "must_be_announced",
			errorMessage: "was not announced in UploadedBlobs",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			env, conn, _ := setupEnv(t)
			clientCtx, cacheCtx := graphTestContexts(t, env)
			sourceDigest := uploadGraphTestBlob(t, env, cacheCtx, "source")
			inputRootDigest := uploadGraphTestProto(t, env, cacheCtx, &repb.Directory{
				Files: []*repb.FileNode{{Name: "src.txt", Digest: sourceDigest}},
			})
			stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(clientCtx)
			require.NoError(t, err)
			_ = beginGraphTest(t, stream)
			sequence := uint64(2)
			if test.announceRoot {
				sendGraphTestRequest(t, stream, sequence, &graphpb.GraphExecuteRequest_UploadedBlobs{
					UploadedBlobs: &graphpb.UploadedBlobs{Digests: []*repb.Digest{inputRootDigest}},
				})
				sequence++
			}
			action := &graphpb.ActionNode{
				NodeId:          "copy",
				Command:         &repb.Command{Arguments: []string{"copy"}},
				InputRootDigest: inputRootDigest,
			}
			if test.includeBinding {
				action.Inputs = []*graphpb.InputBinding{{
					ExecPath: "src.txt",
					Value:    &graphpb.InputBinding_SourceDigest{SourceDigest: sourceDigest},
				}}
			}
			sendGraphTestRequest(t, stream, sequence, &graphpb.GraphExecuteRequest_Action{Action: action})

			response, err := stream.Recv()
			require.NoError(t, err)
			require.True(t, response.GetError().GetTerminal())
			require.Contains(t, response.GetError().GetStatus().GetMessage(), test.errorMessage)
		})
	}
}

func TestGraphExecute_ProducerConsumerCacheHits(t *testing.T) {
	env, conn, _ := setupEnv(t)
	clientCtx, cacheCtx := graphTestContexts(t, env)
	sourceDigest := uploadGraphTestBlob(t, env, cacheCtx, "source")
	producerCommand := &repb.Command{
		Arguments:   []string{"producer"},
		OutputFiles: []string{"middle.txt"},
	}
	_, middleDigest := prepareCachedGraphTestAction(
		t,
		env,
		cacheCtx,
		producerCommand,
		[]*repb.FileNode{{Name: "src.txt", Digest: sourceDigest}},
		"middle.txt",
		"middle",
	)
	consumerCommand := &repb.Command{
		Arguments:   []string{"consumer"},
		OutputFiles: []string{"final.txt"},
	}
	_, finalDigest := prepareCachedGraphTestAction(
		t,
		env,
		cacheCtx,
		consumerCommand,
		// Ordinary Bazel marks produced inputs executable in the downstream
		// Merkle tree even when the producer reports a non-executable output.
		[]*repb.FileNode{{Name: "middle.txt", Digest: middleDigest, IsExecutable: true}},
		"final.txt",
		"final",
	)

	stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(clientCtx)
	require.NoError(t, err)
	_ = beginGraphTest(t, stream, &graphpb.RootOutput{NodeId: "consumer", OutputPath: "final.txt"})
	sendGraphTestRequest(t, stream, 2, &graphpb.GraphExecuteRequest_UploadedBlobs{
		UploadedBlobs: &graphpb.UploadedBlobs{Digests: []*repb.Digest{sourceDigest}},
	})
	sendGraphTestRequest(t, stream, 3, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:  "consumer",
			Command: consumerCommand,
			Inputs: []*graphpb.InputBinding{{
				ExecPath: "middle.txt",
				Value: &graphpb.InputBinding_Produced{Produced: &graphpb.ProducedInput{
					ProducerNodeId: "producer",
					OutputPath:     "middle.txt",
				}},
			}},
		},
	})
	sendGraphTestRequest(t, stream, 4, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:  "producer",
			Command: producerCommand,
			Inputs: []*graphpb.InputBinding{{
				ExecPath: "src.txt",
				Value:    &graphpb.InputBinding_SourceDigest{SourceDigest: sourceDigest},
			}},
		},
	})
	sendGraphTestRequest(t, stream, 5, &graphpb.GraphExecuteRequest_Commit{
		Commit: &graphpb.CommitGraph{ExpectedActionCount: 2},
	})
	require.NoError(t, stream.CloseSend())

	completed, result := receiveGraphTestResult(t, stream)
	require.Equal(t, []string{"producer", "consumer"}, completed)
	require.Equal(t, uint64(2), result.GetCompletedNodes())
	require.Len(t, result.GetRoots(), 1)
	require.True(t, digest.Equal(finalDigest, result.GetRoots()[0].GetDigest()))
}

func TestGraphExecute_ProducerConsumerExecutionMisses(t *testing.T) {
	before := snapshotGraphTestMetrics()
	env, conn, _ := setupEnv(t)
	clientCtx, cacheCtx := graphTestContexts(t, env)
	sourceDigest := uploadGraphTestBlob(t, env, cacheCtx, "source")
	middleDigest := uploadGraphTestBlob(t, env, cacheCtx, "generated middle")
	finalDigest := uploadGraphTestBlob(t, env, cacheCtx, "generated final")

	stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(clientCtx)
	require.NoError(t, err)
	_ = beginGraphTest(t, stream, &graphpb.RootOutput{NodeId: "consumer", OutputPath: "final.txt"})
	sendGraphTestRequest(t, stream, 2, &graphpb.GraphExecuteRequest_UploadedBlobs{
		UploadedBlobs: &graphpb.UploadedBlobs{Digests: []*repb.Digest{sourceDigest}},
	})
	sendGraphTestRequest(t, stream, 3, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:  "consumer",
			Command: &repb.Command{Arguments: []string{"consumer"}, OutputFiles: []string{"final.txt"}},
			Inputs: []*graphpb.InputBinding{{
				ExecPath: "middle.txt",
				Value: &graphpb.InputBinding_Produced{Produced: &graphpb.ProducedInput{
					ProducerNodeId: "producer",
					OutputPath:     "middle.txt",
				}},
			}},
		},
	})
	sendGraphTestRequest(t, stream, 4, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:  "producer",
			Command: &repb.Command{Arguments: []string{"producer"}, OutputFiles: []string{"middle.txt"}},
			Inputs: []*graphpb.InputBinding{{
				ExecPath:     "source.txt",
				IsExecutable: true,
				Value:        &graphpb.InputBinding_SourceDigest{SourceDigest: sourceDigest},
			}},
		},
	})
	sendGraphTestRequest(t, stream, 5, &graphpb.GraphExecuteRequest_Commit{
		Commit: &graphpb.CommitGraph{ExpectedActionCount: 2},
	})
	require.NoError(t, stream.CloseSend())

	progressResponse, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, progressResponse.GetProgress())
	require.Equal(t, uint64(2), progressResponse.GetProgress().GetDeclaredNodes())

	producerRequest, producerTask := receiveGraphTestScheduledTask(t, env)
	require.Equal(t, []string{"producer"}, producerTask.GetCommand().GetArguments())
	publishGraphTestCompletion(t, conn, clientCtx, producerRequest, producerTask, 0, &repb.OutputFile{
		Path:   "middle.txt",
		Digest: middleDigest,
	})

	producerResponse, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, "producer", producerResponse.GetNodeResult().GetNodeId())

	consumerRequest, consumerTask := receiveGraphTestScheduledTask(t, env)
	require.Equal(t, []string{"consumer"}, consumerTask.GetCommand().GetArguments())
	inputRoot := &repb.Directory{}
	require.NoError(t, cachetools.ReadProtoFromCAS(
		cacheCtx,
		env.GetCache(),
		digest.NewCASResourceName(
			consumerTask.GetAction().GetInputRootDigest(),
			graphTestInstanceName,
			repb.DigestFunction_SHA256,
		),
		inputRoot,
	))
	require.Len(t, inputRoot.GetFiles(), 1)
	require.Equal(t, "middle.txt", inputRoot.GetFiles()[0].GetName())
	require.True(t, digest.Equal(middleDigest, inputRoot.GetFiles()[0].GetDigest()))
	require.True(t, inputRoot.GetFiles()[0].GetIsExecutable())
	publishGraphTestCompletion(t, conn, clientCtx, consumerRequest, consumerTask, 0, &repb.OutputFile{
		Path:   "final.txt",
		Digest: finalDigest,
	})

	completed, result := receiveGraphTestResult(t, stream)
	require.Equal(t, []string{"consumer"}, completed)
	require.Len(t, result.GetRoots(), 1)
	require.True(t, digest.Equal(finalDigest, result.GetRoots()[0].GetDigest()))

	after := snapshotGraphTestMetrics()
	require.Equal(t, float64(5), after.requestMessages-before.requestMessages)
	require.Equal(t, float64(5), after.responseMessages-before.responseMessages)
	require.Greater(t, after.requestBytes, before.requestBytes)
	require.Greater(t, after.responseBytes, before.responseBytes)
	require.Equal(t, float64(2), after.readyNodes-before.readyNodes)
	require.Equal(t, float64(0), after.cacheHitNodes-before.cacheHitNodes)
	require.Equal(t, float64(2), after.executedNodes-before.executedNodes)
	require.Equal(t, float64(2), after.avoidedExecuteRPCs-before.avoidedExecuteRPCs)
}

func TestGraphExecute_ResumeReplaysResponsesAndRequests(t *testing.T) {
	env, conn, _ := setupEnv(t)
	clientCtx, cacheCtx := graphTestContexts(t, env)
	sourceDigest := uploadGraphTestBlob(t, env, cacheCtx, "source")
	command := &repb.Command{
		Arguments:   []string{"copy"},
		OutputFiles: []string{"out.txt"},
	}
	_, outputDigest := prepareCachedGraphTestAction(
		t,
		env,
		cacheCtx,
		command,
		[]*repb.FileNode{{Name: "src.txt", Digest: sourceDigest}},
		"out.txt",
		"output",
	)

	firstCtx, cancelFirst := context.WithCancel(clientCtx)
	firstStream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(firstCtx)
	require.NoError(t, err)
	beginResponse := beginGraphTest(t, firstStream, &graphpb.RootOutput{NodeId: "copy", OutputPath: "out.txt"})
	uploadedRequest := &graphpb.GraphExecuteRequest{
		SessionId:      graphTestSessionID,
		SequenceNumber: 2,
		Payload: &graphpb.GraphExecuteRequest_UploadedBlobs{
			UploadedBlobs: &graphpb.UploadedBlobs{Digests: []*repb.Digest{sourceDigest}},
		},
	}
	actionRequest := &graphpb.GraphExecuteRequest{
		SessionId:      graphTestSessionID,
		SequenceNumber: 3,
		Payload: &graphpb.GraphExecuteRequest_Action{
			Action: &graphpb.ActionNode{
				NodeId:  "copy",
				Command: command,
				Inputs: []*graphpb.InputBinding{{
					ExecPath: "src.txt",
					Value:    &graphpb.InputBinding_SourceDigest{SourceDigest: sourceDigest},
				}},
			},
		},
	}
	require.NoError(t, firstStream.Send(uploadedRequest))
	require.NoError(t, firstStream.Send(actionRequest))
	cancelFirst()

	resumedStream, _ := resumeGraphTest(t, conn, clientCtx, beginResponse.GetBegin().GetResumeToken())
	// Replaying durable requests not covered by the last response ACK is
	// idempotent and does not create duplicate graph nodes or executions.
	require.NoError(t, resumedStream.Send(uploadedRequest))
	require.NoError(t, resumedStream.Send(actionRequest))
	sendGraphTestRequest(t, resumedStream, 4, &graphpb.GraphExecuteRequest_Commit{
		Commit: &graphpb.CommitGraph{ExpectedActionCount: 1},
	})
	require.NoError(t, resumedStream.CloseSend())

	completed, result := receiveGraphTestResult(t, resumedStream)
	require.Equal(t, []string{"copy"}, completed)
	require.Len(t, result.GetRoots(), 1)
	require.True(t, digest.Equal(outputDigest, result.GetRoots()[0].GetDigest()))
}

func TestGraphExecute_ByteIdenticalReplayDoesNotReschedule(t *testing.T) {
	env, conn, _ := setupEnv(t)
	clientCtx, _ := graphTestContexts(t, env)
	before := snapshotGraphTestMetrics()
	stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(clientCtx)
	require.NoError(t, err)
	_ = beginGraphTest(t, stream)
	actionRequest := &graphpb.GraphExecuteRequest{
		SessionId:      graphTestSessionID,
		SequenceNumber: 2,
		Payload: &graphpb.GraphExecuteRequest_Action{
			Action: &graphpb.ActionNode{
				NodeId:     "once",
				Command:    &repb.Command{Arguments: []string{"once"}},
				DoNotCache: true,
			},
		},
	}
	require.NoError(t, stream.Send(actionRequest))
	require.NoError(t, stream.Send(actionRequest))
	sendGraphTestRequest(t, stream, 3, &graphpb.GraphExecuteRequest_Commit{
		Commit: &graphpb.CommitGraph{ExpectedActionCount: 1},
	})
	progress, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, uint64(1), progress.GetProgress().GetReadyNodes())
	request, task := receiveGraphTestScheduledTask(t, env)
	select {
	case duplicate := <-env.GetSchedulerService().(*schedulerServerMock).scheduleReqCh:
		require.FailNowf(t, "byte-identical request replay scheduled duplicate work", "%s", duplicate.GetTaskId())
	case <-time.After(250 * time.Millisecond):
	}
	publishGraphTestCompletion(t, conn, clientCtx, request, task, 0)
	_, result := receiveGraphTestResult(t, stream)
	require.Equal(t, int32(codes.OK), result.GetStatus().GetCode())
	after := snapshotGraphTestMetrics()
	require.Equal(t, float64(1), after.readyNodes-before.readyNodes)
}

func TestGraphExecute_RejectsCycleAtCommit(t *testing.T) {
	env, conn, _ := setupEnv(t)
	clientCtx, _ := graphTestContexts(t, env)
	stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(clientCtx)
	require.NoError(t, err)
	_ = beginGraphTest(t, stream)
	sendGraphTestRequest(t, stream, 2, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:                 "a",
			Command:                &repb.Command{Arguments: []string{"a"}},
			SchedulingDependencies: []string{"b"},
		},
	})
	sendGraphTestRequest(t, stream, 3, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:                 "b",
			Command:                &repb.Command{Arguments: []string{"b"}},
			SchedulingDependencies: []string{"a"},
		},
	})
	sendGraphTestRequest(t, stream, 4, &graphpb.GraphExecuteRequest_Commit{
		Commit: &graphpb.CommitGraph{ExpectedActionCount: 2},
	})

	for {
		response, err := stream.Recv()
		if err == io.EOF {
			require.FailNow(t, "stream ended without terminal graph error")
		}
		require.NoError(t, err)
		if streamError := response.GetError(); streamError != nil {
			require.True(t, streamError.GetTerminal())
			require.Contains(t, streamError.GetStatus().GetMessage(), "cycle")
			return
		}
	}
}

func TestGraphExecute_ReportsMissingUploadedBlob(t *testing.T) {
	env, conn, _ := setupEnv(t)
	clientCtx, _ := graphTestContexts(t, env)
	stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(clientCtx)
	require.NoError(t, err)
	_ = beginGraphTest(t, stream)
	missingDigest := &repb.Digest{
		Hash:      strings.Repeat("a", 64),
		SizeBytes: 1,
	}
	sendGraphTestRequest(t, stream, 2, &graphpb.GraphExecuteRequest_UploadedBlobs{
		UploadedBlobs: &graphpb.UploadedBlobs{Digests: []*repb.Digest{missingDigest}},
	})

	response, err := stream.Recv()
	require.NoError(t, err)
	require.Len(t, response.GetMissingBlobs().GetDigests(), 1)
	require.True(t, digest.Equal(missingDigest, response.GetMissingBlobs().GetDigests()[0]))
}

func TestGraphExecute_RejectsConflictingNodeRedeclaration(t *testing.T) {
	env, conn, _ := setupEnv(t)
	clientCtx, cacheCtx := graphTestContexts(t, env)
	sourceDigest := uploadGraphTestBlob(t, env, cacheCtx, "source")
	stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(clientCtx)
	require.NoError(t, err)
	_ = beginGraphTest(t, stream)
	sendGraphTestRequest(t, stream, 2, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:  "copy",
			Command: &repb.Command{Arguments: []string{"first"}},
			Inputs: []*graphpb.InputBinding{{
				ExecPath: "source",
				Value:    &graphpb.InputBinding_SourceDigest{SourceDigest: sourceDigest},
			}},
		},
	})
	sendGraphTestRequest(t, stream, 3, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:  "copy",
			Command: &repb.Command{Arguments: []string{"different"}},
			Inputs: []*graphpb.InputBinding{{
				ExecPath: "source",
				Value:    &graphpb.InputBinding_SourceDigest{SourceDigest: sourceDigest},
			}},
		},
	})

	response, err := stream.Recv()
	require.NoError(t, err)
	require.True(t, response.GetError().GetTerminal())
	require.Contains(t, response.GetError().GetStatus().GetMessage(), "redeclared with different contents")
}

func TestGraphExecute_RejectsMalformedSequence(t *testing.T) {
	env, conn, _ := setupEnv(t)
	clientCtx, _ := graphTestContexts(t, env)
	stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(clientCtx)
	require.NoError(t, err)
	_ = beginGraphTest(t, stream)
	sendGraphTestRequest(t, stream, 3, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:  "action",
			Command: &repb.Command{Arguments: []string{"action"}},
		},
	})

	response, err := stream.Recv()
	require.NoError(t, err)
	require.True(t, response.GetError().GetTerminal())
	require.Equal(t, int32(codes.InvalidArgument), response.GetError().GetStatus().GetCode())
	require.Equal(t, "expected request sequence 2, got 3", response.GetError().GetStatus().GetMessage())
}

func TestGraphExecute_RejectsUncommittedStream(t *testing.T) {
	env, conn, _ := setupEnv(t)
	clientCtx, _ := graphTestContexts(t, env)
	stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(clientCtx)
	require.NoError(t, err)
	_ = beginGraphTest(t, stream)
	require.NoError(t, stream.CloseSend())

	response, err := stream.Recv()
	require.NoError(t, err)
	require.True(t, response.GetError().GetTerminal())
	require.Equal(t, int32(codes.FailedPrecondition), response.GetError().GetStatus().GetCode())
	require.Equal(t, "graph stream closed before CommitGraph", response.GetError().GetStatus().GetMessage())
}

func TestGraphExecute_RejectsUnannouncedSourceAtCommit(t *testing.T) {
	env, conn, _ := setupEnv(t)
	clientCtx, cacheCtx := graphTestContexts(t, env)
	sourceDigest := uploadGraphTestBlob(t, env, cacheCtx, "source")
	stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(clientCtx)
	require.NoError(t, err)
	_ = beginGraphTest(t, stream)
	sendGraphTestRequest(t, stream, 2, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:  "copy",
			Command: &repb.Command{Arguments: []string{"copy"}},
			Inputs: []*graphpb.InputBinding{{
				ExecPath: "source",
				Value:    &graphpb.InputBinding_SourceDigest{SourceDigest: sourceDigest},
			}},
		},
	})
	sendGraphTestRequest(t, stream, 3, &graphpb.GraphExecuteRequest_Commit{
		Commit: &graphpb.CommitGraph{ExpectedActionCount: 1},
	})

	response, err := stream.Recv()
	require.NoError(t, err)
	require.True(t, response.GetError().GetTerminal())
	require.Equal(t, int32(codes.FailedPrecondition), response.GetError().GetStatus().GetCode())
	require.Contains(t, response.GetError().GetStatus().GetMessage(), "was not announced in UploadedBlobs")
}

func TestGraphExecute_RejectsUndeclaredProducerOutputAtCommit(t *testing.T) {
	env, conn, _ := setupEnv(t)
	clientCtx, _ := graphTestContexts(t, env)
	stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(clientCtx)
	require.NoError(t, err)
	_ = beginGraphTest(t, stream)
	sendGraphTestRequest(t, stream, 2, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:  "consumer",
			Command: &repb.Command{Arguments: []string{"consumer"}},
			Inputs: []*graphpb.InputBinding{{
				ExecPath: "missing.txt",
				Value: &graphpb.InputBinding_Produced{Produced: &graphpb.ProducedInput{
					ProducerNodeId: "producer",
					OutputPath:     "missing.txt",
				}},
			}},
		},
	})
	sendGraphTestRequest(t, stream, 3, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:     "producer",
			Command:    &repb.Command{Arguments: []string{"producer"}},
			DoNotCache: true,
		},
	})
	sendGraphTestRequest(t, stream, 4, &graphpb.GraphExecuteRequest_Commit{
		Commit: &graphpb.CommitGraph{ExpectedActionCount: 2},
	})

	response, err := stream.Recv()
	require.NoError(t, err)
	require.True(t, response.GetError().GetTerminal())
	require.Equal(t, int32(codes.InvalidArgument), response.GetError().GetStatus().GetCode())
	require.Contains(t, response.GetError().GetStatus().GetMessage(), "references undeclared file output")
}

func TestGraphExecute_QuiescentWhenProducerOmitsDeclaredOutput(t *testing.T) {
	env, conn, _ := setupEnv(t)
	clientCtx, cacheCtx := graphTestContexts(t, env)
	producerCommand := &repb.Command{
		Arguments:   []string{"producer"},
		OutputFiles: []string{"middle.txt"},
	}
	commandDigest := uploadGraphTestProto(t, env, cacheCtx, producerCommand)
	inputRootDigest := uploadGraphTestProto(t, env, cacheCtx, &repb.Directory{})
	actionDigest := uploadGraphTestProto(t, env, cacheCtx, &repb.Action{
		CommandDigest:   commandDigest,
		InputRootDigest: inputRootDigest,
	})
	cacheGraphTestActionResult(t, env, cacheCtx, actionDigest, &repb.ActionResult{ExitCode: 0})

	stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(clientCtx)
	require.NoError(t, err)
	_ = beginGraphTest(t, stream)
	sendGraphTestRequest(t, stream, 2, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:  "consumer",
			Command: &repb.Command{Arguments: []string{"consumer"}},
			Inputs: []*graphpb.InputBinding{{
				ExecPath: "middle.txt",
				Value: &graphpb.InputBinding_Produced{Produced: &graphpb.ProducedInput{
					ProducerNodeId: "producer",
					OutputPath:     "middle.txt",
				}},
			}},
		},
	})
	sendGraphTestRequest(t, stream, 3, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:  "producer",
			Command: producerCommand,
		},
	})
	sendGraphTestRequest(t, stream, 4, &graphpb.GraphExecuteRequest_Commit{
		Commit: &graphpb.CommitGraph{ExpectedActionCount: 2},
	})
	require.NoError(t, stream.CloseSend())

	var sawProducer bool
	for {
		response, err := stream.Recv()
		require.NoError(t, err)
		if node := response.GetNodeResult(); node != nil {
			require.Equal(t, "producer", node.GetNodeId())
			sawProducer = true
		}
		if streamError := response.GetError(); streamError != nil {
			require.True(t, sawProducer)
			require.True(t, streamError.GetTerminal())
			require.Equal(t, int32(codes.FailedPrecondition), streamError.GetStatus().GetCode())
			require.Contains(t, streamError.GetStatus().GetMessage(), "unresolved inputs")
			return
		}
	}
}

func TestGraphExecute_ActionFailureReturnsGraphResult(t *testing.T) {
	env, conn, _ := setupEnv(t)
	clientCtx, _ := graphTestContexts(t, env)
	stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(clientCtx)
	require.NoError(t, err)
	_ = beginGraphTest(t, stream)
	sendGraphTestRequest(t, stream, 2, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:     "failing",
			Command:    &repb.Command{Arguments: []string{"false"}},
			DoNotCache: true,
		},
	})
	sendGraphTestRequest(t, stream, 3, &graphpb.GraphExecuteRequest_Commit{
		Commit: &graphpb.CommitGraph{ExpectedActionCount: 1},
	})
	require.NoError(t, stream.CloseSend())
	progress, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, progress.GetProgress())
	require.Equal(t, uint64(1), progress.GetProgress().GetReadyNodes())

	request, task := receiveGraphTestScheduledTask(t, env)
	publishGraphTestCompletion(t, conn, clientCtx, request, task, 42)

	nodeResponse, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, "failing", nodeResponse.GetNodeResult().GetNodeId())
	require.Equal(t, int32(42), nodeResponse.GetNodeResult().GetExecuteResponse().GetResult().GetExitCode())
	resultResponse, err := stream.Recv()
	require.NoError(t, err)
	require.Nil(t, resultResponse.GetError())
	require.Equal(t, "failing", resultResponse.GetResult().GetFailedNodeId())
	require.Equal(t, int32(codes.FailedPrecondition), resultResponse.GetResult().GetStatus().GetCode())
	require.Equal(t, uint64(1), resultResponse.GetResult().GetCompletedNodes())
}

func TestGraphExecute_TerminalProtocolErrorDoesNotCancelPotentiallySharedExecution(t *testing.T) {
	env, conn, _ := setupEnv(t)
	clientCtx, _ := graphTestContexts(t, env)
	stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(clientCtx)
	require.NoError(t, err)
	_ = beginGraphTest(t, stream)
	sendGraphTestRequest(t, stream, 2, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:  "running",
			Command: &repb.Command{Arguments: []string{"running"}},
		},
	})
	_, _ = receiveGraphTestScheduledTask(t, env)
	sendGraphTestRequest(t, stream, 3, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:                 "cycle-a",
			Command:                &repb.Command{Arguments: []string{"cycle-a"}},
			SchedulingDependencies: []string{"cycle-b"},
		},
	})
	sendGraphTestRequest(t, stream, 4, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:                 "cycle-b",
			Command:                &repb.Command{Arguments: []string{"cycle-b"}},
			SchedulingDependencies: []string{"cycle-a"},
		},
	})
	sendGraphTestRequest(t, stream, 5, &graphpb.GraphExecuteRequest_Commit{
		Commit: &graphpb.CommitGraph{ExpectedActionCount: 3},
	})

	response, err := stream.Recv()
	require.NoError(t, err)
	require.True(t, response.GetError().GetTerminal())
	require.Contains(t, response.GetError().GetStatus().GetMessage(), "cycle")
	scheduler := env.GetSchedulerService().(*schedulerServerMock)
	select {
	case cancelledTaskID := <-scheduler.cancelReqCh:
		require.FailNowf(t, "potentially shared execution was unsafely cancelled", "%s", cancelledTaskID)
	case <-time.After(250 * time.Millisecond):
	}
}

func TestGraphExecute_RejectsConcurrentResume(t *testing.T) {
	env, conn, _ := setupEnv(t)
	clientCtx, _ := graphTestContexts(t, env)
	first, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(clientCtx)
	require.NoError(t, err)
	beginResponse := beginGraphTest(t, first)

	resumeCtx, cancelResume := context.WithTimeout(clientCtx, time.Second)
	defer cancelResume()
	resumed, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(resumeCtx)
	require.NoError(t, err)
	require.NoError(t, resumed.Send(&graphpb.GraphExecuteRequest{
		SessionId:      graphTestSessionID,
		SequenceNumber: 0,
		Payload: &graphpb.GraphExecuteRequest_Resume{
			Resume: &graphpb.ResumeGraph{
				SessionId:            graphTestSessionID,
				ResumeToken:          beginResponse.GetBegin().GetResumeToken(),
				LastResponseSequence: beginResponse.GetSequenceNumber(),
			},
		},
	}))
	_, err = resumed.Recv()
	require.Equal(t, codes.AlreadyExists, gstatus.Code(err))
	require.NoError(t, first.CloseSend())
}

func TestGraphExecute_BenchmarkTimingCountersObservedOnce(t *testing.T) {
	fakeClock := clockwork.NewFakeClock()
	env, conn, _ := setupEnvWithClock(t, fakeClock)
	clientCtx, _ := graphTestContexts(t, env)
	before := snapshotGraphTestMetrics()
	stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(clientCtx)
	require.NoError(t, err)
	beginResponse := beginGraphTest(t, stream)

	fakeClock.Advance(2 * time.Millisecond)
	sendGraphTestRequest(t, stream, 2, &graphpb.GraphExecuteRequest_Action{
		Action: &graphpb.ActionNode{
			NodeId:     "timed",
			Command:    &repb.Command{Arguments: []string{"timed"}},
			DoNotCache: true,
		},
	})
	sendGraphTestRequest(t, stream, 3, &graphpb.GraphExecuteRequest_Commit{
		Commit: &graphpb.CommitGraph{ExpectedActionCount: 1},
	})
	progress, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, uint64(1), progress.GetProgress().GetReadyNodes())
	request, task := receiveGraphTestScheduledTask(t, env)

	fakeClock.Advance(3 * time.Millisecond)
	publishGraphTestCompletion(t, conn, clientCtx, request, task, 0)
	_, result := receiveGraphTestResult(t, stream)
	require.Equal(t, int32(codes.OK), result.GetStatus().GetCode())

	after := snapshotGraphTestMetrics()
	require.Equal(t, float64(2*time.Millisecond/time.Microsecond), after.beginToFirstReadyUsec-before.beginToFirstReadyUsec)
	require.Equal(t, float64(5*time.Millisecond/time.Microsecond), after.beginToTerminalCompletionUsec-before.beginToTerminalCompletionUsec)

	resumed, replayedBegin := resumeGraphTest(t, conn, clientCtx, beginResponse.GetBegin().GetResumeToken())
	replayedSequences := []uint64{replayedBegin.GetSequenceNumber()}
	for {
		response, err := resumed.Recv()
		require.NoError(t, err)
		replayedSequences = append(replayedSequences, response.GetSequenceNumber())
		if response.GetResult() != nil {
			break
		}
	}
	require.Equal(t, []uint64{1, 2, 3, 4}, replayedSequences)
	afterReplay := snapshotGraphTestMetrics()
	require.Equal(t, after.beginToFirstReadyUsec, afterReplay.beginToFirstReadyUsec)
	require.Equal(t, after.beginToTerminalCompletionUsec, afterReplay.beginToTerminalCompletionUsec)
}

func TestGraphExecute_RejectsExcessiveInputPath(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "depth", path: strings.Repeat("a/", 256) + "a"},
		{name: "component bytes", path: strings.Repeat("a", 256)},
		{name: "total bytes", path: strings.Repeat(strings.Repeat("a", 250)+"/", 16) + strings.Repeat("a", 250)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env, conn, _ := setupEnv(t)
			clientCtx, _ := graphTestContexts(t, env)
			stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(clientCtx)
			require.NoError(t, err)
			_ = beginGraphTest(t, stream)
			sendGraphTestRequest(t, stream, 2, &graphpb.GraphExecuteRequest_Action{
				Action: &graphpb.ActionNode{
					NodeId:  "too-deep",
					Command: &repb.Command{Arguments: []string{"true"}},
					Inputs: []*graphpb.InputBinding{{
						ExecPath: test.path,
						Value: &graphpb.InputBinding_SourceDigest{
							SourceDigest: &repb.Digest{
								Hash:      strings.Repeat("0", 64),
								SizeBytes: 6,
							},
						},
					}},
				},
			})
			response, err := stream.Recv()
			require.NoError(t, err)
			require.True(t, response.GetError().GetTerminal())
			require.Equal(t, int32(codes.ResourceExhausted), response.GetError().GetStatus().GetCode())
		})
	}
}

func TestGraphExecute_LocalDisconnectedSessionExpiresButDurableResumeSucceeds(t *testing.T) {
	fakeClock := clockwork.NewFakeClock()
	env, conn, _ := setupEnvWithClock(t, fakeClock)
	clientCtx, _ := graphTestContexts(t, env)
	firstCtx, cancelFirst := context.WithCancel(clientCtx)
	stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(firstCtx)
	require.NoError(t, err)
	beginResponse := beginGraphTest(t, stream)
	cancelFirst()

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelWait()
	require.NoError(t, fakeClock.BlockUntilContext(waitCtx, 1))
	fakeClock.Advance(6 * time.Minute)

	resumed, replayedBegin := resumeGraphTest(t, conn, clientCtx, beginResponse.GetBegin().GetResumeToken())
	require.Equal(t, beginResponse.GetSequenceNumber(), replayedBegin.GetSequenceNumber())
	sendGraphTestRequest(t, resumed, 2, &graphpb.GraphExecuteRequest_Commit{
		Commit: &graphpb.CommitGraph{ExpectedActionCount: 0},
	})
	_, result := receiveGraphTestResult(t, resumed)
	require.Equal(t, int32(codes.OK), result.GetStatus().GetCode())
}

func TestGraphExecute_SessionsAreTenantScopedAndBounded(t *testing.T) {
	env, conn, _ := setupEnv(t)
	auth := env.GetAuthenticator().(*testauth.TestAuthenticator)
	user1Ctx, err := auth.WithAuthenticatedUser(context.Background(), "US1")
	require.NoError(t, err)
	user2Ctx, err := auth.WithAuthenticatedUser(context.Background(), "US2")
	require.NoError(t, err)

	begin := func(ctx context.Context, sessionID string) (*graphpb.GraphExecuteResponse, graphpb.GraphExecution_GraphExecuteClient) {
		stream, err := graphpb.NewGraphExecutionClient(conn).GraphExecute(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&graphpb.GraphExecuteRequest{
			SessionId:      sessionID,
			SequenceNumber: 1,
			Payload: &graphpb.GraphExecuteRequest_Begin{
				Begin: &graphpb.BeginGraph{
					ProtocolVersion: 1,
					InstanceName:    graphTestInstanceName,
					DigestFunction:  repb.DigestFunction_SHA256,
					ResumeToken:     bytes.Repeat([]byte{0x43}, 32),
				},
			},
		}))
		response, err := stream.Recv()
		require.NoError(t, err)
		return response, stream
	}
	finish := func(stream graphpb.GraphExecution_GraphExecuteClient, sessionID string) {
		require.NoError(t, stream.Send(&graphpb.GraphExecuteRequest{
			SessionId:      sessionID,
			SequenceNumber: 2,
			Payload: &graphpb.GraphExecuteRequest_Commit{
				Commit: &graphpb.CommitGraph{ExpectedActionCount: 0},
			},
		}))
		require.NoError(t, stream.CloseSend())
		for {
			response, err := stream.Recv()
			require.NoError(t, err)
			if response.GetResult() != nil {
				return
			}
		}
	}

	response, stream := begin(user1Ctx, "shared-session-id")
	require.NotNil(t, response.GetBegin())
	finish(stream, "shared-session-id")
	response, _ = begin(user2Ctx, "shared-session-id")
	require.NotNil(t, response.GetBegin())

	// Completed sessions remain resumable but do not consume the active-session
	// allowance.
	// One retained user1 session already exists above; add two more to reach
	// the three-session worst-case retained byte budget.
	for i := 0; i < 2; i++ {
		sessionID := "user1-finished-session-" + strconv.Itoa(i)
		response, stream = begin(user1Ctx, sessionID)
		require.NotNil(t, response.GetBegin())
		finish(stream, sessionID)
	}

	// The active-session allowance is still enforced independently.
	// The distributed byte budget is intentionally tighter than the count
	// limit and admits three worst-case active sessions per tenant.
	for i := 0; i < 3; i++ {
		response, _ = begin(user1Ctx, "user1-active-session-"+strconv.Itoa(i))
		require.NotNil(t, response.GetBegin())
	}
	response, _ = begin(user1Ctx, "over-limit")
	require.True(t, response.GetError().GetTerminal())
	require.Equal(t, int32(codes.ResourceExhausted), response.GetError().GetStatus().GetCode())
}
