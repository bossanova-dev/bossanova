// Package main implements the stub agent-runner's AgentRunnerService.
//
// The stub is designed exclusively for E2E tests: it satisfies the full
// AgentRunnerService contract without launching any real agent subprocess.
// StartRun returns immediately with the resolved session ID and marks the
// run as complete so bossd's WaitAgentRun path (ExitStatus polling) sees a
// clean exit. No tmux, no external process, no network.
package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

const pluginName = "stub"
const pluginVersion = "e2e"

// sessionState tracks the lifecycle of a single stub run.
type sessionState struct {
	complete bool
	exitErr  string // empty on clean exit
}

// Server implements AgentRunnerService for the stub agent.
// It is goroutine-safe: all session state is protected by mu.
type Server struct {
	logger   zerolog.Logger
	mu       sync.Mutex
	sessions map[string]*sessionState
}

func newServer(logger zerolog.Logger) *Server {
	return &Server{
		logger:   logger,
		sessions: make(map[string]*sessionState),
	}
}

// GetInfo returns the fixed stub agent descriptor.
func (s *Server) GetInfo(_ context.Context, _ *bossanovav1.AgentRunnerServiceGetInfoRequest) (*bossanovav1.AgentRunnerServiceGetInfoResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.AgentRunnerServiceGetInfoResponse{
		Info: &bossanovav1.PluginInfo{
			Name:         pluginName,
			Version:      pluginVersion,
			Capabilities: []string{"agent_runner"},
		},
	}, nil
}

// StartRun records the session as immediately complete (clean exit).
// No subprocess is spawned. If req.SessionId is empty, a deterministic
// placeholder is generated. The resolved session ID is returned.
func (s *Server) StartRun(_ context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) { //nolint:unparam // interface implementation
	sid := req.SessionId
	if sid == "" {
		sid = fmt.Sprintf("stub-%d", stubSeq())
	}
	s.mu.Lock()
	s.sessions[sid] = &sessionState{complete: true}
	s.mu.Unlock()
	s.logger.Info().Str("session_id", sid).Msg("stub: StartRun — session recorded as complete")
	return &bossanovav1.StartAgentRunResponse{SessionId: sid}, nil
}

// StopRun is a no-op; the stub run is already complete.
func (s *Server) StopRun(_ context.Context, req *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error) { //nolint:unparam // interface implementation
	s.logger.Info().Str("session_id", req.SessionId).Msg("stub: StopRun (no-op)")
	return &bossanovav1.StopAgentRunResponse{}, nil
}

// IsRunning always returns false; stub runs complete synchronously in StartRun.
func (s *Server) IsRunning(_ context.Context, req *bossanovav1.IsAgentRunningRequest) (*bossanovav1.IsAgentRunningResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.IsAgentRunningResponse{Running: false}, nil
}

// ExitStatus reports the run as complete with no error. Returns
// is_complete=false only for sessions that were never started.
func (s *Server) ExitStatus(_ context.Context, req *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error) { //nolint:unparam // interface implementation
	s.mu.Lock()
	state, ok := s.sessions[req.SessionId]
	s.mu.Unlock()
	if !ok {
		// Unknown session: treat as already complete with no error (permissive
		// so that bossd's polling loop doesn't get stuck on an unknown ID).
		return &bossanovav1.AgentExitStatusResponse{IsComplete: true}, nil
	}
	return &bossanovav1.AgentExitStatusResponse{
		IsComplete: state.complete,
		ExitError:  state.exitErr,
	}, nil
}

// ConfigureFinalizeHook reports unsupported; the stub has no settings file.
// The daemon falls back to ExitStatus polling, which works for the stub.
func (s *Server) ConfigureFinalizeHook(_ context.Context, _ *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.ConfigureFinalizeHookResponse{IsSupported: false}, nil
}

// RemoveAgentRunHook reports unsupported. The stub never writes run-scoped
// hook config, but bossd still calls cleanup after completion.
func (s *Server) RemoveAgentRunHook(_ context.Context, _ *bossanovav1.RemoveAgentRunHookRequest) (*bossanovav1.RemoveAgentRunHookResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.RemoveAgentRunHookResponse{IsSupported: false}, nil
}

