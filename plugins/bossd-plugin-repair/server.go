package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

const (
	// defaultCooldownDuration is the minimum time between repair attempts for the same session.
	defaultCooldownDuration = 1 * time.Minute
	// defaultPollInterval is how often to check attempt status.
	defaultPollInterval = 5 * time.Second
	// defaultRepairSkill is the skill invoked for repair attempts.
	defaultRepairSkill = "boss-repair"
	// defaultSweepInterval is how often the periodic sweep runs.
	defaultSweepInterval = 1 * time.Minute
	// defaultStuckTimeout is how long a session must be in ImplementingPlan
	// before it is considered stuck and eligible for automatic advancement.
	defaultStuckTimeout = 5 * time.Minute
)

// repairConfig holds parsed config for a repair workflow. Fields mirror
// config.RepairConfig but are local to the plugin to avoid importing
// the config package.
type repairConfig struct {
	Skills               repairSkillOverrides `json:"skills,omitempty"`
	CooldownMinutes      int                  `json:"cooldown_minutes,omitempty"`
	PollIntervalSeconds  int                  `json:"poll_interval_seconds,omitempty"`
	SweepIntervalMinutes int                  `json:"sweep_interval_minutes,omitempty"`
	StuckTimeoutMinutes  int                  `json:"stuck_timeout_minutes,omitempty"`
}

type repairSkillOverrides struct {
	Repair string `json:"repair,omitempty"`
}

func (c *repairConfig) cooldownDuration() time.Duration {
	if c != nil && c.CooldownMinutes > 0 {
		return time.Duration(c.CooldownMinutes) * time.Minute
	}
	return defaultCooldownDuration
}

func (c *repairConfig) pollInterval() time.Duration {
	if c != nil && c.PollIntervalSeconds > 0 {
		return time.Duration(c.PollIntervalSeconds) * time.Second
	}
	return defaultPollInterval
}

func (c *repairConfig) skillName() string {
	if c != nil && c.Skills.Repair != "" {
		return c.Skills.Repair
	}
	return defaultRepairSkill
}

func (c *repairConfig) sweepInterval() time.Duration {
	if c != nil && c.SweepIntervalMinutes > 0 {
		return time.Duration(c.SweepIntervalMinutes) * time.Minute
	}
	return defaultSweepInterval
}

func (c *repairConfig) stuckTimeout() time.Duration {
	if c != nil && c.StuckTimeoutMinutes > 0 {
		return time.Duration(c.StuckTimeoutMinutes) * time.Minute
	}
	return defaultStuckTimeout
}

