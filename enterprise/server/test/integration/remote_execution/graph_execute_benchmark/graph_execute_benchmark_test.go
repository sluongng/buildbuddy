package graph_execute_benchmark_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buildbuddy-io/buildbuddy/enterprise/server/test/integration/remote_execution/rbetest"
	graphpb "github.com/buildbuddy-io/buildbuddy/proto/graph_execution"
	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
	"github.com/buildbuddy-io/buildbuddy/server/interfaces"
	"github.com/buildbuddy-io/buildbuddy/server/metrics"
	"github.com/buildbuddy-io/buildbuddy/server/testutil/testmetrics"
	"github.com/buildbuddy-io/buildbuddy/server/util/grpc_client"
	"github.com/buildbuddy-io/buildbuddy/server/util/grpc_server"
	"github.com/buildbuddy-io/buildbuddy/server/util/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const (
	defaultDepth        = 256
	defaultIterations   = 10
	bootstrapIterations = 10_000
)

type benchmarkConfig struct {
	BazelBinary string        `json:"bazel_binary"`
	Depth       int           `json:"depth"`
	Iterations  int           `json:"iterations"`
	Latency     time.Duration `json:"-"`
	LatencyText string        `json:"latency"`
	RandomSeed  int64         `json:"random_seed"`
	Families    []string      `json:"families"`
	Scenarios   []string      `json:"scenarios"`
}

type benchmarkReport struct {
	Config                 benchmarkConfig         `json:"config"`
	ExpectedHash           string                  `json:"expected_output_sha256"`
	BackendTarget          string                  `json:"backend_target"`
	Notes                  []string                `json:"notes"`
	Trials                 []pairedTrial           `json:"trials"`
	Summaries              []aggregateSummary      `json:"summaries"`
	InteroperabilityTrials []interoperabilityTrial `json:"interoperability_trials"`
}

type aggregateSummary struct {
	Scenario                 string             `json:"scenario"`
	Family                   string             `json:"family"`
	SampleCount              int                `json:"sample_count"`
	TraditionalMedianMillis  float64            `json:"traditional_median_ms"`
	GraphMedianMillis        float64            `json:"graph_median_ms"`
	MedianPairedRatio        float64            `json:"median_paired_graph_over_traditional_ratio"`
	PairedRatioBootstrap95CI confidenceInterval `json:"paired_ratio_bootstrap_95ci"`
}

type confidenceInterval struct {
	Lower float64 `json:"lower"`
	Upper float64 `json:"upper"`
}

type pairedTrial struct {
	Scenario string       `json:"scenario"`
	Family   string       `json:"family"`
	Index    int          `json:"index"`
	Order    []string     `json:"order"`
	Results  []modeResult `json:"results"`
}

type modeResult struct {
	Mode                        string            `json:"mode"`
	Command                     string            `json:"command"`
	WallTimeMillis              float64           `json:"wall_time_ms"`
	TasksStarted                int               `json:"tasks_started"`
	MaterializationTasksStarted int               `json:"materialization_tasks_started"`
	IntermediateMaterialized    bool              `json:"intermediate_materialized_before_request"`
	OutputSHA256                string            `json:"output_sha256"`
	ActionDigests               []string          `json:"action_digests"`
	RPCCalls                    rpcCallCounts     `json:"rpc_calls"`
	MaterializationRPCCalls     rpcCallCounts     `json:"materialization_rpc_calls"`
	GraphMetrics                graphMetricCounts `json:"graph_metrics"`
}

type interoperabilityTrial struct {
	Family       string        `json:"family"`
	Direction    string        `json:"direction"`
	InstanceName string        `json:"instance_name"`
	Producer     interopResult `json:"producer"`
	Consumer     interopResult `json:"consumer"`
	OutputSHA256 string        `json:"output_sha256"`
}

type interopResult struct {
	Command      string        `json:"command"`
	TasksStarted int           `json:"tasks_started"`
	RPCCalls     rpcCallCounts `json:"rpc_calls"`
}

type commandFamily struct {
	Name        string
	Traditional string
	Graph       string
	Target      string
}

type mode struct {
	Name    string
	Command string
}

type bazelRunner struct {
	binary      string
	workspace   string
	outputRoot  string
	outputBase  string
	home        string
	remoteFlags []string
}

type invocationResult struct {
	stdout string
	stderr string
	err    error
}

type rpcCallCounter struct {
	mu     sync.Mutex
	counts rpcCallCounts
}

type rpcCallCounts map[string]int64

type actionDigestSnapshot struct {
	traditional int
	graph       int
}

type actionDigestObserver struct {
	mu          sync.Mutex
	traditional []string
	graph       []string
}

type actionDigestServerStream struct {
	grpc.ServerStream
	observer *actionDigestObserver
}

type graphMetricCounts struct {
	RequestMessages               float64 `json:"request_messages"`
	ResponseMessages              float64 `json:"response_messages"`
	RequestBytes                  float64 `json:"request_bytes"`
	ResponseBytes                 float64 `json:"response_bytes"`
	ReadyNodes                    float64 `json:"ready_nodes"`
	CacheHitNodes                 float64 `json:"cache_hit_nodes"`
	ExecutedNodes                 float64 `json:"executed_nodes"`
	AvoidedExecuteRPCs            float64 `json:"avoided_execute_rpcs"`
	BeginToFirstReadyUsec         float64 `json:"begin_to_first_ready_usec"`
	BeginToTerminalCompletionUsec float64 `json:"begin_to_terminal_completion_usec"`
}

type failoverReport struct {
	BazelBinary   string              `json:"bazel_binary"`
	Depth         int                 `json:"depth"`
	RemoteRetries int                 `json:"remote_retries"`
	Iterations    []failoverIteration `json:"iterations"`
}

type failoverIteration struct {
	Index                       int      `json:"index"`
	BlockedTaskOrdinal          int      `json:"blocked_task_ordinal"`
	PrimaryCrashed              bool     `json:"primary_crashed"`
	SameSessionContinued        bool     `json:"same_session_continued"`
	SessionID                   string   `json:"session_id"`
	ResumeTokenSHA256           string   `json:"resume_token_sha256"`
	PrimaryGraphStreams         int      `json:"primary_graph_streams"`
	SecondaryGraphStreams       int      `json:"secondary_graph_streams"`
	SecondaryAdmissionConflicts int      `json:"secondary_admission_conflicts"`
	LeaseTTLAfterCrashMS        int64    `json:"lease_ttl_after_crash_ms"`
	RecoveryLatencyMS           int64    `json:"recovery_latency_ms"`
	RecoveredWaiterAttached     bool     `json:"recovered_waiter_attached"`
	InjectedTransportDrops      int      `json:"injected_transport_drops"`
	DroppedResponseSequences    []uint64 `json:"dropped_response_sequences"`
	ExecutionIDs                []string `json:"execution_ids"`
	TasksStarted                int      `json:"tasks_started"`
	DuplicateTaskStarts         int      `json:"duplicate_task_starts"`
	MaterializationTasksStarted int      `json:"materialization_tasks_started"`
	NodeResultsBeforeCrash      int      `json:"node_results_before_crash"`
	OutputSHA256                string   `json:"output_sha256"`
}

type graphProtocolSnapshot struct {
	BeginSession       string
	BeginToken         []byte
	AckSession         string
	AckToken           []byte
	CommitObserved     bool
	NodeResults        int
	ResumeSession      string
	ResumeToken        []byte
	AdmissionConflicts int
	DroppedResponses   []uint64
}

type graphProtocolObserver struct {
	mu                    sync.Mutex
	snapshot              graphProtocolSnapshot
	errors                chan string
	commitObserved        chan struct{}
	dropResponseSequences map[uint64]struct{}
}

type graphProtocolServerStream struct {
	grpc.ServerStream
	observer *graphProtocolObserver
}

type failoverDirector struct {
	mu                 sync.Mutex
	primary            *grpc.ClientConn
	secondary          *grpc.ClientConn
	useSecondary       bool
	primaryGraphRPCs   int
	secondaryGraphRPCs int
}

type taskExecutionRecorder struct {
	mu  sync.Mutex
	ids []string
}

func TestDeepChainWorkspace(t *testing.T) {
	workspace, gotHash := writeDeepChainWorkspace(t, 3)
	expectedContents := "seed\nstep_0000\nstep_0001\nstep_0002\n"
	sum := sha256.Sum256([]byte(expectedContents))
	wantHash := hex.EncodeToString(sum[:])
	if gotHash != wantHash {
		t.Fatalf("fixture output hash: got %s, want %s", gotHash, wantHash)
	}
	buildContents, err := os.ReadFile(filepath.Join(workspace, "BUILD"))
	if err != nil {
		t.Fatalf("read generated BUILD file: %s", err)
	}
	if got := strings.Count(string(buildContents), "chain_step(name = "); got != 3 {
		t.Fatalf("generated chain length: got %d, want 3", got)
	}
}

