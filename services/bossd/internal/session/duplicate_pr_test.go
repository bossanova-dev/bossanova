package session

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// failingDuplicateStore wraps mockSessionStore so the duplicate-guard list
// queries can be forced to fail.
type failingDuplicateStore struct {
	*mockSessionStore
	listByRepoAndPRErr    error
	listActiveWithRepoErr error
}

func (f *failingDuplicateStore) ListByRepoAndPR(ctx context.Context, repoID string, prNumber int) ([]*db.SessionWithRepo, error) {
	if f.listByRepoAndPRErr != nil {
		return nil, f.listByRepoAndPRErr
	}
	return f.mockSessionStore.ListByRepoAndPR(ctx, repoID, prNumber)
}

func (f *failingDuplicateStore) ListActiveWithRepo(ctx context.Context, repoID string) ([]*db.SessionWithRepo, error) {
	if f.listActiveWithRepoErr != nil {
		return nil, f.listActiveWithRepoErr
	}
	return f.mockSessionStore.ListActiveWithRepo(ctx, repoID)
}

func newDuplicateStore(rows ...*models.Session) *mockSessionStore {
	store := newMockSessionStore()
	for _, row := range rows {
		store.sessions[row.ID] = row
	}
	return store
}

func duplicateTestSession(state machine.State, mutate ...func(*models.Session)) *models.Session {
	s := &models.Session{
		ID:         "sess-existing",
		RepoID:     "repo-1",
		BranchName: "feature/x",
		State:      state,
		PRNumber:   intPtr(7),
	}
	for _, fn := range mutate {
		fn(s)
	}
	return s
}

func TestEnsureNoActivePRSession(t *testing.T) {
	ctx := context.Background()

	t.Run("nil PR number is unrestricted", func(t *testing.T) {
		store := newDuplicateStore(duplicateTestSession(machine.ImplementingPlan))
		if err := EnsureNoActivePRSession(ctx, store, "repo-1", nil); err != nil {
			t.Fatalf("EnsureNoActivePRSession() = %v, want nil", err)
		}
	})

	t.Run("no sessions", func(t *testing.T) {
		if err := EnsureNoActivePRSession(ctx, newDuplicateStore(), "repo-1", intPtr(7)); err != nil {
			t.Fatalf("EnsureNoActivePRSession() = %v, want nil", err)
		}
	})

	t.Run("active session for same repo and PR blocks", func(t *testing.T) {
		store := newDuplicateStore(duplicateTestSession(machine.ImplementingPlan))
		err := EnsureNoActivePRSession(ctx, store, "repo-1", intPtr(7))
		var dup *DuplicateActivePRSessionError
		if !errors.As(err, &dup) {
			t.Fatalf("EnsureNoActivePRSession() = %v, want DuplicateActivePRSessionError", err)
		}
		if dup.ExistingSessionID != "sess-existing" || dup.RepoID != "repo-1" || dup.PRNumber != 7 {
			t.Fatalf("unexpected error fields: %+v", dup)
		}
		want := "active session already exists for repo repo-1 PR #7: sess-existing"
		if dup.Error() != want {
			t.Fatalf("Error() = %q, want %q", dup.Error(), want)
		}
	})

	t.Run("different PR does not block", func(t *testing.T) {
		store := newDuplicateStore(duplicateTestSession(machine.ImplementingPlan))
		if err := EnsureNoActivePRSession(ctx, store, "repo-1", intPtr(8)); err != nil {
			t.Fatalf("EnsureNoActivePRSession() = %v, want nil", err)
		}
	})

	t.Run("archived session does not block", func(t *testing.T) {
		archived := duplicateTestSession(machine.ImplementingPlan, func(s *models.Session) {
			s.ArchivedAt = ptr(time.Unix(1700000000, 0))
		})
		store := newDuplicateStore(archived)
		if err := EnsureNoActivePRSession(ctx, store, "repo-1", intPtr(7)); err != nil {
			t.Fatalf("EnsureNoActivePRSession() = %v, want nil", err)
		}
	})

	t.Run("terminal states do not block", func(t *testing.T) {
		for _, state := range []machine.State{machine.Blocked, machine.Merged, machine.Closed} {
			store := newDuplicateStore(duplicateTestSession(state))
			if err := EnsureNoActivePRSession(ctx, store, "repo-1", intPtr(7)); err != nil {
				t.Fatalf("EnsureNoActivePRSession() with state %v = %v, want nil", state, err)
			}
		}
	})

	t.Run("store error is wrapped and not a duplicate error", func(t *testing.T) {
		store := &failingDuplicateStore{
			mockSessionStore:   newMockSessionStore(),
			listByRepoAndPRErr: errors.New("boom"),
		}
		err := EnsureNoActivePRSession(ctx, store, "repo-1", intPtr(7))
		if err == nil || !errors.Is(err, store.listByRepoAndPRErr) {
			t.Fatalf("EnsureNoActivePRSession() = %v, want wrapped boom", err)
		}
		if IsDuplicateActiveSessionError(err) {
			t.Fatalf("IsDuplicateActiveSessionError(%v) = true, want false", err)
		}
	})
}