func parseRepairConfig(configJSON string) (*repairConfig, error) {
	cfg := &repairConfig{}
	if configJSON == "" {
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(configJSON), cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

// repairMonitor implements the WorkflowService gRPC server for the repair
// plugin. It watches for status changes and triggers repair workflows when
// PRs fail checks, have conflicts, or receive rejection feedback.
type repairMonitor struct {
	host   hostClient
	logger zerolog.Logger

	mu                sync.Mutex
	ctx               context.Context      // Workflow context
	cancel            context.CancelFunc   // Cancel function for the workflow
	stopped           bool                 // True after CancelWorkflow until next StartWorkflow
	paused            bool                 // True after PauseWorkflow until ResumeWorkflow
	config            *repairConfig        // Parsed config from StartWorkflowRequest
	repairing         map[string]bool      // Sessions currently being repaired
	cooldowns         map[string]time.Time // Last repair attempt time per session
	testSweepInterval time.Duration        // Override sweep interval for tests
	testStuckTimeout  time.Duration        // Override stuck timeout for tests
}

func newRepairMonitor(host hostClient, logger zerolog.Logger) *repairMonitor {
	ctx, cancel := context.WithCancel(context.Background())
	return &repairMonitor{
		host:      host,
		logger:    logger,
		ctx:       ctx,
		cancel:    cancel,
		stopped:   true, // Reject notifications until StartWorkflow sets config.
		repairing: make(map[string]bool),
		cooldowns: make(map[string]time.Time),
	}
}

// GetInfo returns plugin metadata.
func (m *repairMonitor) GetInfo(ctx context.Context, req *bossanovav1.WorkflowServiceGetInfoRequest) (*bossanovav1.WorkflowServiceGetInfoResponse, error) {
	return &bossanovav1.WorkflowServiceGetInfoResponse{
		Info: &bossanovav1.PluginInfo{
			Name:         "repair",
			Version:      "0.1.0",
			Capabilities: []string{"workflow", "repair"},
		},
	}, nil
}

// StartWorkflow starts the repair monitoring workflow.
func (m *repairMonitor) StartWorkflow(ctx context.Context, req *bossanovav1.StartWorkflowRequest) (*bossanovav1.StartWorkflowResponse, error) {
	cfg, err := parseRepairConfig(req.GetConfigJson())
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Cancel any previous workflow context and create a new one.
	if m.cancel != nil {
		m.cancel()
	}
	workflowCtx, cancel := context.WithCancel(context.Background())
	m.ctx = workflowCtx
	m.cancel = cancel
	m.stopped = false
	m.paused = false
	m.config = cfg

	m.logger.Info().
		Str("plan_path", req.GetPlanPath()).
		Msg("repair monitoring started")

	// Sweep existing sessions in a goroutine to catch any that are already
	// in a repairable state when the plugin starts.
	go m.sweepExistingSessions(workflowCtx)

	// Periodically re-sweep to catch sessions stuck in a repairable state
	// after failed repairs or missed edge-triggered notifications.
	go m.periodicSweep(workflowCtx)

	return &bossanovav1.StartWorkflowResponse{}, nil
}

// PauseWorkflow pauses the repair monitoring. New repair attempts will be
// skipped until ResumeWorkflow is called.
func (m *repairMonitor) PauseWorkflow(ctx context.Context, req *bossanovav1.PauseWorkflowRequest) (*bossanovav1.PauseWorkflowResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.logger.Info().
		Str("workflow_id", req.GetWorkflowId()).
		Msg("pausing repair workflow")

	m.paused = true
	return &bossanovav1.PauseWorkflowResponse{}, nil
}

// ResumeWorkflow resumes the repair monitoring after a pause.
func (m *repairMonitor) ResumeWorkflow(ctx context.Context, req *bossanovav1.ResumeWorkflowRequest) (*bossanovav1.ResumeWorkflowResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.logger.Info().
		Str("workflow_id", req.GetWorkflowId()).
		Msg("resuming repair workflow")

	m.paused = false
	return &bossanovav1.ResumeWorkflowResponse{}, nil
}

// CancelWorkflow cancels the repair monitoring.
func (m *repairMonitor) CancelWorkflow(ctx context.Context, req *bossanovav1.CancelWorkflowRequest) (*bossanovav1.CancelWorkflowResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.logger.Info().
		Str("workflow_id", req.GetWorkflowId()).
		Msg("canceling repair workflow")

	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.stopped = true

	return &bossanovav1.CancelWorkflowResponse{}, nil
}

// GetWorkflowStatus returns the current status of the repair monitoring.
func (m *repairMonitor) GetWorkflowStatus(ctx context.Context, req *bossanovav1.GetWorkflowStatusRequest) (*bossanovav1.GetWorkflowStatusResponse, error) {
	m.mu.Lock()
	var status bossanovav1.WorkflowStatus
	switch {
	case m.stopped:
		status = bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED
	case m.paused:
		status = bossanovav1.WorkflowStatus_WORKFLOW_STATUS_PAUSED
	default:
		status = bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING
	}
	m.mu.Unlock()

	return &bossanovav1.GetWorkflowStatusResponse{
		Status: &bossanovav1.WorkflowStatusInfo{
			Id:     req.GetWorkflowId(),
			Status: status,
		},
	}, nil
}

// NotifyStatusChange is called when a session's status changes (e.g., PR check fails).
// This is the main entry point for triggering repair workflows.
func (m *repairMonitor) NotifyStatusChange(ctx context.Context, req *bossanovav1.NotifyStatusChangeRequest) (*bossanovav1.NotifyStatusChangeResponse, error) {
	sessionID := req.GetSessionId()
	displayStatus := req.GetDisplayStatus()
	hasFailures := req.GetHasFailures()

	m.logger.Info().
		Str("session_id", sessionID).
		Int32("display_status", int32(displayStatus)).
		Bool("has_failures", hasFailures).
		Msg("received status change notification")

	m.maybeRepair(sessionID, displayStatus, hasFailures)

	return &bossanovav1.NotifyStatusChangeResponse{}, nil
}

// maybeRepair evaluates whether a session needs repair and, if so, launches
// a background repair goroutine. It is called both from NotifyStatusChange
// (edge-triggered) and sweepExistingSessions (level-triggered on startup).
// Returns true if a repair was triggered.
func (m *repairMonitor) maybeRepair(sessionID string, displayStatus bossanovav1.PRDisplayStatus, hasFailures bool) bool {
	// Only trigger repair for failing, conflict, or rejected states.
	needsRepair := displayStatus == bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_FAILING ||
		displayStatus == bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_CONFLICT ||
		displayStatus == bossanovav1.PRDisplayStatus_PR_DISPLAY_STATUS_REJECTED

	if !needsRepair {
		return false
	}

	m.mu.Lock()

	// Do not attempt repairs after CancelWorkflow or PauseWorkflow.
	if m.stopped || m.paused {
		stopped, paused := m.stopped, m.paused
		m.mu.Unlock()
		m.logger.Debug().
			Str("session_id", sessionID).
			Bool("stopped", stopped).
			Bool("paused", paused).
			Msg("workflow stopped or paused, skipping repair")
		return false
	}

	// Check if already repairing this session.
	if m.repairing[sessionID] {
		m.mu.Unlock()
		m.logger.Info().
			Str("session_id", sessionID).
			Msg("repair already in progress for session, skipping")
		return false
	}

	// Check cooldown.
	cooldown := m.config.cooldownDuration()
	if lastAttempt, ok := m.cooldowns[sessionID]; ok {
		elapsed := time.Since(lastAttempt)
		if elapsed < cooldown {
			m.mu.Unlock()
			m.logger.Info().
				Str("session_id", sessionID).
				Dur("cooldown_remaining", cooldown-elapsed).
				Msg("cooldown period not expired, skipping repair")
			return false
		}
	}

	repairCtx := m.ctx
	m.mu.Unlock()

	// Check whether the session has an active workflow (e.g. autopilot running).
	if !m.isSessionIdle(repairCtx, sessionID) {
		m.logger.Info().
			Str("session_id", sessionID).
			Msg("session has active workflow, skipping repair")
		return false
	}

	// Check that the session is in a state where repair makes sense.
	// Repair should only trigger once the session has reached the CI/review
	// cycle (awaiting_checks, fixing_checks, green_draft, ready_for_review).
	// In earlier states like implementing_plan the checks are expected to fail
	// because the code isn't finished yet; firing FixComplete would be invalid.
	repairable, repoName, sessionTitle := m.isSessionRepairable(repairCtx, sessionID)
	if !repairable {
		return false
	}

	// Re-acquire lock to mark as repairing. We must re-check all guards
	// because another goroutine may have completed a repair (setting a fresh
	// cooldown), or the workflow may have been stopped/restarted.
	m.mu.Lock()
	if m.stopped || m.paused || m.repairing[sessionID] {
		m.mu.Unlock()
		return false
	}
	if lastAttempt, ok := m.cooldowns[sessionID]; ok && time.Since(lastAttempt) < cooldown {
		m.mu.Unlock()
		return false
	}
	m.repairing[sessionID] = true
	repairCtx = m.ctx // Re-capture in case StartWorkflow replaced the context.
	m.mu.Unlock()

	// Trigger repair in background.
	go m.repairSession(repairCtx, sessionID, repoName, sessionTitle, displayStatus, hasFailures)

	return true
}

// isSessionIdle checks whether a session has any running workflows.
// Returns true if the session is idle (no active workflows), false otherwise.
// On error, returns false as a fail-safe (don't repair if we can't confirm idle).
func (m *repairMonitor) isSessionIdle(ctx context.Context, sessionID string) bool {
	resp, err := m.host.ListWorkflows(ctx, "running")
	if err != nil {
		m.logger.Warn().Err(err).
			Str("session_id", sessionID).
			Msg("failed to list workflows, assuming session is not idle")
		return false
	}

	for _, wf := range resp.GetWorkflows() {
		if wf.GetSessionId() == sessionID {
			m.logger.Debug().
				Str("session_id", sessionID).
				Str("workflow_id", wf.GetId()).
				Msg("found active workflow for session")
			return false
		}
	}
	return true
}

// isSessionRepairable checks whether the session's state machine is in a state
// where autonomous repair is appropriate. Returns (false, "", "") as a fail-safe
// if the session is in an early state like implementing_plan where check
// failures are expected, or if the state cannot be determined. On success it
// also returns the session's repo display name and title for log enrichment.
func (m *repairMonitor) isSessionRepairable(ctx context.Context, sessionID string) (bool, string, string) {
	resp, err := m.host.ListSessions(ctx)
	if err != nil {
		m.logger.Warn().Err(err).
			Str("session_id", sessionID).
			Msg("failed to list sessions, assuming not repairable")
		return false, "", ""
	}

	for _, sess := range resp.GetSessions() {
		if sess.GetId() != sessionID {
			continue
		}
		repoName := sess.GetRepoDisplayName()
		sessionTitle := sess.GetTitle()
		state := sess.GetState()
		switch state {
		case bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS,
			bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
			bossanovav1.SessionState_SESSION_STATE_GREEN_DRAFT,
			bossanovav1.SessionState_SESSION_STATE_READY_FOR_REVIEW:
			return true, repoName, sessionTitle
		default:
			m.logger.Info().
				Str("session_id", sessionID).
				Str("session_name", sessionTitle).
				Str("repo", repoName).
				Str("state", state.String()).
				Msg("session not in repairable state, skipping repair")
			return false, repoName, sessionTitle
		}
	}

	m.logger.Warn().
		Str("session_id", sessionID).
		Msg("session not found, assuming not repairable")
	return false, "", ""
}

// getSessionState returns the current state machine state for a session.
// Returns SESSION_STATE_UNSPECIFIED if the session is not found or on error.
func (m *repairMonitor) getSessionState(ctx context.Context, sessionID string) bossanovav1.SessionState {
	resp, err := m.host.ListSessions(ctx)
	if err != nil {
		return bossanovav1.SessionState_SESSION_STATE_UNSPECIFIED
	}
	for _, sess := range resp.GetSessions() {
		if sess.GetId() == sessionID {
			return sess.GetState()
		}
	}
	return bossanovav1.SessionState_SESSION_STATE_UNSPECIFIED
}

// advanceStuckSession checks if a session is stuck in ImplementingPlan and
// should be advanced to the next state. A session is considered stuck when:
// 1. It is in ImplementingPlan state
// 2. It has no active Claude Code chat (heartbeat tracker shows no live processes)
// 3. Its updated_at is older than the configured stuck timeout
// 4. It has no running workflows (is idle)
// Returns true if the session was advanced.
func (m *repairMonitor) advanceStuckSession(ctx context.Context, sess *bossanovav1.Session) bool {
	if sess.GetState() != bossanovav1.SessionState_SESSION_STATE_IMPLEMENTING_PLAN {
		return false
	}

	// Do not advance sessions when the workflow is stopped or paused.
	m.mu.Lock()
	if m.stopped || m.paused {
		m.mu.Unlock()
		return false
	}
	m.mu.Unlock()

	// Skip if the session has an active Claude Code chat process.
	if sess.GetHasActiveChat() {
		m.logger.Debug().
			Str("session_id", sess.GetId()).
			Msg("session has active chat, not advancing")
		return false
	}

	// Check the stuck timeout.
	m.mu.Lock()
	timeout := m.testStuckTimeout
	if timeout == 0 {
		timeout = m.config.stuckTimeout()
	}
	m.mu.Unlock()

	updatedAt := sess.GetUpdatedAt()
	if updatedAt == nil || time.Since(updatedAt.AsTime()) < timeout {
		return false
	}

	// Verify the session is idle (no running workflows).
	if !m.isSessionIdle(ctx, sess.GetId()) {
		m.logger.Info().
			Str("session_id", sess.GetId()).
			Msg("session has active workflow, not advancing stuck session")
		return false
	}

	// Fire PlanComplete to advance the state machine.
	m.logger.Info().
		Str("session_id", sess.GetId()).
		Str("title", sess.GetTitle()).
		Str("repo", sess.GetRepoDisplayName()).
		Dur("stuck_for", time.Since(updatedAt.AsTime())).
		Msg("advancing stuck ImplementingPlan session")

	if _, err := m.host.FireSessionEvent(ctx, &bossanovav1.FireSessionEventRequest{
		SessionId: sess.GetId(),
		Event:     bossanovav1.SessionEvent_SESSION_EVENT_PLAN_COMPLETE,
	}); err != nil {
		m.logger.Warn().Err(err).
			Str("session_id", sess.GetId()).
			Msg("failed to advance stuck session")
		return false
	}

	return true
}

// sweepExistingSessions queries all sessions and runs each through the repair
// logic. This catches sessions that were already in a bad state when the plugin
// started (or when a race caused the first notification to be dropped).
func (m *repairMonitor) sweepExistingSessions(ctx context.Context) {
	m.logger.Info().Msg("sweeping existing sessions for repairable state")

	resp, err := m.host.ListSessions(ctx)
	if err != nil {
		m.logger.Error().Err(err).Msg("failed to list sessions for sweep")
		return
	}

	// First pass: advance any sessions stuck in ImplementingPlan.
	var advanced int
	for _, sess := range resp.GetSessions() {
		if m.advanceStuckSession(ctx, sess) {
			advanced++
		}
	}

	// If any sessions were advanced, re-fetch so the second pass sees their
	// new state (e.g. AwaitingChecks) and can evaluate them for repair.
	if advanced > 0 {
		m.logger.Info().Int("advanced", advanced).Msg("re-fetching sessions after advancing stuck sessions")
		resp, err = m.host.ListSessions(ctx)
		if err != nil {
			m.logger.Error().Err(err).Msg("failed to re-list sessions after advancement")
			return
		}
	}

	// Second pass: evaluate each session for repair.
	for _, sess := range resp.GetSessions() {
		m.maybeRepair(sess.GetId(), sess.GetPrDisplayStatus(), sess.GetPrDisplayHasFailures())
	}

	m.logger.Info().
		Int("session_count", len(resp.GetSessions())).
		Int("advanced", advanced).
		Msg("sweep complete")
}

// periodicSweep runs sweepExistingSessions at a regular interval to catch
// sessions stuck in a repairable state that were missed by edge-triggered
// notifications (e.g. failed repair with no re-notification, or session
// transitioning to a repairable state after the initial notification).
func (m *repairMonitor) periodicSweep(ctx context.Context) {
	m.mu.Lock()
	interval := m.testSweepInterval
	if interval == 0 {
		interval = m.config.sweepInterval()
	}
	m.mu.Unlock()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.sweepExistingSessions(ctx)
		}
	}
}