// BuildInteractiveCommand returns a no-op argv. The stub does not support
// interactive (tmux) sessions, but returns a valid response so the daemon
// does not crash if it calls this path.
func (s *Server) BuildInteractiveCommand(_ context.Context, req *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) { //nolint:unparam // interface implementation
	// Return a benign argv (true exits 0 and produces no output).
	return &bossanovav1.BuildInteractiveCommandResponse{
		Argv: []string{"true"},
		// The stub builds no real command line, so any system-prompt suffix
		// bossd offers is dropped. Declare that instead of staying silent.
		AppendSystemPromptSupport: bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_NONE,
	}, nil
}

// ResolveInteractiveSessionID returns not-found; the stub has no transcript store.
func (s *Server) ResolveInteractiveSessionID(_ context.Context, req *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) { //nolint:unparam // interface implementation
	if req.RequestedSessionId != "" {
		return &bossanovav1.ResolveInteractiveSessionIDResponse{
			Found:     true,
			SessionId: req.RequestedSessionId,
			Reason:    "stub: echo requested_session_id",
		}, nil
	}
	return &bossanovav1.ResolveInteractiveSessionIDResponse{
		Found:  false,
		Reason: "stub: no interactive session support",
	}, nil
}

// ListIgnoredDirtyFiles returns an empty list; the stub writes no agent files.
func (s *Server) ListIgnoredDirtyFiles(_ context.Context, _ *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) {
	return &bossanovav1.ListIgnoredDirtyFilesResponse{}, nil
}

// GetChatTitle reports not-supported; the stub has no transcript.
func (s *Server) GetChatTitle(_ context.Context, _ *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.GetChatTitleResponse{Supported: false}, nil
}

// SuggestPRTitle reports not-supported; the stub has no agent to ask. The daemon
// falls back to its deterministic title heuristic.
func (s *Server) SuggestPRTitle(_ context.Context, _ *bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.SuggestPRTitleResponse{Supported: false}, nil
}

// HasQuestionPrompt always returns false; the stub never shows a prompt.
func (s *Server) HasQuestionPrompt(_ context.Context, _ *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.HasQuestionPromptResponse{HasPrompt: false}, nil
}

// DetectUsageLimit always returns limited=false; the stub never renders a
// usage-cap banner. Present to keep the AgentRunnerService contract complete.
func (s *Server) DetectUsageLimit(_ context.Context, _ *bossanovav1.DetectUsageLimitRequest) (*bossanovav1.DetectUsageLimitResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.DetectUsageLimitResponse{Limited: false}, nil
}

// ProbeRateLimit always reports unsupported; the stub queries no provider.
func (s *Server) ProbeRateLimit(_ context.Context, _ *bossanovav1.ProbeRateLimitRequest) (*bossanovav1.ProbeRateLimitResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.ProbeRateLimitResponse{
		Status: &bossanovav1.RateLimitStatus{
			Limited: false,
			Status:  bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_UNSUPPORTED,
		},
	}, nil
}

// HasWorkingIndicator always returns false; the stub never renders a working marker.
func (s *Server) HasWorkingIndicator(_ context.Context, _ *bossanovav1.HasWorkingIndicatorRequest) (*bossanovav1.HasWorkingIndicatorResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.HasWorkingIndicatorResponse{IsWorking: false}, nil
}

// LastTurnIsUser always returns false; the stub has no transcript.
func (s *Server) LastTurnIsUser(_ context.Context, _ *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.LastTurnIsUserResponse{IsUser: false}, nil
}

// TranscriptExists always returns false; the stub writes no transcript files.
func (s *Server) TranscriptExists(_ context.Context, _ *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.TranscriptExistsResponse{Exists: false}, nil
}

// stubSeq is a monotonic counter for generating placeholder session IDs.
var (
	seqMu  sync.Mutex
	seqVal int
)

func stubSeq() int {
	seqMu.Lock()
	defer seqMu.Unlock()
	seqVal++
	return seqVal
}
