// Package main implements the Claude agent plugin's AgentRunnerService.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/bossalib/agenterr"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/loginshell"
	"github.com/recurser/bossalib/plugin/hostclient"
	"github.com/recurser/bossalib/statusdetect"
)

// suggestPRTitleTimeout bounds the one-shot title-suggestion agent call so a
// hung/slow model can never stall finalize; on timeout the daemon falls back to
// its deterministic heuristic.
const suggestPRTitleTimeout = 30 * time.Second

const suggestPRTitleWaitDelay = 5 * time.Second

// suggestedTitleMaxLen caps a suggested PR title (GitHub renders long titles
// poorly and the daemon expects a single headline line).
const suggestedTitleMaxLen = 80

const pluginName = "claude"
const pluginVersion = "1"

// Server implements AgentRunnerService.
type Server struct {
	host   hostclient.Client
	logger zerolog.Logger
	runner *Runner

	// oneShot runs a single read-only claude prompt and returns its text output.
	// Defaults to runClaudeOneShot; overridable in tests so they never exec claude.
	oneShot func(ctx context.Context, workDir, prompt string) (string, error)

	// httpClient and usage URLs back ProbeRateLimit; all are overridable in tests.
	httpClient  *http.Client
	usageURL    string
	messagesURL string

	// progress memoizes transcript progress snapshots for ProbeProgressLiveness,
	// which the daemon calls on its poll tick for every live chat. Its zero
	// value is usable, so it needs no setup in newServer.
	progress progressProbe

	// initProber reads the stream-json system/init event backing
	// DescribeMCPSurface. Nil means the live prober, which spawns a real claude
	// process; tests replace it so they never exec claude.
	initProber claudeInitProber
}

