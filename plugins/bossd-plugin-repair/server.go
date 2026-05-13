package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// repairSkill is the skill the repair plugin invokes on Claude when a PR
// needs auto-repair. Hard-coded for now; making it overridable via
// repairConfig is a small follow-up if needed.
const repairSkill = "/boss-repair"

// repairCleanupTimeout bounds the detached cleanup context the repair
// goroutine uses for post-run RPCs (SetRepairStatus, FireSessionEvent,
// ListSessions). The original ctx may be cancelled by a daemon shutdown so
// cleanup must run on a fresh context.
const repairCleanupTimeout = 30 * time.Second

// repairStatusClearTimeout is a tighter detached timeout for the final
// "is_repairing=false" RPC the deferred cleanup fires.
const repairStatusClearTimeout = 5 * time.Second

const (
	// defaultCooldownDuration is the minimum time between repair attempts for the same session.
	defaultCooldownDuration = 1 * time.Minute
	// defaultSweepInterval is how often the periodic sweep runs.
	defaultSweepInterval = 1 * time.Minute
	// defaultStuckTimeout is how long a session must be in ImplementingPlan
	// before it is considered stuck and eligible for automatic advancement.
	defaultStuckTimeout = 5 * time.Minute
	// defaultForceAdvanceTimeout is how long a session with an active chat
	// must be stuck before force-advancing (zombie/idle chat suspected).
	defaultForceAdvanceTimeout = 1 * time.Hour
	// defaultIdleRepairThreshold is how long a session's chat must have been
	// quiet (no new output) before auto-repair is allowed to proceed despite
	// the chat process still being attached. Default is 5 minutes — the user
	// may have stepped away; we don't want to wait forever.
	defaultIdleRepairThreshold = 5 * time.Minute
)

