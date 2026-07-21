// Package main implements the opencode agent plugin's AgentRunnerService.
//
// The introspection reads (GetChatTitle, TranscriptExists, LastTurnIsUser,
// ResolveInteractiveSessionID) are backed by opencodedb.go, which reads
// opencode's SQLite session store read-only and defensively (BOS-435). The
// small remaining reads are constants for opencode: ListIgnoredDirtyFiles is an
// empty set, the hook-config RPCs report unsupported, and HasQuestionPrompt is
// a conservative false. A handful of run/launch RPCs (StartRun, StopRun, …)
// remain codes.Unimplemented pending the dispatch-wiring slice (part 4).
package main

import (
	"context"
	"errors"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/bossalib/agenterr"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/plugin/hostclient"
)

const pluginName = "opencode"
const pluginVersion = "1"

// Server implements AgentRunnerService for the opencode agent.
type Server struct {
	host   hostclient.Client
	logger zerolog.Logger
	runner *Runner
	store  *opencodeStore
}

func newServer(host hostclient.Client, logger zerolog.Logger, runnerOpts ...Option) *Server {
	runner := NewRunner(logger, runnerOpts...)
	return &Server{
		host:   host,
		logger: logger,
		runner: runner,
		// The introspection store reuses the runner's login shell so
		// `opencode db path` resolves through the same nodenv/asdf shims the
		// run path uses.
		store: &opencodeStore{logger: logger, loginShell: runner.loginShell},
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
					Key:          "model",
					Label:        "Model",
					Description:  "opencode model selection (e.g. anthropic/claude-sonnet). Empty uses the opencode default.",
					Type:         bossanovav1.UserSettingType_USER_SETTING_TYPE_STRING,
					DefaultValue: "",
				},
			},
		},
	}, nil
}

// --- Run / launch path ---

