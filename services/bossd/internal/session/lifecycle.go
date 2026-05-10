// Package session provides the SessionLifecycle orchestrator that wires
// together worktree management, Claude process management, and the state
// machine for a complete session lifecycle.
package session

import (
	"context"
	"fmt"
	"io"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/tmux"
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
	Arm(ctx context.Context, agentSessionID string, client agent.AgentRunnerClient)
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

	// pollArmer arms the per-run poll-fallback goroutine for agent plugins
	// whose ConfigureFinalizeHook reports IsSupported=false (e.g. codex).
	// Plugins that own a finalize hook (claude) skip this entirely. Wired
	// post-construction via SetPollArmer; nil means no fallback (the
	// existing claude-only behaviour).
	pollArmer PollArmer

	// daemonCtx is the long-running context controlling the daemon's
	// goroutine fleet — passed to PollArmer.Arm so the poll outlives the
	// per-call RPC ctx that drove StartSession. Wired via SetDaemonCtx
	// during daemon startup. nil means polls won't be armed even when a
	// PollArmer is set; that combination shouldn't occur in production.
	daemonCtx context.Context //nolint:containedctx // intentional: passed to long-lived goroutines spawned by PollArmer

	// agentClientHookSupport caches the per-agent-name IsSupported result
	// from ConfigureFinalizeHook. Populated lazily during Bootstrap (and,
	// later, SetAgents) so the daemon doesn't probe each plugin twice on
	// restart. nil-safe: a missing entry triggers a fresh probe.
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

// Bootstrap re-arms the poll-fallback for hookless agent runs that were
// alive when the daemon last shut down. Call once during daemon startup,
// after SetAgents / SetPollArmer / SetDaemonCtx and before serving begins.
//
// For each agent_chats row whose tmux session is still alive, Bootstrap
// looks up the parent session's HookToken, probes the agent plugin's
// ConfigureFinalizeHook (caching the result per agent name to avoid
// duplicate writes), and — when IsSupported=false — arms the poll
// fallback under l.daemonCtx so the run's eventual completion still
// signals through to WaitChatRun.
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
		supported, ok := l.agentClientHookSupport[chat.AgentName]
		if !ok {
			hookResp, err := client.ConfigureFinalizeHook(ctx, &bossanovav1.ConfigureFinalizeHookRequest{
				WorkDir:        sess.WorktreePath,
				SessionId:      chat.SessionID,
				AgentSessionId: chat.AgentSessionID,
				HookToken:      *sess.HookToken,
				HookPort:       int32(l.hookPort),
			})
			if err != nil {
				l.logger.Warn().Err(err).
					Str("agent_session", chat.AgentSessionID).
					Msg("bootstrap re-arm: ConfigureFinalizeHook probe failed; skipping")
				continue
			}
			supported = hookResp.GetIsSupported()
			if l.agentClientHookSupport == nil {
				l.agentClientHookSupport = map[string]bool{}
			}
			l.agentClientHookSupport[chat.AgentName] = supported
		}
		if supported {
			continue
		}
		if l.pollArmer == nil || l.daemonCtx == nil {
			continue
		}
		l.pollArmer.Arm(l.daemonCtx, chat.AgentSessionID, client)
		l.logger.Info().
			Str("agent_session", chat.AgentSessionID).
			Str("agent", chat.AgentName).
			Msg("bootstrap: re-armed poll fallback for hookless run")
	}
}

// SetPollArmer wires the poll-fallback armer used when an agent's
// ConfigureFinalizeHook reports IsSupported=false. Wired during daemon
// startup with *agent.PollFallback; tests inject a fake. A nil armer is
// valid — sessions whose agents support hooks ignore it, and sessions
// whose agents don't will simply not get re-driven on completion.
func (l *Lifecycle) SetPollArmer(p PollArmer) { l.pollArmer = p }

