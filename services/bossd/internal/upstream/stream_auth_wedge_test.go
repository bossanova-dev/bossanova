package upstream

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

// This suite reuses the package's existing lockedBuffer (event_forwarding_test.go)
// as its zerolog sink: reading a bare bytes.Buffer while the Run goroutine is
// still writing to it is a data race the -race gate would (correctly) fail.

// authRejectStream models bosso rejecting the daemon session_token: the
// snapshot Send fails with CodeUnauthenticated, so openStream returns
// before the handshake completes and before any forwarder goroutine
// starts. This is the exact shape of the BOS-942 wedge.
type authRejectStream struct{}

func (authRejectStream) Send(*pb.DaemonEvent) error {
	return connect.NewError(connect.CodeUnauthenticated, errors.New("invalid daemon token"))
}

func (authRejectStream) Receive() (*pb.OrchestratorCommand, error) {
	return nil, errors.New("unreachable")
}

func (authRejectStream) CloseRequest() error { return nil }

// handshakeThenDropStream accepts the snapshot (so markConnected runs)
// and then drops the connection with an ordinary, non-auth error.
type handshakeThenDropStream struct{}

func (handshakeThenDropStream) Send(*pb.DaemonEvent) error { return nil }

func (handshakeThenDropStream) Receive() (*pb.OrchestratorCommand, error) {
	return nil, errors.New("connection reset")
}

func (handshakeThenDropStream) CloseRequest() error { return nil }

// authRejectOnReceiveStream models the shape bosso actually presents, which
// authRejectStream above does not: the daemon's snapshot Send returns nil
// (it only has to reach the local HTTP/2 framer), and the CodeUnauthenticated
// arrives afterwards, from Receive, because bosso authenticates on the request
// HEADERS. Run's own backoff comment records this; the wedge state machine has
// to survive it, since a daemon that never gets past Send is the easy case and
// this is the one BOS-942 was.
type authRejectOnReceiveStream struct{}

func (authRejectOnReceiveStream) Send(*pb.DaemonEvent) error { return nil }

func (authRejectOnReceiveStream) Receive() (*pb.OrchestratorCommand, error) {
	return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid daemon token"))
}

func (authRejectOnReceiveStream) CloseRequest() error { return nil }

// wedgeOpener hands out auth-rejecting streams, except on the opens listed
// in handshakeOn, where it completes the handshake instead. It also
// satisfies sessionTokenHolder so tryReRegister has something to rotate.
type wedgeOpener struct {
	mu          sync.Mutex
	opens       int
	handshakeOn map[int]bool
	openCh      chan struct{}
	tokens      *SessionTokenHolder
	// rejectOnReceive switches the rejecting fixture from the Send-rejects
	// shape to the production Receive-rejects one.
	rejectOnReceive bool
}

func newWedgeOpener() *wedgeOpener {
	return &wedgeOpener{
		handshakeOn: map[int]bool{},
		// Generous buffer: the anti-spam pin drives ~120 opens and the
		// send below must never block the Run loop.
		openCh: make(chan struct{}, 1024),
		tokens: NewSessionTokenHolder("stale-token"),
	}
}

// wedgeOpt configures the opener before the harness starts StreamClient.Run.
//
// Opener fixtures MUST be set through these rather than assigned after
// newWedgeHarness returns. Run opens its first stream immediately, so a
// post-return assignment races DaemonStream's read of the same field, and
// even under o.mu it leaves undefined whether open #1 sees the fixture the
// test asked for. Applying the option before the goroutine is created makes
// the write happen-before every read, so no lock is needed here.
type wedgeOpt func(*wedgeOpener)

// withRejectOnReceive switches the rejecting fixture from the Send-rejects
// shape to the production Receive-rejects one.
func withRejectOnReceive() wedgeOpt {
	return func(o *wedgeOpener) { o.rejectOnReceive = true }
}

// withHandshakeOn completes the handshake on open n instead of rejecting it.
func withHandshakeOn(n int) wedgeOpt {
	return func(o *wedgeOpener) { o.handshakeOn[n] = true }
}

func (o *wedgeOpener) DaemonStream(context.Context) bidirectionalStream {
	o.mu.Lock()
	o.opens++
	handshake := o.handshakeOn[o.opens]
	o.mu.Unlock()
	select {
	case o.openCh <- struct{}{}:
	default:
	}
	if handshake {
		return handshakeThenDropStream{}
	}
	o.mu.Lock()
	onReceive := o.rejectOnReceive
	o.mu.Unlock()
	if onReceive {
		return authRejectOnReceiveStream{}
	}
	return authRejectStream{}
}

func (o *wedgeOpener) SessionToken() string     { return o.tokens.Get() }
func (o *wedgeOpener) SetSessionToken(t string) { o.tokens.Set(t) }
func (o *wedgeOpener) CompareAndSwapSessionToken(old, tok string) bool {
	return o.tokens.CompareAndSwap(old, tok)
}

