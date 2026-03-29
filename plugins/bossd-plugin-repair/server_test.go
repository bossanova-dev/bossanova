package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/plugin/hostclient"
)

// mockHostClient is a test double that records calls and returns
// preconfigured responses.
type mockHostClient struct {
	mu sync.Mutex

	// ListWorkflows behaviour.
	workflows    []*bossanovav1.Workflow
	listWfErr    error
	listWfCalls  int
	listWfFilter string // last status_filter received

	// ListSessions behaviour.
	sessions      []*bossanovav1.Session
	listSessErr   error
	listSessCalls int

	// CreateWorkflow behaviour.
	createWfResp  *bossanovav1.CreateWorkflowResponse
	createWfErr   error
	createWfCalls int

	// CreateAttempt behaviour.
	createAtResp  *bossanovav1.CreateAttemptResponse
	createAtErr   error
	createAtCalls int

	// GetAttemptStatus behaviour — returns COMPLETED after pollsBeforeDone polls.
	pollsBeforeDone int
	pollCount       int

	// UpdateWorkflow tracking.
	updateWfCalls []*bossanovav1.UpdateWorkflowRequest

	// FireSessionEvent tracking.
	fireEventCalls int
}

var _ hostClient = (*mockHostClient)(nil)

func newTestMock() *mockHostClient {
	return &mockHostClient{
		createWfResp: &bossanovav1.CreateWorkflowResponse{
			Workflow: &bossanovav1.Workflow{Id: "wf-repair-1"},
		},
		createAtResp: &bossanovav1.CreateAttemptResponse{
			AttemptId: "attempt-1",
		},
		pollsBeforeDone: 1,
	}
}

func (m *mockHostClient) ListWorkflows(_ context.Context, statusFilter string) (*bossanovav1.ListWorkflowsResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listWfCalls++
	m.listWfFilter = statusFilter
	if m.listWfErr != nil {
		return nil, m.listWfErr
	}
	return &bossanovav1.ListWorkflowsResponse{Workflows: m.workflows}, nil
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

func (m *mockHostClient) CreateWorkflow(_ context.Context, _ *bossanovav1.CreateWorkflowRequest) (*bossanovav1.CreateWorkflowResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createWfCalls++
	return m.createWfResp, m.createWfErr
}

func (m *mockHostClient) UpdateWorkflow(_ context.Context, req *bossanovav1.UpdateWorkflowRequest) (*bossanovav1.UpdateWorkflowResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateWfCalls = append(m.updateWfCalls, req)
	return &bossanovav1.UpdateWorkflowResponse{}, nil
}

func (m *mockHostClient) GetWorkflow(_ context.Context, _ string) (*bossanovav1.GetWorkflowResponse, error) {
	return &bossanovav1.GetWorkflowResponse{}, nil
}

func (m *mockHostClient) CreateAttempt(_ context.Context, _ *bossanovav1.CreateAttemptRequest) (*bossanovav1.CreateAttemptResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createAtCalls++
	return m.createAtResp, m.createAtErr
}

func (m *mockHostClient) GetAttemptStatus(_ context.Context, _ string) (*bossanovav1.GetAttemptStatusResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pollCount++
	if m.pollCount >= m.pollsBeforeDone {
		return &bossanovav1.GetAttemptStatusResponse{
			Status: bossanovav1.AttemptRunStatus_ATTEMPT_RUN_STATUS_COMPLETED,
		}, nil
	}
	return &bossanovav1.GetAttemptStatusResponse{
		Status: bossanovav1.AttemptRunStatus_ATTEMPT_RUN_STATUS_RUNNING,
	}, nil
}

func (m *mockHostClient) StreamAttemptOutput(_ context.Context, _ string) (hostclient.AttemptOutputStream, error) {
	return nil, fmt.Errorf("not implemented in mock")
}

func (m *mockHostClient) GetReviewComments(_ context.Context, _ *bossanovav1.GetReviewCommentsRequest) (*bossanovav1.GetReviewCommentsResponse, error) {
	return nil, fmt.Errorf("not implemented in mock")
}

func (m *mockHostClient) FireSessionEvent(_ context.Context, _ *bossanovav1.FireSessionEventRequest) (*bossanovav1.FireSessionEventResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fireEventCalls++
	return &bossanovav1.FireSessionEventResponse{}, nil
}

func (m *mockHostClient) SetRepairStatus(_ context.Context, _ *bossanovav1.SetRepairStatusRequest) (*bossanovav1.SetRepairStatusResponse, error) {
	return &bossanovav1.SetRepairStatusResponse{}, nil
}

// --- helpers ---

func testLogger() zerolog.Logger {
	return zerolog.Nop()
}

func newTestMonitor(mock *mockHostClient) *repairMonitor {
	rm := newRepairMonitor(mock, testLogger())
	rm.stopped = false
	rm.config = &repairConfig{PollIntervalSeconds: 1} // fast polls for tests
	return rm
}

// waitFor spins until cond() returns true or 5 s expires.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for: " + msg)
}

