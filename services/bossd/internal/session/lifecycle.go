// Package session provides the SessionLifecycle orchestrator that wires
// together worktree management, Claude process management, and the state
// machine for a complete session lifecycle.
package session

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/claude"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
)

// Lifecycle orchestrates worktree creation, Claude process management,
// and state machine transitions for coding sessions.
type Lifecycle struct {
	sessions  db.SessionStore
	repos     db.RepoStore
	worktrees gitpkg.WorktreeManager
	claude    claude.ClaudeRunner
	provider  vcs.Provider
	logger    zerolog.Logger
}

// NewLifecycle creates a new session lifecycle orchestrator.
func NewLifecycle(
	sessions db.SessionStore,
	repos db.RepoStore,
	worktrees gitpkg.WorktreeManager,
	claude claude.ClaudeRunner,
	provider vcs.Provider,
	logger zerolog.Logger,
) *Lifecycle {
	return &Lifecycle{
		sessions:  sessions,
		repos:     repos,
		worktrees: worktrees,
		claude:    claude,
		provider:  provider,
		logger:    logger,
	}
}

// StartSession creates a worktree, starts a Claude process, and fires
// state machine events. It updates the session record with the worktree
// path, branch name, and Claude session ID.
//
// If existingBranch is non-empty, the worktree checks out that branch
// instead of creating a new one (used for existing PR sessions).
//
// If forceBranch is true and a branch with the derived name already exists,
// it will be removed before creating the new worktree.
func (l *Lifecycle) StartSession(ctx context.Context, sessionID string, existingBranch string, forceBranch bool) error {
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

	// Update session state to CreatingWorktree.
	creatingState := int(machine.CreatingWorktree)
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		State: &creatingState,
	}); err != nil {
		return fmt.Errorf("set creating_worktree state: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("repo", repo.LocalPath).
		Msg("creating worktree")

	// Create worktree: existing branch (PR) or new branch.
	var result *gitpkg.CreateResult
	if existingBranch != "" {
		result, err = l.worktrees.CreateFromExistingBranch(ctx, gitpkg.CreateFromExistingBranchOpts{
			RepoPath:        repo.LocalPath,
			BranchName:      existingBranch,
			WorktreeBaseDir: repo.WorktreeBaseDir,
			SetupScript:     repo.SetupScript,
		})
	} else {
		result, err = l.worktrees.Create(ctx, gitpkg.CreateOpts{
			RepoPath:        repo.LocalPath,
			BaseBranch:      session.BaseBranch,
			WorktreeBaseDir: repo.WorktreeBaseDir,
			Title:           session.Title,
			SetupScript:     repo.SetupScript,
			Force:           forceBranch,
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

	// Start Claude process.
	claudeSessionID, err := l.claude.Start(ctx, result.WorktreePath, session.Plan, nil)
	if err != nil {
		return fmt.Errorf("start claude: %w", err)
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

	// For new PR sessions (no plan, no existing PR), push the branch and
	// create a draft PR immediately so the user gets a PR right away.
	if session.Plan == "" && session.PRNumber == nil {
		if err := l.createDraftPR(ctx, sessionID, result.WorktreePath, result.BranchName, session, repo); err != nil {
			return fmt.Errorf("create draft PR: %w", err)
		}
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("claudeSession", claudeSessionID).
		Msg("session started, implementing plan")

	return nil
}

// SubmitPR transitions the session from ImplementingPlan through to
// AwaitingChecks. If the PR was already created (no-plan sessions), it skips
// push/create and goes directly to AwaitingChecks. Otherwise it pushes the
// branch, creates a draft PR, and transitions through PushingBranch →
// OpeningDraftPR → AwaitingChecks.
func (l *Lifecycle) SubmitPR(ctx context.Context, sessionID string) error {
	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		return fmt.Errorf("get repo: %w", err)
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
		// PR already exists — skip push/create, go straight to AwaitingChecks.
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
// for no-plan PR sessions to create the PR immediately.
func (l *Lifecycle) createDraftPR(ctx context.Context, sessionID, worktreePath, branchName string, session *models.Session, repo *models.Repo) error {
	l.logger.Info().
		Str("session", sessionID).
		Str("branch", branchName).
		Msg("pushing branch for immediate PR")

	// Create an empty commit so the branch diverges from base — GitHub
	// rejects PRs with "No commits between" otherwise.
	if err := l.worktrees.EmptyCommit(ctx, worktreePath, "chore: initialize session branch"); err != nil {
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

	// Archive worktree (removes directory, keeps branch).
	if session.WorktreePath != "" {
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
	if err := l.worktrees.Resurrect(ctx, gitpkg.ResurrectOpts{
		RepoPath:     repo.LocalPath,
		WorktreePath: session.WorktreePath,
		BranchName:   session.BranchName,
		SetupScript:  repo.SetupScript,
	}); err != nil {
		return fmt.Errorf("resurrect worktree: %w", err)
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

	claudeSessionID, err := l.claude.Start(ctx, session.WorktreePath, session.Plan, resume)
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

// strPtr returns a double pointer to a string (for UpdateSessionParams).
func strPtr(s string) **string {
	p := &s
	return &p
}