// wedgeHarness drives StreamClient.Run one reconnect iteration at a time on
// a fake clock, so the test owns the cadence and nothing depends on wall
// time.
type wedgeHarness struct {
	t      *testing.T
	clock  *fakeClock
	logs   *lockedBuffer
	opener *wedgeOpener
	client *StreamClient
	cancel context.CancelFunc
	done   chan struct{}
}

// wedgeCadence is the virtual gap the harness advances between reconnect
// attempts. It matches streamMaxBackoff, the cadence a genuinely wedged
// daemon settles into once the ramp is capped.
const wedgeCadence = streamMaxBackoff

func newWedgeHarness(t *testing.T, mutate func(*StreamClientConfig), opts ...wedgeOpt) *wedgeHarness {
	t.Helper()
	clock := newFakeClock()
	logs := &lockedBuffer{}
	opener := newWedgeOpener()
	// Before the Run goroutine exists, so these writes are ordered ahead of
	// every DaemonStream read. See wedgeOpt.
	for _, opt := range opts {
		opt(opener)
	}

	cfg := StreamClientConfig{
		Opener: opener,
		Events: NewStreamBus(zerolog.Nop()),
		Clock:  clock,
		Logger: zerolog.New(logs).Level(zerolog.DebugLevel),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	client := NewStreamClient(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	h := &wedgeHarness{
		t:      t,
		clock:  clock,
		logs:   logs,
		opener: opener,
		client: client,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go func() {
		client.Run(ctx)
		close(h.done)
	}()
	t.Cleanup(h.stop)
	return h
}

// step waits for one reconnect attempt to complete (open + log + backoff
// timer registered) and then advances virtual time past the backoff so the
// loop takes the next iteration.
func (h *wedgeHarness) step() {
	h.t.Helper()
	select {
	case <-h.opener.openCh:
	case <-time.After(10 * time.Second):
		h.t.Fatal("stream open never happened")
	}
	// Wait for the reconnect backoff timer specifically, not merely for some
	// timer. The coalescer shares this clock and keeps a 100ms window timer
	// pending, which satisfies a bare waitForTimer while the Run loop is still
	// short of its backoff select; advancing there registers the backoff timer
	// past the virtual now we just moved to, so it never fires and Run parks
	// until the next step times out. The backoff never drops below
	// streamInitialBackoff and the coalesce window is an order of magnitude
	// under it, so the floor separates the two cleanly. A pending backoff
	// timer also proves this iteration's logging is already visible, since the
	// loop writes its log line before it sleeps.
	if !waitForTimerAtLeast(h.clock, streamInitialBackoff, 10*time.Second) {
		h.t.Fatal("reconnect backoff timer never registered")
	}
	h.clock.Advance(wedgeCadence)
}

func (h *wedgeHarness) steps(n int) {
	h.t.Helper()
	for i := 0; i < n; i++ {
		h.step()
	}
}

func (h *wedgeHarness) stop() {
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		h.t.Error("Run did not return after cancel")
	}
}

const (
	wedgeWarnMsg  = streamAuthWedgedMsg
	wedgeFirstMsg = streamAuthRejectedMsg
	wedgeDebugMsg = "stream closed, reconnecting (auth still failing)"
)

// countWarns counts WARN-level log lines in the captured buffer.
func countWarns(logs string) int {
	return strings.Count(logs, `"level":"warn"`)
}

// logLineContaining returns the single captured log line carrying needle, so a
// per-line field assertion cannot be satisfied (or defeated) by a different
// line in the same buffer.
func logLineContaining(t *testing.T, logs, needle string) string {
	t.Helper()
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	t.Fatalf("no log line contains %q:\n%s", needle, logs)
	return ""
}

// countAuthEscalations counts the loop's OWN escalated auth lines — the first
// failure of a run plus every budget re-statement. It is deliberately narrower
// than countWarns: with a ReRegister configured (which is production) each
// escalation also emits a re-register WARN, so a raw warn count measures two
// lines per escalation and cannot express "how often does the loop speak".
func countAuthEscalations(logs string) int {
	return strings.Count(logs, wedgeFirstMsg) + strings.Count(logs, wedgeWarnMsg)
}

// TestStreamAuthWedgeFirstFailureWarnsThenSuppresses pins the two halves of
// the suppression: the first unrecoverable auth failure is loud, and the
// repeats inside the budget drop to debug without adding another warn.
func TestStreamAuthWedgeFirstFailureWarnsThenSuppresses(t *testing.T) {
	h := newWedgeHarness(t, func(cfg *StreamClientConfig) {
		cfg.TokenProvider = &markedTokenProvider{reason: reloginReasonRefreshTokenRejected}
	})

	h.step()
	afterFirst := h.logs.String()
	if got := countWarns(afterFirst); got != 1 {
		t.Fatalf("warn count after first failure = %d, want 1; logs=%s", got, afterFirst)
	}
	if strings.Contains(afterFirst, wedgeWarnMsg) {
		t.Fatalf("first failure used the sustained-wedge wording; it is an ordinary rejection: %s", afterFirst)
	}
	// The FIRST warn of a run is the one an operator reads when they go
	// looking, and it has to answer both "which wedge" and "since when" on its
	// own. It used to fall through to the generic reconnect line and carry
	// neither, which made the opening line of every incident the least
	// informative one in the log.
	if !strings.Contains(afterFirst, wedgeFirstMsg) {
		t.Fatalf("first failure did not use the auth-rejected wording: %s", afterFirst)
	}
	for _, want := range []string{
		`"relogin_reason":"` + reloginReasonRefreshTokenRejected + `"`,
		`"auth_failing_since":`,
		`"auth_failing_for":`,
	} {
		if !strings.Contains(afterFirst, want) {
			t.Fatalf("first warn missing %s: %s", want, afterFirst)
		}
	}

	// Several more failures well inside the budget.
	h.steps(5)
	afterSuppressed := h.logs.String()
	if !strings.Contains(afterSuppressed, wedgeDebugMsg) {
		t.Fatalf("suppressed repeats never logged at debug: %s", afterSuppressed)
	}
	if got := countWarns(afterSuppressed); got != 1 {
		t.Fatalf("warn count after suppressed repeats = %d, want 1 (unchanged); logs=%s", got, afterSuppressed)
	}
}

// TestStreamAuthWedgeEscalatesOnBudgetAndResetsStreak proves the daemon
// stops being silent: crossing authWedgeWarnBudget re-states the wedge at
// warn with the enumerated reason and a non-zero elapsed time, and then the
// streak resets so the next escalation needs a full budget again.
func TestStreamAuthWedgeEscalatesOnBudgetAndResetsStreak(t *testing.T) {
	h := newWedgeHarness(t, func(cfg *StreamClientConfig) {
		cfg.TokenProvider = &markedTokenProvider{reason: reloginReasonRefreshOutcomeUnknown}
	})

	// Iteration 1 warns as a first failure; iterations 2..30 are suppressed;
	// iteration 31 crosses the budget.
	h.steps(authWedgeWarnBudget)
	beforeEscalation := h.logs.String()
	if strings.Contains(beforeEscalation, wedgeWarnMsg) {
		t.Fatalf("escalated before the budget was crossed: %s", beforeEscalation)
	}

	h.step()
	escalated := h.logs.String()
	if got := strings.Count(escalated, wedgeWarnMsg); got != 1 {
		t.Fatalf("wedge warn count = %d, want exactly 1; logs=%s", got, escalated)
	}
	// Assert on the escalated LINE, not on the whole buffer: the first warn of
	// the run legitimately reports auth_failing_for=0 (it is the instant the
	// run began), so a buffer-wide substring check for a zero duration would
	// fail on a correct log.
	line := logLineContaining(t, escalated, wedgeWarnMsg)
	if !strings.Contains(line, `"relogin_reason":"`+reloginReasonRefreshOutcomeUnknown+`"`) {
		t.Fatalf("escalated warn dropped the enumerated reason: %s", line)
	}
	if !strings.Contains(line, `"auth_failing_since":`) {
		t.Fatalf("escalated warn dropped auth_failing_since: %s", line)
	}
	if strings.Contains(line, `"auth_failing_for":0`) {
		t.Fatalf("escalated warn reported a zero duration: %s", line)
	}

	// The streak reset: the next budget-1 iterations must stay quiet.
	h.steps(authWedgeWarnBudget - 1)
	if got := strings.Count(h.logs.String(), wedgeWarnMsg); got != 1 {
		t.Fatalf("wedge warn count = %d after a partial budget, want still 1; the streak did not reset", got)
	}
	h.step()
	if got := strings.Count(h.logs.String(), wedgeWarnMsg); got != 2 {
		t.Fatalf("wedge warn count = %d after a full second budget, want 2", got)
	}
}

// TestStreamAuthWedgeAntiSpamPin is the two-sided property. The upper bound
// is what the original suppression bought (a wedged daemon must not fill the
// log); the lower bound is what BOS-944 adds (it must not go silent either).
// One hour at the capped 30s reconnect cadence is the real-world shape of the
// incident.
//
// It runs with a ReRegister configured because production always has one
// (main.go wires the closure unconditionally when an upstream exists), and
// that is the configuration where the un-suppressed re-register path also
// speaks. Pinning the budget without it measured the one arrangement that
// cannot produce the real log volume: each escalation emits TWO warns in
// production (the loop's line and the re-register's), so a bound measured
// against the loop alone describes a daemon nobody runs.
func TestStreamAuthWedgeAntiSpamPin(t *testing.T) {
	h := newWedgeHarness(t, func(cfg *StreamClientConfig) {
		cfg.ReRegister = func(context.Context) (string, error) {
			return "", errors.New("permission_denied")
		}
	})

	iterations := int(time.Hour / wedgeCadence)
	h.steps(iterations)

	logs := h.logs.String()

	// Lower and upper bound on how often the loop itself speaks. Counting
	// escalations rather than raw warns keeps BOTH bounds live: with a
	// ReRegister attached, one escalation already produces two warn lines, so
	// `warns > 1` would be satisfied by a daemon that spoke exactly once.
	escalations := countAuthEscalations(logs)
	if escalations <= 1 {
		t.Fatalf("auth escalations over a simulated hour = %d, want > 1; a wedged daemon that warns once is indistinguishable from one that recovered\nlogs=%s", escalations, logs)
	}
	if escalations > 5 {
		t.Fatalf("auth escalations over a simulated hour = %d, want <= 5; the suppression is the property this must not regress\nlogs=%s", escalations, logs)
	}

	// And the volume an operator actually sees, re-register lines included.
	// Two lines per escalation is the ceiling; anything above it means a third
	// un-suppressed writer appeared on this path.
	if warns := countWarns(logs); warns > 2*escalations {
		t.Fatalf("warn lines over a simulated hour = %d, want <= %d (at most one loop line and one re-register line per escalation)\nlogs=%s", warns, 2*escalations, logs)
	}
}

// TestStreamAuthWedgeHandshakeResetsStateMachine proves recovery resets the
// whole machine, not just the log level: after a successful handshake the
// next auth failure is a first failure again, and AuthSnapshot reports no
// wedge in between.
func TestStreamAuthWedgeHandshakeResetsStateMachine(t *testing.T) {
	// Open #4 completes the handshake; everything else is rejected.
	h := newWedgeHarness(t, nil, withHandshakeOn(4))

	h.steps(3)
	if snap := h.client.AuthSnapshot(); snap.AuthFailingSince.IsZero() {
		t.Fatal("AuthSnapshot reports no wedge while auth is failing")
	}

	h.step() // the successful handshake
	if snap := h.client.AuthSnapshot(); !snap.AuthFailingSince.IsZero() {
		t.Fatalf("AuthSnapshot still reports a wedge after a successful handshake: %v", snap.AuthFailingSince)
	}

	warnsBefore := countWarns(h.logs.String())
	h.step() // the next auth failure must warn as a first failure again
	if got := countWarns(h.logs.String()); got != warnsBefore+1 {
		t.Fatalf("warn count = %d, want %d; the post-recovery failure did not warn as a first", got, warnsBefore+1)
	}
	if snap := h.client.AuthSnapshot(); snap.AuthFailingSince.IsZero() {
		t.Fatal("AuthSnapshot reports no wedge after the post-recovery failure")
	}
}

// TestStreamAuthWedgeAuthSnapshotHealthyBeforeAnyFailure pins the healthy
// reading, so `boss daemon doctor` cannot report a wedge on a daemon that
// has not had one.
func TestStreamAuthWedgeAuthSnapshotHealthyBeforeAnyFailure(t *testing.T) {
	c := NewStreamClient(StreamClientConfig{
		Opener: newWedgeOpener(),
		Events: NewStreamBus(zerolog.Nop()),
		Logger: zerolog.Nop(),
	})
	snap := c.AuthSnapshot()
	if !snap.AuthFailingSince.IsZero() {
		t.Errorf("AuthFailingSince = %v, want zero on a fresh client", snap.AuthFailingSince)
	}
	if snap.Connected {
		t.Error("Connected = true on a client that never dialled")
	}
}

// TestStreamAuthWedgeReRegisterWarnsOnEscalationIteration proves the
// re-register failure rides the same escalation decision as the loop's own
// log line. Before BOS-944 the suppression was unbounded, so a re-register
// that had been failing all day only ever spoke once.
func TestStreamAuthWedgeReRegisterWarnsOnEscalationIteration(t *testing.T) {
	h := newWedgeHarness(t, func(cfg *StreamClientConfig) {
		cfg.ReRegister = func(context.Context) (string, error) {
			return "", errors.New("permission_denied")
		}
	})

	const reRegisterWarn = "stream: re-register failed after auth rejection"
	const reRegisterDebug = "stream: re-register still failing after auth rejection"

	h.step()
	if got := strings.Count(h.logs.String(), reRegisterWarn); got != 1 {
		t.Fatalf("re-register warn count after the first failure = %d, want 1", got)
	}

	h.steps(authWedgeWarnBudget - 1)
	mid := h.logs.String()
	if !strings.Contains(mid, reRegisterDebug) {
		t.Fatalf("suppressed re-register failures never logged at debug: %s", mid)
	}
	if got := strings.Count(mid, reRegisterWarn); got != 1 {
		t.Fatalf("re-register warn count inside the budget = %d, want 1", got)
	}

	h.step() // budget crossed: suppressFailureWarn=false again
	if got := strings.Count(h.logs.String(), reRegisterWarn); got != 2 {
		t.Fatalf("re-register warn count on the escalation iteration = %d, want 2", got)
	}
}

// TestStreamAuthWedgeLogsNeverCarryTokens is the negative assertion the plan
// requires: these lines are pasted into issues, so no credential may reach
// them.
func TestStreamAuthWedgeLogsNeverCarryTokens(t *testing.T) {
	const accessToken = "access-token-fixture-do-not-log"
	const refreshToken = "refresh-token-fixture-do-not-log"
	const sessionToken = "session-token-fixture-do-not-log"

	provider := &markedTokenProvider{reason: reloginReasonRefreshTokenRejected}
	provider.token = accessToken
	h := newWedgeHarness(t, func(cfg *StreamClientConfig) {
		cfg.TokenProvider = provider
		cfg.ReRegister = func(context.Context) (string, error) {
			return "", errors.New("permission_denied")
		}
	})
	h.opener.tokens.Set(sessionToken)

	// Far enough to cross the budget so the escalated line is in the buffer.
	h.steps(authWedgeWarnBudget + 1)

	logs := h.logs.String()
	if !strings.Contains(logs, wedgeWarnMsg) {
		t.Fatalf("test did not reach the escalated line it is meant to inspect: %s", logs)
	}
	for _, secret := range []string{accessToken, refreshToken, sessionToken} {
		if strings.Contains(logs, secret) {
			t.Errorf("captured logs leaked %q", secret)
		}
	}
}

// --- SessionTokenHolder.LastSetAt ---

func TestSessionTokenHolderLastSetAt(t *testing.T) {
	t.Run("empty seed leaves it zero", func(t *testing.T) {
		if got := NewSessionTokenHolder("").LastSetAt(); !got.IsZero() {
			t.Fatalf("LastSetAt = %v, want zero: an empty seed means startup Register failed", got)
		}
	})

	t.Run("non-empty seed stamps it", func(t *testing.T) {
		if got := NewSessionTokenHolder("tok").LastSetAt(); got.IsZero() {
			t.Fatal("LastSetAt = zero after a seeded holder; the seed follows a successful Register")
		}
	})

	t.Run("Set advances it", func(t *testing.T) {
		h := NewSessionTokenHolder("tok")
		before := h.LastSetAt()
		time.Sleep(time.Millisecond)
		h.Set("tok-2")
		if got := h.LastSetAt(); !got.After(before) {
			t.Fatalf("LastSetAt = %v, want after %v", got, before)
		}
	})

	t.Run("successful CompareAndSwap advances it", func(t *testing.T) {
		h := NewSessionTokenHolder("tok")
		before := h.LastSetAt()
		time.Sleep(time.Millisecond)
		if !h.CompareAndSwap("tok", "tok-2") {
			t.Fatal("CompareAndSwap returned false for a matching old value")
		}
		if got := h.LastSetAt(); !got.After(before) {
			t.Fatalf("LastSetAt = %v, want after %v", got, before)
		}
	})

	t.Run("stamps come from the injectable clock", func(t *testing.T) {
		clock := newFakeClock()
		h := newSessionTokenHolderWithClock("tok", clock)
		seeded := h.LastSetAt()
		if !seeded.Equal(clock.Now()) {
			t.Fatalf("seed stamp = %v, want the clock's now %v", seeded, clock.Now())
		}
		clock.Advance(90 * time.Second)
		h.Set("tok-2")
		if got := h.LastSetAt(); !got.Equal(clock.Now()) {
			t.Fatalf("Set stamp = %v, want the advanced clock %v; the holder is still reading wall time", got, clock.Now())
		}
		clock.Advance(30 * time.Second)
		if !h.CompareAndSwap("tok-2", "tok-3") {
			t.Fatal("CompareAndSwap returned false for a matching old value")
		}
		if got := h.LastSetAt(); !got.Equal(clock.Now()) {
			t.Fatalf("CompareAndSwap stamp = %v, want the advanced clock %v", got, clock.Now())
		}
	})

	t.Run("a zero-value holder does not panic", func(t *testing.T) {
		var h SessionTokenHolder
		h.Set("tok")
		if h.LastSetAt().IsZero() {
			t.Fatal("LastSetAt stayed zero after a write on a constructor-less holder")
		}
	})

	t.Run("failed CompareAndSwap does not advance it", func(t *testing.T) {
		h := NewSessionTokenHolder("tok")
		before := h.LastSetAt()
		time.Sleep(time.Millisecond)
		if h.CompareAndSwap("not-the-current-token", "tok-2") {
			t.Fatal("CompareAndSwap returned true for a stale old value")
		}
		if got := h.LastSetAt(); !got.Equal(before) {
			t.Fatalf("LastSetAt = %v, want unchanged %v: a rejected swap is not a registration", got, before)
		}
	})
}

// TestStreamAuthWedgeSurvivesPostHandshakeRejection is the regression pin for
// the shape every other fixture in this file misses, and the one production
// presents: Send(snapshot) succeeds, then Receive answers
// CodeUnauthenticated.
//
// Before this pin, markConnected() cleared the entire wedge state machine the
// instant the local Send returned, so on this shape authFailingSince was
// re-stamped on every reconnect. Three consequences, all of them BOS-942
// reproduced with a green suite: the duration never accumulated, so
// `boss daemon doctor` reported a daemon wedged for hours as wedged for
// seconds; the suppression streak never reached authWedgeWarnBudget, so every
// single iteration warned as a FIRST failure; and zeroExpiryWarned reopened on
// every reconnect. Nothing else in the suite could see it, because the two
// existing rejecting fixtures both fail at Send.
func TestStreamAuthWedgeSurvivesPostHandshakeRejection(t *testing.T) {
	h := newWedgeHarness(t, nil, withRejectOnReceive())

	h.step()
	first := h.client.AuthSnapshot()
	if first.AuthFailingSince.IsZero() {
		t.Fatalf("AuthFailingSince is zero after a post-handshake auth rejection; logs=%s", h.logs.String())
	}
	// The live-connectivity bit must not still read true from the accepted
	// snapshot: the stream is gone and the loop is in its backoff gap, which
	// is precisely when an operator runs `boss daemon doctor`.
	if first.Connected {
		t.Error("AuthSnapshot reports Connected during the reconnect backoff gap")
	}

	// The discriminating property: the SAME instant, iteration after
	// iteration. A re-stamped clock is what makes an hours-long wedge read as
	// seconds old.
	h.steps(5)
	later := h.client.AuthSnapshot()
	if !later.AuthFailingSince.Equal(first.AuthFailingSince) {
		t.Fatalf("AuthFailingSince moved from %v to %v across sustained failures; the wedge clock is being reset by the local Send", first.AuthFailingSince, later.AuthFailingSince)
	}

	// And the suppression the accumulating clock unlocks: six failures inside
	// one budget must still be one escalation, not six.
	if got := countAuthEscalations(h.logs.String()); got != 1 {
		t.Fatalf("auth escalations over 6 sustained failures = %d, want 1; the streak is being reset by the local Send\nlogs=%s", got, h.logs.String())
	}
}

// TestStreamAuthWedgeBoundsWarnsOnPostHandshakeRejection is AC #12's bound
// re-run over the production rejection shape. It is a separate test from
// TestStreamAuthWedgeAntiSpamPin on purpose: that one drives the Send-rejects
// fixture, where the backoff ramps because wasConnected() reads false, and so
// it never exercised the ramp's own copy of the local-Send trap.
func TestStreamAuthWedgeBoundsWarnsOnPostHandshakeRejection(t *testing.T) {
	h := newWedgeHarness(t, func(cfg *StreamClientConfig) {
		cfg.ReRegister = func(context.Context) (string, error) {
			return "", errors.New("permission_denied")
		}
	}, withRejectOnReceive())

	iterations := int(time.Hour / wedgeCadence)
	h.steps(iterations)

	logs := h.logs.String()
	escalations := countAuthEscalations(logs)
	if escalations <= 1 {
		t.Fatalf("auth escalations over a simulated hour = %d, want > 1; a wedged daemon that warns once is indistinguishable from one that recovered\nlogs=%s", escalations, logs)
	}
	if escalations > 5 {
		t.Fatalf("auth escalations over a simulated hour = %d, want <= 5\nlogs=%s", escalations, logs)
	}
}

// TestStreamAuthWedgeUpstreamTrafficClearsTheWedge pins the one signal that is
// direct evidence the upstream accepted our credentials — a command arriving
// FROM bosso — and the reason it has to exist separately from the loop's
// after-the-fact inferences: it fires while the stream is still open, so a
// wedge that ends without a token rotation or a `boss login` stops being
// reported as ongoing.
func TestStreamAuthWedgeUpstreamTrafficClearsTheWedge(t *testing.T) {
	c := NewStreamClient(StreamClientConfig{
		Opener: newWedgeOpener(),
		Events: NewStreamBus(zerolog.Nop()),
		Logger: zerolog.Nop(),
	})
	if note := c.noteAuthFailure(); !note.First {
		t.Fatal("seeding the wedge did not register as a first failure")
	}
	if c.AuthSnapshot().AuthFailingSince.IsZero() {
		t.Fatal("seeded wedge is not visible in AuthSnapshot")
	}

	c.noteUpstreamAccepted()

	if snap := c.AuthSnapshot(); !snap.AuthFailingSince.IsZero() {
		t.Fatalf("AuthFailingSince = %v after inbound upstream traffic, want zero", snap.AuthFailingSince)
	}
	// The whole machine resets, not just the clock: the zero-expiry warn gate
	// is cleared by the same recovery, which is what keeps that warning to one
	// line per transition rather than one per refresh tick.
	if !c.noteZeroExpiry() {
		t.Error("noteZeroExpiry stayed closed after a recovery; the reset is partial")
	}
}

// TestStreamAuthWedgeSurvivesSucceedingReRegister is the rotate-and-reject
// shape: ReRegister keeps SUCCEEDING (bosso hands out a fresh session_token
// every time) while the stream keeps rejecting. Every other wedge fixture in
// this file drives a ReRegister that fails, which is the easy half — there the
// loop already knows it did not recover.
//
// The hard half is this one, and it is the BOS-942 blind spot one layer up
// from markConnected. tryReRegister reports that a token was STORED, never
// that it authenticates; it even answers true from its "someone else already
// rotated it" fallback. So clearing the wedge on `rotated` reset
// authFailingSince and the suppression streak on EVERY iteration: the doctor
// answered "signed in" with a fresh last_registered_at for a daemon that had
// not authenticated in hours, the escalated WARN never re-fired, and the
// backoff stayed pinned at streamInitialBackoff, tight-looping against bosso.
func TestStreamAuthWedgeSurvivesSucceedingReRegister(t *testing.T) {
	var rotations int
	var mu sync.Mutex
	h := newWedgeHarness(t, func(cfg *StreamClientConfig) {
		cfg.ReRegister = func(context.Context) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			rotations++
			// A genuinely fresh token each time, so CompareAndSwap applies
			// and tryReRegister returns true on every single iteration.
			return "rotated-token-" + strconv.Itoa(rotations), nil
		}
	})

	h.step()
	first := h.client.AuthSnapshot()
	if first.AuthFailingSince.IsZero() {
		t.Fatalf("AuthFailingSince is zero after an auth rejection that rotated the token; logs=%s", h.logs.String())
	}

	h.steps(5)
	later := h.client.AuthSnapshot()
	if !later.AuthFailingSince.Equal(first.AuthFailingSince) {
		t.Fatalf("AuthFailingSince moved from %v to %v while ReRegister kept succeeding; a rotation is being read as proof the credentials work", first.AuthFailingSince, later.AuthFailingSince)
	}
	mu.Lock()
	got := rotations
	mu.Unlock()
	if got < 6 {
		t.Fatalf("ReRegister ran %d times over 6 failures, want 6; the fixture is not exercising the rotate-and-reject shape", got)
	}
	if escalations := countAuthEscalations(h.logs.String()); escalations != 1 {
		t.Fatalf("auth escalations over 6 rotate-and-reject failures = %d, want 1; the streak is being reset by the rotation\nlogs=%s", escalations, h.logs.String())
	}
}

// TestStreamAuthWedgeBoundsWarnsOnSucceedingReRegister is AC #12's bound over
// the rotate-and-reject shape. It is the falsifying half of the test above:
// with the wedge cleared on every rotation each iteration warns as a first
// failure, so an hour produces one escalation per iteration rather than a
// handful.
func TestStreamAuthWedgeBoundsWarnsOnSucceedingReRegister(t *testing.T) {
	var rotations int
	var mu sync.Mutex
	h := newWedgeHarness(t, func(cfg *StreamClientConfig) {
		cfg.ReRegister = func(context.Context) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			rotations++
			return "rotated-token-" + strconv.Itoa(rotations), nil
		}
	})

	iterations := int(time.Hour / wedgeCadence)
	h.steps(iterations)

	logs := h.logs.String()
	escalations := countAuthEscalations(logs)
	if escalations <= 1 {
		t.Fatalf("auth escalations over a simulated hour = %d, want > 1; a wedged daemon that warns once is indistinguishable from one that recovered\nlogs=%s", escalations, logs)
	}
	if escalations > 5 {
		t.Fatalf("auth escalations over a simulated hour = %d, want <= 5; a succeeding ReRegister must not reset the suppression streak\nlogs=%s", escalations, logs)
	}
}

