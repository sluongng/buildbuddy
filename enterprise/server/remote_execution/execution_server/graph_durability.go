package execution_server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	graphpb "github.com/buildbuddy-io/buildbuddy/proto/graph_execution"
	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
	scpb "github.com/buildbuddy-io/buildbuddy/proto/scheduler"
	"github.com/buildbuddy-io/buildbuddy/server/metrics"
	"github.com/buildbuddy-io/buildbuddy/server/remote_cache/digest"
	"github.com/buildbuddy-io/buildbuddy/server/util/proto"
	"github.com/buildbuddy-io/buildbuddy/server/util/status"
)

const (
	graphDurableActiveLifetime       = 30 * time.Minute
	graphDurableLeaseLifetime        = 2 * time.Second
	graphDurableLeaseRefresh         = 500 * time.Millisecond
	graphDurableAdmissionRefresh     = 10 * time.Minute
	graphDurableMaxPreparedTaskBytes = 64 << 20
	graphDurableMaxGeneratedBytes    = 48 << 20

	// Admission reserves worst-case Redis memory before accepting a session.
	// Generated execution fields can duplicate node IDs from the request
	// journal, so reserve a second request-sized allowance plus fixed hash-field
	// overhead. This is intentionally conservative: cross-slot failures may
	// overcount until TTL expiry, but never undercount live durable state.
	graphDurableActiveReservationBytes = 2*graphExecutionMaxRequestReplayBytes +
		graphExecutionMaxResponseReplayBytes +
		graphDurableMaxPreparedTaskBytes +
		16<<20
	graphDurableMaxRecoveryBytes         = graphDurableActiveReservationBytes + 4<<20
	graphDurableRetainedReservationBytes = graphExecutionMaxResponseReplayBytes + 1<<20
	graphDurableMaxActiveBytesPerTenant  = 1 << 30
	graphDurableMaxActiveBytesGlobal     = 64 << 30
	// Terminal replay capacity is reserved at Begin, not after terminal
	// commit. These budgets accommodate every active tenant session plus more
	// than ten sequential retained graphs while remaining operationally
	// bounded.
	graphDurableMaxRetainedBytesPerTenant = 2 << 30
	graphDurableMaxRetainedBytesGlobal    = 32 << 30
)

const graphReserveAdmissionScript = `
local redis_time = redis.call("TIME")
local now_millis = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
local expires_at = now_millis + tonumber(ARGV[2])
redis.call("ZREMRANGEBYSCORE", KEYS[1], "-inf", now_millis)
redis.call("ZREMRANGEBYSCORE", KEYS[2], "-inf", now_millis)
if redis.call("ZSCORE", KEYS[1], ARGV[1]) then
  redis.call("ZADD", KEYS[1], expires_at, ARGV[1])
  redis.call("ZADD", KEYS[2], expires_at, ARGV[1])
  return 0
end
if redis.call("ZCARD", KEYS[1]) >= tonumber(ARGV[3]) then
  return -4
end
if redis.call("ZCARD", KEYS[2]) >= tonumber(ARGV[4]) then
  return -3
end
if (redis.call("ZCARD", KEYS[1]) + 1) * tonumber(ARGV[5]) > tonumber(ARGV[6]) then
  return -4
end
if (redis.call("ZCARD", KEYS[2]) + 1) * tonumber(ARGV[5]) > tonumber(ARGV[7]) then
  return -3
end
redis.call("ZADD", KEYS[1], expires_at, ARGV[1])
redis.call("ZADD", KEYS[2], expires_at, ARGV[1])
return 1
`

const graphReleaseAdmissionScript = `
redis.call("ZREM", KEYS[1], ARGV[1])
redis.call("ZREM", KEYS[2], ARGV[1])
return 1
`

const graphRefreshGuaranteedAdmissionScript = `
local redis_time = redis.call("TIME")
local now_millis = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
local expires_at = now_millis + tonumber(ARGV[2])
redis.call("ZREMRANGEBYSCORE", KEYS[1], "-inf", now_millis)
redis.call("ZREMRANGEBYSCORE", KEYS[2], "-inf", now_millis)
if not redis.call("ZSCORE", KEYS[1], ARGV[1]) or
   not redis.call("ZSCORE", KEYS[2], ARGV[1]) then
  return 0
end
redis.call("ZADD", KEYS[1], expires_at, ARGV[1])
redis.call("ZADD", KEYS[2], expires_at, ARGV[1])
return 1
`

const graphCreateSessionScript = `
local key = KEYS[1]
if redis.call("EXISTS", key) == 0 then
  redis.call("HSET", key,
    "owner", ARGV[1],
    "session", ARGV[2],
    "token", ARGV[3],
    "last_request", "1",
    "last_response", "0",
    "request_bytes", string.len(ARGV[5]),
    "response_bytes", "0",
    "prepared_task_bytes", "0",
    "generated_bytes", "0",
    "state", "active",
    "request:1", ARGV[5],
    "begin_request", ARGV[5])
  redis.call("PEXPIRE", key, ARGV[4])
  return 1
end
if redis.call("HGET", key, "owner") ~= ARGV[1] or
   redis.call("HGET", key, "session") ~= ARGV[2] then
  return -1
end
if redis.call("HGET", key, "token") ~= ARGV[3] then
  return -2
end
if redis.call("HGET", key, "begin_request") ~= ARGV[5] then
  return -3
end
if redis.call("HGET", key, "state") == "active" then
  redis.call("PEXPIRE", key, ARGV[4])
end
return 0
`

const graphAcquireLeaseScript = `
local state_key = KEYS[1]
local lease_key = KEYS[2]
if redis.call("EXISTS", state_key) == 0 then
  return {-1, 0}
end
if redis.call("HGET", state_key, "state") == "finished" then
  return {-2, 0}
end
local current = redis.call("GET", lease_key)
if current then
  return {0, redis.call("PTTL", lease_key)}
end
local epoch = redis.call("HINCRBY", state_key, "lease_epoch", 1)
local value = tostring(epoch) .. ":" .. ARGV[1]
redis.call("SET", lease_key, value, "PX", ARGV[2])
redis.call("PEXPIRE", state_key, ARGV[3])
return {epoch, value}
`

const graphRenewLeaseScript = `
if redis.call("GET", KEYS[2]) ~= ARGV[1] then
  return 0
end
if redis.call("HGET", KEYS[1], "state") ~= "active" or
   redis.call("HEXISTS", KEYS[1], "terminal_response") == 1 then
  return -1
end
redis.call("PEXPIRE", KEYS[2], ARGV[2])
redis.call("PEXPIRE", KEYS[1], ARGV[3])
return 1
`

// Recovery is allowed to replay a terminal response that was journaled before
// the previous coordinator crashed while the session state is still active.
const graphRenewRecoveryLeaseScript = `
if redis.call("GET", KEYS[2]) ~= ARGV[1] then
  return 0
end
if redis.call("HGET", KEYS[1], "state") ~= "active" then
  return -1
end
redis.call("PEXPIRE", KEYS[2], ARGV[2])
redis.call("PEXPIRE", KEYS[1], ARGV[3])
return 1
`

const graphReleaseLeaseScript = `
if redis.call("GET", KEYS[1]) ~= ARGV[1] then
  return 0
end
return redis.call("DEL", KEYS[1])
`

const graphFinishSessionScript = `
if redis.call("GET", KEYS[2]) ~= ARGV[1] then
  return 0
end
if redis.call("HGET", KEYS[1], "state") ~= "active" then
  return -1
end
if redis.call("HEXISTS", KEYS[1], "terminal_response") == 0 then
  return -2
end
redis.call("HSET", KEYS[1], "state", "finished")
redis.call("HSET", KEYS[1], "request_bytes", "0", "prepared_task_bytes", "0")
redis.call("PEXPIRE", KEYS[1], ARGV[2])
redis.call("DEL", KEYS[2])
return 1
`

const graphPurgeTerminalFieldsScript = `
if redis.call("GET", KEYS[2]) ~= ARGV[1] then
  return 0
end
if redis.call("HGET", KEYS[1], "state") ~= "active" or
   redis.call("HEXISTS", KEYS[1], "terminal_response") == 0 then
  return -1
end
if #ARGV > 3 then
  redis.call("HDEL", KEYS[1], unpack(ARGV, 4))
end
redis.call("PEXPIRE", KEYS[2], ARGV[2])
redis.call("PEXPIRE", KEYS[1], ARGV[3])
return 1
`

const graphRenewTerminalLeaseScript = `
if redis.call("GET", KEYS[2]) ~= ARGV[1] then
  return 0
end
if redis.call("HGET", KEYS[1], "state") ~= "active" or
   redis.call("HEXISTS", KEYS[1], "terminal_response") == 0 then
  return -1
end
redis.call("PEXPIRE", KEYS[2], ARGV[2])
redis.call("PEXPIRE", KEYS[1], ARGV[3])
return 1
`

