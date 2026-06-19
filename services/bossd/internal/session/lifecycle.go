// Package session provides the SessionLifecycle orchestrator that wires
// together worktree management, Claude process management, and the state
// machine for a complete session lifecycle.
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossalib/sessionreason"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/tmux"
)

var (
	conventionalCommitPrefixRE = regexp.MustCompile(`^[[:alpha:]][[:alnum:]-]*(\([^)]*\))?!?:[[:space:]]+`)
	prNumberPrefixRE           = regexp.MustCompile(`^\[#[0-9]+\][[:space:]]+`)
)

// PollArmer arms a poll-fallback goroutine that drives an agent run to
// completion when the agent doesn't support a finalize hook. Implemented
// by *agent.PollFallback (which accepts a wider AgentRunnerClient via
// interface satisfaction). Defined here so tests can fake it.
//
// We pass the full agent.AgentRunnerClient — even though the real
// PollFallback.Arm only needs ExitStatus — because Go's structural typing
// would otherwise create a name-only mismatch between session's narrower
// interface and agent's narrower interface.
type PollArmer interface {
	Arm(ctx context.Context, sessionID, agentSessionID string, client agent.AgentRunnerClient)
}

type pollRunCompleter interface {
	SignalRunComplete(sessionID, agentSessionID, exitError string)
}

type cronCompletionNotifier interface {
	NotifyCronAgentStopped(sessionID string)
}

// Lifecycle orchestrates worktree creation, Claude process management,
// and state machine transitions for coding sessions.
type Lifecycle struct {
	sessions    db.SessionStore
	repos       db.RepoStore
	agentChats  db.AgentChatStore
	cronJobs    db.CronJobStore
	worktrees   gitpkg.WorktreeManager
	agentRunner agent.AgentDispatcher
	tmux        *tmux.Client
	provider    vcs.Provider
	logger      zerolog.Logger

	// branchResolver, when set, lets the finalize attach path match PRs
	// against the worktree's live branch (the agent may have switched
	// branches). Nil means stored-branch matching only.
	branchResolver LiveBranchResolver

	// pollArmer is retained for compatibility with older wiring, but tmux-
	// hosted lifecycle paths no longer arm plugin ExitStatus polling.
	pollArmer PollArmer

	// pollCompleter preserves HostServiceServer's StartChatRun waiter path
	// for hookless tmux chats. Lifecycle-owned cron runs are not registered
	// in HostServiceServer, so cron finalization is driven by
	// cronCompletionNotifier below instead of host-service maps.
	pollCompleter pollRunCompleter

	// cronCompletionNotifier receives hookless poll completion candidates
	// for lifecycle-owned cron sessions.
	cronCompletionNotifier cronCompletionNotifier

	// daemonCtx is retained for compatibility with older SetDaemonCtx wiring.
	daemonCtx context.Context //nolint:containedctx // retained compatibility field

	// agentClientHookSupport caches the per-agent-name IsSupported result
	// from ConfigureFinalizeHook. Populated lazily during Bootstrap (and,
	// later, SetAgents). It is used only to skip the RPC for known-hookless
	// agents — hook-supporting agents still call ConfigureFinalizeHook per
	// surviving chat because the hook config is written per-worktree and must
	// carry the current daemon port. nil-safe: a missing entry triggers a
	// fresh probe.
	agentClientHookSupport map[string]bool

	// hookPort is the loopback TCP port of the daemon's Stop-hook server.
	// Stamped via SetHookPort once the hook server has bound, before any
	// session that needs a HookToken is started. Zero means "not yet set"
	// and StartSession will error out rather than write a config that
	// points at no listener.
	hookPort int

	// agents maps an agent plugin name (matching session.AgentName) to its
	// AgentRunnerClient — used for ConfigureFinalizeHook,
	// BuildInteractiveCommand, and other RPCs that aren't on AgentRunner.
	// Populated via SetAgents during daemon startup. An empty map is valid
	// (sessions without HookToken still work); the map is read-only after
	// SetAgents and lookups must not mutate it.
	agents map[string]agent.AgentRunnerClient

	// agentLogsDir is the bossd-owned directory where agent plugins tee
	// their interactive (tmux-hosted) output. Stamped via SetAgentLogsDir
	// during daemon startup. StartTmuxChat passes a per-agent-session log
	// path under this directory into BuildInteractiveCommand so the chat
	// list view's "tail my chat log" affordance has a stable location to
	// read from. An empty string disables interactive launch — StartTmuxChat
	// will reject the call with FailedPrecondition rather than write to an
	// unconfigured path.
	agentLogsDir string

	// newTmuxChatAgentSessionID mints daemon-local IDs for tmux-hosted
	// agent chats. Kept on the Lifecycle instance so tests can make one
	// fixture deterministic without changing package-global state.
	newTmuxChatAgentSessionID func() string

	// tmuxCompletionPollInterval controls how often lifecycle-owned hookless
	// cron runs check whether their tmux session has exited.
	tmuxCompletionPollInterval time.Duration
}

// SetHookPort records the hook server's bound loopback port so
// StartSession can stamp it into a worktree's settings.local.json when
// installing the Stop-hook config. Called from the daemon entrypoint
// after hookSrv.Listen() succeeds.
func (l *Lifecycle) SetHookPort(port int) {
	l.hookPort = port
}

// SetAgents installs the per-name AgentRunnerClient registry used to call
// ConfigureFinalizeHook (and other plugin RPCs) during StartSession. The
// map is keyed by agent plugin name (matching session.AgentName). Must be
// called before any session with a HookToken is started — sessions
// without a HookToken don't need this dep.
//
// An empty (or nil) map is valid: it just means no agent plugins are
// loaded, and StartSession will error with a clear message when a session
// that requires a hook tries to start. The map is treated as read-only
// after this call; callers must not mutate it.
//
// Concurrency: called exactly once during daemon startup, before serving
// begins. Not safe for concurrent re-injection alongside in-flight RPCs.
func (l *Lifecycle) SetAgents(m map[string]agent.AgentRunnerClient) { l.agents = m }

// SetBranchResolver attaches a live-branch resolver used when matching an
// existing PR to a session whose stored branch may have drifted.
func (l *Lifecycle) SetBranchResolver(branches LiveBranchResolver) {
	l.branchResolver = branches
}