// --- Tests ---

func TestMaybeRepairSkipsNonRepairableStatus(t *testing.T) {
	mock := newTestMock()
	rm := newTestMonitor(mock)

	triggered := rm.maybeRepair("s1", bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_PASSING, false)
	assert.False(t, triggered)

	// No workflow created.
	mock.mu.Lock()
	assert.Equal(t, 0, mock.createWfCalls)
	mock.mu.Unlock()
}

func TestMaybeRepairTriggersForRejectStatus(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS},
	}
	rm := newTestMonitor(mock)

	triggered := rm.maybeRepair("s1", bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_REJECTED, false)
	assert.True(t, triggered)

	// Wait for the background goroutine to create a workflow.
	waitFor(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return mock.createWfCalls > 0
	}, "CreateWorkflow to be called")
}

func TestMaybeRepairTriggersForFailingStatus(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS},
	}
	rm := newTestMonitor(mock)

	triggered := rm.maybeRepair("s1", bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_FAILING, true)
	assert.True(t, triggered)

	waitFor(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return mock.createWfCalls > 0
	}, "CreateWorkflow to be called")
}

func TestMaybeRepairTriggersForConflictStatus(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_GREEN_DRAFT},
	}
	rm := newTestMonitor(mock)

	triggered := rm.maybeRepair("s1", bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_CONFLICT, false)
	assert.True(t, triggered)

	waitFor(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return mock.createWfCalls > 0
	}, "CreateWorkflow to be called")
}

func TestMaybeRepairSkipsStopped(t *testing.T) {
	mock := newTestMock()
	rm := newTestMonitor(mock)
	rm.mu.Lock()
	rm.stopped = true
	rm.mu.Unlock()

	triggered := rm.maybeRepair("s1", bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_REJECTED, false)
	assert.False(t, triggered)
}

func TestMaybeRepairSkipsPaused(t *testing.T) {
	mock := newTestMock()
	rm := newTestMonitor(mock)
	rm.mu.Lock()
	rm.paused = true
	rm.mu.Unlock()

	triggered := rm.maybeRepair("s1", bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_REJECTED, false)
	assert.False(t, triggered)
}

func TestPauseAndResumeWorkflow(t *testing.T) {
	mock := newTestMock()
	rm := newTestMonitor(mock)

	// Pause should block repairs.
	_, err := rm.PauseWorkflow(context.Background(), &bossanovav1.PauseWorkflowRequest{})
	require.NoError(t, err)

	triggered := rm.maybeRepair("s1", bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_REJECTED, false)
	assert.False(t, triggered, "should not repair while paused")

	// Resume should allow repairs again.
	_, err = rm.ResumeWorkflow(context.Background(), &bossanovav1.ResumeWorkflowRequest{})
	require.NoError(t, err)

	// Set up a repairable session state for the resumed attempt.
	mock.mu.Lock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS},
	}
	mock.mu.Unlock()

	triggered = rm.maybeRepair("s1", bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_REJECTED, false)
	assert.True(t, triggered, "should repair after resume")

	waitFor(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return mock.createWfCalls > 0
	}, "CreateWorkflow to be called after resume")
}

