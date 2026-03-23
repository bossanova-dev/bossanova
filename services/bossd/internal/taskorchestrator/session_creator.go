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
)

// CreateSessionOpts holds the parameters for creating a new session
// from a plugin-discovered task.
type CreateSessionOpts struct {
	RepoID     string
	Title      string
	Plan       string
	BaseBranch string
	HeadBranch string // if non-empty, checks out existing branch (e.g. dependabot PR branch)
	PRNumber   *int
	PRURL      *string
}

// SessionStarter abstracts the lifecycle's StartSession method for testability.
type SessionStarter interface {
	StartSession(ctx context.Context, sessionID string, existingBranch string, forceBranch bool) error
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

	if err := c.lifecycle.StartSession(ctx, sess.ID, opts.HeadBranch, false); err != nil {
		return nil, fmt.Errorf("start session %s: %w", sess.ID, err)
	}

	// Re-fetch to get updated fields from StartSession (worktree path, branch, state).
	sess, err = c.sessions.Get(ctx, sess.ID)
	if err != nil {
		return nil, fmt.Errorf("re-fetch session %s: %w", sess.ID, err)
	}

	return sess, nil
}