// backoffMillis pulls every `"backoff":N` value zerolog wrote, in order. The
// three reconnect log arms all carry Dur("backoff", backoff), so this is the
// loop's own view of its ramp rather than a re-derivation of it.
func backoffMillis(t *testing.T, logs string) []float64 {
	t.Helper()
	var out []float64
	for _, line := range strings.Split(logs, "\n") {
		idx := strings.Index(line, `"backoff":`)
		if idx < 0 {
			continue
		}
		rest := line[idx+len(`"backoff":`):]
		end := strings.IndexAny(rest, ",}")
		if end < 0 {
			continue
		}
		v, err := strconv.ParseFloat(rest[:end], 64)
		if err != nil {
			t.Fatalf("unparsable backoff field %q in line %q", rest[:end], line)
		}
		out = append(out, v)
	}
	return out
}

func maxFloat(vs []float64) float64 {
	var m float64
	for _, v := range vs {
		if v > m {
			m = v
		}
	}
	return m
}

// TestStreamAuthWedgeRampsBackoffToTheCap is the falsification pin for the two
// backoff clauses this change added — `|| authWedged` on the ramp and
// `case hotRetry:` on the reset. The wedge harness advances a fixed
// wedgeCadence per iteration no matter what `backoff` holds, so every other
// test in this file counts iterations and would stay green with a wedged
// daemon pinned at streamInitialBackoff — the exact tight-reconnect loop the
// ramp comment says it prevents. This one reads the loop's own backoff field.
func TestStreamAuthWedgeRampsBackoffToTheCap(t *testing.T) {
	capMillis := float64(streamMaxBackoff / time.Millisecond)

	t.Run("post-handshake rejection ramps to the cap", func(t *testing.T) {
		h := newWedgeHarness(t, nil, withRejectOnReceive())

		// 1s -> 2 -> 4 -> 8 -> 16 -> 30 (capped) needs six ramps; run well
		// past that so the assertion is about the cap, not the arithmetic.
		h.steps(12)

		got := maxFloat(backoffMillis(t, h.logs.String()))
		if got != capMillis {
			t.Fatalf("max backoff over 12 wedged reconnects = %vms, want %vms; a wedged daemon that reaches the handshake is not ramping, so it tight-loops against bosso\nlogs=%s", got, capMillis, h.logs.String())
		}
	})

	t.Run("a succeeding re-register does not pin the backoff at the floor", func(t *testing.T) {
		var rotations int
		var mu sync.Mutex
		h := newWedgeHarness(t, func(cfg *StreamClientConfig) {
			cfg.ReRegister = func(context.Context) (string, error) {
				mu.Lock()
				defer mu.Unlock()
				rotations++
				return "rotated-token-" + strconv.Itoa(rotations), nil
			}
		}, withRejectOnReceive())

		h.steps(12)

		vals := backoffMillis(t, h.logs.String())
		if got := maxFloat(vals); got != capMillis {
			t.Fatalf("max backoff over 12 rotate-and-reject reconnects = %vms, want %vms; a rotation that keeps succeeding is resetting the backoff every iteration\nlogs=%s", got, capMillis, h.logs.String())
		}
		// Only the first iteration is allowed the hot retry, so the floor
		// must not reappear once the ramp has left it.
		floor := float64(streamInitialBackoff / time.Millisecond)
		for i, v := range vals {
			if i > 1 && v == floor && maxFloat(vals[:i]) > floor {
				t.Fatalf("backoff returned to the %vms floor at log line %d after ramping; the hot retry is not bounded to the first failure\nvals=%v", floor, i, vals)
			}
		}
	})
}