const graphAppendRequestScript = `
if redis.call("GET", KEYS[2]) ~= ARGV[1] then
  return {-3, 0}
end
if redis.call("HGET", KEYS[1], "state") ~= "active" or
   redis.call("HEXISTS", KEYS[1], "terminal_response") == 1 then
  return {-5, 0}
end
local last = tonumber(redis.call("HGET", KEYS[1], "last_request") or "0")
local seq = tonumber(ARGV[2])
local field = "request:" .. ARGV[2]
if seq <= last then
  local old = redis.call("HGET", KEYS[1], field)
  if old == ARGV[3] then
    redis.call("PEXPIRE", KEYS[1], ARGV[5])
    return {0, last}
  end
  return {-1, last}
end
if seq ~= last + 1 then
  return {-2, last}
end
local total = tonumber(redis.call("HGET", KEYS[1], "request_bytes") or "0") + string.len(ARGV[3])
if total > tonumber(ARGV[4]) then
  return {-4, last}
end
redis.call("HSET", KEYS[1], field, ARGV[3], "last_request", ARGV[2], "request_bytes", total)
redis.call("PEXPIRE", KEYS[1], ARGV[5])
return {1, seq}
`

const graphAppendResponseScript = `
if redis.call("GET", KEYS[2]) ~= ARGV[1] then
  return {-3, 0}
end
if redis.call("HGET", KEYS[1], "state") ~= "active" or
   redis.call("HEXISTS", KEYS[1], "terminal_response") == 1 then
  return {-5, 0}
end
local last = tonumber(redis.call("HGET", KEYS[1], "last_response") or "0")
local seq = tonumber(ARGV[2])
if seq ~= last + 1 then
  return {-2, last}
end
local total = tonumber(redis.call("HGET", KEYS[1], "response_bytes") or "0") + string.len(ARGV[3])
if total > tonumber(ARGV[4]) then
  return {-4, last}
end
redis.call("HSET", KEYS[1], "response:" .. ARGV[2], ARGV[3], "last_response", ARGV[2], "response_bytes", total)
if ARGV[6] == "1" then
  redis.call("HSET", KEYS[1], "terminal_response", ARGV[2])
end
redis.call("PEXPIRE", KEYS[1], ARGV[5])
return {1, seq}
`

const graphClaimExecutionScript = `
if redis.call("GET", KEYS[2]) ~= ARGV[1] then
  return {-3, ""}
end
if redis.call("HGET", KEYS[1], "state") ~= "active" or
   redis.call("HEXISTS", KEYS[1], "terminal_response") == 1 then
  return {-5, ""}
end
local id_field = "execution_id:" .. ARGV[2]
local digest_field = "action_digest:" .. ARGV[2]
local old_id = redis.call("HGET", KEYS[1], id_field)
if old_id then
  if redis.call("HGET", KEYS[1], digest_field) ~= ARGV[4] then
    return {-1, old_id}
  end
  return {0, old_id}
end
local total = tonumber(redis.call("HGET", KEYS[1], "generated_bytes") or "0") +
  string.len(id_field) + string.len(ARGV[3]) +
  string.len(digest_field) + string.len(ARGV[4])
if total > tonumber(ARGV[6]) then
  return {-4, ""}
end
redis.call("HSET", KEYS[1],
  id_field, ARGV[3],
  digest_field, ARGV[4],
  "generated_bytes", total)
redis.call("PEXPIRE", KEYS[1], ARGV[5])
return {1, ARGV[3]}
`

const graphStorePreparedTaskScript = `
if redis.call("GET", KEYS[2]) ~= ARGV[1] then
  return -3
end
if redis.call("HGET", KEYS[1], "state") ~= "active" or
   redis.call("HEXISTS", KEYS[1], "terminal_response") == 1 then
  return -5
end
local field = "prepared_task:" .. ARGV[2]
local old = redis.call("HGET", KEYS[1], field)
if old then
  if old == ARGV[3] then
    return 0
  end
  return -1
end
local total = tonumber(redis.call("HGET", KEYS[1], "prepared_task_bytes") or "0") + string.len(ARGV[3])
if total > tonumber(ARGV[5]) then
  return -4
end
redis.call("HSET", KEYS[1], field, ARGV[3], "prepared_task_bytes", total)
redis.call("PEXPIRE", KEYS[1], ARGV[4])
return 1
`

func graphDurableSessionKey(ownerPrefix, sessionID string) string {
	sum := sha256.Sum256([]byte(ownerPrefix + "\x00" + sessionID))
	encoded := hex.EncodeToString(sum[:])
	return "graph-execution:{" + encoded + "}:state"
}

func graphDurableTenantIndexKey(ownerPrefix string) string {
	sum := sha256.Sum256([]byte(ownerPrefix))
	return "graph-execution:{admission}:active:tenant:" + hex.EncodeToString(sum[:])
}

func graphDurableRetainedTenantIndexKey(ownerPrefix string) string {
	sum := sha256.Sum256([]byte(ownerPrefix))
	return "graph-execution:{admission}:retained:tenant:" + hex.EncodeToString(sum[:])
}

func (s *ExecutionServer) graphCoordinatorID() (string, error) {
	s.graphSessionsMu.Lock()
	defer s.graphSessionsMu.Unlock()
	if s.graphShuttingDown {
		return "", status.UnavailableError("graph execution server is shutting down")
	}
	if s.graphServerID != "" {
		return s.graphServerID, nil
	}
	id := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		return "", status.InternalErrorf("generate graph coordinator ID: %s", err)
	}
	s.graphServerID = hex.EncodeToString(id)
	return s.graphServerID, nil
}

func (s *ExecutionServer) beginGraphHandler() bool {
	s.graphSessionsMu.Lock()
	defer s.graphSessionsMu.Unlock()
	if s.graphShuttingDown {
		return false
	}
	s.graphHandlers.Add(1)
	return true
}

func (s *ExecutionServer) publishGraphExecution(g *graphExecution) error {
	if s.beforeGraphSessionPublishForTesting != nil {
		s.beforeGraphSessionPublishForTesting(g)
	}
	s.graphSessionsMu.Lock()
	if s.graphShuttingDown {
		releaseLease := s.graphShutdownReleases
		if !releaseLease {
			g.preserveLeaseOnCrash.Store(true)
		}
		s.graphSessionsMu.Unlock()
		// This coordinator was never published, so it was not present in the
		// shutdown snapshot. Apply the persisted shutdown policy here and make
		// its handler return rather than allowing a post-shutdown insertion.
		g.discardLocalCoordinatorLocked()
		return status.UnavailableError("graph execution server is shutting down")
	}
	if s.graphSessions == nil {
		s.graphSessions = make(map[string]*graphExecution)
	}
	sessionKey := graphSessionKey(g.ownerPrefix, g.sessionID)
	if s.graphSessions[sessionKey] != nil {
		s.graphSessionsMu.Unlock()
		return status.AlreadyExistsErrorf("graph session %q already exists", g.sessionID)
	}
	s.compactFinishedGraphSessionsLocked()
	activeSessions := 0
	tenantActiveSessions := 0
	for _, session := range s.graphSessions {
		if session.finished.Load() {
			continue
		}
		activeSessions++
		if session.ownerPrefix == g.ownerPrefix {
			tenantActiveSessions++
		}
	}
	if activeSessions >= graphExecutionMaxActiveSessionsGlobal {
		s.graphSessionsMu.Unlock()
		return status.ResourceExhaustedErrorf(
			"server has reached the limit of %d active graph sessions",
			graphExecutionMaxActiveSessionsGlobal)
	}
	if tenantActiveSessions >= graphExecutionMaxActiveSessionsPerTenant {
		s.graphSessionsMu.Unlock()
		return status.ResourceExhaustedErrorf(
			"tenant has reached the limit of %d active graph sessions",
			graphExecutionMaxActiveSessionsPerTenant)
	}
	s.graphSessions[sessionKey] = g
	s.graphSessionsMu.Unlock()
	return nil
}

func (s *ExecutionServer) discardUnpublishedGraphExecution(g *graphExecution) {
	s.graphSessionsMu.Lock()
	if s.graphShuttingDown && !s.graphShutdownReleases {
		g.preserveLeaseOnCrash.Store(true)
	}
	s.graphSessionsMu.Unlock()
	g.discardLocalCoordinatorLocked()
}

// ShutdownGraphExecution gracefully stops graph coordinators owned by this app
// and releases their leases. New GraphExecute streams are rejected. This is
// suitable for an orderly process shutdown, but not for testing abrupt loss.
func (s *ExecutionServer) ShutdownGraphExecution() {
	s.stopGraphExecution(true)
}

