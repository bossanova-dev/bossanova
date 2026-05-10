package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// mockHostClient is a test double for hostclient.Client. Each RPC records
// invocations and consults a per-call hook so a test can script the
// response (for example, returning AlreadyExists from StartChatRun).
//
// The original autopilot-era mock covered ListWorkflows / CreateWorkflow /
// CreateAttempt / GetAttemptStatus / UpdateWorkflow / StreamAttemptOutput.
// All of those host RPCs disappeared with autopilot; the trimmed surface
// here matches the repair plugin's current host client.
//
// Repair switched from StartAgentRun/WaitAgentRun to StartChatRun/WaitChatRun
// in Task 5 — the tmux-hosted variants spawn the agent inside an
// operator-attachable tmux session so the run surfaces in the chat list.
// The "AgentRun" RPCs remain on the interface for other callers (eg. the
// host's non-chat agent path) but the repair plugin no longer uses them,
// so the unused stubs return "not implemented".
type mockHostClient struct {
	mu sync.Mutex

	sessions      []*bossanovav1.Session
	listSessErr   error
	listSessCalls int

	startResp  *bossanovav1.StartChatRunHostResponse
	startErr   error
	startCalls int
	startReqs  []*bossanovav1.StartChatRunHostRequest
	startFunc  func(*bossanovav1.StartChatRunHostRequest) (*bossanovav1.StartChatRunHostResponse, error)

	waitResp  *bossanovav1.WaitChatRunHostResponse
	waitErr   error
	waitCalls int
	waitReqs  []*bossanovav1.WaitChatRunHostRequest
	waitFunc  func(ctx context.Context, req *bossanovav1.WaitChatRunHostRequest) (*bossanovav1.WaitChatRunHostResponse, error)

	fireEventCalls int
	fireEventReqs  []*bossanovav1.FireSessionEventRequest

	setRepairStatusReqs []*bossanovav1.SetRepairStatusRequest

	recordOutcomeReqs []*bossanovav1.RecordRepairOutcomeRequest
}

var _ hostClient = (*mockHostClient)(nil)

func newTestMock() *mockHostClient {
	return &mockHostClient{
		startResp: &bossanovav1.StartChatRunHostResponse{AgentSessionId: "claude-1"},
		waitResp:  &bossanovav1.WaitChatRunHostResponse{},
	}
}

func (m *mockHostClient) ListSessions(_ context.Context) (*bossanovav1.HostServiceListSessionsResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listSessCalls++
	if m.listSessErr != nil {
		return nil, m.listSessErr
	}
	return &bossanovav1.HostServiceListSessionsResponse{Sessions: m.sessions}, nil
}

func (m *mockHostClient) GetReviewComments(_ context.Context, _ *bossanovav1.GetReviewCommentsRequest) (*bossanovav1.GetReviewCommentsResponse, error) {
	return nil, errors.New("not implemented in mock")
}

func (m *mockHostClient) FireSessionEvent(_ context.Context, req *bossanovav1.FireSessionEventRequest) (*bossanovav1.FireSessionEventResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fireEventCalls++
	m.fireEventReqs = append(m.fireEventReqs, req)
	return &bossanovav1.FireSessionEventResponse{}, nil
}

func (m *mockHostClient) SetRepairStatus(_ context.Context, req *bossanovav1.SetRepairStatusRequest) (*bossanovav1.SetRepairStatusResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setRepairStatusReqs = append(m.setRepairStatusReqs, req)
	return &bossanovav1.SetRepairStatusResponse{}, nil
}

func (m *mockHostClient) RecordRepairOutcome(_ context.Context, req *bossanovav1.RecordRepairOutcomeRequest) (*bossanovav1.RecordRepairOutcomeResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordOutcomeReqs = append(m.recordOutcomeReqs, req)
	return &bossanovav1.RecordRepairOutcomeResponse{}, nil
}

// StartAgentRun / WaitAgentRun remain on the hostClient interface (other
// callers may still use them) but the repair plugin switched to the chat
// variants in Task 5. The unused stubs fail loudly so any accidental
// regression to the old RPC surfaces immediately rather than silently
// going through an un-recorded path.
func (m *mockHostClient) StartAgentRun(_ context.Context, _ *bossanovav1.StartAgentRunHostRequest) (*bossanovav1.StartAgentRunHostResponse, error) {
	return nil, errors.New("repair plugin must call StartChatRun, not StartAgentRun")
}