// StartRun spawns a headless `opencode run` subprocess for the session and
// returns opencode's own echoed session id (see sessionIDFromOutput). It detaches
// the process from the RPC handler's context: the gRPC framework cancels the
// per-call ctx as soon as we return, which would propagate to the runner's
// procCtx and SIGTERM the just-started opencode process within milliseconds. The
// runner owns subprocess lifecycle via its own Stop()/cancel paths. Mirrors the
// codex twin (plugins/bossd-plugin-codex/server.go).
func (s *Server) StartRun(_ context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	var resume *string
	if req.ResumeId != nil {
		resume = req.ResumeId
	}
	sid, err := s.runner.Start(context.Background(), req.WorkDir, req.Plan, resume, req.SessionId, req.LogPath, req.GetModel(), req.GetExtraEnv())
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

// ExitStatus reports run completion and surfaces the typed failure
// classification through the optional FailureClass/ResetAt fields while leaving
// exit_error untouched for existing consumers. opencode's PostExit (classifyExit)
// upgrades a generic non-zero exit into one of three typed sentinels: an auth
// failure, a usage cap (with reset), or a retryable rate limit — mapped here to
// the shared agenterr Kinds. Auth precedence matches the classifyExit contract.
func (s *Server) ExitStatus(_ context.Context, req *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error) { //nolint:unparam // interface implementation
	if s.runner.IsRunning(req.SessionId) {
		return &bossanovav1.AgentExitStatusResponse{IsComplete: false}, nil
	}
	err := s.runner.ExitError(req.SessionId)
	resp := &bossanovav1.AgentExitStatusResponse{IsComplete: true}
	if err != nil {
		resp.ExitError = err.Error()
	}
	var ul agenterr.ErrUsageLimited
	var rl agenterr.ErrRateLimited
	switch {
	case errors.Is(err, ErrAuthRequired):
		fc := agenterr.KindAuthInvalidated.String()
		resp.FailureClass = &fc
	case errors.As(err, &ul):
		fc := agenterr.KindUsageExhausted.String()
		resp.FailureClass = &fc
		if !ul.ResetAt.IsZero() {
			resp.ResetAt = timestamppb.New(ul.ResetAt)
		}
	case errors.As(err, &rl):
		fc := agenterr.KindRateLimited.String()
		resp.FailureClass = &fc
	}
	return resp, nil
}

// BuildInteractiveCommand is unsupported: the opencode runner drives the headless
// one-shot `opencode run` path only. Interactive TUI attach is out of scope for
// the agent-runner integration (bossd falls back to its non-interactive view),
// so this returns an empty-but-valid response rather than a fabricated command
// line. Resume of a headless run is by `--session <id>` on StartRun, not an
// interactive relaunch.
func (s *Server) BuildInteractiveCommand(_ context.Context, _ *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	return &bossanovav1.BuildInteractiveCommandResponse{}, nil
}

// ResolveInteractiveSessionID binds a chat to its opencode session by finding
// the newest session row whose working directory matches req.WorkDir. opencode
// has no per-process rollout-fd resolution (the codex path binds a rollout file
// the codex process holds open), so the SQLite newest-by-directory lookup is
// the resolution strategy. TranscriptPath is intentionally empty: opencode has
// no per-session transcript FILE (state lives in the shared SQLite store) and
// resume uses `--session <id>` (BOS-434), not a path. Returns Found=false when
// nothing matches or the DB is unavailable.
func (s *Server) ResolveInteractiveSessionID(_ context.Context, req *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) { //nolint:unparam // interface implementation
	id, found := s.store.ResolveInteractiveSessionID(req.WorkDir)
	return &bossanovav1.ResolveInteractiveSessionIDResponse{
		Found:     found,
		SessionId: id,
	}, nil
}

// ReadTranscript reports no readable transcript file: opencode keeps conversation
// state in its shared SQLite store, not a per-session transcript FILE the daemon
// can tail (the codex/claude twins read JSONL rollout files; opencode has none).
// TranscriptExists/LastTurnIsUser answer the daemon's liveness questions off the
// SQLite store instead. Returns an empty-but-valid response (Exists=false) rather
// than a gRPC error so the daemon's transcript reads degrade gracefully.
func (s *Server) ReadTranscript(_ context.Context, _ *bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.ReadTranscriptResponse{Exists: false}, nil
}

// --- Hook config: empty-but-valid, unsupported ---

// ConfigureFinalizeHook reports unsupported: the opencode runner uses the
// one-shot `opencode run` headless path (no in-CLI Stop-hook surface), so the
// daemon falls back to ExitStatus polling for finalize.
func (s *Server) ConfigureFinalizeHook(_ context.Context, _ *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.ConfigureFinalizeHookResponse{IsSupported: false}, nil
}

// RemoveAgentRunHook reports unsupported. opencode has no run-scoped hook config
// to remove (ConfigureFinalizeHook installs none), but the daemon calls this
// cleanup RPC unconditionally after run completion, so it must answer.
func (s *Server) RemoveAgentRunHook(_ context.Context, _ *bossanovav1.RemoveAgentRunHookRequest) (*bossanovav1.RemoveAgentRunHookResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.RemoveAgentRunHookResponse{IsSupported: false}, nil
}

// --- Introspection reads: empty-but-valid ---

// ListIgnoredDirtyFiles returns the (empty) non-nil set of worktree paths bossd
// must not treat as agent-authored. It is empty for opencode; the rationale and
// the shadow-git / .opencode/ / GIT_INDEX_FILE caveats are documented in
// dirty_files.go. The daemon type-asserts on Paths length, so this must be a
// non-nil empty slice.
func (s *Server) ListIgnoredDirtyFiles(_ context.Context, _ *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) { //nolint:unparam // interface implementation
	out := make([]string, len(ignoredDirtyFiles))
	copy(out, ignoredDirtyFiles)
	return &bossanovav1.ListIgnoredDirtyFilesResponse{Paths: out}, nil
}

// GetChatTitle returns the opencode session's title from the SQLite store.
// Supported is always true (mirroring the codex twin); Title is the empty
// string when the DB/session is unavailable, so the daemon falls back to its
// own heuristic. Explicit is false: opencode records auto-generated names and
// user renames the same way (session.title), so the stored title is not
// authoritative.
func (s *Server) GetChatTitle(_ context.Context, req *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.GetChatTitleResponse{
		Supported: true,
		Title:     s.store.GetChatTitle(req.SessionId),
		Explicit:  false,
	}, nil
}

// SuggestPRTitle reports unsupported so the daemon uses its deterministic title
// heuristic. Implemented in a later part.
func (s *Server) SuggestPRTitle(_ context.Context, _ *bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.SuggestPRTitleResponse{Supported: false}, nil
}

// HasQuestionPrompt is a constant false for opencode. The opencode runner uses
// the one-shot `opencode run --format json` headless path (BOS-434) — there is
// no interactive TUI approval/permission menu in the bossd-driven flow to
// detect — so a conservative false is correct and avoids manufacturing a
// false-positive question state from pane bytes.
func (s *Server) HasQuestionPrompt(_ context.Context, _ *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.HasQuestionPromptResponse{HasPrompt: false}, nil
}

func (s *Server) HasWorkingIndicator(_ context.Context, _ *bossanovav1.HasWorkingIndicatorRequest) (*bossanovav1.HasWorkingIndicatorResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.HasWorkingIndicatorResponse{IsWorking: false}, nil
}

// LastTurnIsUser reports whether the newest message in the opencode session is
// a user turn (message.data JSON role == "user"). Returns false when the
// transcript is empty, the DB is unavailable, or the newest turn is the
// assistant's. The daemon uses this to decide whether a question state is real
// or stale.
func (s *Server) LastTurnIsUser(_ context.Context, req *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.LastTurnIsUserResponse{IsUser: s.store.LastTurnIsUser(req.AgentSessionId)}, nil
}

// TranscriptExists reports whether opencode has a session row for
// agent_session_id in its SQLite store. Used by wake-up logic to choose between
// resuming (`opencode run --session <id>`) and a fresh start. A missing DB /
// table collapses to false.
func (s *Server) TranscriptExists(_ context.Context, req *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.TranscriptExistsResponse{Exists: s.store.TranscriptExists(req.AgentSessionId)}, nil
}

// --- Rotation / usage: not-limited / non-rotation ---

func (s *Server) DetectUsageLimit(_ context.Context, _ *bossanovav1.DetectUsageLimitRequest) (*bossanovav1.DetectUsageLimitResponse, error) {
	return &bossanovav1.DetectUsageLimitResponse{}, nil
}

func (s *Server) ProbeRateLimit(_ context.Context, _ *bossanovav1.ProbeRateLimitRequest) (*bossanovav1.ProbeRateLimitResponse, error) {
	return &bossanovav1.ProbeRateLimitResponse{}, nil
}

// RotationCapability reports no rotation support in the foundation slice.
// Account rotation for opencode is wired in part 4.
func (s *Server) RotationCapability(_ context.Context, _ *bossanovav1.RotationCapabilityRequest) (*bossanovav1.RotationCapabilityResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.RotationCapabilityResponse{
		SupportsRotation: false,
		AuthKind:         bossanovav1.AuthKind_AUTH_KIND_UNSPECIFIED,
	}, nil
}

func (s *Server) MaterializeAccount(_ context.Context, _ *bossanovav1.MaterializeAccountRequest) (*bossanovav1.MaterializeAccountResponse, error) {
	return &bossanovav1.MaterializeAccountResponse{}, nil
}
