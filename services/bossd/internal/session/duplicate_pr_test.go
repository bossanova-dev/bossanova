package session

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/domain"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sessionreason"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/rotation"
	"github.com/recurser/bossd/internal/status"
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
		for _, state := range []machine.State{machine.Blocked, machine.Merged, machine.Closed, machine.Orphaned} {
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

// TestLockStartPath is the BOS-717 successor to the original process-global
// LockStartPath test: the lock is now per-repo and acquired through the
// context-aware AcquireStartPath, but the base property is unchanged — a second
// acquire after a release must not deadlock.
func TestLockStartPath(t *testing.T) {
	release, _, err := AcquireStartPath(context.Background(), "repo-1")
	if err != nil {
		t.Fatalf("AcquireStartPath: %v", err)
	}
	release()

	release, _, err = AcquireStartPath(context.Background(), "repo-1")
	if err != nil {
		t.Fatalf("second AcquireStartPath: %v", err)
	}
	release()
}

// TestAcquireStartPathIsPerRepo is the unit-level statement of AC 1: a repo
// whose start lock is held must not block a create for a DIFFERENT repo. Before
// BOS-717 the lock was a single process-global mutex, so this could not hold.
func TestAcquireStartPathIsPerRepo(t *testing.T) {
	releaseA, _, err := AcquireStartPath(context.Background(), "repo-a")
	if err != nil {
		t.Fatalf("acquire repo-a: %v", err)
	}
	defer releaseA()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	releaseB, _, err := AcquireStartPath(ctx, "repo-b")
	if err != nil {
		t.Fatalf("acquire repo-b while repo-a is held: %v, want nil", err)
	}
	releaseB()
}

// TestAcquireStartPathBoundedByContext proves contention surfaces as an error
// rather than an untimed hang (AC 3).
func TestAcquireStartPathBoundedByContext(t *testing.T) {
	release, _, err := AcquireStartPath(context.Background(), "repo-contended")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := AcquireStartPath(ctx, "repo-contended"); err == nil {
		t.Fatal("contended AcquireStartPath error = nil, want a deadline error")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended AcquireStartPath error = %v, want context.DeadlineExceeded", err)
	}
}

// TestAcquireStartPathReleaseFreesTheRepo confirms a released repo lock is
// immediately re-acquirable — the behaviour this layer owns. The ref-counted
// map's don't-grow-without-bound invariant is pinned where it now lives, in
// keyedgate's own TestRegistryReapsEntriesAfterUse.
func TestAcquireStartPathReleaseFreesTheRepo(t *testing.T) {
	release, _, err := AcquireStartPath(context.Background(), "repo-reap")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	again, _, err := AcquireStartPath(ctx, "repo-reap")
	if err != nil {
		t.Fatalf("re-acquire after release: %v, want nil", err)
	}
	again()
}

// TestAcquireTargetStartSerializesTheSameTarget proves the target lock keeps the
// "an overlapping create waits for the first create's outcome" contract that the
// old process-global mutex used to provide, and that it does so per target: a
// different branch, and a different PR, are never made to wait.
func TestAcquireTargetStartSerializesTheSameTarget(t *testing.T) {
	pr42, pr43 := 42, 43

	release, _, err := AcquireTargetStart(context.Background(), "repo-1", "", &pr42)
	if err != nil {
		t.Fatalf("acquire PR 42: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := AcquireTargetStart(ctx, "repo-1", "", &pr42); err == nil {
		t.Fatal("second acquire for the same PR succeeded; want it to wait")
	}

	otherCtx, otherCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer otherCancel()
	releaseOther, _, err := AcquireTargetStart(otherCtx, "repo-1", "", &pr43)
	if err != nil {
		t.Fatalf("acquire PR 43 while PR 42 is held: %v, want nil", err)
	}
	releaseOther()

	releaseBranch, _, err := AcquireTargetStart(otherCtx, "repo-1", "some-branch", nil)
	if err != nil {
		t.Fatalf("acquire branch target while PR 42 is held: %v, want nil", err)
	}
	releaseBranch()

	release()
}

// TestTargetStartKeyPrefersPRNumber pins the key derivation: the PR number is
// the target identity whenever there is one, and the branch is only the key for
// a PR-less create.
//
// The order matters because the two entry points derive the branch differently.
// StreamCreateSession resolves it from a GitHub GetPRStatus call and leaves it
// EMPTY when that call fails, while the task orchestrator takes it from the
// emitted task — so a branch-first key made two creates for the same PR take
// different gates exactly when GitHub was flaky. The PR number is verbatim data
// on both sides, so it cannot diverge that way.
func TestTargetStartKeyPrefersPRNumber(t *testing.T) {
	pr := 7
	if got := targetStartKey("", "b", &pr); got != "" {
		t.Fatalf("empty repo key = %q, want \"\"", got)
	}
	if got := targetStartKey("r", "", nil); got != "" {
		t.Fatalf("no branch and no PR key = %q, want \"\"", got)
	}
	// The regression this ordering fixes: a create that resolved the head branch
	// and one that did not must still claim the SAME target.
	if resolved, unresolved := targetStartKey("r", "b", &pr), targetStartKey("r", "", &pr); resolved != unresolved {
		t.Fatalf("PR key depends on whether the head branch resolved: %q vs %q", resolved, unresolved)
	}
	if branchOnly := targetStartKey("r", "b", nil); branchOnly == targetStartKey("r", "b", &pr) {
		t.Fatalf("branch-only and PR keys collide: %q", branchOnly)
	}
}

// revivalPath is one code path that brings a Blocked or Orphaned sess-1, on
// PR 42 and branch fix/pr-head in repo-1, back to life.
type revivalPath struct {
	name string
	// background marks a sweep, which skips a busy start-path lock rather than
	// waiting the create-sized timeout.
	background bool
	// setup builds the fixture and returns its store and one pass of the path,
	// which reports whether sess-1 came back to life.
	setup func(t *testing.T) (*mockSessionStore, func() bool)
	// onWrite arms fn to run once, just before the path writes the revival.
	onWrite func(store *mockSessionStore, fn func())
}

func onRevivalUpdate(store *mockSessionStore, fn func()) {
	fired := false
	store.beforeUpdateHook = func(id string, params db.UpdateSessionParams) {
		if fired || id != "sess-1" || params.State == nil || machine.State(*params.State) != machine.ImplementingPlan {
			return
		}
		fired = true
		fn()
	}
}

func onRevivalCAS(store *mockSessionStore, fn func()) {
	fired := false
	store.updateStateConditionalHook = func(id string) {
		if fired || id != "sess-1" {
			return
		}
		fired = true
		fn()
	}
}

func newPollerRevival(t *testing.T) (*mockSessionStore, func() bool) {
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", OriginURL: "owner/repo"}
	reason := sessionreason.FixLoopExhausted()
	sessions.sessions["sess-1"] = &models.Session{
		ID: "sess-1", RepoID: "repo-1", PRNumber: intPtr(42), BranchName: "fix/pr-head",
		State: machine.Blocked, BlockedReason: &reason, AttemptCount: machine.MaxAttempts,
	}
	vp := newMockVCSProvider()
	success := vcs.CheckConclusionSuccess
	vp.nextCheckResults = []vcs.CheckResult{{Status: vcs.CheckStatusCompleted, Conclusion: &success}}
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Mergeable: boolPtr(true), HeadSHA: "sha-green"}
	poller := NewDisplayPoller(sessions, repos, vp, status.NewDisplayTracker(), time.Minute, zerolog.Nop())
	return sessions, func() bool {
		if err := poller.RefreshPR(context.Background(), "owner/repo", 42); err != nil {
			t.Fatalf("RefreshPR: %v", err)
		}
		return storedSession(sessions, "sess-1").State == machine.ImplementingPlan
	}
}

func newParkedRotationRevival(t *testing.T) *rotationFixture {
	past := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	f := newParkedRotationFixture(t, past)
	f.decider.outcome = rotation.Outcome{Kind: rotation.OutcomeRotate, NextAccount: &domain.AgentAccount{ID: "acct-next"}}
	f.lc.SetClockForTest(fixedClock(past.Add(time.Minute)))
	parked := f.sessions.sessions[f.sessionID]
	parked.PRNumber, parked.BranchName = intPtr(42), "fix/pr-head"
	return f
}

func revivalPaths() []revivalPath {
	return []revivalPath{
		{name: "display poller stale fix-loop unblock", background: true, onWrite: onRevivalUpdate, setup: newPollerRevival},
		{
			name:       "orphan-resume sweep",
			background: true,
			onWrite:    onRevivalCAS,
			setup: func(t *testing.T) (*mockSessionStore, func() bool) {
				f := newOrphanResumeFixture(t)
				f.lc.SetAgents(map[string]agent.RunnerClient{"claude": newFakeAgent()})
				f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*domain.AgentChat{
					f.sessionID: {{ID: "chat-primary", SessionID: f.sessionID, AgentSessionID: "agent-old"}},
				}}
				orphan := f.sessions.sessions[f.sessionID]
				orphan.PRNumber, orphan.BranchName = intPtr(42), "fix/pr-head"
				return f.sessions, func() bool { return f.lc.ResumeOrphanedHeadlessRuns(context.Background()) == 1 }
			},
		},
		{
			name:       "parked-rotation sweep",
			background: true,
			onWrite:    onRevivalCAS,
			setup: func(t *testing.T) (*mockSessionStore, func() bool) {
				f := newParkedRotationRevival(t)
				return f.sessions, func() bool { return f.lc.SweepParkedRotations(context.Background()) == 1 }
			},
		},
		{
			name:    "manual run resume",
			onWrite: onRevivalUpdate,
			setup: func(t *testing.T) (*mockSessionStore, func() bool) {
				f := newRotationFixture(t)
				row := f.sessions.sessions[f.sessionID]
				row.State, row.PRNumber, row.BranchName = machine.Blocked, intPtr(42), "fix/pr-head"
				f.lc.SetRunResumeStore(&runResumeLedger{})
				f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*domain.AgentChat{
					f.sessionID: {{ID: "original", SessionID: f.sessionID, AgentSessionID: "agent-old"}},
				}}
				return f.sessions, func() bool {
					_, err := f.lc.ResumeSessionRun(context.Background(), f.sessionID)
					return err == nil
				}
			},
		},
	}
}

