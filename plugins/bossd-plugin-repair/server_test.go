package main

import (
	"context"
	"errors"
	"fmt"
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

	sessions         []*bossanovav1.Session
	sessionSequences [][]*bossanovav1.Session
	sessionSeqIndex  int
	listSessErr      error
	listSessCalls    int

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

	reclaimResp *bossanovav1.ReclaimRepairChatHostResponse
	reclaimErr  error
	reclaimReqs []*bossanovav1.ReclaimRepairChatHostRequest

	fireEventCalls int
	fireEventReqs  []*bossanovav1.FireSessionEventRequest

	setRepairStatusReqs []*bossanovav1.SetRepairStatusRequest

	recordOutcomeReqs []*bossanovav1.RecordRepairOutcomeRequest

	reviewCommentsResp *bossanovav1.GetReviewCommentsResponse
	reviewCommentsErr  error
}

var _ hostClient = (*mockHostClient)(nil)

func newTestMock() *mockHostClient {
	return &mockHostClient{
		startResp:   &bossanovav1.StartChatRunHostResponse{AgentSessionId: "claude-1"},
		waitResp:    &bossanovav1.WaitChatRunHostResponse{},
		reclaimResp: &bossanovav1.ReclaimRepairChatHostResponse{Reclaimed: true, TmuxSessionName: "boss-repair-stale"},
	}
}

func (m *mockHostClient) ListSessions(_ context.Context) (*bossanovav1.HostServiceListSessionsResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listSessCalls++
	if m.listSessErr != nil {
		return nil, m.listSessErr
	}
	if len(m.sessionSequences) > 0 {
		idx := m.sessionSeqIndex
		if idx >= len(m.sessionSequences) {
			idx = len(m.sessionSequences) - 1
		}
		m.sessionSeqIndex++
		return &bossanovav1.HostServiceListSessionsResponse{Sessions: m.sessionSequences[idx]}, nil
	}
	return &bossanovav1.HostServiceListSessionsResponse{Sessions: m.sessions}, nil
}

func (m *mockHostClient) GetReviewComments(_ context.Context, _ *bossanovav1.GetReviewCommentsRequest) (*bossanovav1.GetReviewCommentsResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reviewCommentsErr != nil {
		return nil, m.reviewCommentsErr
	}
	if m.reviewCommentsResp != nil {
		return m.reviewCommentsResp, nil
	}
	return &bossanovav1.GetReviewCommentsResponse{}, nil
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

func (m *mockHostClient) ReclaimRepairChat(_ context.Context, req *bossanovav1.ReclaimRepairChatHostRequest) (*bossanovav1.ReclaimRepairChatHostResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reclaimReqs = append(m.reclaimReqs, req)
	if m.reclaimErr != nil {
		return nil, m.reclaimErr
	}
	return m.reclaimResp, nil
}

func (m *mockHostClient) snapshot() (startCalls, waitCalls, fireCalls int, setRepair []*bossanovav1.SetRepairStatusRequest) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*bossanovav1.SetRepairStatusRequest, len(m.setRepairStatusReqs))
	copy(out, m.setRepairStatusReqs)
	return m.startCalls, m.waitCalls, m.fireEventCalls, out
}

func (m *mockHostClient) startRequestsSnapshot() []*bossanovav1.StartChatRunHostRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*bossanovav1.StartChatRunHostRequest, len(m.startReqs))
	copy(out, m.startReqs)
	return out
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
		bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123", "", time.Time{}, "", true)

	startCalls, waitCalls, fireCalls, setRepair := mock.snapshot()
	require.Equal(t, 1, startCalls, "StartChatRun called once")
	require.Equal(t, 1, waitCalls, "WaitChatRun called once")
	require.Equal(t, 1, fireCalls, "FIX_COMPLETE fired in FIXING_CHECKS")
	require.Equal(t, "boss-repair", mock.startReqs[0].GetCommand())
	require.Empty(t, mock.startReqs[0].GetPrompt())
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

func TestRepairSession_WaitsForChecksAndStopsWhenClean(t *testing.T) {
	mock := newTestMock()
	mock.sessionSequences = [][]*bossanovav1.Session{
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS, DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_CHECKING}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_READY_FOR_REVIEW, DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_PASSING}},
	}
	rm := newTestMonitor(mock)
	rm.config = &repairConfig{PostRepairPollMilliseconds: 1, PostRepairWaitMilliseconds: 100}

	rm.repairSession(context.Background(), "s1", "repo", "session-name", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123", "", time.Time{}, "", false)

	startCalls, waitCalls, fireCalls, setRepair := mock.snapshot()
	require.Equal(t, 1, startCalls)
	require.Equal(t, 1, waitCalls)
	require.Equal(t, 1, fireCalls)
	require.Len(t, setRepair, 2)
	mock.mu.Lock()
	require.Equal(t, 3, mock.listSessCalls)
	require.Equal(t, 3, mock.sessionSeqIndex)
	mock.mu.Unlock()
}

func TestRepairSession_StartsSecondFreshRunWhenChecksFailAfterPush(t *testing.T) {
	mock := newTestMock()
	mock.sessionSequences = [][]*bossanovav1.Session{
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS, DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_CHECKING}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS, DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, DisplayHasFailures: true, PrDisplayHeadSha: "def456"}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS, PrDisplayHeadSha: "def456"}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_READY_FOR_REVIEW, DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_PASSING, PrDisplayHeadSha: "def456"}},
	}
	rm := newTestMonitor(mock)
	rm.config = &repairConfig{PostRepairPollMilliseconds: 1, PostRepairWaitMilliseconds: 100}

	rm.repairSession(context.Background(), "s1", "repo", "session-name", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123", "", time.Time{}, "", true)

	startCalls, waitCalls, fireCalls, _ := mock.snapshot()
	require.Equal(t, 2, startCalls)
	require.Equal(t, 2, waitCalls)
	require.Equal(t, 2, fireCalls)
}

