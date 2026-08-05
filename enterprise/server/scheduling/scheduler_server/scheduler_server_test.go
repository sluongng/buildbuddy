package scheduler_server

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/backends/redis_execution_collector"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/experiments"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/githubapp"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/execution_server"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/tasksize"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/testutil/enterprise_testauth"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/testutil/enterprise_testenv"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/testutil/testredis"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/util/ci_runner_env"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/util/ci_runner_util"
	"github.com/buildbuddy-io/buildbuddy/server/environment"
	"github.com/buildbuddy-io/buildbuddy/server/interfaces"
	"github.com/buildbuddy-io/buildbuddy/server/testutil/testauth"
	"github.com/buildbuddy-io/buildbuddy/server/testutil/testcache"
	"github.com/buildbuddy-io/buildbuddy/server/testutil/testenv"
	"github.com/buildbuddy-io/buildbuddy/server/testutil/testfs"
	"github.com/buildbuddy-io/buildbuddy/server/util/log"
	"github.com/buildbuddy-io/buildbuddy/server/util/platform"
	"github.com/buildbuddy-io/buildbuddy/server/util/proto"
	"github.com/buildbuddy-io/buildbuddy/server/util/status"
	"github.com/buildbuddy-io/buildbuddy/server/util/testing/flags"
	"github.com/buildbuddy-io/buildbuddy/server/util/upgrade"
	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-feature/go-sdk/openfeature"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	ctxpb "github.com/buildbuddy-io/buildbuddy/proto/context"
	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
	scpb "github.com/buildbuddy-io/buildbuddy/proto/scheduler"
	uppb "github.com/buildbuddy-io/buildbuddy/proto/upgrade"
	flagd "github.com/open-feature/go-sdk-contrib/providers/flagd/pkg"
)

const (
	defaultOS   = "linux"
	defaultArch = "amd64"
)

type fakeTaskRouter struct {
	preferredExecutors []string
}

type fakeRankedNode struct {
	node      interfaces.ExecutionNode
	preferred bool
}

func (n fakeRankedNode) GetExecutionNode() interfaces.ExecutionNode {
	return n.node
}

func (n fakeRankedNode) IsPreferred() bool {
	return n.preferred
}

func (f *fakeTaskRouter) RankNodes(ctx context.Context, action *repb.Action, cmd *repb.Command, remoteInstanceName string, nodes []interfaces.ExecutionNode) []interfaces.RankedExecutionNode {
	rankedNodes := make([]interfaces.RankedExecutionNode, len(nodes))
	for i, node := range nodes {
		preferred := slices.Contains(f.preferredExecutors, node.GetExecutorId())
		rankedNodes[i] = fakeRankedNode{node: node, preferred: preferred}
	}

	// Return the preferred nodes first, then non-preferred nodes, both sections
	// sorted deterministically by executor ID.
	sort.Slice(rankedNodes, func(i, j int) bool {
		if rankedNodes[i].IsPreferred() && !rankedNodes[j].IsPreferred() {
			return true
		} else if !rankedNodes[i].IsPreferred() && rankedNodes[j].IsPreferred() {
			return false
		}
		return nodes[i].GetExecutorId() < nodes[j].GetExecutorId()
	})
	return rankedNodes
}

func (f *fakeTaskRouter) MarkSucceeded(ctx context.Context, action *repb.Action, cmd *repb.Command, remoteInstanceName, executorInstanceID string) {
}

func (f *fakeTaskRouter) MarkFailed(ctx context.Context, action *repb.Action, cmd *repb.Command, remoteInstanceName, executorInstanceID string) {
}

type schedulerOpts struct {
	options            Options
	userOwnedEnabled   bool
	groupOwnedEnabled  bool
	preferredExecutors []string
}

func getEnv(t *testing.T, opts *schedulerOpts, user string) (*testenv.TestEnv, context.Context) {
	redisTarget := testredis.Start(t).Target
	env := enterprise_testenv.GetCustomTestEnv(t, &enterprise_testenv.Options{
		RedisTarget: redisTarget,
	})
	if opts.options.Clock != nil {
		env.SetClock(opts.options.Clock)
	}

	flags.Set(t, "remote_execution.default_pool_name", "defaultPoolName")
	flags.Set(t, "remote_execution.shared_executor_pool_group_id", "sharedGroupID")

	err := tasksize.Register(env)
	require.NoError(t, err)
	err = redis_execution_collector.Register(env)
	require.NoError(t, err)

	server, runFunc, lis := testenv.RegisterLocalGRPCServer(t, env)
	testcache.Setup(t, env, lis)

	err = execution_server.Register(env)
	require.NoError(t, err)
	env.SetTaskRouter(&fakeTaskRouter{opts.preferredExecutors})
	s, err := NewSchedulerServerWithOptions(env, &opts.options)
	require.NoError(t, err)
	env.SetSchedulerService(s)

	scpb.RegisterSchedulerServer(server, env.GetSchedulerService())
	go runFunc()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	clientConn, err := testenv.LocalGRPCConn(ctx, lis)
	require.NoError(t, err)
	sc := scpb.NewSchedulerClient(clientConn)
	env.SetSchedulerClient(sc)

	testUsers := make(map[string]interfaces.UserInfo, 0)
	testUsers["user1"] = &testauth.TestUser{UserID: "user1", GroupID: "group1", UseGroupOwnedExecutors: opts.groupOwnedEnabled}

	ta := testauth.NewTestAuthenticator(t, testUsers)
	env.SetAuthenticator(ta)
	s.enableUserOwnedExecutors = opts.userOwnedEnabled

	if user != "" {
		authenticatedCtx, err := ta.WithAuthenticatedUser(context.Background(), user)
		require.NoError(t, err)
		ctx = authenticatedCtx
	}
	return env, ctx
}

func getScheduleServer(t *testing.T, userOwnedEnabled, groupOwnedEnabled bool, user string) (*SchedulerServer, context.Context) {
	env, ctx := getEnv(t, &schedulerOpts{userOwnedEnabled: userOwnedEnabled, groupOwnedEnabled: groupOwnedEnabled}, user)
	return env.GetSchedulerService().(*SchedulerServer), ctx
}

func TestSchedulerServerGetPoolInfoUserOwnedDisabled(t *testing.T) {
	s, ctx := getScheduleServer(t, false, false, "")
	p, err := s.GetPoolInfo(ctx, "linux", "amd64", "" /*=pool*/, "" /*=originalPool*/, "" /*=workflowID*/, platform.PoolTypeDefault)
	require.NoError(t, err)
	require.Equal(t, "", p.GroupID)
	require.Equal(t, "defaultPoolName", p.Name)
}

func TestSchedulerServerGetPoolInfoNoAuth(t *testing.T) {
	s, ctx := getScheduleServer(t, true, false, "")
	p, err := s.GetPoolInfo(ctx, "linux", "amd64", "" /*=pool*/, "" /*=originalPool*/, "" /*=workflowID*/, platform.PoolTypeDefault)
	require.NoError(t, err)
	require.Equal(t, "sharedGroupID", p.GroupID)
	require.Equal(t, "defaultPoolName", p.Name)
}

func TestSchedulerServerGetPoolInfoWithOS(t *testing.T) {
	s, ctx := getScheduleServer(t, true, false, "user1")
	s.forceUserOwnedDarwinExecutors = false
	p, err := s.GetPoolInfo(ctx, "linux", "amd64", "" /*=pool*/, "" /*=originalPool*/, "" /*=workflowID*/, platform.PoolTypeDefault)
	require.NoError(t, err)
	require.Equal(t, "sharedGroupID", p.GroupID)
	require.Equal(t, "defaultPoolName", p.Name)

	p, err = s.GetPoolInfo(ctx, "darwin", "arm64", "" /*=pool*/, "" /*=originalPool*/, "" /*=workflowID*/, platform.PoolTypeDefault)
	require.NoError(t, err)
	require.Equal(t, "sharedGroupID", p.GroupID)
	require.Equal(t, "defaultPoolName", p.Name)

	s.forceUserOwnedDarwinExecutors = true
	p, err = s.GetPoolInfo(ctx, "linux", "amd64", "" /*=pool*/, "" /*=originalPool*/, "" /*=workflowID*/, platform.PoolTypeDefault)
	require.NoError(t, err)
	require.Equal(t, "sharedGroupID", p.GroupID)
	require.Equal(t, "defaultPoolName", p.Name)

	p, err = s.GetPoolInfo(ctx, "darwin", "arm64", "" /*=pool*/, "" /*=originalPool*/, "" /*=workflowID*/, platform.PoolTypeDefault)
	require.NoError(t, err)
	require.Equal(t, "group1", p.GroupID)
	require.Equal(t, "", p.Name)
}

func TestSchedulerServerGetPoolInfoWithRequestedPoolWithAuth(t *testing.T) {
	s, ctx := getScheduleServer(t, true, true, "user1")
	s.forceUserOwnedDarwinExecutors = false
	p, err := s.GetPoolInfo(ctx, "linux", "amd64", "my-pool", "" /*=originalPool*/, "" /*=workflowID*/, platform.PoolTypeDefault)
	require.NoError(t, err)
	require.Equal(t, "group1", p.GroupID)
	require.Equal(t, "my-pool", p.Name)
}

func TestSchedulerServerGetPoolInfoWithRequestedPoolWithNoAuth(t *testing.T) {
	s, ctx := getScheduleServer(t, true, false, "")
	s.forceUserOwnedDarwinExecutors = false
	p, err := s.GetPoolInfo(ctx, "linux", "amd64", "my-pool", "" /*=originalPool*/, "" /*=workflowID*/, platform.PoolTypeDefault)
	require.NoError(t, err)
	require.Equal(t, "sharedGroupID", p.GroupID)
	require.Equal(t, "my-pool", p.Name)
}

func TestSchedulerServerGetPoolInfoDarwin(t *testing.T) {
	s, ctx := getScheduleServer(t, true, false, "")
	p, err := s.GetPoolInfo(ctx, "linux", "amd64", "" /*=pool*/, "" /*=originalPool*/, "" /*=workflowID*/, platform.PoolTypeDefault)
	require.NoError(t, err)
	require.Equal(t, "sharedGroupID", p.GroupID)
	require.Equal(t, "defaultPoolName", p.Name)
}

