package session

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/vcs"
)

type fakeSessionLookup struct {
	sessions []SessionForPR
	err      error
}

func (f fakeSessionLookup) SessionsForPR(context.Context, string, int) ([]SessionForPR, error) {
	return f.sessions, f.err
}

func TestSessionEventEmitter_EmitsOnePerSessionPerEvent(t *testing.T) {
	ctx := context.Background()
	lookup := fakeSessionLookup{
		sessions: []SessionForPR{
			{ID: "s1"},
			{ID: "s2"},
		},
	}
	ch := make(chan SessionEvent, 4)
	emitter := NewSessionEventEmitter(lookup, ch, zerolog.Nop())

	err := emitter.EmitForPR(ctx, "https://github.com/owner/repo", 42, []vcs.Event{
		vcs.ConflictDetected{PRID: 42},
	})
	if err != nil {
		t.Fatalf("EmitForPR returned error: %v", err)
	}

	events := drainChannel(ch)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}

	wantSessionIDs := map[string]bool{"s1": true, "s2": true}
	for _, ev := range events {
		if !wantSessionIDs[ev.SessionID] {
			t.Fatalf("unexpected session ID %q", ev.SessionID)
		}
		delete(wantSessionIDs, ev.SessionID)
		if _, ok := ev.Event.(vcs.ConflictDetected); !ok {
			t.Fatalf("got event type %T, want vcs.ConflictDetected", ev.Event)
		}
	}
	if len(wantSessionIDs) != 0 {
		t.Fatalf("missing session IDs: %v", wantSessionIDs)
	}
}

func drainChannel(ch <-chan SessionEvent) []SessionEvent {
	var events []SessionEvent
	for {
		select {
		case ev := <-ch:
			events = append(events, ev)
		default:
			return events
		}
	}
}
