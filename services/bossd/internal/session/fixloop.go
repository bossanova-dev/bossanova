package session

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/claude"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
)

// FixLoop handles the automated fix cycle: when a session enters FixingChecks,
// it fetches failure details, resumes Claude, pushes the fix, and transitions
// back to AwaitingChecks. It uses a per-session mutex to prevent concurrent
// fix attempts on the same session.
type FixLoop struct {
	sessions  db.SessionStore
	attempts  db.AttemptStore
	repos     db.RepoStore
	provider  vcs.Provider
	claude    claude.ClaudeRunner
	worktrees gitpkg.WorktreeManager
	logger    zerolog.Logger

	mu    sync.Mutex
	locks map[string]*sync.Mutex // per-session mutex
}

// NewFixLoop creates a new fix loop handler.
func NewFixLoop(
	sessions db.SessionStore,
	attempts db.AttemptStore,
	repos db.RepoStore,
	provider vcs.Provider,
	claude claude.ClaudeRunner,
	worktrees gitpkg.WorktreeManager,
	logger zerolog.Logger,
) *FixLoop {
	return &FixLoop{
		sessions:  sessions,
		attempts:  attempts,
		repos:     repos,
		provider:  provider,
		claude:    claude,
		worktrees: worktrees,
		logger:    logger,
		locks:     make(map[string]*sync.Mutex),
	}
}

// sessionMutex returns the per-session mutex, creating it if needed.
func (f *FixLoop) sessionMutex(sessionID string) *sync.Mutex {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.locks[sessionID]
	if !ok {
		m = &sync.Mutex{}
		f.locks[sessionID] = m
	}
	return m
}

// HandleCheckFailure processes a check failure event by fetching failed check
// logs, resuming Claude with fix instructions, pushing the fix, and
// transitioning back to AwaitingChecks.
func (f *FixLoop) HandleCheckFailure(ctx context.Context, sessionID string, failedChecks []vcs.CheckResult) error {
	mu := f.sessionMutex(sessionID)
	mu.Lock()
	defer mu.Unlock()

	sess, err := f.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	if sess.State != machine.FixingChecks {
		return fmt.Errorf("session %s is in state %s, expected fixing_checks", sessionID, sess.State)
	}

	repo, err := f.repos.Get(ctx, sess.RepoID)
	if err != nil {
		return fmt.Errorf("get repo: %w", err)
	}

	// Record the attempt.
	attempt, err := f.attempts.Create(ctx, db.CreateAttemptParams{
		SessionID: sessionID,
		Trigger:   int(models.AttemptTriggerCheckFailure),
	})
	if err != nil {
		return fmt.Errorf("create attempt: %w", err)
	}

	// Fetch failed check logs.
	var logSummaries []string
	for _, check := range failedChecks {
		logs, err := f.provider.GetFailedCheckLogs(ctx, repo.OriginURL, check.ID)
		if err != nil {
			f.logger.Warn().Err(err).
				Str("check", check.Name).
				Msg("failed to get check logs")
			logSummaries = append(logSummaries, fmt.Sprintf("- %s: (logs unavailable)", check.Name))
			continue
		}
		// Truncate logs to avoid overwhelming Claude.
		if len(logs) > 4000 {
			logs = logs[len(logs)-4000:]
		}
		logSummaries = append(logSummaries, fmt.Sprintf("- %s:\n```\n%s\n```", check.Name, logs))
	}

	plan := fmt.Sprintf("CI checks failed. Please fix the issues and ensure all checks pass.\n\nFailed checks:\n%s", strings.Join(logSummaries, "\n\n"))

	return f.runFixAttempt(ctx, sess, repo, attempt, plan)
}