// TestStreamAuthSustainedStreamClearsTheWedge pins the second live recovery
// signal (streamAuthSustainedFor). It is the answer to a gap the inbound-
// command signal cannot cover: bosso has no ping or keepalive in the
// OrchestratorCommand oneof, so a daemon that recovers and then holds an idle
// stream would otherwise report an hours-old wedge forever — a `boss daemon
// doctor` FAIL that survives the `boss login` it recommends.
func TestStreamAuthSustainedStreamClearsTheWedge(t *testing.T) {
	newClient := func() (*StreamClient, *fakeClock) {
		clock := newFakeClock()
		return NewStreamClient(StreamClientConfig{
			Opener: newWedgeOpener(),
			Events: NewStreamBus(zerolog.Nop()),
			Clock:  clock,
			Logger: zerolog.Nop(),
		}), clock
	}

	t.Run("an open stream is not proof until it has lasted long enough", func(t *testing.T) {
		c, clock := newClient()
		note := c.noteAuthFailure()
		if note.Since.IsZero() {
			t.Fatal("noteAuthFailure did not stamp a failure start")
		}
		c.markConnected()

		// One millisecond short of the threshold: a rejected daemon's stream
		// dies inside this window, so nothing may be inferred yet.
		clock.Advance(streamAuthSustainedFor - time.Millisecond)
		if got := c.AuthSnapshot(); got.AuthFailingSince.IsZero() {
			t.Fatalf("AuthSnapshot cleared the wedge after only %v of open stream; a header-rejected stream lives about that long", streamAuthSustainedFor-time.Millisecond)
		}

		clock.Advance(time.Millisecond)
		if got := c.AuthSnapshot(); !got.AuthFailingSince.IsZero() {
			t.Fatalf("AuthSnapshot still reports a wedge (%v) after the stream stayed open for %v; an idle healthy stream never delivers an inbound command, so this is the only signal left", got.AuthFailingSince, streamAuthSustainedFor)
		}
	})

	t.Run("a sustained stream resets the state machine when it ends", func(t *testing.T) {
		c, clock := newClient()
		first := c.noteAuthFailure()
		c.markConnected()
		clock.Advance(streamAuthSustainedFor)
		clock.Advance(time.Hour)
		c.markStreamClosed()

		next := c.noteAuthFailure()
		if !next.First {
			t.Fatal("the failure after a sustained stream did not warn as a first; its opening WARN would be suppressed for a full budget")
		}
		if !next.Since.After(first.Since) {
			t.Fatalf("auth failure start stayed at %v after a stream that authenticated for an hour; the doctor would over-report the outage", first.Since)
		}
	})

	t.Run("a stream that dies immediately preserves the wedge", func(t *testing.T) {
		c, _ := newClient()
		first := c.noteAuthFailure()
		c.markConnected()
		c.markStreamClosed()

		next := c.noteAuthFailure()
		if next.First {
			t.Fatal("a stream that was rejected on the handshake reset the wedge; this is the BOS-942 shape")
		}
		if !next.Since.Equal(first.Since) {
			t.Fatalf("auth failure start moved from %v to %v across a rejected attempt", first.Since, next.Since)
		}
	})
}