func (m *mockHostClient) WaitAgentRun(_ context.Context, _ *bossanovav1.WaitAgentRunHostRequest) (*bossanovav1.WaitAgentRunHostResponse, error) {
	return nil, errors.New("repair plugin must call WaitChatRun, not WaitAgentRun")
}

func (m *mockHostClient) StartChatRun(_ context.Context, req *bossanovav1.StartChatRunHostRequest) (*bossanovav1.StartChatRunHostResponse, error) {
	m.mu.Lock()
	m.startCalls++
	m.startReqs = append(m.startReqs, req)
	fn := m.startFunc
	resp, err := m.startResp, m.startErr
	m.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return resp, err
}

func (m *mockHostClient) WaitChatRun(ctx context.Context, req *bossanovav1.WaitChatRunHostRequest) (*bossanovav1.WaitChatRunHostResponse, error) {
	m.mu.Lock()
	m.waitCalls++
	m.waitReqs = append(m.waitReqs, req)
	fn := m.waitFunc
	resp, err := m.waitResp, m.waitErr
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return resp, err
}

func (m *mockHostClient) snapshot() (startCalls, waitCalls, fireCalls int, setRepair []*bossanovav1.SetRepairStatusRequest) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*bossanovav1.SetRepairStatusRequest, len(m.setRepairStatusReqs))
	copy(out, m.setRepairStatusReqs)
	return m.startCalls, m.waitCalls, m.fireEventCalls, out
}

func newTestMonitor(mock *mockHostClient) *repairMonitor {
	rm := newRepairMonitor(mock, zerolog.Nop())
	rm.stopped = false
	rm.config = &repairConfig{}
	return rm
}

// waitFor spins until cond returns true or 2s elapses.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

// --- repairSession unit tests ---

func TestRepairSession_HappyPath(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS},
	}
	rm := newTestMonitor(mock)
	rm.repairing["s1"] = true

	rm.repairSession(t.Context(), "s1", "repo", "session-name",
		bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123")

	startCalls, waitCalls, fireCalls, setRepair := mock.snapshot()
	require.Equal(t, 1, startCalls, "StartChatRun called once")
	require.Equal(t, 1, waitCalls, "WaitChatRun called once")
	require.Equal(t, 1, fireCalls, "FIX_COMPLETE fired in FIXING_CHECKS")
	require.Equal(t, "/boss-repair", mock.startReqs[0].GetPrompt())
	// Title surfaces the repair run in the chat list as
	// "Repair: <session-name>" so operators can attach via tmux.
	require.Equal(t, "Repair: session-name", mock.startReqs[0].GetTitle())

	require.Len(t, setRepair, 2, "IsRepairing flips on then off")
	assert.True(t, setRepair[0].GetIsRepairing())
	assert.False(t, setRepair[1].GetIsRepairing())

	// Cooldown + lastAttemptCommit recorded; repairing cleared.
	rm.mu.Lock()
	defer rm.mu.Unlock()
	assert.False(t, rm.repairing["s1"], "repairing flag cleared")
	assert.False(t, rm.cooldowns["s1"].IsZero(), "cooldown set")
	assert.Equal(t, "abc123", rm.lastAttemptCommit["s1"])
}

