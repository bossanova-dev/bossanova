package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/tmux"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// defaultLegacyAgent is the agent name used when an agent_chats row predates
// the agent_name column (or when a caller passes "" explicitly). The DB
// column already defaults to "claude" — this mirrors that for the in-memory
// path so liveTranscriptOracle and liveArgvBuilder route to the same plugin.
const defaultLegacyAgent = "claude"

// Outcome describes the result of a spawn attempt.
type Outcome int

const (
	// OutcomeAlreadyLive means the tmux session was already alive (or tmux is
	// unavailable, treated as a no-op). No spawn was attempted.
	OutcomeAlreadyLive Outcome = iota
	// OutcomeResumed means a new tmux session was spawned in the agent's
	// resume mode because a transcript for this AgentSessionID was found on
	// disk. Each agent plugin owns the exact CLI shape (claude → `--resume`,
	// codex → `resume` subcommand).
	OutcomeResumed
	// OutcomeFreshFallback means a new tmux session was spawned in the
	// agent's fresh-start mode because either ForceFresh was set or no
	// transcript was found on disk.
	OutcomeFreshFallback
)

const (
	WakeFallbackReasonTranscriptMissing                  = "transcript_missing"
	WakeFallbackReasonProviderIDMissing                  = "provider_id_missing"
	WakeFallbackReasonProviderIDDiscoveryTimeout         = "provider_id_discovery_timeout"
	WakeFallbackReasonProviderIDDiscoveryAmbiguous       = "provider_id_discovery_ambiguous"
	WakeFallbackReasonLegacyProviderIDDiscoveryAmbiguous = "legacy_provider_id_discovery_ambiguous"
)

var (
	// Foreground discovery runs before boss attaches to the freshly spawned
	// tmux session. Keep this short: slow Codex startup is covered by the
	// daemon's background provider-ID discovery and by attach-time legacy
	// backfill, so extending this window only recreates a long "Launching..."
	// screen without improving eventual resume correctness.
	interactiveProviderIDForegroundDiscoveryTimeout      = 2 * time.Second
	interactiveProviderIDForegroundDiscoveryPollInterval = 250 * time.Millisecond
)

// ErrWorktreeMissing means the chat's session worktree directory does not
// exist on disk, so we refuse to spawn (would create a tmux in a deleted
// path). Surfaced to the WakeChat handler as FAILED_PRECONDITION.
var ErrWorktreeMissing = errors.New("worktree directory missing")

// transcriptOracle abstracts the per-agent TranscriptExists RPC for
// testability. The agentName argument lets the live oracle dispatch to
// the matching AgentRunner plugin (claude knows where its JSONL lives;
// codex knows where its SQLite transcript lives) — the daemon stays
// agnostic to either schema.
type transcriptOracle interface {
	TranscriptExists(ctx context.Context, agentName, workDir, agentSessionID string) bool
}

// tmuxSpawner is the narrow surface of *tmux.Client used by spawnChatTmux.
type tmuxSpawner interface {
	Available(ctx context.Context) bool
	HasSession(ctx context.Context, name string) bool
	NewSessionWithCmd(ctx context.Context, name, workDir string, cmd []string) error
}

// argvBuilder resolves the tmux command argv for a given agent. The live
// impl dispatches to the matching AgentRunner plugin's
// BuildInteractiveCommand RPC so each agent can own its own CLI shape and
// flag wiring (claude → `claude --resume <id>`, codex → `codex resume <id>`,
// plus per-plugin user settings). spawnChatTmux stays agent-agnostic.
type argvBuilder interface {
	BuildInteractive(ctx context.Context, agentName, agentSessionID string, resume bool, logPath string) ([]string, error)
}

type interactiveSessionResolution struct {
	SessionID string
	Ambiguous bool
	Reason    string
}

type interactiveSessionResolver interface {
	ResolveInteractiveSessionID(ctx context.Context, agentName, workDir, requestedSessionID string, launchedAfter, chatCreatedAt time.Time, allowLegacyBackfill bool) (interactiveSessionResolution, error)
}