// CrashGraphExecution stops graph coordinators without releasing their Redis
// leases. It models abrupt app loss: another app can take over only after the
// fenced lease expires. It is exported so the multi-app integration harness can
// test the same recovery boundary without terminating the test process.
func (s *ExecutionServer) CrashGraphExecution() {
	s.stopGraphExecution(false)
}

func (s *ExecutionServer) stopGraphExecution(releaseLeases bool) {
	s.graphSessionsMu.Lock()
	if s.graphShuttingDown {
		s.graphSessionsMu.Unlock()
		s.graphHandlers.Wait()
		return
	}
	s.graphShuttingDown = true
	s.graphShutdownReleases = releaseLeases
	sessions := make([]*graphExecution, 0, len(s.graphSessions))
	for _, g := range s.graphSessions {
		if !releaseLeases {
			// Mark every coordinator before canceling its waiters. Their
			// cancellation completions race with shutdown and can look like
			// retriable infrastructure errors, but abrupt process loss must
			// leave the lease live until it expires naturally.
			g.preserveLeaseOnCrash.Store(true)
		}
		sessions = append(sessions, g)
	}
	s.graphSessions = make(map[string]*graphExecution)
	s.graphSessionsMu.Unlock()

	for _, g := range sessions {
		if releaseLeases {
			g.releaseDurableLease(context.Background())
		}
		g.cancelGraphWaiters()
	}
	s.graphHandlers.Wait()
}

// createDurableGraphSession atomically creates the tenant-scoped identity and
// journals the exact sequence-1 Begin request. This closes the crash window
// between identity creation and request journaling.
func (g *graphExecution) createDurableGraphSession(begin *graphpb.BeginGraph, beginRequest []byte) (bool, error) {
	if len(begin.GetResumeToken()) != 32 {
		return false, status.InvalidArgumentError("BeginGraph resume_token must contain exactly 32 bytes")
	}
	request := &graphpb.GraphExecuteRequest{}
	if err := proto.Unmarshal(beginRequest, request); err != nil {
		return false, status.InvalidArgumentErrorf("invalid serialized BeginGraph request: %s", err)
	}
	if request.GetSequenceNumber() != 1 || request.GetSessionId() != g.sessionID ||
		request.GetBegin() == nil || !proto.Equal(request.GetBegin(), begin) {
		return false, status.InvalidArgumentError(
			"durable graph creation requires the exact sequence-1 BeginGraph request")
	}
	g.resumeToken = append([]byte(nil), begin.GetResumeToken()...)
	g.durableKey = graphDurableSessionKey(g.ownerPrefix, g.sessionID)
	g.durableLeaseKey = g.durableKey + ":lease"
	g.durableTenantIndexKey = graphDurableTenantIndexKey(g.ownerPrefix)
	retainedAdded, err := g.reserveDurableRetainedAdmissionWithResult()
	if err != nil {
		return false, err
	}
	activeAdded, err := g.reserveDurableAdmissionWithResult()
	if err != nil {
		// A new graph has no durable record yet, so it is safe to undo a newly
		// inserted terminal-capacity reservation. If the record already exists,
		// preserve the refreshed reservation for its current coordinator.
		if retainedAdded {
			exists, existsErr := g.server.rdb.Exists(g.ctx, g.durableKey).Result()
			if existsErr == nil && exists == 0 {
				g.releaseDurableRetainedAdmission()
			}
		}
		return false, err
	}
	result, err := g.server.rdb.Eval(
		g.ctx,
		graphCreateSessionScript,
		[]string{g.durableKey},
		g.ownerPrefix,
		g.sessionID,
		g.resumeToken,
		graphDurableActiveLifetime.Milliseconds(),
		beginRequest,
	).Int()
	if err != nil {
		return false, &graphDurabilityError{status.UnavailableErrorf("create durable graph session: %s", err)}
	}
	if result != 1 && activeAdded {
		// Retried/conflicting Begin calls must not delete an active session's
		// admission. The only extra active reservation to remove is for an
		// already-finished retained session.
		if state, stateErr := g.server.rdb.HGet(g.ctx, g.durableKey, "state").Result(); stateErr == nil && state == "finished" {
			g.releaseDurableActiveAdmission()
		}
	}
	switch result {
	case 1:
		return true, nil
	case 0:
		return false, nil
	case -2:
		return false, status.PermissionDeniedError("graph session already exists with a different resume token")
	case -3:
		return false, status.AlreadyExistsError(
			"graph session already exists with a different BeginGraph request")
	default:
		return false, status.AlreadyExistsErrorf("graph session %q conflicts with retained durable state", g.sessionID)
	}
}

func (g *graphExecution) reserveDurableAdmissionWithResult() (bool, error) {
	result, err := g.server.rdb.Eval(
		g.ctx,
		graphReserveAdmissionScript,
		[]string{
			"graph-execution:{admission}:active:global",
			g.durableTenantIndexKey,
		},
		g.durableKey,
		graphDurableActiveLifetime.Milliseconds(),
		graphExecutionMaxActiveSessionsGlobal,
		graphExecutionMaxActiveSessionsPerTenant,
		graphDurableActiveReservationBytes,
		graphDurableMaxActiveBytesGlobal,
		graphDurableMaxActiveBytesPerTenant,
	).Int()
	if err != nil {
		return false, &graphDurabilityError{status.UnavailableErrorf("reserve durable graph admission: %s", err)}
	}
	switch result {
	case 0, 1:
		g.durableMu.Lock()
		g.lastAdmissionRefresh = g.server.clock.Now()
		g.durableMu.Unlock()
		return result == 1, nil
	case -3:
		metrics.GraphExecutionDurableAdmissionRejectionsCount.Inc()
		return false, status.ResourceExhaustedErrorf(
			"tenant has reached the active durable graph session limit of %d",
			graphExecutionMaxActiveSessionsPerTenant)
	case -4:
		metrics.GraphExecutionDurableAdmissionRejectionsCount.Inc()
		return false, status.ResourceExhaustedErrorf(
			"server has reached the active durable graph session limit of %d",
			graphExecutionMaxActiveSessionsGlobal)
	default:
		return false, &graphDurabilityError{status.UnavailableErrorf("unexpected graph admission result %d", result)}
	}
}

func (g *graphExecution) reserveDurableAdmission() error {
	_, err := g.reserveDurableAdmissionWithResult()
	return err
}

func (g *graphExecution) reserveDurableRetainedAdmissionWithResult() (bool, error) {
	result, err := g.server.rdb.Eval(
		context.Background(),
		graphReserveAdmissionScript,
		[]string{
			"graph-execution:{admission}:retained:global",
			graphDurableRetainedTenantIndexKey(g.ownerPrefix),
		},
		g.durableKey,
		graphExecutionFinishedSessionLifetime.Milliseconds(),
		graphExecutionMaxFinishedSessionsGlobal,
		graphExecutionMaxFinishedSessionsPerTenant,
		graphDurableRetainedReservationBytes,
		graphDurableMaxRetainedBytesGlobal,
		graphDurableMaxRetainedBytesPerTenant,
	).Int()
	if err != nil {
		return false, &graphDurabilityError{status.UnavailableErrorf("reserve retained durable graph admission: %s", err)}
	}
	switch result {
	case 0, 1:
		return result == 1, nil
	case -3:
		metrics.GraphExecutionDurableAdmissionRejectionsCount.Inc()
		return false, status.ResourceExhaustedErrorf(
			"tenant has reached the retained durable graph session limit of %d",
			graphExecutionMaxFinishedSessionsPerTenant)
	case -4:
		metrics.GraphExecutionDurableAdmissionRejectionsCount.Inc()
		return false, status.ResourceExhaustedErrorf(
			"server has reached the retained durable graph session limit of %d",
			graphExecutionMaxFinishedSessionsGlobal)
	default:
		return false, &graphDurabilityError{status.UnavailableErrorf(
			"unexpected retained graph admission result %d", result)}
	}
}

func (g *graphExecution) refreshGuaranteedDurableRetainedAdmission() error {
	result, err := g.server.rdb.Eval(
		context.Background(),
		graphRefreshGuaranteedAdmissionScript,
		[]string{
			"graph-execution:{admission}:retained:global",
			graphDurableRetainedTenantIndexKey(g.ownerPrefix),
		},
		g.durableKey,
		graphExecutionFinishedSessionLifetime.Milliseconds(),
	).Int()
	if err != nil {
		return &graphDurabilityError{status.UnavailableErrorf(
			"refresh guaranteed retained durable graph admission: %s", err)}
	}
	if result != 1 {
		return status.DataLossError(
			"durable graph terminal replay capacity reservation is missing")
	}
	return nil
}