func TestSchedulerServerGetPoolInfoDarwinNoAuth(t *testing.T) {
	s, ctx := getScheduleServer(t, true, false, "")
	s.forceUserOwnedDarwinExecutors = true
	_, err := s.GetPoolInfo(ctx, "darwin", "arm64", "" /*=pool*/, "" /*=originalPool*/, "" /*=workflowID*/, platform.PoolTypeDefault)
	require.Error(t, err)
}

func TestSchedulerServerGetPoolInfoSelfHostedNoAuth(t *testing.T) {
	s, ctx := getScheduleServer(t, true, false, "")
	_, err := s.GetPoolInfo(ctx, "linux", "amd64", "" /*=pool*/, "" /*=originalPool*/, "" /*=workflowID*/, platform.PoolTypeSelfHosted)
	require.Error(t, err)
}

func TestSchedulerServerGetPoolInfoSelfHosted(t *testing.T) {
	s, ctx := getScheduleServer(t, true, false, "user1")
	p, err := s.GetPoolInfo(ctx, "linux", "amd64", "" /*=pool*/, "" /*=originalPool*/, "" /*=workflowID*/, platform.PoolTypeSelfHosted)
	require.NoError(t, err)
	require.Equal(t, "group1", p.GroupID)
	require.Equal(t, "", p.Name)

	// Linux workflows should respect useSelfHosted bool.
	p, err = s.GetPoolInfo(ctx, "linux", "amd64", "workflows", "" /*=originalPool*/, "WF1234" /*=workflowID*/, platform.PoolTypeDefault)
	require.NoError(t, err)
	require.Equal(t, "sharedGroupID", p.GroupID)
	require.Equal(t, "workflows", p.Name)

	p, err = s.GetPoolInfo(ctx, "linux", "amd64", "workflows", "" /*=originalPool*/, "WF1234" /*=workflowID*/, platform.PoolTypeSelfHosted)
	require.NoError(t, err)
	require.Equal(t, "group1", p.GroupID)
	require.Equal(t, "workflows", p.Name)
}

func TestSchedulerServerGetPoolInfoSelfHostedByDefault(t *testing.T) {
	s, ctx := getScheduleServer(t, true, true, "user1")
	p, err := s.GetPoolInfo(ctx, "linux", "amd64", "", "" /*=originalPool*/, "" /*=workflowID*/, platform.PoolTypeDefault)
	require.NoError(t, err)
	require.Equal(t, "group1", p.GroupID)
	require.Equal(t, "", p.Name)

	p, err = s.GetPoolInfo(ctx, "linux", "amd64", "", "" /*=originalPool*/, "" /*=workflowID*/, platform.PoolTypeSelfHosted)
	require.NoError(t, err)
	require.Equal(t, "group1", p.GroupID)
	require.Equal(t, "", p.Name)

	// Explicitly set use-self-hosted-executors=false
	p, err = s.GetPoolInfo(ctx, "linux", "amd64", "" /*=pool*/, "" /*=originalPool*/, "" /*=workflowID*/, platform.PoolTypeShared)
	require.NoError(t, err)
	require.Equal(t, "sharedGroupID", p.GroupID)
	require.Equal(t, "defaultPoolName", p.Name)

	// Linux workflows should respect useSelfHosted bool.
	p, err = s.GetPoolInfo(ctx, "linux", "amd64", "workflows", "" /*=originalPool*/, "WF1234" /*=workflowID*/, platform.PoolTypeDefault)
	require.NoError(t, err)
	require.Equal(t, "sharedGroupID", p.GroupID)
	require.Equal(t, "workflows", p.Name)

	p, err = s.GetPoolInfo(ctx, "linux", "amd64", "workflows", "" /*=originalPool*/, "WF1234" /*=workflowID*/, platform.PoolTypeSelfHosted)
	require.NoError(t, err)
	require.Equal(t, "group1", p.GroupID)
	require.Equal(t, "workflows", p.Name)
}

func TestSchedulerServerGetPoolInfoWithPoolOverride(t *testing.T) {
	tmp := testfs.MakeTempDir(t)
	overridePool := "experimental-linux-amd64-pool"
	configFile := testfs.WriteFile(t, tmp, "config.flagd.json", `{
	"$schema": "https://flagd.dev/schema/v0/flags.json",
	"flags": {
		"remote_execution.pool_override": {
			"state": "ENABLED",
			"defaultVariant": "default",
			"variants": {
				"experimentalPool": {
					"pool": "`+overridePool+`"
				},
				"default": {}
			},
			"targeting": {
				"if": [
					{
						"and": [
							{ "==": [{ "var": "os" }, "linux"] },
							{ "==": [{ "var": "arch" }, "amd64"] }
						]
					},
					"experimentalPool"
				]
			}
		}
	}
}`)
	provider, err := flagd.NewProvider(flagd.WithInProcessResolver(), flagd.WithOfflineFilePath(configFile))
	require.NoError(t, err)
	openfeature.SetProviderAndWait(provider)
	fp, err := experiments.NewFlagProvider("test")
	require.NoError(t, err)

	env, ctx := getEnv(t, &schedulerOpts{userOwnedEnabled: true}, "user1")
	env.SetExperimentFlagProvider(fp)
	s := env.GetSchedulerService()

	p, err := s.GetPoolInfo(ctx, "linux", "arm64", "" /*=pool*/, "" /*=originalPool*/, "" /*=workflowID*/, platform.PoolTypeDefault)
	require.NoError(t, err)
	require.Equal(t, &interfaces.PoolInfo{
		GroupID:  "sharedGroupID",
		Name:     "defaultPoolName",
		IsShared: true,
	}, p)

	p, err = s.GetPoolInfo(ctx, "darwin", "amd64", "" /*=pool*/, "" /*=originalPool*/, "" /*=workflowID*/, platform.PoolTypeDefault)
	require.NoError(t, err)
	require.Equal(t, &interfaces.PoolInfo{
		GroupID:  "sharedGroupID",
		Name:     "defaultPoolName",
		IsShared: true,
	}, p)

	p, err = s.GetPoolInfo(ctx, "linux", "amd64", "" /*=pool*/, "" /*=originalPool*/, "" /*=workflowID*/, platform.PoolTypeDefault)
	require.NoError(t, err)
	require.Equal(t, &interfaces.PoolInfo{
		GroupID:  "sharedGroupID",
		Name:     overridePool,
		IsShared: true,
	}, p)
}

func TestSchedulerServerPersistentVolumes(t *testing.T) {
	tmp := testfs.MakeTempDir(t)
	configFile := testfs.WriteFile(t, tmp, "config.flagd.json", `{
	"$schema": "https://flagd.dev/schema/v0/flags.json",
	"flags": {
		"remote_execution.persistent_volumes": {
			"state": "ENABLED",
			"defaultVariant": "default",
			"variants": {
				"tmp-cache": "cache:/tmp/.cache",
				"default": ""
			},
			"targeting": {
				"fractional": [
					["tmp-cache", 50],
					["default", 0]
				]
			}
		}
	}
}`)
	provider, err := flagd.NewProvider(flagd.WithInProcessResolver(), flagd.WithOfflineFilePath(configFile))
	require.NoError(t, err)
	openfeature.SetProviderAndWait(provider)
	fp, err := experiments.NewFlagProvider("test")
	require.NoError(t, err)

	env, ctx := getEnv(t, &schedulerOpts{}, "")
	env.SetExperimentFlagProvider(fp)
	fe := newFakeExecutor(ctx, t, env.GetSchedulerClient())
	fe.Register()

	taskID := scheduleTask(ctx, t, env, map[string]string{})

	fe.WaitForTask(taskID)
	lease := fe.Claim(taskID)
	defer lease.Finalize()

	require.Equal(t, []string{"remote_execution.persistent_volumes:tmp-cache"}, lease.task.GetExperiments())
	require.Empty(t, cmp.Diff([]*repb.Platform_Property{
		{Name: "persistent-volumes", Value: "cache:/tmp/.cache"},
	}, lease.task.GetPlatformOverrides().GetProperties(), protocmp.Transform()), nil)
}

type task struct {
	delay time.Duration
}

type Result[T any] struct {
	Value T
	Err   error
}

type schedulerRequest struct {
	request *scpb.RegisterAndStreamWorkRequest
	reply   chan error
}

type fakeExecutor struct {
	t               *testing.T
	schedulerClient scpb.SchedulerClient

	id   string
	node *scpb.ExecutionNode

	ctx       context.Context
	stop      context.CancelFunc
	unhealthy atomic.Bool
	wg        sync.WaitGroup

	mu    sync.Mutex
	tasks map[string]task

	send chan *scpb.RegisterAndStreamWorkRequest
	// Channel for inspecting replies from the scheduler within test cases. This
	// is a buffered channel - sends are non-blocking to avoid blocking the
	// scheduler goroutine. Tests that aren't interested in asserting on the
	// scheduler's replies don't need to receive from this channel.
	schedulerMessages chan *scpb.RegisterAndStreamWorkResponse
}

func newFakeExecutor(ctx context.Context, t *testing.T, schedulerClient scpb.SchedulerClient) *fakeExecutor {
	id, err := uuid.NewRandom()
	require.NoError(t, err)
	return newFakeExecutorWithId(ctx, t, id.String(), schedulerClient)
}

func newFakeExecutorWithId(ctx context.Context, t *testing.T, id string, schedulerClient scpb.SchedulerClient) *fakeExecutor {
	node := &scpb.ExecutionNode{
		ExecutorId:            id,
		OsFamily:              defaultOS,
		Arch:                  defaultArch,
		Host:                  "foo",
		AssignableMemoryBytes: 64_000_000_000,
		AssignableMilliCpu:    32_000,
	}
	ctx = log.EnrichContext(ctx, "executor_id", id)
	ctx, cancel := context.WithCancel(ctx)
	fe := &fakeExecutor{
		t:                 t,
		schedulerClient:   schedulerClient,
		id:                id,
		ctx:               ctx,
		tasks:             make(map[string]task),
		node:              node,
		send:              make(chan *scpb.RegisterAndStreamWorkRequest),
		schedulerMessages: make(chan *scpb.RegisterAndStreamWorkResponse, 128),
	}
	t.Cleanup(func() {
		cancel()
		fe.wait()
	})
	return fe
}