// Bootstrap restores supported session-keyed finalize hooks for tmux chats
// that were alive when the daemon last shut down. Call once during daemon
// startup, after SetAgents and before serving begins.
//
// For each agent_chats row whose tmux session is still alive, Bootstrap
// looks up the parent session's HookToken and calls the agent plugin's
// ConfigureFinalizeHook so that session's worktree gets its hook config
// rewritten with this (post-restart) daemon's port. The call runs per
// surviving chat because the config is per-worktree; the per-agent-name
// support result is cached only to skip the RPC for known-hookless agents,
// which are re-armed onto the completion poll instead. Hookless tmux-hosted
// runs are intentionally not wired to PollFallback: plugin ExitStatus only
// observes StartAgentRun processes, not tmux-spawned processes.
//
// Failures (DB error, missing session, RPC error) are logged and skipped;
// a single bad row mustn't block the rest from re-arming.
func (l *Lifecycle) Bootstrap(ctx context.Context) {
	if l.agentChats == nil {
		return
	}
	chats, err := l.agentChats.ListWithTmuxSession(ctx)
	if err != nil {
		l.logger.Warn().Err(err).Msg("bootstrap re-arm: failed to list chats with tmux session")
		return
	}
	for _, chat := range chats {
		if chat == nil || chat.AgentSessionID == "" {
			continue
		}
		// HookToken lives on the parent session record (the per-run hook
		// token used by /boss-repair lives only in HostServiceServer's
		// in-memory map — that flavour of run is gone after a daemon
		// restart anyway). If the session has no HookToken there's
		// nothing the Stop hook would have authenticated against, so
		// nothing to fall back from.
		sess, err := l.sessions.Get(ctx, chat.SessionID)
		if err != nil {
			l.logger.Warn().Err(err).
				Str("agent_session", chat.AgentSessionID).
				Str("session", chat.SessionID).
				Msg("bootstrap re-arm: failed to load session; skipping")
			continue
		}
		if sess.HookToken == nil || *sess.HookToken == "" {
			continue
		}
		client, ok := l.agents[chat.AgentName]
		if !ok || client == nil {
			l.logger.Warn().
				Str("agent", chat.AgentName).
				Str("agent_session", chat.AgentSessionID).
				Msg("bootstrap re-arm: agent client missing; skipping")
			continue
		}
		tmuxName := tmux.ChatSessionName(sess.RepoID, chat.AgentSessionID)
		if chat.TmuxSessionName != nil && *chat.TmuxSessionName != "" {
			tmuxName = *chat.TmuxSessionName
		}

		// A known-hookless agent has no finalize hook to rewrite — re-arm the
		// completion poll directly and skip the RPC.
		if supported, known := l.agentClientHookSupport[chat.AgentName]; known && !supported {
			l.armTmuxCompletionForHooklessTmux(chat.SessionID, chat.AgentSessionID, tmuxName)
			continue
		}

		// Call ConfigureFinalizeHook for every surviving hook-capable chat:
		// the hook config is written into THIS session's worktree and carries
		// this daemon's (post-restart) hook port. The write is per-worktree,
		// so it must run for each chat — caching the support result across
		// sessions (the previous behavior) skipped the RPC for every chat
		// after the first, leaving those worktrees pointing at the dead
		// previous daemon's port so their Stop hook never reached the
		// restarted daemon. We still cache the support result to skip the RPC
		// for known-hookless agents (above).
		hookResp, err := client.ConfigureFinalizeHook(ctx, &bossanovav1.ConfigureFinalizeHookRequest{
			WorkDir:   sess.WorktreePath,
			SessionId: chat.SessionID,
			HookToken: *sess.HookToken,
			HookPort:  int32(l.hookPort),
		})
		if err != nil {
			l.logger.Warn().Err(err).
				Str("agent_session", chat.AgentSessionID).
				Msg("bootstrap re-arm: ConfigureFinalizeHook probe failed; skipping")
			continue
		}
		supported := hookResp.GetIsSupported()
		if l.agentClientHookSupport == nil {
			l.agentClientHookSupport = map[string]bool{}
		}
		l.agentClientHookSupport[chat.AgentName] = supported
		if supported {
			// Hook config (re)written for this worktree with the current port.
			continue
		}
		l.armTmuxCompletionForHooklessTmux(chat.SessionID, chat.AgentSessionID, tmuxName)
	}
}

// SetPollArmer retains compatibility with older daemon wiring. Tmux-hosted
// lifecycle paths intentionally do not arm plugin ExitStatus polling.
func (l *Lifecycle) SetPollArmer(p PollArmer) { l.pollArmer = p }

// SetPollCompleter wires the host-service waiter completion path for
// hookless StartChatRun calls.
func (l *Lifecycle) SetPollCompleter(c pollRunCompleter) { l.pollCompleter = c }

// SetCronCompletionNotifier wires the cron completion gate for hookless
// lifecycle-owned cron runs.
func (l *Lifecycle) SetCronCompletionNotifier(n cronCompletionNotifier) {
	l.cronCompletionNotifier = n
}

// SignalSessionRunComplete is called by PollFallback when a hookless agent
// run completes. It preserves host-service waiter completion for StartChatRun
// and independently routes lifecycle-owned cron sessions to the cron gate.
func (l *Lifecycle) SignalSessionRunComplete(sessionID, agentSessionID, exitError string) {
	if l.pollCompleter != nil {
		l.pollCompleter.SignalRunComplete(sessionID, agentSessionID, exitError)
	}
	l.notifyCronCompletionIfCronSession(context.Background(), sessionID)
}

func (l *Lifecycle) notifyCronCompletionIfCronSession(ctx context.Context, sessionID string) {
	if sessionID == "" || l.cronCompletionNotifier == nil || l.sessions == nil {
		return
	}

	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		l.logger.Warn().Err(err).Str("session", sessionID).Msg("poll fallback completion: failed to load session")
		return
	}
	if session == nil || session.CronJobID == nil || *session.CronJobID == "" {
		return
	}

	l.cronCompletionNotifier.NotifyCronAgentStopped(sessionID)
}

// SetDaemonCtx retains compatibility with older daemon wiring.
func (l *Lifecycle) SetDaemonCtx(ctx context.Context) { l.daemonCtx = ctx }

func (l *Lifecycle) armTmuxCompletionForHooklessRun(sessionID, agentSessionID, repoID string) {
	l.armTmuxCompletionForHooklessTmux(sessionID, agentSessionID, tmux.ChatSessionName(repoID, agentSessionID))
}