// HandleConflict processes a merge conflict event by resuming Claude with
// conflict resolution instructions.
func (f *FixLoop) HandleConflict(ctx context.Context, sessionID string) error {
	mu := f.sessionMutex(sessionID)
	mu.Lock()
	defer mu.Unlock()

	sess, err := f.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	if sess.State != machine.FixingChecks {
		return fmt.Errorf("session %s is in state %s, expected fixing_checks", sessionID, sess.State)
	}

	repo, err := f.repos.Get(ctx, sess.RepoID)
	if err != nil {
		return fmt.Errorf("get repo: %w", err)
	}

	attempt, err := f.attempts.Create(ctx, db.CreateAttemptParams{
		SessionID: sessionID,
		Trigger:   int(models.AttemptTriggerConflict),
	})
	if err != nil {
		return fmt.Errorf("create attempt: %w", err)
	}

	plan := fmt.Sprintf("Merge conflict detected with base branch %q. Please rebase onto the latest base branch and resolve any conflicts.", sess.BaseBranch)

	return f.runFixAttempt(ctx, sess, repo, attempt, plan)
}

// HandleReviewFeedback processes review feedback by resuming Claude with
// the review comments.
func (f *FixLoop) HandleReviewFeedback(ctx context.Context, sessionID string, comments []vcs.ReviewComment) error {
	mu := f.sessionMutex(sessionID)
	mu.Lock()
	defer mu.Unlock()

	sess, err := f.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	if sess.State != machine.FixingChecks {
		return fmt.Errorf("session %s is in state %s, expected fixing_checks", sessionID, sess.State)
	}

	repo, err := f.repos.Get(ctx, sess.RepoID)
	if err != nil {
		return fmt.Errorf("get repo: %w", err)
	}

	attempt, err := f.attempts.Create(ctx, db.CreateAttemptParams{
		SessionID: sessionID,
		Trigger:   int(models.AttemptTriggerReviewFeedback),
	})
	if err != nil {
		return fmt.Errorf("create attempt: %w", err)
	}

	// Format review comments.
	var commentSummaries []string
	for _, c := range comments {
		summary := fmt.Sprintf("- @%s (%v): %s", c.Author, c.State, c.Body)
		if c.Path != nil {
			summary += fmt.Sprintf(" (file: %s", *c.Path)
			if c.Line != nil {
				summary += fmt.Sprintf(", line: %d", *c.Line)
			}
			summary += ")"
		}
		commentSummaries = append(commentSummaries, summary)
	}

	plan := fmt.Sprintf("Code review feedback received. Please address the following comments:\n\n%s", strings.Join(commentSummaries, "\n"))

	return f.runFixAttempt(ctx, sess, repo, attempt, plan)
}

// runFixAttempt is the common fix attempt logic: resume Claude, wait for
// completion, push the branch, and fire FixComplete.
func (f *FixLoop) runFixAttempt(ctx context.Context, sess *models.Session, repo *models.Repo, attempt *models.Attempt, plan string) error {
	f.logger.Info().
		Str("session", sess.ID).
		Str("attempt", attempt.ID).
		Str("trigger", attempt.Trigger.String()).
		Int("attemptCount", sess.AttemptCount).
		Msg("starting fix attempt")

	// Resume Claude with the fix instructions.
	var resume *string
	if sess.ClaudeSessionID != nil {
		resume = sess.ClaudeSessionID
	}

	claudeSessionID, err := f.claude.Start(ctx, sess.WorktreePath, plan, resume)
	if err != nil {
		f.recordAttemptFailed(ctx, attempt.ID, fmt.Sprintf("start claude: %v", err))
		return f.fireFixFailed(ctx, sess, fmt.Errorf("start claude: %w", err))
	}

	// Update session with new Claude session ID.
	if _, err := f.sessions.Update(ctx, sess.ID, db.UpdateSessionParams{
		ClaudeSessionID: strPtr(claudeSessionID),
	}); err != nil {
		f.recordAttemptFailed(ctx, attempt.ID, fmt.Sprintf("update claude session: %v", err))
		return fmt.Errorf("update claude session: %w", err)
	}

	// Wait for Claude to finish.
	f.waitForClaude(ctx, claudeSessionID)

	// Push the branch.
	if err := f.worktrees.Push(ctx, sess.WorktreePath, sess.BranchName); err != nil {
		f.recordAttemptFailed(ctx, attempt.ID, fmt.Sprintf("push branch: %v", err))
		return f.fireFixFailed(ctx, sess, fmt.Errorf("push branch: %w", err))
	}

	// Record attempt success.
	result := int(models.AttemptResultSuccess)
	if _, err := f.attempts.Update(ctx, attempt.ID, db.UpdateAttemptParams{
		Result: &result,
	}); err != nil {
		f.logger.Warn().Err(err).Str("attempt", attempt.ID).Msg("failed to update attempt result")
	}

	// Fire FixComplete → AwaitingChecks.
	return f.fireFixComplete(ctx, sess)
}

