package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/detach"
	"github.com/recurser/bossd/internal/session"
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

// tmuxSpawner is the narrow surface of *tmux.Client used by spawnChatTmux
// and SendChatMessage.
type tmuxSpawner interface {
	Available(ctx context.Context) bool
	HasSession(ctx context.Context, name string) bool
	NewSessionWithCmd(ctx context.Context, name, workDir string, cmd []string, env map[string]string) error
	// SendMessage delivers text into a live chat composer. submit routes the
	// single-line verified-submit vs. paste-only-prefill behavior (BOS-242 Gap
	// 1); readyMarker is the agent's input-box prompt glyph the submit path waits
	// for before delivering. A payload the agent accepts into its queue behind a
	// running turn is recognised by the verifier from the pane itself (BOS-599),
	// so no working-state probe is threaded through here.
	//
	// modal is the chat agent's "is this pane a menu?" grammar, consulted by the
	// readiness gate before anything is typed; a pane showing one is refused
	// rather than delivered into (BOS-600). It is passed per call because it
	// varies per agent while the underlying client is shared by every chat. A nil
	// detector disables the check.
	SendMessage(ctx context.Context, sessionName, text string, submit bool, readyMarker string, modal tmux.ModalDetector, beforeSubmit func()) error
	// PanePID returns the login-shell PID of the named session's first pane, so
	// the codex provider-session resolver can bind a chat to the rollout its own
	// process holds open (BOS-290). Errors are non-fatal to the caller, which
	// then resolves with pane pid 0 (time-window fallback).
	PanePID(ctx context.Context, sessionName string) (int, error)
	// KillSession destroys the named tmux session. It exists on this interface
	// so a spawn that fails after the pane is already live can roll the pane
	// back (BOS-845): chat panes are spawned with RemainOnExit, so a pane whose
	// name never reached agent_chats.tmux_session_name is invisible to every
	// cleanup path and leaks for the host's lifetime. Callers treat the error as
	// advisory — see killSpawnedChatPaneBestEffort.
	KillSession(ctx context.Context, name string) error
}

// chatReadyMarker returns the input-box prompt glyph the given agent's TUI
// renders when it is ready to accept input. It gates the submit-verified send
// path (SendChatMessage) so it waits for the right agent's composer rather than
// timing out on the wrong glyph. Mirrors each agent plugin's
// BuildInteractiveCommandResponse ReadyMarker (claude "❯", codex "›"); an
// unknown or empty agent name falls back to the claude marker — the same "" →
// "claude" legacy default used elsewhere in this package.
func chatReadyMarker(agentName string) string {
	switch agentName {
	case "codex":
		return "›"
	case "opencode":
		return "┃"
	default:
		return "❯"
	}
}

// chatCommandPrefix returns the leading token an agent's CLI expects for a
// custom (boss) command: claude accepts "/boss-repair", codex only "$boss-repair"
// (it reserves "/" for its own built-ins and rejects an unknown "/boss-repair").
// Mirrors each agent plugin's BuildInteractiveCommandResponse CommandPrefix
// (claude "/", codex "$"); an unknown or empty agent name falls back to claude's
// "/", the same "" → "claude" legacy default used elsewhere in this package. It
// is the send-path sibling of chatReadyMarker: SendChatMessage renders a
// command-shaped message through this prefix so agent-neutral skill dispatches
// (e.g. "/boss-repair watch") reach codex correctly.
func chatCommandPrefix(agentName string) string {
	switch agentName {
	case "codex":
		return "$"
	case "opencode":
		return "/"
	default:
		return "/"
	}
}

// argvBuilder resolves the tmux command argv for a given agent. The live
// impl dispatches to the matching AgentRunner plugin's
// BuildInteractiveCommand RPC so each agent can own its own CLI shape and
// flag wiring (claude → `claude --resume <id>`, codex → `codex resume <id>`,
// plus per-plugin user settings). spawnChatTmux stays agent-agnostic.
//
// It returns the whole BuildInteractiveCommandResponse rather than just argv so
// callers can also read the runner's append_system_prompt declaration and report
// instructions that never reached the command line. Callers still spawn from
// GetArgv() alone — the declaration is reported, never enforced.
type argvBuilder interface {
	BuildInteractive(ctx context.Context, agentName, agentSessionID string, resume bool, worktreePath, logPath, appendSystemPrompt, model string, configHomeEnv map[string]string) (*bossanovav1.BuildInteractiveCommandResponse, error)
}