func (l *Lifecycle) armTmuxCompletionForHooklessTmux(sessionID, agentSessionID, tmuxName string) {
	if l.tmux == nil || (l.pollCompleter == nil && l.cronCompletionNotifier == nil) {
		return
	}
	ctx := l.daemonCtx
	if ctx == nil {
		ctx = context.Background()
	}
	interval := l.tmuxCompletionPollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}

	safego.Go(l.logger, func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			alive, err := l.tmux.HasSessionStatus(ctx, tmuxName)
			if err != nil {
				l.logger.Warn().Err(err).
					Str("session", sessionID).
					Str("agent_session", agentSessionID).
					Str("tmux_session", tmuxName).
					Msg("hookless cron tmux completion poll failed")
			} else if !alive {
				l.SignalSessionRunComplete(sessionID, agentSessionID, "")
				return
			}

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

func (l *Lifecycle) armTmuxCompletionForHooklessCron(sessionID, agentSessionID, repoID string) {
	l.armTmuxCompletionForHooklessRun(sessionID, agentSessionID, repoID)
}

// SetAgentLogsDir records the bossd-owned directory where agent plugins
// write their tmux-hosted interactive run logs. Mirrors the same field
// on HostServiceServer so StartTmuxChat can pass a deterministic
// log_path into BuildInteractiveCommand. Called from the daemon
// entrypoint after MkdirAll succeeds, before any session that spawns a
// tmux chat is started. An empty string leaves StartTmuxChat in a
// fail-closed state.
//
// Concurrency: called exactly once during daemon startup, before serving
// begins. Not safe for concurrent re-injection alongside in-flight RPCs.
func (l *Lifecycle) SetAgentLogsDir(dir string) { l.agentLogsDir = dir }

// agentClientFor returns the registered AgentRunnerClient for sess.AgentName.
// Returns an error wrapping agent.ErrAgentNotLoaded when no client matches —
// defense in depth against an AgentName the daemon was never configured for.
// CreateSession is expected to resolve AgentName before persistence, so an
// empty AgentName here indicates a stale row from before the multi-agent
// migration; the error names that case explicitly so operators can fix the
// data, and callers can use errors.Is to distinguish this from real RPC
// failures.
func (l *Lifecycle) agentClientFor(sess *models.Session) (agent.AgentRunnerClient, error) {
	if c, ok := l.agents[sess.AgentName]; ok && c != nil {
		return c, nil
	}
	return nil, fmt.Errorf("agent %q not loaded for session %s: %w", sess.AgentName, sess.ID, agent.ErrAgentNotLoaded)
}

// NewLifecycle creates a new session lifecycle orchestrator. cronJobs may be
// nil for callers that never spawn cron-linked sessions (tests, legacy flows);
// FinalizeSession requires it and will error if it's absent.
func NewLifecycle(
	sessions db.SessionStore,
	repos db.RepoStore,
	agentChats db.AgentChatStore,
	cronJobs db.CronJobStore,
	worktrees gitpkg.WorktreeManager,
	agentRunner agent.AgentDispatcher,
	tmux *tmux.Client,
	provider vcs.Provider,
	logger zerolog.Logger,
) *Lifecycle {
	return &Lifecycle{
		sessions:                   sessions,
		repos:                      repos,
		agentChats:                 agentChats,
		cronJobs:                   cronJobs,
		worktrees:                  worktrees,
		agentRunner:                agentRunner,
		tmux:                       tmux,
		provider:                   provider,
		logger:                     logger,
		newTmuxChatAgentSessionID:  uuid.NewString,
		tmuxCompletionPollInterval: 2 * time.Second,
	}
}

// StartSessionOpts bundles the optional inputs to StartSession. Each field
// has a zero-value default that preserves the historical behavior, so
// callers only need to populate the fields they care about.
type StartSessionOpts struct {
	// ExistingBranch, when non-empty, makes the worktree check out that
	// branch instead of creating a fresh one (used for existing PR sessions).
	ExistingBranch string

	// ForceBranch removes any pre-existing branch with the derived name
	// before creating the new worktree.
	ForceBranch bool

	// SkipSetupScript bypasses the repo's configured setup script
	// (e.g. for dependabot PRs that should not run user code).
	SkipSetupScript bool

	// SetupOutput receives streamed setup-script output, when non-nil.
	SetupOutput io.Writer

	// DeferPR skips the immediate draft-PR creation that StartSession
	// otherwise performs for sessions without a PR. The Stop-hook
	// finalize path is responsible for calling EnsurePR later.
	DeferPR bool

	// CronJobID, when non-empty, marks this session as cron-spawned
	// (persisted on the session record once the schema/store land).
	CronJobID string

	// HookToken, when non-empty, is the secret written into the
	// worktree's settings.local.json so the Stop hook can authenticate
	// to the bossd hook server. Plumbed through in flight leg 5.
	HookToken string

	// BranchName, when non-empty, overrides the default title-derived
	// branch name. Used by the cron path so each fire gets a unique
	// branch (e.g. cron-<slug>-<unix>) and a previous run's orphaned
	// branch can't trip ErrBranchExists on the next fire. Ignored when
	// ExistingBranch is set.
	BranchName string
}

// StartSession creates a worktree, starts a Claude process, and fires
// state machine events. It updates the session record with the worktree
// path, branch name, and Claude session ID.
//
// See StartSessionOpts for how to customize behavior. The zero-value opts
// preserve historical defaults: a fresh branch, setup script enabled,
// and an immediate draft PR for sessions without one.
func (l *Lifecycle) StartSession(ctx context.Context, sessionID string, opts StartSessionOpts) error {
	existingBranch := opts.ExistingBranch
	forceBranch := opts.ForceBranch
	skipSetupScript := opts.SkipSetupScript
	setupOutput := opts.SetupOutput
	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		return fmt.Errorf("get repo: %w", err)
	}

	// Initialize state machine at CreatingWorktree.
	sm := machine.New(machine.CreatingWorktree)

	// Update session state to CreatingWorktree and stamp the cron_job_id
	// when the cron scheduler spawned us. The cron linkage is set here
	// (rather than by the task orchestrator) so it's guaranteed to land
	// before any finalize path observes the row.
	creatingState := int(machine.CreatingWorktree)
	updateParams := db.UpdateSessionParams{
		State: &creatingState,
	}
	if opts.CronJobID != "" {
		cronJobID := &opts.CronJobID
		updateParams.CronJobID = &cronJobID
	}
	if opts.HookToken != "" {
		hookToken := &opts.HookToken
		updateParams.HookToken = &hookToken
	}
	if _, err := l.sessions.Update(ctx, sessionID, updateParams); err != nil {
		return fmt.Errorf("set creating_worktree state: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("repo", repo.LocalPath).
		Msg("creating worktree")

	// Determine setup script — skip it when the flag is set (e.g. dependabot PRs).
	setupScript := repo.SetupScript
	if skipSetupScript {
		setupScript = nil
	}

	// Create worktree: existing branch (PR) or new branch.
	var result *gitpkg.CreateResult
	if existingBranch != "" {
		result, err = l.worktrees.CreateFromExistingBranch(ctx, gitpkg.CreateFromExistingBranchOpts{
			RepoPath:          repo.LocalPath,
			BranchName:        existingBranch,
			WorktreeBaseDir:   repo.WorktreeBaseDir,
			RepoName:          repo.DisplayName,
			SetupScript:       setupScript,
			SetupScriptOutput: setupOutput,
		})
		if err != nil {
			// The branch may not exist on the remote yet (e.g. a Linear issue
			// with no PR). Fall back to creating a new branch with that name.
			l.logger.Info().
				Str("branch", existingBranch).
				Err(err).
				Msg("existing branch not found on remote, creating new branch")
			result, err = l.worktrees.Create(ctx, gitpkg.CreateOpts{
				RepoPath:          repo.LocalPath,
				BaseBranch:        session.BaseBranch,
				WorktreeBaseDir:   repo.WorktreeBaseDir,
				RepoName:          repo.DisplayName,
				Title:             session.Title,
				BranchName:        existingBranch,
				SetupScript:       setupScript,
				SetupScriptOutput: setupOutput,
				Force:             forceBranch,
			})
		}
	} else {
		result, err = l.worktrees.Create(ctx, gitpkg.CreateOpts{
			RepoPath:          repo.LocalPath,
			BaseBranch:        session.BaseBranch,
			WorktreeBaseDir:   repo.WorktreeBaseDir,
			RepoName:          repo.DisplayName,
			Title:             session.Title,
			BranchName:        opts.BranchName,
			SetupScript:       setupScript,
			SetupScriptOutput: setupOutput,
			Force:             forceBranch,
		})
	}
	if err != nil {
		return fmt.Errorf("create worktree: %w", err)
	}

	// Update session with worktree info.
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		WorktreePath: &result.WorktreePath,
		BranchName:   &result.BranchName,
	}); err != nil {
		return fmt.Errorf("update worktree path: %w", err)
	}

	// Install the Stop-hook config for cron-spawned sessions. This must
	// happen after the setup script ran (otherwise a script-written
	// settings.local.json would be clobbered by a non-merge write
	// elsewhere) and before claude.Start (so Claude reads the config on
	// startup). Non-cron sessions have an empty HookToken and skip this
	// path entirely, preserving historical behaviour.
	var hookResp *bossanovav1.ConfigureFinalizeHookResponse
	if opts.HookToken != "" {
		if l.hookPort == 0 {
			return fmt.Errorf("hook port not configured: SetHookPort must be called before starting sessions with a HookToken")
		}
		client, err := l.agentClientFor(session)
		if err != nil {
			return fmt.Errorf("agent client not configured: %w; SetAgents must be called before starting sessions with a HookToken", err)
		}
		hookResp, err = client.ConfigureFinalizeHook(ctx, &bossanovav1.ConfigureFinalizeHookRequest{
			WorkDir:   result.WorktreePath,
			SessionId: sessionID,
			HookToken: opts.HookToken,
			HookPort:  int32(l.hookPort),
		})
		if err != nil {
			return fmt.Errorf("configure finalize hook: %w", err)
		}
		if !hookResp.GetIsSupported() {
			l.logger.Info().Str("session", sessionID).Msg("agent does not support finalize hook")
		} else {
			l.logger.Info().
				Str("session", sessionID).
				Int("hookPort", l.hookPort).
				Msg("installed Stop-hook config in worktree")
		}
	}

	// Fire WorktreeCreated → StartingAgent.
	if err := sm.FireCtx(ctx, machine.WorktreeCreated); err != nil {
		return fmt.Errorf("fire worktree_created: %w", err)
	}

	startingState := int(machine.StartingAgent)
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		State: &startingState,
	}); err != nil {
		return fmt.Errorf("set starting_claude state: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("worktree", result.WorktreePath).
		Msg("starting claude")

	// Start Claude. Cron-spawned sessions run in a tmux-hosted Claude UI so
	// the user can attach to the live session, while interactive sessions
	// stay on the headless `claude --print` path used historically.
	var claudeSessionID string
	if opts.CronJobID != "" {
		claudeSessionID, err = l.startCronTmuxChat(ctx, sessionID, opts, session, result)
		if err != nil {
			return fmt.Errorf("start cron tmux chat: %w", err)
		}
	} else {
		claudeSessionID, err = l.agentRunner.StartByAgent(ctx, session.AgentName, result.WorktreePath, session.Plan, nil, "")
		if err != nil {
			return fmt.Errorf("start claude: %w", err)
		}
	}
	if hookResp != nil && !hookResp.GetIsSupported() {
		l.logger.Info().Str("session", sessionID).Msg("agent does not support finalize hook")
	}
	hooklessCronTmux := opts.CronJobID != "" && hookResp != nil && !hookResp.GetIsSupported()

	// Update session with Claude session ID.
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		AgentSessionID: strPtr(claudeSessionID),
	}); err != nil {
		return fmt.Errorf("update claude session id: %w", err)
	}
	if hooklessCronTmux {
		l.armTmuxCompletionForHooklessCron(sessionID, claudeSessionID, session.RepoID)
	}

	// Fire AgentStarted → ImplementingPlan.
	if err := sm.FireCtx(ctx, machine.AgentStarted); err != nil {
		return fmt.Errorf("fire agent_started: %w", err)
	}

	implementingState := int(machine.ImplementingPlan)
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		State: &implementingState,
	}); err != nil {
		return fmt.Errorf("set implementing_plan state: %w", err)
	}

	// For sessions without an existing PR, push the branch and create a
	// draft PR immediately so the user gets a PR right away. This covers
	// both plain "new PR" sessions and tracker-sourced sessions (e.g.
	// Linear tickets) — the latter carry a Plan but still need a PR up
	// front for visibility.
	//
	// Cron-spawned sessions opt out via opts.DeferPR — the Stop-hook
	// finalize path calls EnsurePR once the run actually produces commits.
	if session.PRNumber == nil && !opts.DeferPR {
		if err := l.createDraftPR(ctx, sessionID, result.WorktreePath, result.BranchName, session, repo); err != nil {
			l.setDraftPRBlockedReason(ctx, sessionID, err)
			l.logger.Warn().Err(err).
				Str("session", sessionID).
				Str("branch", result.BranchName).
				Msg("draft PR creation failed during session start; branch is preserved and retryable")
		}
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("claudeSession", claudeSessionID).
		Msg("session started, implementing plan")

	return nil
}