func (g *graphExecution) releaseDurableActiveAdmission() {
	if g.server.rdb == nil || g.durableKey == "" {
		return
	}
	_, _ = g.server.rdb.Eval(
		context.Background(),
		graphReleaseAdmissionScript,
		[]string{
			"graph-execution:{admission}:active:global",
			g.durableTenantIndexKey,
		},
		g.durableKey,
	).Result()
}

func (g *graphExecution) releaseDurableRetainedAdmission() {
	if g.server.rdb == nil || g.durableKey == "" {
		return
	}
	_, _ = g.server.rdb.Eval(
		context.Background(),
		graphReleaseAdmissionScript,
		[]string{
			"graph-execution:{admission}:retained:global",
			graphDurableRetainedTenantIndexKey(g.ownerPrefix),
		},
		g.durableKey,
	).Result()
}

func (g *graphExecution) acquireDurableLease() error {
	if err := g.acquireDurableLeaseWithoutAdmission(); err != nil {
		return err
	}
	return g.refreshDurableAdmission()
}

func (g *graphExecution) acquireDurableLeaseWithoutAdmission() error {
	serverID, err := g.server.graphCoordinatorID()
	if err != nil {
		return err
	}
	result, err := g.server.rdb.Eval(
		g.ctx,
		graphAcquireLeaseScript,
		[]string{g.durableKey, g.durableLeaseKey},
		serverID,
		graphDurableLeaseLifetime.Milliseconds(),
		graphDurableActiveLifetime.Milliseconds(),
	).Slice()
	if err != nil {
		return &graphDurabilityError{status.UnavailableErrorf("acquire graph coordinator lease: %s", err)}
	}
	epoch, err := redisResultInt64(result[0])
	if err != nil {
		return &graphDurabilityError{status.UnavailableErrorf("decode graph lease epoch: %s", err)}
	}
	if epoch == -1 {
		return status.NotFoundErrorf("graph session %q was not found", g.sessionID)
	}
	if epoch == -2 {
		return status.FailedPreconditionErrorf("graph session %q is already finished", g.sessionID)
	}
	if epoch == 0 {
		ttl, _ := redisResultInt64(result[1])
		return status.AlreadyExistsErrorf(
			"graph session %q is coordinated by another app (lease expires in %dms)",
			g.sessionID, ttl)
	}
	value, ok := result[1].(string)
	if !ok || value == "" {
		return &graphDurabilityError{status.UnavailableError("invalid graph coordinator lease value")}
	}
	g.durableMu.Lock()
	g.durableLeaseEpoch = uint64(epoch)
	g.durableLeaseValue = value
	g.durableMu.Unlock()
	return nil
}

func (g *graphExecution) refreshDurableAdmission() error {
	// Refresh terminal capacity first. Active admission without a durable
	// terminal replay guarantee would violate the Begin-time invariant.
	if err := g.refreshGuaranteedDurableRetainedAdmission(); err != nil {
		return err
	}
	return g.reserveDurableAdmission()
}

func redisResultInt64(v any) (int64, error) {
	switch v := v.(type) {
	case int64:
		return v, nil
	case string:
		return strconv.ParseInt(v, 10, 64)
	case []byte:
		return strconv.ParseInt(string(v), 10, 64)
	default:
		return 0, fmt.Errorf("unexpected Redis integer type %T", v)
	}
}

func (g *graphExecution) renewDurableLease() error {
	leaseValue := g.currentDurableLeaseValue()
	if leaseValue == "" {
		return nil
	}
	ok, err := g.server.rdb.Eval(
		g.ctx,
		graphRenewLeaseScript,
		[]string{g.durableKey, g.durableLeaseKey},
		leaseValue,
		graphDurableLeaseLifetime.Milliseconds(),
		graphDurableActiveLifetime.Milliseconds(),
	).Int()
	if err != nil {
		return &graphDurabilityError{status.UnavailableErrorf("renew graph coordinator lease: %s", err)}
	}
	if ok != 1 {
		metrics.GraphExecutionDurableFencedMutationsCount.Inc()
		if ok == -1 {
			return &graphDurabilityError{status.AbortedError("graph session became terminal while renewing its coordinator lease")}
		}
		return &graphDurabilityError{status.AbortedError("graph coordinator lease was fenced by another app")}
	}
	g.durableMu.Lock()
	refreshAdmission := g.lastAdmissionRefresh.IsZero() ||
		g.server.clock.Since(g.lastAdmissionRefresh) >= graphDurableActiveLifetime/3
	g.durableMu.Unlock()
	if refreshAdmission {
		return g.refreshDurableAdmission()
	}
	return nil
}

func (g *graphExecution) renewDurableRecoveryLease() error {
	leaseValue := g.currentDurableLeaseValue()
	if leaseValue == "" {
		return &graphDurabilityError{status.AbortedError(
			"graph recovery has no coordinator lease")}
	}
	ok, err := g.server.rdb.Eval(
		context.Background(),
		graphRenewRecoveryLeaseScript,
		[]string{g.durableKey, g.durableLeaseKey},
		leaseValue,
		graphDurableLeaseLifetime.Milliseconds(),
		graphDurableActiveLifetime.Milliseconds(),
	).Int()
	if err != nil {
		return &graphDurabilityError{status.UnavailableErrorf(
			"renew graph recovery coordinator lease: %s", err)}
	}
	if ok != 1 {
		metrics.GraphExecutionDurableFencedMutationsCount.Inc()
		return &graphDurabilityError{status.AbortedError(
			"graph recovery coordinator lease was fenced or session became immutable")}
	}
	return nil
}

func (g *graphExecution) releaseDurableLease(ctx context.Context) {
	leaseValue := g.currentDurableLeaseValue()
	if leaseValue == "" || g.server.rdb == nil {
		return
	}
	_, _ = g.server.rdb.Eval(
		ctx,
		graphReleaseLeaseScript,
		[]string{g.durableLeaseKey},
		leaseValue,
	).Result()
	g.clearDurableLeaseValue(leaseValue)
}

func (g *graphExecution) finishDurableSession() error {
	if g.isDurableFinished() {
		g.releaseDurableActiveAdmission()
		return nil
	}
	leaseValue := g.currentDurableLeaseValue()
	if leaseValue == "" {
		return &graphDurabilityError{status.AbortedError(
			"graph coordinator lease expired before durable terminal commit")}
	}
	if err := g.refreshGuaranteedDurableRetainedAdmission(); err != nil {
		return err
	}
	if err := g.purgeDurableTerminalFields(leaseValue); err != nil {
		return err
	}
	ok, err := g.server.rdb.Eval(
		context.Background(),
		graphFinishSessionScript,
		[]string{g.durableKey, g.durableLeaseKey},
		leaseValue,
		graphExecutionFinishedSessionLifetime.Milliseconds(),
	).Int()
	if err != nil {
		return &graphDurabilityError{status.UnavailableErrorf("finish durable graph session: %s", err)}
	}
	if ok != 1 {
		switch ok {
		case -1:
			return &graphDurabilityError{status.AbortedError("graph durable session was already terminal")}
		case -2:
			return &graphDurabilityError{status.AbortedError("graph terminal response was not durably committed")}
		default:
			return &graphDurabilityError{status.AbortedError("graph coordinator lease was fenced before terminal commit")}
		}
	}
	g.clearDurableLeaseValue(leaseValue)
	g.setDurableFinished()
	g.releaseDurableActiveAdmission()
	return nil
}

func (g *graphExecution) purgeDurableTerminalFields(leaseValue string) error {
	var cursor uint64
	deletedInPass := false
	for {
		renewed, err := g.server.rdb.Eval(
			context.Background(),
			graphRenewTerminalLeaseScript,
			[]string{g.durableKey, g.durableLeaseKey},
			leaseValue,
			graphDurableLeaseLifetime.Milliseconds(),
			graphDurableActiveLifetime.Milliseconds(),
		).Int()
		if err != nil {
			return &graphDurabilityError{status.UnavailableErrorf(
				"renew graph coordinator lease while purging terminal payload: %s", err)}
		}
		if renewed != 1 {
			return &graphDurabilityError{status.AbortedError(
				"graph coordinator lease was fenced while purging terminal payload")}
		}
		fields, next, err := g.server.rdb.HScan(context.Background(), g.durableKey, cursor, "*", 256).Result()
		if err != nil {
			return &graphDurabilityError{status.UnavailableErrorf(
				"scan terminal graph payload for purge: %s", err)}
		}
		toDelete := make([]any, 0, len(fields)/2+3)
		toDelete = append(
			toDelete,
			leaseValue,
			graphDurableLeaseLifetime.Milliseconds(),
			graphDurableActiveLifetime.Milliseconds(),
		)
		for i := 0; i+1 < len(fields); i += 2 {
			field := fields[i]
			if strings.HasPrefix(field, "request:") ||
				field == "begin_request" ||
				strings.HasPrefix(field, "execution_id:") ||
				strings.HasPrefix(field, "action_digest:") ||
				strings.HasPrefix(field, "prepared_task:") {
				toDelete = append(toDelete, field)
			}
		}
		if len(toDelete) > 3 {
			deletedInPass = true
			result, err := g.server.rdb.Eval(
				context.Background(),
				graphPurgeTerminalFieldsScript,
				[]string{g.durableKey, g.durableLeaseKey},
				toDelete...,
			).Int()
			if err != nil {
				return &graphDurabilityError{status.UnavailableErrorf(
					"purge terminal graph payload: %s", err)}
			}
			if result != 1 {
				return &graphDurabilityError{status.AbortedError(
					"graph coordinator lease was fenced while purging terminal payload")}
			}
		}
		if next == 0 {
			if !deletedInPass {
				return nil
			}
			// HSCAN does not promise a stable snapshot while fields are being
			// deleted. Repeat until a complete pass finds nothing.
			cursor = 0
			deletedInPass = false
			continue
		}
		cursor = next
	}
}