// repairSession performs a repair attempt for a session in the background.
func (m *repairMonitor) repairSession(ctx context.Context, sessionID, repoName, sessionName string, displayStatus bossanovav1.PRDisplayStatus, hasFailures bool) {
	log := m.logger.With().Str("session_id", sessionID).Str("session_name", sessionName).Str("repo", repoName).Logger()

	// Cleanup on exit.
	defer func() {
		m.mu.Lock()
		delete(m.repairing, sessionID)
		m.cooldowns[sessionID] = time.Now()
		m.mu.Unlock()

		// Clear repair status so TUI reverts to the underlying PR status.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := m.host.SetRepairStatus(cleanupCtx, &bossanovav1.SetRepairStatusRequest{
			SessionId:   sessionID,
			IsRepairing: false,
		}); err != nil {
			log.Warn().Err(err).Msg("failed to clear repair status")
		}
	}()

	// Notify the daemon that repair is starting so the TUI shows "repairing".
	if _, err := m.host.SetRepairStatus(ctx, &bossanovav1.SetRepairStatusRequest{
		SessionId:   sessionID,
		IsRepairing: true,
	}); err != nil {
		log.Warn().Err(err).Msg("failed to set repair status")
	}

	log.Info().
		Int32("display_status", int32(displayStatus)).
		Bool("has_failures", hasFailures).
		Msg("starting repair attempt")

	// Read skill name from config under the lock.
	m.mu.Lock()
	skill := m.config.skillName()
	m.mu.Unlock()

	// The repair skill assesses the PR state itself, so we just invoke it
	// without additional context.
	prompt := "/" + skill

	// Create a workflow for this repair attempt so the daemon can resolve
	// the session's working directory for the Claude process.
	createResp, err := m.host.CreateWorkflow(ctx, &bossanovav1.CreateWorkflowRequest{
		SessionId: sessionID,
		MaxLegs:   1,
	})
	if err != nil {
		log.Error().Err(err).Msg("failed to create repair workflow")
		return
	}
	workflowID := createResp.GetWorkflow().GetId()
	log.Info().Str("workflow_id", workflowID).Msg("repair workflow created")

	attemptResp, err := m.host.CreateAttempt(ctx, &bossanovav1.CreateAttemptRequest{
		WorkflowId: workflowID,
		SkillName:  skill,
		Input:      prompt,
		WorkDir:    "", // daemon resolves from workflow's session
	})
	if err != nil {
		log.Error().Err(err).Msg("failed to create repair attempt")
		m.updateWorkflowStatus(ctx, workflowID, "failed", err.Error())
		return
	}

	attemptID := attemptResp.GetAttemptId()
	log.Info().Str("attempt_id", attemptID).Msg("repair attempt started")

	// Poll for completion.
	lastError := m.pollAttempt(ctx, attemptID)

	// Use a detached context for cleanup RPCs. If CancelWorkflow was called
	// during polling, ctx is already canceled and any RPC using it would fail,
	// leaving the workflow record stuck in "running" status. A short-lived
	// background context ensures cleanup always completes.
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cleanupCancel()

	if lastError != "" {
		log.Error().
			Str("error", lastError).
			Msg("repair attempt failed")
		m.updateWorkflowStatus(cleanupCtx, workflowID, "failed", lastError)
		return
	}

	// Success — update workflow status and fire a FixComplete event.
	log.Info().Msg("repair attempt completed successfully")
	m.updateWorkflowStatus(cleanupCtx, workflowID, "completed", "")

	// Fire FixComplete to fast-track the state transition, but only if the
	// session is in FixingChecks. In other states (e.g. ready_for_review,
	// awaiting_checks) the event is invalid and the normal polling loop will
	// handle the transition on its next cycle.
	if state := m.getSessionState(cleanupCtx, sessionID); state == bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS {
		if _, err := m.host.FireSessionEvent(cleanupCtx, &bossanovav1.FireSessionEventRequest{
			SessionId: sessionID,
			Event:     bossanovav1.SessionEvent_SESSION_EVENT_FIX_COMPLETE,
		}); err != nil {
			log.Warn().Err(err).Msg("FixComplete event failed; polling will handle transition")
		}
	} else {
		log.Debug().Str("state", state.String()).Msg("skipping FixComplete, session not in fixing_checks; polling will handle transition")
	}
}

