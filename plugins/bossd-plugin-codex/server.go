// Package main implements the codex agent plugin's AgentRunnerService.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/bossalib/agenterr"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/loginshell"
	"github.com/recurser/bossalib/plugin/hostclient"
)

const pluginName = "codex"
const pluginVersion = "1"

// Server implements AgentRunnerService for the codex agent.
type Server struct {
	host   hostclient.Client
	logger zerolog.Logger
	runner *Runner
	// inspector resolves a chat's rollout from the open file descriptors of
	// the codex process running under its tmux pane. Defaults to the real
	// process inspector; unit tests inject a fake.
	inspector processInspector
}

func newServer(host hostclient.Client, logger zerolog.Logger, runnerOpts ...Option) *Server {
	return &Server{
		host:      host,
		logger:    logger,
		runner:    NewRunner(logger, runnerOpts...),
		inspector: defaultProcessInspector,
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

func (s *Server) ExitStatus(_ context.Context, req *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error) { //nolint:unparam // interface implementation
	if s.runner.IsRunning(req.SessionId) {
		return &bossanovav1.AgentExitStatusResponse{IsComplete: false}, nil
	}
	err := s.runner.ExitError(req.SessionId)
	resp := &bossanovav1.AgentExitStatusResponse{IsComplete: true}
	if err != nil {
		resp.ExitError = err.Error()
	}
	// Surface the typed classification through the two optional fields while
	// leaving exit_error (above) untouched so existing consumers keep working.
	var ul agenterr.ErrUsageLimited
	switch {
	case errors.As(err, &ul):
		fc := agenterr.KindUsageExhausted.String()
		resp.FailureClass = &fc
		if !ul.ResetAt.IsZero() {
			resp.ResetAt = timestamppb.New(ul.ResetAt)
		}
	case errors.Is(err, ErrAuthRequired):
		fc := agenterr.KindAuthInvalidated.String()
		resp.FailureClass = &fc
	}
	return resp, nil
}

// ConfigureFinalizeHook reports unsupported. Unlike claude, codex has no
// in-CLI Stop-hook surface (no settings.local.json equivalent). Returning
// IsSupported=false signals to the daemon that finalize-via-hook is not
// available; the daemon's WaitAgentRun path falls back to ExitStatus
// polling for codex sessions. (TODOS: revisit when codex grows hooks.)
func (s *Server) ConfigureFinalizeHook(_ context.Context, _ *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.ConfigureFinalizeHookResponse{IsSupported: false}, nil
}

// RemoveAgentRunHook reports unsupported. Codex has no run-scoped hook config
// to remove, but the daemon calls this cleanup RPC unconditionally after run
// completion.
func (s *Server) RemoveAgentRunHook(_ context.Context, _ *bossanovav1.RemoveAgentRunHookRequest) (*bossanovav1.RemoveAgentRunHookResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.RemoveAgentRunHookResponse{IsSupported: false}, nil
}

// BuildInteractiveCommand returns the argv that boss/bossd should run inside
// a tmux pane to attach a user-interactive codex session. Output capture is
// owned by bossd via tmux pipe-pane; wrapping codex in a tee pipeline makes
// stdout non-TTY and codex exits before the ready marker appears.
//
// Resume vs fresh: codex resume is a positional subcommand, so a resume
// invocation is `codex resume <UUID>`, not `codex --resume <UUID>`.
// (Lane 0 spike finding.)
//
// req.AppendSystemPrompt is intentionally NOT consumed: codex has no
// append-system-prompt flag (the claude plugin maps the field to
// `--append-system-prompt`, but codex's only instruction-override knob,
// `-c model_instructions_file`, REPLACES the AGENTS.md pipeline rather than
// appending, so using it would clobber the worktree's AGENTS.md — and the
// concat-into-a-temp-file workaround would leave exactly the local artifact
// the surrounding feature exists to avoid). The boss session-context suffix
// is therefore a deliberate no-op for codex today; wire it through here once
// codex grows a real append surface (tracked upstream:
// https://github.com/openai/codex/issues/11588). Keeping the daemon
// agent-agnostic (it always offers the suffix) means this gate lives in the
// plugin that owns codex's CLI shape, not in the host.
//
// req.McpConfigPath IS consumed, but differently from claude: codex has no
// --mcp-config flag, so codexMcpOverrideArgs translates the JSON config bossd
// wrote (in app-data, never the worktree) into repeatable `-c mcp_servers.boss.*`
// global overrides. This exposes the same mcp__boss__* tools without writing
// anything into the worktree. It is best-effort — a missing/unparseable config
// is logged and skipped so the launch never fails.
//
// No `tui.notifications` / `tui.notification_method` overrides are appended
// here, deliberately. BOS-487 proposed launching codex with
// `-c 'tui.notifications=["approval-requested"]' -c tui.notification_method="osc9"`
// so a pipe-pane raw-log tailer could turn codex's OSC 9 desktop-notification
// escape into a structured question signal (the BOS-485 seam). Measured against
// codex-cli 0.145.0 that does not work — and the failure is NOT the one the plan
// expected:
//
//   - OSC 9 IS reachable in a bossd tmux pane (the feared blocker is refuted):
//     codex emits it and `tmux pipe-pane` preserves it verbatim in the raw log,
//     even though `capture-pane -p` is blind to it.
//   - But `approval-requested` is not a codex notification kind at all — the
//     string does not exist in the 0.145.0 binary. Since `tui.notifications` is
//     an allow-list, naming it emits nothing AND filters out the one kind that
//     does fire.
//   - The only kind that fires is `agent-turn-complete`, whose payload is the
//     last assistant message. That is the semantic OPPOSITE of "a question is
//     pending", so feeding it to the question-signal store would flag every
//     completed turn as CHAT_STATUS_QUESTION.
//
// Codex's menu-grammar detector (hasCodexQuestionPrompt in question.go) stays
// the source of truth; it already reaches questionState via the per-agent
// HasQuestionPrompt RPC. Full measurements, the A/B table, and a repro live in
// docs/solutions/logic-errors/spike-codex-osc9-notification-is-turn-complete-only.md
// Revisit only if codex upstream grows an approval-time notification kind
// (openai/codex#11808, #19921 track the same gap in the external notify hook).
func (s *Server) BuildInteractiveCommand(_ context.Context, req *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	args := []string{"codex"}
	// Codex has no --mcp-config flag (the claude plugin maps the field to that).
	// It configures MCP servers via repeatable global `-c mcp_servers.<name>.*`
	// overrides, which keep the wiring on the command line rather than in the
	// worktree (HARD CONSTRAINT preserved). These are global options, so they go
	// before the `resume` subcommand. Best-effort: a missing/unparseable config
	// is logged and skipped, never blocking the launch.
	args = append(args, s.codexMcpOverrideArgs(req.GetMcpConfigPath())...)
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
		if model := resolveCodexModel(req.GetModel(), s.runner.model); model != "" {
			args = append(args, "--model", model)
		}
	}
	initialInput := codexInitialInput(req)
	if initialInput != "" {
		args = append(args, initialInput)
	}
	loginShell := ""
	if s.runner != nil {
		loginShell = s.runner.loginShell
	}
	if err := trustCodexWorktree(req.GetWorktreePath()); err != nil {
		return nil, status.Errorf(codes.Internal, "trust codex worktree: %v", err)
	}
	wrapped := loginshell.Wrap(loginShell, loginshell.Flags(loginShell), args)
	return &bossanovav1.BuildInteractiveCommandResponse{
		Argv:                 wrapped,
		ReadyMarker:          "›",
		CommandPrefix:        "$",
		ConsumesInitialInput: initialInput != "",
	}, nil
}

func codexInitialInput(req *bossanovav1.BuildInteractiveCommandRequest) string {
	if req.GetInitialCommand() != "" {
		return "$" + strings.TrimLeft(req.GetInitialCommand(), "/$")
	}
	return req.GetInitialPrompt()
}

// mcpConfigFile is the subset of the JSON config bossd writes (see
// services/bossd/internal/session/mcp_config.go) that codex needs to rebuild as
// `-c` overrides. Kept local to the plugin: module boundaries forbid importing
// the daemon's session package.
type mcpConfigFile struct {
	MCPServers map[string]struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	} `json:"mcpServers"`
}