func TestEnsureNoActivePRSessionWithLiveness(t *testing.T) {
	ctx := context.Background()
	alive := func(context.Context, string) bool { return true }
	dead := func(context.Context, string) bool { return false }

	tests := []struct {
		name          string
		state         machine.State
		mutate        func(*models.Session)
		isAlive       func(context.Context, string) bool
		wantDuplicate bool
	}{
		{
			name:          "startup state without identifiers is treated as stale",
			state:         machine.CreatingWorktree,
			isAlive:       alive,
			wantDuplicate: false,
		},
		{
			name:  "startup state with agent ID and dead process does not block",
			state: machine.CreatingWorktree,
			mutate: func(s *models.Session) {
				s.AgentSessionID = ptr("agent-1")
			},
			isAlive:       dead,
			wantDuplicate: false,
		},
		{
			name:  "startup state with tmux name and live process blocks",
			state: machine.StartingAgent,
			mutate: func(s *models.Session) {
				s.TmuxSessionName = ptr("bossanova-sess")
			},
			isAlive:       alive,
			wantDuplicate: true,
		},
		{
			name:          "implementing plan needs liveness but not startup identifiers",
			state:         machine.ImplementingPlan,
			isAlive:       alive,
			wantDuplicate: true,
		},
		{
			name:          "implementing plan with dead process does not block",
			state:         machine.ImplementingPlan,
			isAlive:       dead,
			wantDuplicate: false,
		},
		{
			name:          "later in-progress state blocks regardless of liveness",
			state:         machine.AwaitingChecks,
			isAlive:       dead,
			wantDuplicate: true,
		},
		{
			name:          "nil liveness treats startup state without identifiers as active",
			state:         machine.CreatingWorktree,
			isAlive:       nil,
			wantDuplicate: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutate := []func(*models.Session){}
			if tt.mutate != nil {
				mutate = append(mutate, tt.mutate)
			}
			store := newDuplicateStore(duplicateTestSession(tt.state, mutate...))
			err := EnsureNoActivePRSessionWithLiveness(ctx, store, "repo-1", intPtr(7), tt.isAlive)
			gotDuplicate := IsDuplicateActiveSessionError(err)
			if gotDuplicate != tt.wantDuplicate {
				t.Fatalf("EnsureNoActivePRSessionWithLiveness() = %v, want duplicate=%v", err, tt.wantDuplicate)
			}
			if !tt.wantDuplicate && err != nil {
				t.Fatalf("EnsureNoActivePRSessionWithLiveness() = %v, want nil", err)
			}
		})
	}
}