// TestRepairSession_ResumeStartErrorStopsCleanly locks in the resume fallback
// guarantee: when a resumed iteration's StartChatRun returns a
// non-AlreadyExists error (the daemon could not honor the run), runRepairAttempt
// returns ("", false), the loop's `if !ok { return }` ends it, and no further
// attempts run. The periodic sweep later re-triggers a fresh repairSession from
// attempt 1 with an empty resume target.
func TestRepairSession_ResumeStartErrorStopsCleanly(t *testing.T) {
	mock := newTestMock()
	// Same two-iteration harness as StartsSecondFreshRunWhenChecksFailAfterPush:
	// attempt 1 runs, checks fail on a new head, so attempt 2 would start.
	mock.sessionSequences = [][]*bossanovav1.Session{
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS, DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_CHECKING}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS, DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, DisplayHasFailures: true, PrDisplayHeadSha: "def456"}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS, PrDisplayHeadSha: "def456"}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_READY_FOR_REVIEW, DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_PASSING, PrDisplayHeadSha: "def456"}},
	}
	var n int
	mock.startFunc = func(*bossanovav1.StartChatRunHostRequest) (*bossanovav1.StartChatRunHostResponse, error) {
		n++
		if n == 1 {
			return &bossanovav1.StartChatRunHostResponse{AgentSessionId: "agent-1"}, nil
		}
		// Attempt 2 is the resumed run; the daemon refuses it with a
		// non-AlreadyExists error (eg. an internal failure).
		return nil, grpcstatus.Error(codes.Internal, "resume failed")
	}
	rm := newTestMonitor(mock)
	rm.repairing["s1"] = true
	rm.config = &repairConfig{PostRepairPollMilliseconds: 1, PostRepairWaitMilliseconds: 100}

	rm.repairSession(context.Background(), "s1", "repo", "session-name", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123", "", time.Time{}, "", true)

	startCalls, waitCalls, _, _ := mock.snapshot()
	// Attempt 1 started + the failed attempt 2 start; the loop stopped after the
	// non-AlreadyExists error, so there is no third start.
	require.Equal(t, 2, startCalls, "StartChatRun called exactly twice (no third attempt)")
	// Only attempt 1 reached WaitChatRun; the failed start never waits.
	require.Equal(t, 1, waitCalls, "WaitChatRun called once")

	// The deferred cleanup cleared the repairing flag — the loop returned
	// cleanly rather than leaking the session as in-repair.
	rm.mu.Lock()
	assert.False(t, rm.repairing["s1"], "repairing flag cleared")
	rm.mu.Unlock()

	// The daemon-refusal path sets outcomeRunnerError + outcomeShouldRecord, so
	// the failed start records an outcome with a non-empty runner error.
	mock.mu.Lock()
	require.NotEmpty(t, mock.recordOutcomeReqs, "an outcome was recorded for the failed start")
	last := mock.recordOutcomeReqs[len(mock.recordOutcomeReqs)-1]
	mock.mu.Unlock()
	assert.NotEmpty(t, last.GetRunnerError(), "failed resume start records a runner error")
}

func TestRepairStartChatRunRequest_SetsResumeTarget(t *testing.T) {
	got := repairStartChatRunRequest("sess", "demo", "", time.Time{}, "agent-prior")
	if got.GetResumeAgentSessionId() != "agent-prior" {
		t.Errorf("ResumeAgentSessionId = %q, want %q", got.GetResumeAgentSessionId(), "agent-prior")
	}
	fresh := repairStartChatRunRequest("sess", "demo", "", time.Time{}, "")
	if fresh.GetResumeAgentSessionId() != "" {
		t.Errorf("fresh ResumeAgentSessionId = %q, want empty", fresh.GetResumeAgentSessionId())
	}
}

func TestRepairSession_SecondAttemptResumesFirstSession(t *testing.T) {
	mock := newTestMock()
	mock.sessionSequences = [][]*bossanovav1.Session{
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS, DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_CHECKING}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS, DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, DisplayHasFailures: true, PrDisplayHeadSha: "def456"}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS, PrDisplayHeadSha: "def456"}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_READY_FOR_REVIEW, DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_PASSING, PrDisplayHeadSha: "def456"}},
	}
	var n int
	mock.startFunc = func(*bossanovav1.StartChatRunHostRequest) (*bossanovav1.StartChatRunHostResponse, error) {
		n++
		return &bossanovav1.StartChatRunHostResponse{AgentSessionId: fmt.Sprintf("agent-%d", n)}, nil
	}
	mock.waitResp = &bossanovav1.WaitChatRunHostResponse{ProviderSessionId: "provider-1"}
	rm := newTestMonitor(mock)
	rm.config = &repairConfig{PostRepairPollMilliseconds: 1, PostRepairWaitMilliseconds: 100}

	rm.repairSession(context.Background(), "s1", "repo", "session-name", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123", "", time.Time{}, "", true)

	reqs := mock.startRequestsSnapshot()
	if len(reqs) != 2 {
		t.Fatalf("StartChatRun calls = %d, want 2", len(reqs))
	}
	if reqs[0].GetResumeAgentSessionId() != "" {
		t.Errorf("attempt 1 resume = %q, want empty (fresh start)", reqs[0].GetResumeAgentSessionId())
	}
	if reqs[1].GetResumeAgentSessionId() != "provider-1" {
		t.Errorf("attempt 2 resume = %q, want %q (the first attempt's provider id)", reqs[1].GetResumeAgentSessionId(), "provider-1")
	}
}

func TestRepairResumeSessionIDFallsBackToAgentSessionID(t *testing.T) {
	if got := repairResumeSessionID("agent-1", &bossanovav1.WaitChatRunHostResponse{}); got != "agent-1" {
		t.Fatalf("resume id = %q, want agent-1", got)
	}
	if got := repairResumeSessionID("agent-1", nil); got != "agent-1" {
		t.Fatalf("nil response resume id = %q, want agent-1", got)
	}
}

func TestRepairSession_DoesNotStartSecondFreshRunWhenChecksFailOnSameInput(t *testing.T) {
	mock := newTestMock()
	mock.sessionSequences = [][]*bossanovav1.Session{
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS, PrDisplayHeadSha: "abc123"}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS, DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_CHECKING, PrDisplayHeadSha: "abc123"}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS, DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, DisplayHasFailures: true, PrDisplayHeadSha: "abc123"}},
	}
	rm := newTestMonitor(mock)
	rm.config = &repairConfig{PostRepairPollMilliseconds: 1, PostRepairWaitMilliseconds: 100}

	// These sessions carry no PR origin/number, so the review fingerprint is
	// unavailable — exactly what maybeRepair computes for an origin-less session.
	// The baseline availability must match what the wait loop recomputes, or the
	// duplicate-input guard would see availability flip and re-run.
	rm.repairSession(context.Background(), "s1", "repo", "session-name", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123", "", time.Time{}, "", false)

	startCalls, waitCalls, fireCalls, _ := mock.snapshot()
	require.Equal(t, 1, startCalls)
	require.Equal(t, 1, waitCalls)
	require.Equal(t, 1, fireCalls)
}

