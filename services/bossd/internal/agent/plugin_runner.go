package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/agenterr"
	"github.com/recurser/bossalib/agenttelemetry"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossd/internal/db"
)

// AgentRunnerClient is the bossd-side wrapper around the AgentRunnerService
// gRPC client. Defined as an interface so plugin_runner_test.go can fake
// it out without spinning up a real plugin subprocess.
type AgentRunnerClient interface {
	GetInfo(context.Context) (*bossanovav1.PluginInfo, error)
	StartRun(context.Context, *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error)
	StopRun(context.Context, *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error)
	IsRunning(context.Context, *bossanovav1.IsAgentRunningRequest) (*bossanovav1.IsAgentRunningResponse, error)
	ExitStatus(context.Context, *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error)
	ConfigureFinalizeHook(context.Context, *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error)
	RemoveAgentRunHook(context.Context, *bossanovav1.RemoveAgentRunHookRequest) (*bossanovav1.RemoveAgentRunHookResponse, error)
	BuildInteractiveCommand(context.Context, *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error)
	ResolveInteractiveSessionID(context.Context, *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error)
	ListIgnoredDirtyFiles(context.Context, *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error)
	GetChatTitle(context.Context, *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error)
	SuggestPRTitle(context.Context, *bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error)
	HasQuestionPrompt(context.Context, *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error)
	DetectUsageLimit(context.Context, *bossanovav1.DetectUsageLimitRequest) (*bossanovav1.DetectUsageLimitResponse, error)
	ProbeRateLimit(context.Context, *bossanovav1.ProbeRateLimitRequest) (*bossanovav1.ProbeRateLimitResponse, error)
	HasWorkingIndicator(context.Context, *bossanovav1.HasWorkingIndicatorRequest) (*bossanovav1.HasWorkingIndicatorResponse, error)
	LastTurnIsUser(context.Context, *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error)
	TranscriptExists(context.Context, *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error)
	ReadTranscript(context.Context, *bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error)
	RotationCapability(context.Context, *bossanovav1.RotationCapabilityRequest) (*bossanovav1.RotationCapabilityResponse, error)
	MaterializeAccount(context.Context, *bossanovav1.MaterializeAccountRequest) (*bossanovav1.MaterializeAccountResponse, error)
}

type headlessCapabilityProfilePreflightClient interface {
	PreflightHeadlessRun(context.Context, *bossanovav1.PreflightHeadlessRunRequest) (*bossanovav1.PreflightHeadlessRunResponse, error)
}

var (
	_ AgentRunner = (*PluginRunner)(nil)
	// PluginRunner is the runner every production agent goes through, and
	// Dispatcher.StopByAgent's bounded path exists only for runners that
	// implement ContextualStopper. Asserting it here makes deleting or renaming
	// StopWithContext a build failure rather than a silent demotion to the
	// unbounded Stop — the wedge BOS-717 removed.
	_ ContextualStopper = (*PluginRunner)(nil)
)

var (
	agentRunStartRecordTimeout   = 10 * time.Second
	completedRunStopTimeout      = 10 * time.Second
	completedRunTelemetryTimeout = 10 * time.Second
)

// PluginRunner adapts the AgentRunnerClient + Tailer to the existing
// agent.AgentRunner interface so all in-process call sites in bossd
// (Lifecycle, fixloop, Server, taskorchestrator) keep working unchanged.
type PluginRunner struct {
	client    AgentRunnerClient
	tailer    *Tailer
	logDir    string
	logger    zerolog.Logger
	runs      db.AgentRunStore
	agentName string
	logMu     sync.Mutex
	logPaths  map[string]string
	runIDs    map[string]string
	workDirs  map[string]string
	starts    map[string]time.Time
}

// NewPluginRunner creates a PluginRunner that forwards Start/Stop/IsRunning/ExitError
// to client and serves Subscribe/History from tailer. logDir is the bossd-owned
// directory where per-session log files are written.
func NewPluginRunner(client AgentRunnerClient, tailer *Tailer, logDir string, logger zerolog.Logger) *PluginRunner {
	return &PluginRunner{client: client, tailer: tailer, logDir: logDir, logger: logger, logPaths: map[string]string{}, runIDs: map[string]string{}, workDirs: map[string]string{}, starts: map[string]time.Time{}}
}