func TestMaybeRepairSkipsAlreadyRepairing(t *testing.T) {
	mock := newTestMock()
	rm := newTestMonitor(mock)

	rm.mu.Lock()
	rm.repairing["s1"] = true
	rm.mu.Unlock()

	triggered := rm.maybeRepair("s1", bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_REJECTED, false)
	assert.False(t, triggered)
}

func TestMaybeRepairCooldown(t *testing.T) {
	mock := newTestMock()
	rm := newTestMonitor(mock)

	// Set a recent cooldown.
	rm.mu.Lock()
	rm.cooldowns["s1"] = time.Now()
	rm.mu.Unlock()

	triggered := rm.maybeRepair("s1", bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_REJECTED, false)
	assert.False(t, triggered)
}

func TestMaybeRepairSkipsActiveSession(t *testing.T) {
	mock := newTestMock()
	// Simulate an active workflow for session s1.
	mock.workflows = []*bossanovav1.Workflow{
		{Id: "wf-autopilot-1", SessionId: "s1", Status: "running"},
	}
	rm := newTestMonitor(mock)

	triggered := rm.maybeRepair("s1", bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_REJECTED, false)
	assert.False(t, triggered)

	// Verify ListWorkflows was called with "running" filter.
	mock.mu.Lock()
	assert.Equal(t, 1, mock.listWfCalls)
	assert.Equal(t, "running", mock.listWfFilter)
	mock.mu.Unlock()
}

func TestMaybeRepairAllowsIdleSession(t *testing.T) {
	mock := newTestMock()
	// No active workflows.
	mock.workflows = []*bossanovav1.Workflow{}
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS},
	}
	rm := newTestMonitor(mock)

	triggered := rm.maybeRepair("s1", bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_REJECTED, false)
	assert.True(t, triggered)

	waitFor(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return mock.createWfCalls > 0
	}, "CreateWorkflow to be called")
}

func TestMaybeRepairSkipsWhenListWorkflowsFails(t *testing.T) {
	mock := newTestMock()
	mock.listWfErr = fmt.Errorf("connection refused")
	rm := newTestMonitor(mock)

	triggered := rm.maybeRepair("s1", bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_REJECTED, false)
	assert.False(t, triggered, "should skip repair when ListWorkflows fails (fail-safe)")
}

func TestIsSessionIdle(t *testing.T) {
	mock := newTestMock()
	rm := newTestMonitor(mock)
	ctx := context.Background()

	t.Run("idle when no workflows", func(t *testing.T) {
		mock.workflows = nil
		assert.True(t, rm.isSessionIdle(ctx, "s1"))
	})

	t.Run("idle when workflows belong to other sessions", func(t *testing.T) {
		mock.workflows = []*bossanovav1.Workflow{
			{Id: "wf-1", SessionId: "s2", Status: "running"},
		}
		assert.True(t, rm.isSessionIdle(ctx, "s1"))
	})

	t.Run("not idle when workflow matches session", func(t *testing.T) {
		mock.workflows = []*bossanovav1.Workflow{
			{Id: "wf-1", SessionId: "s1", Status: "running"},
		}
		assert.False(t, rm.isSessionIdle(ctx, "s1"))
	})

	t.Run("not idle on error (fail-safe)", func(t *testing.T) {
		mock.listWfErr = fmt.Errorf("rpc error")
		assert.False(t, rm.isSessionIdle(ctx, "s1"))
		mock.listWfErr = nil // reset
	})
}