// codexMcpOverrideArgs translates the boss MCP config at path into codex global
// `-c mcp_servers.boss.*` overrides (codex has no --mcp-config). It returns nil
// when path is empty or the config cannot be read/parsed/lacks a usable "boss"
// server — MCP wiring is best-effort and must never block a codex launch. The
// override values are TOML literals, matching how codex parses `-c key=value`.
func (s *Server) codexMcpOverrideArgs(path string) []string {
	if path == "" {
		return nil
	}
	cleaned := filepath.Clean(path)
	raw, err := os.ReadFile(cleaned)
	if err != nil {
		s.logger.Warn().Err(err).Str("mcpConfigPath", path).
			Msg("read mcp config failed; codex launches without mcp__boss__*")
		return nil
	}
	var doc mcpConfigFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		s.logger.Warn().Err(err).Str("mcpConfigPath", path).
			Msg("parse mcp config failed; codex launches without mcp__boss__*")
		return nil
	}
	boss, ok := doc.MCPServers["boss"]
	if !ok || boss.Command == "" {
		s.logger.Warn().Str("mcpConfigPath", path).
			Msg(`mcp config has no usable "boss" server; codex launches without mcp__boss__*`)
		return nil
	}
	out := []string{"-c", "mcp_servers.boss.command=" + tomlString(boss.Command)}
	if len(boss.Args) > 0 {
		quoted := make([]string, len(boss.Args))
		for i, a := range boss.Args {
			quoted[i] = tomlString(a)
		}
		out = append(out, "-c", "mcp_servers.boss.args=["+strings.Join(quoted, ",")+"]")
	}
	return out
}

