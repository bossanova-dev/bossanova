package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/safego"
	"github.com/rs/zerolog"
)

// BOS-1274. openStream shares one `outbound` channel between a single
// consumer (the writer) and several producers (the delta forwarder, the
// token refresher, the async command dispatchers). The channel may only be
// closed once every producer has been joined.
//
// The failure mode these tests pin is narrower than "a parked sender is woken
// by close". Measured on this toolchain: a producer already parked in
//
//	select { case outbound <- ev: case <-ctx.Done(): }
//
// when `cancel()` runs never panics, because closing the Done channel claims
// the select (runtime `sudog.selectDone` CAS) and the later close(outbound)
// skips that sender. What does panic is a producer that reaches the select
// *after* close(outbound) has run: `selectgo` polls the cases in a random
// order and a send case on a closed channel panics on sight, whichever way
// `ctx.Done()` happens to be ordered. That is a coin flip per teardown, which
// is why the defect surfaced once in production and why the discriminating
// tests below repeat the teardown rather than running it once.
//
// Each repetition is an independent openStream, so the fixed ordering is green
// deterministically (a joined producer cannot reach the send at all), while the
// unfixed ordering is red with probability 1-(1-p)^N. Measured p on this
// toolchain is ~0.55 for the refresher case and ~0.32 for the forwarder — the
// select-order coin flip, not a clean 1/2 — which at N=64 leaves under 1e-11
// chance of an unfixed ordering escaping both tests.
const teardownRepetitions = 64

// panicRecorder captures every panic safego recovers while a test runs.
// safego's recover hook is process-global, so tests using it must not run in
// parallel; t.Cleanup clears it.
type panicRecorder struct {
	mu     sync.Mutex
	events []string
}

func recordRecoveredPanics(t *testing.T) *panicRecorder {
	t.Helper()
	rec := &panicRecorder{}
	safego.RegisterRecoverHook(func(r any, stack []byte) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.events = append(rec.events, fmt.Sprintf("%v\n%s", r, stack))
	})
	t.Cleanup(func() { safego.RegisterRecoverHook(nil) })
	return rec
}

func (r *panicRecorder) events0() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// assertNoPanics fails with the first recovered panic verbatim — the stack is
// what tells a reader which producer was still live at close(outbound).
func (r *panicRecorder) assertNoPanics(t *testing.T) {
	t.Helper()
	got := r.events0()
	if len(got) == 0 {
		return
	}
	t.Fatalf("safego recovered %d panic(s) during openStream teardown; first:\n%s", len(got), got[0])
}

// stallingDaemonStream is a bidirectionalStream double that lets a test hold
// the writer inside Send. The handshake snapshot (sent on openStream's own
// goroutine, before the writer exists) is always accepted; every later Send —
// i.e. every send the writer makes — stalls until release is closed or the
// stream context is cancelled.
//
// Holding the writer inside Send is what makes the teardown's writer join
// blocking, so a test can act while the teardown is parked there.
type stallingDaemonStream struct {
	ctx context.Context

	snapshot     chan *pb.DaemonEvent
	sentSnapshot atomic.Bool

	stalledOnce sync.Once
	stalled     chan struct{}
	release     chan struct{}

	readerOnce      sync.Once
	readerStarted   chan struct{}
	returnOnce      sync.Once
	readerReturning chan struct{}

	// receiveGate, when closed, makes Receive return receiveErr (which must
	// be non-nil). Left open, Receive blocks until the stream context is
	// cancelled and returns ctx.Err().
	receiveGate chan struct{}
	receiveErr  error
}

func newStallingDaemonStream() *stallingDaemonStream {
	return &stallingDaemonStream{
		snapshot:        make(chan *pb.DaemonEvent, 1),
		stalled:         make(chan struct{}),
		release:         make(chan struct{}),
		readerStarted:   make(chan struct{}),
		readerReturning: make(chan struct{}),
		receiveGate:     make(chan struct{}),
	}
}

func (s *stallingDaemonStream) Send(event *pb.DaemonEvent) error {
	if s.sentSnapshot.CompareAndSwap(false, true) {
		s.snapshot <- event
		return nil
	}
	s.stalledOnce.Do(func() { close(s.stalled) })
	select {
	case <-s.release:
		return nil
	case <-s.ctx.Done():
		return errors.New("stream closed")
	}
}

