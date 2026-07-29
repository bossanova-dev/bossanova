// Package main implements the opencode agent plugin's AgentRunnerService.
//
// The introspection reads (GetChatTitle, TranscriptExists, LastTurnIsUser,
// ResolveInteractiveSessionID) are backed by opencodedb.go, which reads
// opencode's SQLite session store read-only and defensively (BOS-435). The
// small remaining reads are constants for opencode: ListIgnoredDirtyFiles is
// the single BOS-486 question-hook path, ConfigureFinalizeHook reports
// unsupported, and HasQuestionPrompt is a conservative false. The run/launch
// RPCs (StartRun, StopRun, IsRunning, ExitStatus) are implemented here on top of
// runner.go; the remaining chat-control RPCs report unsupported because opencode
// has no interactive pane to drive.
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
	"github.com/recurser/bossalib/safego"
)

const pluginName = "opencode"
const pluginVersion = "1"

// Server implements AgentRunnerService for the opencode agent.
type Server struct {
	host   hostclient.Client
	logger zerolog.Logger
	runner *Runner
	store  *opencodeStore

	// onHookCleanup, when non-nil, is invoked with the run's session id at the
	// end of the question-hook cleanup goroutine. It is a test seam: it lets a
	// test await its OWN run's cleanup deterministically instead of polling.
	//
	// Set once at construction and never written again, so it needs no mutex —
	// and because each invocation carries its own sessionID it stays correct
	// for any number of concurrent runs, unlike a shared completion slot. nil
	// in production, where nothing observes cleanup at all.
	onHookCleanup func(sessionID string)
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
//
// Before the subprocess starts, StartRun injects the question-signal event hook
// (BOS-486) into the worktree when the request's ExtraEnv carries the loopback
// hook context. Injection is best-effort: a write failure is logged and the run
// proceeds without a question signal, because a missing "? question" label is
// strictly better than a session that will not start. The hook is removed by
// the run-end goroutine armed below — which, on the headless path, is the only
// inverse that actually fires; see RemoveAgentRunHook for why that RPC is not
// the safety net it appears to be.
func (s *Server) StartRun(_ context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	var resume *string
	if req.ResumeId != nil {
		resume = req.ResumeId
	}
	injected, err := installQuestionHook(req.WorkDir, req.GetExtraEnv())
	if err != nil {
		s.logger.Warn().Err(err).Msg("opencode: question hook injection failed; running without a question signal")
		// A failed install can still leave bytes behind: WriteFile truncates
		// before it writes, so a mid-write failure leaves a partial file that
		// opencode would load as a syntactically broken plugin. `injected` is
		// false, so neither the failed-start sweep nor the run-end goroutine
		// below would ever remove it. Sweep it here; removal is idempotent.
		s.removeQuestionHookBestEffort(req.WorkDir)
	}
	sid, err := s.runner.Start(context.Background(), req.WorkDir, req.Plan, resume, req.SessionId, req.LogPath, req.GetModel(), req.GetExtraEnv())
	if err != nil {
		if injected {
			// The run never started, so nothing will ever call the run-end
			// cleanup — sweep the just-written asset here instead of leaking it.
			s.removeQuestionHookBestEffort(req.WorkDir)
		}
		return nil, status.Errorf(codes.Internal, "start run: %v", err)
	}
	if injected {
		s.armQuestionHookCleanup(sid, req.WorkDir)
	}
	return &bossanovav1.StartAgentRunResponse{SessionId: sid}, nil
}

// armQuestionHookCleanup removes the injected hook once the run's subprocess
// exits. It runs in a panic-recovering goroutine, because the RPC must return
// promptly so the gRPC handler's ctx cancellation cannot reach the subprocess.
//
// Cleanup is best-effort and production observes it not at all: the git-exclude
// entry covers an asset stranded by a crash before Wait returns. The optional
// onHookCleanup callback exists solely so a test can wait for its own run.
//
// Removal keys on workDir alone, which assumes at most ONE armed run per
// worktree — true today because only StartSession's fresh-start headless branch
// arms injection. Two overlapping runs in one worktree would let the first to
// finish delete the second's freshly-written asset.
func (s *Server) armQuestionHookCleanup(sessionID, workDir string) {
	safego.Go(s.logger, func() {
		// The runner's Wait returns the run's exit error, which ExitStatus
		// reports separately — here only the "it is over" edge matters.
		_ = s.runner.Wait(context.Background(), sessionID)
		s.removeQuestionHookBestEffort(workDir)
		if s.onHookCleanup != nil {
			s.onHookCleanup(sessionID)
		}
	})
}

// removeQuestionHookBestEffort deletes the injected hook, logging (never
// returning) a failure. Cleanup must not be able to fail a run or an RPC.
func (s *Server) removeQuestionHookBestEffort(workDir string) {
	if err := removeQuestionHook(workDir); err != nil {
		s.logger.Warn().Err(err).Msg("opencode: question hook cleanup failed")
	}
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
//
// This MUST keep returning IsSupported: false. bossd routes finalize off this
// flag (agentClientHookSupport → services/bossd/internal/agent/poll_fallback.go):
// claiming support switches the daemon from ExitStatus polling to waiting for a
// Stop-hook POST that opencode never sends, so finalize would hang forever. The
// BOS-486 question hook is injected on the StartRun path instead (see
// installQuestionHook) precisely so it does not have to travel through this RPC.
// TestConfigureFinalizeHookStaysUnsupported pins the invariant.
func (s *Server) ConfigureFinalizeHook(_ context.Context, _ *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.ConfigureFinalizeHookResponse{IsSupported: false}, nil
}

// RemoveAgentRunHook removes the BOS-486 question-signal plugin that StartRun
// injected into the worktree.
//
// REACHABILITY (do not assume this is the safety net it looks like): bossd's
// only caller, removeRunHookBestEffort, is called exclusively from
// HostServiceServer.WaitChatRun — the tmux StartChatRun path — and resolves the
// session through runSessionByID, which only StartChatRun populates. A headless
// opencode run, the only shape that injects today, never registers there and so
// never receives this RPC. The run-end goroutine armed in StartRun is the SOLE
// production inverse; this is a correct, idempotent second one that opencode is
// not currently wired to receive.
//
// Unlike ConfigureFinalizeHook, this response's IsSupported is not a routing
// flag; it reports true because there is now genuinely a hook to remove.
func (s *Server) RemoveAgentRunHook(_ context.Context, req *bossanovav1.RemoveAgentRunHookRequest) (*bossanovav1.RemoveAgentRunHookResponse, error) { //nolint:unparam // interface implementation
	// Never surface a cleanup failure as an RPC error: this runs on the run's
	// teardown path, where an error would be noise the daemon cannot act on.
	s.removeQuestionHookBestEffort(req.GetWorkDir())
	return &bossanovav1.RemoveAgentRunHookResponse{IsSupported: true}, nil
}

// --- Introspection reads: empty-but-valid ---

// ListIgnoredDirtyFiles returns the non-nil set of worktree paths bossd must
// not treat as agent-authored. For opencode that is exactly one entry — the
// BOS-486 question hook StartRun injects — and the rationale plus the
// shadow-git / .opencode/ / GIT_INDEX_FILE caveats are documented in
// dirty_files.go. The daemon type-asserts on Paths length, so this must always
// be non-nil, even when the list is empty.
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
//
// BOS-486 feeds BOS-485's structured question-signal store from an injected
// opencode `event`-hook plugin instead (see installQuestionHook). That signal
// reaches bossd over the loopback receiver, NOT through this RPC, so this stays
// false: the pane-regex fallback it backs has nothing to scrape for a headless
// agent and a true here could only manufacture a false positive.
//
// How often the injected hook actually fires is bounded by the spike against
// opencode 1.18.4: `question.asked` is not an opencode event type at all (it is
// TUI-internal, and absent from the documented event list), and
// `permission.asked` cannot fire while buildArgv appends an auto-approval
// permission flag unconditionally on every run (--auto, or
// --dangerously-skip-permissions; see TestBuildArgvAlwaysExactlyOnePermissionFlag).
// Only `session.idle` — which merely CLEARS a pending signal — fires headless
// today; the question path is wired for the day one of those changes.
//
// The READ side is gated too, and this is the part that surprises people: the
// question store's only consumer is TmuxStatusPoller, which sources chats from
// ListWithTmuxSession — a headless chat row has no tmux session name, so even a
// correctly authenticated POST can NOT surface `? question` for a headless
// opencode chat today. BOS-486 ships the WRITE half of the pipeline; do not
// read the wiring below as a working end-to-end signal.
//
// This file's reachability facts all trace to one place; read it before
// changing any of them:
// docs/solutions/logic-errors/spike-opencode-question-signal-events-unreachable.md
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
