// Package taskorchestrator coordinates task source plugins with the
// daemon's session lifecycle, routing plugin-discovered tasks to the
// appropriate action (auto-merge, create session, notify user).
package taskorchestrator

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
)

// CreateSessionOpts holds the parameters for creating a new session
// from a plugin-discovered task.
type CreateSessionOpts struct {
	RepoID          string
	Title           string
	Plan            string
	BaseBranch      string
	HeadBranch      string // if non-empty, checks out existing branch (e.g. dependabot PR branch)
	SkipSetupScript bool   // if true, skip running the repo's setup script (e.g. for dependabot PRs)
	PRNumber        *int
	PRURL           *string

	// Cron-session fields. Populated when the scheduler spawns a session.
	// DeferPR and HookToken are persisted through to StartSession; they take
	// effect once the StartSessionOpts refactor lands (flight leg 3).
	CronJobID  string // if non-empty, session was cron-spawned
	DeferPR    bool   // if true, skip draft-PR creation; wait for the Stop-hook finalize path
	HookToken  string // if non-empty, written into settings.local.json for the Stop hook
	BranchName string // if non-empty, overrides the title-derived branch name (cron uses a unique per-fire suffix)
}

// SessionStarter abstracts the lifecycle's StartSession method for testability.
type SessionStarter interface {
	StartSession(ctx context.Context, sessionID string, opts session.StartSessionOpts) error
}

// SessionCreator abstracts session creation so the orchestrator can
// be tested without a real database or lifecycle.
type SessionCreator interface {
	CreateSession(ctx context.Context, opts CreateSessionOpts) (*models.Session, error)
}

// lifecycleSessionCreator implements SessionCreator by creating a
// session record in the DB and starting it via the Lifecycle.
type lifecycleSessionCreator struct {
	sessions  db.SessionStore
	lifecycle SessionStarter
	logger    zerolog.Logger
}

// NewSessionCreator creates a SessionCreator backed by the DB and Lifecycle.
func NewSessionCreator(
	sessions db.SessionStore,
	lifecycle SessionStarter,
	logger zerolog.Logger,
) SessionCreator {
	return &lifecycleSessionCreator{
		sessions:  sessions,
		lifecycle: lifecycle,
		logger:    logger.With().Str("component", "session-creator").Logger(),
	}
}

// CreateSession creates a session record and starts the lifecycle.
// If HeadBranch is set, the lifecycle checks out the existing branch
// (used for dependabot PRs that already have a branch).
func (c *lifecycleSessionCreator) CreateSession(ctx context.Context, opts CreateSessionOpts) (*models.Session, error) {
	sess, err := c.sessions.Create(ctx, db.CreateSessionParams{
		RepoID:     opts.RepoID,
		Title:      opts.Title,
		Plan:       opts.Plan,
		BaseBranch: opts.BaseBranch,
		PRNumber:   opts.PRNumber,
		PRURL:      opts.PRURL,
	})
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	c.logger.Info().
		Str("session", sess.ID).
		Str("repo", opts.RepoID).
		Str("title", opts.Title).
		Msg("created session, starting lifecycle")

	if err := c.lifecycle.StartSession(ctx, sess.ID, session.StartSessionOpts{
		ExistingBranch:  opts.HeadBranch,
		SkipSetupScript: opts.SkipSetupScript,
		CronJobID:       opts.CronJobID,
		DeferPR:         opts.DeferPR,
		HookToken:       opts.HookToken,
		BranchName:      opts.BranchName,
	}); err != nil {
		// StartSession failed mid-flight (e.g. worktree create, hook config
		// write, or claude.Start). Drop the half-started session row so it
		// doesn't surface as a phantom in the home view — empty chat list,
		// no PR, stuck in an early state — that the user can't recover. The
		// cron scheduler still records fire_failed via its own caller.
		if delErr := c.sessions.Delete(ctx, sess.ID); delErr != nil {
			c.logger.Warn().Err(delErr).
				Str("session", sess.ID).
				Msg("clean up half-started session after StartSession failure")
		}
		return nil, fmt.Errorf("start session %s: %w", sess.ID, err)
	}

	// Re-fetch to get updated fields from StartSession (worktree path, branch, state).
	sess, err = c.sessions.Get(ctx, sess.ID)
	if err != nil {
		return nil, fmt.Errorf("re-fetch session %s: %w", sess.ID, err)
	}

	return sess, nil
}