func TestRepairSession_WaitsThroughStaleSameInputBeforeRetryingNewHead(t *testing.T) {
	mock := newTestMock()
	mock.sessionSequences = [][]*bossanovav1.Session{
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS, PrDisplayHeadSha: "abc123"}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS, DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, DisplayHasFailures: true, PrDisplayHeadSha: "abc123"}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS, DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_CHECKING, PrDisplayHeadSha: "abc123"}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS, DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, DisplayHasFailures: true, PrDisplayHeadSha: "def456"}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS, PrDisplayHeadSha: "def456"}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_READY_FOR_REVIEW, DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_PASSING, PrDisplayHeadSha: "def456"}},
	}
	rm := newTestMonitor(mock)
	rm.config = &repairConfig{PostRepairPollMilliseconds: 1, PostRepairWaitMilliseconds: 100}

	rm.repairSession(context.Background(), "s1", "repo", "session-name", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123", "", time.Time{}, "", false)

	startCalls, waitCalls, fireCalls, _ := mock.snapshot()
	require.Equal(t, 2, startCalls)
	require.Equal(t, 2, waitCalls)
	require.Equal(t, 2, fireCalls)
}

func TestRepairSession_StartsSecondFreshRunWhenConflictAppearsAfterPush(t *testing.T) {
	mock := newTestMock()
	mock.sessionSequences = [][]*bossanovav1.Session{
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS, DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_CHECKING}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS, DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, PrDisplayHeadSha: "def456"}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS, PrDisplayHeadSha: "def456"}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_READY_FOR_REVIEW, DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_PASSING, PrDisplayHeadSha: "def456"}},
	}
	rm := newTestMonitor(mock)
	rm.config = &repairConfig{PostRepairPollMilliseconds: 1, PostRepairWaitMilliseconds: 100}

	rm.repairSession(context.Background(), "s1", "repo", "session-name", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false, "abc123", "", time.Time{}, "", true)

	startCalls, waitCalls, fireCalls, _ := mock.snapshot()
	require.Equal(t, 2, startCalls)
	require.Equal(t, 2, waitCalls)
	require.Equal(t, 2, fireCalls)
}

func TestRepairSession_StartsSecondFreshRunWhenReviewFingerprintChangesAfterPush(t *testing.T) {
	mock := newTestMock()
	path := "x.go"
	line := int32(12)
	prNumber := int32(123)
	mock.sessionSequences = [][]*bossanovav1.Session{
		{{
			Id:               "s1",
			State:            bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
			RepoOriginUrl:    "git@github.com:recurser/bossanova.git",
			PrNumber:         &prNumber,
			PrDisplayHeadSha: "abc123",
		}},
		{{
			Id:               "s1",
			State:            bossanovav1.SessionState_SESSION_STATE_READY_FOR_REVIEW,
			DisplayStatus:    bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED,
			RepoOriginUrl:    "git@github.com:recurser/bossanova.git",
			PrNumber:         &prNumber,
			PrDisplayHeadSha: "def456",
		}},
		{{
			Id:               "s1",
			State:            bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
			RepoOriginUrl:    "git@github.com:recurser/bossanova.git",
			PrNumber:         &prNumber,
			PrDisplayHeadSha: "def456",
		}},
		{{
			Id:               "s1",
			State:            bossanovav1.SessionState_SESSION_STATE_READY_FOR_REVIEW,
			DisplayStatus:    bossanovav1.DisplayStatus_DISPLAY_STATUS_PASSING,
			RepoOriginUrl:    "git@github.com:recurser/bossanova.git",
			PrNumber:         &prNumber,
			PrDisplayHeadSha: "def456",
		}},
	}
	mock.reviewCommentsResp = &bossanovav1.GetReviewCommentsResponse{
		Comments: []*bossanovav1.ReviewComment{
			{Author: "reviewer", Body: "please fix the new failure", Path: &path, Line: &line, State: bossanovav1.ReviewState_REVIEW_STATE_CHANGES_REQUESTED},
		},
	}
	rm := newTestMonitor(mock)
	rm.config = &repairConfig{PostRepairPollMilliseconds: 1, PostRepairWaitMilliseconds: 100}

	rm.repairSession(context.Background(), "s1", "repo", "session-name", bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED, false, "abc123", "", time.Time{}, "old-review-fingerprint", true)

	startCalls, waitCalls, _, _ := mock.snapshot()
	require.Equal(t, 2, startCalls)
	require.Equal(t, 2, waitCalls)
}