func (r *PluginRunner) SetAgentName(name string) {
	r.agentName = name
}

// SetAgentRunStore enables daemon-owned run-cost telemetry recording. Nil keeps
// the historical runner behavior for tests and minimal wiring.
func (r *PluginRunner) SetAgentRunStore(store db.AgentRunStore) {
	r.runs = store
}

// Start forwards the request to the agent plugin via gRPC and then opens the
// tailer on the resolved session ID so that Subscribe / History work immediately.
func (r *PluginRunner) Start(ctx context.Context, workDir, plan string, resume *string, sessionID, model, effort string, extraEnv map[string]string) (string, error) {
	return r.startWithBossSession(ctx, workDir, plan, resume, sessionID, model, effort, extraEnv, bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED, sessionID, "")
}

// StartWithHeadlessCapabilityProfile carries an explicit runtime-operation
// requirement to the plugin. It is called only by opted-in headless launches;
// Start above preserves the historical wire request exactly.
func (r *PluginRunner) StartWithHeadlessCapabilityProfile(ctx context.Context, workDir, plan string, resume *string, sessionID, model, effort string, extraEnv map[string]string, profile bossanovav1.HeadlessCapabilityProfile) (string, error) {
	return r.startWithBossSession(ctx, workDir, plan, resume, sessionID, model, effort, extraEnv, profile, sessionID, "")
}

// StartWithHeadlessLaunchOptions forwards every panel-less launch control to
// the plugin in one request so a capability profile cannot be dropped.
func (r *PluginRunner) StartWithHeadlessLaunchOptions(ctx context.Context, workDir, plan string, resume *string, sessionID, model, effort string, extraEnv map[string]string, options HeadlessLaunchOptions) (string, error) {
	return r.startWithBossSession(ctx, workDir, plan, resume, sessionID, model, effort, extraEnv, options.HeadlessCapabilityProfile, options.BossSessionID, options.EffectiveModel)
}

// PreflightHeadlessCapabilityProfile asks the plugin to validate a required
// operation surface using the same managed account/model/work-dir inputs the
// gated run will launch with. It performs no run or tailer side effects.
func (r *PluginRunner) PreflightHeadlessCapabilityProfile(ctx context.Context, workDir, model, effort string, extraEnv map[string]string, profile bossanovav1.HeadlessCapabilityProfile) error {
	client, ok := r.client.(headlessCapabilityProfilePreflightClient)
	if !ok {
		return ErrHeadlessCapabilityProfileUnsupported
	}
	_, err := client.PreflightHeadlessRun(ctx, &bossanovav1.PreflightHeadlessRunRequest{
		Model:                     model,
		Effort:                    effort,
		ExtraEnv:                  extraEnv,
		HeadlessCapabilityProfile: profile,
		WorkDir:                   workDir,
	})
	if err != nil {
		return fmt.Errorf("plugin PreflightHeadlessRun: %w", err)
	}
	return nil
}