func (s *stallingDaemonStream) Receive() (*pb.OrchestratorCommand, error) {
	s.readerOnce.Do(func() { close(s.readerStarted) })
	select {
	case <-s.receiveGate:
		s.returnOnce.Do(func() { close(s.readerReturning) })
		return nil, s.receiveErr
	case <-s.ctx.Done():
		s.returnOnce.Do(func() { close(s.readerReturning) })
		return nil, s.ctx.Err()
	}
}

func (s *stallingDaemonStream) CloseRequest() error { return nil }

type stallingDaemonOpener struct {
	stream *stallingDaemonStream
}

func (o *stallingDaemonOpener) DaemonStream(ctx context.Context) bidirectionalStream {
	o.stream.ctx = ctx
	return o.stream
}

// scriptedEventSource hands subscribeDeltas a channel the test writes to, so a
// test controls exactly when the delta forwarder has an event to push onto
// outbound.
type scriptedEventSource struct {
	ch chan StreamEvent
}

func newScriptedEventSource(buffer int) *scriptedEventSource {
	return &scriptedEventSource{ch: make(chan StreamEvent, buffer)}
}

func (s *scriptedEventSource) Subscribe(context.Context) <-chan StreamEvent { return s.ch }

func sessionEvent(id string) StreamEvent {
	return StreamEvent{Session: &SessionEvent{
		Kind:    pb.SessionDelta_KIND_CREATED,
		Session: &pb.Session{Id: id},
	}}
}

