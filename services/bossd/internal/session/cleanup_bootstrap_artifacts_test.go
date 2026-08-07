package session

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
)

// artifactWorktreeManager records only the two calls this policy makes; the
// embedded mock supplies the rest of the (large) WorktreeManager surface.
type artifactWorktreeManager struct {
	*mockWorktreeManager
	purgedBranches []string
	reapedBranches [][]string
	// purgeErr stands in for a purge that never ran because the shared clone was
	// busy — the one thing the real PurgeWorktree reports.
	purgeErr error
}

func newArtifactWorktreeManager() *artifactWorktreeManager {
	return &artifactWorktreeManager{mockWorktreeManager: &mockWorktreeManager{}}
}

func (m *artifactWorktreeManager) PurgeWorktree(_ context.Context, _, _, _, branch string) error {
	m.purgedBranches = append(m.purgedBranches, branch)
	return m.purgeErr
}

func (m *artifactWorktreeManager) ReapLocalBranches(_ context.Context, _ string, branches []string) error {
	m.reapedBranches = append(m.reapedBranches, append([]string(nil), branches...))
	return nil
}

// TestCleanUpBootstrapArtifactsAppliesTheOwnershipRule pins the single policy
// both create paths share: the leftover worktree DIRECTORY is always removed, so
// it cannot wedge the next attempt, but the branch is force-deleted only when
// the row proves this attempt made the worktree.
func TestCleanUpBootstrapArtifactsAppliesTheOwnershipRule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		worktreePath string
		wantReaps    int
	}{
		{
			// branch_name comes from the request, before Create ever looked at
			// whether that branch already existed — deleting on it alone would
			// trash a pre-existing branch and its unpushed commits.
			name:         "no worktree recorded: purge only",
			worktreePath: "",
			wantReaps:    0,
		},
		{
			name:         "worktree recorded: purge then reap",
			worktreePath: "/worktrees/repo/owned-branch",
			wantReaps:    1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sessions := newMockSessionStore()
			sessions.sessions["sess-1"] = &models.Session{
				ID:           "sess-1",
				RepoID:       "repo-1",
				BranchName:   "owned-branch",
				WorktreePath: tc.worktreePath,
			}
			repos := newMockRepoStore()
			repos.repos["repo-1"] = &models.Repo{ID: "repo-1", LocalPath: "/repo", DisplayName: "repo"}
			worktrees := newArtifactWorktreeManager()

			CleanUpBootstrapArtifacts(context.Background(), zerolog.Nop(), sessions, repos, worktrees, "sess-1")

			if got := len(worktrees.purgedBranches); got != 1 {
				t.Fatalf("PurgeWorktree calls = %d, want 1", got)
			}
			if got := worktrees.purgedBranches[0]; got != "owned-branch" {
				t.Fatalf("PurgeWorktree branch = %q, want %q", got, "owned-branch")
			}
			if got := len(worktrees.reapedBranches); got != tc.wantReaps {
				t.Fatalf("ReapLocalBranches calls = %d, want %d", got, tc.wantReaps)
			}
		})
	}
}