func (r *PluginRunner) startWithBossSession(ctx context.Context, workDir, plan string, resume *string, sessionID, model, effort string, extraEnv map[string]string, profile bossanovav1.HeadlessCapabilityProfile, bossSessionID, effectiveModel string) (string, error) {
	logKey := sessionID
	if logKey == "" {
		logKey = bossSessionID
		if logKey == "" {
			var err error
			logKey, err = sqlutil.NewID()
			if err != nil {
				return "", fmt.Errorf("mint log key: %w", err)
			}
		}
	}
	logPath := r.logPathFor(logKey)
	startedAt := time.Now()
	req := &bossanovav1.StartAgentRunRequest{
		WorkDir:                   workDir,
		Plan:                      plan,
		ResumeId:                  resume,
		SessionId:                 sessionID,
		LogPath:                   logPath,
		Model:                     model,
		Effort:                    effort,
		ExtraEnv:                  extraEnv,
		HeadlessCapabilityProfile: profile,
	}
	resp, err := r.client.StartRun(ctx, req)
	if err != nil {
		return "", fmt.Errorf("plugin StartRun: %w", err)
	}
	if r.runs != nil {
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentRunStartRecordTimeout)
		run, err := r.runs.Start(recordCtx, db.AgentRun{
			SessionID:      bossSessionID,
			AgentSessionID: resp.SessionId,
			AgentName:      r.agentName,
			Model:          firstNonEmpty(effectiveModel, model),
			Effort:         effort,
			StartedAt:      startedAt,
		})
		cancel()
		if err != nil {
			r.logger.Warn().Err(err).Str("session", sessionID).Str("agent_session", resp.SessionId).Msg("record agent run start failed")
		} else {
			r.rememberRunID(resp.SessionId, run.ID)
		}
	}
	// Index the tail under the resolved session ID, but open the path the plugin
	// was asked to write.
	r.rememberLogPath(resp.SessionId, logPath)
	r.rememberWorkDir(resp.SessionId, workDir)
	r.rememberRunStartedAt(resp.SessionId, startedAt)
	if err := r.tailer.Open(resp.SessionId, logPath); err != nil {
		// Plugin already started; we can't easily roll back. Log and continue.
		r.logger.Warn().Err(err).Str("session", resp.SessionId).Msg("tailer.Open failed; AttachSession output will be empty")
	}
	return resp.SessionId, nil
}

// Stop sends a stop request to the agent plugin and closes the local tailer.
//
// Unbounded by design-inertia rather than intent: it has no context to bound the
// RPC with. Callers that have one — and anything waiting on the result needs one
// — should use StopWithContext instead.
func (r *PluginRunner) Stop(sessionID string) error {
	return r.StopWithContext(context.Background(), sessionID)
}