// A reviewer (or PR agent) can leave a new actionable comment without the host
// flipping the PR to REJECTED, so a green/PASSING PR can still carry fresh
// feedback. The loop must notice the changed review fingerprint and start a
// second repair run rather than declaring the PR clean and handing off.
func TestRepairSession_StartsSecondFreshRunWhenNewReviewCommentOnGreenPR(t *testing.T) {
	mock := newTestMock()
	path := "x.go"
	line := int32(7)
	prNumber := int32(99)
	const origin = "git@github.com:recurser/bossanova.git"
	green := &bossanovav1.Session{
		Id:               "s1",
		State:            bossanovav1.SessionState_SESSION_STATE_READY_FOR_REVIEW,
		DisplayStatus:    bossanovav1.DisplayStatus_DISPLAY_STATUS_PASSING,
		RepoOriginUrl:    origin,
		PrNumber:         &prNumber,
		PrDisplayHeadSha: "abc123",
	}
	checking := &bossanovav1.Session{
		Id:               "s1",
		State:            bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
		RepoOriginUrl:    origin,
		PrNumber:         &prNumber,
		PrDisplayHeadSha: "abc123",
	}
	mock.sessionSequences = [][]*bossanovav1.Session{
		{checking}, // attempt 1: runRepairAttempt FIX_COMPLETE state check
		{green},    // attempt 1: wait — PASSING, but a new comment exists -> needs repair
		{checking}, // attempt 2: runRepairAttempt FIX_COMPLETE state check
		{green},    // attempt 2: wait — PASSING, comment now matches baseline -> clean -> stop
	}
	// A single new comment whose fingerprint differs from the baseline passed in
	// below, so the first post-repair assessment sees changed feedback.
	mock.reviewCommentsResp = &bossanovav1.GetReviewCommentsResponse{
		Comments: []*bossanovav1.ReviewComment{
			{Author: "reviewer", Body: "address this", Path: &path, Line: &line, State: bossanovav1.ReviewState_REVIEW_STATE_CHANGES_REQUESTED},
		},
	}
	rm := newTestMonitor(mock)
	rm.config = &repairConfig{PostRepairPollMilliseconds: 1, PostRepairWaitMilliseconds: 100}

	rm.repairSession(context.Background(), "s1", "repo", "session-name",
		bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123", "", time.Time{}, "baseline-fingerprint", true)

	startCalls, waitCalls, _, _ := mock.snapshot()
	require.Equal(t, 2, startCalls, "new review comment on a green PR must start a second repair run")
	require.Equal(t, 2, waitCalls)
}

func TestRepairSession_StopsBeforeSecondRunWhenPaused(t *testing.T) {
	mock := newTestMock()
	path := "x.go"
	line := int32(7)
	prNumber := int32(99)
	const origin = "git@github.com:recurser/bossanova.git"
	green := &bossanovav1.Session{
		Id:               "s1",
		State:            bossanovav1.SessionState_SESSION_STATE_READY_FOR_REVIEW,
		DisplayStatus:    bossanovav1.DisplayStatus_DISPLAY_STATUS_PASSING,
		RepoOriginUrl:    origin,
		PrNumber:         &prNumber,
		PrDisplayHeadSha: "abc123",
	}
	checking := &bossanovav1.Session{
		Id:               "s1",
		State:            bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
		RepoOriginUrl:    origin,
		PrNumber:         &prNumber,
		PrDisplayHeadSha: "abc123",
	}
	mock.sessionSequences = [][]*bossanovav1.Session{
		{checking},
		{green},
	}
	mock.reviewCommentsResp = &bossanovav1.GetReviewCommentsResponse{
		Comments: []*bossanovav1.ReviewComment{
			{Author: "reviewer", Body: "address this", Path: &path, Line: &line, State: bossanovav1.ReviewState_REVIEW_STATE_CHANGES_REQUESTED},
		},
	}
	rm := newTestMonitor(mock)
	rm.config = &repairConfig{PostRepairPollMilliseconds: 1, PostRepairWaitMilliseconds: 100}
	mock.waitFunc = func(_ context.Context, _ *bossanovav1.WaitChatRunHostRequest) (*bossanovav1.WaitChatRunHostResponse, error) {
		rm.mu.Lock()
		rm.paused = true
		rm.mu.Unlock()
		return &bossanovav1.WaitChatRunHostResponse{}, nil
	}

	rm.repairSession(context.Background(), "s1", "repo", "session-name",
		bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123", "", time.Time{}, "baseline-fingerprint", true)

	startCalls, waitCalls, _, _ := mock.snapshot()
	require.Equal(t, 1, startCalls, "paused workflow must not start a second repair run")
	require.Equal(t, 1, waitCalls)
}

// The loop is the primary infinite-loop guardrail: if every post-repair
// assessment reports a fresh failure (changing head SHA each time), the
// duplicate-input guard never trips and only the maxRepairLoopAttempts cap
// stops it.
func TestRepairSession_StopsAtMaxRepairLoopAttempts(t *testing.T) {
	mock := newTestMock()
	// Two sessions per attempt: the FIX_COMPLETE state check then the
	// post-repair wait. Each wait reports FAILING with a brand-new head SHA, so
	// postRepairInputChanged stays true on every iteration.
	seqs := make([][]*bossanovav1.Session, 0, 2*maxRepairLoopAttempts)
	for i := range 2 * maxRepairLoopAttempts {
		seqs = append(seqs, []*bossanovav1.Session{{
			Id:                 "s1",
			State:              bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
			DisplayStatus:      bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING,
			DisplayHasFailures: true,
			PrDisplayHeadSha:   fmt.Sprintf("sha-%d", i),
		}})
	}
	mock.sessionSequences = seqs
	rm := newTestMonitor(mock)
	rm.config = &repairConfig{PostRepairPollMilliseconds: 1, PostRepairWaitMilliseconds: 100}

	rm.repairSession(context.Background(), "s1", "repo", "session-name",
		bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "sha-init", "", time.Time{}, "", false)

	startCalls, _, _, _ := mock.snapshot()
	require.Equal(t, maxRepairLoopAttempts, startCalls, "loop must stop at the max-attempts cap")
}

// When checks never settle (perpetually CHECKING), the wait hits its timeout and
// reports pending. The loop must give up after the single attempt rather than
// spinning or starting another run.
func TestRepairSession_StopsWhenChecksNeverSettle(t *testing.T) {
	mock := newTestMock()
	mock.sessionSequences = [][]*bossanovav1.Session{
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS}},
		{{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS, DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_CHECKING}},
	}
	rm := newTestMonitor(mock)
	// Short timeout, shorter poll: the wait polls the still-CHECKING PR a few
	// times then exits on DeadlineExceeded.
	rm.config = &repairConfig{PostRepairPollMilliseconds: 5, PostRepairWaitMilliseconds: 25}

	rm.repairSession(context.Background(), "s1", "repo", "session-name",
		bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123", "", time.Time{}, "", false)

	startCalls, _, _, _ := mock.snapshot()
	require.Equal(t, 1, startCalls, "a perpetually-pending PR must not spawn a second repair run")
}

func TestRepairSession_AlreadyExists(t *testing.T) {
	mock := newTestMock()
	mock.startErr = grpcstatus.Error(codes.AlreadyExists, "agent run already active")
	rm := newTestMonitor(mock)
	rm.repairing["s1"] = true

	rm.repairSession(t.Context(), "s1", "repo", "title",
		bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123", "", time.Time{}, "", true)

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
	assert.Empty(t, mock.reclaimReqs, "AlreadyExists without agent_session_id is an in-process race loss, not reclaimable")
}

func TestRepairSession_AlreadyExistsAgentIDReclaimRefusedSoftSkips(t *testing.T) {
	mock := newTestMock()
	mock.startErr = grpcstatus.Error(codes.AlreadyExists, "tmux chat already active for session s1 (agent_session_id=active-agent-1)")
	mock.reclaimErr = grpcstatus.Error(codes.FailedPrecondition, "reclaim repair chat: repair chat active or not stale")

	rm := newTestMonitor(mock)
	rm.repairing["s1"] = true

	rm.repairSession(t.Context(), "s1", "repo", "title",
		bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED, false, "abc123", "", time.Time{}, "", true)

	startCalls, waitCalls, fireCalls, setRepair := mock.snapshot()
	require.Equal(t, 1, startCalls)
	require.Equal(t, 0, waitCalls)
	require.Equal(t, 0, fireCalls)
	require.Empty(t, setRepair, "losing/refused reclaim path must not set repair status")
	require.Len(t, mock.reclaimReqs, 1)
	require.Equal(t, "active-agent-1", mock.reclaimReqs[0].GetAgentSessionId())
	require.Empty(t, mock.recordOutcomeReqs, "refused reclaim is a soft skip, not a failed repair attempt")

	rm.mu.Lock()
	defer rm.mu.Unlock()
	assert.False(t, rm.repairing["s1"])
	assert.True(t, rm.cooldowns["s1"].IsZero(), "soft skip must not start cooldown")
	assert.Equal(t, "", rm.lastAttemptCommit["s1"], "soft skip did not run an agent attempt")
}

func TestRepairSession_AlreadyExistsAgentIDReclaimSuccessRetriesOnce(t *testing.T) {
	mock := newTestMock()
	calls := 0
	mock.startFunc = func(req *bossanovav1.StartChatRunHostRequest) (*bossanovav1.StartChatRunHostResponse, error) {
		calls++
		if calls == 1 {
			return nil, grpcstatus.Error(codes.AlreadyExists, "tmux chat already active for session s1 (agent_session_id=stale-agent-1)")
		}
		return &bossanovav1.StartChatRunHostResponse{AgentSessionId: "fresh-agent-1"}, nil
	}
	mock.reclaimResp = &bossanovav1.ReclaimRepairChatHostResponse{
		Reclaimed:       true,
		TmuxSessionName: "boss-stale-repair",
	}
	rm := newTestMonitor(mock)
	rm.repairing["s1"] = true

	rm.repairSession(t.Context(), "s1", "repo", "title",
		bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED, false, "abc123", "", time.Time{}, "", true)

	startCalls, waitCalls, _, setRepair := mock.snapshot()
	require.Equal(t, 2, startCalls)
	require.Equal(t, 1, waitCalls)
	require.Len(t, mock.reclaimReqs, 1)
	require.Equal(t, "s1", mock.reclaimReqs[0].GetSessionId())
	require.Equal(t, "stale-agent-1", mock.reclaimReqs[0].GetAgentSessionId())
	require.Contains(t, mock.reclaimReqs[0].GetReason(), "daemon validated stale repair chat")
	require.NotEmpty(t, setRepair)
	require.Len(t, mock.recordOutcomeReqs, 1)
	require.Equal(t, "fresh-agent-1", mock.recordOutcomeReqs[0].GetAgentSessionId())
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
		bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123", "", time.Time{}, "", true)

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
		bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123", "", time.Time{}, "", true)

	require.Len(t, mock.recordOutcomeReqs, 1, "exit-error path must be recorded")
	got := mock.recordOutcomeReqs[0]
	assert.Equal(t, "exit status 1", got.GetExitError())
	assert.Empty(t, got.GetRunnerError(), "RunnerError stays empty when the agent ran")
	assert.Equal(t, "abc123", got.GetHeadSha())
	assert.Equal(t, bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, got.GetDisplayStatus())

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
		bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123", "", time.Time{}, "", true)

	_, _, fireCalls, setRepair := mock.snapshot()
	assert.Equal(t, 0, fireCalls, "no FIX_COMPLETE on failed run")
	require.Len(t, setRepair, 2)
	assert.False(t, setRepair[1].GetIsRepairing(), "IsRepairing cleared on exit even after failure")

	rm.mu.Lock()
	defer rm.mu.Unlock()
	assert.False(t, rm.cooldowns["s1"].IsZero(), "cooldown set when run owned even on failure")
	assert.Equal(t, "abc123", rm.lastAttemptCommit["s1"], "agent exit failures count as an attempted repair for this head")
	assert.Equal(t, bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, rm.lastAttemptDisplayStatus["s1"])
}

func TestRepairSession_WaitChatRunTimeoutDoesNotRecordAttemptedHead(t *testing.T) {
	t.Parallel()

	mock := newTestMock()
	mock.waitResp = &bossanovav1.WaitChatRunHostResponse{
		ExitError: "interactive chat did not report completion before wait deadline: context deadline exceeded",
	}
	rm := newTestMonitor(mock)
	rm.repairing["s1"] = true

	rm.repairSession(t.Context(), "s1", "repo", "title",
		bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123", "", time.Time{}, "", true)

	rm.mu.Lock()
	lastAttemptCommit := rm.lastAttemptCommit["s1"]
	lastAttemptStatus := rm.lastAttemptDisplayStatus["s1"]
	rm.mu.Unlock()

	assert.Equal(t, "", lastAttemptCommit, "timeout must not mark commit as attempted")
	assert.Equal(t, bossanovav1.DisplayStatus_DISPLAY_STATUS_UNSPECIFIED, lastAttemptStatus, "timeout must not mark status as attempted")
	require.Len(t, mock.recordOutcomeReqs, 1)
	assert.Contains(t, mock.recordOutcomeReqs[0].GetExitError(), "interactive chat did not report completion")
}

func TestRepairSession_NotInFixingChecks(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_READY_FOR_REVIEW},
	}
	rm := newTestMonitor(mock)
	rm.repairing["s1"] = true

	rm.repairSession(t.Context(), "s1", "repo", "title",
		bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123", "", time.Time{}, "", true)

	_, _, fireCalls, _ := mock.snapshot()
	assert.Equal(t, 0, fireCalls, "FIX_COMPLETE only fires in FIXING_CHECKS")
}

func TestRepairSession_WaitErrorSkipsFireEvent(t *testing.T) {
	mock := newTestMock()
	mock.waitErr = errors.New("rpc broken")
	rm := newTestMonitor(mock)
	rm.repairing["s1"] = true

	rm.repairSession(t.Context(), "s1", "repo", "title",
		bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true, "abc123", "", time.Time{}, "", true)

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

func TestRepairSession_DoesNotPersistUnavailableReviewFingerprint(t *testing.T) {
	mock := newTestMock()
	rm := newTestMonitor(mock)
	rm.repairing["s1"] = true

	rm.repairSession(t.Context(), "s1", "repo", "title",
		bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED, false, "abc123", "", time.Time{}, "", false)

	require.Len(t, mock.recordOutcomeReqs, 1)
	assert.Nil(t, mock.recordOutcomeReqs[0].ReviewFingerprint, "unavailable fingerprint must not be persisted as an empty fingerprint")
}

func TestAssessPostRepairStatus_ClassifiesDisplayStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		session    *bossanovav1.Session
		wantStatus postRepairStatus
		wantReason string
	}{
		{
			name: "checking is pending",
			session: &bossanovav1.Session{
				Id:            "s1",
				DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_CHECKING,
			},
			wantStatus: postRepairStatusPending,
			wantReason: "checks still running",
		},
		{
			name: "passing is clean",
			session: &bossanovav1.Session{
				Id:            "s1",
				DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_PASSING,
			},
			wantStatus: postRepairStatusClean,
			wantReason: "checks passed",
		},
		{
			name: "approved is clean",
			session: &bossanovav1.Session{
				Id:            "s1",
				DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_APPROVED,
			},
			wantStatus: postRepairStatusClean,
			wantReason: "checks passed",
		},
		{
			name: "failing needs another repair",
			session: &bossanovav1.Session{
				Id:                 "s1",
				DisplayStatus:      bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING,
				DisplayHasFailures: true,
			},
			wantStatus: postRepairStatusNeedsRepair,
			wantReason: "checks failed",
		},
		{
			name: "conflict needs another repair",
			session: &bossanovav1.Session{
				Id:            "s1",
				DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT,
			},
			wantStatus: postRepairStatusNeedsRepair,
			wantReason: "merge conflict",
		},
		{
			name: "rejected needs another repair",
			session: &bossanovav1.Session{
				Id:            "s1",
				DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED,
			},
			wantStatus: postRepairStatusNeedsRepair,
			wantReason: "review feedback",
		},
		{
			name: "unspecified is unknown",
			session: &bossanovav1.Session{
				Id:            "s1",
				DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_UNSPECIFIED,
			},
			wantStatus: postRepairStatusUnknown,
			wantReason: "display status unspecified",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := assessPostRepairStatus(tt.session)

			require.Equal(t, tt.wantStatus, got.Status)
			require.Equal(t, tt.wantReason, got.Reason)
			require.Equal(t, tt.session.GetDisplayStatus(), got.DisplayStatus)
			require.Equal(t, tt.session.GetDisplayHasFailures(), got.HasFailures)
		})
	}
}