// spawnDeps groups the abstractions spawnChatTmux needs.
type spawnDeps struct {
	Tmux        tmuxSpawner
	Transcripts transcriptOracle
	Argv        argvBuilder
	Resolver    interactiveSessionResolver
}

// spawnInput captures the per-chat parameters for a spawn attempt.
type spawnInput struct {
	Chat         *models.AgentChat
	WorktreePath string
	TmuxName     string
	ForceFresh   bool
}

type spawnResult struct {
	Outcome            Outcome
	LaunchedAt         time.Time
	ProviderSessionID  string
	FallbackReason     string
	DiscoveryAmbiguous bool
	DiscoveryReason    string
}

func chatResumeSessionID(chat *models.AgentChat) (string, bool) {
	if chat == nil {
		return "", false
	}
	if chat.ProviderSessionID != nil && *chat.ProviderSessionID != "" {
		return *chat.ProviderSessionID, true
	}
	return chat.AgentSessionID, false
}

func freshFallbackReason(chat *models.AgentChat, forceFresh bool, hasProviderSessionID bool) string {
	if forceFresh {
		return ""
	}
	if chat != nil && chat.AgentName == "codex" && !hasProviderSessionID {
		return WakeFallbackReasonProviderIDMissing
	}
	return WakeFallbackReasonTranscriptMissing
}

// spawnChatTmux is the single source of truth for "ensure a tmux pane
// running this chat's agent exists". Used by ensureChatTmuxSession (start
// path) and WakeChat (revive path). Idempotent: returns OutcomeAlreadyLive
// without spawning when the named tmux is already alive or tmux is
// unavailable.
//
// When a spawn is required, resume vs. fresh is decided by a transcript
// pre-flight: if ForceFresh is set, always fresh; otherwise consult the
// transcript oracle. This avoids asking the plugin to resume against a
// transcript that doesn't exist (which would fail at the agent CLI).
//
// Argv resolution is delegated to deps.Argv so each agent plugin owns its
// own CLI shape and per-plugin user settings (e.g. claude's
// `--dangerously-skip-permissions`, codex's `--sandbox`/`--ask-for-approval`/
// `--model`). spawnChatTmux stays agent-agnostic.
func spawnChatTmux(ctx context.Context, deps spawnDeps, in spawnInput) (spawnResult, error) {
	if deps.Tmux == nil || !deps.Tmux.Available(ctx) {
		return spawnResult{Outcome: OutcomeAlreadyLive}, nil
	}

	if deps.Tmux.HasSession(ctx, in.TmuxName) {
		return spawnResult{Outcome: OutcomeAlreadyLive}, nil
	}

	if _, err := os.Stat(in.WorktreePath); err != nil {
		if os.IsNotExist(err) {
			return spawnResult{}, ErrWorktreeMissing
		}
		return spawnResult{}, fmt.Errorf("stat worktree: %w", err)
	}

	resumeID, hasProviderSessionID := chatResumeSessionID(in.Chat)
	resume := !in.ForceFresh && deps.Transcripts.TranscriptExists(ctx, in.Chat.AgentName, in.WorktreePath, resumeID)
	fallbackReason := freshFallbackReason(in.Chat, in.ForceFresh, hasProviderSessionID)

	if deps.Argv == nil {
		return spawnResult{}, fmt.Errorf("spawn chat tmux: argv builder not configured")
	}
	// LogPath is intentionally empty: this is the user-attached path where
	// the operator is reading tmux directly, not the unattended-headless
	// path StartTmuxChat handles. Plugins treat empty LogPath as "don't
	// tee" (LogTeeArgv is a no-op when the path is empty).
	args, err := deps.Argv.BuildInteractive(ctx, in.Chat.AgentName, resumeID, resume, "")
	if err != nil {
		return spawnResult{}, fmt.Errorf("build interactive command for agent %q: %w", in.Chat.AgentName, err)
	}
	if len(args) == 0 {
		return spawnResult{}, fmt.Errorf("argv builder returned empty command for agent %q", in.Chat.AgentName)
	}

	launchedAt := time.Now().UTC()
	if err := deps.Tmux.NewSessionWithCmd(ctx, in.TmuxName, in.WorktreePath, args); err != nil {
		return spawnResult{}, fmt.Errorf("new tmux session: %w", err)
	}

	if resume {
		return spawnResult{Outcome: OutcomeResumed, LaunchedAt: launchedAt}, nil
	}

	if deps.Resolver != nil {
		deadline := time.Now().Add(interactiveProviderIDForegroundDiscoveryTimeout)
		for time.Now().Before(deadline) {
			resolution, err := deps.Resolver.ResolveInteractiveSessionID(ctx, in.Chat.AgentName, in.WorktreePath, in.Chat.AgentSessionID, launchedAt, time.Time{}, false)
			if err != nil {
				return spawnResult{}, err
			}
			if resolution.SessionID != "" {
				return spawnResult{Outcome: OutcomeFreshFallback, LaunchedAt: launchedAt, ProviderSessionID: resolution.SessionID, FallbackReason: fallbackReason}, nil
			}
			if resolution.Ambiguous {
				return spawnResult{
					Outcome:            OutcomeFreshFallback,
					LaunchedAt:         launchedAt,
					FallbackReason:     WakeFallbackReasonProviderIDDiscoveryAmbiguous,
					DiscoveryAmbiguous: true,
					DiscoveryReason:    resolution.Reason,
				}, nil
			}
			select {
			case <-ctx.Done():
				return spawnResult{Outcome: OutcomeFreshFallback, LaunchedAt: launchedAt, FallbackReason: WakeFallbackReasonProviderIDDiscoveryTimeout}, nil
			case <-time.After(interactiveProviderIDForegroundDiscoveryPollInterval):
			}
		}
		return spawnResult{Outcome: OutcomeFreshFallback, LaunchedAt: launchedAt, FallbackReason: WakeFallbackReasonProviderIDDiscoveryTimeout}, nil
	}
	return spawnResult{Outcome: OutcomeFreshFallback, LaunchedAt: launchedAt, FallbackReason: fallbackReason}, nil
}

