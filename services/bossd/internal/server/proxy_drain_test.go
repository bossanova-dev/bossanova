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
type heldStreamUpstream struct {
	srv      *httptest.Server
	opened   chan struct{}
	release  chan struct{}
	openOnce sync.Once
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
		opened:  make(chan struct{}),
		release: make(chan struct{}),
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
func startDrainProxyWithLogger(t *testing.T, upstream string, logger zerolog.Logger, progressInterval time.Duration) (*ProxyServer, *recordingListener, string) {
	t.Helper()
	ps, err := NewProxyServer(ProxyServerConfig{
		Failover: &fakeFailover{},
		Logger:   logger,
		Upstream: upstream,
	})
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

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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
	if elapsed > 2*time.Second {
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