func storedSession(store *mockSessionStore, id string) models.Session {
	store.mu.Lock()
	defer store.mu.Unlock()
	return *store.sessions[id]
}

// replaceSession swaps in a changed copy of a row, as a concurrent writer to a
// real store would: a snapshot read earlier keeps the old values.
func replaceSession(store *mockSessionStore, id string, change func(*models.Session)) {
	store.mu.Lock()
	defer store.mu.Unlock()
	updated := *store.sessions[id]
	change(&updated)
	store.sessions[id] = &updated
}

func addLiveRival(store *mockSessionStore) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.sessions["rival"] = &models.Session{ID: "rival", RepoID: "repo-1", Title: "Rival", PRNumber: intPtr(42), State: machine.ImplementingPlan}
}

// raceCreate is a session create for PR 42 racing a revival. Like the real
// create path it claims the PR only when it can take the repo's start-path
// lock and no live session holds the PR.
func raceCreate(store *mockSessionStore) bool {
	release, _, err := acquireStartPathWithin(context.Background(), "repo-1", 20*time.Millisecond)
	if err != nil {
		return false
	}
	defer release()
	if EnsureNoActivePROrBranchSession(context.Background(), store, "repo-1", intPtr(42), "") != nil {
		return false
	}
	addLiveRival(store)
	return true
}

