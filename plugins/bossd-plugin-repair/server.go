package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// repairCommand is the boss command the repair plugin asks the active coding
// agent to run. The daemon formats it with the agent runner's command prefix.
const repairCommand = "boss-repair"

var alreadyExistsAgentSessionRE = regexp.MustCompile(`agent_session_id=([^)]+)`)

func isInteractiveChatIncomplete(exitErr string) bool {
	return strings.Contains(exitErr, "interactive chat did not report completion") ||
		// The daemon's WaitChatRun idle watchdog (BOS-153) fails a run whose
		// agent went idle without ever reporting completion. Like the
		// wait-deadline case, the agent never actually attempted the repair,
		// so a same-head/status retry must stay allowed.
		strings.Contains(exitErr, "agent went idle without completing")
}

func agentSessionIDFromAlreadyExists(err error) string {
	if err == nil {
		return ""
	}
	matches := alreadyExistsAgentSessionRE.FindStringSubmatch(err.Error())
	if len(matches) != 2 {
		return ""
	}
	return matches[1]
}

// repairCleanupTimeout bounds the detached cleanup context the repair
// goroutine uses for post-run RPCs (SetRepairStatus, FireSessionEvent,
// ListSessions). The original ctx may be cancelled by a daemon shutdown so
// cleanup must run on a fresh context.
const repairCleanupTimeout = 30 * time.Second

// repairStatusClearTimeout is a tighter detached timeout for the final
// "is_repairing=false" RPC the deferred cleanup fires.
const repairStatusClearTimeout = 5 * time.Second

const (
	// defaultCooldownDuration is the minimum time between repair attempts
	// for the same session. The actual wait between attempts is then
	// extended by exponential backoff via cooldownFor — see that helper
	// for the schedule. This default is the base of the exponent and the
	// post-success / post-restart cooldown.
	defaultCooldownDuration = 1 * time.Minute
	// defaultSweepInterval is how often the periodic sweep runs.
	defaultSweepInterval = 1 * time.Minute
	// maxCooldownDuration caps the exponential backoff so a long-running
	// hammer loop on a genuinely-broken PR never waits more than 30 m
	// between attempts. The previous code had no cap and a flat 1 m
	// cooldown; that combination racked up 321 attempts over five hours
	// on the Dependabot PRs that motivated this fix. With the cap +
	// backoff: steady-state cadence is one attempt per 30 m (~48/day),
	// down from one per minute (~1440/day).
	maxCooldownDuration = 30 * time.Minute
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

	defaultPostRepairPollInterval = 2 * time.Second
	defaultPostRepairWaitTimeout  = 30 * time.Minute
	maxRepairLoopAttempts         = 5

	// defaultRunIdleFailAfter arms the daemon's WaitChatRun idle watchdog
	// (BOS-153): if the repair agent's chat is continuously idle for this
	// long the daemon fails the run fast and reclaims the dead pane instead
	// of burning the full 30m wait deadline. It mirrors the daemon's
	// repairDisplaceMinIdle (5m) so the "idle enough to displace" and "idle
	// enough to fail the run" thresholds stay aligned.
	defaultRunIdleFailAfter = 5 * time.Minute
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
	PostRepairPollMilliseconds int                  `json:"post_repair_poll_milliseconds,omitempty"`
	PostRepairWaitMilliseconds int                  `json:"post_repair_wait_milliseconds,omitempty"`
}

type repairSkillOverrides struct {
	Repair string `json:"repair,omitempty"`
}

type postRepairStatus string

const (
	postRepairStatusClean       postRepairStatus = "clean"
	postRepairStatusPending     postRepairStatus = "pending"
	postRepairStatusNeedsRepair postRepairStatus = "needs_repair"
	postRepairStatusUnknown     postRepairStatus = "unknown"
)

type postRepairAssessment struct {
	Status                     postRepairStatus
	Reason                     string
	DisplayStatus              bossanovav1.DisplayStatus
	HasFailures                bool
	ReviewFingerprint          string
	ReviewFingerprintAvailable bool
	HeadSHA                    string
}

func (c *repairConfig) cooldownDuration() time.Duration {
	if c != nil && c.CooldownMinutes > 0 {
		return time.Duration(c.CooldownMinutes) * time.Minute
	}
	return defaultCooldownDuration
}