func TestPostRepairAssessment_WithReviewComparisonNewFingerprintNeedsRepair(t *testing.T) {
	t.Parallel()

	session := &bossanovav1.Session{
		Id:            "s1",
		DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_PASSING,
	}

	got := assessPostRepairStatus(session)
	got.ReviewFingerprint = "new-review-fingerprint"
	got.ReviewFingerprintAvailable = true
	got = got.withReviewComparison("old-review-fingerprint", true)

	require.Equal(t, postRepairStatusNeedsRepair, got.Status)
	require.Equal(t, "new review feedback", got.Reason)
}

func TestPostRepairAssessment_WithReviewComparisonResolvedFingerprintStaysClean(t *testing.T) {
	t.Parallel()

	session := &bossanovav1.Session{
		Id:            "s1",
		DisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_PASSING,
	}

	got := assessPostRepairStatus(session)
	got.ReviewFingerprint = ""
	got.ReviewFingerprintAvailable = true
	got = got.withReviewComparison("old-review-fingerprint", true)

	require.Equal(t, postRepairStatusClean, got.Status)
	require.Equal(t, "checks passed", got.Reason)
}

func TestReviewFingerprintStableAcrossCommentOrder(t *testing.T) {
	path := "proxy.go"
	line := int32(164)
	comments := []*bossanovav1.ReviewComment{
		{Author: "bot", Body: "first", Path: &path, Line: &line, State: bossanovav1.ReviewState_REVIEW_STATE_CHANGES_REQUESTED},
		{Author: "bot", Body: "second", State: bossanovav1.ReviewState_REVIEW_STATE_COMMENTED},
	}
	reversed := []*bossanovav1.ReviewComment{comments[1], comments[0]}

	want, err := reviewFingerprint(comments)
	require.NoError(t, err)
	got, err := reviewFingerprint(reversed)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestReviewFingerprintChangesWhenFeedbackChanges(t *testing.T) {
	path := "proxy.go"
	line := int32(164)
	before, err := reviewFingerprint([]*bossanovav1.ReviewComment{
		{Author: "bot", Body: "old", Path: &path, Line: &line, State: bossanovav1.ReviewState_REVIEW_STATE_CHANGES_REQUESTED},
	})
	require.NoError(t, err)
	after, err := reviewFingerprint([]*bossanovav1.ReviewComment{
		{Author: "bot", Body: "new", Path: &path, Line: &line, State: bossanovav1.ReviewState_REVIEW_STATE_CHANGES_REQUESTED},
	})
	require.NoError(t, err)
	assert.NotEqual(t, before, after)
}

func TestReviewFingerprintIgnoresNonActionableReviewStates(t *testing.T) {
	path := "proxy.go"
	line := int32(164)
	got, err := reviewFingerprint([]*bossanovav1.ReviewComment{
		{Author: "reviewer", Body: "approved", Path: &path, Line: &line, State: bossanovav1.ReviewState_REVIEW_STATE_APPROVED},
		{Author: "reviewer", Body: "dismissed", Path: &path, Line: &line, State: bossanovav1.ReviewState_REVIEW_STATE_DISMISSED},
	})
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

func TestReviewFingerprintEmptyForNoComments(t *testing.T) {
	got, err := reviewFingerprint(nil)
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

func TestReviewFingerprintDistinguishesOptionalFieldPresence(t *testing.T) {
	path := "proxy.go"
	emptyPath := ""
	withMissingPath, err := reviewFingerprint([]*bossanovav1.ReviewComment{{Author: "bot", Body: "same", State: bossanovav1.ReviewState_REVIEW_STATE_COMMENTED}})
	require.NoError(t, err)
	withEmptyPath, err := reviewFingerprint([]*bossanovav1.ReviewComment{{Author: "bot", Body: "same", Path: &emptyPath, State: bossanovav1.ReviewState_REVIEW_STATE_COMMENTED}})
	require.NoError(t, err)
	withPath, err := reviewFingerprint([]*bossanovav1.ReviewComment{{Author: "bot", Body: "same", Path: &path, State: bossanovav1.ReviewState_REVIEW_STATE_COMMENTED}})
	require.NoError(t, err)

	assert.NotEqual(t, withMissingPath, withEmptyPath)
	assert.NotEqual(t, withEmptyPath, withPath)
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

func TestMaybeRepair_CapsInMemoryCooldown(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS},
	}
	rm := newTestMonitor(mock)
	rm.config = &repairConfig{CooldownMinutes: 45}
	rm.cooldowns["s1"] = time.Now().Add(-31 * time.Minute)

	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	waitFor(t, func() bool {
		c, _, _, _ := mock.snapshot()
		return c > 0
	}, "StartChatRun called after capped in-memory cooldown")
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
	rm.lastAttemptDisplayStatus["s1"] = bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING

	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	// Since maybeRepair now has an async path (it calls ListSessions before
	// taking the mu.Lock), give it a beat.
	time.Sleep(50 * time.Millisecond)
	startCalls, _, _, _ := mock.snapshot()
	assert.Equal(t, 0, startCalls, "must skip repair for the same commit")
}

func TestMaybeRepair_AllowsSameCommitDifferentStatus(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_READY_FOR_REVIEW, PrDisplayHeadSha: "abc123"},
	}
	rm := newTestMonitor(mock)
	rm.lastAttemptCommit["s1"] = "abc123"
	rm.lastAttemptDisplayStatus["s1"] = bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING

	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED, true)

	waitFor(t, func() bool {
		c, _, _, _ := mock.snapshot()
		return c > 0
	}, "StartChatRun called when same head has a different repair status")
}