// TestCleanUpBootstrapArtifactsSkipsTheReapWhenThePurgeDidNotRun pins the other
// half of the purge-before-reap ordering: the half that only exists once the
// purge can decline to run at all.
//
// PurgeWorktree skips itself when it cannot serialize against a busy clone. The
// worktree is then still REGISTERED, and `git branch -D` refuses a branch
// checked out in a registered worktree — so reaping anyway does not delete the
// branch, it produces a generic delete failure in the log and leaks the branch
// behind it. Skipping instead at least keeps the log honest about what is gone.
//
// This is the case the ordering test at the git layer cannot reach and the rest
// of the mock-based cleanup tests structurally cannot see, because their
// ReapLocalBranches returns nil however registered the worktree is.
//
// The log assertion is part of the contract, not decoration. Both callers delete
// the session row the moment this returns, and a surviving branch pushes the
// next same-titled create onto a `<branch>-2` path, so nothing comes back for
// either half — the skipped cleanup is ABANDONED, not deferred, and the line has
// to name what an operator now has to reclaim. An earlier revision pinned the
// word "deferring", which was the one thing about the behaviour that was untrue.
//
// What it names is NOT constant, which is the second half of the contract. With
// no recorded worktree path this cleanup was never entitled to the branch — the
// name came from the request and may be a pre-existing branch carrying unpushed
// work, which the ownership rule exists to protect. Telling an operator to
// reclaim THAT branch would talk them into the one delete the rule prevents.
func TestCleanUpBootstrapArtifactsSkipsTheReapWhenThePurgeDidNotRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		worktreePath string
		// wantOwned is whether the branch is ours, and so whether the log may
		// invite an operator to reclaim it.
		wantOwned bool
	}{
		{
			// A recorded worktree path: this row would otherwise be reaped, so a
			// suppressed reap here can only come from the refused purge.
			name:         "owned branch: named as abandoned",
			worktreePath: "/worktrees/repo/owned-branch",
			wantOwned:    true,
		},
		{
			name:         "no worktree recorded: the branch is left alone, and said to be",
			worktreePath: "",
			wantOwned:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sessions := newMockSessionStore()
			sessions.sessions["sess-1"] = &models.Session{
				ID:           "sess-1",
				RepoID:       "repo-1",
				BranchName:   "owned-branch",
				WorktreePath: tc.worktreePath,
			}
			repos := newMockRepoStore()
			repos.repos["repo-1"] = &models.Repo{ID: "repo-1", LocalPath: "/repo", DisplayName: "repo"}
			worktrees := newArtifactWorktreeManager()
			worktrees.purgeErr = errors.New("acquire repo-clone lock \"/repo\": context deadline exceeded")

			var logs bytes.Buffer
			CleanUpBootstrapArtifacts(context.Background(), zerolog.New(&logs), sessions, repos, worktrees, "sess-1")
			got := logs.String()

			if n := len(worktrees.reapedBranches); n != 0 {
				t.Fatalf("ReapLocalBranches calls = %d, want 0 after a purge that never ran: `git branch -D` would be refused and the branch would leak", n)
			}
			if strings.Contains(got, "deferring") {
				t.Fatalf("the log promises a deferral, but the row is deleted next and no sweep revisits it; log:\n%s", got)
			}
			if !strings.Contains(got, `"level":"error"`) {
				t.Fatalf("abandoned artifacts were not logged at error level; log:\n%s", got)
			}
			// The branch name appears either way (an operator still has to find
			// the directory, which is derived from it); what must differ is
			// whether the line says the BRANCH was abandoned.
			if !strings.Contains(got, "owned-branch") {
				t.Fatalf("the abandoned artifacts were not named in the log; log:\n%s", got)
			}
			saysBranchAbandoned := strings.Contains(got, "abandoning this session's worktree and branch")
			if saysBranchAbandoned != tc.wantOwned {
				t.Fatalf("log claims the branch was abandoned = %v, want %v; log:\n%s", saysBranchAbandoned, tc.wantOwned, got)
			}
			if !tc.wantOwned && !strings.Contains(got, "The branch is left alone") {
				t.Fatalf("an unowned branch was not reported as deliberately left alone, so an operator would delete it; log:\n%s", got)
			}
		})
	}
}

// TestCleanUpBootstrapArtifactsLogsAFailedSessionRead separates a store read
// that FAILED from the benign "nothing to clean up" cases. They are not the
// same: a failed read means artifacts may be on disk that nobody will come back
// for, and returning silently is how that goes unnoticed.
func TestCleanUpBootstrapArtifactsLogsAFailedSessionRead(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	worktrees := newArtifactWorktreeManager()

	// The mock store errors for an unknown id.
	CleanUpBootstrapArtifacts(context.Background(), zerolog.New(&logs),
		newMockSessionStore(), newMockRepoStore(), worktrees, "sess-missing")

	if !strings.Contains(logs.String(), "get session failed") {
		t.Fatalf("a failed session read logged nothing; log:\n%s", logs.String())
	}
	if got := len(worktrees.purgedBranches); got != 0 {
		t.Fatalf("PurgeWorktree calls = %d, want 0 on an unreadable row", got)
	}
}

// TestCleanUpBootstrapArtifactsIsANoOpWithoutWiring guards the nil-store guard
// the server relies on: it calls this directly (its Lifecycle is optional), so a
// cut-down wiring must degrade to a no-op rather than panic.
func TestCleanUpBootstrapArtifactsIsANoOpWithoutWiring(t *testing.T) {
	t.Parallel()

	CleanUpBootstrapArtifacts(context.Background(), zerolog.Nop(), nil, nil, nil, "sess-1")
	CleanUpBootstrapArtifacts(context.Background(), zerolog.Nop(),
		newMockSessionStore(), newMockRepoStore(), nil, "sess-1")
}