// StopWithContext is Stop with the plugin RPC bounded by ctx, satisfying
// ContextualStopper.
//
// The tailer is closed even when the RPC fails, exactly as Stop has always done:
// a plugin that did not answer is precisely the case where holding the local
// tail open leaks a file handle for a run nobody is coming back for.
func (r *PluginRunner) StopWithContext(ctx context.Context, sessionID string) error {
	_, err := r.client.StopRun(ctx, &bossanovav1.StopAgentRunRequest{SessionId: sessionID})
	r.tailer.Close(sessionID)
	if err != nil {
		telemetryCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		r.recordTelemetryFromLog(telemetryCtx, sessionID)
		return fmt.Errorf("plugin StopRun: %w", err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	r.recordStop(stopCtx, sessionID, db.AgentRunStopStopped)
	stopCancel()
	telemetryCtx, telemetryCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer telemetryCancel()
	r.recordTelemetryFromLog(telemetryCtx, sessionID)
	r.clearRunMemory(sessionID)
	return nil
}

// IsRunning reports whether the agent plugin has an active run for sessionID.
func (r *PluginRunner) IsRunning(sessionID string) bool {
	resp, err := r.client.IsRunning(context.Background(), &bossanovav1.IsAgentRunningRequest{SessionId: sessionID})
	if err != nil {
		return false
	}
	return resp.Running
}

// ExitError returns the exit error for a completed session, or nil if the run
// is still active or completed cleanly.
func (r *PluginRunner) ExitError(sessionID string) error {
	resp, err := r.client.ExitStatus(context.Background(), &bossanovav1.AgentExitStatusRequest{SessionId: sessionID})
	if err != nil {
		return fmt.Errorf("plugin ExitStatus: %w", err)
	}
	if !resp.IsComplete {
		return nil
	}
	if resp.ExitError == "" {
		r.recordCompletedRun(sessionID, db.AgentRunStopClean)
		r.clearRunMemory(sessionID)
		return nil
	}
	r.recordCompletedRun(sessionID, stopReasonForExit(resp.ExitError))
	r.clearRunMemory(sessionID)
	return fmt.Errorf("%s", resp.ExitError) //nolint:err113 // error text comes from the plugin over gRPC; no sentinel to wrap
}

func (r *PluginRunner) recordCompletedRun(sessionID, reason string) {
	stopCtx, stopCancel := context.WithTimeout(context.Background(), completedRunStopTimeout)
	r.recordStop(stopCtx, sessionID, reason)
	stopCancel()
	telemetryCtx, telemetryCancel := context.WithTimeout(context.Background(), completedRunTelemetryTimeout)
	defer telemetryCancel()
	r.recordTelemetryFromLog(telemetryCtx, sessionID)
}

func (r *PluginRunner) recordStop(ctx context.Context, agentSessionID, reason string) {
	if r.runs == nil {
		return
	}
	if err := r.runs.Stop(ctx, agentSessionID, reason, time.Now()); err != nil && !errors.Is(err, sql.ErrNoRows) {
		r.logger.Warn().Err(err).Str("agent_session", agentSessionID).Msg("record agent run stop failed")
	}
}

func (r *PluginRunner) recordTelemetryFromLog(ctx context.Context, agentSessionID string) {
	if r.runs == nil {
		return
	}
	path := r.rememberedLogPath(agentSessionID)
	counts, err := tallyAgentLog(ctx, path, agentSessionID, r.rememberedWorkDir(agentSessionID), r.rememberedRunStartedAt(agentSessionID))
	if err != nil {
		r.logger.Warn().Err(err).Str("agent_session", agentSessionID).Msg("tally agent log telemetry failed")
		return
	}
	telemetry := db.AgentRunTelemetry{
		ParentModelCallCount:  counts.ParentModelCallCount,
		ChildModelCallCount:   counts.ChildModelCallCount,
		ToolCallCount:         counts.ToolCallCount,
		SubagentCount:         counts.SubagentCount,
		DirectSubagentCount:   counts.DirectSubagentCount,
		OutputTokenCount:      counts.OutputTokenCount,
		ReasoningTokenCount:   counts.ReasoningTokenCount,
		ReviewerDispatchCount: counts.ReviewerDispatchCount,
		TerminalState:         counts.TerminalState,
	}
	for _, child := range counts.Children {
		telemetry.Children = append(telemetry.Children, db.AgentRunChild{
			AgentSessionID:      child.AgentSessionID,
			ParentAgentID:       child.ParentAgentID,
			SpawnDepth:          child.SpawnDepth,
			StartedAt:           child.StartedAt,
			StoppedAt:           child.StoppedAt,
			ModelCallCount:      child.ModelCallCount,
			ToolCallCount:       child.ToolCallCount,
			OutputTokenCount:    child.OutputTokenCount,
			ReasoningTokenCount: child.ReasoningTokenCount,
		})
	}
	if runID := r.rememberedRunID(agentSessionID); runID != "" {
		if err := r.runs.RecordTelemetry(ctx, runID, telemetry); err != nil && !errors.Is(err, sql.ErrNoRows) {
			r.logger.Warn().Err(err).Str("agent_session", agentSessionID).Msg("record agent log telemetry failed")
		}
		return
	}
	if err := r.runs.RecordTelemetryByAgentSessionID(ctx, agentSessionID, telemetry); err != nil && !errors.Is(err, sql.ErrNoRows) {
		r.logger.Warn().Err(err).Str("agent_session", agentSessionID).Msg("record agent log telemetry failed")
	}
}

func tallyAgentLog(ctx context.Context, path, agentSessionID, workDir string, since time.Time) (agenttelemetry.Counts, error) {
	if err := ctx.Err(); err != nil {
		return agenttelemetry.Counts{}, err
	}
	if workDir != "" {
		transcript, err := agenttelemetry.ClaudeTranscriptPath(workDir, agentSessionID)
		if err == nil {
			if _, statErr := os.Stat(transcript); statErr == nil {
				counts, err := agenttelemetry.TallyClaudePathWithChildrenSinceContext(ctx, transcript, filepath.Dir(transcript), agentSessionID, since)
				if err != nil {
					return agenttelemetry.Counts{}, err
				}
				if err := ctx.Err(); err != nil {
					return agenttelemetry.Counts{}, err
				}
				return counts, nil
			}
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), agentSessionID, "subagents")); err == nil {
		counts, err := agenttelemetry.TallyClaudePathForSessionContext(ctx, path, agentSessionID)
		if err != nil {
			return agenttelemetry.Counts{}, err
		}
		if err := ctx.Err(); err != nil {
			return agenttelemetry.Counts{}, err
		}
		return counts, nil
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)), "subagents")); err == nil {
		counts, err := agenttelemetry.TallyClaudePathWithChildrenSinceContext(ctx, path, filepath.Dir(path), strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)), time.Time{})
		if err != nil {
			return agenttelemetry.Counts{}, err
		}
		if err := ctx.Err(); err != nil {
			return agenttelemetry.Counts{}, err
		}
		return counts, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return agenttelemetry.Counts{}, err
	}
	defer func() { _ = f.Close() }()
	counts, err := agenttelemetry.TallyCodexSinceContext(ctx, f, since)
	if err != nil {
		return agenttelemetry.Counts{}, err
	}
	if err := ctx.Err(); err != nil {
		return agenttelemetry.Counts{}, err
	}
	return counts, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func stopReasonForExit(exitError string) string {
	switch agenterr.Classify(exitError, time.Now()).Kind {
	case agenterr.KindUsageExhausted:
		return db.AgentRunStopUsageExhausted
	case agenterr.KindRateLimited:
		return db.AgentRunStopRateLimited
	default:
		return db.AgentRunStopUnknown
	}
}