type interactiveSessionResolution struct {
	SessionID string
	Ambiguous bool
	Reason    string
}

type interactiveSessionResolver interface {
	ResolveInteractiveSessionID(ctx context.Context, agentName, workDir, requestedSessionID string, launchedAfter, chatCreatedAt time.Time, allowLegacyBackfill bool, panePID int) (interactiveSessionResolution, error)
}

// spawnDeps groups the abstractions spawnChatTmux needs.
type spawnDeps struct {
	Tmux        tmuxSpawner
	Transcripts transcriptOracle
	Argv        argvBuilder
	Resolver    interactiveSessionResolver
	// Logger receives the undelivered-instruction record when the runner does
	// not declare it carried AppendSystemPrompt into argv. The zero value is a
	// usable zerolog.Logger, so a caller that reports nothing (DescribeChatLaunch,
	// which builds no instructions) can leave it unset.
	Logger zerolog.Logger
}

// spawnInput captures the per-chat parameters for a spawn attempt.
type spawnInput struct {
	Chat         *models.AgentChat
	WorktreePath string
	TmuxName     string
	ForceFresh   bool
	// AppendSystemPrompt is the boss session-context suffix appended to the
	// agent's system prompt so record/wake-spawned chats carry the same boss
	// identifiers StartTmuxChat injects. Empty when no session context applies.
	AppendSystemPrompt string
	// AppendSystemPromptClasses names the instruction classes that went into
	// AppendSystemPrompt, so a runner that does not carry the suffix into argv
	// can be reported in terms of what was dropped. Callers get it from
	// session.BuildAppendSystemPrompt alongside the text; nil (the value a
	// caller that builds no instructions passes) reports nothing.
	AppendSystemPromptClasses []string
	// Model is the session's opaque agent model id ("" = plugin default). It
	// must thread through so a re-ensured (RecordChat) or woken (WakeChat) pane
	// launches on the same model as the initial StartTmuxChat rather than
	// silently reverting to the plugin default.
	Model string
	// SessionEnv is the canonical BOSS_* environment set on the spawned tmux session.
	SessionEnv map[string]string
	// SessionEnvFunc lazily builds SessionEnv after liveness checks pass. It lets
	// callers defer credential materialization until a spawn will actually happen.
	SessionEnvFunc func() map[string]string
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

// killSpawnedChatPaneBestEffort destroys a chat pane this attempt created but
// could not finish claiming, and never fails the caller: the error being rolled
// back is the one worth reporting, and a kill that could not run leaves the
// caller no better option than the log line below.
//
// Why it exists: chat panes are spawned with RemainOnExit (see
// liveTmuxSpawner.NewSessionWithCmd), so they never self-reap, and every
// cleanup path keys off agent_chats.tmux_session_name. A pane that went live
// while the row still carries no name (or a different one) is therefore
// invisible to teardown and leaks for the host's lifetime. Rolling the pane back
// makes "pane exists" and "row names the pane" a single atomic outcome (BOS-845).
// The invariant the rollback upholds: no live chat pane without a recorded name.
//
// Why it is detached: the failure being rolled back is frequently a context
// failure itself (the original production case was a DeadlineExceeded out of
// ResolveInteractiveSessionID; the row writes below can fail the same way), so
// on the caller's own context the kill would fail immediately — leaking exactly
// the pane this exists to reclaim. detach.Cleanup owns that derivation and the
// budget it runs under; see its package doc.
//
// # Audited return paths between pane creation and the name write (BOS-845)
//
// The pane becomes live at deps.Tmux.NewSessionWithCmd in spawnChatTmux; the
// claim completes when UpdateTmuxSessionName has stored TmuxName. Every return
// in between is covered as follows.
//
// In spawnChatTmux, after NewSessionWithCmd succeeds, there is currently NO
// error return at all, so its deferred rollback is unreachable by construction:
//   - the resolver failure inside the discovery loop — once the path that
//     produced the reported leak (`agent "codex" ResolveInteractiveSessionID:
//     ... DeadlineExceeded`), now degraded to a fresh-fallback success, because
//     foreground provider-id discovery is an optimization and losing it must not
//     cost the pane. Killing a usable pane was the strictly worse of the two
//     ways to handle it; not erroring at all is the better one.
//   - every success return (resumed, resolved id, ambiguous, ctx done, discovery
//     timeout, discovery failure, no resolver) — not rolled back here: the pane
//     is handed to the caller, which arms its own rollback for the row-write
//     window.
//
// The deferred rollback is kept regardless. It costs nothing while no error
// return exists, and it means a future error added between the pane going live
// and the caller taking ownership is covered the moment it is written, rather
// than silently reopening a leak that persists for the host's lifetime.
//
// Before NewSessionWithCmd (tmux unavailable, session already live, stat
// worktree, nil argv builder, BuildInteractive error, empty argv, and
// NewSessionWithCmd's own error) no pane of this attempt exists, so nothing is
// armed — notably OutcomeAlreadyLive, where the pane belongs to an earlier
// claim and killing it would destroy a live chat.
//
// In ensureChatTmuxSession, after a spawn with Outcome != OutcomeAlreadyLive:
//   - spawnChatTmux's error return — spawnChatTmux already killed the pane, and
//     the caller arms only after a successful spawn, so there is no double kill.
//   - "persist provider session id" — armed.
//   - "persist tmux session name" — armed.
//   - the final nil return — disarmed the moment the row names the pane,
//     including when the write was skipped because it already did.
//
// In WakeChatInternal, after a spawn with Outcome != OutcomeAlreadyLive: the
// same four, with the name write reported as "persist tmux name". Its earlier
// returns (chat/session lookup, the headless-run refusal, the legacy
// provider-id backfill) all precede the spawn, so no pane exists yet.
func killSpawnedChatPaneBestEffort(ctx context.Context, spawner tmuxSpawner, logger zerolog.Logger, agentSessionID, tmuxName string) {
	if spawner == nil || tmuxName == "" {
		return
	}
	detach.Cleanup(ctx, detach.CleanupBudget, func(killCtx context.Context) {
		if err := spawner.KillSession(killCtx, tmuxName); err != nil {
			logger.Warn().Err(err).
				Str("agentSessionID", agentSessionID).
				Str("tmuxSession", tmuxName).
				Msg("failed to kill orphaned chat tmux pane during spawn rollback")
		}
	})
}

// spawnChatTmux is the single source of truth for "ensure a tmux pane
// running this chat's agent exists". Used by ensureChatTmuxSession (start
// path) and WakeChat (revive path). Idempotent: returns OutcomeAlreadyLive
// without spawning when the named tmux is already alive or tmux is
// unavailable.
//
// Once the pane is live, every error return kills it again before returning, so
// a failed spawn never hands back an unnamed pane the cleanup paths cannot see
// (BOS-845). Callers remain responsible for the window between a *successful*
// spawn and the tmux_session_name write — see killSpawnedChatPaneBestEffort for
// the audited enumeration of both halves.
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
func spawnChatTmux(ctx context.Context, deps spawnDeps, in spawnInput) (res spawnResult, err error) {
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
	// LogPath is intentionally empty: this is the user-attached path
	// where the operator is reading tmux directly, not the unattended-
	// headless path StartTmuxChat handles. Even if it weren't empty,
	// the plugin's BuildInteractiveCommand no longer consumes LogPath
	// at all — pane capture is wired post-NewSession via tmux pipe-pane
	// by StartTmuxChat, and the WakeChat path here just doesn't need
	// any. Keeping the empty argument makes the contract explicit.
	// Resolve the spawn environment before building argv: Codex uses CODEX_HOME
	// or HOME to choose the account home that strict MCP setup must mirror.
	// Liveness was checked above, so this remains lazy for already-live panes.
	sessionEnv := in.SessionEnv
	if in.SessionEnvFunc != nil {
		sessionEnv = in.SessionEnvFunc()
	}
	cmdResp, err := deps.Argv.BuildInteractive(ctx, in.Chat.AgentName, resumeID, resume, in.WorktreePath, "", in.AppendSystemPrompt, in.Model, configHomeEnv(sessionEnv, in.WorktreePath))
	if err != nil {
		return spawnResult{}, fmt.Errorf("build interactive command for agent %q: %w", in.Chat.AgentName, err)
	}
	args := cmdResp.GetArgv()
	if len(args) == 0 {
		return spawnResult{}, fmt.Errorf("argv builder returned empty command for agent %q", in.Chat.AgentName)
	}
	// Report — never enforce. argv is already resolved above and is not touched
	// by what the runner did or did not declare about the instruction suffix.
	session.LogUndeliveredInstructions(deps.Logger, in.Chat.SessionID, in.Chat.AgentSessionID,
		in.Chat.AgentName, in.AppendSystemPromptClasses, cmdResp.GetAppendSystemPromptSupport())

	launchedAt := time.Now().UTC()
	if createErr := deps.Tmux.NewSessionWithCmd(ctx, in.TmuxName, in.WorktreePath, args, sessionEnv); createErr != nil {
		return spawnResult{}, fmt.Errorf("new tmux session: %w", createErr)
	}

	// The pane is live from here down. Roll it back on any error return rather
	// than handing the caller a pane whose name no cleanup path can learn: this
	// is registered after the create so the failures above — where no pane of
	// ours exists — stay untouched (BOS-845).
	//
	// No error return currently reaches this: every path below returns a
	// success, the resolver failure included. It stays armed so that adding one
	// later cannot silently reopen the leak — see killSpawnedChatPaneBestEffort.
	defer func() {
		if err != nil {
			killSpawnedChatPaneBestEffort(ctx, deps.Tmux, deps.Logger, in.Chat.AgentSessionID, in.TmuxName)
		}
	}()

	if resume {
		return spawnResult{Outcome: OutcomeResumed, LaunchedAt: launchedAt}, nil
	}

	if deps.Resolver != nil {
		// Resolve the pane pid once (stable for the session's life) so the
		// resolver can bind this chat to the rollout its own codex process holds
		// open. Best-effort: a tmux failure leaves panePID 0 and the resolver
		// falls back to the (worktree, time-window) scan. The poll loop below is
		// the fd-availability wait — the fd appears shortly after codex starts.
		panePID := 0
		if deps.Tmux != nil {
			if pid, pidErr := deps.Tmux.PanePID(ctx, in.TmuxName); pidErr == nil {
				panePID = pid
			}
		}
		deadline := time.Now().Add(interactiveProviderIDForegroundDiscoveryTimeout)
		for time.Now().Before(deadline) {
			resolution, resolveErr := deps.Resolver.ResolveInteractiveSessionID(ctx, in.Chat.AgentName, in.WorktreePath, in.Chat.AgentSessionID, launchedAt, time.Time{}, false, panePID)
			if resolveErr != nil {
				// Best-effort, exactly like the discovery timeout below: the pane is
				// live and usable, and an unbound provider id is recovered by the
				// daemon's background discovery or by attach-time legacy backfill.
				// Failing here instead would destroy a working chat over an
				// optimization — and, since the pane is already live, force a
				// rollback of it (BOS-845).
				//
				// The reason reported to callers is deliberately the same
				// discovery-timeout value a slow resolution produces: both mean "no
				// id yet, one is still being discovered", which is what the two UI
				// surfaces render from this field. The distinction that matters is
				// an operator's, so it lives in the log line rather than in a new
				// user-facing reason string.
				deps.Logger.Warn().Err(resolveErr).
					Str("agent_session_id", in.Chat.AgentSessionID).
					Str("agent", in.Chat.AgentName).
					Msg("foreground provider session id discovery failed; leaving the pane live for background discovery")
				return spawnResult{Outcome: OutcomeFreshFallback, LaunchedAt: launchedAt, FallbackReason: WakeFallbackReasonProviderIDDiscoveryTimeout}, nil
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

// chatSpawnDeps builds the spawn dependencies for a chat pane: the live
// adapters, with any wakeHook test overrides layered on top. Both spawn callers
// (ensureChatTmuxSession and WakeChatInternal) go through it, so a seam
// installed on the server is honoured on both paths — the record path used to
// build its deps inline and ignore the hook, which left its half of the
// pane-rollback contract unreachable from a test (BOS-845).
func (s *Server) chatSpawnDeps() spawnDeps {
	deps := spawnDeps{
		Transcripts: liveTranscriptOracle{clients: s.agentClients},
		Argv:        liveArgvBuilder{clients: s.agentClients},
		Logger:      s.logger,
	}
	if s.tmux != nil {
		deps.Tmux = liveTmuxSpawner{c: s.tmux}
	}
	if s.agentClients != nil {
		deps.Resolver = liveInteractiveSessionResolver{clients: s.agentClients}
	}
	if s.wakeHook.spawner != nil {
		deps.Tmux = s.wakeHook.spawner
	}
	if s.wakeHook.transcripts != nil {
		deps.Transcripts = s.wakeHook.transcripts
	}
	if s.wakeHook.argv != nil {
		deps.Argv = s.wakeHook.argv
	}
	if s.wakeHook.resolver != nil {
		deps.Resolver = s.wakeHook.resolver
	}
	return deps
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
// env carries the cron session-environment (BOSS_CRON et al.) so cron
// record/wake spawns match StartTmuxChat; it is nil for non-cron sessions.
func (l liveTmuxSpawner) NewSessionWithCmd(ctx context.Context, name, workDir string, cmd []string, env map[string]string) error {
	// RemainOnExit keeps a chat pane present after its agent process exits so
	// the status poller can capture a fast-exiting agent's final output at death
	// before reaping the pane (BOS-477). This spawner hosts only chat panes, so
	// arming it unconditionally is correct.
	return l.c.NewSession(ctx, tmux.NewSessionOpts{Name: name, WorkDir: workDir, Command: cmd, Env: env, RemainOnExit: true})
}

// SendMessage delivers text into a live chat composer, routing on submit intent
// (verified single-line submit vs. paste-only prefill) and payload shape, and
// refusing to deliver at all when modal reports the pane is showing a menu.
func (l liveTmuxSpawner) SendMessage(ctx context.Context, sessionName, text string, submit bool, readyMarker string, modal tmux.ModalDetector, beforeSubmit func()) error {
	return l.c.SendMessageWithModalBeforeSubmit(ctx, sessionName, text, submit, readyMarker, modal, beforeSubmit)
}

// PanePID returns the first pane's login-shell pid for the named session.
func (l liveTmuxSpawner) PanePID(ctx context.Context, sessionName string) (int, error) {
	return l.c.PanePID(ctx, sessionName)
}

// KillSession destroys the named tmux session. The underlying client already
// treats a definitely-absent session as success, so a rollback that races a
// pane which died on its own is not an error.
func (l liveTmuxSpawner) KillSession(ctx context.Context, name string) error {
	return l.c.KillSession(ctx, name)
}

type liveInteractiveSessionResolver struct {
	clients map[string]agent.AgentRunnerClient
}

func (r liveInteractiveSessionResolver) ResolveInteractiveSessionID(ctx context.Context, agentName, workDir, requestedSessionID string, launchedAfter, chatCreatedAt time.Time, allowLegacyBackfill bool, panePID int) (interactiveSessionResolution, error) {
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
	if panePID > 0 {
		req.PanePid = clampInt32(panePID)
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
func (b liveArgvBuilder) BuildInteractive(ctx context.Context, agentName, agentSessionID string, resume bool, worktreePath, logPath, appendSystemPrompt, model string, configHomeEnv map[string]string) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	name := agentName
	if name == "" {
		name = defaultLegacyAgent
	}
	if b.clients == nil {
		return nil, fmt.Errorf("agent runner registry not configured for agent %q", name)
	}
	client, ok := b.clients[name]
	if !ok {
		return nil, agent.AgentRunnerNotLoaded(name, b.clients)
	}
	resp, err := client.BuildInteractiveCommand(ctx, &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:          agentSessionID,
		Resume:             resume,
		WorktreePath:       worktreePath,
		LogPath:            logPath,
		AppendSystemPrompt: appendSystemPrompt,
		Model:              model,
		ConfigHomeEnv:      configHomeEnv,
	})
	if err != nil {
		return nil, fmt.Errorf("agent %q BuildInteractiveCommand: %w", name, err)
	}
	if resp == nil || len(resp.Argv) == 0 {
		return nil, fmt.Errorf("agent %q returned empty argv", name)
	}
	return resp, nil
}

// configHomeEnv forwards only the home selectors needed while argv is built;
// account credentials remain confined to the tmux session environment.
func configHomeEnv(env map[string]string, worktreePath string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, 2)
	for _, key := range []string{"CODEX_HOME", "HOME"} {
		if value, ok := env[key]; ok {
			if value != "" && !filepath.IsAbs(value) {
				value = filepath.Join(worktreePath, value)
			}
			out[key] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