func (g *graphExecution) currentDurableLeaseValue() string {
	g.durableMu.Lock()
	defer g.durableMu.Unlock()
	return g.durableLeaseValue
}

func (g *graphExecution) clearDurableLeaseValue(expected string) {
	g.durableMu.Lock()
	defer g.durableMu.Unlock()
	if g.durableLeaseValue == expected {
		g.durableLeaseValue = ""
	}
}

func (g *graphExecution) isDurableFinished() bool {
	g.durableMu.Lock()
	defer g.durableMu.Unlock()
	return g.durableFinished
}

func (g *graphExecution) setDurableFinished() {
	g.durableMu.Lock()
	g.durableFinished = true
	g.durableMu.Unlock()
}

func (g *graphExecution) appendDurableRequest(sequence uint64, data []byte) (bool, error) {
	if g.reconstructing || g.durableKey == "" {
		return true, nil
	}
	result, err := g.server.rdb.Eval(
		g.ctx,
		graphAppendRequestScript,
		[]string{g.durableKey, g.durableLeaseKey},
		g.currentDurableLeaseValue(),
		strconv.FormatUint(sequence, 10),
		data,
		graphExecutionMaxRequestReplayBytes,
		graphDurableActiveLifetime.Milliseconds(),
	).Slice()
	if err != nil {
		return false, &graphDurabilityError{status.UnavailableErrorf("journal graph request: %s", err)}
	}
	code, err := redisResultInt64(result[0])
	if err != nil {
		return false, &graphDurabilityError{status.UnavailableErrorf("decode graph request journal result: %s", err)}
	}
	switch code {
	case 1:
		metrics.GraphExecutionDurableRequestBytesCount.Add(float64(len(data)))
		return true, nil
	case 0:
		return false, nil
	case -1:
		return false, status.InvalidArgumentErrorf("sequence %d was retransmitted with different contents", sequence)
	case -2:
		last, _ := redisResultInt64(result[1])
		return false, status.InvalidArgumentErrorf("expected request sequence %d, got %d", last+1, sequence)
	case -4:
		return false, status.ResourceExhaustedErrorf("graph request replay data exceeds %d bytes", graphExecutionMaxRequestReplayBytes)
	case -5:
		return false, &graphDurabilityError{status.AbortedError(
			"durable graph session already has a terminal response")}
	default:
		metrics.GraphExecutionDurableFencedMutationsCount.Inc()
		return false, &graphDurabilityError{status.AbortedError("graph coordinator lease was fenced by another app")}
	}
}

func (g *graphExecution) appendDurableResponse(response *graphpb.GraphExecuteResponse, data []byte) error {
	if g.reconstructing || g.durableKey == "" {
		return nil
	}
	result, err := g.server.rdb.Eval(
		g.ctx,
		graphAppendResponseScript,
		[]string{g.durableKey, g.durableLeaseKey},
		g.currentDurableLeaseValue(),
		strconv.FormatUint(response.GetSequenceNumber(), 10),
		data,
		graphExecutionMaxResponseReplayBytes,
		graphDurableActiveLifetime.Milliseconds(),
		boolToRedisInt(response.GetResult() != nil || response.GetError().GetTerminal()),
	).Slice()
	if err != nil {
		return &graphDurabilityError{status.UnavailableErrorf("journal graph response: %s", err)}
	}
	code, err := redisResultInt64(result[0])
	if err != nil {
		return &graphDurabilityError{status.UnavailableErrorf("decode graph response journal result: %s", err)}
	}
	switch code {
	case 1:
		metrics.GraphExecutionDurableResponseBytesCount.Add(float64(len(data)))
		return nil
	case -4:
		return status.ResourceExhaustedErrorf("graph response replay data exceeds %d bytes", graphExecutionMaxResponseReplayBytes)
	case -3:
		metrics.GraphExecutionDurableFencedMutationsCount.Inc()
		return &graphDurabilityError{status.AbortedError("graph coordinator lease was fenced by another app")}
	case -5:
		return &graphDurabilityError{status.AbortedError(
			"durable graph session already has a terminal response")}
	default:
		last, _ := redisResultInt64(result[1])
		return &graphDurabilityError{status.AbortedErrorf(
			"durable response sequence diverged: local %d, durable %d",
			response.GetSequenceNumber(), last)}
	}
}

func (g *graphExecution) claimDurableExecution(nodeID string, actionDigest *repb.Digest) (string, bool, error) {
	if g.durableKey == "" {
		return digest.NewCASResourceName(actionDigest, g.instanceName, g.digestFunction).NewUploadString(), true, nil
	}
	digestData, err := proto.Marshal(actionDigest)
	if err != nil {
		return "", false, status.WrapError(err, "marshal graph action digest")
	}
	candidate := digest.NewCASResourceName(actionDigest, g.instanceName, g.digestFunction).NewUploadString()
	result, err := g.server.rdb.Eval(
		g.ctx,
		graphClaimExecutionScript,
		[]string{g.durableKey, g.durableLeaseKey},
		g.currentDurableLeaseValue(),
		nodeID,
		candidate,
		digestData,
		graphDurableActiveLifetime.Milliseconds(),
		graphDurableMaxGeneratedBytes,
	).Slice()
	if err != nil {
		return "", false, &graphDurabilityError{status.UnavailableErrorf("claim durable graph execution: %s", err)}
	}
	code, err := redisResultInt64(result[0])
	if err != nil {
		return "", false, &graphDurabilityError{status.UnavailableErrorf("decode durable graph execution claim: %s", err)}
	}
	executionID, _ := result[1].(string)
	switch code {
	case 1:
		return executionID, true, nil
	case 0:
		return executionID, false, nil
	case -1:
		return "", false, status.FailedPreconditionErrorf("action digest for node %q changed during recovery", nodeID)
	case -4:
		return "", false, status.ResourceExhaustedErrorf(
			"generated durable graph execution state exceeds %d bytes",
			graphDurableMaxGeneratedBytes)
	case -5:
		return "", false, &graphDurabilityError{status.AbortedError(
			"durable graph session already has a terminal response")}
	default:
		return "", false, &graphDurabilityError{status.AbortedError("graph coordinator lease was fenced by another app")}
	}
}

func (g *graphExecution) storeDurablePreparedTask(nodeID string, request *scpb.EnsureTaskRequest) error {
	data, err := (proto.MarshalOptions{Deterministic: true}).Marshal(request)
	if err != nil {
		return status.WrapError(err, "marshal durable prepared task")
	}
	result, err := g.server.rdb.Eval(
		g.ctx,
		graphStorePreparedTaskScript,
		[]string{g.durableKey, g.durableLeaseKey},
		g.currentDurableLeaseValue(),
		nodeID,
		data,
		graphDurableActiveLifetime.Milliseconds(),
		graphDurableMaxPreparedTaskBytes,
	).Int()
	if err != nil {
		return &graphDurabilityError{status.UnavailableErrorf("journal prepared graph task: %s", err)}
	}
	switch result {
	case 0:
		return nil
	case 1:
		metrics.GraphExecutionDurablePreparedTaskBytesCount.Add(float64(len(data)))
		return nil
	case -1:
		return status.FailedPreconditionErrorf("prepared scheduler task for node %q changed during recovery", nodeID)
	case -4:
		return status.ResourceExhaustedErrorf(
			"graph prepared scheduler tasks exceed %d bytes", graphDurableMaxPreparedTaskBytes)
	case -5:
		return &graphDurabilityError{status.AbortedError(
			"durable graph session already has a terminal response")}
	default:
		return &graphDurabilityError{status.AbortedError("graph coordinator lease was fenced by another app")}
	}
}

func boolToRedisInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

type durableGraphData struct {
	requests     map[uint64][]byte
	responses    map[uint64][]byte
	executionIDs map[string]string
	actionData   map[string][]byte
	taskData     map[string][]byte
	finished     bool
}

