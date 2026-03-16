// Package session provides the SessionLifecycle orchestrator that wires
// together worktree management, Claude process management, and the state
// machine for a complete session lifecycle.
package session

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
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
	logger    zerolog.Logger
}

// NewLifecycle creates a new session lifecycle orchestrator.
func NewLifecycle(
	sessions db.SessionStore,
	repos db.RepoStore,
	worktrees gitpkg.WorktreeManager,
	claude claude.ClaudeRunner,
	logger zerolog.Logger,
) *Lifecycle {
	return &Lifecycle{
		sessions:  sessions,
		repos:     repos,
		worktrees: worktrees,
		claude:    claude,
		logger:    logger,
	}
}

// StartSession creates a worktree, starts a Claude process, and fires
// state machine events. It updates the session record with the worktree
// path, branch name, and Claude session ID.
func (l *Lifecycle) StartSession(ctx context.Context, sessionID string) error {
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

	// Create worktree.
	result, err := l.worktrees.Create(ctx, gitpkg.CreateOpts{
		RepoPath:        repo.LocalPath,
		BaseBranch:      session.BaseBranch,
		WorktreeBaseDir: repo.WorktreeBaseDir,
		Title:           session.Title,
		SetupScript:     repo.SetupScript,
	})
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

	l.logger.Info().
		Str("session", sessionID).
		Str("claudeSession", claudeSessionID).
		Msg("session started, implementing plan")

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