// cooldownFor returns the wait that must elapse since the last repair
// attempt for this session before another attempt may fire, accounting
// for exponential backoff on consecutive failures.
//
// Schedule with the 1-minute default base:
//
//	attemptCount   wait
//	0  (no fail)   base (just the floor; counter resets on a clean run)
//	1              base * 1   = 1 m
//	2              base * 2   = 2 m
//	3              base * 4   = 4 m
//	4              base * 8   = 8 m
//	5              base * 16  = 16 m
//	6+             capped at maxCooldownDuration (30 m)
//
// base is the configured floor — operators who configure a longer
// minimum get a proportionally longer schedule until the next doubled
// value reaches maxCooldownDuration. Larger bases therefore hit the cap
// sooner; for example, base=15 m yields 15 m, 30 m, then 30 m thereafter.
//
// Blocked start-refusals (BOS-153) never advance attemptCount: they are
// recorded in the separate repair-blocked lane and floored at the base
// cooldown only, so a deterministic daemon refusal cannot escalate this
// schedule toward the cap.
func cooldownFor(attemptCount int32, base time.Duration) time.Duration {
	if base <= 0 || base >= maxCooldownDuration {
		return maxCooldownDuration
	}
	if attemptCount <= 0 {
		return base
	}
	const maxShift = 16 // 2^16 is well past the cap for supported bases.
	shift := int(attemptCount) - 1
	if shift > maxShift {
		shift = maxShift
	}
	factor := time.Duration(1) << shift
	if base > maxCooldownDuration/factor {
		return maxCooldownDuration
	}
	return base * factor
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

func (c *repairConfig) postRepairPollInterval() time.Duration {
	if c != nil && c.PostRepairPollMilliseconds > 0 {
		return time.Duration(c.PostRepairPollMilliseconds) * time.Millisecond
	}
	return defaultPostRepairPollInterval
}

func (c *repairConfig) postRepairWaitTimeout() time.Duration {
	if c != nil && c.PostRepairWaitMilliseconds > 0 {
		return time.Duration(c.PostRepairWaitMilliseconds) * time.Millisecond
	}
	return defaultPostRepairWaitTimeout
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
// lastAttempt, refusals, baseMoves, and the test* overrides) MUST happen with mu held.
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
	bossanovav1.UnimplementedWorkflowServiceServer
	host   hostClient
	logger zerolog.Logger

	// dispatcherID is this repair-plugin instance's stable identity, sent on
	// SetRepairStatus so the daemon's single-repairer lease is taken for and
	// released by this dispatcher. Re-entrant across a session's repair passes
	// (same id), and distinct from any other repair dispatcher (a /boss-repair
	// chat or a boss-epic driver), which is refused while this lease is held.
	dispatcherID string

	mu                      sync.Mutex
	ctx                     context.Context        // Workflow context
	cancel                  context.CancelFunc     // Cancel function for the workflow
	stopped                 bool                   // True after CancelWorkflow until next StartWorkflow
	paused                  bool                   // True after PauseWorkflow until ResumeWorkflow
	config                  *repairConfig          // Parsed config from StartWorkflowRequest
	repairing               map[string]bool        // Sessions currently being repaired
	cooldowns               map[string]time.Time   // Last repair attempt time per session
	lastAttempt             map[string]repairInput // Input of the last repair attempt that ran, per session
	refusals                map[string]refusalStreak
	baseMoves               map[string]baseMoveStreak
	testSweepInterval       time.Duration // Override sweep interval for tests
	testStuckTimeout        time.Duration // Override stuck timeout for tests
	testForceAdvanceTimeout time.Duration // Override force-advance timeout for tests

	// wg tracks every goroutine the monitor launches (repair sessions and
	// the two sweep loops). Shutdown cancels ctx and Waits on wg with a
	// timeout so we never abort a repair mid-write.
	wg sync.WaitGroup
}

// newDispatcherID mints a process-stable identity for this repair-plugin
// instance's single-repairer lease. crypto/rand gives a collision-free id
// across restarts; a pid-based fallback keeps the plugin working if the entropy
// source is somehow unavailable (the value only needs to be stable per process
// and distinct from other dispatchers).
func newDispatcherID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("repair-plugin-%d", os.Getpid())
	}
	return "repair-plugin-" + hex.EncodeToString(b[:])
}

