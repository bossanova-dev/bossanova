package db

import (
	"context"
	"testing"
	"time"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
)

func TestRotationEventStore_InsertAndRecent(t *testing.T) {
	store := NewRotationEventStore(setupTestDB(t))

	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)
	for i := range 3 {
		id, err := sqlutil.NewID()
		if err != nil {
			t.Fatalf("NewID: %v", err)
		}
		reset := base.Add(2 * time.Hour)
		ev := RotationEvent{
			ID: id, SessionID: "sess-1", ChatID: "", Provider: "claude",
			Trigger:     "ROTATION_TRIGGER_USAGE_LIMITED",
			FromAccount: "acct-a", ToAccount: "acct-b",
			ResetAt: &reset, Outcome: "ROTATION_OUTCOME_ROTATED",
			Detail: "resets 15:00", CreatedAt: base.Add(time.Duration(i) * time.Second),
		}
		if err := store.Create(ctx, ev); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	got, err := store.ListBySession(ctx, "sess-1", 2)
	if err != nil {
		t.Fatalf("ListBySession: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 events, got %d", len(got))
	}
	if !got[0].CreatedAt.After(got[1].CreatedAt) {
		t.Fatalf("want newest-first ordering, got %v then %v", got[0].CreatedAt, got[1].CreatedAt)
	}
	if got[0].ResetAt == nil || !got[0].ResetAt.Equal(base.Add(2*time.Hour)) {
		t.Fatalf("reset_at round-trip failed: %+v", got[0].ResetAt)
	}
	if got[0].FromAccount != "acct-a" || got[0].ToAccount != "acct-b" {
		t.Fatalf("account labels round-trip failed: %+v", got[0])
	}
}

func TestRotationEventStore_NilResetAndOtherSession(t *testing.T) {
	store := NewRotationEventStore(setupTestDB(t))
	ctx := context.Background()

	id, err := sqlutil.NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}
	if err := store.Create(ctx, RotationEvent{
		ID: id, SessionID: "sess-1", Provider: "codex",
		Trigger: "ROTATION_TRIGGER_MANUAL", Outcome: "ROTATION_OUTCOME_EXHAUSTED",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.ListBySession(ctx, "sess-1", 10)
	if err != nil {
		t.Fatalf("ListBySession: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	if got[0].ResetAt != nil {
		t.Fatalf("want nil reset_at, got %v", got[0].ResetAt)
	}
	if got[0].CreatedAt.IsZero() {
		t.Fatalf("Create must default CreatedAt when unset")
	}

	// A different session sees nothing.
	other, err := store.ListBySession(ctx, "sess-2", 10)
	if err != nil {
		t.Fatalf("ListBySession other: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("want 0 events for other session, got %d", len(other))
	}
}

func TestRotationEventStore_GetByConfirmedAuthInvalidationSince(t *testing.T) {
	store := NewRotationEventStore(setupTestDB(t))
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)

	events := []RotationEvent{
		{
			ID:        "before-current-episode",
			SessionID: "sess-1",
			ChatID:    "agent-1",
			Trigger:   "ROTATION_TRIGGER_AUTH_INVALIDATED",
			Outcome:   "ROTATION_OUTCOME_ROTATED",
			CreatedAt: base.Add(-time.Second),
		},
		{
			ID:        "unconfirmed-current-episode",
			SessionID: "sess-1",
			ChatID:    "agent-1",
			Trigger:   "ROTATION_TRIGGER_AUTH_INVALIDATED",
			Outcome:   "ROTATION_OUTCOME_STATUS_ONLY_DISABLED",
			CreatedAt: base.Add(time.Second),
		},
		{
			ID:        "other-chat-current-episode",
			SessionID: "sess-1",
			ChatID:    "agent-2",
			Trigger:   "ROTATION_TRIGGER_AUTH_INVALIDATED",
			Outcome:   "ROTATION_OUTCOME_ROTATED",
			CreatedAt: base.Add(time.Second),
		},
		{
			ID:        "confirmed-current-episode",
			SessionID: "sess-1",
			ChatID:    "agent-1",
			Trigger:   "ROTATION_TRIGGER_AUTH_INVALIDATED",
			Outcome:   "ROTATION_OUTCOME_ROTATED",
			CreatedAt: base.Add(2 * time.Second),
		},
	}
	for _, ev := range events {
		if err := store.Create(ctx, ev); err != nil {
			t.Fatalf("Create(%s): %v", ev.ID, err)
		}
	}

	ok, err := store.GetByConfirmedAuthInvalidationSince(ctx, "sess-1", "agent-1", base)
	if err != nil {
		t.Fatalf("GetByConfirmedAuthInvalidationSince: %v", err)
	}
	if !ok {
		t.Fatal("GetByConfirmedAuthInvalidationSince = false, want true for confirmed current episode")
	}
	ok, err = store.GetByConfirmedAuthInvalidationSince(ctx, "sess-1", "agent-1", base.Add(3*time.Second))
	if err != nil {
		t.Fatalf("GetByConfirmedAuthInvalidationSince later: %v", err)
	}
	if ok {
		t.Fatal("GetByConfirmedAuthInvalidationSince later = true, want false after confirmed event")
	}
}
