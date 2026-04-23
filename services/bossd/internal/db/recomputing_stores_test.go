package db

import (
	"context"
	"sync/atomic"
	"testing"
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
		ClaudeSessionID: &pClaude,
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
			params: UpdateSessionParams{ClaudeSessionID: &pClaude},
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
