package main

import (
	"context"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/session"
)

func TestMergeSessionEventsFansInAndCloses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := make(chan session.SessionEvent, 1)
	b := make(chan session.SessionEvent, 1)
	a <- session.SessionEvent{SessionID: "from-a"}
	b <- session.SessionEvent{SessionID: "from-b"}
	close(a)
	close(b)

	merged := mergeSessionEvents(ctx, a, b)

	got := map[string]bool{}
	for ev := range merged {
		got[ev.SessionID] = true
	}

	if !got["from-a"] || !got["from-b"] || len(got) != 2 {
		t.Fatalf("merged events = %#v, want both inputs", got)
	}
}

func TestMergeSessionEventsStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := make(chan session.SessionEvent)
	b := make(chan session.SessionEvent)

	merged := mergeSessionEvents(ctx, a, b)
	cancel()

	select {
	case _, ok := <-merged:
		if ok {
			t.Fatal("merged channel still open after context cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for merged channel to close")
	}
}
