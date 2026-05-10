package agent

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
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
	BuildInteractiveCommand(context.Context, *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error)
	ListIgnoredDirtyFiles(context.Context, *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error)
	GetChatTitle(context.Context, *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error)
	HasQuestionPrompt(context.Context, *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error)
	LastTurnIsUser(context.Context, *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error)
	TranscriptExists(context.Context, *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error)
}

var _ AgentRunner = (*PluginRunner)(nil)

// PluginRunner adapts the AgentRunnerClient + Tailer to the existing
// agent.AgentRunner interface so all in-process call sites in bossd
// (Lifecycle, fixloop, Server, taskorchestrator) keep working unchanged.
type PluginRunner struct {
	client AgentRunnerClient
	tailer *Tailer
	logDir string
	logger zerolog.Logger
}

// NewPluginRunner creates a PluginRunner that forwards Start/Stop/IsRunning/ExitError
// to client and serves Subscribe/History from tailer. logDir is the bossd-owned
// directory where per-session log files are written.
func NewPluginRunner(client AgentRunnerClient, tailer *Tailer, logDir string, logger zerolog.Logger) *PluginRunner {
	return &PluginRunner{client: client, tailer: tailer, logDir: logDir, logger: logger}
}

// Start forwards the request to the agent plugin via gRPC and then opens the
// tailer on the resolved session ID so that Subscribe / History work immediately.
func (r *PluginRunner) Start(ctx context.Context, workDir, plan string, resume *string, sessionID string) (string, error) {
	req := &bossanovav1.StartAgentRunRequest{
		WorkDir:   workDir,
		Plan:      plan,
		ResumeId:  resume,
		SessionId: sessionID,
		LogPath:   r.logPathFor(sessionID),
	}
	resp, err := r.client.StartRun(ctx, req)
	if err != nil {
		return "", fmt.Errorf("plugin StartRun: %w", err)
	}
	// Open the tailer on the resolved session ID.
	logPath := r.logPathFor(resp.SessionId)
	if err := r.tailer.Open(resp.SessionId, logPath); err != nil {
		// Plugin already started; we can't easily roll back. Log and continue.
		r.logger.Warn().Err(err).Str("session", resp.SessionId).Msg("tailer.Open failed; AttachSession output will be empty")
	}
	return resp.SessionId, nil
}

// Stop sends a stop request to the agent plugin and closes the local tailer.
func (r *PluginRunner) Stop(sessionID string) error {
	_, err := r.client.StopRun(context.Background(), &bossanovav1.StopAgentRunRequest{SessionId: sessionID})
	r.tailer.Close(sessionID)
	if err != nil {
		return fmt.Errorf("plugin StopRun: %w", err)
	}
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
		return nil
	}
	return fmt.Errorf("%s", resp.ExitError) //nolint:err113 // error text comes from the plugin over gRPC; no sentinel to wrap
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
