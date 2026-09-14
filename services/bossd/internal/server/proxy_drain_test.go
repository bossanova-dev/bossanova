package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// --- BOS-888: shutdown drains in-flight proxied streams ---
//
// Agents route every model request through this loopback proxy, so the proxy's
// lifetime IS the agent's connection lifetime. A daemon restart that tears the
// proxy down mid-turn severs the SSE stream ("Connection lost mid-response").
// These tests pin that Shutdown waits for in-flight streams under its own
// budget, reports whether it drained or hit the deadline, and does not release
// the listening socket before that drain resolves.

// recordingListener wraps a net.Listener and timestamps every Close, so a test
// can assert WHEN the socket was released relative to the drain resolving.
// Shutdown closes the listener twice in the normal path (http.Server closes its
// tracked listener as it stops accepting, then ProxyServer.Shutdown closes it
// explicitly for the BOS-409 deterministic re-bind); the LAST close is the
// explicit one whose ordering this test cares about.
type recordingListener struct {
	net.Listener
	mu       sync.Mutex
	closes   []time.Time
	closeErr error
}

func (l *recordingListener) Close() error {
	l.mu.Lock()
	l.closes = append(l.closes, time.Now())
	injected := l.closeErr
	l.mu.Unlock()
	err := l.Listener.Close()
	if injected != nil {
		return injected
	}
	return err
}

// failCloses makes every later Close report err while still releasing the real
// socket, so a test can produce the "drained cleanly, listener close was noisy"
// shape http.Server.Shutdown surfaces as a non-nil return.
func (l *recordingListener) failCloses(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closeErr = err
}

func (l *recordingListener) lastClose() (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.closes) == 0 {
		return time.Time{}, false
	}
	return l.closes[len(l.closes)-1], true
}

// heldStreamUpstream is a fake Anthropic upstream that opens an SSE response,
// writes a first frame, then holds the stream open until release is closed
// before writing a final frame. It models a Claude turn in flight.
// release is CLOSED to finish every held handler at once. releaseOne is
// RECEIVED from, so a single send finishes exactly one — which is how a test
// makes the in-flight count fall by one while the rest stay held (BOS-1219).
type heldStreamUpstream struct {
	srv        *httptest.Server
	opened     chan struct{}
	release    chan struct{}
	releaseOne chan struct{}
	openOnce   sync.Once
}

// The first frame is a content_block_delta on purpose: it is decisive for the
// proxy's opening-frame rate-limit peek, so the response starts streaming to the
// client immediately — the mid-turn "content already shipping" shape the
// incident actually cut. An undecided opening frame (message_start) would leave
// the peek buffering and never reach the streaming loop under test.
const (
	drainTestFirstFrame = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n"
	drainTestLastFrame  = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

func newHeldStreamUpstream(t *testing.T) *heldStreamUpstream {
	t.Helper()
	u := &heldStreamUpstream{
		opened:     make(chan struct{}),
		release:    make(chan struct{}),
		releaseOne: make(chan struct{}),
	}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		if _, err := io.WriteString(w, drainTestFirstFrame); err != nil {
			return
		}
		_ = rc.Flush()
		u.openOnce.Do(func() { close(u.opened) })
		select {
		case <-u.release:
		case <-u.releaseOne:
		case <-r.Context().Done():
			return
		case <-time.After(30 * time.Second):
			return
		}
		if _, err := io.WriteString(w, drainTestLastFrame); err != nil {
			return
		}
		_ = rc.Flush()
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// startDrainProxy binds a real loopback ProxyServer pointed at upstream, wraps
// its listener so closes are observable, and serves it. It returns the server,
// the recording listener, and the proxy's base URL.
func startDrainProxy(t *testing.T, upstream string) (*ProxyServer, *recordingListener, string) {
	t.Helper()
	return startDrainProxyWithLogger(t, upstream, zerolog.Nop(), 0)
}

// startDrainProxyWithLogger is startDrainProxy with the logger and the drain
// progress cadence chosen by the caller. Both are set BEFORE Serve so a test
// that wants to read the drain's log output does not race the handler
// goroutines reading the same fields.
//
// An optional StreamRecorder wires the durable in-flight stream record (BOS-890)
// so a test can observe the seal; omitted, the proxy runs without one exactly as
// the other drain tests do.
func startDrainProxyWithLogger(t *testing.T, upstream string, logger zerolog.Logger, progressInterval time.Duration, streams ...StreamRecorder) (*ProxyServer, *recordingListener, string) {
	t.Helper()
	cfg := ProxyServerConfig{
		Failover: &fakeFailover{},
		Logger:   logger,
		Upstream: upstream,
	}
	if len(streams) == 1 {
		cfg.Streams = streams[0]
	}
	ps, err := NewProxyServer(cfg)
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}
	ps.drainProgressInterval = progressInterval
	if err := ps.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	rec := &recordingListener{Listener: ps.listener}
	ps.listener = rec
	go func() { _ = ps.Serve() }()
	return ps, rec, fmt.Sprintf("http://127.0.0.1:%d", ps.Port())
}

// openStreamThroughProxy issues a proxied request and returns the response once
// its first SSE frame has arrived, so the caller knows the stream is genuinely
// in flight before it triggers a shutdown.
func openStreamThroughProxy(t *testing.T, ps *ProxyServer, base string) *http.Response {
	t.Helper()
	token := ps.TokenForSession("sess-drain")
	if token == "" {
		t.Fatal("TokenForSession returned an empty token")
	}
	return openStreamWithToken(t, base, token)
}

// openStreamWithToken is openStreamThroughProxy for a token the caller minted —
// a CHAT token, when the test needs the durable stream record exercised, since
// only chat-scoped targets are Entered and Left.
func openStreamWithToken(t *testing.T, base, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/s/"+token+"/v1/messages", strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxied request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("proxied request status = %d, want 200 (body %q)", resp.StatusCode, body)
	}
	return resp
}