// updateWorkflowStatus updates the workflow record with a final status.
func (m *repairMonitor) updateWorkflowStatus(ctx context.Context, workflowID, status, lastError string) {
	req := &bossanovav1.UpdateWorkflowRequest{
		Id:     workflowID,
		Status: stringPtr(status),
	}
	if lastError != "" {
		req.LastError = stringPtr(lastError)
	}
	if _, err := m.host.UpdateWorkflow(ctx, req); err != nil {
		m.logger.Error().Err(err).
			Str("workflow_id", workflowID).
			Str("target_status", status).
			Msg("failed to update workflow status")
	}
}

func stringPtr(s string) *string {
	return &s
}

// pollAttempt polls GetAttemptStatus until the attempt is no longer running.
// Returns the error string from the attempt (empty if successful).
func (m *repairMonitor) pollAttempt(ctx context.Context, attemptID string) string {
	m.mu.Lock()
	interval := m.config.pollInterval()
	m.mu.Unlock()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Sprintf("context canceled: %v", ctx.Err())
		case <-ticker.C:
			resp, err := m.host.GetAttemptStatus(ctx, attemptID)
			if err != nil {
				return fmt.Sprintf("get attempt status: %v", err)
			}

			switch resp.GetStatus() {
			case bossanovav1.AttemptRunStatus_ATTEMPT_RUN_STATUS_RUNNING:
				continue
			case bossanovav1.AttemptRunStatus_ATTEMPT_RUN_STATUS_COMPLETED:
				return ""
			case bossanovav1.AttemptRunStatus_ATTEMPT_RUN_STATUS_FAILED:
				return resp.GetError()
			default:
				return fmt.Sprintf("unexpected attempt status: %v", resp.GetStatus())
			}
		}
	}
}
