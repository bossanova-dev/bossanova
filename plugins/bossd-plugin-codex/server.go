// Package main implements the codex agent plugin's AgentRunnerService.
package main

import (
	"context"
	"errors"
	"io/fs"
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
	// operationRegistry reads the operation inventory Codex exposes at runtime
	// for an explicitly profiled headless launch. Tests replace it with a fixed
	// surface; production uses the app-server registry.
	operationRegistry runtimeOperationRegistry
	// inspector resolves a chat's rollout from the open file descriptors of
	// the codex process running under its tmux pane. Defaults to the real
	// process inspector; unit tests inject a fake.
	inspector processInspector
}

func newServer(host hostclient.Client, logger zerolog.Logger, runnerOpts ...Option) *Server {
	runner := NewRunner(logger, runnerOpts...)
	return &Server{
		host:              host,
		logger:            logger,
		runner:            runner,
		inspector:         defaultProcessInspector,
		operationRegistry: codexAppServerOperationRegistry{binary: "codex", loginShell: runner.loginShell},
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
					Key:           "effort",
					Label:         "Reasoning effort",
					Description:   "Codex model_reasoning_effort override. Defaults to medium.",
					Type:          bossanovav1.UserSettingType_USER_SETTING_TYPE_ENUM,
					AllowedValues: []string{"", "low", "medium", "high", "xhigh"},
					DefaultValue:  "medium",
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

// runtimeTarget describes the Codex runtime a capability-profile check should
// inspect. Both PreflightHeadlessRun and the real StartRun build their target
// here, so for the same request inputs the early check profiles the same
// runtime the real run launches. Keeping this in one place means a field added
// to codexRuntimeTarget cannot be populated on only one of the two paths;
// TestPreflightAndStartRunInspectIdenticalRuntimeTarget is the ratchet that
// enforces it.
//
// The four inputs the two RPCs now reconcile:
//
//   - Model — resolved by the same per-request-wins/env-fallback rule as the
//     launch argv (resolveCodexModel), because the operation registry passes it
//     to `codex app-server` as `-c model="…"` and a different model can expose a
//     different operation surface.
//   - Home — the selected account's CODEX_HOME, read out of ExtraEnv rather
//     than projected, so the profiled home is the launched home.
//   - ExtraEnv — the daemon builds both the preflight env and the run env from
//     the same dotenv.OverlayWithRepo expression over the created worktree
//     (services/bossd/internal/session/lifecycle.go, the preflight env and the
//     headless run env; tmux_chat.go passes the same map the tmux child gets).
//     Since BOS-1749 the preflight runs after worktree creation and after the
//     setup script, with rollback on rejection, so the two are no longer
//     divergent by construction.
//   - WorkDir — the directory the gated run executes in. Codex resolves a
//     repo-level `.codex/config.toml` relative to its working directory, so a
//     preflight launched without it profiles a runtime that never saw the MCP
//     servers the repo declares for itself (BOS-865).
func (s *Server) runtimeTarget(reqModel, reqEffort, workDir string, extraEnv map[string]string) codexRuntimeTarget {
	home, _ := codexConfigDirForEnv(extraEnv)
	return codexRuntimeTarget{
		Home:     home,
		Model:    resolveCodexModel(reqModel, s.runner.model),
		Effort:   resolveCodexEffort(reqEffort, s.runner.effort),
		WorkDir:  workDir,
		ExtraEnv: extraEnv,
	}
}

func (s *Server) PreflightHeadlessRun(ctx context.Context, req *bossanovav1.PreflightHeadlessRunRequest) (*bossanovav1.PreflightHeadlessRunResponse, error) {
	return s.preflightHeadlessCapabilityProfile(
		ctx,
		req.GetHeadlessCapabilityProfile(),
		s.runtimeTarget(req.GetModel(), req.GetEffort(), req.GetWorkDir(), req.GetExtraEnv()),
	)
}

func (s *Server) StartRun(ctx context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	var resume *string
	if req.ResumeId != nil {
		resume = req.ResumeId
	}
	// CODEX_HOME arrives in ExtraEnv already resolved to the selected account's
	// home — that is credential/account routing, not MCP wiring, and this plugin
	// no longer projects a synthetic home on top of it. The runtime the preflight
	// profiles is therefore the runtime that starts, with no substitution between
	// the two.
	extraEnv := req.GetExtraEnv()
	if _, err := s.preflightHeadlessCapabilityProfile(
		ctx,
		req.GetHeadlessCapabilityProfile(),
		s.runtimeTarget(req.GetModel(), req.GetEffort(), req.GetWorkDir(), extraEnv),
	); err != nil {
		return nil, err
	}
	// Detach the spawned subprocess from this RPC handler's context. The
	// gRPC framework cancels the per-call ctx as soon as we return, which
	// would propagate to runner.Start's procCtx and SIGTERM the just-started
	// codex process within milliseconds. The runner owns subprocess
	// lifecycle via its own Stop()/cancel paths. (Mirrors the claude plugin
	// fix in services/bossd's host_service.)
	sid, err := s.runner.Start(context.Background(), req.WorkDir, req.Plan, resume, req.SessionId, req.LogPath, req.GetModel(), req.GetEffort(), extraEnv)
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
// That no-op is DECLARED rather than silent: the response sets
// append_system_prompt_support = NONE, so bossd can report the instruction
// classes it built that never reached this argv instead of assuming they
// landed. Flip the declaration to IN_ARGV in the same change that starts
// appending the flag, never before.
//
// No MCP wiring is appended here, deliberately. Boss does not own MCP
// configuration: codex reads `$CODEX_HOME/config.toml` (the selected account's
// home, routed through the session environment) plus the repo's own
// `.codex/config.toml`, and the repo is responsible for declaring the servers
// it needs. This plugin neither writes nor overrides either file — a `-c
// mcp_servers.*` override or a synthetic CODEX_HOME would each put boss back in
// the business of owning a harness's config format.
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
	// No `-c mcp_servers.*` overrides and no synthetic CODEX_HOME: codex resolves
	// MCP servers from `$CODEX_HOME/config.toml` and the repo's `.codex/config.toml`,
	// which is the repo's responsibility to declare. CODEX_HOME still reaches the
	// child through the session environment for account selection.
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
		if model := resolveCodexModel(req.GetModel(), s.runner.model); model != "" {
			args = append(args, "--model", model)
		}
		if effort := resolveCodexEffort(req.GetEffort(), s.runner.effort); effort != "" {
			args = append(args, "-c", "model_reasoning_effort="+effort)
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
		// codex has no append-system-prompt flag, so the suffix never reaches
		// argv above. Declaring NONE lets bossd say so out loud.
		AppendSystemPromptSupport: bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_NONE,
	}, nil
}

func codexInitialInput(req *bossanovav1.BuildInteractiveCommandRequest) string {
	if req.GetInitialCommand() != "" {
		return "$" + strings.TrimLeft(req.GetInitialCommand(), "/$")
	}
	return req.GetInitialPrompt()
}

// Reasons a resolution miss reports back to the daemon. They must all stay
// non-empty — an unexplained miss is what made the BOS-1144 unbound chats
// invisible in the logs — and they are pinned by tests, so a rename is a visible
// signal in review rather than a silent contract change.
//
// The ladder that chooses between them is ordered by REMEDY, not by cheapness
// (BOS-1298). "The fd is not open yet" is routinely true on a cold worktree and
// is the only rung whose remedy is "wait", so it goes LAST: put it first and it
// shadows every real fault behind it, which is exactly what happened for eleven
// hours. Ahead of it sit the two that never resolve on their own —
// reasonPaneProbeFailed (go and look at the probe host) and reasonNoRolloutFound
// (wrong CODEX_HOME, or codex wrote its rollout somewhere else).
const (
	reasonPaneProbeFailed         = "codex process open-file probe failed; rollout fd unreadable"
	reasonNoRolloutFound          = "no matching codex-tui rollout found"
	reasonPaneRolloutFDNotOpenYet = "codex process tree visible but no rollout fd open yet"
)

// anyRolloutUnderSessionsRoot reports whether the codex sessions root holds at
// least one rollout file. It separates two misses whose remedies differ
// completely: rollouts present but none held open by this pane's tree is a
// genuine "codex has not got there yet" (wait), while a root with no rollout at
// all means the root itself is wrong — a CODEX_HOME mismatch, or a codex that
// wrote elsewhere — which waiting never fixes.
func anyRolloutUnderSessionsRoot() bool {
	root, err := codexSessionsRoot()
	if err != nil {
		return false
	}
	return anyRolloutUnder(root)
}

// anyRolloutUnder is the testable core of anyRolloutUnderSessionsRoot. It stops
// at the first hit, and an unreadable directory is skipped rather than aborting
// the walk: this feeds a diagnostic rung, so it must never turn a readable
// sibling into a wrong verdict.
func anyRolloutUnder(root string) bool {
	found := false
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil //nolint:nilerr // an unreadable shard must not abort the probe
		}
		if d.IsDir() {
			return nil
		}
		if _, ok := rolloutUUIDFromPath(path); ok {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

func (s *Server) ResolveInteractiveSessionID(_ context.Context, req *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) { //nolint:unparam // interface implementation
	var id, path, reason string
	var ambiguous bool
	// Process-fd resolution is the authoritative path: bind this chat to the
	// rollout its own codex process (under the given tmux pane) holds open. It
	// is deterministic and race-free, so siblings in one worktree never collide.
	// Gated on a live pane pid ALONE. It used to also require
	// !AllowLegacyBackfill, which meant the three RECOVERY sites — wake,
	// RecordChat resume, and the host-service lookup — could never reach it even
	// once they knew which pane a chat owned, so a chat with a live codex
	// process and a nil ProviderSessionID was unbindable by the one mechanism
	// that could read its id (BOS-1298). AllowLegacyBackfill now selects the
	// FALLBACK scan below, which is what the flag is actually for; a caller with
	// no live pane still passes pane pid 0 and takes that path unchanged.
	//
	// NOTE: this relies on codex keeping the rollout fd open for the process
	// lifetime (verified in the BOS-290 spike). If a future codex stops doing
	// so, resolution silently regresses to the time-window fallback — no worse
	// than before this change.
	if req.GetPanePid() > 0 {
		fdID, fdPath, outcome := resolveInteractiveSessionIDByPID(s.inspector, req.WorkDir, int(req.GetPanePid()))
		if outcome == fdOutcomeBound {
			return &bossanovav1.ResolveInteractiveSessionIDResponse{
				Found:          true,
				SessionId:      fdID,
				TranscriptPath: fdPath,
			}, nil
		}
		// Everything from here down is a MISS, and which misses terminate here
		// depends on WHO asked. A LAUNCH poll (AllowLegacyBackfill false) must
		// never accept the racy time-window scan below — with its -2s window slack
		// it could bind a sibling chat's already-written rollout in the same
		// worktree (BOS-290) — so it reports the miss and gets polled again until
		// its OWN fd appears. A RECOVERY caller (wake, RecordChat resume, the
		// host-service lookup) gets no such second chance: its chat's codex may be
		// long gone while tmux `remain-on-exit` still reports a live pane pid, and
		// then every fd probe fails forever. It therefore falls THROUGH to the
		// time-window scan on any non-binding outcome — which is precisely what
		// AllowLegacyBackfill selects for (BOS-1298).
		if !req.GetAllowLegacyBackfill() {
			// Rung 1 — the probe itself could not run. No amount of waiting repairs
			// it, so it is reported ahead of the routinely-true "not yet" below.
			if outcome == fdOutcomeProbeFailed {
				return &bossanovav1.ResolveInteractiveSessionIDResponse{
					Found:  false,
					Reason: reasonPaneProbeFailed,
				}, nil
			}
			// Rung 2 — the tree was fully readable and holds no rollout, and there is
			// no rollout under the sessions root at all. The root is wrong; waiting
			// does not make one appear.
			if outcome == fdOutcomeNoRolloutFDOpen && !anyRolloutUnderSessionsRoot() {
				return &bossanovav1.ResolveInteractiveSessionIDResponse{
					Found:  false,
					Reason: reasonNoRolloutFound,
				}, nil
			}
			// Trade-off: on a host where `ps` works but open-file reads never do
			// (e.g. Linux hidepid where /proc/<pid>/fd is denied), the outcome is
			// fdOutcomeProbeFailed — or fdOutcomeNoRolloutFDOpen, when the probe
			// returns an empty list without erroring — and never
			// fdOutcomeTreeNotVisible, because the tree WAS enumerable. So a launch
			// poll on such a host keeps waiting for its own fd for the whole live
			// session, which is the deliberate choice: binding a sibling's rollout
			// is worse than binding late. A recovery caller does not wait — it fell
			// through above and the time-window scan binds the id. The launch-side
			// wait is in any case rare, because bossd spawns codex as the same user
			// and same-user /proc/<pid>/fd is readable by default.
			//
			// Rung 3 — rollouts exist, this tree simply holds none of them open yet.
			// The one rung whose remedy really is "wait", so it comes last. Only
			// fdOutcomeTreeNotVisible (fd inspection genuinely unavailable here)
			// escapes to the time-window scan on the launch path.
			if outcome != fdOutcomeTreeNotVisible {
				return &bossanovav1.ResolveInteractiveSessionIDResponse{
					Found:  false,
					Reason: reasonPaneRolloutFDNotOpenYet,
				}, nil
			}
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
	// A miss must always name itself. The scan helpers already do, but a caller
	// that only ever sees an empty Reason cannot tell "codex is still starting"
	// (retry, the fd will appear) from "nothing matched this worktree" (the
	// window is wrong) — and that distinction is what the daemon logs on its
	// discovery warn line.
	if id == "" && reason == "" {
		reason = reasonNoRolloutFound
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
// the Lane 0 spike. It also matches a conversational reply-choice instruction
// ("Reply 1, 2, or 3.") that codex asked in prose with the composer left live
// (BOS-1180); unlike the drawn-UI arms that one is bounded to the rendered
// tail, because a prose line is transcript and never redraws itself away.
// blocks_input (hasCodexModalPrompt) runs that same question grammar over the
// TAIL of the pane rather than all of it, and adds clauses has_prompt does
// not have. has_prompt asks "has this chat asked something?", which is worth
// surfacing wherever in the buffer it appears; blocks_input asks "is the
// composer taken right now?", and a capture carries up to 1000 lines of
// scrollback in which a long-answered approval footer still sits. Reading that
// pane-wide would wedge delivery to an idle chat forever (BOS-600).
//
// The clause that decides the subset question is the boot interstitial
// (BOS-894): codex's "Update
// available!" screen owns the composer but asks nothing, so it must block
// delivery WITHOUT notifying — it answers blocks_input=true, has_prompt=false.
// blocks_input is therefore NOT a subset of has_prompt, and neither answer may
// be inferred from the other.
func (s *Server) HasQuestionPrompt(_ context.Context, req *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.HasQuestionPromptResponse{
		HasPrompt:   hasCodexQuestionPrompt(req.PaneContent),
		BlocksInput: hasCodexModalPrompt(req.PaneContent),
	}, nil
}

// HasWorkingIndicator reports whether the pane shows an affirmative "this chat
// is busy" marker. Delegates to hasCodexWorkingIndicator, which owns codex's
// TUI grammar the way statusdetect owns claude's.
//
// This used to return a hardcoded false, on the reasoning that codex's spinner
// animates and so the daemon's content-diff path already reports WORKING with
// no static-but-busy pane left to rescue. The spinner half of that is true; the
// conclusion was not. A codex turn that backgrounds a child and then RETURNS
// leaves a pane that is finished — no spinner, composer available, nothing
// redrawing — while the child keeps running, and the diff path correctly sees
// no change and flips the chat to IDLE after IdleThreshold. Claude reports that
// same state WORKING off its own "N shells still running" footer; codex
// reported IDLE purely because this RPC declined to look.
func (s *Server) HasWorkingIndicator(_ context.Context, req *bossanovav1.HasWorkingIndicatorRequest) (*bossanovav1.HasWorkingIndicatorResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.HasWorkingIndicatorResponse{
		IsWorking: hasCodexWorkingIndicator(req.PaneContent),
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

// ProbeProgressLiveness always reports known=false for codex.
//
// The RPC's contract is "what does the LAST record leave the agent doing", and
// codex's rollout JSONL does not answer that question cleanly. Its tail is
// dominated by bookkeeping envelopes that carry no phase meaning at all —
// event_msg/token_count is emitted periodically, and turn_context and
// task_started interleave with the semantic records — so the final line is
// routinely one of those rather than a user_message, agent_message,
// function_call, or function_call_output. Skipping past them to the last
// semantic record would report a phase for a moment that has already been
// superseded, and treating a bookkeeping envelope as progress would report
// liveness for a turn that has actually stalled: both are worse than silence.
//
// So we fail open (the same direction every other branch of this feature
// takes) rather than guess. The daemon raises nothing on known=false, which
// means codex sessions simply do not get stall detection yet — the deliberate,
// documented scope choice for BOS-667. Wiring codex properly needs a phase
// mapping derived from the rollout protocol itself, not from this heuristic.
func (s *Server) ProbeProgressLiveness(_ context.Context, _ *bossanovav1.ProbeProgressLivenessRequest) (*bossanovav1.ProbeProgressLivenessResponse, error) { //nolint:unparam // interface implementation; fail-open means the error result is always nil
	return &bossanovav1.ProbeProgressLivenessResponse{
		Phase:   bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_UNKNOWN,
		IsKnown: false,
	}, nil
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
	return readTranscriptAt(root, req.WorkDir, req.AgentSessionId, req.MaxMessages, s.logger)
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