func teardownTestClient(
	t *testing.T,
	clock *fakeClock,
	opener streamOpener,
	events EventSource,
	tp TokenProvider,
) *StreamClient {
	t.Helper()
	stores := emptySnapshotStores{}
	return NewStreamClient(StreamClientConfig{
		Opener: opener,
		Stores: StreamStores{
			Sessions: stores,
			Chats:    stores,
			Repos:    stores,
			Statuses: stores,
		},
		Events:           events,
		TokenProvider:    tp,
		Clock:            clock,
		CoalesceWindow:   10 * time.Millisecond,
		RefreshInterval:  time.Second,
		RefreshThreshold: 10 * time.Minute,
		Logger:           zerolog.New(io.Discard),
	})
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// advanceUntil steps the fake clock one refresh interval at a time until
// signal fires. The refresher registers its tick as a timer at least
// refreshInterval out, so waiting for such a timer before each Advance is what
// keeps a tick from being dropped on a clock nothing has armed yet.
func advanceUntil(t *testing.T, clock *fakeClock, signal <-chan struct{}, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case <-signal:
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		waitForTimerAtLeast(clock, 500*time.Millisecond, 200*time.Millisecond)
		clock.Advance(2 * time.Second)
		select {
		case <-signal:
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestOpenStreamDoesNotCloseOutboundWhileRefresherCanStillSend is the primary
// BOS-1274 regression (R1, R4): the token refresher must be joined before
// close(outbound), so it cannot reach `case outbound <- ev` on a closed
// channel.
//
// Each repetition holds the refresher inside Refresh — one step short of its
// outbound send — and holds the writer inside Send, which pins the unfixed
// teardown at its writer join, i.e. strictly after close(outbound). Releasing
// the refresher there is what lets it walk into the closed channel.
func TestOpenStreamDoesNotCloseOutboundWhileRefresherCanStillSend(t *testing.T) {
	rec := recordRecoveredPanics(t)
	for i := 0; i < teardownRepetitions; i++ {
		runRefresherTeardown(t)
	}
	rec.assertNoPanics(t)
}

func runRefresherTeardown(t *testing.T) {
	t.Helper()

	clock := newFakeClock()
	stream := newStallingDaemonStream()

	var refreshes atomic.Int32
	var gateOnce sync.Once
	atGate := make(chan struct{})
	releaseRefresh := make(chan struct{})
	tp := &fakeTokenProvider{
		token:     "tok",
		expiresAt: clock.Now().Add(time.Minute),
		refreshFn: func(context.Context) (string, error) {
			// First refresh emits an event so the writer picks it up and
			// stalls inside Send. Second refresh is held at the gate.
			if refreshes.Add(1) == 1 {
				return "tok-1", nil
			}
			gateOnce.Do(func() { close(atGate) })
			select {
			case <-releaseRefresh:
			case <-time.After(10 * time.Second):
			}
			return "tok-2", nil
		},
	}

	client := teardownTestClient(t, clock, &stallingDaemonOpener{stream: stream}, NoopEventSource{}, tp)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- client.openStream(ctx) }()

	select {
	case event := <-stream.snapshot:
		if event.GetSnapshot() == nil {
			t.Fatalf("first event = %T, want snapshot", event.GetEvent())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the handshake snapshot")
	}
	waitClosed(t, stream.readerStarted, "the command reader to start")

	advanceUntil(t, clock, stream.stalled, "the writer to stall inside Send")
	advanceUntil(t, clock, atGate, "the refresher to reach the gate inside Refresh")

	// Start the teardown. The writer is stalled inside Send, so an unfixed
	// teardown parks on its writer join — which sits after close(outbound).
	cancel()
	waitClosed(t, stream.readerReturning, "the command reader to return")

	close(releaseRefresh)
	close(stream.release)

	select {
	case <-errCh:
	case <-time.After(10 * time.Second):
		t.Fatal("openStream did not return after teardown")
	}
}

// TestOpenStreamDoesNotCloseOutboundWhileForwarderCanStillSend is the same
// regression for the second producer on outbound, the delta forwarder
// (R1, and the plan's second regression case).
//
// The forwarder is parked mid-send on a full outbound with more deltas still
// queued behind it. cancel() releases that parked send harmlessly, but the
// forwarder then loops straight back for the next queued delta — and an
// unfixed teardown has closed outbound by the time it gets there.
func TestOpenStreamDoesNotCloseOutboundWhileForwarderCanStillSend(t *testing.T) {
	rec := recordRecoveredPanics(t)
	for i := 0; i < teardownRepetitions; i++ {
		runForwarderTeardown(t)
	}
	rec.assertNoPanics(t)
}

func runForwarderTeardown(t *testing.T) {
	t.Helper()

	clock := newFakeClock()
	stream := newStallingDaemonStream()
	events := newScriptedEventSource(queuedDeltas)

	client := teardownTestClient(t, clock, &stallingDaemonOpener{stream: stream}, events, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- client.openStream(ctx) }()

	select {
	case <-stream.snapshot:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the handshake snapshot")
	}
	waitClosed(t, stream.readerStarted, "the command reader to start")

	parkForwarderOnFullOutbound(t, events, stream)

	cancel()
	waitClosed(t, stream.readerReturning, "the command reader to return")
	close(stream.release)

	select {
	case <-errCh:
	case <-time.After(10 * time.Second):
		t.Fatal("openStream did not return after teardown")
	}
}

// outboundAbsorbed is what the pipeline swallows before a send can park:
// outbound's buffered slots plus the event the stalled writer already took,
// plus the one the forwarder is parked on.
const outboundAbsorbed = outboundBufferSize + 2

// queuedDeltas is comfortably more than what the pipeline can absorb, so the
// forwarder parks mid-send with deltas to spare. Both constants are derived
// from outboundBufferSize rather than hardcoded: growing the production buffer
// past queuedDeltas would otherwise drain every queued delta, satisfy
// parkForwarderOnFullOutbound's wait loop instantly, and leave the two
// crash-regression tests below parking nothing while still reporting green.
const queuedDeltas = 2 * outboundAbsorbed

// parkForwarderOnFullOutbound fills outbound, stalls the writer inside Send,
// and returns once the delta forwarder is parked mid-send with deltas still
// queued behind it.
func parkForwarderOnFullOutbound(t *testing.T, events *scriptedEventSource, stream *stallingDaemonStream) {
	t.Helper()
	for i := 0; i < queuedDeltas; i++ {
		events.ch <- sessionEvent(fmt.Sprintf("delta-%d", i))
	}
	waitClosed(t, stream.stalled, "the writer to stall inside Send")

	deadline := time.Now().Add(5 * time.Second)
	for len(events.ch) > queuedDeltas-outboundAbsorbed {
		if time.Now().After(deadline) {
			t.Fatalf("forwarder never parked on a full outbound; %d deltas still queued", len(events.ch))
		}
		time.Sleep(time.Millisecond)
	}
}

// TestOpenStreamTeardownCompletesWithForwarderParkedOnFullOutbound pins R2:
// joining the producers before the close must not deadlock. The delta
// forwarder is parked mid-send on a completely full outbound and the writer
// never drains it, so the only thing that can release the forwarder is the
// teardown's own cancel(). A producer added later without a ctx.Done() case
// hangs this test instead of panicking.
func TestOpenStreamTeardownCompletesWithForwarderParkedOnFullOutbound(t *testing.T) {
	rec := recordRecoveredPanics(t)

	clock := newFakeClock()
	stream := newStallingDaemonStream()
	events := newScriptedEventSource(queuedDeltas)

	client := teardownTestClient(t, clock, &stallingDaemonOpener{stream: stream}, events, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- client.openStream(ctx) }()

	select {
	case <-stream.snapshot:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the handshake snapshot")
	}
	waitClosed(t, stream.readerStarted, "the command reader to start")

	parkForwarderOnFullOutbound(t, events, stream)

	// Tear down without ever releasing the writer: Send returns only because
	// the stream context is cancelled, so outbound is never drained.
	cancel()

	select {
	case <-errCh:
	case <-time.After(10 * time.Second):
		t.Fatal("openStream deadlocked tearing down with a producer parked on a full outbound")
	}
	rec.assertNoPanics(t)
}

// TestOpenStreamReturnsRefreshErrorRacingReaderEOF pins R3 and KTD3: a refresh
// error raised while the command reader is returning for an unrelated reason
// (EOF here) must still be the error openStream reports. Draining refreshErrCh
// before the refresher is joined loses it and reports the bare EOF instead.
func TestOpenStreamReturnsRefreshErrorRacingReaderEOF(t *testing.T) {
	rec := recordRecoveredPanics(t)

	clock := newFakeClock()
	stream := newStallingDaemonStream()
	stream.receiveErr = io.EOF

	refreshErr := errors.New("workos down")
	atGate := make(chan struct{})
	releaseRefresh := make(chan struct{})
	var gateOnce sync.Once
	tp := &fakeTokenProvider{
		token: "tok",
		// Already expired, so runTokenRefresher returns the error rather
		// than retrying on the next tick.
		expiresAt: clock.Now().Add(-time.Second),
		refreshFn: func(context.Context) (string, error) {
			gateOnce.Do(func() { close(atGate) })
			select {
			case <-releaseRefresh:
			case <-time.After(10 * time.Second):
			}
			return "", refreshErr
		},
	}

	client := teardownTestClient(t, clock, &stallingDaemonOpener{stream: stream}, NoopEventSource{}, tp)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- client.openStream(ctx) }()

	select {
	case <-stream.snapshot:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the handshake snapshot")
	}
	waitClosed(t, stream.readerStarted, "the command reader to start")
	advanceUntil(t, clock, atGate, "the refresher to reach the gate inside Refresh")

	// The reader returns EOF while the refresh is still in flight.
	close(stream.receiveGate)
	waitClosed(t, stream.readerReturning, "the command reader to return")
	close(releaseRefresh)
	close(stream.release)

	select {
	case err := <-errCh:
		if !errors.Is(err, refreshErr) {
			t.Fatalf("openStream error = %v, want wrapped %v", err, refreshErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("openStream did not return after teardown")
	}
	rec.assertNoPanics(t)
}

// TestOpenStreamIdleTeardownReturnsReaderError is the unchanged-behaviour
// case (R5): with nothing parked and an empty buffer, teardown still reports
// the reader's error.
func TestOpenStreamIdleTeardownReturnsReaderError(t *testing.T) {
	rec := recordRecoveredPanics(t)

	clock := newFakeClock()
	stream := newStallingDaemonStream()
	readErr := errors.New("peer reset")
	stream.receiveErr = readErr

	client := teardownTestClient(t, clock, &stallingDaemonOpener{stream: stream}, NoopEventSource{}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- client.openStream(ctx) }()

	select {
	case <-stream.snapshot:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the handshake snapshot")
	}
	waitClosed(t, stream.readerStarted, "the command reader to start")
	close(stream.receiveGate)

	select {
	case err := <-errCh:
		if !errors.Is(err, readErr) {
			t.Fatalf("openStream error = %v, want wrapped %v", err, readErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("openStream did not return after teardown")
	}
	rec.assertNoPanics(t)
}
