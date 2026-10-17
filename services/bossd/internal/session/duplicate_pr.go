package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
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

// ErrRevivalStale reports that a session changed while its revival waited for
// the start-path lock: it was archived, relinked, or left the state the caller
// revives it from.
var ErrRevivalStale = errors.New("session changed while waiting to revive it")

// AcquireRevival takes the repo's start-path lock, waiting at most wait, for
// sess, a Blocked or Orphaned row about to come back to life. Under the lock it
// re-reads the row and keeps the lock only when the fresh row is unarchived,
// still passes eligible, and no other live session holds its PR or branch.
// Creation and link-pr write targets under the same lock, so the fresh row's
// target cannot move while it is held. The caller writes the revival from the
// returned row and only then calls release, so a racing create or link cannot
// claim the target between the check and the write.
func AcquireRevival(ctx context.Context, sessions db.SessionStore, sess *models.Session, wait time.Duration, eligible func(*models.Session) bool) (fresh *models.Session, release func(), err error) {
	release, _, err = acquireStartPathWithin(ctx, sess.RepoID, wait)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrStartPathBusy, err)
	}
	fresh, err = sessions.Get(ctx, sess.ID)
	if err == nil && (fresh.ArchivedAt != nil || !eligible(fresh)) {
		err = ErrRevivalStale
	}
	if err == nil {
		err = EnsureNoOtherActivePROrBranchSession(ctx, sessions, fresh.ID, fresh.RepoID, fresh.PRNumber, fresh.BranchName)
	}
	if err != nil {
		release()
		return nil, nil, err
	}
	return fresh, release, nil
}

// lockRevival is AcquireRevival for a background sweep. It waits only
// RevivalLockTimeout, logs a refusal under who, and leaves the row down for
// the next pass.
func lockRevival(ctx context.Context, sessions db.SessionStore, sess *models.Session, logger zerolog.Logger, who string, eligible func(*models.Session) bool) (fresh *models.Session, release func(), ok bool) {
	fresh, release, err := AcquireRevival(ctx, sessions, sess, RevivalLockTimeout, eligible)
	if err == nil {
		return fresh, release, true
	}
	var msg string
	switch {
	case errors.Is(err, ErrRevivalStale):
		logger.Debug().Str("session", sess.ID).Msg(who + ": session changed while waiting to revive it; nothing to do")
		return nil, nil, false
	case errors.Is(err, ErrStartPathBusy):
		msg = "start-path lock busy; leaving the session down for the next pass"
	case IsDuplicateActiveSessionError(err):
		msg = "not reviving; another live session now holds its PR or branch"
	default:
		msg = "could not check who holds its PR or branch; leaving the session down"
	}
	logger.Warn().Err(err).Str("session", sess.ID).Msg(who + ": " + msg)
	return nil, nil, false
}

// RevivalStartGrace bounds how long a revived row reads as starting. A spawn
// that fails puts the row back down at once; this covers one that never
// returns.
const RevivalStartGrace = 2 * time.Minute

// RevivalStarting reports whether the orphan resume or the parked rotation
// wrote sess back into ImplementingPlan less than RevivalStartGrace ago and its
// replacement run has not landed yet. Both write the state first, release the
// start-path lock, and only then spawn, so until the new run id is committed
// the row names the dead run. A liveness check must read it as starting, or a
// create would take its PR in that window. The mark is the revival write
// itself: one statement sets the state and state_entered_at, and the marker it
// claimed from (the orphan blocked_reason, or rotation_resume_at) stays until
// the commit that writes the new run id clears it.
//
// A claim written before daemonStartedAt belongs to a daemon that has since
// exited, so no spawn is in flight for it and the row is dead. Without that
// bound the startup orphan sweep, which runs once, would skip the row and leave
// it in ImplementingPlan for good. daemonStartedAt is compared at millisecond
// precision, the precision state_entered_at is stored at.
func RevivalStarting(sess *models.Session, daemonStartedAt, now time.Time) bool {
	if sess == nil || sess.State != machine.ImplementingPlan || sess.StateEnteredAt == nil {
		return false
	}
	if sess.StateEnteredAt.Before(daemonStartedAt.Truncate(time.Millisecond)) {
		return false
	}
	if now.Sub(*sess.StateEnteredAt) >= RevivalStartGrace {
		return false
	}
	orphanClaim := sess.BlockedReason != nil && *sess.BlockedReason == OrphanedHeadlessRunReason
	return orphanClaim || sess.RotationResumeAt != nil
}

// RevivalRefusal turns an AcquireRevival error into the error a person asking
// for the revival sees: a rival holder is named, as link-pr names one.
func RevivalRefusal(err error) error {
	const next = "archive that session before resuming this one"
	var pr *DuplicateActivePRSessionError
	if errors.As(err, &pr) {
		return &targetOwnedError{
			msg: fmt.Sprintf("PR #%d is now held by session %s; %s", pr.PRNumber, describeSession(pr.ExistingSessionID, pr.ExistingTitle), next),
			err: err,
		}
	}
	var branch *DuplicateActiveBranchSessionError
	if errors.As(err, &branch) {
		return &targetOwnedError{
			msg: fmt.Sprintf("branch %q is now held by session %s; %s", branch.BranchName, describeSession(branch.ExistingSessionID, branch.ExistingTitle), next),
			err: err,
		}
	}
	return err
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
// Because a Blocked or Orphaned row can still come back to life, every path
// that revives one re-checks the target under the start-path lock first
// (AcquireRevival), and link-pr uses the stricter EnsureNoOtherPROrBranchClaim.
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