// StartQuickChatSession starts a Claude process directly in the repo's base
// directory. No worktree, branch, or PR is created.
func (l *Lifecycle) StartQuickChatSession(ctx context.Context, sessionID string) error {
	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		return fmt.Errorf("get repo: %w", err)
	}

	// Set WorktreePath to repo's base directory (no worktree created).
	worktreePath := repo.LocalPath
	emptyBranch := ""
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		WorktreePath: &worktreePath,
		BranchName:   &emptyBranch,
	}); err != nil {
		return fmt.Errorf("update worktree path: %w", err)
	}

	// Skip CreatingWorktree, go straight to StartingAgent.
	startingState := int(machine.StartingAgent)
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		State: &startingState,
	}); err != nil {
		return fmt.Errorf("set starting_claude state: %w", err)
	}

	// Quick chat has no plan — Claude starts on-demand when user attaches.
	// Transition directly to ImplementingPlan so the session is ready.
	implementingState := int(machine.ImplementingPlan)
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		State: &implementingState,
	}); err != nil {
		return fmt.Errorf("set implementing_plan state: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Msg("quick chat session started (Claude on-demand)")

	return nil
}

// SubmitPR transitions the session from ImplementingPlan through to
// AwaitingChecks. If the PR was already created (draft-PR-up-front sessions),
// it pushes any pending commits and goes directly to AwaitingChecks. Otherwise
// it pushes the branch, creates a draft PR, and transitions through
// PushingBranch → OpeningDraftPR → AwaitingChecks.
func (l *Lifecycle) SubmitPR(ctx context.Context, sessionID string) error {
	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		return fmt.Errorf("get repo: %w", err)
	}

	// Ensure origin URL is available before any VCS operations.
	if _, err := l.resolveOriginURL(ctx, repo); err != nil {
		return fmt.Errorf("resolve origin URL: %w", err)
	}

	hasPR := session.PRNumber != nil

	// Initialize state machine at the session's current state.
	sm := machine.NewWithContext(session.State, &machine.SessionContext{
		AttemptCount: session.AttemptCount,
		MaxAttempts:  machine.MaxAttempts,
		HasPR:        hasPR,
	})

	// Fire PlanComplete.
	// If HasPR: → AwaitingChecks (PR already exists).
	// Otherwise: → PushingBranch (need to push and create PR).
	if err := sm.FireCtx(ctx, machine.PlanComplete); err != nil {
		return fmt.Errorf("fire plan_complete: %w", err)
	}

	if hasPR {
		// PR already exists — skip PR creation, but still push so that any
		// commits made since createDraftPR (e.g. Claude's implementation
		// commits on top of the empty placeholder commit) reach the remote.
		if err := l.worktrees.Push(ctx, session.WorktreePath, session.BranchName); err != nil {
			return fmt.Errorf("push branch: %w", err)
		}

		awaitingState := int(machine.AwaitingChecks)
		if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
			State: &awaitingState,
		}); err != nil {
			return fmt.Errorf("set awaiting_checks state: %w", err)
		}

		l.logger.Info().
			Str("session", sessionID).
			Msg("plan complete, PR exists, awaiting checks")

		return nil
	}

	// No PR yet — push branch and create draft PR.
	pushingState := int(machine.PushingBranch)
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		State: &pushingState,
	}); err != nil {
		return fmt.Errorf("set pushing_branch state: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("branch", session.BranchName).
		Msg("pushing branch")

	// Push the branch to remote.
	if err := l.worktrees.Push(ctx, session.WorktreePath, session.BranchName); err != nil {
		return fmt.Errorf("push branch: %w", err)
	}

	// Fire BranchPushed → OpeningDraftPR.
	if err := sm.FireCtx(ctx, machine.BranchPushed); err != nil {
		return fmt.Errorf("fire branch_pushed: %w", err)
	}

	openingState := int(machine.OpeningDraftPR)
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		State: &openingState,
	}); err != nil {
		return fmt.Errorf("set opening_draft_pr state: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Msg("creating draft PR")

	if err := l.openDraftPRForBranch(ctx, sessionID, session, repo); err != nil {
		l.logDraftPRBranchDebugSnapshot(ctx, sessionID, session.WorktreePath, session.BranchName, session.BaseBranch, err)
		return err
	}

	// Fire PROpened → AwaitingChecks.
	if err := sm.FireCtx(ctx, machine.PROpened); err != nil {
		return fmt.Errorf("fire pr_opened: %w", err)
	}

	awaitingState := int(machine.AwaitingChecks)
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		State: &awaitingState,
	}); err != nil {
		return fmt.Errorf("set awaiting_checks state: %w", err)
	}

	prNumber := 0
	if session.PRNumber != nil {
		prNumber = *session.PRNumber
	}
	prURL := ""
	if session.PRURL != nil {
		prURL = *session.PRURL
	}
	l.logger.Info().
		Str("session", sessionID).
		Int("prNumber", prNumber).
		Str("prURL", prURL).
		Msg("draft PR created, awaiting checks")

	return nil
}

