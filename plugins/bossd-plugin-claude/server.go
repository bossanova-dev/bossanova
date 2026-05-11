// Package main implements the Claude agent plugin's AgentRunnerService.
package main

import (
	"context"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/recurser/bossalib/agentruntime"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/plugin/hostclient"
	"github.com/recurser/bossalib/statusdetect"
)

const pluginName = "claude"
const pluginVersion = "1"

// Server implements AgentRunnerService.
type Server struct {
	host   hostclient.Client
	logger zerolog.Logger
	runner *Runner
}

func newServer(host hostclient.Client, logger zerolog.Logger, runnerOpts ...RunnerOption) *Server {
	return &Server{
		host:   host,
		logger: logger,
		runner: NewRunner(logger, runnerOpts...),
	}
}

func (s *Server) GetInfo(_ context.Context, _ *bossanovav1.AgentRunnerServiceGetInfoRequest) (*bossanovav1.AgentRunnerServiceGetInfoResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.AgentRunnerServiceGetInfoResponse{
		Info: &bossanovav1.PluginInfo{
			Name:         pluginName,
			Version:      pluginVersion,
			Capabilities: []string{"agent_runner"},
			UserSettings: []*bossanovav1.UserSetting{
				{
					Key:          "dangerously_skip_permissions",
					Label:        "Skip permission prompts",
					Description:  "Pass --dangerously-skip-permissions to claude. Use only in trusted worktrees.",
					Type:         bossanovav1.UserSettingType_USER_SETTING_TYPE_BOOL,
					DefaultValue: "false",
				},
			},
		},
	}, nil
}

func (s *Server) StartRun(_ context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	var resume *string
	if req.ResumeId != nil {
		resume = req.ResumeId
	}
	// Detach the spawned subprocess from this RPC handler's context. The
	// gRPC framework cancels the per-call ctx as soon as we return, which
	// would propagate to runner.Start's procCtx and SIGTERM the just-started
	// claude process within milliseconds. The runner owns subprocess
	// lifecycle via its own Stop()/cancel paths.
	sid, err := s.runner.Start(context.Background(), req.WorkDir, req.Plan, resume, req.SessionId, req.LogPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "start run: %v", err)
	}
	return &bossanovav1.StartAgentRunResponse{SessionId: sid}, nil
}

func (s *Server) StopRun(_ context.Context, req *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error) { //nolint:unparam // interface implementation
	if err := s.runner.Stop(req.SessionId); err != nil {
		return nil, status.Errorf(codes.NotFound, "stop run: %v", err)
	}
	return &bossanovav1.StopAgentRunResponse{}, nil
}

func (s *Server) IsRunning(_ context.Context, req *bossanovav1.IsAgentRunningRequest) (*bossanovav1.IsAgentRunningResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.IsAgentRunningResponse{Running: s.runner.IsRunning(req.SessionId)}, nil
}

func (s *Server) ExitStatus(_ context.Context, req *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error) { //nolint:unparam // interface implementation
	if s.runner.IsRunning(req.SessionId) {
		return &bossanovav1.AgentExitStatusResponse{IsComplete: false}, nil
	}
	err := s.runner.ExitError(req.SessionId)
	var msg string
	if err != nil {
		msg = err.Error()
	}
	return &bossanovav1.AgentExitStatusResponse{IsComplete: true, ExitError: msg}, nil
}

func (s *Server) ConfigureFinalizeHook(_ context.Context, req *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error) {
	if err := WriteHookConfig(req.WorkDir, req.SessionId, req.AgentSessionId, req.HookToken, int(req.HookPort)); err != nil {
		return nil, status.Errorf(codes.Internal, "write hook config: %v", err)
	}
	return &bossanovav1.ConfigureFinalizeHookResponse{IsSupported: true}, nil
}

func (s *Server) BuildInteractiveCommand(_ context.Context, req *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) { //nolint:unparam // interface implementation
	args := []string{"claude"}
	if req.Resume {
		args = append(args, "--resume", req.SessionId)
	} else {
		args = append(args, "--session-id", req.SessionId)
	}
	if s.runner.dangerouslySkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	return &bossanovav1.BuildInteractiveCommandResponse{
		Argv: agentruntime.LogTeeArgv(args, req.LogPath),
	}, nil
}

func (s *Server) ResolveInteractiveSessionID(_ context.Context, req *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) { //nolint:unparam // interface implementation
	if req.GetRequestedSessionId() == "" {
		return &bossanovav1.ResolveInteractiveSessionIDResponse{Found: false, Reason: "requested_session_id empty"}, nil
	}
	return &bossanovav1.ResolveInteractiveSessionIDResponse{
		Found:     true,
		SessionId: req.GetRequestedSessionId(),
	}, nil
}

func (s *Server) ListIgnoredDirtyFiles(_ context.Context, _ *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) { //nolint:unparam // interface implementation
	out := make([]string, len(ignoredDirtyFiles))
	copy(out, ignoredDirtyFiles)
	return &bossanovav1.ListIgnoredDirtyFilesResponse{Paths: out}, nil
}

func (s *Server) GetChatTitle(_ context.Context, req *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) { //nolint:unparam // interface implementation
	title := chatTitle(req.WorkDir, req.SessionId)
	return &bossanovav1.GetChatTitleResponse{
		Supported: true,
		Title:     title,
	}, nil
}

// HasQuestionPrompt reports whether the supplied pane bytes look like a Claude
// Code question prompt (AskUserQuestion / permission UI / conversational ?).
// Delegates to bossalib/statusdetect, which is shared between the daemon's
// tmux poller and the client-side PTY monitor.
func (s *Server) HasQuestionPrompt(_ context.Context, req *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.HasQuestionPromptResponse{
		HasPrompt: statusdetect.HasQuestionPrompt(req.PaneContent),
	}, nil
}

// LastTurnIsUser reports whether the last meaningful entry in the Claude Code
// JSONL transcript for (work_dir, agent_session_id) is a real user text turn
// (skipping tool_result-only "user" entries). Returns is_user=false when the
// transcript is missing, unreadable, or ends with an assistant turn. Used by
// the daemon to decide whether a question state is real or stale.
func (s *Server) LastTurnIsUser(_ context.Context, req *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error) { //nolint:unparam // interface implementation
	path, err := transcriptPath(req.WorkDir, req.AgentSessionId)
	if err != nil {
		return &bossanovav1.LastTurnIsUserResponse{IsUser: false}, nil
	}
	return &bossanovav1.LastTurnIsUserResponse{IsUser: lastTurnIsUser(path)}, nil
}

// TranscriptExists reports whether a Claude Code JSONL transcript exists on
// disk for (work_dir, agent_session_id). Used by wake-up logic to choose
// between `claude --resume` (transcript present) and `claude --session-id`
// (transcript missing — fresh fallback). Errors collapse to false: "can't
// tell" is treated as "transcript missing" so we never silently lie about a
// resume.
func (s *Server) TranscriptExists(_ context.Context, req *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.TranscriptExistsResponse{
		Exists: transcriptExists(req.WorkDir, req.AgentSessionId),
	}, nil
}