func TestMaybeRepair_SkipsPersistedSameHeadStatusAfterAgentRun(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{
			Id:                      "s1",
			State:                   bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
			PrDisplayHeadSha:        "abc123",
			LastRepairHeadSha:       "abc123",
			LastRepairDisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING,
			LastRepairExitError:     "exit status 1",
		},
	}
	rm := newTestMonitor(mock)

	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	time.Sleep(50 * time.Millisecond)
	startCalls, _, _, _ := mock.snapshot()
	assert.Equal(t, 0, startCalls, "persisted same-head/status agent exit must not retry forever")
}

func TestMaybeRepair_RejectedAllowsSameHeadWhenReviewFingerprintChanges(t *testing.T) {
	path := "proxy.go"
	line := int32(164)
	prNumber := int32(399)
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{
			Id:                          "s1",
			State:                       bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
			PrDisplayHeadSha:            "abc123",
			LastRepairHeadSha:           "abc123",
			LastRepairDisplayStatus:     bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED,
			LastRepairExitError:         "exit status 1",
			LastRepairReviewFingerprint: "old-fingerprint",
			RepoOriginUrl:               "git@github.com:recurser/bossanova.git",
			PrNumber:                    &prNumber,
		},
	}
	mock.reviewCommentsResp = &bossanovav1.GetReviewCommentsResponse{
		Comments: []*bossanovav1.ReviewComment{
			{Author: "reviewer", Body: "new feedback", Path: &path, Line: &line, State: bossanovav1.ReviewState_REVIEW_STATE_CHANGES_REQUESTED},
		},
	}
	rm := newTestMonitor(mock)

	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED, false)

	waitFor(t, func() bool {
		c, _, _, _ := mock.snapshot()
		return c > 0
	}, "StartChatRun called when same rejected head has changed review feedback")
}

