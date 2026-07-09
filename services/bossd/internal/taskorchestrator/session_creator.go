// Package taskorchestrator coordinates task source plugins with the
// daemon's session lifecycle, routing plugin-discovered tasks to the
// appropriate action (auto-merge, create session, notify user).
package taskorchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
)

// setupLogWriter routes setup-script output to the session logger for the
// non-interactive (task-orchestrator) session-creation path, which has no
// client stream to receive it. Each non-blank line is logged individually.
type setupLogWriter struct {
	logger    zerolog.Logger
	sessionID string
}

func (w setupLogWriter) Write(p []byte) (int, error) {
	for line := range strings.SplitSeq(strings.TrimRight(string(p), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		w.logger.Info().Str("session", w.sessionID).Str("source", "setup_script").Msg(line)
	}
	return len(p), nil
}

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
	AgentName       string // Agent plugin name; empty means use the daemon default agent.
	Model           string // Opaque agent model id; "" = plugin default.

	// PreventDuplicateActiveSession is set for Dependabot repair sessions so
	// one active PR/branch session covers repeated plugin emissions.
	PreventDuplicateActiveSession bool

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

// DefaultAccountResolver picks the default rotation account for a provider at
// session-creation time so task-created (cron/dependabot) sessions honor the
// same default-account policy as the interactive StreamCreateSession path.
// Satisfied by *account.Resolver; kept as a narrow local interface so
// taskorchestrator stays testable and does not depend on the account package.
type DefaultAccountResolver interface {
	DefaultAccountID(ctx context.Context, provider string, now time.Time) (string, error)
}

// lifecycleSessionCreator implements SessionCreator by creating a
// session record in the DB and starting it via the Lifecycle.
type lifecycleSessionCreator struct {
	sessions             db.SessionStore
	lifecycle            SessionStarter
	defaultAgentProvider func() string
	duplicateLiveness    SessionLivenessChecker
	// onSessionDeleted, when non-nil, is invoked after a half-started
	// session row is cleaned up so the deletion propagates upstream.
	// Without it the row lingers as a phantom in the web read model until
	// the daemon reconnects and re-snapshots.
	onSessionDeleted func(context.Context, string)
	// defaultAccount, when non-nil, applies the creation-time default-account
	// policy so task-created sessions bind to the same default account the
	// interactive path would pick. A resolver error never fails creation — the
	// session is created unbound (account 0).
	defaultAccount DefaultAccountResolver
	logger         zerolog.Logger
}

// NewSessionCreator creates a SessionCreator backed by the DB and Lifecycle.
func NewSessionCreator(
	sessions db.SessionStore,
	lifecycle SessionStarter,
	defaultAgent string,
	logger zerolog.Logger,
) SessionCreator {
	return NewSessionCreatorWithDefaultAgentProvider(sessions, lifecycle, func() string {
		return defaultAgent
	}, logger)
}

// NewSessionCreatorWithDefaultAgentProvider creates a SessionCreator that
// resolves the default agent at session creation time.
func NewSessionCreatorWithDefaultAgentProvider(
	sessions db.SessionStore,
	lifecycle SessionStarter,
	defaultAgentProvider func() string,
	logger zerolog.Logger,
) SessionCreator {
	return NewSessionCreatorWithDefaultAgentProviderAndLiveness(sessions, lifecycle, defaultAgentProvider, nil, logger)
}

// NewSessionCreatorWithDefaultAgentProviderAndLiveness creates a SessionCreator
// that can ignore stale early-state sessions during duplicate detection.
func NewSessionCreatorWithDefaultAgentProviderAndLiveness(
	sessions db.SessionStore,
	lifecycle SessionStarter,
	defaultAgentProvider func() string,
	duplicateLiveness SessionLivenessChecker,
	logger zerolog.Logger,
) SessionCreator {
	return NewSessionCreatorWithNotifier(sessions, lifecycle, defaultAgentProvider, duplicateLiveness, nil, logger)
}

// NewSessionCreatorWithNotifier is like
// NewSessionCreatorWithDefaultAgentProviderAndLiveness but also takes an
// onSessionDeleted callback invoked when a half-started session is cleaned
// up, so the deletion propagates to the orchestrator read model instead of
// lingering as a phantom row until the next daemon reconnect. A nil callback
// is allowed (no-op).
func NewSessionCreatorWithNotifier(
	sessions db.SessionStore,
	lifecycle SessionStarter,
	defaultAgentProvider func() string,
	duplicateLiveness SessionLivenessChecker,
	onSessionDeleted func(context.Context, string),
	logger zerolog.Logger,
) SessionCreator {
	return NewSessionCreatorWithAccountResolver(sessions, lifecycle, defaultAgentProvider, duplicateLiveness, onSessionDeleted, nil, logger)
}

// NewSessionCreatorWithAccountResolver is like NewSessionCreatorWithNotifier
// but also takes a DefaultAccountResolver so task-created sessions apply the
// creation-time default-account policy. A nil resolver preserves the prior
// (unbound) behavior.
func NewSessionCreatorWithAccountResolver(
	sessions db.SessionStore,
	lifecycle SessionStarter,
	defaultAgentProvider func() string,
	duplicateLiveness SessionLivenessChecker,
	onSessionDeleted func(context.Context, string),
	defaultAccount DefaultAccountResolver,
	logger zerolog.Logger,
) SessionCreator {
	return &lifecycleSessionCreator{
		sessions:             sessions,
		lifecycle:            lifecycle,
		defaultAgentProvider: defaultAgentProvider,
		duplicateLiveness:    duplicateLiveness,
		onSessionDeleted:     onSessionDeleted,
		defaultAccount:       defaultAccount,
		logger:               logger.With().Str("component", "session-creator").Logger(),
	}
}

func (c *lifecycleSessionCreator) resolveAgentName(requested string) string {
	if requested != "" {
		return requested
	}
	if c.defaultAgentProvider != nil {
		if defaultAgent := c.defaultAgentProvider(); defaultAgent != "" {
			return defaultAgent
		}
	}
	return "claude"
}

// CreateSession creates a session record and starts the lifecycle.
// If HeadBranch is set, the lifecycle checks out the existing branch
// (used for dependabot PRs that already have a branch).
func (c *lifecycleSessionCreator) CreateSession(ctx context.Context, opts CreateSessionOpts) (*models.Session, error) {
	var sess *models.Session
	agentName := c.resolveAgentName(opts.AgentName)
	unlockStart := session.LockStartPath()
	startLocked := true
	defer func() {
		if startLocked {
			unlockStart()
		}
	}()
	var err error
	branchName := opts.BranchName
	if branchName == "" {
		branchName = opts.HeadBranch
	}
	if opts.PreventDuplicateActiveSession {
		err = session.EnsureNoActivePROrBranchSessionWithLiveness(ctx, c.sessions, opts.RepoID, opts.PRNumber, opts.HeadBranch, c.isDuplicateSessionAlive)
	}
	if err == nil {
		params := db.CreateSessionParams{
			RepoID:     opts.RepoID,
			Title:      opts.Title,
			Plan:       opts.Plan,
			BaseBranch: opts.BaseBranch,
			PRNumber:   opts.PRNumber,
			PRURL:      opts.PRURL,
			AgentName:  agentName,
			Model:      opts.Model,
			BranchName: branchName,
		}
		params.AccountID = c.resolveDefaultAccountID(ctx, agentName)
		sess, err = c.sessions.Create(ctx, params)
	}
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
		BranchName:      branchName,
		// The interactive RPC path streams setup output to the client; this
		// non-interactive path has no client, so route it to the session log
		// rather than discarding it (a failing setup script would otherwise
		// leave only an opaque exit code).
		SetupOutput: setupLogWriter{logger: c.logger, sessionID: sess.ID},
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
		} else if c.onSessionDeleted != nil {
			// Publish the deletion so the orchestrator read model drops the
			// row immediately rather than leaving a phantom until reconnect.
			c.onSessionDeleted(ctx, sess.ID)
		}
		return nil, fmt.Errorf("start session %s: %w", sess.ID, err)
	}
	unlockStart()
	startLocked = false

	// Re-fetch to get updated fields from StartSession (worktree path, branch, state).
	sess, err = c.sessions.Get(ctx, sess.ID)
	if err != nil {
		return nil, fmt.Errorf("re-fetch session %s: %w", sess.ID, err)
	}

	return sess, nil
}

// resolveDefaultAccountID applies the creation-time default-account policy for
// task-created sessions, returning the bound account id or nil (account 0 /
// system default). A resolver error NEVER fails session creation — it logs and
// returns nil, mirroring the interactive server's resolveSessionAccount
// behavior. A nil resolver leaves sessions unbound.
func (c *lifecycleSessionCreator) resolveDefaultAccountID(ctx context.Context, agentName string) *string {
	if c.defaultAccount == nil {
		return nil
	}
	id, err := c.defaultAccount.DefaultAccountID(ctx, agentName, time.Now())
	if err != nil {
		c.logger.Warn().Err(err).Str("agent", agentName).
			Msg("account: default-account policy failed; creating session unbound")
		return nil
	}
	if id == "" {
		return nil
	}
	return &id
}

func (c *lifecycleSessionCreator) isDuplicateSessionAlive(ctx context.Context, sessionID string) bool {
	if c.duplicateLiveness == nil {
		return true
	}
	return c.duplicateLiveness.IsSessionAlive(ctx, sessionID)
}
