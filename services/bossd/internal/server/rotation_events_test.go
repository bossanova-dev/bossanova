package server

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

func TestWithRotationEvents_HydratesNewestFirst(t *testing.T) {
	store := db.NewRotationEventStore(setupServerTestDB(t))
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)

	// Two events for sess-1; the ROTATED one is newer.
	reset := base.Add(90 * time.Minute)
	if err := store.Insert(ctx, db.RotationEvent{
		ID: "ev-old", SessionID: "sess-1", Provider: "claude",
		Trigger: "ROTATION_TRIGGER_USAGE_LIMITED", Outcome: "ROTATION_OUTCOME_EXHAUSTED",
		ResetAt: &reset, CreatedAt: base,
	}); err != nil {
		t.Fatalf("insert old: %v", err)
	}
	if err := store.Insert(ctx, db.RotationEvent{
		ID: "ev-new", SessionID: "sess-1", Provider: "claude",
		Trigger: "ROTATION_TRIGGER_USAGE_LIMITED", Outcome: "ROTATION_OUTCOME_ROTATED",
		FromAccount: "acct-a", ToAccount: "acct-b", CreatedAt: base.Add(time.Second),
	}); err != nil {
		t.Fatalf("insert new: %v", err)
	}

	s := &Server{rotationEvents: store, logger: zerolog.Nop()}
	p := &pb.Session{}
	s.withRotationEvents(ctx, p, &models.Session{ID: "sess-1"})

	if len(p.RotationEvents) != 2 {
		t.Fatalf("want 2 hydrated events, got %d", len(p.RotationEvents))
	}
	if p.RotationEvents[0].GetOutcome() != pb.RotationOutcome_ROTATION_OUTCOME_ROTATED {
		t.Fatalf("want newest (ROTATED) first, got %v", p.RotationEvents[0].GetOutcome())
	}
	if p.RotationEvents[0].GetFromAccount() != "acct-a" || p.RotationEvents[0].GetToAccount() != "acct-b" {
		t.Fatalf("account labels not mapped: %+v", p.RotationEvents[0])
	}
	if got := p.RotationEvents[1]; got.GetOutcome() != pb.RotationOutcome_ROTATION_OUTCOME_EXHAUSTED ||
		got.GetResetAt() == nil {
		t.Fatalf("want exhausted event with reset_at, got %+v", got)
	}
}

func TestWithRotationEvents_NilStoreNoop(t *testing.T) {
	s := &Server{logger: zerolog.Nop()}
	p := &pb.Session{}
	s.withRotationEvents(context.Background(), p, &models.Session{ID: "sess-x"})
	if len(p.RotationEvents) != 0 {
		t.Fatalf("nil store must hydrate nothing, got %d", len(p.RotationEvents))
	}
}
