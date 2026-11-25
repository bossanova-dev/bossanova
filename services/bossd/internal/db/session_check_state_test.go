package db

import (
	"testing"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossalib/machine"
)

func TestSessionStore_SetCheckStateIfUnchanged(t *testing.T) {
	database := setupTestDB(t)
	repo := createTestRepo(t, NewRepoStore(database))
	store := NewSessionStore(database)
	ctx := t.Context()

	sess, err := store.Create(ctx, CreateSessionParams{RepoID: repo.ID, Title: "check latch"})
	if err != nil {
		t.Fatal(err)
	}

	// First latch succeeds against the initial (UNSPECIFIED, nil, nil) snapshot.
	updated, err := store.SetCheckStateIfUnchanged(
		ctx,
		sess.ID,
		sess.LastCheckState,
		sess.LastCheckStateHeadSHA,
		sess.LastCheckStateAt,
		machine.CheckStatePending,
		"head-1",
	)
	if err != nil {
		t.Fatalf("initial SetCheckStateIfUnchanged: %v", err)
	}
	if !updated {
		t.Fatal("initial SetCheckStateIfUnchanged = false, want true")
	}

	snap1, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap1.LastCheckState != machine.CheckStatePending || snap1.LastCheckStateHeadSHA == nil || *snap1.LastCheckStateHeadSHA != "head-1" || snap1.LastCheckStateAt == nil {
		t.Fatalf("after first latch = (%v, %v, %v), want (Pending, head-1, non-nil)", snap1.LastCheckState, snap1.LastCheckStateHeadSHA, snap1.LastCheckStateAt)
	}

	// Simulate Dispatcher writing a newer PASSED verdict on head-2 while a poller holds snap1.
	passed := int(machine.CheckStatePassed)
	head2 := "head-2"
	head2Ptr := &head2
	observedAt := sqlutil.TimeNow()
	observedAtPtr := &observedAt
	if _, err := store.Update(ctx, sess.ID, UpdateSessionParams{
		LastCheckState:        &passed,
		LastCheckStateHeadSHA: &head2Ptr,
		LastCheckStateAt:      &observedAtPtr,
	}); err != nil {
		t.Fatalf("concurrent dispatcher update: %v", err)
	}

	// Stale poller CAS using snap1 must not overwrite the newer PASSED/head-2 verdict.
	updated, err = store.SetCheckStateIfUnchanged(
		ctx,
		sess.ID,
		snap1.LastCheckState,
		snap1.LastCheckStateHeadSHA,
		snap1.LastCheckStateAt,
		machine.CheckStateFailed,
		"head-1",
	)
	if err != nil {
		t.Fatalf("stale SetCheckStateIfUnchanged: %v", err)
	}
	if updated {
		t.Fatal("stale SetCheckStateIfUnchanged = true, want false")
	}

	got, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastCheckState != machine.CheckStatePassed {
		t.Errorf("LastCheckState = %v, want preserved Passed", got.LastCheckState)
	}
	if got.LastCheckStateHeadSHA == nil || *got.LastCheckStateHeadSHA != "head-2" {
		t.Errorf("LastCheckStateHeadSHA = %v, want preserved head-2", got.LastCheckStateHeadSHA)
	}

	// A fresh poller reading got can still advance the latch on a subsequent head.
	updated, err = store.SetCheckStateIfUnchanged(
		ctx,
		sess.ID,
		got.LastCheckState,
		got.LastCheckStateHeadSHA,
		got.LastCheckStateAt,
		machine.CheckStateFailed,
		"head-3",
	)
	if err != nil {
		t.Fatalf("fresh SetCheckStateIfUnchanged: %v", err)
	}
	if !updated {
		t.Fatal("fresh SetCheckStateIfUnchanged = false, want true")
	}
	after, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.LastCheckState != machine.CheckStateFailed || after.LastCheckStateHeadSHA == nil || *after.LastCheckStateHeadSHA != "head-3" {
		t.Fatalf("after fresh latch = (%v, %v), want (Failed, head-3)", after.LastCheckState, after.LastCheckStateHeadSHA)
	}
}
