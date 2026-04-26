package upstream

import (
	"context"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

// staticEventSource is a test-only EventSource that delivers a
// pre-seeded set of events on Subscribe and then closes the channel.
// Close-on-drain models the "publisher exited" path; tests that want
// to hold the subscription open simply don't exhaust the seed.
type staticEventSource struct {
	events []StreamEvent
}

func (s *staticEventSource) Subscribe(ctx context.Context) <-chan StreamEvent {
	out := make(chan StreamEvent, len(s.events)+1)
	for _, ev := range s.events {
		out <- ev
	}
	close(out)
	return out
}

// newForwarderClient wires a StreamClient with just enough plumbing
// for subscribeDeltas to run end-to-end: an EventSource, a zero Clock,
// and nothing else. The test then drives the forwarder directly via
// subscribeDeltas and reads from the outbound channel.
func newForwarderClient(t *testing.T, events EventSource, window time.Duration) *StreamClient {
	t.Helper()
	if window == 0 {
		window = 10 * time.Millisecond
	}
	return NewStreamClient(StreamClientConfig{
		Events:         events,
		Logger:         zerolog.Nop(),
		CoalesceWindow: window,
	})
}

// drainFor runs subscribeDeltas in a goroutine and returns every
// event that lands on outbound before the deadline elapses. After the
// deadline it cancels the context to shut the forwarder down cleanly.
func drainFor(t *testing.T, client *StreamClient, timeout time.Duration) []*pb.DaemonEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	outbound := make(chan *pb.DaemonEvent, 32)

	done := make(chan struct{})
	go func() {
		defer close(done)
		client.subscribeDeltas(ctx, outbound)
		close(outbound)
	}()

	deadline := time.After(timeout)
	var got []*pb.DaemonEvent
	for {
		select {
		case ev, ok := <-outbound:
			if !ok {
				cancel()
				<-done
				return got
			}
			got = append(got, ev)
		case <-deadline:
			cancel()
			<-done
			// Drain anything still queued after cancel.
			for ev := range outbound {
				got = append(got, ev)
			}
			return got
		}
	}
}

func TestSubscribeDeltas_SessionCreatedEvent_EmitsSessionDeltaCreated(t *testing.T) {
	src := &staticEventSource{events: []StreamEvent{{
		Session: &SessionEvent{
			Kind:    pb.SessionDelta_KIND_CREATED,
			Session: &pb.Session{Id: "s1", Title: "a"},
		},
	}}}
	got := drainFor(t, newForwarderClient(t, src, 0), 200*time.Millisecond)

	found := false
	for _, ev := range got {
		if d := ev.GetSession(); d != nil {
			if d.GetKind() == pb.SessionDelta_KIND_CREATED && d.GetSession().GetId() == "s1" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected session.created delta for s1; got %v", got)
	}
}

func TestSubscribeDeltas_SessionUpdatedEvent_EmitsSessionDeltaUpdated(t *testing.T) {
	src := &staticEventSource{events: []StreamEvent{{
		Session: &SessionEvent{
			Kind:    pb.SessionDelta_KIND_UPDATED,
			Session: &pb.Session{Id: "s1", Title: "renamed"},
		},
	}}}
	got := drainFor(t, newForwarderClient(t, src, 0), 200*time.Millisecond)
	found := false
	for _, ev := range got {
		if d := ev.GetSession(); d != nil && d.GetKind() == pb.SessionDelta_KIND_UPDATED {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected session.updated delta; got %v", got)
	}
}

func TestSubscribeDeltas_SessionDeletedEvent_EmitsSessionDeltaDeleted(t *testing.T) {
	// Deletes propagate with only session.id populated — the forwarder
	// is dumb on purpose: the publisher upstream decides what fields
	// survive into the delta.
	src := &staticEventSource{events: []StreamEvent{{
		Session: &SessionEvent{
			Kind:    pb.SessionDelta_KIND_DELETED,
			Session: &pb.Session{Id: "s1"},
		},
	}}}
	got := drainFor(t, newForwarderClient(t, src, 0), 200*time.Millisecond)
	found := false
	for _, ev := range got {
		if d := ev.GetSession(); d != nil &&
			d.GetKind() == pb.SessionDelta_KIND_DELETED &&
			d.GetSession().GetId() == "s1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected session.deleted delta for s1; got %v", got)
	}
}

func TestSubscribeDeltas_ChatCreated_EmitsChatDelta(t *testing.T) {
	src := &staticEventSource{events: []StreamEvent{{
		Chat: &ChatEvent{
			Kind: pb.ChatDelta_KIND_CREATED,
			Chat: &pb.ClaudeChatMetadata{Id: "c1", SessionId: "s1"},
		},
	}}}
	got := drainFor(t, newForwarderClient(t, src, 0), 200*time.Millisecond)
	found := false
	for _, ev := range got {
		if d := ev.GetChat(); d != nil && d.GetKind() == pb.ChatDelta_KIND_CREATED && d.GetChat().GetId() == "c1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected chat.created delta for c1; got %v", got)
	}
}

func TestSubscribeDeltas_StatusChanged_EmitsChatStatus(t *testing.T) {
	// Status events flow through the coalescer — use a short window
	// so the assertion doesn't wait on the default 100ms.
	src := &staticEventSource{events: []StreamEvent{{
		Status: &StatusEvent{
			Status: &pb.ChatStatusDelta{
				SessionId: "s1",
				ClaudeId:  "c1",
				Status:    pb.ChatStatus_CHAT_STATUS_WORKING,
			},
		},
	}}}
	got := drainFor(t, newForwarderClient(t, src, 5*time.Millisecond), 500*time.Millisecond)
	found := false
	for _, ev := range got {
		if s := ev.GetStatus(); s != nil &&
			s.GetSessionId() == "s1" &&
			s.GetClaudeId() == "c1" &&
			s.GetStatus() == pb.ChatStatus_CHAT_STATUS_WORKING {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected chat status delta for s1/c1; got %v", got)
	}
}

func TestSubscribeDeltas_ContextCancelled_ReturnsCleanly(t *testing.T) {
	// A source that never closes — the forwarder must exit only when
	// ctx is cancelled, not by draining the channel.
	neverClose := make(chan StreamEvent)
	src := funcEventSource(func(_ context.Context) <-chan StreamEvent { return neverClose })

	client := newForwarderClient(t, src, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	outbound := make(chan *pb.DaemonEvent, 4)

	done := make(chan struct{})
	go func() {
		defer close(done)
		client.subscribeDeltas(ctx, outbound)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("subscribeDeltas did not return after ctx cancel")
	}
	close(neverClose)
}

// funcEventSource is a tiny adapter letting an inline closure serve as
// an EventSource. Saves a struct declaration for one-off test cases.
type funcEventSource func(ctx context.Context) <-chan StreamEvent

func (f funcEventSource) Subscribe(ctx context.Context) <-chan StreamEvent { return f(ctx) }