// waitForInFlight blocks until the proxy reports want in-flight streams.
func waitForInFlight(t *testing.T, ps *ProxyServer, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ps.InFlightStreams() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("InFlightStreams() = %d, want %d within 5s", ps.InFlightStreams(), want)
}

// TestProxyShutdown_InFlightSSEStreamSurvivesShutdown is the regression test for
// the incident itself: a Claude SSE stream in flight when the daemon shuts down
// must receive its remaining bytes rather than a connection reset.
func TestProxyShutdown_InFlightSSEStreamSurvivesShutdown(t *testing.T) {
	upstream := newHeldStreamUpstream(t)
	ps, _, base := startDrainProxy(t, upstream.srv.URL)

	resp := openStreamThroughProxy(t, ps, base)
	defer func() { _ = resp.Body.Close() }()

	<-upstream.opened
	waitForInFlight(t, ps, 1)

	// Read the first frame so the client leg is genuinely mid-stream.
	br := bufio.NewReader(resp.Body)
	got, err := readFrame(br)
	if err != nil {
		t.Fatalf("read first frame: %v", err)
	}
	if got != drainTestFirstFrame {
		t.Fatalf("first frame = %q, want %q", got, drainTestFirstFrame)
	}

	// Shut down mid-stream with a realistic drain budget, then let the upstream
	// finish. The drain must hold the connection open for the rest of the turn.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type result struct {
		outcome DrainOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		o, err := ps.Shutdown(ctx)
		done <- result{o, err}
	}()

	// Give Shutdown a moment to begin draining, then release the upstream.
	time.Sleep(50 * time.Millisecond)
	close(upstream.release)

	last, err := readFrame(br)
	if err != nil {
		t.Fatalf("in-flight stream was severed by shutdown instead of drained: %v", err)
	}
	if last != drainTestLastFrame {
		t.Fatalf("final frame = %q, want %q", last, drainTestLastFrame)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Shutdown err = %v, want nil (the stream finished inside the budget)", r.err)
		}
		if !r.outcome.Drained {
			t.Error("outcome.Drained = false, want true")
		}
		if r.outcome.InFlightAtStart != 1 {
			t.Errorf("outcome.InFlightAtStart = %d, want 1", r.outcome.InFlightAtStart)
		}
		if r.outcome.InFlightAtEnd != 0 {
			t.Errorf("outcome.InFlightAtEnd = %d, want 0", r.outcome.InFlightAtEnd)
		}
		// A drained resolution has no non-drained reason to report (BOS-1219).
		if r.outcome.StopReason != DrainStopNone {
			t.Errorf("outcome.StopReason = %q, want %q on a clean drain", r.outcome.StopReason, DrainStopNone)
		}
		if r.outcome.StallWindow != 0 {
			t.Errorf("outcome.StallWindow = %v, want 0 on a clean drain", r.outcome.StallWindow)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return within 10s after the stream finished")
	}
}

// TestProxyShutdown_StreamSurvivesPastTheSharedFiveSecondBudget pins the number
// that made the incident possible. Before BOS-888 the proxy shut down on the
// same 5s context as the gRPC and hook servers, so any turn still streaming at
// t+5s was cut. This holds a stream open past that mark and requires it to
// finish, which is only true when the drain runs on a budget of its own.
//
// It costs real wall time, so it is skipped in -short runs; the ordering and
// budget guards in services/bossd/cmd cover the same wiring structurally.
func TestProxyShutdown_StreamSurvivesPastTheSharedFiveSecondBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("holds a stream open for >5s; covered structurally by the cmd budget test")
	}

	const oldSharedBudget = 5 * time.Second

	upstream := newHeldStreamUpstream(t)
	ps, _, base := startDrainProxy(t, upstream.srv.URL)

	resp := openStreamThroughProxy(t, ps, base)
	defer func() { _ = resp.Body.Close() }()

	<-upstream.opened
	waitForInFlight(t, ps, 1)

	br := bufio.NewReader(resp.Body)
	if _, err := readFrame(br); err != nil {
		t.Fatalf("read first frame: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	started := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := ps.Shutdown(ctx)
		done <- err
	}()

	// Keep the turn in flight well past the old shared budget before finishing it.
	time.Sleep(oldSharedBudget + 500*time.Millisecond)
	close(upstream.release)

	last, err := readFrame(br)
	if err != nil {
		t.Fatalf("stream in flight at t+%v was severed instead of drained: %v", oldSharedBudget, err)
	}
	if last != drainTestLastFrame {
		t.Fatalf("final frame = %q, want %q", last, drainTestLastFrame)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown err = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return after the stream finished")
	}

	if elapsed := time.Since(started); elapsed <= oldSharedBudget {
		t.Fatalf("drain returned after %v, so it never outlived the old %v budget and proves nothing", elapsed, oldSharedBudget)
	}
}

