package db

import (
	"context"
	"testing"
	"time"
)

func TestPendingTrackerMergeStore_ScheduleListDueDelete(t *testing.T) {
	database := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(database))
	sessions := NewSessionStore(database)
	early := createTestSession(t, sessions, repo.ID)
	late := createTestSession(t, sessions, repo.ID)
	store := NewPendingTrackerMergeStore(database)
	t0 := time.Date(2026, 10, 18, 0, 50, 0, 0, time.UTC)

	if err := store.Schedule(ctx, PendingTrackerMerge{SessionID: late.ID, AttemptCount: 1, NextAttemptAt: t0.Add(time.Hour), LastReason: "failed"}); err != nil {
		t.Fatalf("schedule late: %v", err)
	}
	if err := store.Schedule(ctx, PendingTrackerMerge{SessionID: early.ID, AttemptCount: 1, NextAttemptAt: t0.Add(time.Minute), LastReason: "pr-unreadable"}); err != nil {
		t.Fatalf("schedule early: %v", err)
	}
	if got, err := store.ListDue(ctx, t0); err != nil || len(got) != 0 {
		t.Fatalf("due before either = %+v, %v; want none", got, err)
	}
	got, err := store.ListDue(ctx, t0.Add(time.Minute))
	if err != nil || len(got) != 1 || got[0] != (PendingTrackerMerge{SessionID: early.ID, AttemptCount: 1, NextAttemptAt: t0.Add(time.Minute), LastReason: "pr-unreadable"}) {
		t.Fatalf("due at +1m = %+v, %v; want only the early entry as scheduled", got, err)
	}

	// A reschedule replaces the entry rather than adding one.
	if err := store.Schedule(ctx, PendingTrackerMerge{SessionID: early.ID, AttemptCount: 2, NextAttemptAt: t0.Add(2 * time.Hour), LastReason: "failed"}); err != nil {
		t.Fatalf("reschedule: %v", err)
	}
	got, err = store.ListDue(ctx, t0.Add(3*time.Hour))
	if err != nil || len(got) != 2 || got[0].SessionID != late.ID || got[1].SessionID != early.ID || got[1].AttemptCount != 2 {
		t.Fatalf("due at +3h = %+v, %v; want late then the rescheduled early", got, err)
	}

	if err := store.Delete(ctx, early.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.Delete(ctx, early.ID); err != nil {
		t.Fatalf("delete again: %v", err)
	}
	if err := sessions.Delete(ctx, late.ID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if got, err := store.ListDue(ctx, t0.Add(3*time.Hour)); err != nil || len(got) != 0 {
		t.Fatalf("due after deletes = %+v, %v; want none (a deleted session takes its entry with it)", got, err)
	}
}