// repairConfig holds parsed config for a repair workflow. Fields mirror
// config.RepairConfig but are local to the plugin to avoid importing
// the config package.
type repairConfig struct {
	Skills                     repairSkillOverrides `json:"skills,omitempty"`
	CooldownMinutes            int                  `json:"cooldown_minutes,omitempty"`
	PollIntervalSeconds        int                  `json:"poll_interval_seconds,omitempty"`
	SweepIntervalMinutes       int                  `json:"sweep_interval_minutes,omitempty"`
	StuckTimeoutMinutes        int                  `json:"stuck_timeout_minutes,omitempty"`
	ForceAdvanceTimeoutMinutes int                  `json:"force_advance_timeout_minutes,omitempty"`
	IdleRepairThresholdMinutes int                  `json:"idle_repair_threshold_minutes,omitempty"`
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

func (c *repairConfig) forceAdvanceTimeout() time.Duration {
	if c != nil && c.ForceAdvanceTimeoutMinutes > 0 {
		return time.Duration(c.ForceAdvanceTimeoutMinutes) * time.Minute
	}
	return defaultForceAdvanceTimeout
}

func (c *repairConfig) idleRepairThreshold() time.Duration {
	if c != nil && c.IdleRepairThresholdMinutes > 0 {
		return time.Duration(c.IdleRepairThresholdMinutes) * time.Minute
	}
	return defaultIdleRepairThreshold
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
//
// Locking discipline: every read or write of the mutable fields below
// (ctx, cancel, stopped, paused, config, repairing, cooldowns,
// lastAttemptCommit, lastAttemptDisplayStatus, and the test* overrides) MUST happen with mu held.
// In particular, anything derived from config (cooldownDuration,
// pollInterval, sweepInterval, stuckTimeout, forceAdvanceTimeout, skillName)
// must be re-read under the lock — values cached across an unlock are
// stale because StartWorkflow may have swapped the config pointer.
//
// Goroutine lifecycle: every goroutine spawned by the monitor (repairSession,
// sweepExistingSessions, periodicSweep) increments wg before launch and
// decrements via defer. Shutdown cancels the workflow ctx and waits on wg with
// a bounded timeout so the plugin process can exit cleanly without aborting an
// in-flight repair.
type repairMonitor struct {
	host   hostClient
	logger zerolog.Logger

	mu                       sync.Mutex
	ctx                      context.Context      // Workflow context
	cancel                   context.CancelFunc   // Cancel function for the workflow
	stopped                  bool                 // True after CancelWorkflow until next StartWorkflow
	paused                   bool                 // True after PauseWorkflow until ResumeWorkflow
	config                   *repairConfig        // Parsed config from StartWorkflowRequest
	repairing                map[string]bool      // Sessions currently being repaired
	cooldowns                map[string]time.Time // Last repair attempt time per session
	lastAttemptCommit        map[string]string    // Head SHA of last repair attempt per session
	lastAttemptDisplayStatus map[string]bossanovav1.DisplayStatus
	testSweepInterval        time.Duration // Override sweep interval for tests
	testStuckTimeout         time.Duration // Override stuck timeout for tests
	testForceAdvanceTimeout  time.Duration // Override force-advance timeout for tests

	// wg tracks every goroutine the monitor launches (repair sessions and
	// the two sweep loops). Shutdown cancels ctx and Waits on wg with a
	// timeout so we never abort a repair mid-write.
	wg sync.WaitGroup
}

func newRepairMonitor(host hostClient, logger zerolog.Logger) *repairMonitor {
	ctx, cancel := context.WithCancel(context.Background())
	return &repairMonitor{
		host:                     host,
		logger:                   logger,
		ctx:                      ctx,
		cancel:                   cancel,
		stopped:                  true, // Reject notifications until StartWorkflow sets config.
		repairing:                make(map[string]bool),
		cooldowns:                make(map[string]time.Time),
		lastAttemptCommit:        make(map[string]string),
		lastAttemptDisplayStatus: make(map[string]bossanovav1.DisplayStatus),
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
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.sweepExistingSessions(workflowCtx)
	}()

	// Periodically re-sweep to catch sessions stuck in a repairable state
	// after failed repairs or missed edge-triggered notifications.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.periodicSweep(workflowCtx)
	}()

	return &bossanovav1.StartWorkflowResponse{}, nil
}

// Shutdown cancels the monitor's workflow context and waits up to timeout for
// every tracked goroutine (in-flight repairs, sweep loops) to finish. Repair
// goroutines use detached cleanup contexts internally, so cancellation lets
// them complete the in-progress poll cycle and persist final state instead of
// being abandoned mid-write. Safe to call multiple times.
func (m *repairMonitor) Shutdown(timeout time.Duration) {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.stopped = true
	m.mu.Unlock()

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		m.logger.Info().Msg("repair monitor shutdown: all goroutines drained")
	case <-time.After(timeout):
		m.logger.Warn().Dur("timeout", timeout).Msg("repair monitor shutdown: timed out waiting for goroutines")
	}
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
func (m *repairMonitor) maybeRepair(sessionID string, displayStatus bossanovav1.DisplayStatus, hasFailures bool) {
	// Only trigger repair for failing, conflict, or rejected states.
	needsRepair := displayStatus == bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING ||
		displayStatus == bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT ||
		displayStatus == bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED

	if !needsRepair {
		return
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
		return
	}

	// Check if already repairing this session.
	if m.repairing[sessionID] {
		m.mu.Unlock()
		m.logger.Info().
			Str("session_id", sessionID).
			Msg("repair already in progress for session, skipping")
		return
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
			return
		}
	}

	repairCtx := m.ctx
	m.mu.Unlock()

	// Check that the session is in a state where repair makes sense.
	// The set depends on displayStatus:
	//   - FAILING / REJECTED: only meaningful in the CI/review cycle
	//     (awaiting_checks, fixing_checks, green_draft, ready_for_review),
	//     since checks and reviews don't run outside it.
	//   - CONFLICT: also includes finalizing — a merge conflict is
	//     independent of plan/lifecycle stage and the repair skill can
	//     resolve it from any live-PR state.
	// In earlier states like implementing_plan the checks are expected to fail
	// because the code isn't finished yet; firing FixComplete would be invalid.
	// Blocked stays excluded across the board: it is the manual-intervention
	// terminal state.
	info := m.lookupSession(repairCtx, sessionID, displayStatus)
	if !info.Repairable {
		return
	}

	// Idle gate: never interrupt a live agent chat with an automated repair —
	// unless the chat has been quiet for at least idleRepairThreshold. The
	// claude process can stay attached for days; we don't want a single
	// rejected PR to wait forever just because nobody closed the terminal.
	// HasActiveChat means the chat tracker has a fresh heartbeat (within
	// StaleThreshold = 15s); LastChatActivityAt is the max LastOutputAt across
	// the session's live chats. The next periodic sweep will retry; cooldown is
	// not recorded so the retry isn't throttled by this gate.
	m.mu.Lock()
	idleThreshold := m.config.idleRepairThreshold()
	m.mu.Unlock()

	if info.HasActiveChat {
		if info.LastChatActivityAt.IsZero() {
			// HasActiveChat=true but no activity timestamp — fail closed and
			// defer, since we can't tell how long the chat has been quiet.
			// (Should not happen once the host populates the field, but the
			// proto field is optional so a stale daemon could send true/empty.)
			m.logger.Info().
				Str("session_id", sessionID).
				Str("repo", info.RepoName).
				Str("session_name", info.SessionTitle).
				Msg("active chat in session with unknown activity timestamp, deferring repair")
			return
		}
		silentFor := time.Since(info.LastChatActivityAt)
		if silentFor < idleThreshold {
			m.logger.Info().
				Str("session_id", sessionID).
				Str("repo", info.RepoName).
				Str("session_name", info.SessionTitle).
				Dur("silent_for", silentFor).
				Dur("idle_threshold", idleThreshold).
				Msg("active chat in session, deferring repair to next sweep")
			return
		}
		m.logger.Info().
			Str("session_id", sessionID).
			Str("repo", info.RepoName).
			Str("session_name", info.SessionTitle).
			Dur("silent_for", silentFor).
			Dur("idle_threshold", idleThreshold).
			Msg("chat attached but idle past threshold, proceeding with repair")
	}

	repoName := info.RepoName
	sessionTitle := info.SessionTitle
	headSHA := info.HeadSHA
	if headSHA != "" &&
		info.LastRepairHeadSHA == headSHA &&
		info.LastRepairDisplayStatus == displayStatus &&
		info.LastRepairRunnerError == "" {
		m.logger.Info().
			Str("session_id", sessionID).
			Str("head_sha", headSHA).
			Int32("display_status", int32(displayStatus)).
			Msg("already attempted repair on this commit according to persisted diagnostics, skipping")
		return
	}

	// Re-acquire lock to mark as repairing and re-check every guard. The
	// world may have moved while we were doing RPCs without the lock:
	// StartWorkflow could have swapped m.config (so cooldownDuration must be
	// re-read, not reused), Cancel/Pause could have flipped stopped/paused,
	// another goroutine could have started repairing this session, and a
	// previous repair could have written lastAttemptCommit. Folding the
	// commit check into this same critical section keeps the
	// "decide → mark repairing" transition atomic.
	m.mu.Lock()
	if m.stopped || m.paused || m.repairing[sessionID] {
		m.mu.Unlock()
		return
	}
	if lastAttempt, ok := m.cooldowns[sessionID]; ok && time.Since(lastAttempt) < m.config.cooldownDuration() {
		m.mu.Unlock()
		return
	}
	if headSHA != "" && m.lastAttemptCommit[sessionID] == headSHA && m.lastAttemptDisplayStatus[sessionID] == displayStatus {
		m.mu.Unlock()
		m.logger.Info().
			Str("session_id", sessionID).
			Str("head_sha", headSHA).
			Int32("display_status", int32(displayStatus)).
			Msg("already attempted repair on this commit, skipping")
		return
	}
	m.repairing[sessionID] = true
	repairCtx = m.ctx // Re-capture in case StartWorkflow replaced the context.
	m.wg.Add(1)
	m.mu.Unlock()

	// Trigger repair in background. wg.Done is paired with the wg.Add(1)
	// above; repairSession's defer chain runs cleanup with a detached
	// context so Shutdown can wait for it without aborting writes.
	go func() {
		defer m.wg.Done()
		m.repairSession(repairCtx, sessionID, repoName, sessionTitle, displayStatus, hasFailures, headSHA)
	}()
}