// TestGraphExecuteAppFailover is intentionally manual because it requires a
// source-built Bazel containing gbuild. It proves that a single Bazel command
// resumes the same durable graph on a second app after the coordinating app is
// abruptly stopped while a real remote action is running.
func TestGraphExecuteAppFailover(t *testing.T) {
	config := readConfig(t)
	if config.BazelBinary == "" {
		t.Skip("set GRAPH_BAZEL_BIN to a source-built Bazel binary containing gbuild")
	}
	if _, err := os.Stat(config.BazelBinary); err != nil {
		t.Fatalf("stat GRAPH_BAZEL_BIN: %s", err)
	}
	depth := positiveIntEnv(t, "GRAPH_FAILOVER_DEPTH", 4)
	iterations := positiveIntEnv(t, "GRAPH_FAILOVER_ITERATIONS", 5)
	report := failoverReport{BazelBinary: config.BazelBinary, Depth: depth, RemoteRetries: 0}
	for i := 0; i < iterations; i++ {
		var result failoverIteration
		t.Run(fmt.Sprintf("iteration_%02d", i), func(t *testing.T) {
			result = runGraphExecuteAppFailover(t, config.BazelBinary, depth, i)
		})
		report.Iterations = append(report.Iterations, result)
	}
	outputPath := filepath.Join(undeclaredOutputsDir(t), "graph_execute_failover.json")
	writeJSON(t, outputPath, &report)
	t.Logf("GraphExecute failover results: %s", outputPath)
}

func runGraphExecuteAppFailover(
	t *testing.T, bazelBinary string, depth, iteration int,
) failoverIteration {
	t.Helper()
	workspace, expectedHash := writeDeepChainWorkspace(t, depth)
	rbe := rbetest.NewRBETestEnv(t)
	// Sequence 1 is BeginAck, so dropping it proves lost-BeginAck recovery.
	// The next three periodic acknowledgement responses exercise repeated
	// reconnects after distinct durable request boundaries.
	primaryProtocol := newGraphProtocolObserver(1, 2, 3, 4)
	secondaryProtocol := newGraphProtocolObserver()
	primary := rbe.AddBuildBuddyServerWithOptions(&rbetest.BuildBuddyServerOptions{
		GRPCServerConfig: grpc_server.GRPCServerConfig{
			ExtraChainedStreamInterceptors: []grpc.StreamServerInterceptor{
				primaryProtocol.streamServerInterceptor,
			},
		},
	})
	secondary := rbe.AddBuildBuddyServerWithOptions(&rbetest.BuildBuddyServerOptions{
		GRPCServerConfig: grpc_server.GRPCServerConfig{
			ExtraChainedStreamInterceptors: []grpc.StreamServerInterceptor{
				secondaryProtocol.streamServerInterceptor,
			},
		},
	})
	recoveredWaiterSubscribed := make(chan string, 1)
	secondary.SetGraphRecoveredWaiterSubscribedHook(func(executionID string) {
		select {
		case recoveredWaiterSubscribed <- executionID:
		default:
		}
	})
	director := installFailoverDirector(t, rbe, primary.GRPCAddress(), secondary.GRPCAddress())

	directCacheConn, err := grpc_client.DialSimpleWithoutPooling(secondary.GRPCAddress())
	if err != nil {
		t.Fatalf("dial direct executor cache connection: %s", err)
	}
	t.Cleanup(func() {
		if err := directCacheConn.Close(); err != nil {
			t.Errorf("close direct executor cache connection: %s", err)
		}
	})
	firstRunStarted := make(chan struct{})
	releaseFirstRun := make(chan struct{})
	var releaseFirstRunOnce sync.Once
	releaseFirstRunFn := func() {
		releaseFirstRunOnce.Do(func() { close(releaseFirstRun) })
	}
	t.Cleanup(releaseFirstRunFn)
	var runMu sync.Mutex
	runOrdinal := 0
	blockedTaskOrdinal := 1 + iteration%2
	recorder := &taskExecutionRecorder{}
	rbe.AddExecutorWithOptions(t, &rbetest.ExecutorOptions{
		Name:                  fmt.Sprintf("failover-executor-%02d", iteration),
		APIKey:                rbe.APIKey1,
		CacheConn:             directCacheConn,
		ScheduledTaskObserver: recorder.observe,
		RunInterceptor: func(ctx context.Context, original rbetest.RunFunc) *interfaces.CommandResult {
			runMu.Lock()
			runOrdinal++
			block := runOrdinal == blockedTaskOrdinal
			runMu.Unlock()
			if block {
				close(firstRunStarted)
				<-releaseFirstRun
			}
			return original(ctx, &repb.IOStats{})
		},
	})

	runner := newBazelRunner(
		t, bazelBinary, workspace, fmt.Sprintf("gbuild-failover-%02d", iteration),
		rbe.GetRemoteExecutionTarget(), rbe.APIKey1)
	instanceName := fmt.Sprintf("graph-execute-failover/%02d", iteration)
	flags := append([]string{}, runner.remoteFlags...)
	flags = append(flags, "--remote_instance_name="+instanceName, "//:chain")
	invocationDone := make(chan invocationResult, 1)
	tasksBefore := tasksStarted(t)
	go func() {
		invocationDone <- runner.invoke("gbuild", flags...)
	}()
	select {
	case <-firstRunStarted:
	case graphError := <-primaryProtocol.errors:
		t.Fatalf("primary returned a terminal graph error before the first action started: %s", graphError)
	case invocation := <-invocationDone:
		t.Fatalf(
			"gbuild completed before the first graph action started: %v\nstdout:\n%s\nstderr:\n%s",
			invocation.err, invocation.stdout, invocation.stderr)
	case <-time.After(2 * time.Minute):
		t.Fatal("first graph action did not start")
	}
	select {
	case <-primaryProtocol.commitObserved:
	case graphError := <-primaryProtocol.errors:
		t.Fatalf("primary returned a terminal graph error before CommitGraph: %s", graphError)
	case invocation := <-invocationDone:
		t.Fatalf(
			"gbuild completed before CommitGraph was observed: %v\nstdout:\n%s\nstderr:\n%s",
			invocation.err, invocation.stdout, invocation.stderr)
	case <-time.After(30 * time.Second):
		t.Fatal("primary did not observe CommitGraph before failover")
	}
	primaryBeforeCrash := primaryProtocol.getSnapshot()
	if blockedTaskOrdinal > 1 && primaryBeforeCrash.NodeResults < blockedTaskOrdinal-1 {
		t.Fatalf(
			"primary observed %d node results before blocking task %d, want at least %d",
			primaryBeforeCrash.NodeResults, blockedTaskOrdinal, blockedTaskOrdinal-1)
	}

	// Route every newly opened GraphExecute stream to app B before abruptly
	// stopping app A and its app-owned coordinators.
	director.failover()
	crashTime := time.Now()
	rbe.CrashBuildBuddyServer(primary)
	leaseTTLAfterCrash := requireLiveGraphLease(t, rbe)
	var recoveredExecutionID string
	select {
	case recoveredExecutionID = <-recoveredWaiterSubscribed:
	case graphError := <-secondaryProtocol.errors:
		t.Fatalf("secondary returned a terminal graph error before waiter reattachment: %s", graphError)
	case invocation := <-invocationDone:
		t.Fatalf(
			"gbuild completed before secondary waiter reattachment: %v\nstdout:\n%s\nstderr:\n%s",
			invocation.err, invocation.stdout, invocation.stderr)
	case <-time.After(30 * time.Second):
		t.Fatal("secondary did not attach a recovered WaitExecution waiter")
	}
	recoveryLatency := time.Since(crashTime)
	startedAtRecovery := recorder.snapshot()
	if len(startedAtRecovery) < blockedTaskOrdinal {
		t.Fatalf(
			"executor starts at recovery: got %v, want blocked task ordinal %d",
			startedAtRecovery, blockedTaskOrdinal)
	}
	blockedExecutionID := startedAtRecovery[blockedTaskOrdinal-1]
	if recoveredExecutionID != blockedExecutionID {
		t.Fatalf(
			"secondary reattached execution %q, want blocked in-flight execution %q",
			recoveredExecutionID, blockedExecutionID)
	}
	releaseFirstRunFn()

	var invocation invocationResult
	select {
	case invocation = <-invocationDone:
	case <-time.After(3 * time.Minute):
		t.Fatal("gbuild did not complete after app failover")
	}
	requireInvocation(t, invocation)

	primarySnapshot := primaryProtocol.getSnapshot()
	secondarySnapshot := secondaryProtocol.getSnapshot()
	requireGraphProtocolFailover(t, primarySnapshot, secondarySnapshot)
	executionIDs := recorder.snapshot()
	if len(executionIDs) != depth {
		t.Fatalf("executor task starts: got IDs %v, want exactly %d", executionIDs, depth)
	}
	seen := make(map[string]struct{}, len(executionIDs))
	for _, id := range executionIDs {
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate executor task start for stable execution ID %q: %v", id, executionIDs)
		}
		seen[id] = struct{}{}
	}
	started := tasksStarted(t) - tasksBefore
	if started != depth {
		t.Fatalf("remote task starts: got %d, want %d", started, depth)
	}
	// Materialize through ordinary REAPI after clearing Bazel's local action
	// state. This both makes the output bytes available for comparison and
	// proves that the failover result was committed to the standard action
	// cache without another execution.
	requireInvocation(t, runner.invoke("clean"))
	materializeFlags := append([]string{}, runner.remoteFlags...)
	materializeFlags = append(
		materializeFlags,
		"--remote_instance_name="+instanceName,
		"--remote_download_outputs=all",
		"//:chain")
	materializeTasksBefore := tasksStarted(t)
	requireInvocation(t, runner.invoke("build", materializeFlags...))
	materializeTasks := tasksStarted(t) - materializeTasksBefore
	if materializeTasks != 0 {
		t.Fatalf("ordinary REAPI materialization re-executed %d tasks, want 0", materializeTasks)
	}
	outputPath := filepath.Join(workspace, "bazel-bin", fmt.Sprintf("step_%04d.txt", depth-1))
	outputHash := sha256File(t, outputPath)
	if outputHash != expectedHash {
		t.Fatalf("failover output hash: got %s, want %s", outputHash, expectedHash)
	}
	primaryRPCs, secondaryRPCs := director.graphRPCCounts()
	wantPrimaryRPCs := len(primarySnapshot.DroppedResponses) + 1
	if primaryRPCs != wantPrimaryRPCs {
		t.Fatalf(
			"primary GraphExecute streams: got %d, want %d (one initial plus injected drops)",
			primaryRPCs, wantPrimaryRPCs)
	}
	if len(primarySnapshot.DroppedResponses) != 4 {
		t.Fatalf(
			"injected primary transport drops: got sequences %v, want [1 2 3 4]",
			primarySnapshot.DroppedResponses)
	}
	if secondaryRPCs < 2 {
		t.Fatalf(
			"secondary GraphExecute streams: got %d, want at least 2 to prove retry while crashed app's lease remained live",
			secondaryRPCs)
	}
	tokenHash := sha256.Sum256(primarySnapshot.AckToken)
	return failoverIteration{
		Index:                       iteration,
		BlockedTaskOrdinal:          blockedTaskOrdinal,
		PrimaryCrashed:              true,
		SameSessionContinued:        true,
		SessionID:                   primarySnapshot.AckSession,
		ResumeTokenSHA256:           hex.EncodeToString(tokenHash[:]),
		PrimaryGraphStreams:         primaryRPCs,
		SecondaryGraphStreams:       secondaryRPCs,
		SecondaryAdmissionConflicts: secondarySnapshot.AdmissionConflicts,
		LeaseTTLAfterCrashMS:        leaseTTLAfterCrash.Milliseconds(),
		RecoveryLatencyMS:           recoveryLatency.Milliseconds(),
		RecoveredWaiterAttached:     true,
		InjectedTransportDrops:      len(primarySnapshot.DroppedResponses),
		DroppedResponseSequences:    primarySnapshot.DroppedResponses,
		ExecutionIDs:                executionIDs,
		TasksStarted:                started,
		DuplicateTaskStarts:         0,
		MaterializationTasksStarted: materializeTasks,
		NodeResultsBeforeCrash:      primaryBeforeCrash.NodeResults,
		OutputSHA256:                outputHash,
	}
}