// TestRevivalPathsStayDownWhileALiveSessionHoldsTheTarget pins note 1 of the
// #3192 verifier: every path that revives a Blocked or Orphaned session leaves
// it down while another live session holds its PR, and revives it once the
// holder is gone.
func TestRevivalPathsStayDownWhileALiveSessionHoldsTheTarget(t *testing.T) {
	for _, p := range revivalPaths() {
		t.Run(p.name, func(t *testing.T) {
			store, revive := p.setup(t)
			before := storedSession(store, "sess-1")
			addLiveRival(store)

			if revive() {
				t.Fatal("revived sess-1 while a live rival holds PR 42")
			}
			if after := storedSession(store, "sess-1"); after.State != before.State {
				t.Fatalf("state = %v, want %v kept", after.State, before.State)
			}

			store.mu.Lock()
			delete(store.sessions, "rival")
			store.mu.Unlock()
			if !revive() {
				t.Fatal("did not revive sess-1 once the rival was gone")
			}
		})
	}
}

// TestRevivalPathsHoldTheStartPathLockAcrossTheWrite pins note 3: a create
// racing a revival must not claim the PR between the revival's ownership check
// and its write. The create fires at the moment of the write. Had the revival
// already let go of the lock, the create would see a Blocked row and claim the
// PR, and the write would then leave two live sessions on it.
func TestRevivalPathsHoldTheStartPathLockAcrossTheWrite(t *testing.T) {
	for _, p := range revivalPaths() {
		t.Run(p.name, func(t *testing.T) {
			store, revive := p.setup(t)
			raced := false
			p.onWrite(store, func() { raced = raceCreate(store) })

			if !revive() {
				t.Fatal("did not revive sess-1")
			}
			if raced {
				t.Fatal("a create claimed PR 42 during the revival's write; two live sessions now drive it")
			}
			var dup *DuplicateActivePRSessionError
			err := EnsureNoActivePROrBranchSession(context.Background(), store, "repo-1", intPtr(42), "")
			if !errors.As(err, &dup) || dup.ExistingSessionID != "sess-1" {
				t.Fatalf("create after the revival: err = %v, want a refusal naming sess-1", err)
			}
		})
	}
}