func TestEnsureNoActivePROrBranchSession(t *testing.T) {
	ctx := context.Background()

	t.Run("PR duplicate takes precedence over branch duplicate", func(t *testing.T) {
		store := newDuplicateStore(duplicateTestSession(machine.ImplementingPlan))
		err := EnsureNoActivePROrBranchSession(ctx, store, "repo-1", intPtr(7), "feature/x")
		var dup *DuplicateActivePRSessionError
		if !errors.As(err, &dup) {
			t.Fatalf("EnsureNoActivePROrBranchSession() = %v, want DuplicateActivePRSessionError", err)
		}
	})

	t.Run("active session for same branch blocks", func(t *testing.T) {
		store := newDuplicateStore(duplicateTestSession(machine.ImplementingPlan))
		err := EnsureNoActivePROrBranchSession(ctx, store, "repo-1", nil, "feature/x")
		var dup *DuplicateActiveBranchSessionError
		if !errors.As(err, &dup) {
			t.Fatalf("EnsureNoActivePROrBranchSession() = %v, want DuplicateActiveBranchSessionError", err)
		}
		if dup.ExistingSessionID != "sess-existing" || dup.RepoID != "repo-1" || dup.BranchName != "feature/x" {
			t.Fatalf("unexpected error fields: %+v", dup)
		}
		want := `active session already exists for repo repo-1 branch "feature/x": sess-existing`
		if dup.Error() != want {
			t.Fatalf("Error() = %q, want %q", dup.Error(), want)
		}
	})

	t.Run("head branch is trimmed before comparison", func(t *testing.T) {
		store := newDuplicateStore(duplicateTestSession(machine.ImplementingPlan))
		err := EnsureNoActivePROrBranchSession(ctx, store, "repo-1", nil, "  feature/x  ")
		var dup *DuplicateActiveBranchSessionError
		if !errors.As(err, &dup) {
			t.Fatalf("EnsureNoActivePROrBranchSession() = %v, want DuplicateActiveBranchSessionError", err)
		}
	})

	t.Run("blank branch skips the branch check", func(t *testing.T) {
		store := newDuplicateStore(duplicateTestSession(machine.ImplementingPlan))
		for _, branch := range []string{"", "   "} {
			if err := EnsureNoActivePROrBranchSession(ctx, store, "repo-1", nil, branch); err != nil {
				t.Fatalf("EnsureNoActivePROrBranchSession(%q) = %v, want nil", branch, err)
			}
		}
	})

	t.Run("different branch does not block", func(t *testing.T) {
		store := newDuplicateStore(duplicateTestSession(machine.ImplementingPlan))
		if err := EnsureNoActivePROrBranchSession(ctx, store, "repo-1", nil, "feature/y"); err != nil {
			t.Fatalf("EnsureNoActivePROrBranchSession() = %v, want nil", err)
		}
	})

	t.Run("archived session on same branch does not block", func(t *testing.T) {
		archived := duplicateTestSession(machine.ImplementingPlan, func(s *models.Session) {
			s.ArchivedAt = ptr(time.Unix(1700000000, 0))
		})
		store := newDuplicateStore(archived)
		if err := EnsureNoActivePROrBranchSession(ctx, store, "repo-1", nil, "feature/x"); err != nil {
			t.Fatalf("EnsureNoActivePROrBranchSession() = %v, want nil", err)
		}
	})

	t.Run("terminal session on same branch does not block", func(t *testing.T) {
		store := newDuplicateStore(duplicateTestSession(machine.Merged))
		if err := EnsureNoActivePROrBranchSession(ctx, store, "repo-1", nil, "feature/x"); err != nil {
			t.Fatalf("EnsureNoActivePROrBranchSession() = %v, want nil", err)
		}
	})

	t.Run("store error is wrapped and not a duplicate error", func(t *testing.T) {
		store := &failingDuplicateStore{
			mockSessionStore:      newMockSessionStore(),
			listActiveWithRepoErr: errors.New("boom"),
		}
		err := EnsureNoActivePROrBranchSession(ctx, store, "repo-1", nil, "feature/x")
		if err == nil || !errors.Is(err, store.listActiveWithRepoErr) {
			t.Fatalf("EnsureNoActivePROrBranchSession() = %v, want wrapped boom", err)
		}
		if IsDuplicateActiveSessionError(err) {
			t.Fatalf("IsDuplicateActiveSessionError(%v) = true, want false", err)
		}
	})
}

func TestIsDuplicateActiveSessionError(t *testing.T) {
	prErr := &DuplicateActivePRSessionError{ExistingSessionID: "sess-1", RepoID: "repo-1", PRNumber: 7}
	branchErr := &DuplicateActiveBranchSessionError{ExistingSessionID: "sess-1", RepoID: "repo-1", BranchName: "feature/x"}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "PR duplicate", err: prErr, want: true},
		{name: "branch duplicate", err: branchErr, want: true},
		{name: "wrapped PR duplicate", err: fmt.Errorf("start session: %w", prErr), want: true},
		{name: "wrapped branch duplicate", err: fmt.Errorf("start session: %w", branchErr), want: true},
		{name: "unrelated error", err: errors.New("boom"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsDuplicateActiveSessionError(tt.err); got != tt.want {
				t.Fatalf("IsDuplicateActiveSessionError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestLockStartPath(t *testing.T) {
	unlock := LockStartPath()
	unlock()
	// A second acquire must not deadlock after the first release.
	LockStartPath()()
}