// liveTmuxSpawner adapts *tmux.Client to the tmuxSpawner interface.
type liveTmuxSpawner struct{ c *tmux.Client }

// Available reports whether tmux is installed on the host.
func (l liveTmuxSpawner) Available(ctx context.Context) bool { return l.c.Available(ctx) }

// HasSession reports whether the named tmux session is currently alive.
func (l liveTmuxSpawner) HasSession(ctx context.Context, name string) bool {
	return l.c.HasSession(ctx, name)
}

// NewSessionWithCmd creates a detached tmux session running the given command.
func (l liveTmuxSpawner) NewSessionWithCmd(ctx context.Context, name, workDir string, cmd []string) error {
	return l.c.NewSession(ctx, tmux.NewSessionOpts{Name: name, WorkDir: workDir, Command: cmd})
}

type liveInteractiveSessionResolver struct {
	clients map[string]agent.AgentRunnerClient
}

func (r liveInteractiveSessionResolver) ResolveInteractiveSessionID(ctx context.Context, agentName, workDir, requestedSessionID string, launchedAfter, chatCreatedAt time.Time, allowLegacyBackfill bool) (interactiveSessionResolution, error) {
	name := agentName
	if name == "" {
		name = defaultLegacyAgent
	}
	if r.clients == nil {
		return interactiveSessionResolution{}, nil
	}
	client, ok := r.clients[name]
	if !ok {
		return interactiveSessionResolution{}, nil
	}
	req := &bossanovav1.ResolveInteractiveSessionIDRequest{
		WorkDir:             workDir,
		RequestedSessionId:  requestedSessionID,
		AllowLegacyBackfill: allowLegacyBackfill,
	}
	if !launchedAfter.IsZero() {
		req.LaunchedAfter = timestamppb.New(launchedAfter)
	}
	if !chatCreatedAt.IsZero() {
		req.ChatCreatedAt = timestamppb.New(chatCreatedAt)
	}
	resp, err := client.ResolveInteractiveSessionID(ctx, req)
	if err != nil {
		return interactiveSessionResolution{}, fmt.Errorf("agent %q ResolveInteractiveSessionID: %w", name, err)
	}
	if resp == nil {
		return interactiveSessionResolution{}, nil
	}
	if resp.GetAmbiguous() {
		return interactiveSessionResolution{Ambiguous: true, Reason: resp.GetReason()}, nil
	}
	if !resp.GetFound() {
		return interactiveSessionResolution{}, nil
	}
	return interactiveSessionResolution{SessionID: resp.GetSessionId()}, nil
}