func TestRepairSession_AlreadyExists(t *testing.T) {
	mock := newTestMock()
	mock.startErr = grpcstatus.Error(codes.AlreadyExists, "agent run already active")
	rm := newTestMonitor(mock)
	rm.repairing["s1"] = true

	rm.repairSession(t.Context(), "s1", "repo", "title",
		bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123")

	_, waitCalls, fireCalls, setRepair := mock.snapshot()
	assert.Equal(t, 0, waitCalls, "WaitChatRun must not be called when StartChatRun returned AlreadyExists")
	assert.Equal(t, 0, fireCalls, "no FIX_COMPLETE")
	assert.Empty(t, setRepair, "no SetRepairStatus when we lost the race")

	rm.mu.Lock()
	defer rm.mu.Unlock()
	assert.False(t, rm.repairing["s1"])
	assert.True(t, rm.cooldowns["s1"].IsZero(), "cooldown must NOT be recorded when we lost the race")
	assert.Equal(t, "", rm.lastAttemptCommit["s1"], "lastAttemptCommit unset because attempt did not run")
	// AlreadyExists is a soft skip: the loser must not bump the
	// session's attempt count, otherwise two plugins racing on the same
	// session would double-count and the TUI hint would lie.
	assert.Empty(t, mock.recordOutcomeReqs, "AlreadyExists must not record a repair outcome")
}

// TestRepairSession_RecordsOutcomeOnRunnerFailure asserts that a
// non-AlreadyExists StartChatRun failure (eg. "claude not on PATH")
// is captured into RecordRepairOutcome with a non-empty runner_error.
// This is the field the TUI's "⚠ repair failed" hint reads from.
func TestRepairSession_RecordsOutcomeOnRunnerFailure(t *testing.T) {
	mock := newTestMock()
	mock.startErr = grpcstatus.Error(codes.FailedPrecondition, "claude not on PATH")
	rm := newTestMonitor(mock)
	rm.repairing["s1"] = true

	rm.repairSession(t.Context(), "s1", "repo", "title",
		bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123")

	require.Len(t, mock.recordOutcomeReqs, 1, "runner failure must be recorded once")
	got := mock.recordOutcomeReqs[0]
	assert.Equal(t, "s1", got.GetSessionId())
	assert.Contains(t, got.GetRunnerError(), "claude not on PATH")
	assert.Empty(t, got.GetExitError(), "ExitError stays empty when the runner refused to spawn")
	assert.NotZero(t, got.GetStartedAtUnix(), "StartedAtUnix recorded")

	// A runner failure means the agent never started — there is no signal
	// that the session was repaired, so FIX_COMPLETE must not fire.
	_, _, fireCalls, _ := mock.snapshot()
	assert.Equal(t, 0, fireCalls, "no FIX_COMPLETE when runner refused to start")
}

// TestRepairSession_RecordsOutcomeOnAgentExitError asserts that a clean
// StartChatRun followed by a non-zero agent exit lands in ExitError, not
// RunnerError — the TUI distinguishes the two for diagnosis.
func TestRepairSession_RecordsOutcomeOnAgentExitError(t *testing.T) {
	mock := newTestMock()
	mock.waitResp = &bossanovav1.WaitChatRunHostResponse{ExitError: "exit status 1"}
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS},
	}
	rm := newTestMonitor(mock)
	rm.repairing["s1"] = true

	rm.repairSession(t.Context(), "s1", "repo", "title",
		bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123")

	require.Len(t, mock.recordOutcomeReqs, 1, "exit-error path must be recorded")
	got := mock.recordOutcomeReqs[0]
	assert.Equal(t, "exit status 1", got.GetExitError())
	assert.Empty(t, got.GetRunnerError(), "RunnerError stays empty when the agent ran")

	// A non-zero agent exit is "the agent gave up", not "the issue was
	// resolved" — FIX_COMPLETE must not fire.
	_, _, fireCalls, _ := mock.snapshot()
	assert.Equal(t, 0, fireCalls, "no FIX_COMPLETE on non-zero agent exit")
}

func TestRepairSession_RunReturnsExitError(t *testing.T) {
	mock := newTestMock()
	mock.waitResp = &bossanovav1.WaitChatRunHostResponse{ExitError: "exit status 1"}
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS},
	}
	rm := newTestMonitor(mock)
	rm.repairing["s1"] = true

	rm.repairSession(t.Context(), "s1", "repo", "title",
		bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123")

	_, _, fireCalls, setRepair := mock.snapshot()
	assert.Equal(t, 0, fireCalls, "no FIX_COMPLETE on failed run")
	require.Len(t, setRepair, 2)
	assert.False(t, setRepair[1].GetIsRepairing(), "IsRepairing cleared on exit even after failure")

	rm.mu.Lock()
	defer rm.mu.Unlock()
	assert.False(t, rm.cooldowns["s1"].IsZero(), "cooldown set when run owned even on failure")
	// A failed run (exit error) means the agent never produced a useful
	// outcome on this commit — recording the SHA would block retries
	// indefinitely until a new push, even though the next sweep should be
	// allowed to try again. The 1-minute cooldown still prevents thrash.
	assert.Equal(t, "", rm.lastAttemptCommit["s1"], "lastAttemptCommit must NOT be set when the agent exited with an error")
}