// tomlString renders s as a TOML basic string (double-quoted, with backslashes,
// quotes, and control characters escaped) for use as a `-c key=value` override
// value. Real binary/socket paths never contain control characters, but
// escaping them keeps the emitted override valid TOML regardless of input.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func (s *Server) ResolveInteractiveSessionID(_ context.Context, req *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) { //nolint:unparam // interface implementation
	var id, path, reason string
	var ambiguous bool
	// Process-fd resolution is the authoritative path: bind this chat to the
	// rollout its own codex process (under the given tmux pane) holds open. It
	// is deterministic and race-free, so siblings in one worktree never collide.
	// Only attempted for the live spawn path (not legacy backfill, which has no
	// pane pid); a miss falls through to the time-window scan below unchanged.
	//
	// NOTE: this relies on codex keeping the rollout fd open for the process
	// lifetime (verified in the BOS-290 spike). If a future codex stops doing
	// so, resolution silently regresses to the time-window fallback — no worse
	// than before this change.
	if !req.GetAllowLegacyBackfill() && req.GetPanePid() > 0 {
		fdID, fdPath, ok, treeVisible := resolveInteractiveSessionIDByPID(s.inspector, req.WorkDir, int(req.GetPanePid()))
		if ok {
			return &bossanovav1.ResolveInteractiveSessionIDResponse{
				Found:          true,
				SessionId:      fdID,
				TranscriptPath: fdPath,
			}, nil
		}
		// fd missed but the pane's process tree is visible: this chat's codex
		// simply hasn't opened its rollout yet. Report not-found so the daemon
		// keeps polling for THIS chat's OWN fd rather than accepting the racy
		// time-window scan below, which — with its -2s window slack — could bind
		// a sibling chat's already-written rollout in the same worktree
		// (BOS-290). Only when the tree is NOT visible (fd inspection genuinely
		// unavailable on this host) do we fall through to the time-window scan.
		//
		// Trade-off: on a host where `ps` works but open-file reads never do
		// (e.g. Linux hidepid where /proc/<pid>/fd is denied), treeVisible stays
		// true and this path never time-window-falls-back during the live
		// session. That is acceptable because bossd spawns codex as the same
		// user (same-user /proc/<pid>/fd is readable by default), and the
		// wake-time legacy backfill (pane_pid 0) still uses the time-window scan,
		// so an id is bound on first wake — the pre-fix result, merely deferred.
		if treeVisible {
			return &bossanovav1.ResolveInteractiveSessionIDResponse{Found: false}, nil
		}
	}
	if req.GetAllowLegacyBackfill() {
		chatCreatedAt := time.Time{}
		if req.GetChatCreatedAt() != nil {
			chatCreatedAt = req.GetChatCreatedAt().AsTime()
		}
		id, path, ambiguous, reason = resolveLegacyInteractiveSessionID(req.WorkDir, chatCreatedAt)
	} else {
		launchedAfter := time.Time{}
		if req.GetLaunchedAfter() != nil {
			launchedAfter = req.GetLaunchedAfter().AsTime()
		}
		id, path, ambiguous, reason = resolveInteractiveSessionID(req.WorkDir, launchedAfter)
	}
	return &bossanovav1.ResolveInteractiveSessionIDResponse{
		Found:          id != "",
		SessionId:      id,
		TranscriptPath: path,
		Ambiguous:      ambiguous,
		Reason:         reason,
	}, nil
}