func newGraphProtocolObserver(dropResponseSequences ...uint64) *graphProtocolObserver {
	o := &graphProtocolObserver{
		errors:                make(chan string, 1),
		commitObserved:        make(chan struct{}),
		dropResponseSequences: make(map[uint64]struct{}, len(dropResponseSequences)),
	}
	for _, sequence := range dropResponseSequences {
		o.dropResponseSequences[sequence] = struct{}{}
	}
	return o
}

func TestExpectedRemoteTasks(t *testing.T) {
	build := commandFamily{
		Name: "build", Traditional: "build", Graph: "gbuild", Target: "//:chain"}
	test := commandFamily{
		Name: "test", Traditional: "test", Graph: "gtest", Target: "//:chain_test"}
	for _, tc := range []struct {
		name   string
		family commandFamily
		want   int
	}{
		{name: "build", family: build, want: 3},
		{name: "test", family: test, want: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := expectedRemoteTasks(tc.family, 3); got != tc.want {
				t.Fatalf("expected remote tasks: got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestParseSelection(t *testing.T) {
	allowed := []string{"build", "test"}
	for _, tc := range []struct {
		name      string
		value     string
		want      []string
		wantError bool
	}{
		{name: "default", value: "", want: []string{"build", "test"}},
		{name: "one", value: "build", want: []string{"build"}},
		{name: "both", value: "test,build", want: []string{"test", "build"}},
		{name: "whitespace", value: " build , test ", want: []string{"build", "test"}},
		{name: "unknown", value: "query", wantError: true},
		{name: "duplicate", value: "build,build", wantError: true},
		{name: "empty_element", value: "build,", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSelection(tc.value, allowed)
			if tc.wantError {
				if err == nil {
					t.Fatalf("parseSelection(%q) succeeded, want error", tc.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSelection(%q): %s", tc.value, err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("parseSelection(%q): got %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestAggregateSummaries(t *testing.T) {
	trials := []pairedTrial{
		syntheticPairedTrial("warm", "build", 100, 50, false),
		syntheticPairedTrial("warm", "build", 200, 100, true),
		syntheticPairedTrial("warm", "build", 300, 150, false),
	}
	got := aggregateSummaries(trials, 123)
	if len(got) != 1 {
		t.Fatalf("summary count: got %d, want 1", len(got))
	}
	summary := got[0]
	if summary.TraditionalMedianMillis != 200 {
		t.Errorf("traditional median: got %g, want 200", summary.TraditionalMedianMillis)
	}
	if summary.GraphMedianMillis != 100 {
		t.Errorf("graph median: got %g, want 100", summary.GraphMedianMillis)
	}
	if summary.MedianPairedRatio != 0.5 {
		t.Errorf("paired median ratio: got %g, want 0.5", summary.MedianPairedRatio)
	}
	if summary.PairedRatioBootstrap95CI != (confidenceInterval{Lower: 0.5, Upper: 0.5}) {
		t.Errorf(
			"paired ratio CI: got %+v, want {Lower:0.5 Upper:0.5}",
			summary.PairedRatioBootstrap95CI)
	}
	if again := aggregateSummaries(trials, 123); fmt.Sprint(again) != fmt.Sprint(got) {
		t.Fatalf("aggregate summaries are not deterministic: first=%+v second=%+v", got, again)
	}
}

func syntheticPairedTrial(
	scenario, family string, traditionalMillis, graphMillis float64, graphFirst bool,
) pairedTrial {
	traditional := modeResult{Mode: "traditional", WallTimeMillis: traditionalMillis}
	graph := modeResult{Mode: "graph", WallTimeMillis: graphMillis}
	results := []modeResult{traditional, graph}
	if graphFirst {
		results[0], results[1] = results[1], results[0]
	}
	return pairedTrial{Scenario: scenario, Family: family, Results: results}
}

func aggregateSummaries(trials []pairedTrial, randomSeed int64) []aggregateSummary {
	type samples struct {
		scenario    string
		family      string
		traditional []float64
		graph       []float64
		ratios      []float64
	}
	byKey := make(map[string]*samples)
	for _, trial := range trials {
		var traditional, graph float64
		var haveTraditional, haveGraph bool
		for _, result := range trial.Results {
			switch result.Mode {
			case "traditional":
				traditional = result.WallTimeMillis
				haveTraditional = true
			case "graph":
				graph = result.WallTimeMillis
				haveGraph = true
			}
		}
		if !haveTraditional || !haveGraph || traditional <= 0 {
			continue
		}
		key := trial.Scenario + "\x00" + trial.Family
		group := byKey[key]
		if group == nil {
			group = &samples{scenario: trial.Scenario, family: trial.Family}
			byKey[key] = group
		}
		group.traditional = append(group.traditional, traditional)
		group.graph = append(group.graph, graph)
		group.ratios = append(group.ratios, graph/traditional)
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	summaries := make([]aggregateSummary, 0, len(keys))
	for i, key := range keys {
		group := byKey[key]
		summaries = append(summaries, aggregateSummary{
			Scenario:                group.scenario,
			Family:                  group.family,
			SampleCount:             len(group.ratios),
			TraditionalMedianMillis: median(group.traditional),
			GraphMedianMillis:       median(group.graph),
			MedianPairedRatio:       median(group.ratios),
			PairedRatioBootstrap95CI: bootstrapMedian95CI(
				group.ratios, randomSeed+int64(i)*1_000_003),
		})
	}
	return summaries
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

func bootstrapMedian95CI(values []float64, randomSeed int64) confidenceInterval {
	if len(values) == 0 {
		return confidenceInterval{}
	}
	rng := rand.New(rand.NewSource(randomSeed))
	medians := make([]float64, bootstrapIterations)
	sample := make([]float64, len(values))
	for i := range medians {
		for j := range sample {
			sample[j] = values[rng.Intn(len(values))]
		}
		medians[i] = median(sample)
	}
	sort.Float64s(medians)
	return confidenceInterval{
		Lower: percentile(medians, 0.025),
		Upper: percentile(medians, 0.975),
	}
}

func percentile(sortedValues []float64, quantile float64) float64 {
	if len(sortedValues) == 0 {
		return 0
	}
	position := quantile * float64(len(sortedValues)-1)
	lower := int(position)
	upper := lower + 1
	if upper >= len(sortedValues) {
		return sortedValues[lower]
	}
	weight := position - float64(lower)
	return sortedValues[lower]*(1-weight) + sortedValues[upper]*weight
}

func TestGraphExecuteBenchmark(t *testing.T) {
	config := readConfig(t)
	if config.BazelBinary == "" {
		t.Skip("set GRAPH_BAZEL_BIN to a source-built Bazel binary containing gbuild and gtest")
	}
	if _, err := os.Stat(config.BazelBinary); err != nil {
		t.Fatalf("stat GRAPH_BAZEL_BIN: %s", err)
	}

	workspace, expectedHash := writeDeepChainWorkspace(t, config.Depth)

	rbe := rbetest.NewRBETestEnv(t)
	actionDigests := &actionDigestObserver{}
	app := rbe.AddBuildBuddyServerWithOptions(&rbetest.BuildBuddyServerOptions{
		GRPCServerConfig: grpc_server.GRPCServerConfig{
			ExtraChainedUnaryInterceptors: []grpc.UnaryServerInterceptor{
				actionDigests.unaryServerInterceptor,
			},
			ExtraChainedStreamInterceptors: []grpc.StreamServerInterceptor{
				actionDigests.streamServerInterceptor,
			},
		},
	})

	// Executors use a direct cache connection so the configured latency applies
	// only to Bazel's external REAPI / GraphExecute traffic.
	directCacheConn, err := grpc_client.DialSimpleWithoutPooling(app.GRPCAddress())
	if err != nil {
		t.Fatalf("dial direct executor cache connection: %s", err)
	}
	t.Cleanup(func() {
		if err := directCacheConn.Close(); err != nil {
			t.Errorf("close direct executor cache connection: %s", err)
		}
	})
	rbe.AddExecutorWithOptions(t, &rbetest.ExecutorOptions{
		APIKey:    rbe.APIKey1,
		CacheConn: directCacheConn,
	})

	rpcCalls := installLatencyDirector(t, rbe, app.GRPCAddress(), config.Latency)

	report := benchmarkReport{
		Config:        config,
		ExpectedHash:  expectedHash,
		BackendTarget: rbe.GetRemoteExecutionTarget(),
		Notes: []string{
			"Each warm paired mode uses an isolated namespace primed through its ordinary REAPI command.",
			"Each cold paired mode uses an isolated unprimed namespace.",
			"RPC counts are front-proxy RPC opens grouped by full method name; they do not count messages within a stream.",
			"Configured latency is an independent delay on each client RPC open; it is not transport-level per-message network latency.",
			"Graph metrics are zero-label BuildBuddy counter deltas covering only the measured invocation.",
			"Graph first-ready and terminal-completion values are server-observed microseconds since BeginGraph; multi-stream gtest values are sums.",
		},
	}
	rng := rand.New(rand.NewSource(config.RandomSeed))
	families := selectedCommandFamilies(config.Families)
	runners := make(map[string]*bazelRunner)
	for _, family := range families {
		for _, command := range []string{family.Traditional, family.Graph} {
			runners[command] = newBazelRunner(
				t,
				config.BazelBinary,
				workspace,
				command,
				rbe.GetRemoteExecutionTarget(),
				rbe.APIKey1)
		}
	}
	for _, scenario := range config.Scenarios {
		for _, family := range families {
			for i := 0; i < config.Iterations; i++ {
				trial := runPairedTrial(
					t, config, rbe, rpcCalls, actionDigests, runners, expectedHash, scenario, family, i, rng)
				report.Trials = append(report.Trials, trial)
			}
		}
	}
	report.Summaries = aggregateSummaries(report.Trials, config.RandomSeed)
	report.InteroperabilityTrials = runInteroperabilityTrials(
		t, config, rbe, rpcCalls, runners, expectedHash, families)

	outputPath := filepath.Join(undeclaredOutputsDir(t), "graph_execute_benchmark.json")
	writeJSON(t, outputPath, &report)
	t.Logf("GraphExecute benchmark results: %s", outputPath)
}

func TestGraphExecuteFailingTestAndUnsupportedBuild(t *testing.T) {
	config := readConfig(t)
	if config.BazelBinary == "" {
		t.Skip("set GRAPH_BAZEL_BIN to a source-built Bazel binary containing gbuild and gtest")
	}
	if _, err := os.Stat(config.BazelBinary); err != nil {
		t.Fatalf("stat GRAPH_BAZEL_BIN: %s", err)
	}

	workspace, _ := writeDeepChainWorkspace(t, 1)
	rbe := rbetest.NewRBETestEnv(t)
	app := rbe.AddBuildBuddyServer()
	directCacheConn, err := grpc_client.DialSimpleWithoutPooling(app.GRPCAddress())
	if err != nil {
		t.Fatalf("dial direct executor cache connection: %s", err)
	}
	t.Cleanup(func() {
		if err := directCacheConn.Close(); err != nil {
			t.Errorf("close direct executor cache connection: %s", err)
		}
	})
	rbe.AddExecutorWithOptions(t, &rbetest.ExecutorOptions{
		APIKey:    rbe.APIKey1,
		CacheConn: directCacheConn,
	})

	var exitCodes []int
	for _, command := range []string{"test", "gtest"} {
		runner := newBazelRunner(
			t,
			config.BazelBinary,
			workspace,
			command+"-failing",
			rbe.GetRemoteExecutionTarget(),
			rbe.APIKey1)
		flags := append([]string{}, runner.remoteFlags...)
		flags = append(
			flags,
			"--remote_instance_name=graph-execute-correctness/failing-"+command,
			"--remote_download_outputs=all",
			"//:chain_failing_test")
		tasksBefore := tasksStarted(t)
		result := runner.invoke(command, flags...)
		exitCode := requireFailedTestInvocation(t, command, result)
		exitCodes = append(exitCodes, exitCode)
		if tasks := tasksStarted(t) - tasksBefore; tasks != 3 {
			t.Fatalf("%s executed %d tasks, want 3", command, tasks)
		}
		for _, name := range []string{"test.log", "test.xml", "test.cache_status"} {
			path := filepath.Join(workspace, "bazel-testlogs", "chain_failing_test", name)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("%s did not preserve %s: %s", command, path, err)
			}
			if info.Size() == 0 {
				t.Fatalf("%s preserved empty test result file %s", command, path)
			}
		}
	}
	if exitCodes[0] != exitCodes[1] {
		t.Fatalf("test and gtest exit codes differ: %d vs %d", exitCodes[0], exitCodes[1])
	}

	runner := newBazelRunner(
		t,
		config.BazelBinary,
		workspace,
		"gbuild-unsupported",
		rbe.GetRemoteExecutionTarget(),
		rbe.APIKey1)
	flags := append([]string{}, runner.remoteFlags...)
	flags = append(
		flags,
		"--remote_instance_name=graph-execute-correctness/unsupported",
		"//:unsupported_local")
	tasksBefore := tasksStarted(t)
	graphMetricsBefore := snapshotGraphMetrics(t)
	result := runner.invoke("gbuild", flags...)
	if result.err == nil {
		t.Fatalf("unsupported gbuild unexpectedly succeeded")
	}
	output := result.stdout + "\n" + result.stderr
	if !strings.Contains(output, "spawn may not be executed remotely") {
		t.Fatalf("unsupported gbuild did not fail closed clearly:\n%s", output)
	}
	if tasks := tasksStarted(t) - tasksBefore; tasks != 0 {
		t.Fatalf("unsupported gbuild scheduled %d remote tasks, want 0", tasks)
	}
	graphMetrics := snapshotGraphMetrics(t).subtract(graphMetricsBefore)
	if graphMetrics.ReadyNodes != 0 || graphMetrics.ExecutedNodes != 0 {
		t.Fatalf("unsupported gbuild submitted executable graph nodes: %+v", graphMetrics)
	}
}

func requireFailedTestInvocation(t *testing.T, command string, result invocationResult) int {
	t.Helper()
	exitError, ok := result.err.(*exec.ExitError)
	if !ok {
		t.Fatalf(
			"%s returned %v, want a Bazel test-failure exit\nstdout:\n%s\nstderr:\n%s",
			command, result.err, result.stdout, result.stderr)
	}
	output := result.stdout + "\n" + result.stderr
	if !strings.Contains(output, "//:chain_failing_test") ||
		!strings.Contains(output, "FAILED") {
		t.Fatalf(
			"%s did not report the failing Bazel test normally:\n%s",
			command, output)
	}
	for _, forbidden := range []string{
		"graph execution setup failed",
		"GraphExecute transport failed",
		"graph execution service is unavailable",
	} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("%s converted test failure into graph infrastructure failure %q:\n%s", command, forbidden, output)
		}
	}
	return exitError.ExitCode()
}

func selectedCommandFamilies(names []string) []commandFamily {
	catalog := map[string]commandFamily{
		"build": {
			Name: "build", Traditional: "build", Graph: "gbuild", Target: "//:chain",
		},
		"test": {
			Name: "test", Traditional: "test", Graph: "gtest", Target: "//:chain_test",
		},
	}
	families := make([]commandFamily, 0, len(names))
	for _, name := range names {
		families = append(families, catalog[name])
	}
	return families
}

func readConfig(t *testing.T) benchmarkConfig {
	t.Helper()
	binary := os.Getenv("GRAPH_BAZEL_BIN")
	if binary != "" {
		var err error
		binary, err = filepath.Abs(binary)
		if err != nil {
			t.Fatalf("resolve GRAPH_BAZEL_BIN: %s", err)
		}
	}
	latency := durationEnv(t, "GRAPH_BENCH_LATENCY", 0)
	families := selectionEnv(t, "GRAPH_BENCH_FAMILIES", []string{"build", "test"})
	scenarios := selectionEnv(t, "GRAPH_BENCH_SCENARIOS", []string{"warm", "cold"})
	return benchmarkConfig{
		BazelBinary: binary,
		Depth:       positiveIntEnv(t, "GRAPH_BENCH_DEPTH", defaultDepth),
		Iterations:  positiveIntEnv(t, "GRAPH_BENCH_ITERATIONS", defaultIterations),
		Latency:     latency,
		LatencyText: latency.String(),
		RandomSeed:  int64Env(t, "GRAPH_BENCH_RANDOM_SEED", 1),
		Families:    families,
		Scenarios:   scenarios,
	}
}

func selectionEnv(t *testing.T, name string, allowed []string) []string {
	t.Helper()
	selection, err := parseSelection(os.Getenv(name), allowed)
	if err != nil {
		t.Fatalf("%s: %s", name, err)
	}
	return selection
}

func parseSelection(value string, allowed []string) ([]string, error) {
	if value == "" {
		return append([]string(nil), allowed...), nil
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, item := range allowed {
		allowedSet[item] = struct{}{}
	}
	seen := make(map[string]struct{})
	var selection []string
	for raw := range strings.SplitSeq(value, ",") {
		item := strings.TrimSpace(raw)
		if item == "" {
			return nil, fmt.Errorf("selection contains an empty item")
		}
		if _, ok := allowedSet[item]; !ok {
			return nil, fmt.Errorf("unsupported value %q; allowed values are %s", item, strings.Join(allowed, ","))
		}
		if _, ok := seen[item]; ok {
			return nil, fmt.Errorf("selection contains duplicate value %q", item)
		}
		seen[item] = struct{}{}
		selection = append(selection, item)
	}
	return selection, nil
}

func positiveIntEnv(t *testing.T, name string, defaultValue int) int {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		return defaultValue
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		t.Fatalf("%s must be a positive integer, got %q", name, value)
	}
	return n
}

func int64Env(t *testing.T, name string, defaultValue int64) int64 {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		return defaultValue
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		t.Fatalf("%s must be an integer, got %q", name, value)
	}
	return n
}

func durationEnv(t *testing.T, name string, defaultValue time.Duration) time.Duration {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		return defaultValue
	}
	d, err := time.ParseDuration(value)
	if err != nil || d < 0 {
		t.Fatalf("%s must be a non-negative duration, got %q", name, value)
	}
	return d
}

func writeDeepChainWorkspace(t *testing.T, depth int) (string, string) {
	t.Helper()
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "MODULE.bazel"), []byte(
		"module(name = \"graph_execute_benchmark\")\n"))
	mustWriteFile(t, filepath.Join(workspace, "chain.bzl"), []byte(chainRuleDefinitions))

	var build strings.Builder
	platform := fmt.Sprintf(
		`{"OSFamily": %q, "Arch": %q}`, runtime.GOOS, runtime.GOARCH)
	previous := "seed.txt"
	for i := 0; i < depth; i++ {
		name := fmt.Sprintf("step_%04d", i)
		fmt.Fprintf(
			&build,
			"chain_step(name = %q, src = %q, marker = %q, exec_properties = %s)\n",
			name, previous, name, platform)
		previous = ":" + name
	}
	fmt.Fprintf(&build, "alias(name = \"chain\", actual = %q)\n", previous)
	fmt.Fprintf(
		&build,
		"chain_test(name = \"chain_test\", src = %q, expected_marker = %q, exec_properties = %s)\n",
		previous, fmt.Sprintf("step_%04d", depth-1), platform)
	fmt.Fprintf(
		&build,
		"chain_test(name = \"chain_failing_test\", src = %q, expected_marker = \"never\", exec_properties = %s)\n",
		previous, platform)
	fmt.Fprintf(
		&build,
		"local_only(name = \"unsupported_local\", exec_properties = %s)\n",
		platform)
	mustWriteFile(
		t,
		filepath.Join(workspace, "BUILD"),
		[]byte("load(\":chain.bzl\", \"chain_step\", \"chain_test\", \"local_only\")\n\n"+build.String()))
	mustWriteFile(t, filepath.Join(workspace, "seed.txt"), []byte("seed\n"))

	expected := strings.Builder{}
	expected.WriteString("seed\n")
	for i := 0; i < depth; i++ {
		fmt.Fprintf(&expected, "step_%04d\n", i)
	}
	sum := sha256.Sum256([]byte(expected.String()))
	return workspace, hex.EncodeToString(sum[:])
}

const chainRuleDefinitions = `
def _chain_step_impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name + ".txt")
    ctx.actions.run_shell(
        inputs = [ctx.file.src],
        outputs = [out],
        command = """
set -eu
cat "$1" > "$2"
printf '%s\n' "$3" >> "$2"
""",
        arguments = [ctx.file.src.path, out.path, ctx.attr.marker],
    )
    return [DefaultInfo(files = depset([out]))]

chain_step = rule(
    implementation = _chain_step_impl,
    attrs = {
        "src": attr.label(allow_single_file = True),
        "marker": attr.string(mandatory = True),
    },
)

def _chain_test_impl(ctx):
    executable = ctx.actions.declare_file(ctx.label.name + ".sh")
    ctx.actions.write(
        output = executable,
        is_executable = True,
        content = """#!/bin/sh
set -eu
input="$TEST_SRCDIR/$TEST_WORKSPACE/%s"
tail -n 1 "$input" | grep -Fx %s
""" % (ctx.file.src.short_path, repr(ctx.attr.expected_marker)),
    )
    return [DefaultInfo(
        executable = executable,
        runfiles = ctx.runfiles(files = [ctx.file.src]),
    )]

chain_test = rule(
    implementation = _chain_test_impl,
    test = True,
    attrs = {
        "src": attr.label(allow_single_file = True),
        "expected_marker": attr.string(mandatory = True),
    },
)

def _local_only_impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name + ".txt")
    ctx.actions.run_shell(
        outputs = [out],
        command = "printf local-only > \"$1\"",
        arguments = [out.path],
        execution_requirements = {"no-remote": "1"},
    )
    return [DefaultInfo(files = depset([out]))]

local_only = rule(
    implementation = _local_only_impl,
)
`

func runPairedTrial(
	t *testing.T,
	config benchmarkConfig,
	rbe *rbetest.Env,
	rpcCalls *rpcCallCounter,
	actionDigests *actionDigestObserver,
	runners map[string]*bazelRunner,
	expectedHash string,
	scenario string,
	family commandFamily,
	index int,
	rng *rand.Rand,
) pairedTrial {
	t.Helper()
	traditional := mode{Name: "traditional", Command: family.Traditional}
	graph := mode{Name: "graph", Command: family.Graph}
	order := []mode{traditional, graph}
	if rng.Intn(2) == 1 {
		order[0], order[1] = order[1], order[0]
	}
	trial := pairedTrial{
		Scenario: scenario,
		Family:   family.Name,
		Index:    index,
		Order:    []string{order[0].Name, order[1].Name},
	}
	for _, mode := range order {
		instanceName := fmt.Sprintf(
			"graph-execute-benchmark/%s/%s/%03d/%s",
			scenario, family.Name, index, mode.Name)
		runner := runners[mode.Command]
		result := runMode(
			t,
			rbe,
			rpcCalls,
			actionDigests,
			runner,
			scenario,
			family,
			mode,
			instanceName,
			expectedHash,
			config.Depth)
		trial.Results = append(trial.Results, result)
	}
	var traditionalDigests, graphDigests []string
	for _, result := range trial.Results {
		switch result.Mode {
		case "traditional":
			traditionalDigests = result.ActionDigests
		case "graph":
			graphDigests = result.ActionDigests
		}
	}
	if strings.Join(traditionalDigests, "\n") != strings.Join(graphDigests, "\n") {
		t.Fatalf(
			"%s %s action digest sets differ:\ntraditional=%v\ngraph=%v",
			scenario, family.Name, traditionalDigests, graphDigests)
	}
	return trial
}

func newBazelRunner(
	t *testing.T,
	binary string,
	workspace string,
	name string,
	remoteTarget string,
	apiKey string,
) *bazelRunner {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	outputRoot := filepath.Join(root, "output-user-root")
	outputBase := filepath.Join(root, "output-base")
	home := filepath.Join(root, "home")
	for _, path := range []string{outputRoot, outputBase, home} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("create Bazel directory %q: %s", path, err)
		}
	}
	return &bazelRunner{
		binary:     binary,
		workspace:  workspace,
		outputRoot: outputRoot,
		outputBase: outputBase,
		home:       home,
		remoteFlags: []string{
			"--remote_executor=" + remoteTarget,
			"--remote_header=x-buildbuddy-api-key=" + apiKey,
			"--remote_download_outputs=minimal",
			"--nobuild_runfile_links",
			"--remote_local_fallback=false",
			"--spawn_strategy=remote",
			"--jobs=64",
			"--remote_retries=0",
			"--remote_timeout=60s",
			"--cache_test_results=yes",
		},
	}
}

func runMode(
	t *testing.T,
	rbe *rbetest.Env,
	rpcCalls *rpcCallCounter,
	actionDigests *actionDigestObserver,
	runner *bazelRunner,
	scenario string,
	family commandFamily,
	mode mode,
	instanceName string,
	expectedHash string,
	depth int,
) modeResult {
	t.Helper()
	flags := append([]string{}, runner.remoteFlags...)
	flags = append(flags, "--remote_instance_name="+instanceName, family.Target)

	// Starts the Bazel server and extracts its install base without populating
	// either the local action state or the remote cache.
	requireInvocation(t, runner.invoke("clean"))
	if scenario == "warm" {
		// Prime both modes through ordinary REAPI. A warm graph run must be
		// compatible with ActionResults produced by the traditional command.
		requireInvocation(t, runner.invoke(family.Traditional, flags...))
		requireInvocation(t, runner.invoke("clean"))
	}

	tasksBefore := tasksStarted(t)
	rpcCallsBefore := rpcCalls.snapshot()
	actionDigestsBefore := actionDigests.snapshot()
	graphMetricsBefore := snapshotGraphMetrics(t)
	start := time.Now()
	requireInvocation(t, runner.invoke(mode.Command, flags...))
	elapsed := time.Since(start)
	tasks := tasksStarted(t) - tasksBefore
	measuredRPCCalls := rpcCalls.delta(rpcCallsBefore)
	measuredActionDigests := actionDigests.delta(mode.Name, actionDigestsBefore)
	measuredGraphMetrics := snapshotGraphMetrics(t).subtract(graphMetricsBefore)
	if len(measuredActionDigests) != expectedRemoteTasks(family, depth) {
		t.Fatalf(
			"%s %s observed %d action digests, want %d",
			mode.Name, family.Name, len(measuredActionDigests), expectedRemoteTasks(family, depth))
	}
	if scenario == "warm" && tasks != 0 {
		t.Fatalf(
			"%s %s warm run unexpectedly executed %d tasks",
			mode.Name, family.Name, tasks)
	}
	if scenario == "cold" && tasks != expectedRemoteTasks(family, depth) {
		t.Fatalf(
			"%s %s cold run executed %d tasks, want %d",
			mode.Name, family.Name, tasks, expectedRemoteTasks(family, depth))
	}
	intermediateMaterialized := false
	if depth > 1 {
		intermediatePath := filepath.Join(runner.workspace, "bazel-bin", "step_0000.txt")
		_, err := os.Stat(intermediatePath)
		switch {
		case err == nil:
			intermediateMaterialized = true
			t.Fatalf(
				"%s %s eagerly materialized intermediate output %q under remote_download_outputs=minimal",
				mode.Name, family.Name, intermediatePath)
		case !os.IsNotExist(err):
			t.Fatalf("stat intermediate output %q: %s", intermediatePath, err)
		}
	}

	// Validate both output bytes and standard REAPI action-cache compatibility.
	// Cleaning first prevents Bazel's local action state from satisfying this
	// build. Ordinary build must rematerialize the graph result without causing
	// any new remote executions.
	requireInvocation(t, runner.invoke("clean"))
	materializeFlags := append([]string{}, runner.remoteFlags...)
	materializeFlags = append(
		materializeFlags,
		"--remote_instance_name="+instanceName,
		"--remote_download_outputs=all",
		family.Target)
	materializeTasksBefore := tasksStarted(t)
	materializeRPCsBefore := rpcCalls.snapshot()
	requireInvocation(t, runner.invoke(family.Traditional, materializeFlags...))
	materializeTasks := tasksStarted(t) - materializeTasksBefore
	materializeRPCCalls := rpcCalls.delta(materializeRPCsBefore)
	if materializeTasks != 0 {
		t.Fatalf(
			"%s %s result is not reusable through ordinary REAPI: materialization executed %d tasks",
			mode.Name, family.Name, materializeTasks)
	}
	outputPath := filepath.Join(runner.workspace, "bazel-bin", fmt.Sprintf("step_%04d.txt", depth-1))
	outputHash := sha256File(t, outputPath)
	if outputHash != expectedHash {
		t.Fatalf(
			"%s %s output hash mismatch: got %s, want %s",
			mode.Name, family.Name, outputHash, expectedHash)
	}
	return modeResult{
		Mode:                        mode.Name,
		Command:                     mode.Command,
		WallTimeMillis:              float64(elapsed) / float64(time.Millisecond),
		TasksStarted:                tasks,
		MaterializationTasksStarted: materializeTasks,
		IntermediateMaterialized:    intermediateMaterialized,
		OutputSHA256:                outputHash,
		ActionDigests:               measuredActionDigests,
		RPCCalls:                    measuredRPCCalls,
		MaterializationRPCCalls:     materializeRPCCalls,
		GraphMetrics:                measuredGraphMetrics,
	}
}

func runInteroperabilityTrials(
	t *testing.T,
	config benchmarkConfig,
	rbe *rbetest.Env,
	rpcCalls *rpcCallCounter,
	runners map[string]*bazelRunner,
	expectedHash string,
	families []commandFamily,
) []interoperabilityTrial {
	t.Helper()
	var trials []interoperabilityTrial
	for _, family := range families {
		for _, commands := range []struct {
			direction string
			producer  string
			consumer  string
		}{
			{
				direction: "traditional_to_graph",
				producer:  family.Traditional,
				consumer:  family.Graph,
			},
			{
				direction: "graph_to_traditional",
				producer:  family.Graph,
				consumer:  family.Traditional,
			},
		} {
			instanceName := fmt.Sprintf(
				"graph-execute-benchmark/interop/%s/%s", family.Name, commands.direction)
			producerRunner := runners[commands.producer]
			consumerRunner := runners[commands.consumer]
			producerFlags := benchmarkFlags(producerRunner, instanceName, family.Target)
			consumerFlags := benchmarkFlags(consumerRunner, instanceName, family.Target)

			requireInvocation(t, producerRunner.invoke("clean"))
			producerTasksBefore := tasksStarted(t)
			producerRPCsBefore := rpcCalls.snapshot()
			requireInvocation(t, producerRunner.invoke(commands.producer, producerFlags...))
			producerTasks := tasksStarted(t) - producerTasksBefore
			producerRPCs := rpcCalls.delta(producerRPCsBefore)
			wantTasks := expectedRemoteTasks(family, config.Depth)
			if producerTasks != wantTasks {
				t.Fatalf(
					"%s %s producer executed %d tasks, want %d",
					family.Name, commands.direction, producerTasks, wantTasks)
			}

			requireInvocation(t, consumerRunner.invoke("clean"))
			consumerTasksBefore := tasksStarted(t)
			consumerRPCsBefore := rpcCalls.snapshot()
			requireInvocation(t, consumerRunner.invoke(commands.consumer, consumerFlags...))
			consumerTasks := tasksStarted(t) - consumerTasksBefore
			consumerRPCs := rpcCalls.delta(consumerRPCsBefore)
			if consumerTasks != 0 {
				t.Fatalf(
					"%s %s consumer executed %d tasks; action-cache reuse failed",
					family.Name, commands.direction, consumerTasks)
			}

			// Materialize through ordinary REAPI in the same instance so the
			// interoperability check also validates the resulting output bytes.
			traditionalRunner := runners[family.Traditional]
			requireInvocation(t, traditionalRunner.invoke("clean"))
			materializeFlags := append([]string{}, traditionalRunner.remoteFlags...)
			materializeFlags = append(
				materializeFlags,
				"--remote_instance_name="+instanceName,
				"--remote_download_outputs=all",
				family.Target)
			materializeTasksBefore := tasksStarted(t)
			requireInvocation(
				t, traditionalRunner.invoke(family.Traditional, materializeFlags...))
			if materializeTasks := tasksStarted(t) - materializeTasksBefore; materializeTasks != 0 {
				t.Fatalf(
					"%s %s output materialization executed %d tasks",
					family.Name, commands.direction, materializeTasks)
			}
			outputHash := sha256File(
				t,
				filepath.Join(
					traditionalRunner.workspace,
					"bazel-bin",
					fmt.Sprintf("step_%04d.txt", config.Depth-1)))
			if outputHash != expectedHash {
				t.Fatalf(
					"%s %s output hash mismatch: got %s, want %s",
					family.Name, commands.direction, outputHash, expectedHash)
			}
			trials = append(trials, interoperabilityTrial{
				Family:       family.Name,
				Direction:    commands.direction,
				InstanceName: instanceName,
				Producer: interopResult{
					Command:      commands.producer,
					TasksStarted: producerTasks,
					RPCCalls:     producerRPCs,
				},
				Consumer: interopResult{
					Command:      commands.consumer,
					TasksStarted: consumerTasks,
					RPCCalls:     consumerRPCs,
				},
				OutputSHA256: outputHash,
			})
		}
	}
	return trials
}

func benchmarkFlags(runner *bazelRunner, instanceName, target string) []string {
	flags := append([]string{}, runner.remoteFlags...)
	return append(flags, "--remote_instance_name="+instanceName, target)
}

func expectedRemoteTasks(family commandFamily, depth int) int {
	if family.Name == "test" {
		// Both test commands remotely execute the main test spawn and Bazel's
		// generate-xml postprocessing spawn.
		return depth + 2
	}
	return depth
}

func (r *bazelRunner) invoke(command string, args ...string) invocationResult {
	startupArgs := []string{
		"--output_user_root=" + r.outputRoot,
		"--output_base=" + r.outputBase,
		"--max_idle_secs=60",
		command,
	}
	startupArgs = append(startupArgs, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.binary, startupArgs...)
	cmd.Dir = r.workspace
	cmd.Env = append(os.Environ(), "HOME="+r.home)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return invocationResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

func requireInvocation(t *testing.T, result invocationResult) {
	t.Helper()
	if result.err != nil {
		t.Fatalf(
			"Bazel invocation failed: %s\nstdout:\n%s\nstderr:\n%s",
			result.err, result.stdout, result.stderr)
	}
}

func installLatencyDirector(
	t *testing.T, rbe *rbetest.Env, backendTarget string, latency time.Duration,
) *rpcCallCounter {
	t.Helper()
	conn, err := grpc_client.DialSimpleWithoutPooling(
		backendTarget,
		grpc.WithChainUnaryInterceptor(
			func(
				ctx context.Context,
				method string,
				req, reply any,
				cc *grpc.ClientConn,
				invoker grpc.UnaryInvoker,
				opts ...grpc.CallOption,
			) error {
				if err := waitForClientRPCOpen(ctx, method, latency); err != nil {
					return err
				}
				return invoker(ctx, method, req, reply, cc, opts...)
			}),
		grpc.WithChainStreamInterceptor(
			func(
				ctx context.Context,
				desc *grpc.StreamDesc,
				cc *grpc.ClientConn,
				method string,
				streamer grpc.Streamer,
				opts ...grpc.CallOption,
			) (grpc.ClientStream, error) {
				if err := waitForClientRPCOpen(ctx, method, latency); err != nil {
					return nil, err
				}
				return streamer(ctx, desc, cc, method, opts...)
			}))
	if err != nil {
		t.Fatalf("dial benchmark backend: %s", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close benchmark backend connection: %s", err)
		}
	})
	counter := &rpcCallCounter{counts: make(rpcCallCounts)}
	rbe.AppProxy.SetDirector(
		func(ctx context.Context, fullMethodName string) (context.Context, *grpc.ClientConn, error) {
			if isClientBuildRPC(fullMethodName) {
				counter.increment(fullMethodName)
			}
			if md, ok := metadata.FromIncomingContext(ctx); ok {
				ctx = metadata.NewOutgoingContext(ctx, md.Copy())
			}
			return ctx, conn, nil
		})
	return counter
}

func waitForClientRPCOpen(
	ctx context.Context, fullMethodName string, latency time.Duration,
) error {
	if latency <= 0 || !isClientBuildRPC(fullMethodName) {
		return nil
	}
	timer := time.NewTimer(latency)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func isClientBuildRPC(fullMethodName string) bool {
	return strings.Contains(fullMethodName, "build.bazel.remote.execution.v2.") ||
		strings.Contains(fullMethodName, "google.bytestream.ByteStream/") ||
		strings.Contains(fullMethodName, "GraphExecute")
}

func (c *rpcCallCounter) increment(method string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[method]++
}

func (c *rpcCallCounter) snapshot() rpcCallCounts {
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := make(rpcCallCounts, len(c.counts))
	for method, count := range c.counts {
		snapshot[method] = count
	}
	return snapshot
}

func (c *rpcCallCounter) delta(before rpcCallCounts) rpcCallCounts {
	after := c.snapshot()
	delta := make(rpcCallCounts)
	for method, count := range after {
		if d := count - before[method]; d != 0 {
			delta[method] = d
		}
	}
	return delta
}

func (o *actionDigestObserver) unaryServerInterceptor(
	ctx context.Context,
	req any,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (any, error) {
	if request, ok := req.(*repb.GetActionResultRequest); ok {
		o.recordTraditional(request.GetActionDigest())
	}
	return handler(ctx, req)
}

func (o *actionDigestObserver) streamServerInterceptor(
	srv any,
	stream grpc.ServerStream,
	info *grpc.StreamServerInfo,
	handler grpc.StreamHandler,
) error {
	return handler(srv, &actionDigestServerStream{ServerStream: stream, observer: o})
}

func (o *graphProtocolObserver) streamServerInterceptor(
	srv any,
	stream grpc.ServerStream,
	info *grpc.StreamServerInfo,
	handler grpc.StreamHandler,
) error {
	if !strings.Contains(info.FullMethod, "GraphExecute") {
		return handler(srv, stream)
	}
	err := handler(srv, &graphProtocolServerStream{ServerStream: stream, observer: o})
	if err != nil && strings.Contains(err.Error(), "coordinated by another app") {
		o.mu.Lock()
		o.snapshot.AdmissionConflicts++
		o.mu.Unlock()
	}
	return err
}

func (s *graphProtocolServerStream) RecvMsg(message any) error {
	if err := s.ServerStream.RecvMsg(message); err != nil {
		return err
	}
	request, ok := message.(*graphpb.GraphExecuteRequest)
	if !ok {
		return nil
	}
	s.observer.mu.Lock()
	defer s.observer.mu.Unlock()
	if begin := request.GetBegin(); begin != nil {
		s.observer.snapshot.BeginSession = request.GetSessionId()
		s.observer.snapshot.BeginToken = append([]byte(nil), begin.GetResumeToken()...)
	}
	if resume := request.GetResume(); resume != nil {
		s.observer.snapshot.ResumeSession = resume.GetSessionId()
		s.observer.snapshot.ResumeToken = append([]byte(nil), resume.GetResumeToken()...)
	}
	if request.GetCommit() != nil && !s.observer.snapshot.CommitObserved {
		s.observer.snapshot.CommitObserved = true
		close(s.observer.commitObserved)
	}
	return nil
}

func (s *graphProtocolServerStream) SendMsg(message any) error {
	if response, ok := message.(*graphpb.GraphExecuteResponse); ok {
		if ack := response.GetBegin(); ack != nil {
			s.observer.mu.Lock()
			s.observer.snapshot.AckSession = ack.GetSessionId()
			s.observer.snapshot.AckToken = append([]byte(nil), ack.GetResumeToken()...)
			s.observer.mu.Unlock()
		}
		if response.GetNodeResult() != nil {
			s.observer.mu.Lock()
			s.observer.snapshot.NodeResults++
			s.observer.mu.Unlock()
		}
		if streamError := response.GetError(); streamError != nil && streamError.GetTerminal() {
			select {
			case s.observer.errors <- streamError.GetStatus().GetMessage():
			default:
			}
		}
		if result := response.GetResult(); result != nil && result.GetStatus().GetCode() != 0 {
			select {
			case s.observer.errors <- result.GetStatus().GetMessage():
			default:
			}
		}
		s.observer.mu.Lock()
		if _, shouldDrop := s.observer.dropResponseSequences[response.GetSequenceNumber()]; shouldDrop {
			delete(s.observer.dropResponseSequences, response.GetSequenceNumber())
			s.observer.snapshot.DroppedResponses = append(
				s.observer.snapshot.DroppedResponses, response.GetSequenceNumber())
			s.observer.mu.Unlock()
			return status.UnavailableErrorf(
				"injected GraphExecute response drop at sequence %d",
				response.GetSequenceNumber())
		}
		s.observer.mu.Unlock()
	}
	return s.ServerStream.SendMsg(message)
}

func (o *graphProtocolObserver) getSnapshot() graphProtocolSnapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	snapshot := o.snapshot
	snapshot.BeginToken = append([]byte(nil), snapshot.BeginToken...)
	snapshot.AckToken = append([]byte(nil), snapshot.AckToken...)
	snapshot.ResumeToken = append([]byte(nil), snapshot.ResumeToken...)
	snapshot.DroppedResponses = append([]uint64(nil), snapshot.DroppedResponses...)
	return snapshot
}

func requireGraphProtocolFailover(
	t *testing.T, primary, secondary graphProtocolSnapshot,
) {
	t.Helper()
	if primary.BeginSession == "" || primary.AckSession == "" {
		t.Fatalf("primary did not observe BeginGraph and BeginAck: %+v", primary)
	}
	if !primary.CommitObserved {
		t.Fatalf("primary did not observe CommitGraph before failover: %+v", primary)
	}
	if secondary.ResumeSession == "" {
		t.Fatalf("secondary did not observe ResumeGraph: %+v", secondary)
	}
	if secondary.AdmissionConflicts < 1 {
		t.Fatalf(
			"secondary observed no coordinator admission conflict while the crashed app's lease remained live: %+v",
			secondary)
	}
	if primary.BeginSession != primary.AckSession ||
		primary.AckSession != secondary.ResumeSession {
		t.Fatalf(
			"durable session changed across failover: begin=%q ack=%q resume=%q",
			primary.BeginSession, primary.AckSession, secondary.ResumeSession)
	}
	if len(primary.BeginToken) != 32 {
		t.Fatalf("client resume token length: got %d, want 32", len(primary.BeginToken))
	}
	if string(primary.BeginToken) != string(primary.AckToken) ||
		string(primary.AckToken) != string(secondary.ResumeToken) {
		t.Fatal("resume token changed across BeginGraph, BeginAck, and ResumeGraph")
	}
}

func requireLiveGraphLease(t *testing.T, rbe *rbetest.Env) time.Duration {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	keys, err := rbe.GetRedisClient().Keys(ctx, "graph-execution:*:lease").Result()
	if err != nil {
		t.Fatalf("list durable graph leases after app crash: %s", err)
	}
	if len(keys) != 1 {
		t.Fatalf("durable graph leases after app crash: got %v, want exactly one live lease", keys)
	}
	ttl, err := rbe.GetRedisClient().PTTL(ctx, keys[0]).Result()
	if err != nil {
		t.Fatalf("read durable graph lease TTL after app crash: %s", err)
	}
	if ttl <= 0 {
		t.Fatalf("durable graph lease was released during app crash: TTL=%s", ttl)
	}
	return ttl
}

func (s *actionDigestServerStream) SendMsg(message any) error {
	if response, ok := message.(*graphpb.GraphExecuteResponse); ok {
		if result := response.GetNodeResult(); result != nil {
			s.observer.recordGraph(result.GetActionDigest())
		}
	}
	return s.ServerStream.SendMsg(message)
}

func installFailoverDirector(
	t *testing.T, rbe *rbetest.Env, primaryTarget, secondaryTarget string,
) *failoverDirector {
	t.Helper()
	primary, err := grpc_client.DialSimpleWithoutPooling(primaryTarget)
	if err != nil {
		t.Fatalf("dial primary app: %s", err)
	}
	secondary, err := grpc_client.DialSimpleWithoutPooling(secondaryTarget)
	if err != nil {
		_ = primary.Close()
		t.Fatalf("dial secondary app: %s", err)
	}
	t.Cleanup(func() {
		if err := primary.Close(); err != nil {
			t.Errorf("close primary app connection: %s", err)
		}
		if err := secondary.Close(); err != nil {
			t.Errorf("close secondary app connection: %s", err)
		}
	})
	director := &failoverDirector{primary: primary, secondary: secondary}
	rbe.AppProxy.SetDirector(director.direct)
	return director
}

func (d *failoverDirector) direct(
	ctx context.Context, fullMethodName string,
) (context.Context, *grpc.ClientConn, error) {
	d.mu.Lock()
	target := d.secondary
	if strings.Contains(fullMethodName, "GraphExecute") {
		if d.useSecondary {
			d.secondaryGraphRPCs++
		} else {
			target = d.primary
			d.primaryGraphRPCs++
		}
	}
	d.mu.Unlock()
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		ctx = metadata.NewOutgoingContext(ctx, md.Copy())
	}
	return ctx, target, nil
}

func (d *failoverDirector) failover() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.useSecondary = true
}

func (d *failoverDirector) graphRPCCounts() (int, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.primaryGraphRPCs, d.secondaryGraphRPCs
}

func (r *taskExecutionRecorder) observe(task *repb.ScheduledTask) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, task.GetExecutionTask().GetExecutionId())
}