// createDraftPR pushes the branch and creates a draft PR on GitHub,
// storing the PR number and URL on the session. Used during StartSession
// to create the PR immediately for any session without an existing one.
func draftPRBlockedReason(err error) string {
	return sessionreason.DraftPRCreationFailure(err)
}

func isDraftPRBlockedReason(reason *string) bool {
	return sessionreason.IsDraftPRCreationFailure(reason)
}

func (l *Lifecycle) setDraftPRBlockedReason(ctx context.Context, sessionID string, err error) {
	if err == nil {
		return
	}
	reason := draftPRBlockedReason(err)
	reasonPtr := &reason
	if _, updateErr := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		BlockedReason: &reasonPtr,
	}); updateErr != nil {
		l.logger.Warn().Err(updateErr).
			Str("session", sessionID).
			Msg("failed to persist draft PR creation error")
	}
}

func (l *Lifecycle) clearDraftPRBlockedReason(ctx context.Context, sessionID string, session *models.Session) error {
	if !isDraftPRBlockedReason(session.BlockedReason) {
		return nil
	}
	var cleared *string
	updated, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		BlockedReason: &cleared,
	})
	if err != nil {
		return fmt.Errorf("clear draft PR blocked reason: %w", err)
	}
	session.BlockedReason = updated.BlockedReason
	return nil
}

// draftPRPlaceholderCommitSubject is the message of the empty commit used to
// give a branch a diff so GitHub will open a PR for it (session-start draft
// PRs and dirty-only cron finalize both use it). draftPRTitle recognizes it
// and falls back to the session/cron title rather than deriving a user-facing
// PR title from this scaffolding commit.
const draftPRPlaceholderCommitSubject = "chore: [skip ci] create pull request"

func (l *Lifecycle) createDraftPR(ctx context.Context, sessionID, worktreePath, branchName string, session *models.Session, repo *models.Repo) error {
	// Ensure origin URL is available before any VCS operations.
	if _, err := l.resolveOriginURL(ctx, repo); err != nil {
		return fmt.Errorf("resolve origin URL: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("branch", branchName).
		Msg("pushing branch for immediate PR")

	if err := l.worktrees.VerifyCurrentBranch(ctx, worktreePath, branchName); err != nil {
		return fmt.Errorf("verify worktree branch before draft PR: %w", err)
	}

	// Create an empty commit so the branch diverges from base; GitHub
	// rejects PRs with "No commits between" otherwise.
	if err := l.worktrees.EmptyCommit(ctx, worktreePath, draftPRPlaceholderCommitSubject); err != nil {
		return fmt.Errorf("empty commit: %w", err)
	}

	if err := l.worktrees.Push(ctx, worktreePath, branchName); err != nil {
		return fmt.Errorf("push branch: %w", err)
	}

	verification, err := l.worktrees.VerifyPushedBranchAheadOfBase(ctx, worktreePath, branchName, session.BaseBranch)
	if err != nil {
		return fmt.Errorf("verify PR branch before draft PR: %w", err)
	}

	session.WorktreePath = worktreePath
	session.BranchName = branchName
	if err := l.openDraftPRForBranch(ctx, sessionID, session, repo); err != nil {
		if strings.Contains(err.Error(), "No commits between") && verification != nil {
			err = fmt.Errorf(
				"head branch %s has no commits over base origin/%s (base SHA %s, head SHA %s): %w",
				branchName,
				session.BaseBranch,
				verification.BaseSHA,
				verification.HeadSHA,
				err,
			)
		}
		l.logDraftPRBranchDebugSnapshot(ctx, sessionID, worktreePath, branchName, session.BaseBranch, err)
		return err
	}

	return nil
}

// logDraftPRBranchDebugSnapshot logs the worktree's current branch/HEAD/remote
// state alongside a failed draft PR error. The snapshot reflects state at call
// time, which is after the branch has been pushed — so remote_head_sha shows
// the just-pushed HEAD. That's intentional: it confirms whether the PR branch
// actually received the commit when CreateDraftPR still failed.
func (l *Lifecycle) logDraftPRBranchDebugSnapshot(ctx context.Context, sessionID, worktreePath, branchName, baseBranch string, draftPRErr error) {
	if snapshot, snapshotErr := l.worktrees.BranchDebugSnapshot(ctx, worktreePath, branchName, baseBranch); snapshotErr == nil {
		l.logger.Warn().Err(draftPRErr).
			Str("session", sessionID).
			Str("branch", branchName).
			Str("base", baseBranch).
			Str("current_branch", snapshot.CurrentBranch).
			Str("head_sha", snapshot.HeadSHA).
			Str("remote_head_sha", snapshot.RemoteHeadSHA).
			Str("ahead_behind", snapshot.AheadBehind).
			Msg("draft PR creation failed with branch debug snapshot")
	} else {
		l.logger.Warn().Err(snapshotErr).
			Str("session", sessionID).
			Str("branch", branchName).
			Str("draft_pr_error", draftPRErr.Error()).
			Msg("failed to collect branch debug snapshot after draft PR failure")
	}
}

func (l *Lifecycle) openDraftPRForBranch(ctx context.Context, sessionID string, session *models.Session, repo *models.Repo) error {
	title := l.draftPRTitle(ctx, session)
	prInfo, err := l.provider.CreateDraftPR(ctx, vcs.CreatePROpts{
		RepoPath:   repo.OriginURL,
		HeadBranch: session.BranchName,
		BaseBranch: session.BaseBranch,
		Title:      title,
		Body:       session.Plan,
		Draft:      true,
	})
	if err != nil {
		if errors.Is(err, vcs.ErrPRAlreadyExists) {
			if attachErr := l.attachExistingPRForBranch(ctx, sessionID, session, repo); attachErr != nil {
				return fmt.Errorf("attach existing PR after duplicate create: %w", attachErr)
			}
			return nil
		}
		return fmt.Errorf("create draft PR: %w", err)
	}

	prNumber := &prInfo.Number
	prURL := &prInfo.URL
	updated, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		PRNumber: &prNumber,
		PRURL:    &prURL,
	})
	if err != nil {
		return fmt.Errorf("update PR info: %w", err)
	}

	session.PRNumber = updated.PRNumber
	session.PRURL = updated.PRURL
	if err := l.clearDraftPRBlockedReason(ctx, sessionID, session); err != nil {
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Msg("clear draft PR blocked reason failed after PR creation")
	}

	return nil
}