// liveTranscriptOracle dispatches TranscriptExists to the AgentRunner
// plugin matching agentName. A nil/empty registry, or an unknown agent
// name, returns false (safe default: spawn fresh rather than guess at a
// resume that would fail). The map is read-only after construction so
// concurrent reads from spawnChatTmux callers are race-free.
type liveTranscriptOracle struct {
	clients map[string]agent.AgentRunnerClient
}

// TranscriptExists asks the per-agent plugin whether a transcript for the
// given (worktree, agentSessionID) is present. The AgentName comes from
// the chat row; an empty AgentName falls through to the default registry
// key ("claude" today) for backward compatibility with rows persisted
// before AgentName was tracked.
func (o liveTranscriptOracle) TranscriptExists(ctx context.Context, agentName, workDir, agentSessionID string) bool {
	if o.clients == nil {
		return false
	}
	name := agentName
	if name == "" {
		name = defaultLegacyAgent
	}
	client, ok := o.clients[name]
	if !ok {
		return false
	}
	resp, err := client.TranscriptExists(ctx, &bossanovav1.TranscriptExistsRequest{
		WorkDir:        workDir,
		AgentSessionId: agentSessionID,
	})
	if err != nil || resp == nil {
		return false
	}
	return resp.GetExists()
}

// liveArgvBuilder dispatches BuildInteractive to the AgentRunner plugin
// matching agentName. Mirrors liveTranscriptOracle: the same registry, the
// same "" → "claude" legacy default. Refusing to spawn when the named
// plugin is absent (FailedPrecondition surface) is preferred over silently
// launching the wrong agent — that is exactly the bug PR #254 set out to
// fix.
type liveArgvBuilder struct {
	clients map[string]agent.AgentRunnerClient
}

// BuildInteractive resolves argv for (agentName, resume) by calling the
// matching plugin's BuildInteractiveCommand RPC. Plugins own their own CLI
// shape and per-plugin settings, so spawnChatTmux stays agnostic to either.
func (b liveArgvBuilder) BuildInteractive(ctx context.Context, agentName, agentSessionID string, resume bool, logPath string) ([]string, error) {
	name := agentName
	if name == "" {
		name = defaultLegacyAgent
	}
	if b.clients == nil {
		return nil, fmt.Errorf("agent runner registry not configured for agent %q", name)
	}
	client, ok := b.clients[name]
	if !ok {
		return nil, fmt.Errorf("agent runner not loaded for agent %q", name)
	}
	resp, err := client.BuildInteractiveCommand(ctx, &bossanovav1.BuildInteractiveCommandRequest{
		SessionId: agentSessionID,
		Resume:    resume,
		LogPath:   logPath,
	})
	if err != nil {
		return nil, fmt.Errorf("agent %q BuildInteractiveCommand: %w", name, err)
	}
	if resp == nil || len(resp.Argv) == 0 {
		return nil, fmt.Errorf("agent %q returned empty argv", name)
	}
	return resp.Argv, nil
}