// TestProxyShutdown_ZeroBudgetCutsTheStream is the negative control for the test
// above: with no drain budget at all, the old behaviour reproduces — the stream
// is cut and Shutdown reports a deadline rather than a drain. If this passes as
// "drained" the drain test above proves nothing.
func TestProxyShutdown_ZeroBudgetCutsTheStream(t *testing.T) {
	upstream := newHeldStreamUpstream(t)
	ps, _, base := startDrainProxy(t, upstream.srv.URL)
	defer close(upstream.release)

	resp := openStreamThroughProxy(t, ps, base)
	defer func() { _ = resp.Body.Close() }()

	<-upstream.opened
	waitForInFlight(t, ps, 1)

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	outcome, err := ps.Shutdown(ctx)
	if err == nil {
		t.Fatal("Shutdown err = nil, want a deadline error with a stream still in flight")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown err = %v, want context.DeadlineExceeded", err)
	}
	if outcome.Drained {
		t.Error("outcome.Drained = true, want false when the budget expired")
	}
	if outcome.InFlightAtStart != 1 {
		t.Errorf("outcome.InFlightAtStart = %d, want 1", outcome.InFlightAtStart)
	}
	if outcome.InFlightAtEnd != 1 {
		t.Errorf("outcome.InFlightAtEnd = %d, want 1 (the stream was cut, not drained)", outcome.InFlightAtEnd)
	}
	// The deadline must win: this is an expired budget, never a stall. A drain
	// with no budget at all cannot have observed a window of flat samples, and
	// reporting it as a stall would send drainFailoverProxy down the early-bail
	// log line for a shutdown that simply ran out of time (BOS-1219).
	if outcome.StopReason != DrainStopBudgetExpired {
		t.Errorf("outcome.StopReason = %q, want %q", outcome.StopReason, DrainStopBudgetExpired)
	}

	// "Cut" has to be true of the socket, not just of the report.
	// http.Server.Shutdown returns ctx.Err() on expiry and leaves active
	// connections running, so without the explicit srv.Close() on the expired
	// branch this read would block until the upstream released and then succeed —
	// a stream the daemon had already logged as severed still delivering bytes.
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Error("reading the cut stream succeeded; the expired drain left the connection alive")
	}
}

// TestProxyShutdown_IdleReturnsImmediately pins the fast path: a restart with
// nothing in flight must not pay the drain budget.
func TestProxyShutdown_IdleReturnsImmediately(t *testing.T) {
	upstream := newHeldStreamUpstream(t)
	ps, _, _ := startDrainProxy(t, upstream.srv.URL)

	const drainBudget = 120 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), drainBudget)
	defer cancel()
	started := time.Now()
	outcome, err := ps.Shutdown(ctx)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Shutdown err = %v, want nil when idle", err)
	}
	if !outcome.Drained {
		t.Error("outcome.Drained = false, want true when idle")
	}
	if outcome.InFlightAtStart != 0 {
		t.Errorf("outcome.InFlightAtStart = %d, want 0", outcome.InFlightAtStart)
	}
	if outcome.StopReason != DrainStopNone {
		t.Errorf("outcome.StopReason = %q, want %q when idle", outcome.StopReason, DrainStopNone)
	}
	// A sixtieth of the drain budget: paying that budget out is exactly the failure this pins.
	if elapsed > drainBudget/60 {
		t.Errorf("idle Shutdown took %v, want it to return promptly rather than wait out the budget", elapsed)
	}
}

// TestProxyShutdown_DrainedDespiteListenerCloseError pins what Drained actually
// answers: "did the in-flight streams finish?", NOT "did Shutdown return nil".
// http.Server.Shutdown closes its listeners first and returns that close error
// even when the wait afterwards drained every connection cleanly, so deriving
// Drained from `err == nil` makes the daemon log drained=false for a shutdown
// that cut nothing — a severed agent turn reported that never happened.
func TestProxyShutdown_DrainedDespiteListenerCloseError(t *testing.T) {
	upstream := newHeldStreamUpstream(t)
	ps, rec, base := startDrainProxy(t, upstream.srv.URL)

	// Run one request to completion. That both proves Serve has registered the
	// listener with http.Server (so Shutdown will close it, and can therefore
	// report the injected failure) and leaves nothing in flight to cut.
	resp := openStreamThroughProxy(t, ps, base)
	close(upstream.release)
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("drain response body: %v", err)
	}
	_ = resp.Body.Close()
	waitForInFlight(t, ps, 0)

	closeErr := errors.New("use of closed network connection")
	rec.failCloses(closeErr)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	outcome, err := ps.Shutdown(ctx)

	if !errors.Is(err, closeErr) {
		t.Fatalf("Shutdown err = %v, want the injected listener-close error", err)
	}
	if !outcome.Drained {
		t.Error("outcome.Drained = false, want true: the listener close errored but nothing was cut")
	}
	if outcome.InFlightAtEnd != 0 {
		t.Errorf("outcome.InFlightAtEnd = %d, want 0", outcome.InFlightAtEnd)
	}
	// The case that must never be confused with a cut: nothing was severed, so
	// there is no non-drained reason even though Shutdown returned an error.
	if outcome.StopReason != DrainStopNone {
		t.Errorf("outcome.StopReason = %q, want %q: the listener close errored but nothing was cut", outcome.StopReason, DrainStopNone)
	}
}