func (e *fakeExecutor) markUnhealthy() {
	e.unhealthy.Store(true)
}

// Send sends a request to the scheduler.
func (e *fakeExecutor) Send(req *scpb.RegisterAndStreamWorkRequest) {
	// Send via channel to the goroutine managing the stream, since it's not
	// safe to send on the stream from multiple goroutines.
	e.send <- req
}

func (e *fakeExecutor) Register() {
	ctx := e.ctx
	stream, err := e.schedulerClient.RegisterAndStreamWork(e.ctx)
	require.NoError(e.t, err)
	err = stream.Send(&scpb.RegisterAndStreamWorkRequest{
		RegisterExecutorRequest: &scpb.RegisterExecutorRequest{
			Node: e.node,
		},
	})
	require.NoError(e.t, err)

	recvChan := make(chan Result[*scpb.RegisterAndStreamWorkResponse], 1)
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		for {
			msg, err := stream.Recv()
			recvChan <- Result[*scpb.RegisterAndStreamWorkResponse]{Value: msg, Err: err}
			if err != nil {
				return
			}
		}
	}()
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-recvChan:
				rsp, err := msg.Value, msg.Err
				if streamClosed(ctx, err) {
					return
				}
				require.NoError(e.t, err)
				log.CtxInfof(ctx, "Received scheduler message: %+v", rsp)
				if req := rsp.GetEnqueueTaskReservationRequest(); req != nil {
					if e.unhealthy.Load() {
						log.CtxInfof(ctx, "Executor %s got task %q but is unhealthy -- ignoring so it times out", e.id, req.GetTaskId())
					} else {
						err = stream.Send(&scpb.RegisterAndStreamWorkRequest{
							EnqueueTaskReservationResponse: &scpb.EnqueueTaskReservationResponse{
								TaskId: req.GetTaskId(),
							},
						})
						if streamClosed(ctx, err) {
							return
						}
						require.NoError(e.t, err)
						e.mu.Lock()
						log.CtxInfof(ctx, "Executor %s got task %q with scheduling delay %s", e.id, req.GetTaskId(), req.GetDelay())
						taskID := req.GetTaskId()
						e.tasks[taskID] = task{delay: req.GetDelay().AsDuration()}
						e.mu.Unlock()
					}
				}
				// Best effort: notify the test of every scheduler reply.
				select {
				case e.schedulerMessages <- rsp:
				default:
				}
			case req := <-e.send:
				err := stream.Send(req)
				if streamClosed(ctx, err) {
					return
				}
				require.NoError(e.t, err)
			}
		}
	}()

	// Give the executor a moment to register with the scheduler.
	// TODO: explicitly wait for a scheduler reply.
	time.Sleep(100 * time.Millisecond)
}

func streamClosed(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	return ctx.Err() != nil || errors.Is(err, io.EOF) || status.IsCanceledError(err) || status.IsUnavailableError(err)
}

func (e *fakeExecutor) NextSchedulerMessage() *scpb.RegisterAndStreamWorkResponse {
	select {
	case msg := <-e.schedulerMessages:
		return msg
	case <-time.After(5 * time.Second):
		require.FailNowf(e.t, "executor did not receive scheduler message", "executor %s", e.id)
	}
	return nil
}

func (e *fakeExecutor) EnsureNoSchedulerMessage() {
	select {
	case msg := <-e.schedulerMessages:
		require.FailNowf(e.t, "executor received scheduler message but was not expecting it", "executor %s message: %+v", e.id, msg)
	case <-time.After(100 * time.Millisecond):
	}
}

func (e *fakeExecutor) wait() {
	e.wg.Wait()
}

func (e *fakeExecutor) WaitForTask(taskID string) {
	e.WaitForTaskWithDelay(taskID, 0*time.Second)
}