func (r *taskExecutionRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

func (o *actionDigestObserver) recordTraditional(digest *repb.Digest) {
	if digest == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.traditional = append(o.traditional, digestKey(digest))
}

func (o *actionDigestObserver) recordGraph(digest *repb.Digest) {
	if digest == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.graph = append(o.graph, digestKey(digest))
}

func (o *actionDigestObserver) snapshot() actionDigestSnapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	return actionDigestSnapshot{
		traditional: len(o.traditional),
		graph:       len(o.graph),
	}
}

func (o *actionDigestObserver) delta(mode string, before actionDigestSnapshot) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var observed []string
	switch mode {
	case "traditional":
		observed = o.traditional[before.traditional:]
	case "graph":
		observed = o.graph[before.graph:]
	default:
		return nil
	}
	set := make(map[string]struct{}, len(observed))
	for _, digest := range observed {
		set[digest] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for digest := range set {
		result = append(result, digest)
	}
	sort.Strings(result)
	return result
}

func digestKey(digest *repb.Digest) string {
	return fmt.Sprintf("%s/%d", digest.GetHash(), digest.GetSizeBytes())
}

func snapshotGraphMetrics(t *testing.T) graphMetricCounts {
	t.Helper()
	return graphMetricCounts{
		RequestMessages:    testmetrics.CounterValue(t, metrics.GraphExecutionRequestMessagesCount),
		ResponseMessages:   testmetrics.CounterValue(t, metrics.GraphExecutionResponseMessagesCount),
		RequestBytes:       testmetrics.CounterValue(t, metrics.GraphExecutionRequestBytesCount),
		ResponseBytes:      testmetrics.CounterValue(t, metrics.GraphExecutionResponseBytesCount),
		ReadyNodes:         testmetrics.CounterValue(t, metrics.GraphExecutionReadyNodesCount),
		CacheHitNodes:      testmetrics.CounterValue(t, metrics.GraphExecutionCacheHitNodesCount),
		ExecutedNodes:      testmetrics.CounterValue(t, metrics.GraphExecutionExecutedNodesCount),
		AvoidedExecuteRPCs: testmetrics.CounterValue(t, metrics.GraphExecutionAvoidedClientExecuteRPCsCount),
		BeginToFirstReadyUsec: testmetrics.CounterValue(
			t, metrics.GraphExecutionBeginToFirstReadyUsecCount),
		BeginToTerminalCompletionUsec: testmetrics.CounterValue(
			t, metrics.GraphExecutionBeginToTerminalCompletionUsecCount),
	}
}