// SetDaemonCtx records the daemon-scoped context PollArmer.Arm should
// use. Required alongside SetPollArmer for the poll-fallback path to
// activate. Tests that exercise the poll path inject context.Background.
func (l *Lifecycle) SetDaemonCtx(ctx context.Context) { l.daemonCtx = ctx }

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
		sessions:    sessions,
		repos:       repos,
		agentChats:  agentChats,
		cronJobs:    cronJobs,
		worktrees:   worktrees,
		agentRunner: agentRunner,
		tmux:        tmux,
		provider:    provider,
		logger:      logger,
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
	if opts.HookToken != "" {
		if l.hookPort == 0 {
			return fmt.Errorf("hook port not configured: SetHookPort must be called before starting sessions with a HookToken")
		}
		client, err := l.agentClientFor(session)
		if err != nil {
			return fmt.Errorf("agent client not configured: %w; SetAgents must be called before starting sessions with a HookToken", err)
		}
		hookResp, err := client.ConfigureFinalizeHook(ctx, &bossanovav1.ConfigureFinalizeHookRequest{
			WorkDir:   result.WorktreePath,
			SessionId: sessionID,
			HookToken: opts.HookToken,
			HookPort:  int32(l.hookPort),
		})
		if err != nil {
			return fmt.Errorf("configure finalize hook: %w", err)
		}
		if !hookResp.IsSupported {
			// Hookless agent (e.g. codex) — arm the daemon-side poll
			// fallback so we still learn when the run finishes. Plugins
			// that own a finalize hook (claude) skip this path entirely;
			// their Stop hook drives CompleteAgentRun directly.
			l.logger.Info().Str("session", sessionID).Msg("agent does not support finalize hook; arming poll fallback")
			if l.pollArmer != nil && l.daemonCtx != nil {
				// agent_session_id for cron-spawned tmux sessions is minted
				// inside StartTmuxChat below; for the headless agentRunner
				// path the poller would need that id too. The cron path
				// re-arms via StartTmuxChat (see Task D.4), so for the
				// non-cron branch we arm with the boss session_id as the
				// agent_session_id key — matching the historical claude
				// behaviour where the two were the same value.
				l.pollArmer.Arm(l.daemonCtx, sessionID, client)
			}
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

	// Update session with Claude session ID.
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		AgentSessionID: strPtr(claudeSessionID),
	}); err != nil {
		return fmt.Errorf("update claude session id: %w", err)
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
			l.logger.Warn().Err(err).
				Str("session", sessionID).
				Str("branch", result.BranchName).
				Msg("draft PR creation failed during session start; PR will be created on submit")
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

	// Create a draft PR via the VCS provider.
	prInfo, err := l.provider.CreateDraftPR(ctx, vcs.CreatePROpts{
		RepoPath:   repo.OriginURL,
		HeadBranch: session.BranchName,
		BaseBranch: session.BaseBranch,
		Title:      session.Title,
		Body:       session.Plan,
		Draft:      true,
	})
	if err != nil {
		return fmt.Errorf("create draft PR: %w", err)
	}

	// Update session with PR info.
	prNumber := &prInfo.Number
	prURL := &prInfo.URL
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		PRNumber: &prNumber,
		PRURL:    &prURL,
	}); err != nil {
		return fmt.Errorf("update PR info: %w", err)
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

	l.logger.Info().
		Str("session", sessionID).
		Int("prNumber", prInfo.Number).
		Str("prURL", prInfo.URL).
		Msg("draft PR created, awaiting checks")

	return nil
}

// createDraftPR pushes the branch and creates a draft PR on GitHub,
// storing the PR number and URL on the session. Used during StartSession
// to create the PR immediately for any session without an existing one.
func (l *Lifecycle) createDraftPR(ctx context.Context, sessionID, worktreePath, branchName string, session *models.Session, repo *models.Repo) error {
	// Ensure origin URL is available before any VCS operations.
	if _, err := l.resolveOriginURL(ctx, repo); err != nil {
		return fmt.Errorf("resolve origin URL: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("branch", branchName).
		Msg("pushing branch for immediate PR")

	// Create an empty commit so the branch diverges from base — GitHub
	// rejects PRs with "No commits between" otherwise.
	if err := l.worktrees.EmptyCommit(ctx, worktreePath, "chore: [skip ci] create pull request"); err != nil {
		return fmt.Errorf("empty commit: %w", err)
	}

	if err := l.worktrees.Push(ctx, worktreePath, branchName); err != nil {
		return fmt.Errorf("push branch: %w", err)
	}

	prInfo, err := l.provider.CreateDraftPR(ctx, vcs.CreatePROpts{
		RepoPath:   repo.OriginURL,
		HeadBranch: branchName,
		BaseBranch: session.BaseBranch,
		Title:      session.Title,
		Body:       session.Plan,
		Draft:      true,
	})
	if err != nil {
		return fmt.Errorf("create draft PR: %w", err)
	}

	prNumber := &prInfo.Number
	prURL := &prInfo.URL
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		PRNumber: &prNumber,
		PRURL:    &prURL,
	}); err != nil {
		return fmt.Errorf("update PR info: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Int("prNumber", prInfo.Number).
		Str("prURL", prInfo.URL).
		Msg("draft PR created during session setup")

	return nil
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

	prInfo, err := l.provider.CreateDraftPR(ctx, vcs.CreatePROpts{
		RepoPath:   repo.OriginURL,
		HeadBranch: session.BranchName,
		BaseBranch: session.BaseBranch,
		Title:      session.Title,
		Body:       session.Plan,
		Draft:      true,
	})
	if err != nil {
		return fmt.Errorf("create draft PR: %w", err)
	}

	prNumber := &prInfo.Number
	prURL := &prInfo.URL
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		PRNumber: &prNumber,
		PRURL:    &prURL,
	}); err != nil {
		return fmt.Errorf("update PR info: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Int("prNumber", prInfo.Number).
		Str("prURL", prInfo.URL).
		Msg("draft PR ensured")

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