func TestRepairSession_NotInFixingChecks(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_READY_FOR_REVIEW},
	}
	rm := newTestMonitor(mock)
	rm.repairing["s1"] = true

	rm.repairSession(t.Context(), "s1", "repo", "title",
		bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123")

	_, _, fireCalls, _ := mock.snapshot()
	assert.Equal(t, 0, fireCalls, "FIX_COMPLETE only fires in FIXING_CHECKS")
}

func TestRepairSession_WaitErrorSkipsFireEvent(t *testing.T) {
	mock := newTestMock()
	mock.waitErr = errors.New("rpc broken")
	rm := newTestMonitor(mock)
	rm.repairing["s1"] = true

	rm.repairSession(t.Context(), "s1", "repo", "title",
		bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123")

	_, _, fireCalls, _ := mock.snapshot()
	assert.Equal(t, 0, fireCalls, "no FIX_COMPLETE when WaitChatRun errored")

	// A failed WaitChatRun is an infrastructure failure, not an agent
	// outcome — we have no idea whether the agent did anything useful, so
	// we must NOT blacklist this SHA. Otherwise a transient daemon hiccup
	// permanently disables repair on this commit until a new push.
	rm.mu.Lock()
	defer rm.mu.Unlock()
	assert.Equal(t, "", rm.lastAttemptCommit["s1"], "lastAttemptCommit must NOT be set when WaitChatRun errored")
}

// --- maybeRepair filtering tests ---

func TestMaybeRepair_SkipsNonRepairableStatus(t *testing.T) {
	mock := newTestMock()
	rm := newTestMonitor(mock)
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_PASSING, false)

	startCalls, _, _, _ := mock.snapshot()
	assert.Equal(t, 0, startCalls)
}

func TestMaybeRepair_TriggersForFailing(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS},
	}
	rm := newTestMonitor(mock)
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	waitFor(t, func() bool {
		c, _, _, _ := mock.snapshot()
		return c > 0
	}, "StartChatRun called")
}

func TestMaybeRepair_TriggersForConflict(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS},
	}
	rm := newTestMonitor(mock)
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)

	waitFor(t, func() bool {
		c, _, _, _ := mock.snapshot()
		return c > 0
	}, "StartChatRun called for CONFLICT")
}

func TestMaybeRepair_TriggersForRejected(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_READY_FOR_REVIEW},
	}
	rm := newTestMonitor(mock)
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED, false)

	waitFor(t, func() bool {
		c, _, _, _ := mock.snapshot()
		return c > 0
	}, "StartChatRun called for REJECTED")
}

func TestMaybeRepair_SkipsWhileStopped(t *testing.T) {
	mock := newTestMock()
	rm := newTestMonitor(mock)
	rm.stopped = true
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	startCalls, _, _, _ := mock.snapshot()
	assert.Equal(t, 0, startCalls)
}

func TestMaybeRepair_SkipsWhilePaused(t *testing.T) {
	mock := newTestMock()
	rm := newTestMonitor(mock)
	rm.paused = true
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	startCalls, _, _, _ := mock.snapshot()
	assert.Equal(t, 0, startCalls)
}

func TestMaybeRepair_SkipsDuringCooldown(t *testing.T) {
	mock := newTestMock()
	rm := newTestMonitor(mock)
	rm.cooldowns["s1"] = time.Now()
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	startCalls, _, _, _ := mock.snapshot()
	assert.Equal(t, 0, startCalls)
}

func TestMaybeRepair_SkipsWhileChatActive(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{
			Id:                 "s1",
			State:              bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
			HasActiveChat:      true, // user is mid-conversation; do not interrupt.
			LastChatActivityAt: timestamppb.New(time.Now()),
		},
	}
	rm := newTestMonitor(mock)
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	// Give the async ListSessions a beat — see SkipsSameCommit for context.
	time.Sleep(50 * time.Millisecond)
	startCalls, _, _, _ := mock.snapshot()
	assert.Equal(t, 0, startCalls, "must defer repair while chat is active")

	// And no cooldown should be recorded — the next sweep must be free to
	// re-evaluate as soon as the chat goes idle, without waiting for
	// cooldownDuration to elapse.
	rm.mu.Lock()
	defer rm.mu.Unlock()
	assert.True(t, rm.cooldowns["s1"].IsZero(), "no cooldown recorded for idle-gate skip")
}

