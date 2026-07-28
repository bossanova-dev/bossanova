package db

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/recurser/bossalib/machine"
)

// spyRecomputer counts Recompute invocations and records the session IDs.
type spyRecomputer struct {
	calls atomic.Int32
}

func (s *spyRecomputer) Recompute(_ context.Context, _ string) error {
	s.calls.Add(1)
	return nil
}

// TestRecomputingSessionStore_TriggersOnClaudeSessionIDOnlyUpdate verifies
// that writing only ClaudeSessionID (a composite input) through the
// decorator triggers Recompute. Before the fix, the State-only allow-list
// guard caused these writes to be silently skipped.
func TestRecomputingSessionStore_TriggersOnClaudeSessionIDOnlyUpdate(t *testing.T) {
	database := setupTestDB(t)
	repos := NewRepoStore(database)
	repo, err := repos.Create(context.Background(), CreateRepoParams{
		DisplayName:       "test-repo",
		LocalPath:         "/tmp/test-recompute-claude-id",
		OriginURL:         "https://github.com/test/repo.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}

	inner := NewSessionStore(database)
	sess, err := inner.Create(context.Background(), CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "t",
		WorktreePath: "/tmp/wt-claude-id",
		BranchName:   "br",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	spy := &spyRecomputer{}
	store := NewRecomputingSessionStore(inner, spy)

	claude := "claude-abc"
	pClaude := &claude
	if _, err := store.Update(context.Background(), sess.ID, UpdateSessionParams{
		AgentSessionID: &pClaude,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	if got := spy.calls.Load(); got != 1 {
		t.Errorf("Recompute calls = %d, want 1 (ClaudeSessionID is a composite input)", got)
	}
}

// TestRecomputingSessionStore_SkipsOnDisplayTrioOnlyUpdate verifies that the
// computer's own write-back (display-trio only) does NOT re-trigger
// Recompute, preventing recursion / write storms.
func TestRecomputingSessionStore_SkipsOnDisplayTrioOnlyUpdate(t *testing.T) {
	database := setupTestDB(t)
	repos := NewRepoStore(database)
	repo, err := repos.Create(context.Background(), CreateRepoParams{
		DisplayName:       "test-repo",
		LocalPath:         "/tmp/test-recompute-trio",
		OriginURL:         "https://github.com/test/repo.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}

	inner := NewSessionStore(database)
	sess, err := inner.Create(context.Background(), CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "t",
		WorktreePath: "/tmp/wt-trio",
		BranchName:   "br",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	spy := &spyRecomputer{}
	store := NewRecomputingSessionStore(inner, spy)

	label := "working"
	intent := int32(1)
	spinner := true
	if _, err := store.Update(context.Background(), sess.ID, UpdateSessionParams{
		DisplayLabel:   &label,
		DisplayIntent:  &intent,
		DisplaySpinner: &spinner,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	if got := spy.calls.Load(); got != 0 {
		t.Errorf("Recompute calls = %d, want 0 (display-trio-only writes are self-writes)", got)
	}
}

// TestRecomputingSessionStore_TriggersOnStateUpdate keeps the original
// State-change behavior covered: lifecycle transitions remain the canonical
// composite-input trigger.
func TestRecomputingSessionStore_TriggersOnStateUpdate(t *testing.T) {
	database := setupTestDB(t)
	repos := NewRepoStore(database)
	repo, err := repos.Create(context.Background(), CreateRepoParams{
		DisplayName:       "test-repo",
		LocalPath:         "/tmp/test-recompute-state",
		OriginURL:         "https://github.com/test/repo.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}

	inner := NewSessionStore(database)
	sess, err := inner.Create(context.Background(), CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "t",
		WorktreePath: "/tmp/wt-state",
		BranchName:   "br",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	spy := &spyRecomputer{}
	store := NewRecomputingSessionStore(inner, spy)

	newState := 1
	if _, err := store.Update(context.Background(), sess.ID, UpdateSessionParams{
		State: &newState,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	if got := spy.calls.Load(); got != 1 {
		t.Errorf("Recompute calls = %d, want 1 (State is a composite input)", got)
	}
}

// TestIsComputerSelfWrite_TableDriven exhaustively covers the classifier.
func TestIsComputerSelfWrite_TableDriven(t *testing.T) {
	label := "x"
	intent := int32(0)
	spinner := false
	state := 1
	claude := "c"
	pClaude := &claude

	cases := []struct {
		name   string
		params UpdateSessionParams
		want   bool
	}{
		{
			name:   "empty params is not a self-write",
			params: UpdateSessionParams{},
			want:   false,
		},
		{
			name:   "display label only",
			params: UpdateSessionParams{DisplayLabel: &label},
			want:   true,
		},
		{
			name: "full display trio",
			params: UpdateSessionParams{
				DisplayLabel:   &label,
				DisplayIntent:  &intent,
				DisplaySpinner: &spinner,
			},
			want: true,
		},
		{
			name: "display trio plus state is NOT self-write",
			params: UpdateSessionParams{
				DisplayLabel: &label,
				State:        &state,
			},
			want: false,
		},
		{
			name:   "claude session id only is NOT self-write",
			params: UpdateSessionParams{AgentSessionID: &pClaude},
			want:   false,
		},
		{
			name:   "state only is NOT self-write",
			params: UpdateSessionParams{State: &state},
			want:   false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := isComputerSelfWrite(tc.params); got != tc.want {
				t.Errorf("isComputerSelfWrite = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- Session transition observer (BOS-557) ---------------------------------
//
// These cover THE single hook standing broadcast subscriptions fire from. The
// contract under test is deliberately narrow: fire once per real state change,
// never on a no-op re-write, and never let the observer's outcome change what
// the store method returns or persists.

// spyTransitionObserver counts OnSessionState invocations, records the states
// it was told about, and can be scripted to fail.
type spyTransitionObserver struct {
	mu     sync.Mutex
	states []machine.State
	ids    []string
	err    error
}

func (s *spyTransitionObserver) OnSessionState(_ context.Context, sessionID string, to machine.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids = append(s.ids, sessionID)
	s.states = append(s.states, to)
	return s.err
}

func (s *spyTransitionObserver) seen() []machine.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]machine.State(nil), s.states...)
}

func (s *spyTransitionObserver) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.states)
}

// newObservedSessionStore builds a session in a fresh database, parks it in
// startState, and returns the decorated store plus the observer watching it.
// The park happens on the RAW store so the setup write is not itself observed.
func newObservedSessionStore(t *testing.T, startState machine.State) (*RecomputingSessionStore, *spyTransitionObserver, string) {
	t.Helper()
	database := setupTestDB(t)
	repos := NewRepoStore(database)
	repo, err := repos.Create(context.Background(), CreateRepoParams{
		DisplayName:       "test-repo",
		LocalPath:         "/tmp/test-transition-observer",
		OriginURL:         "https://github.com/test/repo.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	inner := NewSessionStore(database)
	sess, err := inner.Create(context.Background(), CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "t",
		WorktreePath: "/tmp/wt-transition-observer",
		BranchName:   "br",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	start := int(startState)
	if _, err := inner.Update(context.Background(), sess.ID, UpdateSessionParams{State: &start}); err != nil {
		t.Fatalf("park session in %v: %v", startState, err)
	}
	obs := &spyTransitionObserver{}
	store := NewRecomputingSessionStore(inner, &spyRecomputer{}).WithTransitionObserver(obs)
	return store, obs, sess.ID
}

// TestRecomputingSessionStore_ObserverFiresOnceOnARealStateChange is the
// exactly-one-hook proof for the Update path: one write that really changes the
// state notifies the observer exactly once, with the state actually persisted.
func TestRecomputingSessionStore_ObserverFiresOnceOnARealStateChange(t *testing.T) {
	store, obs, id := newObservedSessionStore(t, machine.ImplementingPlan)

	merged := int(machine.Merged)
	if _, err := store.Update(context.Background(), id, UpdateSessionParams{State: &merged}); err != nil {
		t.Fatalf("update: %v", err)
	}

	if got := obs.count(); got != 1 {
		t.Fatalf("observer calls = %d, want exactly 1", got)
	}
	if got := obs.seen()[0]; got != machine.Merged {
		t.Errorf("observed state = %v, want %v", got, machine.Merged)
	}
	if obs.ids[0] != id {
		t.Errorf("observed session = %q, want %q", obs.ids[0], id)
	}
}

// TestRecomputingSessionStore_ObserverSeesTheErroredTransition covers the other
// outcome class: an arrival in Blocked is what an `errored` subscription waits
// for, so the hook must report it identically.
func TestRecomputingSessionStore_ObserverSeesTheErroredTransition(t *testing.T) {
	store, obs, id := newObservedSessionStore(t, machine.ImplementingPlan)

	blocked := int(machine.Blocked)
	if _, err := store.Update(context.Background(), id, UpdateSessionParams{State: &blocked}); err != nil {
		t.Fatalf("update: %v", err)
	}

	if got := obs.seen(); len(got) != 1 || got[0] != machine.Blocked {
		t.Fatalf("observed states = %v, want exactly [%v]", got, machine.Blocked)
	}
}

// TestRecomputingSessionStore_ObserverIgnoresANoOpRewrite is the edge gate: a
// non-nil params.State is NOT enough. Re-writing the state a session already
// holds is not a transition and must notify nothing.
func TestRecomputingSessionStore_ObserverIgnoresANoOpRewrite(t *testing.T) {
	store, obs, id := newObservedSessionStore(t, machine.Merged)

	merged := int(machine.Merged)
	if _, err := store.Update(context.Background(), id, UpdateSessionParams{State: &merged}); err != nil {
		t.Fatalf("update: %v", err)
	}

	if got := obs.count(); got != 0 {
		t.Fatalf("observer calls = %d, want 0 (re-writing the same state is not a transition)", got)
	}
}

// TestRecomputingSessionStore_ObserverIgnoresAStatelessUpdate keeps the hook off
// the many writes that touch no state at all.
func TestRecomputingSessionStore_ObserverIgnoresAStatelessUpdate(t *testing.T) {
	store, obs, id := newObservedSessionStore(t, machine.ImplementingPlan)

	title := "renamed"
	if _, err := store.Update(context.Background(), id, UpdateSessionParams{Title: &title}); err != nil {
		t.Fatalf("update: %v", err)
	}

	if got := obs.count(); got != 0 {
		t.Fatalf("observer calls = %d, want 0", got)
	}
}

// TestRecomputingSessionStore_ObserverFiresOnceForAConditionalUpdate covers the
// second family of state writers. UpdateStateConditional's bool return already
// edge-gates, so a won CAS notifies once and the lost repeat notifies not at all.
func TestRecomputingSessionStore_ObserverFiresOnceForAConditionalUpdate(t *testing.T) {
	store, obs, id := newObservedSessionStore(t, machine.ImplementingPlan)

	advanced, err := store.UpdateStateConditional(context.Background(), id,
		int(machine.Blocked), int(machine.ImplementingPlan))
	if err != nil {
		t.Fatalf("conditional update: %v", err)
	}
	if !advanced {
		t.Fatal("first conditional update did not advance")
	}
	// The losing repeat: the row is no longer in the expected state.
	again, err := store.UpdateStateConditional(context.Background(), id,
		int(machine.Blocked), int(machine.ImplementingPlan))
	if err != nil {
		t.Fatalf("second conditional update: %v", err)
	}
	if again {
		t.Fatal("second conditional update advanced; the CAS should have lost")
	}

	if got := obs.seen(); len(got) != 1 || got[0] != machine.Blocked {
		t.Fatalf("observed states = %v, want exactly [%v]", got, machine.Blocked)
	}
}

// TestRecomputingSessionStore_ObserverIgnoresASelfConditional guards the one
// conditional shape that provably cannot change anything: expected == new.
func TestRecomputingSessionStore_ObserverIgnoresASelfConditional(t *testing.T) {
	store, obs, id := newObservedSessionStore(t, machine.Merged)

	advanced, err := store.UpdateStateConditional(context.Background(), id,
		int(machine.Merged), int(machine.Merged))
	if err != nil {
		t.Fatalf("conditional update: %v", err)
	}
	if !advanced {
		t.Fatal("self-conditional did not report a row (it should still match)")
	}
	if got := obs.count(); got != 0 {
		t.Fatalf("observer calls = %d, want 0 (expected == new is not a transition)", got)
	}
}

// TestRecomputingSessionStore_ObserverFiresForAConditionalFromSet covers the
// from-set variant used by the finalize path.
func TestRecomputingSessionStore_ObserverFiresForAConditionalFromSet(t *testing.T) {
	store, obs, id := newObservedSessionStore(t, machine.ImplementingPlan)

	advanced, err := store.UpdateStateConditionalFrom(context.Background(), id,
		int(machine.Finalizing), []int{int(machine.ImplementingPlan), int(machine.AwaitingChecks)})
	if err != nil {
		t.Fatalf("conditional-from update: %v", err)
	}
	if !advanced {
		t.Fatal("conditional-from update did not advance")
	}
	if got := obs.seen(); len(got) != 1 || got[0] != machine.Finalizing {
		t.Fatalf("observed states = %v, want exactly [%v]", got, machine.Finalizing)
	}
}

// TestRecomputingSessionStore_ObserverFiresForTheOrphanMarker covers the atomic
// orphan stamp — an `errored` outcome that never passes through Update.
func TestRecomputingSessionStore_ObserverFiresForTheOrphanMarker(t *testing.T) {
	store, obs, id := newObservedSessionStore(t, machine.ImplementingPlan)

	advanced, err := store.OrphanHeadlessRun(context.Background(), id, "daemon restart")
	if err != nil {
		t.Fatalf("orphan headless run: %v", err)
	}
	if !advanced {
		t.Fatal("orphan headless run did not advance")
	}
	if got := obs.seen(); len(got) != 1 || got[0] != machine.Orphaned {
		t.Fatalf("observed states = %v, want exactly [%v]", got, machine.Orphaned)
	}
	// Read the row back. The override notifies with a hand-copied literal while
	// the target state actually lives in the store's SQL, and comparing the
	// notification only against that same literal is circular: it would stay
	// green if the SQL moved the session somewhere else, leaving the observer
	// reporting a transition that never happened. This pins the two together.
	read, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if read.State != obs.seen()[0] {
		t.Fatalf("observer was told %v but the store persisted %v: the notify literal "+
			"has drifted from OrphanHeadlessRun's SQL", obs.seen()[0], read.State)
	}
}

// TestRecomputingSessionStore_ObserverErrorNeverFailsTheTransition is the
// acceptance criterion in test form: a broadcast-send failure surfaces as an
// observer error, and the session transition must still succeed and persist.
func TestRecomputingSessionStore_ObserverErrorNeverFailsTheTransition(t *testing.T) {
	store, obs, id := newObservedSessionStore(t, machine.ImplementingPlan)
	obs.err = errors.New("broadcast send exploded")

	merged := int(machine.Merged)
	sess, err := store.Update(context.Background(), id, UpdateSessionParams{State: &merged})
	if err != nil {
		t.Fatalf("update returned %v; a failing observer must not fail the transition", err)
	}
	if sess == nil || sess.State != machine.Merged {
		t.Fatalf("update returned state %v, want %v", sess, machine.Merged)
	}
	// And the change is really persisted, not just reported.
	read, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if read.State != machine.Merged {
		t.Errorf("persisted state = %v, want %v", read.State, machine.Merged)
	}

	// The conditional path must behave identically.
	advanced, err := store.UpdateStateConditional(context.Background(), id,
		int(machine.Closed), int(machine.Merged))
	if err != nil {
		t.Fatalf("conditional update returned %v; a failing observer must not fail it", err)
	}
	if !advanced {
		t.Fatal("conditional update did not advance under a failing observer")
	}
}

// TestRecomputingSessionStore_NilObserverIsACleanNoOp: every construction site
// other than the daemon builds the store without an evaluator.
func TestRecomputingSessionStore_NilObserverIsACleanNoOp(t *testing.T) {
	database := setupTestDB(t)
	repos := NewRepoStore(database)
	repo, err := repos.Create(context.Background(), CreateRepoParams{
		DisplayName:       "test-repo",
		LocalPath:         "/tmp/test-nil-observer",
		OriginURL:         "https://github.com/test/repo.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	inner := NewSessionStore(database)
	sess, err := inner.Create(context.Background(), CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "t",
		WorktreePath: "/tmp/wt-nil-observer",
		BranchName:   "br",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	store := NewRecomputingSessionStore(inner, &spyRecomputer{})

	merged := int(machine.Merged)
	if _, err := store.Update(context.Background(), sess.ID, UpdateSessionParams{State: &merged}); err != nil {
		t.Fatalf("update with no observer: %v", err)
	}
	if _, err := store.UpdateStateConditional(context.Background(), sess.ID,
		int(machine.Closed), int(machine.Merged)); err != nil {
		t.Fatalf("conditional update with no observer: %v", err)
	}
}
