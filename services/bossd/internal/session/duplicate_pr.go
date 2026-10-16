package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// The per-repo start-path lock this file's dedup checks run under lives in
// start_lock.go (AcquireStartPath).

// DuplicateActivePRSessionError reports an existing non-archived,
// non-terminal session for the same repo and PR.
type DuplicateActivePRSessionError struct {
	ExistingSessionID string
	ExistingTitle     string
	RepoID            string
	PRNumber          int
}

func (e *DuplicateActivePRSessionError) Error() string {
	return fmt.Sprintf("active session already exists for repo %s PR #%d: %s", e.RepoID, e.PRNumber, e.ExistingSessionID)
}

// DuplicateActiveBranchSessionError reports an existing non-archived,
// non-terminal session for the same repo and branch.
type DuplicateActiveBranchSessionError struct {
	ExistingSessionID string
	ExistingTitle     string
	RepoID            string
	BranchName        string
}

func (e *DuplicateActiveBranchSessionError) Error() string {
	return fmt.Sprintf("active session already exists for repo %s branch %q: %s", e.RepoID, e.BranchName, e.ExistingSessionID)
}

// IsDuplicateActiveSessionError reports whether err is one of the duplicate
// active-session errors produced by this package.
func IsDuplicateActiveSessionError(err error) bool {
	var duplicatePR *DuplicateActivePRSessionError
	if errors.As(err, &duplicatePR) {
		return true
	}
	var duplicateBranch *DuplicateActiveBranchSessionError
	return errors.As(err, &duplicateBranch)
}

// EnsureNoActivePRSession returns a typed error when repoID already has a
// non-archived, non-terminal session for prNumber. Nil prNumber sessions are
// unrestricted.
func EnsureNoActivePRSession(ctx context.Context, sessions db.SessionStore, repoID string, prNumber *int) error {
	return EnsureNoActivePRSessionWithLiveness(ctx, sessions, repoID, prNumber, nil)
}

// EnsureNoActivePRSessionWithLiveness returns a typed error when repoID already
// has a non-archived, non-terminal session for prNumber. For early in-progress
// states, liveness can distinguish a running session from a stale row left
// behind by task recovery.
func EnsureNoActivePRSessionWithLiveness(ctx context.Context, sessions db.SessionStore, repoID string, prNumber *int, isAlive func(context.Context, string) bool) error {
	return EnsureNoActivePROrBranchSessionWithLiveness(ctx, sessions, repoID, prNumber, "", isAlive)
}

// EnsureNoActivePROrBranchSession returns a typed error when repoID already
// has a non-archived, non-terminal session for prNumber or headBranch.
func EnsureNoActivePROrBranchSession(ctx context.Context, sessions db.SessionStore, repoID string, prNumber *int, headBranch string) error {
	return EnsureNoActivePROrBranchSessionWithLiveness(ctx, sessions, repoID, prNumber, headBranch, nil)
}

// EnsureNoActivePROrBranchSessionWithLiveness returns a typed error when repoID
// already has a non-archived, non-terminal session for prNumber or headBranch.
// The optional liveness check is used only for early in-progress states.
func EnsureNoActivePROrBranchSessionWithLiveness(ctx context.Context, sessions db.SessionStore, repoID string, prNumber *int, headBranch string, isAlive func(context.Context, string) bool) error {
	return ensureNoTargetOwner(ctx, sessions, repoID, prNumber, headBranch, func(row *db.SessionWithRepo) bool {
		return duplicateSessionIsActive(ctx, row, isAlive)
	})
}

// EnsureNoOtherActivePROrBranchSession is EnsureNoActivePROrBranchSession for a
// session that already exists: its own row, selfID, never conflicts with it.
func EnsureNoOtherActivePROrBranchSession(ctx context.Context, sessions db.SessionStore, selfID, repoID string, prNumber *int, headBranch string) error {
	return ensureNoTargetOwner(ctx, sessions, repoID, prNumber, headBranch, func(row *db.SessionWithRepo) bool {
		return row.ID != selfID && duplicateSessionIsActive(ctx, row, nil)
	})
}

// EnsureNoOtherPROrBranchClaim is the stricter check link-pr runs before
// moving selfID onto a PR or branch. A Blocked or Orphaned row that is not
// archived also owns its target here: the fix-loop auto-unblock and the orphan
// resume bring such a row back to life without asking, so linking past it
// would leave two live sessions on one target. Only archiving, a merge or a
// close releases it.
func EnsureNoOtherPROrBranchClaim(ctx context.Context, sessions db.SessionStore, selfID, repoID string, prNumber *int, headBranch string) error {
	return ensureNoTargetOwner(ctx, sessions, repoID, prNumber, headBranch, func(row *db.SessionWithRepo) bool {
		return row.ID != selfID && row.ArchivedAt == nil &&
			row.State != machine.Merged && row.State != machine.Closed
	})
}