func TestMaybeRepair_RejectedSkipsSameHeadWhenReviewFingerprintMatches(t *testing.T) {
	path := "proxy.go"
	line := int32(164)
	prNumber := int32(399)
	comments := []*bossanovav1.ReviewComment{
		{Author: "reviewer", Body: "same feedback", Path: &path, Line: &line, State: bossanovav1.ReviewState_REVIEW_STATE_CHANGES_REQUESTED},
	}
	fp, err := reviewFingerprint(comments)
	require.NoError(t, err)
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{
			Id:                          "s1",
			State:                       bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
			PrDisplayHeadSha:            "abc123",
			LastRepairHeadSha:           "abc123",
			LastRepairDisplayStatus:     bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED,
			LastRepairExitError:         "exit status 1",
			LastRepairReviewFingerprint: fp,
			RepoOriginUrl:               "git@github.com:recurser/bossanova.git",
			PrNumber:                    &prNumber,
		},
	}
	mock.reviewCommentsResp = &bossanovav1.GetReviewCommentsResponse{Comments: comments}
	rm := newTestMonitor(mock)

	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED, false)

	time.Sleep(50 * time.Millisecond)
	startCalls, _, _, _ := mock.snapshot()
	assert.Equal(t, 0, startCalls, "same rejected head with same review fingerprint must not retry")
}

func TestMaybeRepair_RejectedSkipsSameHeadWhenReviewFingerprintUnavailable(t *testing.T) {
	prNumber := int32(399)
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{
			Id:                      "s1",
			State:                   bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
			PrDisplayHeadSha:        "abc123",
			LastRepairHeadSha:       "abc123",
			LastRepairDisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED,
			LastRepairExitError:     "exit status 1",
			RepoOriginUrl:           "git@github.com:recurser/bossanova.git",
			PrNumber:                &prNumber,
		},
	}
	mock.reviewCommentsErr = errors.New("github unavailable")
	rm := newTestMonitor(mock)

	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED, false)

	time.Sleep(50 * time.Millisecond)
	startCalls, _, _, _ := mock.snapshot()
	assert.Equal(t, 0, startCalls, "same rejected head must not retry when review fingerprint is unavailable")
}

func TestMaybeRepair_AllowsPersistedRunnerFailureSameHeadStatus(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{
			Id:                      "s1",
			State:                   bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
			PrDisplayHeadSha:        "abc123",
			LastRepairHeadSha:       "abc123",
			LastRepairDisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING,
			LastRepairRunnerError:   "claude not on PATH",
		},
	}
	rm := newTestMonitor(mock)

	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	waitFor(t, func() bool {
		c, _, _, _ := mock.snapshot()
		return c > 0
	}, "StartChatRun called after runner failure because no agent repair ran")
}

func TestMaybeRepair_AllowsPersistedIncompleteInteractiveChatSameHeadStatus(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{
			Id:                      "s1",
			State:                   bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
			PrDisplayHeadSha:        "abc123",
			LastRepairHeadSha:       "abc123",
			LastRepairDisplayStatus: bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING,
			LastRepairExitError:     "interactive chat did not report completion before wait deadline: context deadline exceeded",
		},
	}
	rm := newTestMonitor(mock)

	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	waitFor(t, func() bool {
		c, _, _, _ := mock.snapshot()
		return c > 0
	}, "StartChatRun called after incomplete interactive chat because no agent repair completed")
}

func TestMaybeRepair_SkipsAlreadyRepairing(t *testing.T) {
	mock := newTestMock()
	rm := newTestMonitor(mock)
	rm.repairing["s1"] = true
	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	startCalls, _, _, _ := mock.snapshot()
	assert.Equal(t, 0, startCalls)
}