func newRepairMonitor(host hostClient, logger zerolog.Logger) *repairMonitor {
	ctx, cancel := context.WithCancel(context.Background())
	return &repairMonitor{
		host:         host,
		logger:       logger,
		dispatcherID: newDispatcherID(),
		ctx:          ctx,
		cancel:       cancel,
		stopped:      true, // Reject notifications until StartWorkflow sets config.
		repairing:    make(map[string]bool),
		cooldowns:    make(map[string]time.Time),
		lastAttempt:  make(map[string]repairInput),
		refusals:     make(map[string]refusalStreak),
		baseMoves:    make(map[string]baseMoveStreak),
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
	// Only trigger repair for failing, conflict, or rejected states. The verify
	// holds (VERIFYING, NEEDS_HUMAN) are deliberately not triggers, even with an
	// open changes-requested review: the verify stage owns a held head, and a
	// verify failure re-enters here as FAILING. hasUnresolvedReviewFeedback only
	// matters after a repair has started, so inside this trigger path review
	// feedback means exactly REJECTED, which the duplicate-attempt guards below
	// rely on when they compare review fingerprints.
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

	// Fast-path cooldown check on the in-memory map. The map only tracks
	// attempts from the current process so this is just a "did we
	// already attempt very recently" optimization — the authoritative
	// exponential-backoff gate runs after lookupSession, using the
	// persisted last_repair_started_at / last_repair_attempt_count so
	// the schedule survives daemon restarts. The base cooldown is also
	// the floor that the exponential is built from, capped to match the
	// persisted cooldown path below.
	baseCooldown := cooldownFor(0, m.config.cooldownDuration())
	if lastAttempt, ok := m.cooldowns[sessionID]; ok {
		elapsed := time.Since(lastAttempt)
		if elapsed < baseCooldown {
			m.mu.Unlock()
			m.logger.Info().
				Str("session_id", sessionID).
				Dur("cooldown_remaining", baseCooldown-elapsed).
				Msg("base cooldown not expired (in-memory), skipping repair")
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
		m.clearRefusals(sessionID)
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

	replaceExistingReason := ""
	var replaceExistingObservedLastChatActivityAt time.Time
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
			m.clearRefusals(sessionID)
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
		replaceExistingReason = fmt.Sprintf(
			"auto-repair replacing idle chat after %s of silence; threshold %s",
			silentFor.Round(time.Second),
			idleThreshold,
		)
		replaceExistingObservedLastChatActivityAt = info.LastChatActivityAt
	}

	repoName := info.RepoName
	sessionTitle := info.SessionTitle
	headSHA := info.HeadSHA

	if reason, exhausted := repairExhaustedReason(info); exhausted {
		m.blockSession(repairCtx, sessionID, reason)
		return
	}

	// Authoritative exponential-backoff gate. Sourced from persisted
	// last_repair_started_at + last_repair_attempt_count so a daemon
	// restart can't reset the schedule and let the next sweep fire an
	// immediate retry — that was the regression behind the 321× counter
	// on the production Dependabot PRs. The in-memory `m.cooldowns`
	// fast-path above is just an "already attempted this minute"
	// short-circuit; this is the real wait. It runs before the review
	// fingerprint read below, which costs a GitHub call: a session sitting
	// out a 30-minute backoff would otherwise pay that call on every
	// one-minute sweep before being skipped.
	if info.LastRepairAttemptCount > 0 && !info.LastRepairStartedAt.IsZero() {
		wait := cooldownFor(info.LastRepairAttemptCount, baseCooldown)
		elapsed := time.Since(info.LastRepairStartedAt)
		if elapsed < wait {
			m.logger.Info().
				Str("session_id", sessionID).
				Int32("attempt_count", info.LastRepairAttemptCount).
				Dur("wait", wait).
				Dur("elapsed", elapsed).
				Dur("remaining", wait-elapsed).
				Msg("exponential backoff active, skipping repair")
			return
		}
	}

	input, persisted := m.buildRepairInput(repairCtx, sessionID, info, headSHA, displayStatus)
	reviewFP, reviewFPOk := input.reviewFP, input.reviewFPOk
	baseSHA, baseOk := input.baseSHA, input.baseOk
	baseMoveRetry := false
	if info.LastRepairRunnerError == "" && !isInteractiveChatIncomplete(info.LastRepairExitError) {
		if input.repeats(persisted) {
			m.logger.Info().
				Str("session_id", sessionID).
				Str("head_sha", headSHA).
				Int32("display_status", int32(displayStatus)).
				Str("review_fingerprint", reviewFP).
				Bool("review_fingerprint_available", reviewFPOk).
				Str("base_sha", baseSHA).
				Bool("base_sha_available", baseOk).
				Msg("already attempted repair on this input according to persisted diagnostics, skipping")
			return
		}
		if input.sameHeadAndStatus(persisted) {
			m.logger.Info().
				Str("session_id", sessionID).
				Str("head_sha", headSHA).
				Int32("display_status", int32(displayStatus)).
				Str("old_review_fingerprint", persisted.reviewFP).
				Str("new_review_fingerprint", reviewFP).
				Str("old_base_sha", persisted.baseSHA).
				Str("new_base_sha", baseSHA).
				Msg("repair input moved on the same head; allowing repair")
			baseMoveRetry = attemptBaseSHA(displayStatus, baseSHA) != ""
		}
	}

	// Re-acquire lock to mark as repairing and re-check every guard. The
	// world may have moved while we were doing RPCs without the lock:
	// StartWorkflow could have swapped m.config (so the capped base cooldown
	// must be re-read, not reused), Cancel/Pause could have flipped stopped/paused,
	// another goroutine could have started repairing this session, and a
	// previous repair could have written lastAttempt. Folding the
	// input check into this same critical section keeps the
	// "decide → mark repairing" transition atomic.
	m.mu.Lock()
	if m.stopped || m.paused || m.repairing[sessionID] {
		m.mu.Unlock()
		return
	}
	if lastAttempt, ok := m.cooldowns[sessionID]; ok && time.Since(lastAttempt) < cooldownFor(0, m.config.cooldownDuration()) {
		m.mu.Unlock()
		return
	}
	if input.repeats(m.lastAttempt[sessionID]) {
		m.mu.Unlock()
		m.logger.Info().
			Str("session_id", sessionID).
			Str("head_sha", headSHA).
			Int32("display_status", int32(displayStatus)).
			Str("review_fingerprint", reviewFP).
			Bool("review_fingerprint_available", reviewFPOk).
			Str("base_sha", baseSHA).
			Bool("base_sha_available", baseOk).
			Msg("already attempted repair on this input, skipping")
		return
	}
	if streak, exhausted := m.noteBaseMoveRetryLocked(sessionID, input, baseMoveRetry); exhausted {
		m.mu.Unlock()
		m.blockSession(repairCtx, sessionID, baseMoveExhaustedReason(displayStatus, headSHA, streak.count))
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
		m.repairSession(repairCtx, sessionID, repoName, sessionTitle, displayStatus, hasFailures, headSHA, replaceExistingReason, replaceExistingObservedLastChatActivityAt, reviewFP, reviewFPOk, m.baseSHAForAttempt(repairCtx, info, input))
	}()
}

// repairExhaustedReason reports whether automatic repair has failed
// maxRepairLoopAttempts times in a row on the session's current head, the same
// count the repair-exhausted alert uses, and the reason to block it with. The
// persisted counter survives restarts and is reset by a clean run or a head
// move, so it already counts consecutive failures on one head.
func repairExhaustedReason(info sessionInfo) (string, bool) {
	if info.LastRepairAttemptCount < maxRepairLoopAttempts || info.HeadSHA == "" || info.LastRepairHeadSHA != info.HeadSHA {
		return "", false
	}
	last := info.LastRepairExitError
	if last == "" {
		last = info.LastRepairRunnerError
	}
	return fmt.Sprintf("automatic repair failed %d times in a row on %s; last error: %s",
		info.LastRepairAttemptCount, shortSHA(info.HeadSHA), firstLine(last)), true
}

// refusalStreak counts consecutive start refusals (the daemon would not start
// the repair chat) on one head. It is in memory: a plugin restart starts a
// fresh streak, which only delays escalation. A started run, a head move, a
// sweep that finds the session attended (its chat active inside the idle
// threshold) or outside the repairable states (Blocked included) ends it, so
// refusals from a tracker warming after a restart cannot combine with a lone
// refusal half an hour later into a block on a session someone is working in.
type refusalStreak struct {
	headSHA string
	first   time.Time
	count   int
}

// repairRefusalEscalateAfter is how long refusals on one head must persist
// before repair gives up. Refusals the daemon expects to clear by themselves
// (a tracker still warming after a restart, a chat not yet idle long enough,
// a question younger than its 15m window) all clear well inside it.
const repairRefusalEscalateAfter = 30 * time.Minute

// noteRefusal records one start refusal and reports whether the streak now
// warrants blocking the session: maxRepairLoopAttempts refusals in a row on the
// same head, spread over at least repairRefusalEscalateAfter.
func (m *repairMonitor) noteRefusal(sessionID, headSHA string, now time.Time) (refusalStreak, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	streak := m.refusals[sessionID]
	if streak.count == 0 || streak.headSHA != headSHA {
		streak = refusalStreak{headSHA: headSHA, first: now}
	}
	streak.count++
	m.refusals[sessionID] = streak
	return streak, headSHA != "" && streak.count >= maxRepairLoopAttempts && now.Sub(streak.first) >= repairRefusalEscalateAfter
}

func (m *repairMonitor) clearRefusals(sessionID string) {
	m.mu.Lock()
	delete(m.refusals, sessionID)
	m.mu.Unlock()
}

// runRepairAttempt drives a single Claude repair run for a failing/conflicted/
// rejected session. It asks the daemon to spawn a Claude process in the
// session's worktree (StartClaudeRun), waits for it to exit (WaitClaudeRun),
// and on success fires FIX_COMPLETE so the state machine fast-tracks past
// FIXING_CHECKS instead of waiting for the next display poll.
//
// Concurrency: the caller holds the maybeRepair guard (m.repairing), but the
// daemon also enforces one-active-Claude-per-session via StartClaudeRun.
// Losing that race surfaces as AlreadyExists and is treated as a soft skip:
// no cooldown recorded, no IsRepairing flag set, so the winner's cleanup
// owns both. attemptRan controls whether lastAttempt is updated:
// a completed agent run, even with a non-zero exit, blocks same-head/status
// retries; StartChatRun and WaitChatRun infrastructure failures stay retryable.
func (m *repairMonitor) runRepairAttempt(
	ctx context.Context,
	sessionID, repoName, sessionName string,
	displayStatus bossanovav1.DisplayStatus,
	hasFailures bool,
	headSHA string,
	replaceExistingReason string,
	replaceExistingObservedLastChatActivityAt time.Time,
	reviewFingerprint string,
	reviewFingerprintAvailable bool,
	baseSHA string,
	resumeAgentSessionID string,
	repairFlagSet *bool,
) (string, bool) {
	log := m.logger.With().
		Str("session_id", sessionID).
		Str("session_name", sessionName).
		Str("repo", repoName).
		Logger()

	attemptRan := false
	runOwned := false
	startedAt := time.Now()
	var (
		outcomeAgentSessionID string
		outcomeRunnerError    string
		outcomeExitError      string
		outcomeBlockedReason  string
		outcomeShouldRecord   bool
	)

	defer func() {
		m.mu.Lock()
		if runOwned {
			m.cooldowns[sessionID] = time.Now()
		}
		if attemptRan && headSHA != "" {
			attempted := repairInput{
				headSHA:       headSHA,
				displayStatus: displayStatus,
				reviewFP:      m.lastAttempt[sessionID].reviewFP,
				baseSHA:       baseSHA,
			}
			if reviewFingerprintAvailable {
				attempted.reviewFP = reviewFingerprint
			}
			m.lastAttempt[sessionID] = attempted
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
				req := &bossanovav1.RecordRepairOutcomeRequest{
					SessionId:      sessionID,
					StartedAtUnix:  startedAt.Unix(),
					RunnerError:    outcomeRunnerError,
					ExitError:      outcomeExitError,
					BlockedReason:  outcomeBlockedReason,
					AgentSessionId: outcomeAgentSessionID,
					HeadSha:        headSHA,
					DisplayStatus:  displayStatus,
					BaseSha:        baseSHA,
				}
				// The blocked lane is mutually exclusive with a real
				// runner/exit outcome: a start-refusal never ran an agent, so
				// it must not carry a runner or exit error (the daemon keys off
				// BlockedReason to record the blocked state without bumping the
				// failure count — BOS-153 Task 4).
				if outcomeBlockedReason != "" {
					req.RunnerError = ""
					req.ExitError = ""
				}
				if reviewFingerprintAvailable {
					req.ReviewFingerprint = &reviewFingerprint
				}
				if _, err := m.host.RecordRepairOutcome(outcomeCtx, req); err != nil {
					log.Warn().Err(err).Msg("failed to record repair outcome")
				}
			}()
		}
		// The single-repairer lease taken below (SetRepairStatus IsRepairing=true)
		// is intentionally NOT cleared here: repairSession holds it across this
		// attempt and the following post-repair assessment window, releasing it
		// once the whole bounded loop exits.
	}()

	// markStartRefused records daemon refusals (gRPC FailedPrecondition — eg.
	// the target chat is still working, the agent is not configured, or reclaim
	// refused to displace a live repair chat) into the separate "blocked" lane
	// rather than the failure lane (BOS-153). It stamps ONLY the base in-memory
	// cooldown so refusal retries are floored at the base cadence without
	// triggering the exponential backoff that real failures do (the blocked
	// record never bumps last_repair_attempt_count, so cooldownFor sees only
	// real failures). No agent ran, so the caller returns ("", false).
	markStartRefused := func(err error) {
		outcomeBlockedReason = grpcstatus.Convert(err).Message()
		outcomeShouldRecord = true
		m.mu.Lock()
		m.cooldowns[sessionID] = time.Now()
		m.mu.Unlock()
		log.Info().Err(err).Msg("repair start refused; recorded as blocked")
		if streak, escalate := m.noteRefusal(sessionID, headSHA, time.Now()); escalate {
			m.blockSession(ctx, sessionID, fmt.Sprintf("automatic repair could not start %d times in a row over %s on %s; last refusal: %s",
				streak.count, time.Since(streak.first).Round(time.Minute), shortSHA(headSHA), firstLine(outcomeBlockedReason)))
		}
	}

	log.Info().
		Int32("display_status", int32(displayStatus)).
		Bool("has_failures", hasFailures).
		Msg("starting repair attempt")

	startResp, err := m.host.StartChatRun(ctx, repairStartChatRunRequest(sessionID, sessionName, replaceExistingReason, replaceExistingObservedLastChatActivityAt, resumeAgentSessionID))
	if err != nil {
		if grpcstatus.Code(err) == codes.AlreadyExists {
			staleAgentSessionID := agentSessionIDFromAlreadyExists(err)
			if staleAgentSessionID == "" {
				// Soft skip — another instance owns this run, do not record an
				// outcome here (the winner's deferred cleanup will).
				log.Info().Err(err).Msg("another repair run is active, skipping repair")
				return "", false
			}
			reclaimResp, reclaimErr := m.host.ReclaimRepairChat(ctx, &bossanovav1.ReclaimRepairChatHostRequest{
				SessionId:      sessionID,
				AgentSessionId: staleAgentSessionID,
				Reason:         "repair StartChatRun returned AlreadyExists; daemon validated stale repair chat and reclaimed it",
			})
			if reclaimErr != nil {
				log.Info().Err(reclaimErr).
					Str("stale_agent_session_id", staleAgentSessionID).
					Msg("repair chat reclaim failed, skipping repair")
				if grpcstatus.Code(reclaimErr) == codes.FailedPrecondition {
					markStartRefused(reclaimErr)
				}
				return "", false
			}
			log.Info().
				Str("stale_agent_session_id", staleAgentSessionID).
				Bool("reclaimed", reclaimResp.GetReclaimed()).
				Str("tmux_session_name", reclaimResp.GetTmuxSessionName()).
				Msg("reclaimed stale repair chat, retrying repair")

			startResp, err = m.host.StartChatRun(ctx, repairStartChatRunRequest(sessionID, sessionName, replaceExistingReason, replaceExistingObservedLastChatActivityAt, resumeAgentSessionID))
			if err != nil {
				if grpcstatus.Code(err) == codes.AlreadyExists {
					log.Info().Err(err).Msg("another repair run is active after reclaim, skipping repair")
					return "", false
				}
				if grpcstatus.Code(err) == codes.FailedPrecondition {
					markStartRefused(err)
					return "", false
				}
				outcomeRunnerError = err.Error()
				outcomeShouldRecord = true
				log.Error().Err(err).Msg("failed to start repair chat run after reclaim")
				return "", false
			}
		} else if grpcstatus.Code(err) == codes.FailedPrecondition {
			// Daemon-side StartChatRun refusal (eg. the chat is still working,
			// or the agent is not configured). This is a "blocked" outcome, not
			// a failed repair attempt: record the reason for operator
			// visibility but do NOT bump the failure count or escalate backoff.
			markStartRefused(err)
			return "", false
		} else {
			// Any other error (eg. codes.Internal "start agent: ...") is a real
			// runner failure. Record so the TUI surfaces the reason instead of
			// the operator having to grep daemon stderr.
			outcomeRunnerError = err.Error()
			outcomeShouldRecord = true
			log.Error().Err(err).Msg("failed to start repair chat run")
			return "", false
		}
	}
	runOwned = true
	m.clearRefusals(sessionID)
	outcomeShouldRecord = true
	agentSessionID := startResp.GetAgentSessionId()
	outcomeAgentSessionID = agentSessionID
	log.Info().Str("agent_session_id", agentSessionID).Msg("repair chat run started")

	// Set IsRepairing only after we own the run, so a losing instance does
	// not clobber the winner's flag in its deferred cleanup. repairSession owns
	// clearing the flag once its bounded loop (including post-repair assessment)
	// finishes, so we flag it as set for the whole session here.
	if _, err := m.host.SetRepairStatus(ctx, &bossanovav1.SetRepairStatusRequest{
		SessionId:    sessionID,
		IsRepairing:  true,
		DispatcherId: m.dispatcherID,
	}); err != nil {
		log.Warn().Err(err).Msg("failed to set repair status")
	} else {
		*repairFlagSet = true
	}

	waitResp, waitErr := m.host.WaitChatRun(ctx, &bossanovav1.WaitChatRunHostRequest{
		AgentSessionId:       agentSessionID,
		IdleFailAfterSeconds: int32(defaultRunIdleFailAfter.Seconds()),
	})

	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), repairCleanupTimeout)
	defer cleanupCancel()

	if waitErr != nil {
		// Treat WaitChatRun gRPC errors as runner-class so the TUI sees
		// "the agent process couldn't be observed" rather than an empty
		// status field.
		outcomeRunnerError = "wait for repair run failed: " + waitErr.Error()
		log.Error().Err(waitErr).Msg("wait for repair run failed")
		return "", false
	}
	if exitErr := waitResp.GetExitError(); exitErr != "" {
		outcomeExitError = exitErr
		log.Error().Str("error", exitErr).Msg("repair attempt failed")
		if !isInteractiveChatIncomplete(exitErr) {
			// The agent process ran and reported a real non-zero outcome. Do
			// not rerun the same head/status forever for those failures.
			attemptRan = true
		}
		return "", false
	}
	// The chat reported clean completion. Do not rerun the same head/status
	// forever after a successful repair attempt.
	attemptRan = true

	log.Info().Msg("repair attempt completed successfully")
	nextResumeAgentSessionID := repairResumeSessionID(agentSessionID, waitResp)

	// FIX_COMPLETE is only valid in FIXING_CHECKS; in other states the
	// next polling cycle handles the transition. With CreateWorkflow gone
	// we no longer have a session-state lookup helper — list and find.
	sessionsResp, err := m.host.ListSessions(cleanupCtx)
	if err != nil {
		log.Warn().Err(err).Msg("post-repair ListSessions failed; polling will handle transition")
		return nextResumeAgentSessionID, true
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
	return nextResumeAgentSessionID, true
}

func repairResumeSessionID(agentSessionID string, waitResp *bossanovav1.WaitChatRunHostResponse) string {
	// WaitChatRun discovers and persists provider-owned ids for agents such as
	// Codex before returning; fallback only applies to agents that resume by the
	// logical bossd agent_session_id or when discovery could not find a match.
	if waitResp != nil && waitResp.GetProviderSessionId() != "" {
		return waitResp.GetProviderSessionId()
	}
	return agentSessionID
}