func (e *fakeExecutor) WaitForTaskWithDelay(taskID string, delay time.Duration) {
	for i := 0; i < 5; i++ {
		e.mu.Lock()
		task, ok := e.tasks[taskID]
		e.mu.Unlock()
		if ok {
			if task.delay == delay {
				return
			} else {
				require.FailNowf(e.t, "executor received task with unexpected delay", "executor %s task %q expected delay: %s actual delay: %s", e.id, taskID, delay, task.delay)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.FailNowf(e.t, "executor did not receive task", "task %q", taskID)
}

func (e *fakeExecutor) EnsureTaskNotReceived(taskID string) {
	// Allow some time for re-enqueuing, etc. No easy way around this,
	// unfortunately.
	time.Sleep(100 * time.Millisecond)
	e.mu.Lock()
	_, ok := e.tasks[taskID]
	e.mu.Unlock()
	if ok {
		require.FailNowf(e.t, "executor received task but was not expecting it", "task %q", taskID)
	}
}

func (e *fakeExecutor) ResetTasks() {
	e.mu.Lock()
	e.tasks = make(map[string]task)
	e.mu.Unlock()
}

type taskLease struct {
	t       *testing.T
	stream  scpb.Scheduler_LeaseTaskClient
	leaseID string
	taskID  string
	task    *repb.ExecutionTask
}

func (tl *taskLease) Renew() error {
	err := tl.stream.Send(&scpb.LeaseTaskRequest{
		TaskId: tl.taskID,
	})
	if err != nil {
		return err
	}
	_, err = tl.stream.Recv()
	if err != nil {
		return err
	}
	return nil
}

func (tl *taskLease) Finalize() error {
	err := tl.stream.Send(&scpb.LeaseTaskRequest{
		TaskId:   tl.taskID,
		Finalize: true,
	})
	if err != nil {
		return err
	}
	_, err = tl.stream.Recv()
	if err != nil {
		return err
	}
	return nil
}

func (tl *taskLease) ReEnqueue() error {
	err := tl.stream.Send(&scpb.LeaseTaskRequest{
		TaskId:    tl.taskID,
		ReEnqueue: true,
	})
	if err != nil {
		return err
	}
	_, err = tl.stream.Recv()
	if err != nil {
		return err
	}
	return nil
}

func (e *fakeExecutor) Claim(taskID string) *taskLease {
	lease, err := e.leaseTask(taskID, "" /*=reconnectToken*/)
	require.NoError(e.t, err)
	return lease
}

// Reconnect claims a task using a reconnect token from a previous claim.
func (e *fakeExecutor) Reconnect(taskID, reconnectToken string) (*taskLease, error) {
	require.NotEmpty(e.t, reconnectToken)
	return e.leaseTask(taskID, reconnectToken)
}

func (e *fakeExecutor) leaseTask(taskID, reconnectToken string) (*taskLease, error) {
	stream, err := e.schedulerClient.LeaseTask(e.ctx)
	if err != nil {
		return nil, err
	}
	err = stream.Send(&scpb.LeaseTaskRequest{
		TaskId:            taskID,
		ExecutorId:        e.id,
		ExecutorHostname:  e.node.GetHost(),
		SupportsReconnect: true,
		ReconnectToken:    reconnectToken,
	})
	if err != nil {
		return nil, err
	}
	rsp, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	require.NotZero(e.t, rsp.GetLeaseDurationSeconds())

	var task *repb.ExecutionTask
	if len(rsp.GetSerializedTask()) > 0 {
		task = &repb.ExecutionTask{}
		err = proto.Unmarshal(rsp.GetSerializedTask(), task)
		require.NoError(e.t, err)
	}

	lease := &taskLease{
		t:       e.t,
		stream:  stream,
		taskID:  taskID,
		task:    task,
		leaseID: rsp.GetLeaseId(),
	}
	return lease, nil
}

type scheduleOpts struct {
	props map[string]string
}

func newScheduleRequest(ctx context.Context, t *testing.T, env environment.Env, opts scheduleOpts) *scpb.ScheduleTaskRequest {
	id, err := uuid.NewRandom()
	require.NoError(t, err)
	taskID := id.String()
	task := &repb.ExecutionTask{
		ExecutionId: taskID,
		Command: &repb.Command{
			Platform: &repb.Platform{
				Properties: []*repb.Platform_Property{},
			},
		},
	}
	for k, v := range opts.props {
		task.Command.Platform.Properties = append(task.Command.Platform.Properties, &repb.Platform_Property{Name: k, Value: v})
	}
	size := tasksize.Override(tasksize.Default(task), tasksize.Requested(task))
	taskBytes, err := proto.Marshal(task)
	require.NoError(t, err)
	return &scpb.ScheduleTaskRequest{
		TaskId: taskID,
		Metadata: &scpb.SchedulingMetadata{
			Os:       defaultOS,
			Arch:     defaultArch,
			TaskSize: size,
		},
		SerializedTask: taskBytes,
	}
}

func newEnsureTaskRequest(req *scpb.ScheduleTaskRequest) *scpb.EnsureTaskRequest {
	return &scpb.EnsureTaskRequest{
		TaskId:         req.GetTaskId(),
		Metadata:       proto.Clone(req.GetMetadata()).(*scpb.SchedulingMetadata),
		SerializedTask: slices.Clone(req.GetSerializedTask()),
	}
}

func scheduleTask(ctx context.Context, t *testing.T, env environment.Env, props map[string]string) string {
	req := newScheduleRequest(ctx, t, env, scheduleOpts{props: props})
	_, err := env.GetSchedulerService().ScheduleTask(ctx, req)
	require.NoError(t, err)
	return req.GetTaskId()
}

func enqueueTaskReservation(ctx context.Context, t *testing.T, env environment.Env, delay time.Duration) string {
	id, err := uuid.NewRandom()
	require.NoError(t, err)
	taskID := id.String()

	require.NoError(t, err)
	_, err = env.GetSchedulerService().EnqueueTaskReservation(ctx, &scpb.EnqueueTaskReservationRequest{
		TaskId: taskID,
		TaskSize: &scpb.TaskSize{
			EstimatedMemoryBytes:   100,
			EstimatedMilliCpu:      100,
			EstimatedFreeDiskBytes: 100,
		},
		SchedulingMetadata: &scpb.SchedulingMetadata{
			Os:   defaultOS,
			Arch: defaultArch,
			TaskSize: &scpb.TaskSize{
				EstimatedMemoryBytes:   100,
				EstimatedMilliCpu:      100,
				EstimatedFreeDiskBytes: 100,
			},
		},
		Delay: durationpb.New(delay),
	})
	require.NoError(t, err)
	return taskID
}

func TestExecutorReEnqueue_NoLeaseID(t *testing.T) {
	env, ctx := getEnv(t, &schedulerOpts{}, "user1")

	fe := newFakeExecutor(ctx, t, env.GetSchedulerClient())
	fe.Register()

	taskID := scheduleTask(ctx, t, env, map[string]string{})
	fe.WaitForTask(taskID)
	fe.Claim(taskID)

	fe.ResetTasks()
	_, err := env.GetSchedulerClient().ReEnqueueTask(ctx, &scpb.ReEnqueueTaskRequest{
		TaskId: taskID,
		Reason: "for fun",
	})
	require.NoError(t, err)
	// On a successful re-enqueue the executor should receive the task again.
	fe.WaitForTask(taskID)
}

func TestExecutorReEnqueue_MatchingLeaseID(t *testing.T) {
	env, ctx := getEnv(t, &schedulerOpts{}, "user1")

	fe := newFakeExecutor(ctx, t, env.GetSchedulerClient())
	fe.Register()

	taskID := scheduleTask(ctx, t, env, map[string]string{})
	fe.WaitForTask(taskID)
	lease := fe.Claim(taskID)

	fe.ResetTasks()
	_, err := env.GetSchedulerClient().ReEnqueueTask(ctx, &scpb.ReEnqueueTaskRequest{
		TaskId:  taskID,
		Reason:  "for fun",
		LeaseId: lease.leaseID,
	})
	require.NoError(t, err)
	// On a successful re-enqueue the executor should receive the task again.
	fe.WaitForTask(taskID)
}

func TestExecutorReEnqueue_NonMatchingLeaseID(t *testing.T) {
	env, ctx := getEnv(t, &schedulerOpts{}, "user1")

	fe := newFakeExecutor(ctx, t, env.GetSchedulerClient())
	fe.Register()

	taskID := scheduleTask(ctx, t, env, map[string]string{})
	fe.WaitForTask(taskID)
	fe.Claim(taskID)

	_, err := env.GetSchedulerClient().ReEnqueueTask(ctx, &scpb.ReEnqueueTaskRequest{
		TaskId:  taskID,
		Reason:  "for fun",
		LeaseId: "bad lease ID",
	})
	require.True(t, status.IsPermissionDeniedError(err))
}

func TestExecutorReEnqueue_RetriesDisabled(t *testing.T) {
	env, ctx := getEnv(t, &schedulerOpts{}, "user1")

	fe := newFakeExecutor(ctx, t, env.GetSchedulerClient())
	fe.Register()

	taskID := scheduleTask(ctx, t, env, map[string]string{platform.RetryPropertyName: "false"})
	fe.WaitForTask(taskID)
	lease := fe.Claim(taskID)
	fe.ResetTasks()

	_, err := env.GetSchedulerClient().ReEnqueueTask(ctx, &scpb.ReEnqueueTaskRequest{
		TaskId:  taskID,
		Reason:  "for fun",
		LeaseId: lease.leaseID,
	})
	require.NoError(t, err)

	// Ensure the task was never re-enqueued
	fe.EnsureTaskNotReceived(taskID)
}

func TestLeaseExpiration(t *testing.T) {
	fakeClock := clockwork.NewFakeClock()
	env, ctx := getEnv(t, &schedulerOpts{options: Options{
		Clock:            fakeClock,
		LeaseDuration:    10 * time.Second,
		LeaseGracePeriod: 10 * time.Second,
	}}, "user1")

	fe := newFakeExecutor(ctx, t, env.GetSchedulerClient())
	fe.Register()

	taskID := scheduleTask(ctx, t, env, map[string]string{})
	fe.WaitForTask(taskID)
	lease := fe.Claim(taskID)

	// Reset known tasks so that we can tell if the task gets re-enqueued.
	fe.ResetTasks()
	// Lease should expire after 20 seconds so there should be no expiration
	// right now.
	fakeClock.Advance(19 * time.Second)
	fe.EnsureTaskNotReceived(taskID)

	// Renew task lease to avoid expiration.
	err := lease.Renew()
	require.NoError(t, err)
	// Get close to expiration, but lease should not expire yet.
	fakeClock.Advance(20 * time.Second)
	fe.EnsureTaskNotReceived(taskID)

	// Move past the grace period. Task should be re-enqueued.
	fakeClock.Advance(2 * time.Second)
	fe.WaitForTask(taskID)

	// Lease renewal should fail as the stream should be broken.
	err = lease.Renew()
	require.ErrorIs(t, io.EOF, err)
}

func TestLeaseReconnectGrace_OtherExecutorsCannotStealTask(t *testing.T) {
	// Set a high grace period since we use real time in the test.
	// TODO: use fake time.
	flags.Set(t, "remote_execution.lease_reconnect_grace_period", 24*time.Hour)
	// Disable unclaimed tasks cache so we test immediate work stealing.
	flags.Set(t, "remote_execution.unclaimed_tasks_cache_ttl", 0*time.Second)
	env, ctx := getEnv(t, &schedulerOpts{}, "user1")

	holder := newFakeExecutorWithId(ctx, t, "holder", env.GetSchedulerClient())
	holder.Register()

	// The holder claims a task so that a scheduler shutdown can leave the
	// task reserved for the original lease holder.
	taskID := scheduleTask(ctx, t, env, map[string]string{})
	holder.WaitForTask(taskID)
	lease := holder.Claim(taskID)

	// Simulate a scheduler shutdown path that re-enqueues the task while
	// allowing the holder to re-establish its lease.
	s := env.GetSchedulerService().(*SchedulerServer)
	reconnectToken := lease.leaseID
	err := s.reEnqueueTask(ctx, taskID, lease.leaseID, reconnectToken, 1 /*=numReplicas*/, "server shutting down")
	require.NoError(t, err)

	// A newly registered executor asks for work and samples unclaimed tasks.
	// While the task is reserved for the holder to reconnect, it must not be
	// handed out, so the thief should not receive a reservation for it.
	thief := newFakeExecutorWithId(ctx, t, "thief", env.GetSchedulerClient())
	thief.Register()
	thief.EnsureTaskNotReceived(taskID)

	// Even if the thief attempts a lease directly (e.g. acting on a stale
	// reservation), the claim must be rejected until the grace period expires.
	_, err = thief.leaseTask(taskID, "" /*=reconnectToken*/)
	require.True(t, status.IsNotFoundError(err), "unexpected claim error: %s", err)

	// The original holder can still reconnect using the token it received
	// before the scheduler shutdown.
	reconnectedLease, err := holder.Reconnect(taskID, reconnectToken)
	require.NoError(t, err)
	require.NotEmpty(t, reconnectedLease.leaseID)

	// Simulate the executor shutting down by canceling the newly reconnected
	// lease. Since the reconnect was successful, this should re-enqueue the
	// task without preserving the stale reconnect grace period.
	require.NoError(t, reconnectedLease.ReEnqueue())

	// Because the lease is canceled, another client should be able to get
	// the lease now without waiting for the old reconnect grace period.
	normalClient := newFakeExecutorWithId(ctx, t, "norm", env.GetSchedulerClient())
	normalLease := normalClient.Claim(taskID)
	require.NotEmpty(t, normalLease.leaseID)
	require.NoError(t, normalLease.Finalize())
}

func TestLeaseTask_RefreshToken_FailureDoesNotFailLease(t *testing.T) {
	env, ctx := getEnv(t, &schedulerOpts{}, "user1")
	fe := newFakeExecutor(ctx, t, env.GetSchedulerClient())
	fe.Register()

	gh, err := githubapp.NewAppService(env, nil, nil)
	require.NoError(t, err)
	env.SetGitHubAppService(gh)

	// Simulate an auth error for token refresh.
	env.GetAuthenticator().(*testauth.TestAuthenticator).APIKeyProvider = func(ctx context.Context, apiKey string) (interfaces.UserInfo, error) {
		return nil, status.UnauthenticatedError("invalid API key")
	}

	overrides := ci_runner_env.BuildBuddyAPIKeyEnvVarName + "=INVALID_API_KEY"
	task := &repb.ExecutionTask{
		ExecutionId: "task1",
		Command: &repb.Command{
			Arguments: []string{"./" + ci_runner_util.ExecutableName, "--pushed_repo_url=https://github.com/acme-inc/repo"},
		},
		PlatformOverrides: &repb.Platform{
			Properties: []*repb.Platform_Property{
				{
					Name:  platform.EnvOverridesPropertyName,
					Value: overrides,
				},
			},
		},
	}
	taskBytes, err := proto.Marshal(task)
	require.NoError(t, err)

	req := &scpb.ScheduleTaskRequest{
		TaskId:         "task1",
		SerializedTask: taskBytes,
		Metadata: &scpb.SchedulingMetadata{
			Os:          defaultOS,
			Arch:        defaultArch,
			TaskGroupId: "group1",
			TaskSize: &scpb.TaskSize{
				EstimatedMemoryBytes:   100,
				EstimatedMilliCpu:      100,
				EstimatedFreeDiskBytes: 100,
			},
		},
	}

	_, err = env.GetSchedulerService().ScheduleTask(ctx, req)
	require.NoError(t, err)
	fe.WaitForTask(req.GetTaskId())
	lease := fe.Claim(req.GetTaskId())
	defer lease.Finalize()

	require.Equal(t, task, lease.task)
}

func TestSchedulingDelay_NoDelay(t *testing.T) {
	env, ctx := getEnv(t, &schedulerOpts{}, "user1")

	fe1 := newFakeExecutorWithId(ctx, t, "1", env.GetSchedulerClient())
	fe2 := newFakeExecutorWithId(ctx, t, "2", env.GetSchedulerClient())
	fe1.Register()
	fe2.Register()

	taskID := scheduleTask(ctx, t, env, map[string]string{})

	fe1.WaitForTaskWithDelay(taskID, 0*time.Second)
	fe2.WaitForTaskWithDelay(taskID, 0*time.Second)
}

func TestSchedulingDelay_DelayTooSmall(t *testing.T) {
	env, ctx := getEnv(t, &schedulerOpts{}, "user1")

	fe1 := newFakeExecutorWithId(ctx, t, "1", env.GetSchedulerClient())
	fe2 := newFakeExecutorWithId(ctx, t, "2", env.GetSchedulerClient())
	fe1.Register()
	fe2.Register()

	taskID := scheduleTask(ctx, t, env, map[string]string{"runner-recycling-max-wait": "-1s"})

	fe1.WaitForTaskWithDelay(taskID, 0*time.Second)
	fe2.WaitForTaskWithDelay(taskID, 0*time.Second)
}

func TestSchedulingDelay_NoPreferredExecutors(t *testing.T) {
	env, ctx := getEnv(t, &schedulerOpts{}, "user1")

	fe1 := newFakeExecutorWithId(ctx, t, "1", env.GetSchedulerClient())
	fe2 := newFakeExecutorWithId(ctx, t, "2", env.GetSchedulerClient())
	fe1.Register()
	fe2.Register()

	taskID := scheduleTask(ctx, t, env, map[string]string{"runner-recycling-max-wait": "5s"})

	fe1.WaitForTaskWithDelay(taskID, 0*time.Second)
	fe2.WaitForTaskWithDelay(taskID, 0*time.Second)
}

func TestSchedulingDelay_DelayTooLarge(t *testing.T) {
	env, ctx := getEnv(t, &schedulerOpts{preferredExecutors: []string{"2"}}, "user1")

	fe1 := newFakeExecutorWithId(ctx, t, "1", env.GetSchedulerClient())
	fe2 := newFakeExecutorWithId(ctx, t, "2", env.GetSchedulerClient())
	fe1.Register()
	fe2.Register()

	taskID := scheduleTask(ctx, t, env, map[string]string{"runner-recycling-max-wait": "1h"})

	fe1.WaitForTaskWithDelay(taskID, 5*time.Second)
	fe2.WaitForTaskWithDelay(taskID, 0*time.Second)
}

func TestSchedulingDelay_OnePreferredExecutor(t *testing.T) {
	env, ctx := getEnv(t, &schedulerOpts{preferredExecutors: []string{"2"}}, "user1")

	fe1 := newFakeExecutorWithId(ctx, t, "1", env.GetSchedulerClient())
	fe2 := newFakeExecutorWithId(ctx, t, "2", env.GetSchedulerClient())
	fe1.Register()
	fe2.Register()

	taskID := scheduleTask(ctx, t, env, map[string]string{"runner-recycling-max-wait": "5s"})

	fe1.WaitForTaskWithDelay(taskID, 5*time.Second)
	fe2.WaitForTaskWithDelay(taskID, 0*time.Second)
}

func TestSchedulingDelay_PreferredExecutorUnhealthy(t *testing.T) {
	env, ctx := getEnv(t, &schedulerOpts{preferredExecutors: []string{"2"}}, "user1")

	fe1 := newFakeExecutorWithId(ctx, t, "1", env.GetSchedulerClient())
	fe2 := newFakeExecutorWithId(ctx, t, "2", env.GetSchedulerClient())
	fe1.Register()
	fe2.Register()
	fe2.markUnhealthy()

	taskID := scheduleTask(ctx, t, env, map[string]string{"runner-recycling-max-wait": "5s"})

	fe2.EnsureTaskNotReceived(taskID)
	fe1.WaitForTaskWithDelay(taskID, 0*time.Second)
}

func TestEnqueueTaskReservation_DoesntOverwriteDelay(t *testing.T) {
	env, ctx := getEnv(t, &schedulerOpts{}, "user1")

	fe1 := newFakeExecutorWithId(ctx, t, "1", env.GetSchedulerClient())
	fe1.Register()

	taskID := enqueueTaskReservation(ctx, t, env, 3*time.Second)

	fe1.WaitForTaskWithDelay(taskID, 3*time.Second)
}

func TestEnqueueTaskReservation_Exists(t *testing.T) {
	env, ctx := getEnv(t, &schedulerOpts{}, "user1")

	fe := newFakeExecutor(ctx, t, env.GetSchedulerClient())
	fe.Register()

	taskID := scheduleTask(ctx, t, env, map[string]string{platform.RetryPropertyName: "false"})
	fe.WaitForTask(taskID)
	lease := fe.Claim(taskID)

	resp, err := env.GetSchedulerClient().TaskExists(ctx, &scpb.TaskExistsRequest{TaskId: taskID})

	require.Nil(t, err)
	require.True(t, resp.GetExists())

	lease.Finalize()

	resp, err = env.GetSchedulerClient().TaskExists(ctx, &scpb.TaskExistsRequest{TaskId: taskID})

	require.Nil(t, err)
	require.False(t, resp.GetExists())
}

func TestScheduleTask_DeletesTaskOnEnqueueFailure(t *testing.T) {
	env, ctx := getEnv(t, &schedulerOpts{}, "user1")

	// Don't register any executors, so enqueuing task reservations will fail
	// with an "No registered executors" error.
	req := newScheduleRequest(ctx, t, env, scheduleOpts{})
	_, err := env.GetSchedulerService().ScheduleTask(ctx, req)
	require.Error(t, err)

	resp, err := env.GetSchedulerClient().TaskExists(ctx, &scpb.TaskExistsRequest{TaskId: req.GetTaskId()})
	require.NoError(t, err)
	require.False(t, resp.GetExists())
}

func TestFinalizeTask_NonDurableTasksDeleteImmediately(t *testing.T) {
	for _, testCase := range []struct {
		name                string
		addTransitionalHash bool
		forceLegacyShape    bool
	}{
		{name: "ordinary_schedule_task"},
		{name: "legacy_without_fingerprint", forceLegacyShape: true},
		// Simulate a task created by the previous intermediate implementation,
		// which fingerprinted every task but did not have an explicit durable
		// marker. A fingerprint alone must never opt a task into retention.
		{name: "rolling_upgrade_fingerprint_without_marker", addTransitionalHash: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			env, ctx := getEnv(t, &schedulerOpts{}, "user1")
			s := env.GetSchedulerService().(*SchedulerServer)
			fe := newFakeExecutor(ctx, t, env.GetSchedulerClient())
			fe.Register()

			req := newScheduleRequest(ctx, t, env, scheduleOpts{})
			_, err := s.ScheduleTask(ctx, req)
			require.NoError(t, err)
			fe.WaitForTask(req.GetTaskId())

			key := s.redisKeyForTask(req.GetTaskId())
			if testCase.forceLegacyShape {
				require.NoError(t, s.rdb.HDel(
					ctx,
					key,
					redisTaskFingerprintField,
					redisTaskDurableEnsureField,
				).Err())
			}
			if testCase.addTransitionalHash {
				metadata, err := (proto.MarshalOptions{Deterministic: true}).Marshal(req.GetMetadata())
				require.NoError(t, err)
				require.NoError(t, s.rdb.HSet(
					ctx,
					key,
					redisTaskFingerprintField,
					immutableTaskFingerprint(req.GetSerializedTask(), metadata),
				).Err())
			}
			require.Empty(t, s.rdb.HGet(ctx, key, redisTaskDurableEnsureField).Val())

			lease := fe.Claim(req.GetTaskId())
			require.NoError(t, lease.Finalize())

			exists, err := s.rdb.Exists(ctx, key).Result()
			require.NoError(t, err)
			require.Zero(t, exists)
		})
	}
}

func TestEnsureTask_AdoptsIdenticalActiveTaskAsDurable(t *testing.T) {
	env, ctx := getEnv(t, &schedulerOpts{}, "user1")
	s := env.GetSchedulerService().(*SchedulerServer)
	fe := newFakeExecutor(ctx, t, env.GetSchedulerClient())
	fe.Register()

	scheduleReq := newScheduleRequest(ctx, t, env, scheduleOpts{})
	_, err := s.ScheduleTask(ctx, scheduleReq)
	require.NoError(t, err)
	fe.WaitForTask(scheduleReq.GetTaskId())

	key := s.redisKeyForTask(scheduleReq.GetTaskId())
	require.Empty(t, s.rdb.HGet(ctx, key, redisTaskFingerprintField).Val())
	require.Empty(t, s.rdb.HGet(ctx, key, redisTaskDurableEnsureField).Val())

	resp, err := s.EnsureTask(ctx, newEnsureTaskRequest(scheduleReq))
	require.NoError(t, err)
	require.False(t, resp.GetCreated())
	require.False(t, resp.GetCompleted())
	require.Equal(t, "1", s.rdb.HGet(ctx, key, redisTaskDurableEnsureField).Val())
	require.Len(t, s.rdb.HGet(ctx, key, redisTaskFingerprintField).Val(), sha256.Size)

	lease := fe.Claim(scheduleReq.GetTaskId())
	require.NoError(t, lease.Finalize())
	fields, err := s.rdb.HGetAll(ctx, key).Result()
	require.NoError(t, err)
	require.Equal(t, "1", fields[redisTaskCompletedField])
	require.Len(t, fields[redisTaskFingerprintField], sha256.Size)
	require.Len(t, fields, 2)
}

func TestEnsureTask_IdenticalUnclaimedTaskReissuesWithoutMutation(t *testing.T) {
	env, ctx := getEnv(t, &schedulerOpts{}, "user1")
	s := env.GetSchedulerService().(*SchedulerServer)
	fe := newFakeExecutor(ctx, t, env.GetSchedulerClient())
	fe.Register()

	req := newEnsureTaskRequest(newScheduleRequest(ctx, t, env, scheduleOpts{}))
	resp, err := s.EnsureTask(ctx, req)
	require.NoError(t, err)
	require.True(t, resp.GetCreated())
	fe.WaitForTask(req.GetTaskId())
	fe.ResetTasks()

	key := s.redisKeyForTask(req.GetTaskId())
	require.NoError(t, s.rdb.Expire(ctx, key, time.Hour).Err())
	before, err := s.rdb.HGetAll(ctx, key).Result()
	require.NoError(t, err)

	resp, err = s.EnsureTask(ctx, req)
	require.NoError(t, err)
	require.False(t, resp.GetCreated())
	require.False(t, resp.GetCompleted())
	fe.WaitForTask(req.GetTaskId())

	after, err := s.rdb.HGetAll(ctx, key).Result()
	require.NoError(t, err)
	require.Equal(t, before, after)
	ttl, err := s.rdb.TTL(ctx, key).Result()
	require.NoError(t, err)
	require.Positive(t, ttl)
	require.LessOrEqual(t, ttl, time.Hour)
}

func TestEnsureTask_IdenticalClaimedTaskDoesNotReissueReservation(t *testing.T) {
	env, ctx := getEnv(t, &schedulerOpts{}, "user1")
	s := env.GetSchedulerService().(*SchedulerServer)
	fe := newFakeExecutor(ctx, t, env.GetSchedulerClient())
	fe.Register()

	req := newEnsureTaskRequest(newScheduleRequest(ctx, t, env, scheduleOpts{}))
	resp, err := s.EnsureTask(ctx, req)
	require.NoError(t, err)
	require.True(t, resp.GetCreated())
	require.Equal(
		t,
		"1",
		s.rdb.HGet(ctx, s.redisKeyForTask(req.GetTaskId()), redisTaskDurableEnsureField).Val(),
	)
	fe.WaitForTask(req.GetTaskId())
	_ = fe.Claim(req.GetTaskId())
	fe.ResetTasks()

	key := s.redisKeyForTask(req.GetTaskId())
	require.NoError(t, s.rdb.Expire(ctx, key, time.Hour).Err())
	before, err := s.rdb.HGetAll(ctx, key).Result()
	require.NoError(t, err)

	resp, err = s.EnsureTask(ctx, req)
	require.NoError(t, err)
	require.False(t, resp.GetCreated())
	require.False(t, resp.GetCompleted())
	fe.EnsureTaskNotReceived(req.GetTaskId())

	after, err := s.rdb.HGetAll(ctx, key).Result()
	require.NoError(t, err)
	require.Equal(t, before, after)
	ttl, err := s.rdb.TTL(ctx, key).Result()
	require.NoError(t, err)
	require.Positive(t, ttl)
	require.LessOrEqual(t, ttl, time.Hour)
}

func TestEnsureTask_RejectsMismatchedImmutablePayload(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*testing.T, *scpb.EnsureTaskRequest)
	}{
		{
			name: "task",
			mutate: func(t *testing.T, req *scpb.EnsureTaskRequest) {
				task := &repb.ExecutionTask{}
				require.NoError(t, proto.Unmarshal(req.GetSerializedTask(), task))
				task.ExecutionId += "-different"
				serializedTask, err := proto.Marshal(task)
				require.NoError(t, err)
				req.SerializedTask = serializedTask
			},
		},
		{
			name: "metadata",
			mutate: func(t *testing.T, req *scpb.EnsureTaskRequest) {
				req.Metadata.Priority++
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			env, ctx := getEnv(t, &schedulerOpts{}, "user1")
			s := env.GetSchedulerService().(*SchedulerServer)
			fe := newFakeExecutor(ctx, t, env.GetSchedulerClient())
			fe.Register()

			req := newEnsureTaskRequest(newScheduleRequest(ctx, t, env, scheduleOpts{}))
			resp, err := s.EnsureTask(ctx, req)
			require.NoError(t, err)
			require.True(t, resp.GetCreated())
			fe.WaitForTask(req.GetTaskId())

			key := s.redisKeyForTask(req.GetTaskId())
			before, err := s.rdb.HGetAll(ctx, key).Result()
			require.NoError(t, err)

			mismatched := proto.Clone(req).(*scpb.EnsureTaskRequest)
			testCase.mutate(t, mismatched)
			_, err = s.EnsureTask(ctx, mismatched)
			require.True(t, status.IsAlreadyExistsError(err), "error: %s", err)

			after, err := s.rdb.HGetAll(ctx, key).Result()
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestEnsureTask_AfterFinalizeReturnsCompletedWithoutReservation(t *testing.T) {
	env, ctx := getEnv(t, &schedulerOpts{}, "user1")
	s := env.GetSchedulerService().(*SchedulerServer)
	fe := newFakeExecutor(ctx, t, env.GetSchedulerClient())
	fe.Register()

	req := newEnsureTaskRequest(newScheduleRequest(ctx, t, env, scheduleOpts{}))
	resp, err := s.EnsureTask(ctx, req)
	require.NoError(t, err)
	require.True(t, resp.GetCreated())
	fe.WaitForTask(req.GetTaskId())
	lease := fe.Claim(req.GetTaskId())
	require.NoError(t, lease.Finalize())

	// Finalization atomically leaves only a compact, bounded tombstone.
	key := s.redisKeyForTask(req.GetTaskId())
	fields, err := s.rdb.HGetAll(ctx, key).Result()
	require.NoError(t, err)
	require.Equal(t, "1", fields[redisTaskCompletedField])
	require.Len(t, fields[redisTaskFingerprintField], sha256.Size)
	require.Len(t, fields, 2)
	ttl, err := s.rdb.TTL(ctx, key).Result()
	require.NoError(t, err)
	require.Positive(t, ttl)
	require.LessOrEqual(t, ttl, taskCompletionTTL)

	exists, err := s.ExistsTask(ctx, req.GetTaskId())
	require.NoError(t, err)
	require.False(t, exists)

	// This is the critical recovery interleaving: the coordinator observed
	// completion, the executor finalized, and only then recovery calls Ensure.
	const shortenedTTL = 5 * time.Minute
	require.NoError(t, s.rdb.Expire(ctx, key, shortenedTTL).Err())
	fe.ResetTasks()
	resp, err = s.EnsureTask(ctx, req)
	require.NoError(t, err)
	require.False(t, resp.GetCreated())
	require.True(t, resp.GetCompleted())
	fe.EnsureTaskNotReceived(req.GetTaskId())
	ttl, err = s.rdb.TTL(ctx, key).Result()
	require.NoError(t, err)
	require.Positive(t, ttl)
	require.LessOrEqual(t, ttl, shortenedTTL)

	// A completed task ID stays bound to its immutable fingerprint.
	mismatched := proto.Clone(req).(*scpb.EnsureTaskRequest)
	mismatched.Metadata.Priority++
	_, err = s.EnsureTask(ctx, mismatched)
	require.True(t, status.IsAlreadyExistsError(err), "error: %s", err)

	// Neither legacy scheduling nor cancellation may overwrite/remove a live
	// completion tombstone.
	_, err = s.ScheduleTask(ctx, &scpb.ScheduleTaskRequest{
		TaskId:         req.GetTaskId(),
		Metadata:       req.GetMetadata(),
		SerializedTask: req.GetSerializedTask(),
	})
	require.True(t, status.IsAlreadyExistsError(err), "error: %s", err)
	deleted, err := s.CancelTask(ctx, req.GetTaskId())
	require.NoError(t, err)
	require.False(t, deleted)

	_, err = s.claimTask(ctx, req.GetTaskId(), "", true)
	require.True(t, status.IsNotFoundError(err), "error: %s", err)
}

func TestEnsureTask_ConcurrentWithFinalizeCannotRecreateTask(t *testing.T) {
	env, ctx := getEnv(t, &schedulerOpts{}, "user1")
	s := env.GetSchedulerService().(*SchedulerServer)
	fe := newFakeExecutor(ctx, t, env.GetSchedulerClient())
	fe.Register()

	req := newEnsureTaskRequest(newScheduleRequest(ctx, t, env, scheduleOpts{}))
	_, err := s.EnsureTask(ctx, req)
	require.NoError(t, err)
	fe.WaitForTask(req.GetTaskId())
	lease := fe.Claim(req.GetTaskId())

	start := make(chan struct{})
	ensureResult := make(chan error, 1)
	finalizeResult := make(chan error, 1)
	go func() {
		<-start
		_, err := s.EnsureTask(ctx, req)
		ensureResult <- err
	}()
	go func() {
		<-start
		finalizeResult <- lease.Finalize()
	}()
	close(start)

	require.NoError(t, <-ensureResult)
	require.NoError(t, <-finalizeResult)

	// Regardless of which Lua operation won the race, finalization is terminal.
	resp, err := s.EnsureTask(ctx, req)
	require.NoError(t, err)
	require.True(t, resp.GetCompleted())
	_, err = s.claimTask(ctx, req.GetTaskId(), "", true)
	require.True(t, status.IsNotFoundError(err), "error: %s", err)

	fields, err := s.rdb.HGetAll(ctx, s.redisKeyForTask(req.GetTaskId())).Result()
	require.NoError(t, err)
	require.Equal(t, "1", fields[redisTaskCompletedField])
	require.Len(t, fields[redisTaskFingerprintField], sha256.Size)
	require.Len(t, fields, 2)
}

func TestEnsureTask_ConcurrentIdenticalCallsCreateOnce(t *testing.T) {
	s, ctx := getScheduleServer(t, false, false, "user1")
	req := newScheduleRequest(ctx, t, s.env, scheduleOpts{})

	const concurrency = 32
	start := make(chan struct{})
	errs := make(chan error, concurrency)
	var createdCount atomic.Int32
	var wg sync.WaitGroup
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			state, err := s.ensureTask(ctx, req.GetTaskId(), req.GetMetadata(), req.GetSerializedTask())
			if state == ensureTaskCreated {
				createdCount.Add(1)
			}
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int32(1), createdCount.Load())
	task, err := s.readTask(ctx, req.GetTaskId())
	require.NoError(t, err)
	require.Equal(t, req.GetSerializedTask(), task.serializedTask)
	require.Equal(t, int64(0), task.attemptCount)
}

func TestEnqueueTaskReservation_RoutingConfig(t *testing.T) {
	for _, tc := range []struct {
		name          string
		routingConfig *scpb.RoutingConfig

		expectRoutedToHosts   []string
		expectSchedulingError bool
	}{
		{
			name: "RequiredRoutingConfig_MatchesExecutorSubset_ShouldRouteOnlyToSubset",
			routingConfig: &scpb.RoutingConfig{
				HostnamePattern: "ex1",
				BestEffort:      false,
			},
			expectRoutedToHosts: []string{"ex1"},
		},
		{
			name: "RequiredRoutingConfig_PatternMatchesNoExecutors_ShouldFail",
			routingConfig: &scpb.RoutingConfig{
				HostnamePattern: "nonexistent-executor-pattern",
				BestEffort:      false,
			},
			expectSchedulingError: true,
		},
		{
			name: "BestEffortRoutingConfig_MatchesExecutorSubset_ShouldRouteOnlyToSubset",
			routingConfig: &scpb.RoutingConfig{
				HostnamePattern: "ex1",
				BestEffort:      true,
			},
			expectRoutedToHosts: []string{"ex1"},
		},
		{
			name: "BestEffortRoutingConfig_PatternMatchesNoExecutors_ShouldRouteToAllExecutors",
			routingConfig: &scpb.RoutingConfig{
				HostnamePattern: "nonexistent-executor-pattern",
				BestEffort:      true,
			},
			expectRoutedToHosts: []string{"ex1", "ex2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, ctx := getEnv(t, &schedulerOpts{}, "user1")

			ex1 := newFakeExecutor(ctx, t, env.GetSchedulerClient())
			ex1.node.Host = "ex1"
			ex1.Register()
			ex2 := newFakeExecutor(ctx, t, env.GetSchedulerClient())
			ex2.node.Host = "ex2"
			ex2.Register()

			req := newScheduleRequest(ctx, t, env, scheduleOpts{})
			req.Metadata.RoutingConfig = tc.routingConfig
			_, err := env.GetSchedulerService().ScheduleTask(ctx, req)
			if tc.expectSchedulingError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			for _, ex := range []*fakeExecutor{ex1, ex2} {
				if slices.Contains(tc.expectRoutedToHosts, ex.node.GetHost()) {
					msg := ex.NextSchedulerMessage()
					require.Equal(t, req.GetTaskId(), msg.GetEnqueueTaskReservationRequest().GetTaskId())
				} else {
					ex.EnsureNoSchedulerMessage()
				}
			}
		})
	}
}

func TestAskForMoreWork_OnlyEnqueuesTasksThatFitOnNode(t *testing.T) {
	// Disable unclaimedTasks cache to simulate the TTL expiring immediately
	// for the purposes of this test.
	flags.Set(t, "remote_execution.unclaimed_tasks_cache_ttl", 0*time.Second)

	clock := clockwork.NewFakeClock()
	env, ctx := getEnv(t, &schedulerOpts{options: Options{Clock: clock}}, "user1")

	// Register two nodes with different capacities.
	largeExecutor := newFakeExecutorWithId(ctx, t, "large", env.GetSchedulerClient())
	largeExecutor.node.AssignableMilliCpu = 32_000
	largeExecutor.Register()

	smallExecutor := newFakeExecutorWithId(ctx, t, "small", env.GetSchedulerClient())
	smallExecutor.node.AssignableMilliCpu = 1000
	smallExecutor.Register()

	var rsp *scpb.RegisterAndStreamWorkResponse

	// Schedule a task that only fits on largeExecutor.
	taskID := scheduleTask(ctx, t, env, map[string]string{"EstimatedCPU": "8000m"})
	// Ensure the task was enqueued on largeExecutor, but don't have
	// largeExecutor claim the task, so that it's eligible to be enqueued
	// as part of AskForMoreWork.
	rsp = <-largeExecutor.schedulerMessages
	require.Equal(t, taskID, rsp.GetEnqueueTaskReservationRequest().GetTaskId())

	// Now have largeExecutor ask for more work. The scheduler should enqueue
	// the unclaimed task.
	largeExecutor.Send(&scpb.RegisterAndStreamWorkRequest{
		AskForMoreWorkRequest: &scpb.AskForMoreWorkRequest{},
	})
	// Make sure we get an EnqueueTaskReservationRequest. The scheduler doesn't
	// know what tasks are currently enqueued on largeExecutor, so it's fair
	// to expect the task to be enqueued again.
	// Note: we don't expect an AskForMoreWorkResponse here - the scheduler only
	// sends a response to increase the client backoff after enqueuing 0 tasks.
	rsp = <-largeExecutor.schedulerMessages
	require.Equal(t, taskID, rsp.GetEnqueueTaskReservationRequest().GetTaskId())

	// Have smallExecutor ask for more work now. The scheduler should not
	// schedule any work on smallExecutor because the task doesn't fit. It
	// should only reply with an AskForMoreWorkResponse to increase the client
	// backoff.
	smallExecutor.Send(&scpb.RegisterAndStreamWorkRequest{
		AskForMoreWorkRequest: &scpb.AskForMoreWorkRequest{},
	})
	rsp = <-smallExecutor.schedulerMessages
	require.Greater(t, rsp.GetAskForMoreWorkResponse().GetDelay().AsDuration(), time.Duration(0))
}

func TestAskForMoreWork_RespectRequestedExecutorID(t *testing.T) {
	clock := clockwork.NewFakeClock()
	env, ctx := getEnv(t, &schedulerOpts{options: Options{Clock: clock}}, "user1")

	// Register two nodes.
	executor1 := newFakeExecutorWithId(ctx, t, "n1", env.GetSchedulerClient())
	executor1.node.AssignableMilliCpu = 1000
	executor1.Register()

	executor2 := newFakeExecutorWithId(ctx, t, "n2", env.GetSchedulerClient())
	executor2.node.AssignableMilliCpu = 1000
	executor2.Register()

	var rsp *scpb.RegisterAndStreamWorkResponse

	// Schedule a task on executor1.
	taskID := scheduleTask(ctx, t, env, map[string]string{"debug-executor-id": "n1"})
	// Ensure the task was enqueued on executor1, but don't have
	// executor1 claim the task, so that it's eligible to be enqueued
	// as part of AskForMoreWork.
	rsp = <-executor1.schedulerMessages
	require.Equal(t, taskID, rsp.GetEnqueueTaskReservationRequest().GetTaskId())

	// Have executor2 ask for more work now. The scheduler should not
	// schedule any work on executor2 because the task specifically requested
	// executor1. It should only reply with an AskForMoreWorkResponse to increase
	// the client backoff.
	executor2.Send(&scpb.RegisterAndStreamWorkRequest{
		AskForMoreWorkRequest: &scpb.AskForMoreWorkRequest{},
	})
	rsp = <-executor2.schedulerMessages
	require.Greater(t, rsp.GetAskForMoreWorkResponse().GetDelay().AsDuration(), time.Duration(0))
}

func TestAskForMoreWork_CachesResultsToReduceRedisLoad(t *testing.T) {
	for _, testCase := range []struct {
		name                string
		cacheTTL            time.Duration
		expectedZrangeCount int64
	}{
		{
			name:                "should dedupe ZRANGE calls within cache TTL",
			cacheTTL:            time.Duration(1 * time.Minute),
			expectedZrangeCount: 1,
		},
		{
			name:     "should issue individual ZRANGE requests if cache TTL is disabled",
			cacheTTL: 0,
			// 1 request on registration + 1 for each AskForMoreWorkRequest
			expectedZrangeCount: 11,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			flags.Set(t, "remote_execution.unclaimed_tasks_cache_ttl", testCase.cacheTTL)

			clock := clockwork.NewFakeClock()
			env, ctx := getEnv(t, &schedulerOpts{options: Options{Clock: clock}}, "user1")
			zrangeCounter := testredis.NewCommandCounter("ZRANGE")
			env.GetRemoteExecutionRedisClient().AddHook(zrangeCounter)

			executor := newFakeExecutorWithId(ctx, t, "large", env.GetSchedulerClient())
			executor.Register()

			// Register an executor and have it ask for more work several times.
			for range 10 {
				executor.Send(&scpb.RegisterAndStreamWorkRequest{
					AskForMoreWorkRequest: &scpb.AskForMoreWorkRequest{},
				})
				<-executor.schedulerMessages
			}

			require.Equal(t, testCase.expectedZrangeCount, zrangeCounter.Count())

			// After advancing past cache TTL and issuing one more
			// AskForMoreWorkRequest, we should expect to see more ZRANGE calls.
			clock.Advance(testCase.cacheTTL + 1*time.Nanosecond)

			executor.Send(&scpb.RegisterAndStreamWorkRequest{
				AskForMoreWorkRequest: &scpb.AskForMoreWorkRequest{},
			})
			<-executor.schedulerMessages
			require.Equal(t, testCase.expectedZrangeCount+1, zrangeCounter.Count())
		})
	}

}

// registerLabeledExecutorPair registers two executors whose labels differ.
// executor1 has foo=1,bar=2; executor2 has foo=a,bar=2. Used by tests that
// want a "matching" executor and a "non-matching" executor for tasks that
// request labels foo=1,bar=2.
func registerLabeledExecutorPair(ctx context.Context, t *testing.T, env environment.Env) (executor1, executor2 *fakeExecutor) {
	executor1 = newFakeExecutorWithId(ctx, t, "n1", env.GetSchedulerClient())
	executor1.node.AssignableMilliCpu = 1000
	executor1.node.Labels = map[string]string{"foo": "1", "bar": "2"}
	executor1.Register()

	executor2 = newFakeExecutorWithId(ctx, t, "n2", env.GetSchedulerClient())
	executor2.node.AssignableMilliCpu = 1000
	executor2.node.Labels = map[string]string{"foo": "a", "bar": "2"}
	executor2.Register()
	return executor1, executor2
}

// assertLabelsHonored asserts that a task with debug-executor-labels matching
// only executor1 was enqueued on executor1 and that executor2's AskForMoreWork
// gets only a backoff response (no task), proving the labels filtered.
func assertLabelsHonored(t *testing.T, taskID string, executor1, executor2 *fakeExecutor) {
	rsp := <-executor1.schedulerMessages
	require.Equal(t, taskID, rsp.GetEnqueueTaskReservationRequest().GetTaskId())

	executor2.Send(&scpb.RegisterAndStreamWorkRequest{
		AskForMoreWorkRequest: &scpb.AskForMoreWorkRequest{},
	})
	rsp = <-executor2.schedulerMessages
	require.Greater(t, rsp.GetAskForMoreWorkResponse().GetDelay().AsDuration(), time.Duration(0))
}

// assertLabelsIgnored asserts that a task with debug-executor-labels was
// scheduled without filtering, so both executors received an enqueue
// reservation (probesPerTask=3 > 2 executors).
func assertLabelsIgnored(taskID string, executor1, executor2 *fakeExecutor) {
	executor1.WaitForTask(taskID)
	executor2.WaitForTask(taskID)
}

// When the debug_executor_labels_key flag is unset, anyone can use
// debug-executor-labels without supplying a key.
func TestAskForMoreWork_RespectDebugExecutorLabels(t *testing.T) {
	clock := clockwork.NewFakeClock()
	env, ctx := getEnv(t, &schedulerOpts{options: Options{Clock: clock}}, "user1")

	executor1, executor2 := registerLabeledExecutorPair(ctx, t, env)

	taskID := scheduleTask(ctx, t, env, map[string]string{"debug-executor-labels": "foo=1,bar=2"})
	assertLabelsHonored(t, taskID, executor1, executor2)
}

// When the flag is set and the request supplies a matching key, labels are
// honored.
func TestAskForMoreWork_DebugExecutorLabels_ValidKey(t *testing.T) {
	clock := clockwork.NewFakeClock()
	env, ctx := getEnv(t, &schedulerOpts{options: Options{Clock: clock}}, "user1")
	flags.Set(t, "remote_execution.debug_executor_labels_key", "shh")

	executor1, executor2 := registerLabeledExecutorPair(ctx, t, env)

	taskID := scheduleTask(ctx, t, env, map[string]string{
		"debug-executor-labels":     "foo=1,bar=2",
		"debug-executor-labels-key": "shh",
	})
	assertLabelsHonored(t, taskID, executor1, executor2)
}

// When the flag is set and the request supplies the wrong key, the labels are
// ignored entirely.
func TestAskForMoreWork_DebugExecutorLabels_WrongKey(t *testing.T) {
	clock := clockwork.NewFakeClock()
	env, ctx := getEnv(t, &schedulerOpts{options: Options{Clock: clock}}, "user1")
	flags.Set(t, "remote_execution.debug_executor_labels_key", "shh")

	executor1, executor2 := registerLabeledExecutorPair(ctx, t, env)

	taskID := scheduleTask(ctx, t, env, map[string]string{
		"debug-executor-labels":     "foo=1,bar=2",
		"debug-executor-labels-key": "wrong",
	})
	assertLabelsIgnored(taskID, executor1, executor2)
}

// When the flag is set and the request omits the key, the labels are ignored
// entirely.
func TestAskForMoreWork_DebugExecutorLabels_MissingKey(t *testing.T) {
	clock := clockwork.NewFakeClock()
	env, ctx := getEnv(t, &schedulerOpts{options: Options{Clock: clock}}, "user1")
	flags.Set(t, "remote_execution.debug_executor_labels_key", "shh")

	executor1, executor2 := registerLabeledExecutorPair(ctx, t, env)

	taskID := scheduleTask(ctx, t, env, map[string]string{"debug-executor-labels": "foo=1,bar=2"})
	assertLabelsIgnored(taskID, executor1, executor2)
}

func TestGetExecutionNodes(t *testing.T) {
	clock := clockwork.NewFakeClock()
	env, ctx := getEnv(t, &schedulerOpts{options: Options{Clock: clock}}, "user1")
	enterprise_testauth.Configure(t, env)

	// Register several nodes with different IDs
	for _, hostID := range []string{"z", "m", "a"} {
		executor := newFakeExecutorWithId(ctx, t, hostID, env.GetSchedulerClient())
		executor.Register()
	}

	u := enterprise_testauth.CreateRandomUser(t, env, "org1.invalid")
	g := u.Groups[0].Group
	groupID := g.GroupID

	auther := env.GetAuthenticator().(*testauth.TestAuthenticator)
	authCtx, err := auther.WithAuthenticatedUser(ctx, u.UserID)
	require.NoError(t, err)

	rsp, err := env.GetSchedulerService().GetExecutionNodes(authCtx, &scpb.GetExecutionNodesRequest{
		RequestContext: &ctxpb.RequestContext{
			GroupId: groupID,
		},
	})
	require.NoError(t, err)

	// Ensure that executors are sorted by host id.
	for i, executor := range rsp.GetExecutor() {
		if i > 0 {
			last := rsp.GetExecutor()[i-1]
			require.GreaterOrEqual(t, executor.GetNode().GetHost(), last.GetNode().GetHost())
		}
	}
}

// testUpgradeDetector prompts an upgrade when an executor is more than 10
// minor versions behind the newest registered version, escalating at 20.
func testUpgradeDetector() *upgrade.Detector {
	return upgrade.NewDetector(map[uppb.Prompt_Urgency]upgrade.Trigger{
		uppb.Prompt_LOW:    {MaxLag: semver.MustParse("0.10.0")},
		uppb.Prompt_MEDIUM: {MaxLag: semver.MustParse("0.20.0")},
	})
}

func TestGetExecutionNodes_UpgradePrompt(t *testing.T) {
	clock := clockwork.NewFakeClock()
	env, ctx := getEnv(t, &schedulerOpts{options: Options{Clock: clock, UpgradeDetector: testUpgradeDetector()}}, "user1")
	enterprise_testauth.Configure(t, env)

	for id, version := range map[string]string{"a": "v2.153.0", "b": "v2.140.0"} {
		executor := newFakeExecutorWithId(ctx, t, id, env.GetSchedulerClient())
		executor.node.Version = version
		executor.Register()
	}

	u := enterprise_testauth.CreateRandomUser(t, env, "org1.invalid")
	auther := env.GetAuthenticator().(*testauth.TestAuthenticator)
	authCtx, err := auther.WithAuthenticatedUser(ctx, u.UserID)
	require.NoError(t, err)

	rsp, err := env.GetSchedulerService().GetExecutionNodes(authCtx, &scpb.GetExecutionNodesRequest{
		RequestContext: &ctxpb.RequestContext{
			GroupId: u.Groups[0].Group.GroupID,
		},
	})
	require.NoError(t, err)
	require.Len(t, rsp.GetExecutor(), 2)
	require.NotNil(t, rsp.GetUpgradePrompt())
	require.Equal(t, uppb.Prompt_LOW, rsp.GetUpgradePrompt().GetUrgency())
}

func TestGetExecutionNodes_UpgradePrompt_WithinAllowance(t *testing.T) {
	clock := clockwork.NewFakeClock()
	env, ctx := getEnv(t, &schedulerOpts{options: Options{Clock: clock, UpgradeDetector: testUpgradeDetector()}}, "user1")
	enterprise_testauth.Configure(t, env)

	for id, version := range map[string]string{"a": "v2.153.0", "b": "v2.152.0"} {
		executor := newFakeExecutorWithId(ctx, t, id, env.GetSchedulerClient())
		executor.node.Version = version
		executor.Register()
	}

	u := enterprise_testauth.CreateRandomUser(t, env, "org1.invalid")
	auther := env.GetAuthenticator().(*testauth.TestAuthenticator)
	authCtx, err := auther.WithAuthenticatedUser(ctx, u.UserID)
	require.NoError(t, err)

	rsp, err := env.GetSchedulerService().GetExecutionNodes(authCtx, &scpb.GetExecutionNodesRequest{
		RequestContext: &ctxpb.RequestContext{
			GroupId: u.Groups[0].Group.GroupID,
		},
	})
	require.NoError(t, err)
	require.Len(t, rsp.GetExecutor(), 2)
	require.Nil(t, rsp.GetUpgradePrompt())
}

// With user-owned executors enabled, only registrations in the shared
// executor pool group ("sharedGroupID", set by getEnv) set the
// newest-version bar.
func TestGetNewestVersion_ScopedToSharedPoolGroup(t *testing.T) {
	clock := clockwork.NewFakeClock()
	env, ctx := getEnv(t, &schedulerOpts{options: Options{Clock: clock, UpgradeDetector: testUpgradeDetector()}, userOwnedEnabled: true}, "user1")
	s := env.GetSchedulerService().(*SchedulerServer)

	register := func(groupID, version string) {
		reg := &scpb.RegisteredExecutionNode{
			Registration: &scpb.ExecutionNode{ExecutorId: "id-" + groupID, Version: version},
			GroupId:      groupID,
			LastPingTime: timestamppb.Now(),
		}
		b, err := proto.Marshal(reg)
		require.NoError(t, err)
		poolKey := "executorPool/" + groupID + "-linux-amd64-p"
		require.NoError(t, s.rdb.HSet(ctx, poolKey, reg.GetRegistration().GetExecutorId(), b).Err())
		require.NoError(t, s.rdb.SAdd(ctx, "executorPools/"+groupID, poolKey).Err())
	}
	// A newer version outside the shared pool group shouldn't set the bar.
	register("GR-OTHER", "v2.199.0")
	register("sharedGroupID", "v2.153.0")

	v := s.getNewestVersion(ctx)
	require.NotNil(t, v)
	require.Equal(t, "2.153.0", v.String())
}