// TestProxyShutdown_ListenerClosedOnlyAfterDrainResolves pins that the explicit
// BOS-409 listener close — the one that makes the fixed-port re-bind
// deterministic — runs AFTER the drain resolves, never before it. Closing early
// would discard the drained-vs-deadline distinction the outcome reports.
func TestProxyShutdown_ListenerClosedOnlyAfterDrainResolves(t *testing.T) {
	upstream := newHeldStreamUpstream(t)
	ps, rec, base := startDrainProxy(t, upstream.srv.URL)

	resp := openStreamThroughProxy(t, ps, base)
	defer func() { _ = resp.Body.Close() }()

	<-upstream.opened
	waitForInFlight(t, ps, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_, _ = ps.Shutdown(ctx)
		close(done)
	}()

	// While the stream is still in flight the drain has not resolved.
	time.Sleep(100 * time.Millisecond)
	releasedAt := time.Now()
	close(upstream.release)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return within 10s")
	}

	last, ok := rec.lastClose()
	if !ok {
		t.Fatal("listener was never closed; the BOS-409 fixed-port re-bind guarantee is lost")
	}
	if last.Before(releasedAt) {
		t.Fatalf("listener's final close at %v preceded the drain resolving (stream released at %v)", last, releasedAt)
	}
}

// TestProxyShutdown_TracksInFlightStreams pins the counter the daemon logs, in
// both directions: it rises while a stream is being proxied and falls back to
// zero once it completes.
func TestProxyShutdown_TracksInFlightStreams(t *testing.T) {
	upstream := newHeldStreamUpstream(t)
	ps, _, base := startDrainProxy(t, upstream.srv.URL)
	defer func() { _, _ = ps.Shutdown(context.Background()) }()

	if got := ps.InFlightStreams(); got != 0 {
		t.Fatalf("InFlightStreams() = %d before any request, want 0", got)
	}

	resp := openStreamThroughProxy(t, ps, base)
	<-upstream.opened
	waitForInFlight(t, ps, 1)

	close(upstream.release)
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("drain response body: %v", err)
	}
	_ = resp.Body.Close()
	waitForInFlight(t, ps, 0)
}

// The drain-progress ticker logs from its own goroutine while the test reads,
// so these two use the package's mutex-guarded syncBuffer sink rather than a
// bare bytes.Buffer.
//
// TestProxyShutdown_LogsDrainProgressWhileWaiting exercises the ticker branch of
// startDrainProgressLog with a PINNED cadence, so it proves the branch works —
// not that production ever reaches it. Reaching it is a function of the derived
// cadence and is pinned separately by
// TestProxyShutdown_LogsDrainProgressAtTheDerivedCadence below, which injects no
// interval at all; do not read this test as covering that.
func TestProxyShutdown_LogsDrainProgressWhileWaiting(t *testing.T) {
	logs := &syncBuffer{}
	upstream := newHeldStreamUpstream(t)
	ps, _, base := startDrainProxyWithLogger(t, upstream.srv.URL, zerolog.New(logs), 10*time.Millisecond)

	resp := openStreamThroughProxy(t, ps, base)
	defer func() { _ = resp.Body.Close() }()

	<-upstream.opened
	waitForInFlight(t, ps, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_, _ = ps.Shutdown(ctx)
		close(done)
	}()

	// Hold the stream open long enough for several ticks to land, then let the
	// turn finish so Shutdown returns.
	time.Sleep(150 * time.Millisecond)
	close(upstream.release)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Shutdown did not return after the stream was released")
	}

	out := logs.String()
	if !strings.Contains(out, "failover proxy: draining in-flight agent streams before shutdown") {
		t.Errorf("drain start was not logged; got %q", out)
	}
	if !strings.Contains(out, "failover proxy: still draining in-flight agent streams") {
		t.Errorf("drain progress was never logged; got %q", out)
	}
}