func TestIsSessionRepairable(t *testing.T) {
	ctx := context.Background()

	t.Run("repairable in awaiting_checks", func(t *testing.T) {
		mock := newTestMock()
		mock.sessions = []*bossanovav1.Session{
			{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS},
		}
		rm := newTestMonitor(mock)
		ok, _, _ := rm.isSessionRepairable(ctx, "s1")
		assert.True(t, ok)
	})

	t.Run("repairable in fixing_checks", func(t *testing.T) {
		mock := newTestMock()
		mock.sessions = []*bossanovav1.Session{
			{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS},
		}
		rm := newTestMonitor(mock)
		ok, _, _ := rm.isSessionRepairable(ctx, "s1")
		assert.True(t, ok)
	})

	t.Run("repairable in green_draft", func(t *testing.T) {
		mock := newTestMock()
		mock.sessions = []*bossanovav1.Session{
			{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_GREEN_DRAFT},
		}
		rm := newTestMonitor(mock)
		ok, _, _ := rm.isSessionRepairable(ctx, "s1")
		assert.True(t, ok)
	})

	t.Run("repairable in ready_for_review", func(t *testing.T) {
		mock := newTestMock()
		mock.sessions = []*bossanovav1.Session{
			{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_READY_FOR_REVIEW},
		}
		rm := newTestMonitor(mock)
		ok, _, _ := rm.isSessionRepairable(ctx, "s1")
		assert.True(t, ok)
	})

	t.Run("not repairable in implementing_plan", func(t *testing.T) {
		mock := newTestMock()
		mock.sessions = []*bossanovav1.Session{
			{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_IMPLEMENTING_PLAN},
		}
		rm := newTestMonitor(mock)
		ok, _, _ := rm.isSessionRepairable(ctx, "s1")
		assert.False(t, ok)
	})

	t.Run("not repairable in blocked", func(t *testing.T) {
		mock := newTestMock()
		mock.sessions = []*bossanovav1.Session{
			{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_BLOCKED},
		}
		rm := newTestMonitor(mock)
		ok, _, _ := rm.isSessionRepairable(ctx, "s1")
		assert.False(t, ok)
	})

	t.Run("not repairable when session not found", func(t *testing.T) {
		mock := newTestMock()
		mock.sessions = []*bossanovav1.Session{}
		rm := newTestMonitor(mock)
		ok, _, _ := rm.isSessionRepairable(ctx, "s1")
		assert.False(t, ok)
	})

	t.Run("not repairable on ListSessions error", func(t *testing.T) {
		mock := newTestMock()
		mock.listSessErr = fmt.Errorf("connection refused")
		rm := newTestMonitor(mock)
		ok, _, _ := rm.isSessionRepairable(ctx, "s1")
		assert.False(t, ok)
		mock.listSessErr = nil
	})
}

func TestMaybeRepairSkipsImplementingPlan(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_IMPLEMENTING_PLAN},
	}
	rm := newTestMonitor(mock)

	triggered := rm.maybeRepair("s1", bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_FAILING, true)
	assert.False(t, triggered, "should not repair while session is implementing plan")
}

func TestSweepExistingSessions(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS, PrDisplayStatus: bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_REJECTED, PrDisplayHasFailures: false},
		{Id: "s2", State: bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS, PrDisplayStatus: bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_PASSING, PrDisplayHasFailures: false},
		{Id: "s3", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS, PrDisplayStatus: bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_FAILING, PrDisplayHasFailures: true},
	}
	rm := newTestMonitor(mock)

	rm.sweepExistingSessions(context.Background())

	// Wait for background goroutines to create workflows. s1 and s3 should trigger.
	waitFor(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return mock.createWfCalls >= 2
	}, "two CreateWorkflow calls")

	mock.mu.Lock()
	assert.Equal(t, 2, mock.createWfCalls, "only REJECTED and FAILING sessions should trigger repair")
	// 1 call from sweep itself + 2 calls from isSessionRepairable (for s1 and s3).
	assert.Equal(t, 3, mock.listSessCalls)
	mock.mu.Unlock()
}

func TestSweepSkipsSessionsWithActiveWorkflows(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", PrDisplayStatus: bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_REJECTED},
	}
	// s1 has an active workflow.
	mock.workflows = []*bossanovav1.Workflow{
		{Id: "wf-autopilot", SessionId: "s1", Status: "running"},
	}
	rm := newTestMonitor(mock)

	rm.sweepExistingSessions(context.Background())

	// Give time for any async work.
	time.Sleep(100 * time.Millisecond)

	mock.mu.Lock()
	assert.Equal(t, 0, mock.createWfCalls, "should not create repair workflow for active session")
	mock.mu.Unlock()
}