func TestMaybeRepair_FiresWhenChatIdle(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{
			Id:            "s1",
			State:         bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
			HasActiveChat: false, // chat has gone stale (no heartbeat).
		},
	}
	rm := newTestMonitor(mock)
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	waitFor(t, func() bool {
		c, _, _, _ := mock.snapshot()
		return c > 0
	}, "StartChatRun called once chat went idle")
}

func TestMaybeRepair_SkipsSameCommit(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS, PrDisplayHeadSha: "abc123"},
	}
	rm := newTestMonitor(mock)
	rm.lastAttemptCommit["s1"] = "abc123"

	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	// Since maybeRepair now has an async path (it calls ListSessions before
	// taking the mu.Lock), give it a beat.
	time.Sleep(50 * time.Millisecond)
	startCalls, _, _, _ := mock.snapshot()
	assert.Equal(t, 0, startCalls, "must skip repair for the same commit")
}

func TestMaybeRepair_SkipsAlreadyRepairing(t *testing.T) {
	mock := newTestMock()
	rm := newTestMonitor(mock)
	rm.repairing["s1"] = true
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	startCalls, _, _, _ := mock.snapshot()
	assert.Equal(t, 0, startCalls)
}

// --- CONFLICT-aware lookupSession tests ---
//
// Conflicts are independent of plan completion: a PR can become unmergeable
// at any point in its lifecycle once main moves. The repair plugin must
// therefore allow repair from a broader state set when displayStatus is
// CONFLICT than for FAILING / REJECTED (which only make sense in the
// CI/review cycle).

// TestMaybeRepair_RepairsConflictInFinalizing covers the user-reported
// edge case: a session that ran /boss-finalize, transitioned to Finalizing,
// then later had main move under it making the PR unmergeable. Without the
// CONFLICT-aware fix this session is silently skipped indefinitely.
func TestMaybeRepair_RepairsConflictInFinalizing(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FINALIZING},
	}
	rm := newTestMonitor(mock)
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)

	waitFor(t, func() bool {
		c, _, _, _ := mock.snapshot()
		return c > 0
	}, "StartChatRun called for CONFLICT in FINALIZING")
}

// TestMaybeRepair_SkipsConflictInBlocked documents the deliberate exclusion
// of Blocked sessions from auto-repair. Blocked is a terminal state that
// requires manual `boss unblock` to leave; auto-repair would defeat that.
func TestMaybeRepair_SkipsConflictInBlocked(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_BLOCKED},
	}
	rm := newTestMonitor(mock)
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)

	time.Sleep(50 * time.Millisecond) // give async ListSessions a beat
	startCalls, _, _, _ := mock.snapshot()
	assert.Equal(t, 0, startCalls, "must not auto-repair Blocked sessions")
}

// TestMaybeRepair_SkipsConflictInImplementingPlan documents the deliberate
// exclusion of mid-implementation sessions. The idle-chat heuristic owns
// "should we interrupt?" for ImplementingPlan; auto-repair on conflict would
// trample a user mid-conversation.
func TestMaybeRepair_SkipsConflictInImplementingPlan(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_IMPLEMENTING_PLAN},
	}
	rm := newTestMonitor(mock)
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)

	time.Sleep(50 * time.Millisecond)
	startCalls, _, _, _ := mock.snapshot()
	assert.Equal(t, 0, startCalls, "must not auto-repair ImplementingPlan sessions")
}

// TestMaybeRepair_SkipsFailingInFinalizing pins the asymmetry: FAILING is
// only meaningful in the CI/review cycle (CI runs there), so Finalizing +
// FAILING stays excluded even though Finalizing + CONFLICT is allowed.
func TestMaybeRepair_SkipsFailingInFinalizing(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FINALIZING},
	}
	rm := newTestMonitor(mock)
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	time.Sleep(50 * time.Millisecond)
	startCalls, _, _, _ := mock.snapshot()
	assert.Equal(t, 0, startCalls, "FAILING must remain CI/review-cycle only")
}