func (l *Lifecycle) draftPRTitle(ctx context.Context, session *models.Session) string {
	if session.CronJobID == nil || *session.CronJobID == "" {
		return session.Title
	}

	fallback := normalizeCronPRTitle(session.Title)
	subject, err := l.worktrees.LatestCommitSubject(ctx, session.WorktreePath)
	if err != nil {
		l.logger.Warn().Err(err).
			Str("session", session.ID).
			Str("worktree", session.WorktreePath).
			Msg("failed to read latest commit subject for cron PR title")
		return fallback
	}
	// A dirty-only cron run gets an empty placeholder commit so a PR can be
	// opened; its subject is scaffolding, not the user's work, so fall back to
	// the cron/session title instead of deriving the PR title from it.
	if trimmed := strings.TrimSpace(subject); trimmed == "" || trimmed == draftPRPlaceholderCommitSubject {
		return fallback
	}
	return normalizeCronPRTitle(subject)
}

func normalizeCronPRTitle(subject string) string {
	title := strings.TrimSpace(subject)
	if title == "" {
		return title
	}

	for {
		next := conventionalCommitPrefixRE.ReplaceAllString(title, "")
		next = prNumberPrefixRE.ReplaceAllString(strings.TrimSpace(next), "")
		next = strings.TrimSpace(next)
		if next == title {
			break
		}
		title = next
	}
	if title == "" {
		return strings.TrimSpace(subject)
	}

	first, size := utf8.DecodeRuneInString(title)
	if first == utf8.RuneError && size == 0 {
		return title
	}
	return string(unicode.ToUpper(first)) + title[size:]
}

func (l *Lifecycle) attachExistingPRForBranch(ctx context.Context, sessionID string, session *models.Session, repo *models.Repo) error {
	found, err := l.attachOpenPRForBranch(ctx, sessionID, session, repo)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("existing PR for branch %q not found", session.BranchName)
	}
	return nil
}

// attachOpenPRForBranch finds an open PR whose head matches one of the
// session's candidate branches and persists its metadata (number, URL, and —
// via attachPRMetadata — the corrected branch and title).
//
// Every caller invokes this only for a session whose PRNumber is still unset
// (the first-time duplicate-create / safety-net paths in lifecycle.go and
// finalize.go). That precondition is what makes the title persist in
// attachPRMetadata safe: the stored title is still the freshly-generated one,
// so it cannot clobber a user-edited title. Do not reuse this as a generic
// re-attach for sessions that already have a PR without revisiting that
// guarantee.
func (l *Lifecycle) attachOpenPRForBranch(ctx context.Context, sessionID string, session *models.Session, repo *models.Repo) (bool, error) {
	prs, err := l.provider.ListOpenPRs(ctx, repo.OriginURL)
	if err != nil {
		return false, fmt.Errorf("list open PRs: %w", err)
	}

	candidates := l.candidateBranches(ctx, session)
	for _, branch := range candidates {
		for _, pr := range prs {
			if pr.State != vcs.PRStateOpen || pr.HeadBranch != branch {
				continue
			}

			prTitle := pr.Title
			if session.CronJobID != nil && *session.CronJobID != "" {
				title := l.draftPRTitle(ctx, session)
				if err := l.provider.UpdatePRTitle(ctx, repo.OriginURL, pr.Number, title); err != nil {
					return false, fmt.Errorf("update attached PR title: %w", err)
				}
				prTitle = title
			}

			if err := l.attachPRMetadata(ctx, sessionID, session, repo, pr.Number, pr.HeadBranch, prTitle); err != nil {
				return false, err
			}
			return true, nil
		}
	}

	return false, nil
}

// candidateBranches returns the branches a session's PR might live on: the
// stored branch, plus the worktree's live branch when it differs and is
// resolvable. Live-resolution failures are swallowed (stored branch only).
func (l *Lifecycle) candidateBranches(ctx context.Context, session *models.Session) []string {
	out := []string{}
	if session.BranchName != "" {
		out = append(out, session.BranchName)
	}
	if l.branchResolver == nil || session.WorktreePath == "" {
		return out
	}
	live, err := l.branchResolver.CurrentBranch(ctx, session.WorktreePath)
	if err != nil {
		l.logger.Debug().Err(err).
			Str("session", session.ID).
			Str("worktree", session.WorktreePath).
			Msg("attach PR: resolve live worktree branch")
		return out
	}
	if live = strings.TrimSpace(live); live != "" && live != session.BranchName {
		out = append(out, live)
	}
	return out
}

func (l *Lifecycle) attachPRMetadata(ctx context.Context, sessionID string, session *models.Session, repo *models.Repo, prNumber int, matchedBranch, prTitle string) error {
	prURL, err := prURLForRepo(repo.OriginURL, prNumber)
	if err != nil {
		return err
	}
	prNumberPtr := &prNumber
	prURLPtr := &prURL
	updateParams := db.UpdateSessionParams{
		PRNumber: &prNumberPtr,
		PRURL:    &prURLPtr,
	}
	if matchedBranch != "" && matchedBranch != session.BranchName {
		branch := matchedBranch
		updateParams.BranchName = &branch
	}
	// Persisting the title is safe because every caller reaches this only when
	// the session has no PR yet (see attachOpenPRForBranch), so the stored
	// title is still the generated one and cannot overwrite a user edit.
	if title := strings.TrimSpace(prTitle); title != "" {
		updateParams.Title = &title
	}
	updated, err := l.sessions.Update(ctx, sessionID, updateParams)
	if err != nil {
		return fmt.Errorf("update PR info: %w", err)
	}

	session.PRNumber = updated.PRNumber
	session.PRURL = updated.PRURL
	if updateParams.BranchName != nil {
		session.BranchName = *updateParams.BranchName
	}
	if updateParams.Title != nil {
		session.Title = *updateParams.Title
	}
	if err := l.clearDraftPRBlockedReason(ctx, sessionID, session); err != nil {
		return err
	}
	return nil
}