// TestProxyShutdown_LogsDrainProgressAtTheDerivedCadence pins the cadence a real
// daemon gets: it injects NO drainProgressInterval, so the ticker is armed from
// whatever drainProgressCadence derives from the ctx budget. This is the
// regression test for the cadence being a fixed 15s while the shipped drain
// budget was also 15s — the ticker was then armed for exactly the moment the ctx
// expired and the periodic line could never print outside tests that pinned a
// short interval.
func TestProxyShutdown_LogsDrainProgressAtTheDerivedCadence(t *testing.T) {
	logs := &syncBuffer{}
	upstream := newHeldStreamUpstream(t)
	// progressInterval 0 == production: derive the cadence from the budget.
	ps, _, base := startDrainProxyWithLogger(t, upstream.srv.URL, zerolog.New(logs), 0)

	resp := openStreamThroughProxy(t, ps, base)
	defer func() { _ = resp.Body.Close() }()

	<-upstream.opened
	waitForInFlight(t, ps, 1)

	// A 600ms budget derives a 200ms cadence, so a drain that spends its whole
	// budget prints twice before the deadline cuts the stream.
	const budget = 600 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if got := ps.drainProgressCadence(ctx); got >= budget {
		t.Fatalf("derived cadence %v >= budget %v: the ticker can never fire inside the drain", got, budget)
	}

	// Never release the upstream: this is the timed-out drain, the one case
	// where the operator most needs to see progress before the cut.
	if _, err := ps.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown err = %v, want context.DeadlineExceeded", err)
	}
	close(upstream.release)

	if out := logs.String(); !strings.Contains(out, "failover proxy: still draining in-flight agent streams") {
		t.Errorf("drain progress was never logged at the derived cadence; got %q", out)
	}
}

// TestProxyShutdown_LogsNoProgressWhenIdle is the negative control for the test
// above: with nothing in flight the drain must start no ticker and say nothing,
// which is what keeps an ordinary restart exactly as cheap as before BOS-888.
func TestProxyShutdown_LogsNoProgressWhenIdle(t *testing.T) {
	logs := &syncBuffer{}
	upstream := newHeldStreamUpstream(t)
	ps, _, _ := startDrainProxyWithLogger(t, upstream.srv.URL, zerolog.New(logs), 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := ps.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown on an idle proxy: %v", err)
	}

	if out := logs.String(); strings.Contains(out, "draining in-flight agent streams") {
		t.Errorf("idle shutdown logged a drain; got %q", out)
	}
	// The negative control for the stall detector too: an idle drain starts no
	// sampler at all, so it can never bail out early (BOS-1219).
	if out := logs.String(); strings.Contains(out, "stopped falling") {
		t.Errorf("idle shutdown logged a stall bail-out; got %q", out)
	}
}

// --- BOS-1219: the drain stops waiting once the count has stopped falling ---
//
// The measured defect: an SSE agent turn holds the in-flight count at 1 for
// minutes, so every shutdown paid its entire 15s budget and severed the stream
// anyway. These pin that a count which has demonstrably stopped falling ends the
// drain early, that a count which is still falling does not, and that the early
// path keeps every guarantee the expiry path already had.
//
// They run against the REAL drainStallSampleInterval/drainStallSamples rather
// than a pinned test-only cadence: drainProgressInterval exists precisely so a
// cadence can be pinned, and a stall test that pinned one would pass at a cadence
// production never sees. Each therefore holds a stream for one drainStallWindow
// (8s), past the 5s guard, so each carries a -short skip naming that cost.

// sealOrderRecorder timestamps Seal and every Leave. The BOS-890 ordering it
// exists to observe is that Seal pins the record BEFORE srv.Close cuts the
// handlers, because each cut handler runs its own deferred Leave on the way out
// and would otherwise empty the record the seal exists to pin.
type sealOrderRecorder struct {
	mu     sync.Mutex
	sealed time.Time
	leaves []time.Time
}

func (r *sealOrderRecorder) Enter(string) {}

func (r *sealOrderRecorder) Leave(string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leaves = append(r.leaves, time.Now())
}

func (r *sealOrderRecorder) Seal() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed.IsZero() {
		r.sealed = time.Now()
	}
}