// sessionInfo is the small bundle maybeRepair needs about a target session
// to make its decisions. Decoupling from the proto session type keeps the
// idle-gate logic isolated and easy to test.
type sessionInfo struct {
	Repairable              bool
	HasActiveChat           bool
	LastChatActivityAt      time.Time // zero if no live chat or unknown
	RepoName                string
	SessionTitle            string
	HeadSHA                 string
	LastRepairHeadSHA       string
	LastRepairDisplayStatus bossanovav1.DisplayStatus
	LastRepairRunnerError   string
}

// lookupSession resolves the daemon's view of the session and returns the
// fields maybeRepair needs. Returns Repairable=false as a fail-safe if the
// session is in a state where repair would be invalid for the given
// displayStatus (eg. implementing_plan where check failures are expected),
// if the state cannot be determined, or if the session is not found.
//
// The repairable-state set is keyed off displayStatus because the safety
// reasoning differs per signal — see isRepairableState.
func (m *repairMonitor) lookupSession(ctx context.Context, sessionID string, displayStatus bossanovav1.DisplayStatus) sessionInfo {
	resp, err := m.host.ListSessions(ctx)
	if err != nil {
		m.logger.Warn().Err(err).
			Str("session_id", sessionID).
			Msg("failed to list sessions, assuming not repairable")
		return sessionInfo{}
	}

	for _, sess := range resp.GetSessions() {
		if sess.GetId() != sessionID {
			continue
		}
		info := sessionInfo{
			HasActiveChat:           sess.GetHasActiveChat(),
			RepoName:                sess.GetRepoDisplayName(),
			SessionTitle:            sess.GetTitle(),
			HeadSHA:                 sess.GetPrDisplayHeadSha(),
			LastRepairHeadSHA:       sess.GetLastRepairHeadSha(),
			LastRepairDisplayStatus: sess.GetLastRepairDisplayStatus(),
			LastRepairRunnerError:   sess.GetLastRepairRunnerError(),
		}
		if ts := sess.GetLastChatActivityAt(); ts != nil {
			info.LastChatActivityAt = ts.AsTime()
		}
		state := sess.GetState()
		if isRepairableState(state, displayStatus) {
			info.Repairable = true
			return info
		}
		m.logger.Info().
			Str("session_id", sessionID).
			Str("session_name", info.SessionTitle).
			Str("repo", info.RepoName).
			Str("state", state.String()).
			Str("display_status", displayStatus.String()).
			Msg("session not in repairable state, skipping repair")
		return info
	}

	m.logger.Warn().
		Str("session_id", sessionID).
		Msg("session not found, assuming not repairable")
	return sessionInfo{}
}