func (s *Server) ListIgnoredDirtyFiles(_ context.Context, _ *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) { //nolint:unparam // interface implementation
	out := make([]string, len(ignoredDirtyFiles))
	copy(out, ignoredDirtyFiles)
	return &bossanovav1.ListIgnoredDirtyFilesResponse{Paths: out}, nil
}

func (s *Server) GetChatTitle(_ context.Context, req *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) { //nolint:unparam // interface implementation
	title, explicit := chatTitle(req.WorkDir, req.SessionId)
	return &bossanovav1.GetChatTitleResponse{
		Supported: true,
		Title:     title,
		Explicit:  explicit,
	}, nil
}

// SuggestPRTitle is not yet implemented for codex. Returning supported=false
// makes the daemon fall back to its deterministic title heuristic (which
// preserves a meaningful existing PR title), so codex cron runs are unaffected
// and no longer get their title clobbered by the last commit subject. A future
// change can implement this via `codex exec` (a one-shot read-only run).
func (s *Server) SuggestPRTitle(_ context.Context, _ *bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.SuggestPRTitleResponse{Supported: false}, nil
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

// HasWorkingIndicator always returns false for codex. Codex's TUI spinner
// animates, so its pane content keeps changing while it works and the daemon's
// content-diff path already reports WORKING — there is no static-but-busy pane
// to rescue. A codex-specific detector (via its codexWorking regex) is a
// possible follow-up but is intentionally out of scope here.
func (s *Server) HasWorkingIndicator(_ context.Context, _ *bossanovav1.HasWorkingIndicatorRequest) (*bossanovav1.HasWorkingIndicatorResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.HasWorkingIndicatorResponse{IsWorking: false}, nil
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

// ReadTranscript reads the codex rollout JSONL transcript for (work_dir,
// agent_session_id) and returns ordered chat messages. Returns Exists=false
// (nil error) when no rollout file is found, so callers can distinguish
// "never started" from hard errors.
func (s *Server) ReadTranscript(_ context.Context, req *bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error) {
	root, err := codexSessionsRoot()
	if err != nil {
		return &bossanovav1.ReadTranscriptResponse{Exists: false}, nil
	}
	return readTranscriptAt(root, req.WorkDir, req.AgentSessionId, req.MaxMessages)
}

// RotationCapability: codex injects its credential as a per-account home dir.
func (s *Server) RotationCapability(_ context.Context, _ *bossanovav1.RotationCapabilityRequest) (*bossanovav1.RotationCapabilityResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.RotationCapabilityResponse{
		SupportsRotation: true,
		AuthKind:         bossanovav1.AuthKind_AUTH_KIND_HOME_DIR,
	}, nil
}

// MaterializeAccount turns the stored codex credential blob into an auth.json
// file spec (mode 0600) plus HomeDirEnvKey=CODEX_HOME.
//
// IMPORTANT (BOS-158): the returned home-dir spec assumes a BASE-SEEDED codex
// home. The per-account dir must have its sessions/ (and session_index.jsonl)
// shared/symlinked from a shared base home; only auth.json is written fresh
// here. A per-account home whose sessions/ lacks the rollout makes codex resume
// fail fast (thread/resume: no rollout found ... code -32600). Seeding sessions/
// is BOS-162's credmaterialize executor's responsibility, NOT this RPC's — this
// method returns ONLY auth.json + HomeDirEnvKey. See
// docs/solutions/account-rotation/spike-cross-account-resume-credential-isolation.md.
// The blob is NEVER logged.
func (s *Server) MaterializeAccount(_ context.Context, req *bossanovav1.MaterializeAccountRequest) (*bossanovav1.MaterializeAccountResponse, error) { //nolint:unparam // interface implementation; error result always nil today
	return &bossanovav1.MaterializeAccountResponse{
		Files: []*bossanovav1.MaterializedFile{{
			RelativePath: "auth.json",
			Content:      req.GetCredentialBlob(),
			Mode:         0o600,
		}},
		HomeDirEnvKey: "CODEX_HOME",
	}, nil
}