// waitForLeave returns the seal time and the recorded Leave times once at least
// one Leave has landed. The cut handler unwinds on its own goroutine, so a bare
// read the instant Shutdown returns would see an empty slice and make the
// ordering assertion vacuous rather than failing.
func (r *sealOrderRecorder) waitForLeave(t *testing.T) (time.Time, []time.Time) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.mu.Lock()
		sealed, leaves := r.sealed, append([]time.Time(nil), r.leaves...)
		r.mu.Unlock()
		if len(leaves) > 0 {
			return sealed, leaves
		}
		if !time.Now().Before(deadline) {
			t.Fatal("no Leave was recorded within 5s, so the seal ordering is untested: the stream was never entered into the durable record")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestProxyShutdown_StalledCountEndsTheDrainEarly is the ticket itself: a drain
// whose in-flight count never falls must resolve on the stall rather than on the
// deadline, and must say so.
func TestProxyShutdown_StalledCountEndsTheDrainEarly(t *testing.T) {
	if testing.Short() {
		t.Skip("holds a stream flat for the real 8s drainStallWindow rather than pinning a shorter test-only cadence")
	}

	logs := &syncBuffer{}
	rec := &sealOrderRecorder{}
	upstream := newHeldStreamUpstream(t)
	// No pinned progress interval: production's derived cadence.
	ps, _, base := startDrainProxyWithLogger(t, upstream.srv.URL, zerolog.New(logs), 0, rec)
	defer close(upstream.release)

	// A CHAT token: only chat-scoped targets are Entered into and Left from the
	// durable stream record, so a session-scoped one would make the seal-ordering
	// half of this test vacuous.
	token := ps.TokenForChat("sess-drain", "agent-drain", "acct-drain")
	if token == "" {
		t.Fatal("TokenForChat returned an empty token")
	}
	resp := openStreamWithToken(t, base, token)
	defer func() { _ = resp.Body.Close() }()
	<-upstream.opened
	waitForInFlight(t, ps, 1)

	// A budget far longer than the stall window, so "resolved early" is
	// unambiguous rather than a wall-clock judgement: before this change the
	// drain paid all of it and cut the stream anyway.
	const budget = 30 * time.Second
	if budget <= drainStallWindow {
		t.Fatalf("budget %v is not longer than the stall window %v; the deadline would win and this test would prove nothing", budget, drainStallWindow)
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("drain ctx has no deadline")
	}

	outcome, err := ps.Shutdown(ctx)
	resolvedAt := time.Now()

	if !resolvedAt.Before(deadline) {
		t.Fatalf("drain resolved at %v, at or after its deadline %v: it waited the budget out", resolvedAt, deadline)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown err = %v, want context.Canceled from the stall bail-out", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown err = %v, want a cancel rather than an expired budget", err)
	}
	if outcome.Drained {
		t.Error("outcome.Drained = true, want false: the stream was cut, not drained")
	}
	if outcome.StopReason != DrainStopStalled {
		t.Errorf("outcome.StopReason = %q, want %q", outcome.StopReason, DrainStopStalled)
	}
	if outcome.StallWindow != drainStallWindow {
		t.Errorf("outcome.StallWindow = %v, want %v", outcome.StallWindow, drainStallWindow)
	}
	// InFlightAtEnd still names the streams the resolution cut, exactly as on the
	// expiry path — that number is what the operator reads.
	if outcome.InFlightAtEnd != 1 {
		t.Errorf("outcome.InFlightAtEnd = %d, want 1 (the cut stream)", outcome.InFlightAtEnd)
	}
	// And the cut is true of the socket, not just of the report.
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Error("reading the cut stream succeeded; the early bail-out left the connection alive")
	}
	if out := logs.String(); !strings.Contains(out, "in-flight stream count stopped falling") {
		t.Errorf("the early bail-out was not logged; got %q", out)
	}

	// The early bail-out takes the SAME non-drained path as an expired budget
	// rather than inventing a softer one: the durable stream record is sealed
	// BEFORE srv.Close cuts the handlers, whose own deferred Leave would
	// otherwise empty the record the seal exists to pin (BOS-890). The cut
	// handler unwinds on its own goroutine, so the Leave lands after Shutdown
	// returns and has to be waited for rather than read.
	sealed, leaves := rec.waitForLeave(t)
	if sealed.IsZero() {
		t.Fatal("the durable stream record was never sealed on the early-bail path; a severed stream would be recovered as nothing")
	}
	for i, at := range leaves {
		if at.Before(sealed) {
			t.Fatalf("Leave %d ran at %v, before the seal at %v: the cut handlers emptied the record the seal exists to pin", i, at, sealed)
		}
	}
}

// TestProxyShutdown_StreamReleasedInsideTheWindowStillDrains is the R2 negative
// control, and the one that catches a stall window set so short that ordinary
// progress reads as a stall: a stream that finishes inside the window must drain
// normally, with no early bail-out at all.
func TestProxyShutdown_StreamReleasedInsideTheWindowStillDrains(t *testing.T) {
	const releaseAfter = 200 * time.Millisecond
	if releaseAfter >= drainStallWindow {
		t.Fatalf("release delay %v is not inside the stall window %v; this control proves nothing", releaseAfter, drainStallWindow)
	}

	upstream := newHeldStreamUpstream(t)
	ps, _, base := startDrainProxy(t, upstream.srv.URL)

	resp := openStreamThroughProxy(t, ps, base)
	defer func() { _ = resp.Body.Close() }()
	<-upstream.opened
	waitForInFlight(t, ps, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type result struct {
		outcome DrainOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		o, err := ps.Shutdown(ctx)
		done <- result{o, err}
	}()

	time.Sleep(releaseAfter)
	close(upstream.release)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Shutdown err = %v, want nil: the stream finished well inside the stall window", r.err)
		}
		if !r.outcome.Drained {
			t.Error("outcome.Drained = false, want true")
		}
		if r.outcome.StopReason != DrainStopNone {
			t.Errorf("outcome.StopReason = %q, want %q: nothing was cut", r.outcome.StopReason, DrainStopNone)
		}
		if r.outcome.InFlightAtEnd != 0 {
			t.Errorf("outcome.InFlightAtEnd = %d, want 0", r.outcome.InFlightAtEnd)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Shutdown did not return after the stream finished")
	}
}