// isRepairableState answers "is it safe and meaningful to run /boss-repair
// on this (state, displayStatus) pair?".
//
//   - FAILING / REJECTED: the signal only exists in the CI/review cycle, so
//     repair is gated to those states. Outside the cycle the agent has
//     nothing to look at.
//   - CONFLICT: a PR can become unmergeable at any post-PR lifecycle stage
//     once main moves; the repair skill rebases-and-pushes regardless of
//     plan-completion or finalize state. We therefore also allow Finalizing.
//
// Blocked stays excluded everywhere: it is the manual-intervention terminal
// state and auto-repair would defeat the max-attempts guardrail.
// ImplementingPlan stays excluded: the idle-chat heuristic owns that phase.
func isRepairableState(state bossanovav1.SessionState, displayStatus bossanovav1.DisplayStatus) bool {
	switch state {
	case bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS,
		bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
		bossanovav1.SessionState_SESSION_STATE_GREEN_DRAFT,
		bossanovav1.SessionState_SESSION_STATE_READY_FOR_REVIEW:
		return true
	case bossanovav1.SessionState_SESSION_STATE_FINALIZING:
		return displayStatus == bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT
	default:
		return false
	}
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

	// Extract updatedAt early so both hasActiveChat and stuck timeout can use it.
	updatedAt := sess.GetUpdatedAt()

	// Skip if the session has an active Claude Code chat process,
	// unless stuck far beyond normal timeout (zombie/idle chat).
	if sess.GetHasActiveChat() {
		m.mu.Lock()
		forceTimeout := m.testForceAdvanceTimeout
		if forceTimeout == 0 {
			forceTimeout = m.config.forceAdvanceTimeout()
		}
		m.mu.Unlock()

		if updatedAt == nil || time.Since(updatedAt.AsTime()) < forceTimeout {
			m.logger.Debug().
				Str("session_id", sess.GetId()).
				Msg("session has active chat, not advancing")
			return false
		}
		m.logger.Warn().
			Str("session_id", sess.GetId()).
			Str("title", sess.GetTitle()).
			Dur("stuck_for", time.Since(updatedAt.AsTime())).
			Msg("force-advancing stuck session despite active chat (zombie process suspected)")
	} else {
		// Check the stuck timeout (for sessions WITHOUT active chat).
		m.mu.Lock()
		timeout := m.testStuckTimeout
		if timeout == 0 {
			timeout = m.config.stuckTimeout()
		}
		m.mu.Unlock()

		if updatedAt == nil || time.Since(updatedAt.AsTime()) < timeout {
			return false
		}
	}

	// Fire PlanComplete to advance the state machine. With host-side
	// workflow CRUD removed, the daemon's per-session active-run mutex on
	// StartClaudeRun is the only source of truth for "session has work in
	// flight" — we no longer have a host-side dedup query to consult here.
	// In practice the active-chat heuristic above already gates this path.
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
		m.maybeRepair(sess.GetId(), sess.GetDisplayStatus(), sess.GetDisplayHasFailures())
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

// repairSession drives a single Claude repair run for a failing/conflicted/
// rejected session. It asks the daemon to spawn a Claude process in the
// session's worktree (StartClaudeRun), waits for it to exit (WaitClaudeRun),
// and on success fires FIX_COMPLETE so the state machine fast-tracks past
// FIXING_CHECKS instead of waiting for the next display poll.
//
// Concurrency: the caller holds the maybeRepair guard (m.repairing), but the
// daemon also enforces one-active-Claude-per-session via StartClaudeRun.
// Losing that race surfaces as AlreadyExists and is treated as a soft skip:
// no cooldown recorded, no IsRepairing flag set, so the winner's cleanup
// owns both. attemptRan controls whether lastAttemptCommit is updated:
// a completed agent run, even with a non-zero exit, blocks same-head/status
// retries; StartChatRun and WaitChatRun infrastructure failures stay retryable.
func (m *repairMonitor) repairSession(
	ctx context.Context,
	sessionID, repoName, sessionName string,
	displayStatus bossanovav1.DisplayStatus,
	hasFailures bool,
	headSHA string,
) {
	log := m.logger.With().
		Str("session_id", sessionID).
		Str("session_name", sessionName).
		Str("repo", repoName).
		Logger()

	attemptRan := false
	runOwned := false
	repairFlagSet := false
	startedAt := time.Now()
	var (
		outcomeAgentSessionID string
		outcomeRunnerError    string
		outcomeExitError      string
		outcomeShouldRecord   bool
	)

	defer func() {
		m.mu.Lock()
		delete(m.repairing, sessionID)
		if runOwned {
			m.cooldowns[sessionID] = time.Now()
		}
		if attemptRan && headSHA != "" {
			m.lastAttemptCommit[sessionID] = headSHA
			m.lastAttemptDisplayStatus[sessionID] = displayStatus
		}
		m.mu.Unlock()

		// Persist the outcome onto the session row so the TUI can surface
		// "failing ⚠ repair: claude not in PATH (3×)" style hints. We skip
		// the record on AlreadyExists soft-skip (where outcomeShouldRecord
		// stays false) so the losing instance doesn't double-bump the
		// session's attempt count.
		if outcomeShouldRecord {
			func() {
				outcomeCtx, outcomeCancel := context.WithTimeout(context.Background(), repairCleanupTimeout)
				defer outcomeCancel()
				if _, err := m.host.RecordRepairOutcome(outcomeCtx, &bossanovav1.RecordRepairOutcomeRequest{
					SessionId:      sessionID,
					StartedAtUnix:  startedAt.Unix(),
					RunnerError:    outcomeRunnerError,
					ExitError:      outcomeExitError,
					AgentSessionId: outcomeAgentSessionID,
					HeadSha:        headSHA,
					DisplayStatus:  displayStatus,
				}); err != nil {
					log.Warn().Err(err).Msg("failed to record repair outcome")
				}
			}()
		}

		if !repairFlagSet {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), repairStatusClearTimeout)
		defer cancel()
		if _, err := m.host.SetRepairStatus(cleanupCtx, &bossanovav1.SetRepairStatusRequest{
			SessionId:   sessionID,
			IsRepairing: false,
		}); err != nil {
			log.Warn().Err(err).Msg("failed to clear repair status")
		}
	}()

	log.Info().
		Int32("display_status", int32(displayStatus)).
		Bool("has_failures", hasFailures).
		Msg("starting repair attempt")

	startResp, err := m.host.StartChatRun(ctx, &bossanovav1.StartChatRunHostRequest{
		SessionId: sessionID,
		Prompt:    repairSkill,
		Title:     "Repair: " + sessionName,
	})
	if err != nil {
		if grpcstatus.Code(err) == codes.AlreadyExists {
			// Soft skip — another instance owns this run, do not record an
			// outcome here (the winner's deferred cleanup will).
			log.Info().Err(err).Msg("another repair run is active, skipping repair")
			return
		}
		// Daemon-side StartChatRun refusal (eg. "claude not on PATH",
		// "agent client not configured"). Record so the TUI surfaces the
		// reason instead of the operator having to grep daemon stderr.
		outcomeRunnerError = err.Error()
		outcomeShouldRecord = true
		log.Error().Err(err).Msg("failed to start repair chat run")
		return
	}
	runOwned = true
	outcomeShouldRecord = true
	agentSessionID := startResp.GetAgentSessionId()
	outcomeAgentSessionID = agentSessionID
	log.Info().Str("agent_session_id", agentSessionID).Msg("repair chat run started")

	// Set IsRepairing only after we own the run, so a losing instance does
	// not clobber the winner's flag in its deferred cleanup.
	if _, err := m.host.SetRepairStatus(ctx, &bossanovav1.SetRepairStatusRequest{
		SessionId:   sessionID,
		IsRepairing: true,
	}); err != nil {
		log.Warn().Err(err).Msg("failed to set repair status")
	} else {
		repairFlagSet = true
	}

	waitResp, waitErr := m.host.WaitChatRun(ctx, &bossanovav1.WaitChatRunHostRequest{
		AgentSessionId: agentSessionID,
	})

	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), repairCleanupTimeout)
	defer cleanupCancel()

	if waitErr != nil {
		// Treat WaitChatRun gRPC errors as runner-class so the TUI sees
		// "the agent process couldn't be observed" rather than an empty
		// status field.
		outcomeRunnerError = "wait for repair run failed: " + waitErr.Error()
		log.Error().Err(waitErr).Msg("wait for repair run failed")
		return
	}
	// The agent process ran and reported an outcome. Whether it exits clean
	// or non-zero, do not rerun the same head/status forever.
	attemptRan = true
	if exitErr := waitResp.GetExitError(); exitErr != "" {
		outcomeExitError = exitErr
		log.Error().Str("error", exitErr).Msg("repair attempt failed")
		return
	}

	log.Info().Msg("repair attempt completed successfully")

	// FIX_COMPLETE is only valid in FIXING_CHECKS; in other states the
	// next polling cycle handles the transition. With CreateWorkflow gone
	// we no longer have a session-state lookup helper — list and find.
	sessionsResp, err := m.host.ListSessions(cleanupCtx)
	if err != nil {
		log.Warn().Err(err).Msg("post-repair ListSessions failed; polling will handle transition")
		return
	}
	var st bossanovav1.SessionState
	for _, s := range sessionsResp.GetSessions() {
		if s.GetId() == sessionID {
			st = s.GetState()
			break
		}
	}
	if st == bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS {
		if _, err := m.host.FireSessionEvent(cleanupCtx, &bossanovav1.FireSessionEventRequest{
			SessionId: sessionID,
			Event:     bossanovav1.SessionEvent_SESSION_EVENT_FIX_COMPLETE,
		}); err != nil {
			log.Warn().Err(err).Msg("FixComplete event failed; polling will handle transition")
		}
	} else {
		log.Debug().Str("state", st.String()).Msg("not in fixing_checks, skipping FixComplete")
	}
}