func ensureNoTargetOwner(ctx context.Context, sessions db.SessionStore, repoID string, prNumber *int, headBranch string, owns func(*db.SessionWithRepo) bool) error {
	rival := func(row *db.SessionWithRepo) bool {
		return row != nil && row.Session != nil && owns(row)
	}
	if prNumber != nil {
		existing, err := sessions.ListByRepoAndPR(ctx, repoID, *prNumber)
		if err != nil {
			return fmt.Errorf("list sessions by repo and PR: %w", err)
		}
		for _, row := range existing {
			if !rival(row) {
				continue
			}
			return &DuplicateActivePRSessionError{
				ExistingSessionID: row.ID,
				ExistingTitle:     row.Title,
				RepoID:            repoID,
				PRNumber:          *prNumber,
			}
		}
	}
	branch := strings.TrimSpace(headBranch)
	if branch == "" {
		return nil
	}
	existing, err := sessions.ListActiveWithRepo(ctx, repoID)
	if err != nil {
		return fmt.Errorf("list active sessions by repo: %w", err)
	}
	for _, row := range existing {
		if !rival(row) || row.BranchName != branch {
			continue
		}
		return &DuplicateActiveBranchSessionError{
			ExistingSessionID: row.ID,
			ExistingTitle:     row.Title,
			RepoID:            repoID,
			BranchName:        branch,
		}
	}
	return nil
}

// lockRevival takes the repo's start-path lock for sess, a Blocked or Orphaned
// row about to come back to life, and keeps it only when no other live session
// now holds the row's PR or branch. The caller writes the revival before
// calling release, so a racing create or link cannot claim the target in
// between, the same check-then-write session creation does. A refusal is
// logged under who and leaves the row down for the next pass.
func lockRevival(ctx context.Context, sessions db.SessionStore, sess *models.Session, logger zerolog.Logger, who string) (release func(), ok bool) {
	release, _, err := AcquireStartPath(ctx, sess.RepoID)
	if err != nil {
		logger.Warn().Err(err).Str("session", sess.ID).Msg(who + ": start-path lock busy; leaving the session down for the next pass")
		return nil, false
	}
	err = EnsureNoOtherActivePROrBranchSession(ctx, sessions, sess.ID, sess.RepoID, sess.PRNumber, sess.BranchName)
	if err == nil {
		return release, true
	}
	release()
	if IsDuplicateActiveSessionError(err) {
		logger.Warn().Err(err).Str("session", sess.ID).Msg(who + ": not reviving; another live session now holds its PR or branch")
	} else {
		logger.Warn().Err(err).Str("session", sess.ID).Msg(who + ": could not check who holds its PR or branch; leaving the session down")
	}
	return nil, false
}

func duplicateSessionIsActive(ctx context.Context, row *db.SessionWithRepo, isAlive func(context.Context, string) bool) bool {
	if row == nil || row.Session == nil || row.ArchivedAt != nil {
		return false
	}
	if !StateBlocksDuplicateTarget(row.State) {
		return false
	}
	if isAlive != nil && duplicateSessionStateNeedsStartupIdentifier(row) {
		return false
	}
	if isAlive != nil && duplicateSessionStateNeedsLiveness(row.State) {
		return isAlive(ctx, row.ID)
	}
	return true
}

// StateBlocksDuplicateTarget reports whether a session state should reserve a
// PR or branch target from another session. Dead states release their target so
// a fresh session can reuse it — Orphaned belongs with Blocked/Merged/Closed
// here: a headless run killed by a daemon restart is over, and the plan lets a
// human re-dispatch the same ticket, which must not be refused as a duplicate.
// Because a Blocked or Orphaned row can still come back to life, its revival
// paths re-check the target first (lockRevival), and link-pr uses the
// stricter EnsureNoOtherPROrBranchClaim.
func StateBlocksDuplicateTarget(state machine.State) bool {
	return state != machine.Blocked && state != machine.Merged &&
		state != machine.Closed && state != machine.Orphaned
}

// IsPlanningSessionPlan reports whether plan starts with the /boss-plan slash
// command after leading whitespace. It deliberately does not infer intent from
// prose or looser command lookalikes.
func IsPlanningSessionPlan(plan string) bool {
	const command = "/boss-plan"

	trimmed := strings.TrimLeftFunc(plan, unicode.IsSpace)
	if !strings.HasPrefix(trimmed, command) {
		return false
	}

	rest := trimmed[len(command):]
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return unicode.IsSpace(r)
}

func duplicateSessionStateNeedsLiveness(state machine.State) bool {
	return state == machine.CreatingWorktree || state == machine.StartingAgent || state == machine.ImplementingPlan
}

func duplicateSessionStateNeedsStartupIdentifier(row *db.SessionWithRepo) bool {
	if row.State != machine.CreatingWorktree && row.State != machine.StartingAgent {
		return false
	}
	hasAgentID := row.AgentSessionID != nil && *row.AgentSessionID != ""
	hasTmuxName := row.TmuxSessionName != nil && *row.TmuxSessionName != ""
	return !hasAgentID && !hasTmuxName
}