// TestProxyShutdown_AFallingCountResetsTheStallWindow pins that the window is
// measured from the LAST observed progress, not from the drain's start. Without
// the reset a drain that is still finishing streams would be cut at
// drainStallWindow regardless of how much progress it was making.
func TestProxyShutdown_AFallingCountResetsTheStallWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("holds two streams across the real 8s drainStallWindow to prove the window restarts at the fall")
	}

	const holdBoth = 600 * time.Millisecond

	upstream := newHeldStreamUpstream(t)
	ps, _, base := startDrainProxy(t, upstream.srv.URL)
	defer close(upstream.release)

	first := openStreamThroughProxy(t, ps, base)
	defer func() { _ = first.Body.Close() }()
	second := openStreamThroughProxy(t, ps, base)
	defer func() { _ = second.Body.Close() }()
	<-upstream.opened
	waitForInFlight(t, ps, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	type result struct {
		outcome    DrainOutcome
		resolvedAt time.Time
	}
	done := make(chan result, 1)
	go func() {
		o, _ := ps.Shutdown(ctx)
		done <- result{o, time.Now()}
	}()

	// Hold both, then finish exactly one so the count falls 2 -> 1 and then holds.
	time.Sleep(holdBoth)
	// fellAt is read BEFORE the release, so it is a sound LOWER bound on when the
	// detector could have observed the fall: the decrement cannot precede the send
	// that causes it. Reading it after waitForInFlight instead would put fellAt
	// strictly after the decrement — by up to that helper's 2ms poll granularity
	// plus scheduler latency — so a sample landing inside that gap would reset the
	// window fractionally BEFORE fellAt and fail the assertion below on a correct
	// implementation. The assertion keeps its full strength either way: a window
	// measured from the drain's start would fire at ~holdBoth+drainStallWindow,
	// which is still short of fellAt+drainStallWindow.
	fellAt := time.Now()
	upstream.releaseOne <- struct{}{}
	waitForInFlight(t, ps, 1)

	select {
	case r := <-done:
		if r.outcome.StopReason != DrainStopStalled {
			t.Fatalf("outcome.StopReason = %q, want %q; this test no longer exercises the stall path", r.outcome.StopReason, DrainStopStalled)
		}
		// The decisive assertion: a window measured from the drain's start would
		// have fired drainStallWindow after it began, i.e. BEFORE this point.
		if gap := r.resolvedAt.Sub(fellAt); gap < drainStallWindow {
			t.Fatalf("drain resolved %v after the count fell, want >= %v: the fall did not reset the window", gap, drainStallWindow)
		}
		if r.outcome.InFlightAtEnd != 1 {
			t.Errorf("outcome.InFlightAtEnd = %d, want 1", r.outcome.InFlightAtEnd)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Shutdown did not return")
	}
}

// TestProxyShutdown_StalledDrainStillJoinsInFlightRepairJobs is the R5a control,
// and it guards the single easiest way to implement this ticket incorrectly while
// every other test in the package stays green.
//
// waitRepairJobs(ctx) runs immediately after srv.Shutdown(ctx) and selects on the
// SAME ctx. Ending the drain early by cancelling the context Shutdown was handed
// would therefore also abandon the BOS-982 in-flight pane-repair join — on
// exactly the busy hosts where a repair is most likely to be running, and
// silently. The detector must cancel a child scoped to srv.Shutdown alone.
func TestProxyShutdown_StalledDrainStillJoinsInFlightRepairJobs(t *testing.T) {
	if testing.Short() {
		t.Skip("holds a stream across the real 8s drainStallWindow so the repair outlives the bail-out")
	}

	logs := &syncBuffer{}
	upstream := newHeldStreamUpstream(t)
	ps, _, base := startDrainProxyWithLogger(t, upstream.srv.URL, zerolog.New(logs), 0)
	defer close(upstream.release)

	resp := openStreamThroughProxy(t, ps, base)
	defer func() { _ = resp.Body.Close() }()
	<-upstream.opened
	waitForInFlight(t, ps, 1)

	// Register a repair the way the handler does — Add under repairMu, Done from
	// the job — and finish it AFTER the stall detector has fired.
	//
	// The wait is CAUSAL, not a wall clock. A fixed drainStallWindow+ε sleep
	// races the detector: the sleep starts before Shutdown does, while the stall
	// fires a whole window after the sampler starts, so a stall delayed past ε —
	// ticker drift under a loaded, race-instrumented, 8-way-sharded run — would
	// let the repair finish FIRST. This control would then observe a non-zero
	// repairFinishedAt and pass green under exactly the parent-cancel
	// implementation it exists to catch, guarding nothing and saying nothing.
	// Gating on the detector's own log line instead makes the repair provably
	// still in flight at the instant the drain is cut, however far the ticker
	// drifts.
	ps.repairMu.Lock()
	ps.repairJobs.Add(1)
	ps.repairMu.Unlock()
	var repairFinishedAt atomic.Int64
	go func() {
		defer ps.repairJobs.Done()
		// A generous bound so a detector that never fires ends the goroutine
		// rather than wedging the suite; that case is reported by the
		// DrainStopStalled assertion below, which is the honest message for it.
		deadline := time.Now().Add(45 * time.Second)
		for !strings.Contains(logs.String(), "stopped falling") {
			if !time.Now().Before(deadline) {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		time.Sleep(250 * time.Millisecond)
		repairFinishedAt.Store(time.Now().UnixNano())
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	outcome, _ := ps.Shutdown(ctx)
	returnedAt := time.Now()

	if outcome.StopReason != DrainStopStalled {
		t.Fatalf("outcome.StopReason = %q, want %q; this test no longer exercises the early-bail path", outcome.StopReason, DrainStopStalled)
	}
	finished := repairFinishedAt.Load()
	if finished == 0 {
		t.Fatal("Shutdown returned before the in-flight pane repair ran: the stall detector cancelled the context waitRepairJobs joins on, so the BOS-982 repair join was abandoned")
	}
	if returnedAt.Before(time.Unix(0, finished)) {
		t.Fatalf("Shutdown returned at %v, before the repair finished at %v", returnedAt, time.Unix(0, finished))
	}
}

// TestDrainProgressCadenceAndStallWindowAtTheShippedBudget is the R5b guard: the
// stall window is a stated constant whose bail point holds against the REAL
// cadence at the shipped budget, not against a pinned drainProgressInterval.
//
// It also keeps the stall tests above from passing vacuously through a log
// cadence that never ticks, by asserting the derived cadence directly.
func TestDrainProgressCadenceAndStallWindowAtTheShippedBudget(t *testing.T) {
	// The shipped budget: lib/bossalib/config's defaultProxyDrainTimeout, and the
	// number the measured 15.002s shutdown burned. Restated rather than imported
	// because services/bossd/internal/server must not depend on it to be correct;
	// if the default moves, the arithmetic below is what needs re-reading.
	const shippedBudget = 15 * time.Second

	ps, err := NewProxyServer(ProxyServerConfig{Failover: &fakeFailover{}, Logger: zerolog.Nop(), Upstream: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), shippedBudget)
	defer cancel()

	// The log cadence is coarse: budget/3, clamped. Two samples land inside the
	// shipped budget, which is why the stall detector does not ride it.
	// The derivation reads time.Until(deadline), which has already shrunk by the
	// time it runs, so this is a tolerance rather than an equality.
	cadence := ps.drainProgressCadence(ctx)
	want := shippedBudget / drainProgressTicksPerDrain
	if cadence > want || cadence < want-100*time.Millisecond {
		t.Fatalf("drainProgressCadence = %v at a %v budget, want ~%v", cadence, shippedBudget, want)
	}
	// A window counted in log-cadence samples could only be one (bail at ~5s,
	// under the budget BOS-888 rejected) or two (~10s); 8s is not on that grid at
	// all, which is why the detector samples on its own finer interval.
	if drainStallSampleInterval >= cadence {
		t.Fatalf("stall sample interval %v is not finer than the log cadence %v; the window could then only be a multiple of the cadence", drainStallSampleInterval, cadence)
	}
	// Against the NOMINAL cadence, not the derived one. drainProgressCadence
	// reads time.Until(deadline), so `cadence` is a few microseconds under 5s and
	// `drainStallWindow % cadence` is non-zero for every conceivable window —
	// including the 5s and 10s multiples this guard exists to reject. Taking the
	// modulus against the exact budget/ticks value is what makes it fire.
	if drainStallWindow%want == 0 {
		t.Fatalf("stall window %v is a whole number of log cadences (%v), so it gains nothing from its own sampler", drainStallWindow, want)
	}

	// The documented bail point: a drain whose count never falls resolves one
	// window in, not at the budget.
	if drainStallWindow != drainStallSampleInterval*drainStallSamples {
		t.Fatalf("drainStallWindow = %v, want %v (%v x %d)", drainStallWindow, drainStallSampleInterval*drainStallSamples, drainStallSampleInterval, drainStallSamples)
	}
	if drainStallWindow >= shippedBudget {
		t.Fatalf("stall window %v is not shorter than the shipped budget %v, so the deadline always wins and the bail-out is dead code", drainStallWindow, shippedBudget)
	}
	// Bounded from below by the shared 5s shutdown ctx BOS-888 rejected as too
	// short. A window at or under it would cut a single in-flight turn SOONER
	// than the behaviour BOS-888 was filed to remove.
	const bos888RejectedSharedBudget = 5 * time.Second
	if drainStallWindow <= bos888RejectedSharedBudget {
		t.Fatalf("stall window %v is not longer than the %v shared budget BOS-888 rejected; it would reinstate the cut that fix removed", drainStallWindow, bos888RejectedSharedBudget)
	}
	if want := 8 * time.Second; drainStallWindow != want {
		t.Fatalf("drainStallWindow = %v, want the documented %v; update the bail point stated on the constant with it", drainStallWindow, want)
	}
}

// readFrame reads one SSE frame (terminated by a blank line) from br.
func readFrame(br *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		line, err := br.ReadString('\n')
		sb.WriteString(line)
		if err != nil {
			return sb.String(), err
		}
		if line == "\n" {
			return sb.String(), nil
		}
	}
}
