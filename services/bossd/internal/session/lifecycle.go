// Package session provides the SessionLifecycle orchestrator that wires
// together worktree management, Claude process management, and the state
// machine for a complete session lifecycle.
package session

import (
	"context"
	"fmt"
	"io"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/claude"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/tmux"
)

// Lifecycle orchestrates worktree creation, Claude process management,
// and state machine transitions for coding sessions.
type Lifecycle struct {
	sessions    db.SessionStore
	repos       db.RepoStore
	claudeChats db.ClaudeChatStore
	cronJobs    db.CronJobStore
	worktrees   gitpkg.WorktreeManager
	claude      claude.ClaudeRunner
	tmux        *tmux.Client
	provider    vcs.Provider
	logger      zerolog.Logger

	// hookPort is the loopback TCP port of the daemon's Stop-hook server.
	// Stamped via SetHookPort once the hook server has bound, before any
	// session that needs a HookToken is started. Zero means "not yet set"
	// and StartSession will error out rather than write a config that
	// points at no listener.
	hookPort int
}

// SetHookPort records the hook server's bound loopback port so
// StartSession can stamp it into a worktree's settings.local.json when
// installing the Stop-hook config. Called from the daemon entrypoint
// after hookSrv.Listen() succeeds.
func (l *Lifecycle) SetHookPort(port int) {
	l.hookPort = port
}

// NewLifecycle creates a new session lifecycle orchestrator. cronJobs may be
// nil for callers that never spawn cron-linked sessions (tests, legacy flows);
// FinalizeSession requires it and will error if it's absent.
func NewLifecycle(
	sessions db.SessionStore,
	repos db.RepoStore,
	claudeChats db.ClaudeChatStore,
	cronJobs db.CronJobStore,
	worktrees gitpkg.WorktreeManager,
	claude claude.ClaudeRunner,
	tmux *tmux.Client,
	provider vcs.Provider,
	logger zerolog.Logger,
) *Lifecycle {
	return &Lifecycle{
		sessions:    sessions,
		repos:       repos,
		claudeChats: claudeChats,
		cronJobs:    cronJobs,
		worktrees:   worktrees,
		claude:      claude,
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
		if err := claude.WriteHookConfig(result.WorktreePath, sessionID, opts.HookToken, l.hookPort); err != nil {
			return fmt.Errorf("write hook config: %w", err)
		}
		l.logger.Info().
			Str("session", sessionID).
			Int("hookPort", l.hookPort).
			Msg("installed Stop-hook config in worktree")
	}

	// Fire WorktreeCreated → StartingClaude.
	if err := sm.FireCtx(ctx, machine.WorktreeCreated); err != nil {
		return fmt.Errorf("fire worktree_created: %w", err)
	}

	startingState := int(machine.StartingClaude)
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
		claudeSessionID, err = l.claude.Start(ctx, result.WorktreePath, session.Plan, nil, "")
		if err != nil {
			return fmt.Errorf("start claude: %w", err)
		}
	}

	// Update session with Claude session ID.
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		ClaudeSessionID: strPtr(claudeSessionID),
	}); err != nil {
		return fmt.Errorf("update claude session id: %w", err)
	}

	// Fire ClaudeStarted → ImplementingPlan.
	if err := sm.FireCtx(ctx, machine.ClaudeStarted); err != nil {
		return fmt.Errorf("fire claude_started: %w", err)
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

	// Skip CreatingWorktree, go straight to StartingClaude.
	startingState := int(machine.StartingClaude)
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
	if session.ClaudeSessionID != nil && l.claude.IsRunning(*session.ClaudeSessionID) {
		if err := l.claude.Stop(*session.ClaudeSessionID); err != nil {
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
	if session.ClaudeSessionID != nil && l.claude.IsRunning(*session.ClaudeSessionID) {
		if err := l.claude.Stop(*session.ClaudeSessionID); err != nil {
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
	if session.ClaudeSessionID != nil {
		resume = session.ClaudeSessionID
	}

	claudeSessionID, err := l.claude.Start(ctx, session.WorktreePath, session.Plan, resume, "")
	if err != nil {
		return fmt.Errorf("start claude: %w", err)
	}

	// Update Claude session ID.
	implementingState := int(machine.ImplementingPlan)
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		ClaudeSessionID: strPtr(claudeSessionID),
		State:           &implementingState,
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
	chats, err := l.claudeChats.ListBySession(ctx, sessionID)
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
				Str("claudeID", chat.ClaudeID).
				Str("tmuxSession", *chat.TmuxSessionName).
				Msg("failed to kill chat tmux session during cleanup")
		} else {
			l.logger.Info().
				Str("session", sessionID).
				Str("claudeID", chat.ClaudeID).
				Str("tmuxSession", *chat.TmuxSessionName).
				Msg("killed chat tmux session")
		}
		if err := l.claudeChats.UpdateTmuxSessionName(ctx, chat.ClaudeID, nil); err != nil {
			l.logger.Warn().Err(err).Str("claudeID", chat.ClaudeID).Msg("failed to clear tmux name during cleanup")
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