func prURLForRepo(originURL string, prNumber int) (string, error) {
	nwo := vcs.GitHubNWO(originURL)
	if nwo == "" && strings.Count(originURL, "/") == 1 && !strings.Contains(originURL, "://") {
		nwo = strings.TrimSuffix(strings.TrimSpace(originURL), ".git")
	}
	if nwo == "" {
		return "", fmt.Errorf("construct PR URL: unsupported origin URL %q", originURL)
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", nwo, prNumber), nil
}

// LinkPR attaches an existing pull request to a session. It is intended as a
// manual repair path for cron sessions whose agent already created a PR before
// the Stop-hook finalize path ran.
//
// It assumes the session row still exists and does not guard against a
// concurrent finalize that deletes a no-changes session: the two are not
// serialized, so a link issued at the exact moment such a finalize deletes the
// row will fail benignly on the row lookup or update rather than corrupt state.
// In practice this path is used precisely because finalize already ran or
// failed, so that window is negligible.
func (l *Lifecycle) LinkPR(ctx context.Context, sessionID, prRef string) (*models.Session, error) {
	prRef = strings.TrimSpace(prRef)
	if sessionID == "" {
		return nil, fmt.Errorf("session ID is required")
	}
	if prRef == "" {
		return nil, fmt.Errorf("PR number or URL is required")
	}

	sess, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	repo, err := l.repos.Get(ctx, sess.RepoID)
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}
	if _, err := l.resolveOriginURL(ctx, repo); err != nil {
		return nil, fmt.Errorf("resolve origin URL: %w", err)
	}

	prNumber, prURL, err := resolvePRReference(repo.OriginURL, prRef)
	if err != nil {
		return nil, err
	}
	if l.provider != nil {
		if _, err := l.provider.GetPRStatus(ctx, repo.OriginURL, prNumber); err != nil {
			return nil, fmt.Errorf("get PR status: %w", err)
		}
	}

	prNumberPtr := &prNumber
	prURLPtr := &prURL
	updateParams := db.UpdateSessionParams{
		PRNumber: &prNumberPtr,
		PRURL:    &prURLPtr,
	}
	if sess.State == machine.Finalizing || sess.State == machine.Blocked {
		awaitingState := int(machine.AwaitingChecks)
		updateParams.State = &awaitingState
	}
	if sess.State == machine.Blocked {
		var cleared *string
		updateParams.BlockedReason = &cleared
	}
	updated, err := l.sessions.Update(ctx, sessionID, updateParams)
	if err != nil {
		return nil, fmt.Errorf("update PR info: %w", err)
	}
	if err := l.clearDraftPRBlockedReason(ctx, sessionID, updated); err != nil {
		return nil, err
	}

	if updated.CronJobID != nil && *updated.CronJobID != "" && l.cronJobs != nil {
		recordedID := updated.ID
		if err := l.cronJobs.UpdateLastRun(ctx, *updated.CronJobID, db.UpdateCronJobLastRunParams{
			SessionID: &recordedID,
			RanAt:     time.Now(),
			Outcome:   models.CronJobOutcomePRCreated,
		}); err != nil {
			return nil, fmt.Errorf("update cron last run: %w", err)
		}
	}

	l.logger.Info().
		Str("session", sessionID).
		Int("pr", prNumber).
		Msg("linked existing PR to session")

	return updated, nil
}

func resolvePRReference(originURL, prRef string) (int, string, error) {
	if prNumber, err := strconv.Atoi(prRef); err == nil {
		if prNumber <= 0 {
			return 0, "", fmt.Errorf("PR number must be positive")
		}
		prURL, err := prURLForRepo(originURL, prNumber)
		if err != nil {
			return 0, "", err
		}
		return prNumber, prURL, nil
	}

	u, err := url.Parse(prRef)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return 0, "", fmt.Errorf("PR reference must be a PR number or URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" {
		return 0, "", fmt.Errorf("PR URL must look like https://github.com/owner/repo/pull/123")
	}
	prNumber, err := strconv.Atoi(parts[3])
	if err != nil || prNumber <= 0 {
		return 0, "", fmt.Errorf("PR URL has invalid number %q", parts[3])
	}

	expectedRepoURL := vcs.NormalizeRepoURL(originURL)
	actualRepoURL := fmt.Sprintf("https://%s/%s/%s", u.Hostname(), parts[0], parts[1])
	if expectedRepoURL == "" || !strings.EqualFold(expectedRepoURL, actualRepoURL) {
		return 0, "", fmt.Errorf("PR URL repo %s/%s does not match session repo", parts[0], parts[1])
	}

	return prNumber, fmt.Sprintf("https://%s/%s/%s/pull/%d", u.Host, parts[0], parts[1], prNumber), nil
}

// EnsurePR pushes the session's branch and creates a draft PR if one does
// not already exist. It is idempotent: if session.PRNumber is already set,
// the call is a no-op. Used by the cron-finalize path (FL4) once the
// session has produced real commits, where DeferPR=true skipped the
// up-front PR creation.
//
// Unlike createDraftPR, EnsurePR does NOT make an empty placeholder commit:
// callers invoke it after Claude has produced its own commits.
func (l *Lifecycle) EnsurePR(ctx context.Context, sessionID string) error {
	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	if session.PRNumber != nil {
		return nil
	}

	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		return fmt.Errorf("get repo: %w", err)
	}

	if _, err := l.resolveOriginURL(ctx, repo); err != nil {
		return fmt.Errorf("resolve origin URL: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("branch", session.BranchName).
		Msg("ensuring PR: pushing branch")

	if err := l.worktrees.Push(ctx, session.WorktreePath, session.BranchName); err != nil {
		return fmt.Errorf("push branch: %w", err)
	}

	if err := l.openDraftPRForBranch(ctx, sessionID, session, repo); err != nil {
		if errors.Is(err, vcs.ErrPRAlreadyExists) {
			if attachErr := l.attachExistingPRForBranch(ctx, sessionID, session, repo); attachErr == nil {
				return nil
			} else {
				l.setDraftPRBlockedReason(ctx, sessionID, attachErr)
				return fmt.Errorf("attach existing PR after duplicate create: %w", attachErr)
			}
		}
		l.logDraftPRBranchDebugSnapshot(ctx, sessionID, session.WorktreePath, session.BranchName, session.BaseBranch, err)
		l.setDraftPRBlockedReason(ctx, sessionID, err)
		return err
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("branch", session.BranchName).
		Msg("ensured PR")

	return nil
}

// StopSession stops the Claude process for a session.
func (l *Lifecycle) StopSession(ctx context.Context, sessionID string) error {
	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	// Stop Claude process if running.
	if session.AgentSessionID != nil && l.agentRunner.IsRunningByAgent(session.AgentName, *session.AgentSessionID) {
		if err := l.agentRunner.StopByAgent(session.AgentName, *session.AgentSessionID); err != nil {
			l.logger.Warn().Err(err).
				Str("session", sessionID).
				Msg("failed to stop claude process")
		}
	}

	// Kill all per-chat tmux sessions.
	l.killAllChatTmuxSessions(ctx, sessionID)

	// Also kill the legacy per-session tmux session if it exists.
	if session.TmuxSessionName != nil {
		l.KillTmuxByName(ctx, sessionID, *session.TmuxSessionName)
	}

	// Update state to Closed.
	closedState := int(machine.Closed)
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		State: &closedState,
	}); err != nil {
		return fmt.Errorf("set closed state: %w", err)
	}

	l.logger.Info().Str("session", sessionID).Msg("session stopped")
	return nil
}

// ArchiveSession stops the Claude process and removes the worktree,
// but keeps the branch alive for later resurrection.
func (l *Lifecycle) ArchiveSession(ctx context.Context, sessionID string) error {
	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	// Stop Claude process if running.
	if session.AgentSessionID != nil && l.agentRunner.IsRunningByAgent(session.AgentName, *session.AgentSessionID) {
		if err := l.agentRunner.StopByAgent(session.AgentName, *session.AgentSessionID); err != nil {
			l.logger.Warn().Err(err).
				Str("session", sessionID).
				Msg("failed to stop claude process")
		}
	}

	// Kill all per-chat tmux sessions.
	l.killAllChatTmuxSessions(ctx, sessionID)

	// Also kill the legacy per-session tmux session if it exists.
	if session.TmuxSessionName != nil {
		l.KillTmuxByName(ctx, sessionID, *session.TmuxSessionName)
	}

	// Archive worktree (removes directory, keeps branch).
	// Skip for quick chat sessions where WorktreePath is the base repo.
	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		return fmt.Errorf("get repo: %w", err)
	}
	if session.WorktreePath != "" && session.WorktreePath != repo.LocalPath {
		if err := l.worktrees.Archive(ctx, session.WorktreePath); err != nil {
			return fmt.Errorf("archive worktree: %w", err)
		}
	}

	// Mark session as archived in DB.
	if err := l.sessions.Archive(ctx, sessionID); err != nil {
		return fmt.Errorf("archive session: %w", err)
	}

	l.logger.Info().Str("session", sessionID).Msg("session archived")
	return nil
}