func (g *graphExecution) inspectDurableGraphIdentityAndState() (bool, error) {
	values, err := g.server.rdb.HMGet(
		g.ctx, g.durableKey, "owner", "session", "token", "state",
	).Result()
	if err != nil {
		return false, &graphDurabilityError{status.UnavailableErrorf(
			"inspect durable graph identity: %s", err)}
	}
	if len(values) != 4 {
		return false, &graphDurabilityError{status.UnavailableError(
			"inspect durable graph identity returned an invalid field count")}
	}
	allMissing := true
	for _, value := range values {
		if value != nil {
			allMissing = false
			break
		}
	}
	if allMissing {
		return false, status.NotFoundErrorf("graph session %q was not found", g.sessionID)
	}
	asString := func(field string, value any) (string, error) {
		switch value := value.(type) {
		case string:
			return value, nil
		case []byte:
			return string(value), nil
		case nil:
			return "", status.DataLossErrorf("durable graph session is missing %s", field)
		default:
			return "", status.DataLossErrorf(
				"durable graph session has invalid %s type %T", field, value)
		}
	}
	owner, err := asString("owner", values[0])
	if err != nil {
		return false, err
	}
	session, err := asString("session", values[1])
	if err != nil {
		return false, err
	}
	token, err := asString("token", values[2])
	if err != nil {
		return false, err
	}
	state, err := asString("state", values[3])
	if err != nil {
		return false, err
	}
	if owner != g.ownerPrefix || session != g.sessionID {
		return false, status.PermissionDeniedError("durable graph session tenant mismatch")
	}
	if token != string(g.resumeToken) {
		return false, status.PermissionDeniedError("invalid graph resume token")
	}
	switch state {
	case "active":
		return false, nil
	case "finished":
		return true, nil
	default:
		return false, status.DataLossErrorf("durable graph session has invalid state %q", state)
	}
}