func newServer(host hostclient.Client, logger zerolog.Logger, runnerOpts ...RunnerOption) *Server {
	s := &Server{
		host:   host,
		logger: logger,
		runner: NewRunner(logger, runnerOpts...),

		httpClient: &http.Client{Timeout: 10 * time.Second},
		usageURL:   claudeUsageURL,
	}
	s.oneShot = s.runClaudeOneShot
	return s
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
				{
					Key:          "model",
					Label:        "Model",
					Description:  "Fallback claude --model for runs without their own. Empty uses the claude CLI default.",
					Type:         bossanovav1.UserSettingType_USER_SETTING_TYPE_STRING,
					DefaultValue: "",
				},
				{
					Key:           "effort",
					Label:         "Reasoning effort",
					Description:   "Fallback claude --effort for runs without their own. Defaults to high.",
					Type:          bossanovav1.UserSettingType_USER_SETTING_TYPE_ENUM,
					AllowedValues: []string{"", "low", "medium", "high", "xhigh", "max"},
					DefaultValue:  "high",
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
	sid, err := s.runner.StartWithModelAndEffort(context.Background(), req.WorkDir, req.Plan, resume, req.SessionId, req.LogPath, req.GetModel(), req.GetEffort(), req.GetExtraEnv())
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
	resp := &bossanovav1.AgentExitStatusResponse{IsComplete: true}
	if err != nil {
		resp.ExitError = err.Error()
	}
	// Surface a usage-cap or rate-limit classification through the optional
	// fields while leaving exit_error (above) untouched. Claude has no auth
	// sentinel, so only the usage-limited and rate-limited branches apply here.
	var ul agenterr.ErrUsageLimited
	var rl agenterr.ErrRateLimited
	switch {
	case errors.As(err, &ul):
		fc := agenterr.KindUsageExhausted.String()
		resp.FailureClass = &fc
		if !ul.ResetAt.IsZero() {
			resp.ResetAt = timestamppb.New(ul.ResetAt)
		}
	case errors.As(err, &rl):
		// Rate-limited carries no reset; only FailureClass is set. This is for
		// logging fidelity — the rotation trigger flows through exit_error.
		fc := agenterr.KindRateLimited.String()
		resp.FailureClass = &fc
	}
	return resp, nil
}

func (s *Server) ConfigureFinalizeHook(_ context.Context, req *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error) {
	if err := WriteHookConfig(req.WorkDir, req.SessionId, req.AgentSessionId, req.HookToken, int(req.HookPort)); err != nil {
		return nil, status.Errorf(codes.Internal, "write hook config: %v", err)
	}
	return &bossanovav1.ConfigureFinalizeHookResponse{IsSupported: true}, nil
}

func (s *Server) RemoveAgentRunHook(_ context.Context, req *bossanovav1.RemoveAgentRunHookRequest) (*bossanovav1.RemoveAgentRunHookResponse, error) { //nolint:unparam // interface implementation
	if err := RemoveRunHookConfig(req.WorkDir, req.AgentSessionId); err != nil {
		return nil, status.Errorf(codes.Internal, "remove run hook: %v", err)
	}
	return &bossanovav1.RemoveAgentRunHookResponse{IsSupported: true}, nil
}

// BuildInteractiveCommand returns the bare claude argv for tmux-hosted
// runs. It must NOT wrap the argv in a `bash -c "claude … | tee log"`
// pipeline (the shape this RPC used pre-#350): piping claude's stdout
// through tee makes isatty(stdout)=false, and modern claude treats a
// non-TTY stdout as headless-print mode. Without a -p/--print prompt
// arg it errors with "Input must be provided either through stdin or as
// a prompt argument when using --print", the tmux pane never shows the
// ❯ ready marker, SendPlan times out at 5s, and every repair attempt
// fails forever.
//
// Log capture for interactive runs is the caller's job. The bossd
// host-side StartTmuxChat (services/bossd/internal/session/tmux_chat.go)
// arms `tmux pipe-pane` against the LogPath the caller passed in this
// request, which mirrors pane output to disk without touching claude's
// stdio.
//
// req.LogPath is accepted for API compatibility but intentionally
// unused — bossd reads it from its own state, not from this response.
//
// The error result is always nil now that argv assembly is pure: the only
// failure this ever had was the strict-MCP guard, and boss no longer owns MCP
// configuration. The signature stays as-is because it implements the plugin
// interface.
func (s *Server) BuildInteractiveCommand(_ context.Context, req *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) { //nolint:unparam // interface implementation
	args := []string{"claude"}
	if req.Resume {
		args = append(args, "--resume", req.SessionId)
	} else {
		args = append(args, "--session-id", req.SessionId)
	}
	if s.runner != nil && s.runner.dangerouslySkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	// The declaration is set in the same branch that appends the flag, so it
	// cannot drift from what actually reached argv: no append, no claim.
	appendSystemPromptSupport := bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_UNSPECIFIED
	if sp := req.GetAppendSystemPrompt(); sp != "" {
		args = append(args, "--append-system-prompt", sp)
		appendSystemPromptSupport = bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_IN_ARGV
	}
	pluginModel := ""
	if s.runner != nil {
		pluginModel = s.runner.model
	}
	if model := resolveClaudeModel(req.GetModel(), pluginModel); model != "" {
		args = append(args, "--model", model)
	}
	pluginEffort := ""
	if s.runner != nil {
		pluginEffort = s.runner.effort
	}
	if effort := resolveClaudeEffort(req.GetEffort(), pluginEffort); effort != "" {
		args = append(args, "--effort", effort)
	}
	// No MCP flags, deliberately. Boss does not own MCP configuration: Claude
	// Code discovers servers its own native way (project .mcp.json gated by
	// enabledMcpjsonServers, plus the user and local scopes) and the repo is
	// responsible for declaring them. --strict-mcp-config would suppress exactly
	// that — it is the coupling this removes, not an isolation guarantee worth
	// keeping. See docs/solutions/workflow-issues/repo-owns-its-mcp-servers.md.
	loginShell := ""
	if s.runner != nil {
		loginShell = s.runner.loginShell
	}
	return &bossanovav1.BuildInteractiveCommandResponse{
		Argv:                      loginshell.Wrap(loginShell, loginshell.Flags(loginShell), args),
		ReadyMarker:               "❯",
		CommandPrefix:             "/",
		AppendSystemPromptSupport: appendSystemPromptSupport,
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
	title, explicit := chatTitle(req.WorkDir, req.SessionId)
	return &bossanovav1.GetChatTitleResponse{
		Supported: true,
		Title:     title,
		Explicit:  explicit,
	}, nil
}

// SuggestPRTitle asks claude (one-shot, read-only, headless) to propose a PR
// title from the current title + PR body + git log, returning the current title
// verbatim when it already fits. Any failure or empty result returns
// supported=false so the daemon falls back to its deterministic heuristic — a
// title suggestion must never block finalize.
func (s *Server) SuggestPRTitle(ctx context.Context, req *bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error) {
	runCtx, cancel := context.WithTimeout(ctx, suggestPRTitleTimeout)
	defer cancel()

	out, err := s.oneShot(runCtx, req.WorkDir, buildPRTitlePrompt(req))
	if err != nil {
		s.logger.Warn().Err(err).Msg("claude SuggestPRTitle failed; daemon will fall back to its deterministic title")
		return &bossanovav1.SuggestPRTitleResponse{Supported: false}, nil
	}
	title := sanitizeSuggestedTitle(out)
	if title == "" {
		return &bossanovav1.SuggestPRTitleResponse{Supported: false}, nil
	}
	return &bossanovav1.SuggestPRTitleResponse{Supported: true, Title: title}, nil
}

// runClaudeOneShot runs `claude -p --permission-mode plan` (read-only headless)
// with the prompt piped on stdin and returns stdout. The plan permission mode
// forbids edits/executes; piping on stdin avoids ARG_MAX limits for large
// prompts. The caller-supplied ctx bounds the run (exec.CommandContext kills the
// process when ctx is done).
func (s *Server) runClaudeOneShot(ctx context.Context, workDir, prompt string) (string, error) {
	loginShell := ""
	if s.runner != nil {
		loginShell = s.runner.loginShell
	}
	argv := loginshell.Wrap(loginShell, loginshell.Flags(loginShell),
		[]string{"claude", "-p", "--permission-mode", "plan"})
	// #nosec G204 -- claude -p --permission-mode plan invoked via the operator-configured login shell (argv[0]); inner argv constant; local-trust
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = workDir
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = suggestPRTitleWaitDelay
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude one-shot: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func buildPRTitlePrompt(req *bossanovav1.SuggestPRTitleRequest) string {
	orEmpty := func(s, fallback string) string {
		if strings.TrimSpace(s) == "" {
			return fallback
		}
		return s
	}
	var b strings.Builder
	b.WriteString("You are titling a pull request. Output ONLY the pull request title on a single line — the title text itself and nothing else. ")
	b.WriteString("No quotes, no markdown, no preamble, and NO sentence that describes, evaluates, or comments on the title (never output things like \"The current title accurately describes the change\").\n\n")
	b.WriteString("When the CURRENT TITLE below already describes the whole change well, repeat it back exactly, character for character. ")
	b.WriteString("Otherwise write a new concise title (<= 70 characters) capturing the PR's overall intent — summarize the WHOLE change, not just the last commit.\n\n")
	b.WriteString("Example — CURRENT TITLE is \"[BOS-12] Add dark mode toggle\" and it already fits: output exactly\n[BOS-12] Add dark mode toggle\n\n")
	b.WriteString("CURRENT TITLE:\n")
	b.WriteString(orEmpty(req.GetCurrentTitle(), "(none)") + "\n\n")
	b.WriteString("BASE BRANCH: " + orEmpty(req.GetBaseBranch(), "(unknown)") + "\n\n")
	b.WriteString("PR DESCRIPTION:\n")
	b.WriteString(orEmpty(req.GetPrBody(), "(none)") + "\n\n")
	b.WriteString("COMMITS (oldest to newest):\n")
	b.WriteString(orEmpty(req.GetGitLog(), "(none)") + "\n")
	return b.String()
}

// sanitizeSuggestedTitle reduces raw model output to a single clean title line:
// first non-empty line, surrounding quotes/backticks stripped, length-capped.
func sanitizeSuggestedTitle(out string) string {
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) != "" {
			line = strings.TrimSpace(l)
			break
		}
	}
	line = strings.Trim(line, "\"'`")
	line = strings.TrimSpace(line)
	// Drop self-referential commentary the model sometimes emits instead of a
	// title (e.g. "The current title accurately describes the whole change.").
	// Returning "" makes SuggestPRTitle report unsupported, so the daemon keeps
	// the existing title rather than persisting the model's reasoning as one.
	if looksLikeTitleCommentary(line) {
		return ""
	}
	if utf8.RuneCountInString(line) > suggestedTitleMaxLen {
		runes := []rune(line)
		line = strings.TrimSpace(string(runes[:suggestedTitleMaxLen]))
	}
	return line
}

// looksLikeTitleCommentary reports whether a line is the model narrating its
// keep/replace decision rather than an actual PR title. The patterns are kept
// deliberately tight so they only catch meta-sentences about "the title", not
// genuine titles that happen to contain the word "describes".
func looksLikeTitleCommentary(line string) bool {
	l := strings.ToLower(strings.TrimSpace(line))
	if l == "" {
		return false
	}
	for _, prefix := range []string{"the current title", "the title", "current title", "this title"} {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	for _, sub := range []string{
		"accurately describes",
		"concisely describes",
		"describes the whole change",
		"describes the change",
		"describes the overall change",
		"already accurate",
		"already fits",
	} {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// HasQuestionPrompt reports whether the supplied pane bytes look like a Claude
// Code question prompt (AskUserQuestion / permission UI / conversational ?).
// Delegates to bossalib/statusdetect, which is shared between the daemon's
// tmux poller and the client-side PTY monitor.
//
// The two fields are NOT the same predicate. has_prompt is the notify signal
// and includes a conversational "…?" asked with a live composer; blocks_input
// is, FOR THIS AGENT, the modal subset, and is the only one safe to gate
// delivery on — a caller that refused on has_prompt would refuse to answer the
// very question Claude just asked (BOS-600). The per-agent qualifier matters
// since BOS-894: plugin.proto no longer promises the subset in general, because
// codex answers blocks_input=true with has_prompt=false for a boot screen that
// owns the composer but asks nothing.
func (s *Server) HasQuestionPrompt(_ context.Context, req *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.HasQuestionPromptResponse{
		HasPrompt:   statusdetect.HasQuestionPrompt(req.PaneContent),
		BlocksInput: statusdetect.HasModalPrompt(req.PaneContent),
	}, nil
}

// HasWorkingIndicator reports whether the supplied pane bytes show an
// affirmative "still working" marker (running background work — a shell, a
// monitor — or an active spinner), so the daemon can keep a busy-but-static
// pane from flipping idle.
// Delegates to bossalib/statusdetect, shared between the daemon's tmux poller
// and the client-side PTY monitor.
func (s *Server) HasWorkingIndicator(_ context.Context, req *bossanovav1.HasWorkingIndicatorRequest) (*bossanovav1.HasWorkingIndicatorResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.HasWorkingIndicatorResponse{
		IsWorking: statusdetect.HasWorkingIndicator(req.PaneContent),
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

// ReadTranscript reads the Claude Code JSONL transcript for
// (work_dir, agent_session_id) and returns ordered ChatMessages. A missing
// transcript is not an error — it returns Exists=false with nil messages.
func (s *Server) ReadTranscript(_ context.Context, req *bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error) {
	path, err := transcriptPath(req.WorkDir, req.AgentSessionId)
	if err != nil {
		return &bossanovav1.ReadTranscriptResponse{Exists: false}, nil
	}
	msgs, finalAssistant, exists, err := readTranscript(path, req.MaxMessages)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read transcript: %v", err)
	}
	return &bossanovav1.ReadTranscriptResponse{
		Messages:           msgs,
		FinalAssistantText: finalAssistant,
		Exists:             exists,
	}, nil
}

// ProbeProgressLiveness reports when the Claude Code transcript for
// (work_dir, agent_session_id) last recorded semantic progress, and what that
// last record leaves the agent doing. It exists because pane content is not a
// progress signal: an animated spinner keeps the pane changing while the turn
// behind it is dead, so only the transcript distinguishes "thinking hard" from
// "silently stopped".
//
// Fail open, always: a missing, unreadable, empty, or unclassifiable transcript
// returns known=false with NO timestamp and phase=UNKNOWN, and never an error.
// The daemon must raise nothing on known=false — a false stall banner on a
// healthy long-running tool call is worse than the stall this detects.
func (s *Server) ProbeProgressLiveness(_ context.Context, req *bossanovav1.ProbeProgressLivenessRequest) (*bossanovav1.ProbeProgressLivenessResponse, error) { //nolint:unparam // interface implementation; fail-open means the error result is always nil
	unknown := &bossanovav1.ProbeProgressLivenessResponse{
		Phase: bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_UNKNOWN,
	}
	path, err := transcriptPath(req.WorkDir, req.AgentSessionId)
	if err != nil {
		// Includes the traversal guard rejecting a hostile session id.
		return unknown, nil
	}
	snap := s.progress.snapshot(path)
	if !snap.known {
		return unknown, nil
	}
	return &bossanovav1.ProbeProgressLivenessResponse{
		LastProgressAt: timestamppb.New(snap.lastProgressAt),
		Phase:          snap.phase,
		IsKnown:        true,
	}, nil
}

// RotationCapability: claude injects its credential as an env token.
func (s *Server) RotationCapability(_ context.Context, _ *bossanovav1.RotationCapabilityRequest) (*bossanovav1.RotationCapabilityResponse, error) { //nolint:unparam // interface implementation
	return &bossanovav1.RotationCapabilityResponse{
		SupportsRotation: true,
		AuthKind:         bossanovav1.AuthKind_AUTH_KIND_ENV_TOKEN,
	}, nil
}

// MaterializeAccount turns the stored claude credential blob (the raw OAuth
// token string) into a CLAUDE_CODE_OAUTH_TOKEN env overlay. No home dir, no
// files. The blob/token is NEVER logged.
func (s *Server) MaterializeAccount(_ context.Context, req *bossanovav1.MaterializeAccountRequest) (*bossanovav1.MaterializeAccountResponse, error) { //nolint:unparam // interface implementation; error result always nil today
	token := strings.TrimSpace(string(req.GetCredentialBlob()))
	return &bossanovav1.MaterializeAccountResponse{
		Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": token},
	}, nil
}