// waitForClaude blocks until the Claude process exits or context is cancelled.
func (f *FixLoop) waitForClaude(ctx context.Context, claudeSessionID string) {
	// Poll IsRunning until the process exits.
	// Subscribe and drain the channel — it closes when the process exits.
	ch, err := f.claude.Subscribe(ctx, claudeSessionID)
	if err != nil {
		f.logger.Warn().Err(err).Str("claude", claudeSessionID).Msg("could not subscribe to claude output")
		return
	}
	for range ch {
		// Drain output until channel closes (process exits).
	}
}

// fireFixComplete fires FixComplete on the state machine and updates the DB.
func (f *FixLoop) fireFixComplete(ctx context.Context, sess *models.Session) error {
	// Re-fetch session to get latest state.
	sess, err := f.sessions.Get(ctx, sess.ID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	sm := machine.NewWithContext(sess.State, &machine.SessionContext{
		AttemptCount: sess.AttemptCount,
		MaxAttempts:  machine.MaxAttempts,
	})

	if err := sm.FireCtx(ctx, machine.FixComplete); err != nil {
		return fmt.Errorf("fire fix_complete: %w", err)
	}

	newState := int(sm.State())
	if _, err := f.sessions.Update(ctx, sess.ID, db.UpdateSessionParams{
		State: &newState,
	}); err != nil {
		return fmt.Errorf("update session state: %w", err)
	}

	f.logger.Info().
		Str("session", sess.ID).
		Str("newState", sm.State().String()).
		Msg("fix complete, re-entering awaiting checks")

	return nil
}

// fireFixFailed fires FixFailed on the state machine and updates the DB.
func (f *FixLoop) fireFixFailed(ctx context.Context, sess *models.Session, reason error) error {
	// Re-fetch session to get latest state.
	sess, err := f.sessions.Get(ctx, sess.ID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	sm := machine.NewWithContext(sess.State, &machine.SessionContext{
		AttemptCount: sess.AttemptCount,
		MaxAttempts:  machine.MaxAttempts,
	})

	if err := sm.FireCtx(ctx, machine.FixFailed); err != nil {
		return fmt.Errorf("fire fix_failed: %w", err)
	}

	newState := int(sm.State())
	attemptCount := sm.Context().AttemptCount
	update := db.UpdateSessionParams{
		State:        &newState,
		AttemptCount: &attemptCount,
	}

	if sm.State() == machine.Blocked {
		blockedReason := sm.Context().BlockedReason
		reasonPtr := &blockedReason
		update.BlockedReason = &reasonPtr
	}

	if _, err := f.sessions.Update(ctx, sess.ID, update); err != nil {
		return fmt.Errorf("update session: %w", err)
	}

	f.logger.Info().
		Str("session", sess.ID).
		Str("newState", sm.State().String()).
		Err(reason).
		Msg("fix failed")

	return nil
}

// recordAttemptFailed records a failed attempt result.
func (f *FixLoop) recordAttemptFailed(ctx context.Context, attemptID string, errMsg string) {
	result := int(models.AttemptResultFailed)
	errPtr := &errMsg
	if _, err := f.attempts.Update(ctx, attemptID, db.UpdateAttemptParams{
		Result: &result,
		Error:  &errPtr,
	}); err != nil {
		f.logger.Warn().Err(err).Str("attempt", attemptID).Msg("failed to record attempt failure")
	}
}