// Subscribe returns a channel of OutputLines served from the local Tailer.
func (r *PluginRunner) Subscribe(ctx context.Context, sessionID string) (<-chan OutputLine, error) {
	return r.tailer.Subscribe(ctx, sessionID)
}

// History returns the buffered OutputLines for sessionID from the local Tailer.
func (r *PluginRunner) History(sessionID string) []OutputLine {
	return r.tailer.History(sessionID)
}

// AgentClient exposes the underlying client for callers that need RPCs
// outside the AgentRunner interface (e.g. ConfigureFinalizeHook,
// BuildInteractiveCommand, ListIgnoredDirtyFiles, GetChatTitle).
func (r *PluginRunner) AgentClient() AgentRunnerClient { return r.client }

// logPathFor returns the bossd-owned log path for a session.
// Files live in r.logDir/<sessionID>.log.
func (r *PluginRunner) logPathFor(sessionID string) string {
	return filepath.Join(r.logDir, sessionID+".log")
}

func (r *PluginRunner) rememberLogPath(agentSessionID, path string) {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	r.logPaths[agentSessionID] = path
}

func (r *PluginRunner) rememberedLogPath(agentSessionID string) string {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	if path := r.logPaths[agentSessionID]; path != "" {
		return path
	}
	return r.logPathFor(agentSessionID)
}

func (r *PluginRunner) rememberRunID(agentSessionID, runID string) {
	if agentSessionID == "" || runID == "" {
		return
	}
	r.logMu.Lock()
	defer r.logMu.Unlock()
	r.runIDs[agentSessionID] = runID
}

func (r *PluginRunner) rememberedRunID(agentSessionID string) string {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	return r.runIDs[agentSessionID]
}

func (r *PluginRunner) clearRunMemory(agentSessionID string) {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	delete(r.logPaths, agentSessionID)
	delete(r.runIDs, agentSessionID)
	delete(r.workDirs, agentSessionID)
	delete(r.starts, agentSessionID)
}

func (r *PluginRunner) rememberWorkDir(agentSessionID, workDir string) {
	if agentSessionID == "" || workDir == "" {
		return
	}
	r.logMu.Lock()
	defer r.logMu.Unlock()
	r.workDirs[agentSessionID] = workDir
}

func (r *PluginRunner) rememberRunStartedAt(agentSessionID string, startedAt time.Time) {
	if agentSessionID == "" || startedAt.IsZero() {
		return
	}
	r.logMu.Lock()
	defer r.logMu.Unlock()
	r.starts[agentSessionID] = startedAt
}

func (r *PluginRunner) rememberedWorkDir(agentSessionID string) string {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	return r.workDirs[agentSessionID]
}

func (r *PluginRunner) rememberedRunStartedAt(agentSessionID string) time.Time {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	return r.starts[agentSessionID]
}