func TestNotifyStatusChangeDelegatesToMaybeRepair(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_READY_FOR_REVIEW},
	}
	rm := newTestMonitor(mock)

	resp, err := rm.NotifyStatusChange(context.Background(), &bossanovav1.NotifyStatusChangeRequest{
		SessionId:     "s1",
		DisplayStatus: bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_REJECTED,
	})
	require.NoError(t, err)
	assert.NotNil(t, resp)

	waitFor(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return mock.createWfCalls > 0
	}, "CreateWorkflow to be called via NotifyStatusChange")
}

func TestNotifyStatusChangePassingDoesNotRepair(t *testing.T) {
	mock := newTestMock()
	rm := newTestMonitor(mock)

	resp, err := rm.NotifyStatusChange(context.Background(), &bossanovav1.NotifyStatusChangeRequest{
		SessionId:     "s1",
		DisplayStatus: bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_PASSING,
	})
	require.NoError(t, err)
	assert.NotNil(t, resp)

	time.Sleep(100 * time.Millisecond)
	mock.mu.Lock()
	assert.Equal(t, 0, mock.createWfCalls)
	mock.mu.Unlock()
}

func TestPeriodicSweepTriggersRepair(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS, PrDisplayStatus: bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_REJECTED},
	}
	rm := newTestMonitor(mock)
	// Set an expired cooldown so the session is eligible.
	rm.mu.Lock()
	rm.cooldowns["s1"] = time.Now().Add(-10 * time.Minute)
	rm.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rm.mu.Lock()
	rm.testSweepInterval = 50 * time.Millisecond
	rm.mu.Unlock()

	go rm.periodicSweep(ctx)

	waitFor(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return mock.createWfCalls > 0
	}, "periodic sweep to trigger repair")
}

func TestPeriodicSweepRespectsCancel(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS, PrDisplayStatus: bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_REJECTED},
	}
	rm := newTestMonitor(mock)

	ctx, cancel := context.WithCancel(context.Background())

	rm.mu.Lock()
	rm.testSweepInterval = 50 * time.Millisecond
	rm.mu.Unlock()

	done := make(chan struct{})
	go func() {
		rm.periodicSweep(ctx)
		close(done)
	}()

	// Cancel immediately before any tick fires.
	cancel()

	select {
	case <-done:
		// periodicSweep exited as expected.
	case <-time.After(2 * time.Second):
		t.Fatal("periodicSweep did not exit after cancel")
	}
}

func TestPeriodicSweepRespectsCooldown(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS, PrDisplayStatus: bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_REJECTED},
	}
	rm := newTestMonitor(mock)

	// Set a very recent cooldown — repair should be skipped.
	rm.mu.Lock()
	rm.cooldowns["s1"] = time.Now()
	rm.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rm.mu.Lock()
	rm.testSweepInterval = 50 * time.Millisecond
	rm.mu.Unlock()

	go rm.periodicSweep(ctx)

	// Let a few sweep cycles run.
	time.Sleep(200 * time.Millisecond)

	mock.mu.Lock()
	assert.Equal(t, 0, mock.createWfCalls, "should not repair while cooldown is active")
	mock.mu.Unlock()
}

func TestStartWorkflowLaunchesSweep(t *testing.T) {
	mock := newTestMock()
	mock.sessions = []*bossanovav1.Session{
		{Id: "s1", State: bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS, PrDisplayStatus: bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_FAILING, PrDisplayHasFailures: true},
	}
	rm := newRepairMonitor(mock, testLogger())
	// rm starts stopped=true, StartWorkflow should fix that and sweep.

	_, err := rm.StartWorkflow(context.Background(), &bossanovav1.StartWorkflowRequest{
		ConfigJson: `{"poll_interval_seconds": 1}`,
	})
	require.NoError(t, err)

	// The sweep goroutine should list sessions and trigger a repair.
	waitFor(t, func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return mock.listSessCalls > 0 && mock.createWfCalls > 0
	}, "sweep to list sessions and create workflow")
}