func (c graphMetricCounts) subtract(before graphMetricCounts) graphMetricCounts {
	return graphMetricCounts{
		RequestMessages:    c.RequestMessages - before.RequestMessages,
		ResponseMessages:   c.ResponseMessages - before.ResponseMessages,
		RequestBytes:       c.RequestBytes - before.RequestBytes,
		ResponseBytes:      c.ResponseBytes - before.ResponseBytes,
		ReadyNodes:         c.ReadyNodes - before.ReadyNodes,
		CacheHitNodes:      c.CacheHitNodes - before.CacheHitNodes,
		ExecutedNodes:      c.ExecutedNodes - before.ExecutedNodes,
		AvoidedExecuteRPCs: c.AvoidedExecuteRPCs - before.AvoidedExecuteRPCs,
		BeginToFirstReadyUsec: c.BeginToFirstReadyUsec -
			before.BeginToFirstReadyUsec,
		BeginToTerminalCompletionUsec: c.BeginToTerminalCompletionUsec -
			before.BeginToTerminalCompletionUsec,
	}
}

func tasksStarted(t *testing.T) int {
	t.Helper()
	return int(testmetrics.CounterValue(t, metrics.RemoteExecutionTasksStartedCount))
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output file %q: %s", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func undeclaredOutputsDir(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR"); dir != "" {
		return dir
	}
	return t.TempDir()
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal benchmark report: %s", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write benchmark report %q: %s", path, err)
	}
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %q: %s", path, err)
	}
}