func (g *graphExecution) loadDurableGraph() (*durableGraphData, error) {
	fieldCount, err := g.server.rdb.HLen(g.ctx, g.durableKey).Result()
	if err != nil {
		return nil, &graphDurabilityError{status.UnavailableErrorf("inspect durable graph session: %s", err)}
	}
	if fieldCount == 0 {
		return nil, status.NotFoundErrorf("graph session %q was not found", g.sessionID)
	}
	maxFields := int64(32 + graphExecutionMaxReplayRequests + graphExecutionMaxReplayResponses + 3*graphExecutionMaxNodes)
	if fieldCount > maxFields {
		return nil, status.ResourceExhaustedErrorf(
			"durable graph session has %d fields, exceeding recovery limit %d",
			fieldCount, maxFields)
	}
	owner, err := g.server.rdb.HGet(g.ctx, g.durableKey, "owner").Result()
	if err != nil {
		return nil, &graphDurabilityError{status.UnavailableErrorf("load durable graph owner: %s", err)}
	}
	session, err := g.server.rdb.HGet(g.ctx, g.durableKey, "session").Result()
	if err != nil {
		return nil, &graphDurabilityError{status.UnavailableErrorf("load durable graph session ID: %s", err)}
	}
	token, err := g.server.rdb.HGet(g.ctx, g.durableKey, "token").Result()
	if err != nil {
		return nil, &graphDurabilityError{status.UnavailableErrorf("load durable graph token: %s", err)}
	}
	state, err := g.server.rdb.HGet(g.ctx, g.durableKey, "state").Result()
	if err != nil {
		return nil, &graphDurabilityError{status.UnavailableErrorf("load durable graph state: %s", err)}
	}
	if owner != g.ownerPrefix || session != g.sessionID {
		return nil, status.PermissionDeniedError("durable graph session tenant mismatch")
	}
	if token != string(g.resumeToken) {
		return nil, status.PermissionDeniedError("invalid graph resume token")
	}
	data := &durableGraphData{
		requests:     make(map[uint64][]byte),
		responses:    make(map[uint64][]byte),
		executionIDs: make(map[string]string),
		actionData:   make(map[string][]byte),
		taskData:     make(map[string][]byte),
		finished:     state == "finished",
	}
	var cursor uint64
	var requestBytes, responseBytes, preparedBytes, recoveryBytes int64
	seenFields := make(map[string]struct{}, fieldCount)
	for {
		values, next, err := g.server.rdb.HScan(g.ctx, g.durableKey, cursor, "*", 256).Result()
		if err != nil {
			return nil, &graphDurabilityError{status.UnavailableErrorf(
				"scan durable graph session: %s", err)}
		}
		for i := 0; i+1 < len(values); i += 2 {
			field, value := values[i], values[i+1]
			// HSCAN may return the same field on multiple pages while the hash
			// table is being resized. Account and decode each durable field
			// exactly once.
			if _, duplicate := seenFields[field]; duplicate {
				continue
			}
			seenFields[field] = struct{}{}
			recoveryBytes += int64(len(field) + len(value))
			if recoveryBytes > graphDurableMaxRecoveryBytes {
				return nil, status.ResourceExhaustedErrorf(
					"durable graph recovery data exceeds %d bytes",
					graphDurableMaxRecoveryBytes)
			}
			switch {
			case strings.HasPrefix(field, "request:"):
				requestBytes += int64(len(value))
				if requestBytes > graphExecutionMaxRequestReplayBytes ||
					len(data.requests) >= graphExecutionMaxReplayRequests {
					return nil, status.ResourceExhaustedError(
						"durable graph request journal exceeds recovery limits")
				}
				seq, err := strconv.ParseUint(strings.TrimPrefix(field, "request:"), 10, 64)
				if err != nil {
					return nil, status.DataLossErrorf("invalid durable graph request field %q", field)
				}
				data.requests[seq] = []byte(value)
			case strings.HasPrefix(field, "response:"):
				responseBytes += int64(len(value))
				if responseBytes > graphExecutionMaxResponseReplayBytes ||
					len(data.responses) >= graphExecutionMaxReplayResponses {
					return nil, status.ResourceExhaustedError(
						"durable graph response journal exceeds recovery limits")
				}
				seq, err := strconv.ParseUint(strings.TrimPrefix(field, "response:"), 10, 64)
				if err != nil {
					return nil, status.DataLossErrorf("invalid durable graph response field %q", field)
				}
				data.responses[seq] = []byte(value)
			case strings.HasPrefix(field, "execution_id:"):
				data.executionIDs[strings.TrimPrefix(field, "execution_id:")] = value
			case strings.HasPrefix(field, "action_digest:"):
				data.actionData[strings.TrimPrefix(field, "action_digest:")] = []byte(value)
			case strings.HasPrefix(field, "prepared_task:"):
				preparedBytes += int64(len(value))
				if preparedBytes > graphDurableMaxPreparedTaskBytes {
					return nil, status.ResourceExhaustedError(
						"durable graph prepared tasks exceed recovery limits")
				}
				data.taskData[strings.TrimPrefix(field, "prepared_task:")] = []byte(value)
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return data, nil
}

func sortedDurableSequences[V any](m map[uint64]V) []uint64 {
	sequences := make([]uint64, 0, len(m))
	for sequence := range m {
		sequences = append(sequences, sequence)
	}
	sort.Slice(sequences, func(i, j int) bool { return sequences[i] < sequences[j] })
	return sequences
}

func newGraphExecution(
	s *ExecutionServer,
	stream graphpb.GraphExecution_GraphExecuteServer,
	ctx context.Context,
	ownerPrefix string,
) *graphExecution {
	graphCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	missingBlobFinder := graphMissingBlobFinder(s.cache)
	if s.graphMissingBlobFinderForTesting != nil {
		missingBlobFinder = s.graphMissingBlobFinderForTesting
	}
	return &graphExecution{
		server:               s,
		stream:               stream,
		ctx:                  graphCtx,
		cancel:               cancel,
		ownerPrefix:          ownerPrefix,
		missingBlobFinder:    missingBlobFinder,
		requestData:          make(map[uint64][]byte),
		nodes:                make(map[string]*graphNodeState),
		availableBlobs:       make(map[digest.Key]struct{}),
		outputs:              make(map[string]map[string]*graphArtifact),
		completions:          make(chan graphActionCompletion, graphExecutionMaxInFlightActions),
		dirtyNodeSet:         make(map[string]struct{}),
		sourceConsumers:      make(map[digest.Key]map[string]int),
		producerConsumers:    make(map[string][]graphProducedConsumer),
		schedulingDependents: make(map[string]map[string]int),
	}
}

func (s *ExecutionServer) recoverDurableGraphExecution(
	stream graphpb.GraphExecution_GraphExecuteServer,
	ctx context.Context,
	ownerPrefix, sessionID string,
	resumeToken []byte,
) (*graphExecution, error) {
	g := newGraphExecution(s, stream, ctx, ownerPrefix)
	g.sessionID = sessionID
	g.resumeToken = append([]byte(nil), resumeToken...)
	g.durableKey = graphDurableSessionKey(ownerPrefix, sessionID)
	g.durableLeaseKey = g.durableKey + ":lease"
	g.durableTenantIndexKey = graphDurableTenantIndexKey(ownerPrefix)
	if err := g.loadAndReconstructDurableGraph(); err != nil {
		s.discardUnpublishedGraphExecution(g)
		return nil, err
	}
	if err := s.publishGraphExecution(g); err != nil {
		if !g.preserveLeaseOnCrash.Load() {
			g.releaseDurableLease(context.Background())
		}
		return nil, err
	}
	return g, nil
}

func (g *graphExecution) loadDurableGraphForRecovery() (*durableGraphData, error) {
	finished, err := g.inspectDurableGraphIdentityAndState()
	if err != nil {
		return nil, err
	}
	if finished {
		data, err := g.loadDurableGraph()
		if err != nil {
			return nil, err
		}
		g.setDurableFinished()
		g.releaseDurableActiveAdmission()
		return data, nil
	}
	if g.beforeRecoveryLeaseAcquireForTesting != nil {
		g.beforeRecoveryLeaseAcquireForTesting()
	}
	if err := g.acquireDurableLeaseWithoutAdmission(); err != nil {
		if status.IsFailedPreconditionError(err) {
			// The bounded state probe may race a coordinator that commits the
			// immutable finished transition immediately before lease acquire.
			finishedData, reloadErr := g.loadDurableGraph()
			if reloadErr != nil {
				return nil, reloadErr
			}
			if finishedData.finished {
				g.setDurableFinished()
				g.releaseDurableActiveAdmission()
				return finishedData, nil
			}
		}
		return nil, err
	}
	g.startRecoveryHeartbeat()
	if g.afterRecoveryLeaseAcquireForTesting != nil {
		g.afterRecoveryLeaseAcquireForTesting()
	}
	if err := g.waitRecoveryAdmissionReady(); err != nil {
		if heartbeatErr := g.stopRecoveryHeartbeat(false); heartbeatErr != nil {
			err = heartbeatErr
		}
		g.releaseDurableLease(context.Background())
		return nil, err
	}
	// Active reconstruction performs its only full scan after acquiring the
	// lease and starting both recovery keepalive loops.
	data, err := g.loadDurableGraph()
	if err != nil {
		if heartbeatErr := g.stopRecoveryHeartbeat(false); heartbeatErr != nil {
			err = heartbeatErr
		}
		g.releaseDurableLease(context.Background())
		return nil, err
	}
	if data.finished {
		if err := g.stopRecoveryHeartbeat(false); err != nil {
			g.releaseDurableLease(context.Background())
			return nil, err
		}
		g.setDurableFinished()
		g.releaseDurableLease(context.Background())
		g.releaseDurableActiveAdmission()
	}
	return data, nil
}

func (g *graphExecution) startRecoveryHeartbeat() {
	g.recoveryHeartbeatMu.Lock()
	if g.recoveryHeartbeatStop != nil {
		g.recoveryHeartbeatMu.Unlock()
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	g.recoveryHeartbeatStop = stop
	g.recoveryHeartbeatDone = done
	g.recoveryHeartbeatErr = nil
	admissionStop := make(chan struct{})
	admissionDone := make(chan struct{})
	admissionReady := make(chan struct{})
	g.recoveryAdmissionStop = admissionStop
	g.recoveryAdmissionDone = admissionDone
	g.recoveryAdmissionReady = admissionReady
	g.recoveryHeartbeatMu.Unlock()
	go func() {
		defer close(done)
		ticker := time.NewTicker(graphDurableLeaseRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := g.renewDurableRecoveryLease(); err != nil {
					g.setRecoveryHeartbeatError(err)
					return
				}
			}
		}
	}()
	go func() {
		defer close(admissionDone)
		interval := graphDurableAdmissionRefresh
		if g.recoveryAdmissionIntervalForTesting > 0 {
			interval = g.recoveryAdmissionIntervalForTesting
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		refresh := g.refreshDurableAdmission
		if g.recoveryAdmissionRefreshForTesting != nil {
			refresh = g.recoveryAdmissionRefreshForTesting
		}
		initial := true
		for {
			if err := refresh(); err != nil {
				g.setRecoveryHeartbeatError(err)
				return
			}
			// Admission writes are deliberately allowed to overcount when this
			// coordinator is fenced. Never publish after such a slow or
			// unfenced write without rechecking the exact lease.
			if err := g.renewDurableRecoveryLease(); err != nil {
				g.setRecoveryHeartbeatError(err)
				return
			}
			if initial {
				close(admissionReady)
				initial = false
			}
			select {
			case <-admissionStop:
				return
			case <-ticker.C:
			}
		}
	}()
}

func (g *graphExecution) waitRecoveryAdmissionReady() error {
	g.recoveryHeartbeatMu.Lock()
	ready := g.recoveryAdmissionReady
	done := g.recoveryAdmissionDone
	g.recoveryHeartbeatMu.Unlock()
	if ready == nil {
		return nil
	}
	select {
	case <-ready:
		return nil
	case <-done:
		g.recoveryHeartbeatMu.Lock()
		err := g.recoveryHeartbeatErr
		g.recoveryHeartbeatMu.Unlock()
		if err == nil {
			return &graphDurabilityError{status.UnavailableError(
				"graph recovery admission refresher stopped before initial refresh")}
		}
		return err
	}
}

func (g *graphExecution) setRecoveryHeartbeatError(err error) {
	g.recoveryHeartbeatMu.Lock()
	if g.recoveryHeartbeatErr == nil {
		g.recoveryHeartbeatErr = err
	}
	g.recoveryHeartbeatMu.Unlock()
	g.cancel()
}

func (g *graphExecution) stopRecoveryHeartbeat(verifyFence bool) error {
	g.recoveryHeartbeatMu.Lock()
	stop := g.recoveryHeartbeatStop
	done := g.recoveryHeartbeatDone
	admissionStop := g.recoveryAdmissionStop
	admissionDone := g.recoveryAdmissionDone
	if stop == nil {
		err := g.recoveryHeartbeatErr
		g.recoveryHeartbeatMu.Unlock()
		if err != nil || !verifyFence || g.currentDurableLeaseValue() == "" {
			return err
		}
		if g.beforeRecoveryFinalFenceCheckForTesting != nil {
			g.beforeRecoveryFinalFenceCheckForTesting()
		}
		return g.renewDurableRecoveryLease()
	}
	g.recoveryHeartbeatMu.Unlock()
	// Stop and join the potentially slow admission refresher while the pure
	// lease heartbeat remains live.
	if admissionStop != nil {
		close(admissionStop)
		<-admissionDone
	}
	g.recoveryHeartbeatMu.Lock()
	g.recoveryAdmissionStop = nil
	g.recoveryAdmissionDone = nil
	g.recoveryAdmissionReady = nil
	g.recoveryHeartbeatStop = nil
	g.recoveryHeartbeatDone = nil
	close(stop)
	g.recoveryHeartbeatMu.Unlock()
	<-done
	g.recoveryHeartbeatMu.Lock()
	err := g.recoveryHeartbeatErr
	g.recoveryHeartbeatMu.Unlock()
	if err != nil || !verifyFence {
		return err
	}
	if g.beforeRecoveryFinalFenceCheckForTesting != nil {
		g.beforeRecoveryFinalFenceCheckForTesting()
	}
	// Close the race between joining the heartbeat and publishing this local
	// coordinator: verify the exact lease one final time.
	return g.renewDurableRecoveryLease()
}

func (g *graphExecution) loadAndReconstructDurableGraph() (returnErr error) {
	data, err := g.loadDurableGraphForRecovery()
	if err != nil {
		return err
	}
	return g.reconstructDurableGraph(data)
}

func (g *graphExecution) reconstructDurableGraph(data *durableGraphData) (returnErr error) {
	defer func() {
		if err := g.stopRecoveryHeartbeat(true); err != nil {
			returnErr = err
		}
	}()
	metrics.GraphExecutionDurableRecoveriesCount.Inc()
	g.reconstructing = true
	defer func() { g.reconstructing = false }()

	if !data.finished {
		for _, raw := range data.responses {
			response := &graphpb.GraphExecuteResponse{}
			if err := proto.Unmarshal(raw, response); err == nil &&
				(response.GetResult() != nil || response.GetError().GetTerminal()) {
				data.finished = true
				break
			}
		}
	}
	if data.finished {
		for _, sequence := range sortedDurableSequences(data.requests) {
			raw := data.requests[sequence]
			request := &graphpb.GraphExecuteRequest{}
			if err := proto.Unmarshal(raw, request); err != nil {
				return status.DataLossErrorf("unmarshal durable graph request %d: %s", sequence, err)
			}
			if request.GetSequenceNumber() != sequence || request.GetSessionId() != g.sessionID {
				return status.DataLossErrorf("durable graph request %d has inconsistent identity", sequence)
			}
			g.requestData[sequence] = append([]byte(nil), raw...)
			g.requestReplayBytes += int64(len(raw))
			g.lastRequestSequence = sequence
		}
		for _, sequence := range sortedDurableSequences(data.responses) {
			raw := data.responses[sequence]
			response := &graphpb.GraphExecuteResponse{}
			if err := proto.Unmarshal(raw, response); err != nil {
				return status.DataLossErrorf("unmarshal durable graph response %d: %s", sequence, err)
			}
			if response.GetSequenceNumber() != sequence {
				return status.DataLossErrorf("durable graph response %d has inconsistent sequence", sequence)
			}
			g.responses = append(g.responses, response)
			g.responseSequence = sequence
			g.responseReplayBytes += int64(len(raw))
		}
		g.finished.Store(true)
		return nil
	}

	sawBegin := false
requestLoop:
	for _, sequence := range sortedDurableSequences(data.requests) {
		raw := data.requests[sequence]
		request := &graphpb.GraphExecuteRequest{}
		if err := proto.Unmarshal(raw, request); err != nil {
			return status.DataLossErrorf("unmarshal durable graph request %d: %s", sequence, err)
		}
		if request.GetSequenceNumber() != sequence || request.GetSessionId() != g.sessionID {
			return status.DataLossErrorf("durable graph request %d has inconsistent identity", sequence)
		}
		g.requestData[sequence] = append([]byte(nil), raw...)
		g.requestReplayBytes += int64(len(raw))
		g.lastRequestSequence = sequence
		switch {
		case request.GetBegin() != nil:
			begin := request.GetBegin()
			switch {
			case sawBegin:
				g.recoveredTerminalErr = status.FailedPreconditionError("BeginGraph was already received")
				break requestLoop
			case begin.GetProtocolVersion() != graphExecutionProtocolVersion:
				g.recoveredTerminalErr = status.InvalidArgumentErrorf(
					"unsupported graph protocol version %d", begin.GetProtocolVersion())
				break requestLoop
			case begin.GetDigestFunction() == repb.DigestFunction_UNKNOWN:
				g.recoveredTerminalErr = status.InvalidArgumentError("digest_function is required")
				break requestLoop
			case len(begin.GetResumeToken()) != 32:
				g.recoveredTerminalErr = status.InvalidArgumentError(
					"BeginGraph resume_token must contain exactly 32 bytes")
				break requestLoop
			case string(begin.GetResumeToken()) != string(g.resumeToken):
				g.recoveredTerminalErr = status.PermissionDeniedError(
					"BeginGraph resume token does not match durable session")
				break requestLoop
			}
			sawBegin = true
			g.instanceName = begin.GetInstanceName()
			g.digestFunction = begin.GetDigestFunction()
			g.invocationID = begin.GetInvocationId()
			g.roots = proto.Clone(begin).(*graphpb.BeginGraph).GetRoots()
			g.beginTime = g.server.clock.Now()
		case request.GetUploadedBlobs() != nil:
			if !sawBegin {
				g.recoveredTerminalErr = status.FailedPreconditionError(
					"first graph request must contain BeginGraph")
				break requestLoop
			}
			if g.committed {
				g.recoveredTerminalErr = status.FailedPreconditionError(
					"graph declarations are immutable after CommitGraph")
				break requestLoop
			}
			if err := g.handleUploadedBlobs(request.GetUploadedBlobs()); err != nil {
				if isRetriableGraphError(err) {
					return err
				}
				g.recoveredTerminalErr = err
				break requestLoop
			}
		case request.GetAction() != nil:
			if !sawBegin {
				g.recoveredTerminalErr = status.FailedPreconditionError(
					"first graph request must contain BeginGraph")
				break requestLoop
			}
			if g.committed {
				g.recoveredTerminalErr = status.FailedPreconditionError(
					"graph declarations are immutable after CommitGraph")
				break requestLoop
			}
			if err := g.handleAction(request.GetAction()); err != nil {
				if isRetriableGraphError(err) {
					return err
				}
				g.recoveredTerminalErr = err
				break requestLoop
			}
		case request.GetCommit() != nil:
			if !sawBegin {
				g.recoveredTerminalErr = status.FailedPreconditionError(
					"first graph request must contain BeginGraph")
				break requestLoop
			}
			if g.committed {
				g.recoveredTerminalErr = status.FailedPreconditionError(
					"graph declarations are immutable after CommitGraph")
				break requestLoop
			}
			commit := request.GetCommit()
			if commit.GetExpectedActionCount() != uint64(len(g.nodes)) {
				g.recoveredTerminalErr = status.InvalidArgumentErrorf(
					"CommitGraph expected %d actions but received %d",
					commit.GetExpectedActionCount(), len(g.nodes))
				break requestLoop
			}
			if err := g.validateCompleteGraph(); err != nil {
				g.recoveredTerminalErr = err
				break requestLoop
			}
			g.committed = true
			for nodeID, node := range g.nodes {
				if node.definition.GetDoNotCache() {
					g.markNodeDirty(nodeID)
				}
			}
		case request.GetResume() != nil:
			g.recoveredTerminalErr = status.UnimplementedError(
				"resuming GraphExecute streams is not implemented")
			break requestLoop
		default:
			g.recoveredTerminalErr = status.InvalidArgumentError(
				"GraphExecuteRequest payload is required")
			break requestLoop
		}
	}

	for _, sequence := range sortedDurableSequences(data.responses) {
		raw := data.responses[sequence]
		response := &graphpb.GraphExecuteResponse{}
		if err := proto.Unmarshal(raw, response); err != nil {
			return status.DataLossErrorf("unmarshal durable graph response %d: %s", sequence, err)
		}
		if response.GetSequenceNumber() != sequence {
			return status.DataLossErrorf("durable graph response %d has inconsistent sequence", sequence)
		}
		g.responses = append(g.responses, response)
		g.responseSequence = sequence
		g.responseReplayBytes += int64(len(raw))
		if nodeResult := response.GetNodeResult(); nodeResult != nil {
			if err := g.applyRecoveredNodeResult(nodeResult); err != nil {
				return err
			}
		}
		if response.GetResult() != nil || response.GetError().GetTerminal() {
			g.finished.Store(true)
			g.recoveredTerminalErr = nil
		}
	}

	if g.recoveredTerminalErr != nil {
		return nil
	}

	for nodeID, executionID := range data.executionIDs {
		node := g.nodes[nodeID]
		if node == nil {
			return status.DataLossErrorf("durable execution references unknown node %q", nodeID)
		}
		if node.completed {
			continue
		}
		actionDigest := &repb.Digest{}
		if err := proto.Unmarshal(data.actionData[nodeID], actionDigest); err != nil {
			return status.DataLossErrorf("unmarshal durable action digest for node %q: %s", nodeID, err)
		}
		node.running = true
		node.actionDigest = actionDigest
		node.executionID = executionID
		if raw := data.taskData[nodeID]; len(raw) > 0 {
			node.preparedTask = &scpb.EnsureTaskRequest{}
			if err := proto.Unmarshal(raw, node.preparedTask); err != nil {
				return status.DataLossErrorf("unmarshal durable prepared task for node %q: %s", nodeID, err)
			}
		}
		g.inFlight++
	}
	g.needsRecoveredWaiters = g.inFlight > 0
	return nil
}

func (g *graphExecution) applyRecoveredNodeResult(result *graphpb.NodeResult) error {
	node := g.nodes[result.GetNodeId()]
	if node == nil {
		return status.DataLossErrorf("durable result references unknown node %q", result.GetNodeId())
	}
	if node.completed {
		return nil
	}
	if result.GetExecuteResponse() == nil || result.GetActionDigest() == nil {
		return status.DataLossErrorf("durable result for node %q is incomplete", result.GetNodeId())
	}
	node.completed = true
	node.running = false
	node.actionDigest = result.GetActionDigest()
	node.response = result.GetExecuteResponse()
	g.completedNodes++
	for _, output := range result.GetExecuteResponse().GetResult().GetOutputFiles() {
		if output.GetDigest() == nil {
			continue
		}
		if g.outputs[result.GetNodeId()] == nil {
			g.outputs[result.GetNodeId()] = make(map[string]*graphArtifact)
		}
		g.outputs[result.GetNodeId()][output.GetPath()] = &graphArtifact{
			digest:       output.GetDigest(),
			isExecutable: output.GetIsExecutable(),
		}
	}
	if executeResponseSucceeded(result.GetExecuteResponse()) {
		return g.resolveDownstreamPrerequisites(result.GetNodeId())
	}
	return nil
}
