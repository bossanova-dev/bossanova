package rotation

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

type fakeAuditStore struct {
	inserted  []AuditEvent
	recent    []AuditEvent
	insertErr error
}

func (f *fakeAuditStore) Insert(_ context.Context, ev AuditEvent) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	f.inserted = append(f.inserted, ev)
	return nil
}

func (f *fakeAuditStore) RecentBySession(_ context.Context, _ string, _ int) ([]AuditEvent, error) {
	return f.recent, nil
}

func TestRecorder_RecordFillsIDAndTimeAndNeverFails(t *testing.T) {
	t.Parallel()
	fs := &fakeAuditStore{}
	r := NewRecorder(fs, zerolog.Nop())
	r.Record(context.Background(), AuditEvent{
		SessionID: "s1", Provider: "claude",
		Trigger: "ROTATION_TRIGGER_USAGE_LIMITED", Outcome: "ROTATION_OUTCOME_ROTATED",
		FromAccount: "a", ToAccount: "b",
	})
	if len(fs.inserted) != 1 {
		t.Fatalf("want 1 insert, got %d", len(fs.inserted))
	}
	got := fs.inserted[0]
	if got.ID == "" || got.CreatedAt.IsZero() {
		t.Fatalf("Record must fill ID and CreatedAt: %+v", got)
	}

	// Insert failure must not panic or propagate.
	r2 := NewRecorder(&fakeAuditStore{insertErr: context.DeadlineExceeded}, zerolog.Nop())
	r2.Record(context.Background(), AuditEvent{SessionID: "s1"})

	// A nil recorder is a safe no-op (unwired production seam).
	var nilRec *Recorder
	nilRec.Record(context.Background(), AuditEvent{SessionID: "s1"})
	if nilRec.RecordExhausted(context.Background(), AuditEvent{SessionID: "s1"}) {
		t.Fatal("nil recorder RecordExhausted must report false")
	}
}

func TestRecorder_RecordExhaustedDedupsPerEpisode(t *testing.T) {
	t.Parallel()
	until := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Minute)
	ev := AuditEvent{
		SessionID: "s1", Provider: "claude",
		Trigger: "ROTATION_TRIGGER_USAGE_LIMITED",
		Outcome: "ROTATION_OUTCOME_EXHAUSTED", ResetAt: &until,
	}

	// Empty history: records.
	fs := &fakeAuditStore{}
	r := NewRecorder(fs, zerolog.Nop())
	if !r.RecordExhausted(context.Background(), ev) {
		t.Fatal("first exhausted event of an episode must record")
	}
	if len(fs.inserted) != 1 {
		t.Fatalf("first exhausted must insert once, got %d", len(fs.inserted))
	}

	// Latest event is the same exhausted episode (same reset minute): dedup.
	fs2 := &fakeAuditStore{recent: []AuditEvent{{
		Outcome: "ROTATION_OUTCOME_EXHAUSTED", ResetAt: &until,
	}}}
	r2 := NewRecorder(fs2, zerolog.Nop())
	if r2.RecordExhausted(context.Background(), ev) {
		t.Fatal("duplicate exhausted signal within an episode must dedup")
	}
	if len(fs2.inserted) != 0 {
		t.Fatalf("dedup must not insert, got %d", len(fs2.inserted))
	}

	// A rotation after the exhausted event starts a new episode: records again.
	fs3 := &fakeAuditStore{recent: []AuditEvent{{Outcome: "ROTATION_OUTCOME_ROTATED"}}}
	r3 := NewRecorder(fs3, zerolog.Nop())
	if !r3.RecordExhausted(context.Background(), ev) {
		t.Fatal("exhausted after a rotation is a new episode and must record")
	}
}

func TestRecorder_RecordNoEligibleDedupsPerEpisode(t *testing.T) {
	t.Parallel()
	ev := AuditEvent{
		SessionID: "s1", Provider: "claude",
		Trigger: "ROTATION_TRIGGER_USAGE_LIMITED",
		Outcome: "ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT",
		Detail:  NoEligibleAccountDetail,
	}

	// Empty history: records (nil ResetAt is a valid episode key).
	fs := &fakeAuditStore{}
	r := NewRecorder(fs, zerolog.Nop())
	if !r.RecordNoEligible(context.Background(), ev) {
		t.Fatal("first no-eligible event of an episode must record")
	}
	if len(fs.inserted) != 1 {
		t.Fatalf("first no-eligible must insert once, got %d", len(fs.inserted))
	}

	// Latest event is the same no-eligible episode (both ResetAt nil): dedup.
	fs2 := &fakeAuditStore{recent: []AuditEvent{{
		Outcome: "ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT",
	}}}
	r2 := NewRecorder(fs2, zerolog.Nop())
	if r2.RecordNoEligible(context.Background(), ev) {
		t.Fatal("duplicate no-eligible signal within an episode must dedup")
	}
	if len(fs2.inserted) != 0 {
		t.Fatalf("dedup must not insert, got %d", len(fs2.inserted))
	}

	// A different outcome newest starts a new episode: records again.
	fs3 := &fakeAuditStore{recent: []AuditEvent{{Outcome: "ROTATION_OUTCOME_ROTATED"}}}
	r3 := NewRecorder(fs3, zerolog.Nop())
	if !r3.RecordNoEligible(context.Background(), ev) {
		t.Fatal("no-eligible after a rotation is a new episode and must record")
	}

	// A nil recorder is a safe no-op.
	var nilRec *Recorder
	if nilRec.RecordNoEligible(context.Background(), ev) {
		t.Fatal("nil recorder RecordNoEligible must report false")
	}
}
