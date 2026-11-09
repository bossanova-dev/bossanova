package db

import (
	"context"
	"testing"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
)

// TestMarkOrphanReattached_ClearsMarkerOnlyForMatchingUnarchivedOrphan pins the
// false-orphan recovery write: it moves an unarchived Orphaned row carrying the
// marker to the requested state and clears the marker, and leaves any other row
// untouched.
func TestMarkOrphanReattached_ClearsMarkerOnlyForMatchingUnarchivedOrphan(t *testing.T) {
	database := setupTestDB(t)
	repoStore := NewRepoStore(database)
	store := NewSessionStore(database)
	ctx := context.Background()
	repo := createTestRepo(t, repoStore)
	marker := "headless run orphaned: daemon restart"
	agentID := "run-prior"

	newOrphan := func(t *testing.T, reason string) *models.Session {
		t.Helper()
		sess, err := store.Create(ctx, CreateSessionParams{
			RepoID:       repo.ID,
			Title:        "reattach orphan",
			WorktreePath: "/tmp/wt/reattach-orphan",
			BranchName:   "feat/reattach-orphan",
			BaseBranch:   "main",
		})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		setSessionState(t, store, sess.ID, machine.Orphaned)
		reasonPtr := &reason
		agentPtr := &agentID
		if _, err := store.Update(ctx, sess.ID, UpdateSessionParams{BlockedReason: &reasonPtr, AgentSessionID: &agentPtr}); err != nil {
			t.Fatalf("set orphan marker: %v", err)
		}
		return sess
	}

	t.Run("matching marker", func(t *testing.T) {
		sess := newOrphan(t, marker)
		advanced, err := store.MarkOrphanReattached(ctx, sess.ID, marker, int(machine.ImplementingPlan))
		if err != nil || !advanced {
			t.Fatalf("reattach = %v, %v; want true, nil", advanced, err)
		}
		got, err := store.Get(ctx, sess.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.State != machine.ImplementingPlan {
			t.Errorf("state = %s, want ImplementingPlan", got.State)
		}
		if got.BlockedReason != nil {
			t.Errorf("blocked reason = %q, want cleared", *got.BlockedReason)
		}
		if got.AgentSessionID == nil || *got.AgentSessionID != agentID {
			t.Errorf("agent session id = %v, want %q kept", got.AgentSessionID, agentID)
		}
	})

	t.Run("other marker", func(t *testing.T) {
		sess := newOrphan(t, "orphaned for another reason")
		if advanced, err := store.MarkOrphanReattached(ctx, sess.ID, marker, int(machine.ImplementingPlan)); err != nil || advanced {
			t.Fatalf("reattach = %v, %v; want false, nil", advanced, err)
		}
		if got, _ := store.Get(ctx, sess.ID); got.State != machine.Orphaned {
			t.Errorf("state = %s, want Orphaned", got.State)
		}
	})

	t.Run("archived", func(t *testing.T) {
		sess := newOrphan(t, marker)
		if err := store.MarkArchived(ctx, sess.ID); err != nil {
			t.Fatalf("archive: %v", err)
		}
		if advanced, err := store.MarkOrphanReattached(ctx, sess.ID, marker, int(machine.ImplementingPlan)); err != nil || advanced {
			t.Fatalf("reattach = %v, %v; want false, nil", advanced, err)
		}
	})
}
