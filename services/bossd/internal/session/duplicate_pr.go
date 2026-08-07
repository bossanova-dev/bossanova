package session

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossd/internal/db"
)

// The per-repo start-path lock this file's dedup checks run under lives in
// start_lock.go (AcquireStartPath).

// DuplicateActivePRSessionError reports an existing non-archived,
// non-terminal session for the same repo and PR.
type DuplicateActivePRSessionError struct {
	ExistingSessionID string
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
	if prNumber == nil {
		return nil
	}
	existing, err := sessions.ListByRepoAndPR(ctx, repoID, *prNumber)
	if err != nil {
		return fmt.Errorf("list sessions by repo and PR: %w", err)
	}
	for _, row := range existing {
		if !duplicateSessionIsActive(ctx, row, isAlive) {
			continue
		}
		return &DuplicateActivePRSessionError{
			ExistingSessionID: row.ID,
			RepoID:            repoID,
			PRNumber:          *prNumber,
		}
	}
	return nil
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
	if err := EnsureNoActivePRSessionWithLiveness(ctx, sessions, repoID, prNumber, isAlive); err != nil {
		return err
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
		if !duplicateSessionIsActive(ctx, row, isAlive) {
			continue
		}
		if row.BranchName != branch {
			continue
		}
		return &DuplicateActiveBranchSessionError{
			ExistingSessionID: row.ID,
			RepoID:            repoID,
			BranchName:        branch,
		}
	}
	return nil
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
func StateBlocksDuplicateTarget(state machine.State) bool {
	return state != machine.Blocked && state != machine.Merged &&
		state != machine.Closed && state != machine.Orphaned
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
