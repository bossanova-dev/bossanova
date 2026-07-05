// Package main implements the Claude agent plugin's AgentRunnerService.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/loginshell"
	"github.com/recurser/bossalib/plugin/hostclient"
	"github.com/recurser/bossalib/statusdetect"
)

// suggestPRTitleTimeout bounds the one-shot title-suggestion agent call so a
// hung/slow model can never stall finalize; on timeout the daemon falls back to
// its deterministic heuristic.
const suggestPRTitleTimeout = 30 * time.Second

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
}

func newServer(host hostclient.Client, logger zerolog.Logger, runnerOpts ...RunnerOption) *Server {
	s := &Server{
		host:   host,
		logger: logger,
		runner: NewRunner(logger, runnerOpts...),
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
	sid, err := s.runner.Start(context.Background(), req.WorkDir, req.Plan, resume, req.SessionId, req.LogPath, req.GetModel(), req.GetExtraEnv())
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
	if sp := req.GetAppendSystemPrompt(); sp != "" {
		args = append(args, "--append-system-prompt", sp)
	}
	if model := req.GetModel(); model != "" {
		args = append(args, "--model", model)
	}
	// --mcp-config wires the boss MCP server into this session so mcp__boss__*
	// tools are reachable. bossd writes the config to its app-data dir (never the
	// worktree) and passes the absolute path; empty means no boss MCP wiring.
	if mcpCfg := req.GetMcpConfigPath(); mcpCfg != "" {
		args = append(args, "--mcp-config", mcpCfg)
	}
	loginShell := ""
	if s.runner != nil {
		loginShell = s.runner.loginShell
	}
	return &bossanovav1.BuildInteractiveCommandResponse{
		Argv:          loginshell.Wrap(loginShell, loginshell.Flags(loginShell), args),
		ReadyMarker:   "❯",
		CommandPrefix: "/",
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
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = workDir
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
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