// TestCooldownFor pins the exponential-backoff schedule that protects
// against the hammer loop behind the production 321× counter on the
// Dependabot PRs. Schedule with the default 1-minute base: 1m, 2m, 4m,
// 8m, 16m, then cap at 30m. Pre-fix the cooldown was a flat 1m forever
// regardless of attempt count, which let a stuck PR rack up ~60 attempts/hr.
func TestCooldownFor(t *testing.T) {
	base := time.Minute
	tests := []struct {
		name         string
		attemptCount int32
		want         time.Duration
	}{
		{"no failures uses base floor", 0, base},
		{"negative defensively uses base", -3, base},
		{"first failure waits base", 1, base},
		{"second failure doubles", 2, 2 * base},
		{"third failure", 3, 4 * base},
		{"fourth failure", 4, 8 * base},
		{"fifth failure", 5, 16 * base},
		{"sixth failure caps at 30m", 6, 30 * time.Minute},
		{"tenth failure still capped", 10, 30 * time.Minute},
		{"huge count never overflows", 1000, 30 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, cooldownFor(tc.attemptCount, base))
		})
	}
}

// TestCooldownFor_RespectsConfiguredBase verifies that an operator who
// raises the floor via CooldownMinutes gets a proportionally longer
// schedule until maxCooldownDuration. Larger bases reach the cap sooner.
func TestCooldownFor_RespectsConfiguredBase(t *testing.T) {
	tests := []struct {
		name         string
		base         time.Duration
		attemptCount int32
		want         time.Duration
	}{
		{"ten minute base first failure", 10 * time.Minute, 1, 10 * time.Minute},
		{"ten minute base second failure doubles", 10 * time.Minute, 2, 20 * time.Minute},
		{"ten minute base third failure caps", 10 * time.Minute, 3, 30 * time.Minute},
		{"ten minute base later failures stay capped", 10 * time.Minute, 10, 30 * time.Minute},
		{"fifteen minute base first failure", 15 * time.Minute, 1, 15 * time.Minute},
		{"fifteen minute base second failure reaches cap", 15 * time.Minute, 2, 30 * time.Minute},
		{"fifteen minute base later failures stay capped", 15 * time.Minute, 3, 30 * time.Minute},
		{"oversized base with no persisted failure caps", 45 * time.Minute, 0, 30 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, cooldownFor(tc.attemptCount, tc.base))
		})
	}
}

// TestMaybeRepair_SkipsDuringExponentialBackoff covers the regression that
// motivated the persisted-state cooldown: a stuck PR with N consecutive
// failures must wait the exponential-backoff window — sourced from the
// daemon-persisted last_repair_started_at + last_repair_attempt_count so a
// daemon restart can't reset the schedule and immediately re-fire. Before
// the fix, m.cooldowns was in-memory only and lost on restart; combined
// with the flat 1m cooldown that gave the 321× count.
func TestMaybeRepair_SkipsDuringExponentialBackoff(t *testing.T) {
	mock := newTestMock()
	// Persisted state: 5 consecutive failures. cooldownFor(5, 1m) = 16m.
	// LastRepairStartedAt = 1 minute ago — well inside the 16m window.
	startedAt := time.Now().Add(-1 * time.Minute)
	mock.sessions = []*bossanovav1.Session{
		{
			Id:                     "s1",
			State:                  bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
			LastRepairAttemptCount: 5,
			LastRepairStartedAt:    timestamppb.New(startedAt),
		},
	}
	rm := newTestMonitor(mock)
	// Critically: the in-memory `cooldowns` map is empty (simulating a
	// daemon restart). The persisted-state gate must still block.
	assert.True(t, rm.cooldowns["s1"].IsZero(), "in-memory cooldown must be empty for this regression test")

	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	time.Sleep(50 * time.Millisecond)
	startCalls, _, _, _ := mock.snapshot()
	assert.Equal(t, 0, startCalls, "exponential backoff must skip repair when persisted attempt count says we should wait")
}

// TestMaybeRepair_FiresAfterExponentialBackoffElapsed verifies the
// counterpart: once the backoff window has elapsed, repair fires
// normally. Counter=3 → wait=4m; LastRepairStartedAt=5m ago, so the
// gate has elapsed and the attempt proceeds.
func TestMaybeRepair_FiresAfterExponentialBackoffElapsed(t *testing.T) {
	mock := newTestMock()
	startedAt := time.Now().Add(-5 * time.Minute)
	mock.sessions = []*bossanovav1.Session{
		{
			Id:                     "s1",
			State:                  bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
			LastRepairAttemptCount: 3, // wait = 4m, elapsed = 5m → fire
			LastRepairStartedAt:    timestamppb.New(startedAt),
			LastRepairRunnerError:  "previous attempt failed",
		},
	}
	rm := newTestMonitor(mock)

	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	waitFor(t, func() bool {
		c, _, _, _ := mock.snapshot()
		return c > 0
	}, "StartChatRun called once exponential backoff window elapsed")
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

func TestMaybeRepair_RequestsReplacementWhenActiveChatIdlePastThreshold(t *testing.T) {
	mock := newTestMock()
	agentSessionID := "agent-finalize"
	stale := timestamppb.New(time.Now().Add(-10 * time.Minute))
	mock.sessions = []*bossanovav1.Session{
		{
			Id:                 "s1",
			Title:              "Improve conflict detection",
			State:              bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
			AgentSessionId:     &agentSessionID,
			HasActiveChat:      true,
			LastChatActivityAt: stale,
		},
	}
	rm := newTestMonitor(mock)
	rm.config = &repairConfig{IdleRepairThresholdMinutes: 5}

	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED, false)

	waitFor(t, func() bool {
		reqs := mock.startRequestsSnapshot()
		return len(reqs) == 1
	}, "StartChatRun called")

	reqs := mock.startRequestsSnapshot()
	require.Len(t, reqs, 1)
	require.True(t, reqs[0].GetReplaceExistingChat())
	require.Contains(t, reqs[0].GetReplaceExistingReason(), "auto-repair replacing idle chat")
	require.Equal(t, stale.AsTime(), reqs[0].GetReplaceExistingObservedLastChatActivityAt().AsTime())
	require.Equal(t, "Repair: Improve conflict detection", reqs[0].GetTitle())
}

func TestMaybeRepair_DoesNotRequestReplacementWhenActiveChatRecent(t *testing.T) {
	mock := newTestMock()
	agentSessionID := "agent-finalize"
	recent := timestamppb.New(time.Now().Add(-2 * time.Second))
	mock.sessions = []*bossanovav1.Session{
		{
			Id:                 "s1",
			Title:              "Improve conflict detection",
			State:              bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
			AgentSessionId:     &agentSessionID,
			HasActiveChat:      true,
			LastChatActivityAt: recent,
		},
	}
	rm := newTestMonitor(mock)
	rm.config = &repairConfig{IdleRepairThresholdMinutes: 5}

	rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED, false)

	time.Sleep(50 * time.Millisecond)
	startCalls, _, _, _ := mock.snapshot()
	require.Zero(t, startCalls)
	require.Empty(t, mock.startRequestsSnapshot())
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