// TestBackgroundRevivalsSkipABusyStartPathLock pins note 2: a sweep waits only
// RevivalLockTimeout for the lock, leaves the session down, and revives it on
// a later pass once the lock is free.
func TestBackgroundRevivalsSkipABusyStartPathLock(t *testing.T) {
	prev := RevivalLockTimeout
	RevivalLockTimeout = 20 * time.Millisecond
	t.Cleanup(func() { RevivalLockTimeout = prev })

	for _, p := range revivalPaths() {
		if !p.background {
			continue
		}
		t.Run(p.name, func(t *testing.T) {
			store, revive := p.setup(t)
			before := storedSession(store, "sess-1")
			release, _, err := AcquireStartPath(context.Background(), "repo-1")
			if err != nil {
				t.Fatalf("hold start-path lock: %v", err)
			}

			start := time.Now()
			revived := revive()
			waited := time.Since(start)
			release()
			if revived {
				t.Fatal("revived sess-1 while the start-path lock was held")
			}
			if waited > 2*time.Second {
				t.Fatalf("pass took %v behind a busy lock, want about RevivalLockTimeout", waited)
			}
			if after := storedSession(store, "sess-1"); after.State != before.State {
				t.Fatalf("state = %v, want %v kept", after.State, before.State)
			}
			if !revive() {
				t.Fatal("did not revive sess-1 on the next pass")
			}
		})
	}
}

// TestRevivalActsOnTheRowAsItIsUnderTheLock pins note 4: a revival that read
// its row before waiting for the lock must not write over an archive or a
// relink that landed in the meantime.
func TestRevivalActsOnTheRowAsItIsUnderTheLock(t *testing.T) {
	archive := func(s *models.Session) { now := time.Now(); s.ArchivedAt = &now }
	relink := func(s *models.Session) {
		s.PRNumber, s.BranchName, s.State, s.BlockedReason = intPtr(43), "other/head", machine.AwaitingChecks, nil
	}
	t.Run("poller, archived", func(t *testing.T) {
		store, revive := newPollerRevival(t)
		landAfterFirstRead(store, archive)
		if revive() {
			t.Fatal("unblocked a session archived while the unblock waited for the lock")
		}
		if got := storedSession(store, "sess-1"); got.State != machine.Blocked {
			t.Fatalf("archived row's state = %v, want Blocked kept", got.State)
		}
	})
	t.Run("poller, relinked", func(t *testing.T) {
		store, revive := newPollerRevival(t)
		landAfterFirstRead(store, relink)
		if revive() {
			t.Fatal("unblocked a session relinked while the unblock waited for the lock")
		}
		if got := storedSession(store, "sess-1"); got.State != machine.AwaitingChecks || *got.PRNumber != 43 {
			t.Fatalf("relinked row = %v on PR %d, want AwaitingChecks on PR 43", got.State, *got.PRNumber)
		}
	})
	t.Run("rotation sweep, archived", func(t *testing.T) {
		f := newParkedRotationRevival(t)
		decide := f.decider.Decide
		f.lc.SetRotationDecider(func(ctx context.Context, sig rotation.Signal) (rotation.Outcome, error) {
			replaceSession(f.sessions, f.sessionID, archive)
			return decide(ctx, sig)
		})
		if n := f.lc.SweepParkedRotations(context.Background()); n != 0 {
			t.Fatalf("redispatched = %d, want 0 for a session archived while the sweep decided", n)
		}
		if got := storedSession(f.sessions, f.sessionID); got.State != machine.Blocked {
			t.Fatalf("archived row's state = %v, want Blocked kept", got.State)
		}
		if len(f.runner.started) != 0 {
			t.Fatalf("StartByAgent calls = %d, want 0", len(f.runner.started))
		}
	})
}

// landAfterFirstRead applies change to sess-1 right after the first Get of it
// returns, so that read is a snapshot taken before the change.
func landAfterFirstRead(store *mockSessionStore, change func(*models.Session)) {
	landed := false
	store.afterGetHook = func(id string) {
		if id == "sess-1" && !landed {
			landed = true
			replaceSession(store, id, change)
		}
	}
}
