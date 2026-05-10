// Package main implements the codex agent plugin's AgentRunnerService.
package main

import (
	"context"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/recurser/bossalib/agentruntime"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/plugin/hostclient"
)

const pluginName = "codex"
const pluginVersion = "1"

// Server implements AgentRunnerService for the codex agent.
type Server struct {
	host   hostclient.Client
	logger zerolog.Logger
	runner *Runner
}

func newServer(host hostclient.Client, logger zerolog.Logger, runnerOpts ...Option) *Server {
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
					Key:         "sandbox",
					Label:       "Sandbox mode",
					Description: "Codex --sandbox mode. Empty uses codex default (no --sandbox flag passed).",
					Type:        bossanovav1.UserSettingType_USER_SETTING_TYPE_ENUM,
					// First entry is "" — the cycle picker treats it as the
					// "use codex default" state so users can reset back to
					// it. The remaining entries are the modes accepted by
					// `codex --sandbox <mode>` per the Lane 0 spike.
					AllowedValues: []string{"", "read-only", "workspace-write", "danger-full-access"},
					DefaultValue:  "",
				},
				{
					Key:         "approval",
					Label:       "Approval policy",
					Description: "Codex --ask-for-approval policy. Empty uses codex default (no flag passed).",
					Type:        bossanovav1.UserSettingType_USER_SETTING_TYPE_ENUM,
					// First entry is "" (use codex default). Remaining
					// entries match the policies accepted by
					// `codex --ask-for-approval <policy>`.
					AllowedValues: []string{"", "untrusted", "on-failure", "on-request", "never"},
					DefaultValue:  "",
				},
				{
					Key:          "model",
					Label:        "Model",
					Description:  "Codex --model selection. Empty uses codex default.",
					Type:         bossanovav1.UserSettingType_USER_SETTING_TYPE_STRING,
					DefaultValue: "",
				},
				{
					Key:          "dangerously_bypass_approvals_and_sandbox",
					Label:        "Bypass approvals & sandbox (dangerous)",
					Description:  "Pass --dangerously-bypass-approvals-and-sandbox to codex. Overrides sandbox/approval. Use only in trusted worktrees.",
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
	// codex process within milliseconds. The runner owns subprocess
	// lifecycle via its own Stop()/cancel paths. (Mirrors the claude plugin
	// fix in services/bossd's host_service.)
	sid, err := s.runner.Start(context.Background(), req.WorkDir, req.Plan, resume, req.SessionId, req.LogPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "start run: %v", err)
	}
	return &bossanovav1.StartAgentRunResponse{SessionId: sid}, nil
}

func (s *Server) StopRun(_ context.Context, req *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error) {
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

// ConfigureFinalizeHook reports unsupported. Unlike claude, codex has no
// in-CLI Stop-hook surface (no settings.local.json equivalent). Returning
// IsSupported=false signals to the daemon that finalize-via-hook is not
// available; the daemon's WaitAgentRun path falls back to ExitStatus
// polling for codex sessions. (TODOS: revisit when codex grows hooks.)
func (s *Server) ConfigureFinalizeHook(_ context.Context, _ *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.ConfigureFinalizeHookResponse{IsSupported: false}, nil
}

// BuildInteractiveCommand returns the argv that boss/bossd should run inside
// a tmux pane to attach a user-interactive codex session. Mirrors claude's
// LogTeeArgv wiring so output is teed into the per-session log file.
//
// Resume vs fresh: codex resume is a positional subcommand, so a resume
// invocation is `codex resume <UUID>`, not `codex --resume <UUID>`.
// (Lane 0 spike finding.)
func (s *Server) BuildInteractiveCommand(_ context.Context, req *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) { //nolint:unparam // interface implementation
	args := []string{"codex"}
	if req.Resume {
		args = append(args, "resume", req.SessionId)
	}
	if s.runner != nil {
		if s.runner.dangerouslyBypass {
			// Mutually exclusive with --sandbox / --ask-for-approval
			// (codex errors out when combined); drop them here to mirror
			// runner.buildArgv.
			args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		} else {
			if s.runner.sandbox != "" {
				args = append(args, "--sandbox", s.runner.sandbox)
			}
			if s.runner.approval != "" {
				args = append(args, "--ask-for-approval", s.runner.approval)
			}
		}
		if s.runner.model != "" {
			args = append(args, "--model", s.runner.model)
		}
	}
	return &bossanovav1.BuildInteractiveCommandResponse{
		Argv: agentruntime.LogTeeArgv(args, req.LogPath),
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

// HasQuestionPrompt reports whether the supplied tmux pane bytes look like
// a codex TUI question prompt (e.g. an approval/permission menu). Unlike
// claude (which delegates to bossalib/statusdetect), codex's TUI grammar
// differs enough that we run a codex-specific detector. Implementation
// lives in question.go: hasCodexQuestionPrompt strips user-prompt-history
// and activity-bullet lines, refuses to fire while the working spinner is
// visible, and matches against the codex approval-menu grammar captured in
// the Lane 0 spike.
func (s *Server) HasQuestionPrompt(_ context.Context, req *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.HasQuestionPromptResponse{
		HasPrompt: hasCodexQuestionPrompt(req.PaneContent),
	}, nil
}

// LastTurnIsUser reports whether the last meaningful entry in the codex
// rollout JSONL transcript for agentSessionID is a real user turn (not a
// function_call_output or token_count bookkeeping event). Returns
// is_user=false when the transcript is missing, unreadable, or ends with an
// agent turn. Used by the daemon to decide whether a question state is real
// or stale.
func (s *Server) LastTurnIsUser(_ context.Context, req *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error) { //nolint:unparam // interface implementation
	path, err := transcriptPath(req.WorkDir, req.AgentSessionId)
	if err != nil {
		return &bossanovav1.LastTurnIsUserResponse{IsUser: false}, nil
	}
	return &bossanovav1.LastTurnIsUserResponse{IsUser: lastTurnIsUser(path)}, nil
}

// TranscriptExists reports whether a codex rollout JSONL transcript exists
// on disk for (work_dir, agent_session_id). Used by wake-up logic to choose
// between `codex exec resume <UUID>` (transcript present) and a fresh start
// (transcript missing). Errors collapse to false.
func (s *Server) TranscriptExists(_ context.Context, req *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.TranscriptExistsResponse{
		Exists: transcriptExists(req.WorkDir, req.AgentSessionId),
	}, nil
}