// ResurrectSession re-creates a worktree from an existing branch and
// starts a new Claude process (with --resume if a previous Claude session exists).
func (l *Lifecycle) ResurrectSession(ctx context.Context, sessionID string) error {
	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	if session.ArchivedAt == nil {
		return fmt.Errorf("session %s is not archived", sessionID)
	}

	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		return fmt.Errorf("get repo: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("branch", session.BranchName).
		Msg("resurrecting session")

	// Resurrect worktree from existing branch.
	// Skip for quick chat sessions where WorktreePath is the base repo.
	if session.WorktreePath != repo.LocalPath {
		if err := l.worktrees.Resurrect(ctx, gitpkg.ResurrectOpts{
			RepoPath:     repo.LocalPath,
			WorktreePath: session.WorktreePath,
			BranchName:   session.BranchName,
			SetupScript:  repo.SetupScript,
		}); err != nil {
			return fmt.Errorf("resurrect worktree: %w", err)
		}
	}

	// Clear archived status.
	if err := l.sessions.Resurrect(ctx, sessionID); err != nil {
		return fmt.Errorf("resurrect session: %w", err)
	}

	// Start Claude process, resuming previous session if available.
	var resume *string
	if session.AgentSessionID != nil {
		resume = session.AgentSessionID
	}

	claudeSessionID, err := l.agentRunner.StartByAgent(ctx, session.AgentName, session.WorktreePath, session.Plan, resume, "")
	if err != nil {
		return fmt.Errorf("start claude: %w", err)
	}

	// Update Claude session ID.
	implementingState := int(machine.ImplementingPlan)
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		AgentSessionID: strPtr(claudeSessionID),
		State:          &implementingState,
	}); err != nil {
		return fmt.Errorf("update session: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("claudeSession", claudeSessionID).
		Msg("session resurrected")

	return nil
}

// resolveOriginURL ensures the repo has a non-empty OriginURL. If it's
// empty (e.g. git remote get-url failed during initial registration), it
// re-detects the URL from the repo's local path and persists it.
func (l *Lifecycle) resolveOriginURL(ctx context.Context, repo *models.Repo) (string, error) {
	if repo.OriginURL != "" {
		return repo.OriginURL, nil
	}

	url, err := l.worktrees.DetectOriginURL(ctx, repo.LocalPath)
	if err != nil {
		return "", fmt.Errorf("detect origin URL: %w", err)
	}
	if url == "" {
		return "", fmt.Errorf("repo %q has no origin remote configured", repo.DisplayName)
	}

	if _, err := l.repos.Update(ctx, repo.ID, db.UpdateRepoParams{
		OriginURL: &url,
	}); err != nil {
		return "", fmt.Errorf("persist origin URL: %w", err)
	}

	l.logger.Info().
		Str("repo", repo.ID).
		Str("originURL", url).
		Msg("re-detected and persisted origin URL")

	repo.OriginURL = url
	return url, nil
}

// killAllChatTmuxSessions kills the tmux session for every chat in the given
// boss session and clears the tmux_session_name on each chat record.
func (l *Lifecycle) killAllChatTmuxSessions(ctx context.Context, sessionID string) {
	if l.tmux == nil {
		return
	}
	chats, err := l.agentChats.ListBySession(ctx, sessionID)
	if err != nil {
		l.logger.Warn().Err(err).Str("session", sessionID).Msg("failed to list chats for tmux cleanup")
		return
	}
	for _, chat := range chats {
		if chat.TmuxSessionName == nil || *chat.TmuxSessionName == "" {
			continue
		}
		if err := l.tmux.KillSession(ctx, *chat.TmuxSessionName); err != nil {
			l.logger.Warn().Err(err).
				Str("session", sessionID).
				Str("agentSessionID", chat.AgentSessionID).
				Str("tmuxSession", *chat.TmuxSessionName).
				Msg("failed to kill chat tmux session during cleanup")
		} else {
			l.logger.Info().
				Str("session", sessionID).
				Str("agentSessionID", chat.AgentSessionID).
				Str("tmuxSession", *chat.TmuxSessionName).
				Msg("killed chat tmux session")
		}
		if err := l.agentChats.UpdateTmuxSessionName(ctx, chat.AgentSessionID, nil); err != nil {
			l.logger.Warn().Err(err).Str("agentSessionID", chat.AgentSessionID).Msg("failed to clear tmux name during cleanup")
		}
	}
}

// KillTmuxByName kills a tmux session by name and clears the
// TmuxSessionName field on the associated boss session record.
func (l *Lifecycle) KillTmuxByName(ctx context.Context, sessionID, tmuxName string) {
	if tmuxName == "" || l.tmux == nil || !l.tmux.Available(ctx) {
		return
	}
	if err := l.tmux.KillSession(ctx, tmuxName); err != nil {
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Str("tmuxSession", tmuxName).
			Msg("failed to kill tmux session during cleanup")
	} else {
		l.logger.Info().
			Str("session", sessionID).
			Str("tmuxSession", tmuxName).
			Msg("tmux session killed")
	}
	var nilName *string
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		TmuxSessionName: &nilName,
	}); err != nil {
		l.logger.Warn().Err(err).Str("session", sessionID).Msg("failed to clear tmux name during cleanup")
	}
}

// IsTmuxSessionAlive reports whether the given tmux session name is still
// running. Returns false when tmux is unavailable or the name is empty.
func (l *Lifecycle) IsTmuxSessionAlive(ctx context.Context, name string) bool {
	if name == "" || l.tmux == nil {
		return false
	}
	return l.tmux.HasSession(ctx, name)
}

// strPtr returns a double pointer to a string (for UpdateSessionParams).
func strPtr(s string) **string {
	p := &s
	return &p
}