// --- Shutdown drains in-flight repairs ---

func TestShutdown_WaitsForInflightRepair(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS},
	}

	// Block WaitChatRun until we cancel the test ctx.
	done := make(chan struct{})
	mock.waitFunc = func(ctx context.Context, _ *bossanovav1.WaitChatRunHostRequest) (*bossanovav1.WaitChatRunHostResponse, error) {
		select {
		case <-ctx.Done():
		case <-done:
		}
		return &bossanovav1.WaitChatRunHostResponse{}, nil
	}

	rm := newTestMonitor(mock)

	// Trigger a repair and let the goroutine block inside WaitChatRun.
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)
	waitFor(t, func() bool {
		c, _, _, _ := mock.snapshot()
		return c > 0
	}, "StartChatRun called")

	// Shutdown with a generous timeout. Cancellation of the workflow ctx
	// causes WaitChatRun (via ctx.Done) to return, draining the goroutine.
	close(done) // belt-and-braces: also unblock via the override channel
	rm.Shutdown(2 * time.Second)
}

func TestMaybeRepair_DefersWhenChatActiveButTimestampMissing(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{
			Id:            "s1",
			State:         bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
			HasActiveChat: true, // claims active chat…
			// LastChatActivityAt deliberately omitted (nil) — old daemon path.
		},
	}
	rm := newTestMonitor(mock)
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	// Same async beat as the other defer tests.
	time.Sleep(50 * time.Millisecond)
	startCalls, _, _, _ := mock.snapshot()
	assert.Equal(t, 0, startCalls, "must defer when chat is active but activity timestamp is unknown (fail-closed)")

	// And no cooldown recorded — the next sweep must be free to retry.
	rm.mu.Lock()
	defer rm.mu.Unlock()
	assert.True(t, rm.cooldowns["s1"].IsZero(), "no cooldown recorded for fail-closed defer")
}

func TestMaybeRepair_DefersWhenChatActiveAndRecent(t *testing.T) {
	mock := newTestMock()
	recent := timestamppb.New(time.Now().Add(-30 * time.Second))
	mock.sessions = []*bossanovav1.Session{
		{
			Id:                 "s1",
			State:              bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
			HasActiveChat:      true,
			LastChatActivityAt: recent,
		},
	}
	rm := newTestMonitor(mock)
	// Default 5m threshold; 30s ago is well within it.
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	time.Sleep(50 * time.Millisecond)
	startCalls, _, _, _ := mock.snapshot()
	assert.Equal(t, 0, startCalls, "must defer when chat is active AND output is recent")

	rm.mu.Lock()
	defer rm.mu.Unlock()
	assert.True(t, rm.cooldowns["s1"].IsZero(), "no cooldown recorded for idle-gate skip")
}

func TestMaybeRepair_FiresWhenChatActiveButQuietPastThreshold(t *testing.T) {
	mock := newTestMock()
	stale := timestamppb.New(time.Now().Add(-10 * time.Minute))
	mock.sessions = []*bossanovav1.Session{
		{
			Id:                 "s1",
			State:              bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
			HasActiveChat:      true,  // chat process is attached
			LastChatActivityAt: stale, // …but it has not produced output in 10 minutes
		},
	}
	rm := newTestMonitor(mock)
	// Default threshold is 5m; 10m of silence > threshold → fire.
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	waitFor(t, func() bool {
		c, _, _, _ := mock.snapshot()
		return c > 0
	}, "StartChatRun should fire when chat has been idle past threshold")
}

func TestMaybeRepair_RespectsCustomIdleThreshold(t *testing.T) {
	mock := newTestMock()
	// 90s ago — past 1m custom threshold but well under default 5m.
	activity := timestamppb.New(time.Now().Add(-90 * time.Second))
	mock.sessions = []*bossanovav1.Session{
		{
			Id:                 "s1",
			State:              bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
			HasActiveChat:      true,
			LastChatActivityAt: activity,
		},
	}
	rm := newTestMonitor(mock)
	rm.config = &repairConfig{IdleRepairThresholdMinutes: 1}
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	waitFor(t, func() bool {
		c, _, _, _ := mock.snapshot()
		return c > 0
	}, "StartChatRun should fire when chat idle past custom 1-minute threshold")
}
