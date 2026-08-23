package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/rs/zerolog"
)

// switchSpy is the switchAccountFn seam wired for the budget/serialization
// tests: it records how many flights actually started, what deadline each one
// arrived with, and lets a test hold a flight open so a second caller can be
// observed joining it rather than starting its own.
type switchSpy struct {
	mu        sync.Mutex
	calls     int
	deadlines []time.Duration
	params    []session.SwitchAccountParams

	entered chan struct{} // closed-ish signal: one send per flight entered
	release chan struct{} // flights block until this is closed

	result session.SwitchAccountResult
	err    error
}

func newSwitchSpy() *switchSpy {
	return &switchSpy{
		entered: make(chan struct{}, 8),
		release: make(chan struct{}),
		result:  session.SwitchAccountResult{TargetLabel: "leader"},
	}
}

func (s *switchSpy) switchFn(ctx context.Context, p session.SwitchAccountParams) (session.SwitchAccountResult, error) {
	s.mu.Lock()
	s.calls++
	s.params = append(s.params, p)
	if d, ok := ctx.Deadline(); ok {
		s.deadlines = append(s.deadlines, time.Until(d))
	} else {
		s.deadlines = append(s.deadlines, 0)
	}
	s.mu.Unlock()

	s.entered <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
		// Surface the cancellation as the real SwitchAccount would: every store,
		// tmux and plugin call on that path refuses a dead context, so the switch
		// comes back as an error rather than a result. Returning s.result here
		// instead — as this stub first did — makes a cancelled flight
		// indistinguishable from a completed one, and every "the flight survived"
		// assertion then passes whether or not the flight was detached.
		return session.SwitchAccountResult{}, ctx.Err()
	}
	return s.result, s.err
}

func (s *switchSpy) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// withSwitchAttach wires the switchFlightAttached seam onto s and returns the
// channel it reports attachments on. ONLY the two *WithAttach constructors go
// through it — every other server in this file keeps switchFlightAttached nil,
// which is the shape production actually runs: executeAccountSwitch builds no
// opts slice and hands detach.Flight no WithAttachHook at all. Hooking every
// server would have made the ~11 tests that assert on production behaviour
// assert it against the instrumented variant instead, and would have made
// TestSwitchFlightAttachedIsAssignedOnlyByTests and the "a nil hook still
// completes a switch" subtest gates over a shape nothing else in the file ran.
//
// The seam replaces the four sleeps this file used to carry: three that gave a
// singleflight JOINER time to attach to the leader's live flight before the
// held leader was released, and one that drained for a second publish. A
// caller that missed the 50ms window started its own flight and failed an
// assertion the production code never violated.
func withSwitchAttach(t *testing.T, s *Server) (*Server, chan string) {
	t.Helper()
	// Capacity 2, which IS the bound: one signal per caller, and every test
	// that takes a hooked server runs at most one leader plus one joiner. A
	// test that adds a THIRD caller must raise this — the default arm below
	// says so at the moment it matters.
	//
	// The send is NON-BLOCKING, and that is the point. This hook runs
	// synchronously inside executeAccountSwitch's detach.Flight call, on the
	// switching goroutine itself, so a blocking send on a full channel would
	// not fail the test — it would HANG the switch inside production code with
	// no diagnostic until the package timeout killed the binary, which is a
	// strictly worse failure than the 50ms flake this seam removed. The
	// default arm turns overflow into a named failure instead. t.Errorf is
	// goroutine-safe, so calling it from the flight's caller is legal.
	//
	// The reporting arm does assume the overflow happens while the test is
	// still running, and that assumption is what confines the hook to the
	// *WithAttach constructors: every test holding a hooked server drains one
	// signal per caller it starts before it returns, so no invocation can
	// outlive its test and no successful send can be the last one. A test that
	// starts a caller it does not wait for would put t.Errorf on a post-return
	// goroutine, where it panics with "Log in goroutine after test has
	// completed" — so drain every caller, or do not hook the server.
	attached := make(chan string, 2)
	s.switchFlightAttached = func(agentSessionID string) {
		select {
		case attached <- agentSessionID:
		default:
			// cap(attached) rather than a literal: a message that named the
			// capacity independently of the channel would go stale the first
			// time someone changed one of them.
			t.Errorf("the attach channel overflowed its capacity of %d (the bound is one signal per "+
				"caller: one leader plus one joiner); agentSessionID=%q was dropped. Drain the channel "+
				"in this test or raise the capacity — a blocking send here would have hung the switch "+
				"instead", cap(attached), agentSessionID)
		}
	}
	return s, attached
}

// newSwitchBudgetServer builds the production shape: switchFlightAttached is
// nil, so executeAccountSwitch passes detach.Flight no option and the flight is
// the unmodified primitive. Tests that assert on production behaviour must come
// through here rather than through the *WithAttach variant.
func newSwitchBudgetServer(t *testing.T, fn func(context.Context, session.SwitchAccountParams) (session.SwitchAccountResult, error)) *Server {
	t.Helper()
	return &Server{switchAccountFn: fn, logger: zerolog.Nop()}
}

// newSwitchBudgetServerWithAttach is newSwitchBudgetServer for the tests that
// actually wait on attachment. It is a separate constructor rather than a
// widened return on the original so the call sites that do not care keep the
// production shape.
func newSwitchBudgetServerWithAttach(
	t *testing.T,
	fn func(context.Context, session.SwitchAccountParams) (session.SwitchAccountResult, error),
) (*Server, chan string) {
	t.Helper()
	return withSwitchAttach(t, newSwitchBudgetServer(t, fn))
}

// newSwitchBudgetServerWithTmux is the same server with a tmux client wired, so
// a test can exercise the CONFIGURED half of switchRespawnBudget (BOS-948).
// Every other test here wants the nil-client default and goes through
// newSwitchBudgetServer, which is why the client is a separate constructor
// rather than a parameter on all twelve call sites.
func newSwitchBudgetServerWithTmux(
	t *testing.T,
	fn func(context.Context, session.SwitchAccountParams) (session.SwitchAccountResult, error),
	tmuxClient *tmux.Client,
) *Server {
	t.Helper()
	return &Server{switchAccountFn: fn, tmux: tmuxClient, logger: zerolog.Nop()}
}

// waitBothAttached blocks until BOTH callers of a coalesced switch — the leader
// and the joiner — have attached to the flight.
//
// A COUNT, never an ordering: DoChan starts the flight body on a new goroutine,
// so the leader's attach signal and its arrival inside the spy are unordered.
// Each caller signals exactly once, which is what makes a two-receive wait
// order-independent — and what makes the per-caller arity pinned by
// TestExecuteAccountSwitch_AttachHookInvocationCount load-bearing for every
// caller of this helper.
func waitBothAttached(t *testing.T, attached <-chan string) {
	t.Helper()
	// A constant rather than a parameter because unparam flags a parameter every
	// caller passes the same value for. Every call site today runs exactly one
	// leader and one joiner; a test that adds a THIRD caller must give this a
	// parameter back rather than let the extra signal sit undrained.
	const want = 2
	for i := 0; i < want; i++ {
		select {
		case <-attached:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d callers attached to the switch flight — the joiner never attached, "+
				"so it would have started a flight of its own", i, want)
		}
	}
}

// TestExecuteAccountSwitch_AppliesRespawnBudget is R1: the switch route's call
// into the lifecycle (and so into StartTmuxChat) arrives carrying a real
// deadline. Without it the command reaches the daemon on the long-lived stream
// context and the readiness wait runs its full per-attempt budget twice with
// nothing above it to stop.
//
// BOS-948 added the second half: WHICH deadline. The budget is derived from the
// composer-readiness value the daemon's tmux client is configured with, so a
// host that raised session_start_ready_deadline_seconds gets a proportionally
// larger switch budget instead of a wait silently clamped below the number it
// configured. The configured row is the one that could not exist before — with
// a compiled constant both rows produced the same 90s.
func TestExecuteAccountSwitch_AppliesRespawnBudget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		tmux     *tmux.Client
		override time.Duration
		want     time.Duration
	}{
		{
			name: "no tmux client falls back to the default-derived budget",
			want: config.SwitchRespawnBudgetFor(config.DefaultSessionStartReadyDeadline),
		},
		{
			name: "a client on the default readiness derives the same budget",
			tmux: tmux.NewClient(),
			want: config.SwitchRespawnBudgetFor(config.DefaultSessionStartReadyDeadline),
		},
		{
			name: "a client configured at 120s derives a materially larger budget",
			tmux: tmux.NewClient(tmux.WithSessionStartReadyDeadline(120 * time.Second)),
			want: config.SwitchRespawnBudgetFor(120 * time.Second),
		},
		{
			name:     "a test override replaces only the magnitude",
			tmux:     tmux.NewClient(tmux.WithSessionStartReadyDeadline(120 * time.Second)),
			override: 150 * time.Millisecond,
			want:     150 * time.Millisecond,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			spy := newSwitchSpy()
			close(spy.release)
			s := newSwitchBudgetServerWithTmux(t, spy.switchFn, tt.tmux)
			s.switchRespawnBudgetOverride = tt.override

			if _, err := s.executeAccountSwitch(context.Background(), "sess-1", "agent-1", "acct-2", false); err != nil {
				t.Fatalf("executeAccountSwitch: %v", err)
			}

			spy.mu.Lock()
			defer spy.mu.Unlock()
			if len(spy.deadlines) != 1 {
				t.Fatalf("switch calls = %d, want 1", len(spy.deadlines))
			}
			got := spy.deadlines[0]
			if got == 0 {
				t.Fatal("the switch ran with NO deadline — the respawn budget was not applied, " +
					"so the readiness wait inside StartTmuxChat is unbounded on this route again")
			}
			// Remaining, not exact: some wall clock elapses between WithTimeout
			// and the read. A second of slack is far tighter than the gap
			// between any two rows here.
			if got > tt.want || got < tt.want-time.Second {
				t.Errorf("switch deadline = %v, want ~%v", got, tt.want)
			}
		})
	}

	// The rows are only evidence if the configured one is distinguishable from
	// the default one. Assert the gap rather than trusting the table: a
	// derivation accidentally reduced back to a constant would pass every row
	// above by coincidence.
	base := config.SwitchRespawnBudgetFor(config.DefaultSessionStartReadyDeadline)
	raised := config.SwitchRespawnBudgetFor(120 * time.Second)
	if raised-base < 30*time.Second {
		t.Fatalf("a 120s configured readiness produced %v against the default's %v — a gap of %v.\n"+
			"The switch budget is supposed to SCALE with session_start_ready_deadline_seconds (BOS-948); "+
			"a gap this small means it has drifted back toward a fixed value.",
			raised, base, raised-base)
	}
}

// TestExecuteAccountSwitch_SecondSwitchJoinsTheFlight is R4's mechanism. Two
// concurrent switches for one chat must produce exactly ONE switch: the second
// caller joins the leader instead of running its own, so there is no losing
// attempt left to tear down the pane the winner established.
//
// It also pins the coalescing semantic the choice buys: keyed on
// agentSessionID, a joiner asking for a DIFFERENT target account receives the
// leader's result. That is chosen, not discovered — see executeAccountSwitch.
func TestExecuteAccountSwitch_SecondSwitchJoinsTheFlight(t *testing.T) {
	t.Parallel()

	spy := newSwitchSpy()
	s, attached := newSwitchBudgetServerWithAttach(t, spy.switchFn)

	leaderDone := make(chan switchOutcome, 1)
	go func() {
		res, err := s.executeAccountSwitch(context.Background(), "sess-1", "agent-1", "acct-leader", false)
		leaderDone <- switchOutcome{res, err}
	}()

	select {
	case <-spy.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the leader never entered the switch")
	}

	joinerDone := make(chan switchOutcome, 1)
	go func() {
		// A different target account on purpose: the group keys on the chat.
		res, err := s.executeAccountSwitch(context.Background(), "sess-1", "agent-1", "acct-joiner", false)
		joinerDone <- switchOutcome{res, err}
	}()

	// Both callers have ATTACHED to the flight — observed, not slept for. The
	// leader is safe to release only now: released earlier, singleflight would
	// delete the key before the joiner arrived and the joiner would start a
	// second flight, failing the assertion below against correct production
	// code.
	waitBothAttached(t, attached)
	close(spy.release)

	for _, got := range []switchOutcome{waitOutcome(t, leaderDone), waitOutcome(t, joinerDone)} {
		if got.err != nil {
			t.Fatalf("executeAccountSwitch: %v", got.err)
		}
		if got.res.TargetLabel != "leader" {
			t.Errorf("TargetLabel = %q, want the leader's result %q", got.res.TargetLabel, "leader")
		}
	}
	if got := spy.callCount(); got != 1 {
		t.Fatalf("switch calls = %d, want exactly 1 — a second flight means a losing attempt exists "+
			"that can tear down the winner's pane", got)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if spy.params[0].TargetAccountID != "acct-leader" {
		t.Errorf("the flight ran for %q, want the leader's target acct-leader", spy.params[0].TargetAccountID)
	}
}

// TestExecuteAccountSwitch_JoinerBoundedByItsOwnContext is why the group is
// joined with DoChan and a caller-owned select rather than Do. Do ignores a
// joiner's context, which would pin a 30s-relay joiner behind a 90s leader —
// re-creating on the joiner exactly the "outlives its caller" defect this
// change removes. The joiner must return on its own deadline, and it must not
// have started a switch of its own to tear anything down with.
func TestExecuteAccountSwitch_JoinerBoundedByItsOwnContext(t *testing.T) {
	t.Parallel()

	spy := newSwitchSpy()
	s := newSwitchBudgetServer(t, spy.switchFn)

	go func() {
		_, _ = s.executeAccountSwitch(context.Background(), "sess-1", "agent-1", "acct-leader", false)
	}()
	select {
	case <-spy.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the leader never entered the switch")
	}
	defer close(spy.release)

	joinerCtx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := s.executeAccountSwitch(joinerCtx, "sess-1", "agent-1", "acct-joiner", false)
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("the joiner returned success while the leader was still in flight")
	}
	if got := connect.CodeOf(err); got != connect.CodeDeadlineExceeded {
		t.Errorf("code = %v, want DeadlineExceeded", got)
	}
	if elapsed > 2*time.Second {
		t.Errorf("the joiner waited %v — it inherited the leader's clock instead of its own", elapsed)
	}
	if got := spy.callCount(); got != 1 {
		t.Errorf("switch calls = %d, want 1: the abandoning joiner must not have run a switch of its own", got)
	}
}

// TestExecuteAccountSwitch_DifferentChatsDoNotSerialize keeps the gate as
// narrow as the harm. The teardown race is per-chat (tmux.ChatSessionName is
// pure over repoID and agentSessionID), so serializing across chats would only
// queue unrelated users behind each other.
func TestExecuteAccountSwitch_DifferentChatsDoNotSerialize(t *testing.T) {
	t.Parallel()

	spy := newSwitchSpy()
	s := newSwitchBudgetServer(t, spy.switchFn)

	for _, chat := range []string{"agent-1", "agent-2"} {
		go func() {
			_, _ = s.executeAccountSwitch(context.Background(), "sess-1", chat, "acct-2", false)
		}()
	}
	defer close(spy.release)

	for i := 0; i < 2; i++ {
		select {
		case <-spy.entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 2 switches for DIFFERENT chats were in flight at once — "+
				"the singleflight key is coarser than agentSessionID", i)
		}
	}
}

func TestExpireSwitchBudgetForTest_RealExpiryMapsToDeadlineExceeded(t *testing.T) {
	t.Parallel()

	const budget = 150 * time.Millisecond
	start := time.Now()
	err := ExpireSwitchBudgetForTest(context.Background(), budget)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected budget expiry")
	}
	if got := connect.CodeOf(err); got != connect.CodeDeadlineExceeded {
		t.Fatalf("code = %v, want DeadlineExceeded; err=%v", got, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline cause", err)
	}
	if !strings.Contains(err.Error(), "switch session account:") {
		t.Fatalf("error = %q, want switch session account provenance", err.Error())
	}
	if elapsed < budget || elapsed > 2*time.Second {
		t.Fatalf("elapsed = %v, want real budget expiry near %v", elapsed, budget)
	}
}

// TestExecuteAccountSwitch_DeadlineMapsToDeadlineExceeded is R6. Answering a
// caller whose own budget ended the request with a retryable-looking
// CodeInternal is the failure BOS-747 names; the context arm must sit ahead of
// the default arm that would otherwise claim it.
func TestExecuteAccountSwitch_DeadlineMapsToDeadlineExceeded(t *testing.T) {
	t.Parallel()

	spy := newSwitchSpy()
	close(spy.release)
	// The shape the real path produces: the readiness wait's context error,
	// wrapped by SwitchAccount's respawn message.
	spy.err = fmt.Errorf("respawn chat after switch: %w", context.DeadlineExceeded)
	s := newSwitchBudgetServer(t, spy.switchFn)

	_, err := s.executeAccountSwitch(context.Background(), "sess-1", "agent-1", "acct-2", false)
	if err == nil {
		t.Fatal("expected the switch to fail")
	}
	if got := connect.CodeOf(err); got != connect.CodeDeadlineExceeded {
		t.Errorf("code = %v, want DeadlineExceeded (CodeInternal would invite a retry of a switch "+
			"whose budget simply ran out)", got)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the mapped error dropped the context cause: %v", err)
	}
}

// TestExecuteAccountSwitch_ReadinessTimeoutStaysInternal is the other side of
// the arm above, and the reason it can be added safely. A readiness wait that
// exhausts its OWN budget returns a diagnostic timeout wrapping no context
// error, so it must still land on the default arm rather than being relabelled
// as the caller's deadline.
func TestExecuteAccountSwitch_ReadinessTimeoutStaysInternal(t *testing.T) {
	t.Parallel()

	spy := newSwitchSpy()
	close(spy.release)
	spy.err = errors.New(`respawn chat after switch: wait for ready marker on "bossd-agent-run-agent-1": ` +
		`ready marker "❯" not seen in pane within 45s; last pane (truncated): Welcome to Claude`)
	s := newSwitchBudgetServer(t, spy.switchFn)

	_, err := s.executeAccountSwitch(context.Background(), "sess-1", "agent-1", "acct-2", false)
	if err == nil {
		t.Fatal("expected the switch to fail")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Errorf("code = %v, want Internal — a readiness timeout is a diagnostic verdict, not a context expiry", got)
	}
}

// TestExecuteAccountSwitch_FlightPanicDoesNotCrashTheDaemon pins the one
// property DoChan does NOT share with Do. singleflight re-raises a flight panic
// on the caller's goroutine only while the call has no result channels; DoChan
// always seeds one, so the branch it takes is `go panic(e); select{}` — a bare
// goroutine that neither net/http's per-connection recover nor safego.Go can
// reach. Recovering inside the flight body is what keeps a nil-deref anywhere
// in SwitchAccount's long call chain from taking the whole daemon down.
//
// Without the recover this test does not fail, it CRASHES the test binary — an
// unrecoverable panic is exactly what is being prevented, so there is no
// gentler observation available. A surviving process that reports the panic as
// an error is the whole assertion.
func TestExecuteAccountSwitch_FlightPanicDoesNotCrashTheDaemon(t *testing.T) {
	t.Parallel()

	s := newSwitchBudgetServer(t, func(context.Context, session.SwitchAccountParams) (session.SwitchAccountResult, error) {
		panic("boom from inside the switch")
	})

	_, err := s.executeAccountSwitch(context.Background(), "sess-1", "agent-panic", "acct-2", false)
	if err == nil {
		t.Fatal("a panicking switch returned no error")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Errorf("code = %v, want Internal", got)
	}
	if !strings.Contains(err.Error(), "boom from inside the switch") {
		t.Errorf("the recovered panic lost its cause: %v", err)
	}
}

// TestExecuteAccountSwitch_LeaderAbandoningDoesNotKillTheFlight is why the
// flight is derived from context.WithoutCancel rather than from the caller's
// context. `ctx` inside the flight body belongs to whichever caller happened to
// WIN the race to be leader, so a flight built on it dies when that particular
// caller hangs up — cancelling work a healthy joiner is still waiting on, and
// leaving the respawn budget as something any caller can silently pre-empt
// rather than the respawn's own.
//
// The leader here abandons immediately; the joiner's context stays healthy and
// must still receive the flight's real result.
func TestExecuteAccountSwitch_LeaderAbandoningDoesNotKillTheFlight(t *testing.T) {
	t.Parallel()

	spy := newSwitchSpy()
	s, attached := newSwitchBudgetServerWithAttach(t, spy.switchFn)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan switchOutcome, 1)
	go func() {
		res, err := s.executeAccountSwitch(leaderCtx, "sess-1", "agent-1", "acct-leader", false)
		leaderDone <- switchOutcome{res, err}
	}()
	select {
	case <-spy.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the leader never entered the switch")
	}

	joinerDone := make(chan switchOutcome, 1)
	go func() {
		res, err := s.executeAccountSwitch(context.Background(), "sess-1", "agent-1", "acct-joiner", false)
		joinerDone <- switchOutcome{res, err}
	}()
	// Both callers attached. The old joinerReady channel proved only that the
	// joiner's GOROUTINE had started, which is why a sleep had to follow it;
	// attachment is the property the test actually needs, so the channel is
	// redundant now that attachment itself is observable.
	waitBothAttached(t, attached)

	// The leader hangs up. The flight must not go with it.
	cancelLeader()
	if got := waitOutcome(t, leaderDone); got.err == nil {
		t.Error("the abandoning leader returned success rather than its own cancellation")
	} else if code := connect.CodeOf(got.err); code != connect.CodeCanceled {
		t.Errorf("abandoning leader code = %v, want Canceled", code)
	}

	close(spy.release)

	got := waitOutcome(t, joinerDone)
	if got.err != nil {
		t.Fatalf("the healthy joiner inherited the leader's cancellation: %v", got.err)
	}
	if got.res.TargetLabel != "leader" {
		t.Errorf("TargetLabel = %q, want the leader's result %q", got.res.TargetLabel, "leader")
	}
	if got := spy.callCount(); got != 1 {
		t.Errorf("switch calls = %d, want 1", got)
	}
}

// TestExecuteAccountSwitch_EmptyChatKeyIsRefused makes the singleflight key's
// safety LOCAL. Today both production callers resolve a real chat before they
// reach here, so this is an assertion rather than a reachable path — but the
// failure it guards is silent and cross-session, which is why it is not left
// to inheritance: on an empty key every concurrent switch coalesces into ONE
// flight regardless of chat or session, so a switch for session B would return
// session A's result and never run its own.
func TestExecuteAccountSwitch_EmptyChatKeyIsRefused(t *testing.T) {
	t.Parallel()

	spy := newSwitchSpy()
	s := newSwitchBudgetServer(t, spy.switchFn)
	defer close(spy.release)

	_, err := s.executeAccountSwitch(context.Background(), "sess-1", "", "acct-2", false)
	if err == nil {
		t.Fatal("an empty agentSessionID was accepted as a singleflight key")
	}
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", got)
	}
	if got := spy.callCount(); got != 0 {
		t.Errorf("switch calls = %d, want 0 — an empty key seeded a flight every other chat would join", got)
	}
}

// TestExecuteAccountSwitch_DeadCallerStartsNoFlight fences the one hazard the
// detached flight creates. DoChan runs before the caller-owned select, so
// without a pre-check a caller whose context had ALREADY ended would still
// seed a flight — and because that flight is detached, it would spend the full
// budget killing and respawning the chat's pane on behalf of a caller that had
// already gone. Failing fast is what the undetached version did for free.
//
// The guard is deliberately narrow, and the two tests either side of this one
// are what keep it that way: a joiner arriving with a dead context still
// attaches (it starts nothing), and a caller that leaves MID-flight still
// leaves the leader running.
func TestExecuteAccountSwitch_DeadCallerStartsNoFlight(t *testing.T) {
	t.Parallel()

	// release is deliberately NOT closed: a flight that does start then BLOCKS
	// inside the spy, so its entry is observable. Releasing it instead would
	// let the flight finish before the assertion ran and make a started flight
	// indistinguishable from no flight at all — this test read as green against
	// the unguarded code until that was fixed.
	spy := newSwitchSpy()
	s := newSwitchBudgetServer(t, spy.switchFn)
	defer close(spy.release)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // dead before the call

	_, err := s.executeAccountSwitch(ctx, "sess-1", "agent-dead", "acct-2", false)
	if err == nil {
		t.Fatal("expected an error for an already-cancelled caller")
	}
	if got := connect.CodeOf(err); got != connect.CodeCanceled {
		t.Errorf("code = %v, want Canceled", got)
	}
	select {
	case <-spy.entered:
		t.Fatal("an already-dead caller seeded a detached flight — it will spend the whole budget " +
			"killing and respawning the pane on behalf of a caller that had already gone")
	case <-time.After(250 * time.Millisecond):
		// Nothing entered the switch, which is the assertion.
	}
}

// TestExecuteAccountSwitch_CallerCancelMapsToCanceled keeps the ctx.Done arm
// honest about WHICH context error it saw. A caller that hung up on purpose
// must not be told DeadlineExceeded, which reads as "retry me" — the same
// timeout-vs-cancel split acquireRepoMerge's queued-merge wait already makes.
func TestExecuteAccountSwitch_CallerCancelMapsToCanceled(t *testing.T) {
	t.Parallel()

	spy := newSwitchSpy()
	s := newSwitchBudgetServer(t, spy.switchFn)
	defer close(spy.release)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-spy.entered
		cancel()
	}()

	_, err := s.executeAccountSwitch(ctx, "sess-1", "agent-cancel", "acct-2", false)
	if err == nil {
		t.Fatal("expected the cancelled caller to get an error")
	}
	if got := connect.CodeOf(err); got != connect.CodeCanceled {
		t.Errorf("code = %v, want Canceled (DeadlineExceeded invites a retry of a request the caller abandoned)", got)
	}
}

// TestExecuteAccountSwitch_AttachHookInvocationCount pins the seam's own
// contract, which the three coalescing tests above now DEPEND on: they wait for
// a COUNT of attach signals, so a hook that fired once per flight rather than
// once per caller would hang them, and one that fired on a refusal path would
// let them pass vacuously.
func TestExecuteAccountSwitch_AttachHookInvocationCount(t *testing.T) {
	t.Parallel()

	t.Run("a single switch attaches exactly one caller", func(t *testing.T) {
		t.Parallel()

		spy := newSwitchSpy()
		close(spy.release)
		s, attached := newSwitchBudgetServerWithAttach(t, spy.switchFn)

		if _, err := s.executeAccountSwitch(context.Background(), "sess-1", "agent-1", "acct-2", false); err != nil {
			t.Fatalf("executeAccountSwitch: %v", err)
		}
		// The hook fires before the caller-owned select, so by the time
		// executeAccountSwitch has returned its signal is already queued.
		if n := len(attached); n != 1 {
			t.Fatalf("attach signals = %d, want exactly 1 — the hook is per CALLER", n)
		}
		if got := <-attached; got != "agent-1" {
			t.Errorf("attach signal carried %q, want the chat's agentSessionID agent-1", got)
		}
	})

	t.Run("a joined switch attaches both callers", func(t *testing.T) {
		t.Parallel()

		spy := newSwitchSpy()
		s, attached := newSwitchBudgetServerWithAttach(t, spy.switchFn)

		leaderDone := make(chan switchOutcome, 1)
		go func() {
			res, err := s.executeAccountSwitch(context.Background(), "sess-1", "agent-1", "acct-2", false)
			leaderDone <- switchOutcome{res, err}
		}()
		select {
		case <-spy.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the leader never entered the switch")
		}

		joinerDone := make(chan switchOutcome, 1)
		go func() {
			res, err := s.executeAccountSwitch(context.Background(), "sess-1", "agent-1", "acct-2", false)
			joinerDone <- switchOutcome{res, err}
		}()

		waitBothAttached(t, attached)
		close(spy.release)
		for _, got := range []switchOutcome{waitOutcome(t, leaderDone), waitOutcome(t, joinerDone)} {
			if got.err != nil {
				t.Fatalf("executeAccountSwitch: %v", got.err)
			}
		}
		// Two received above and none left over is the "exactly 2" half: a
		// per-flight hook would have produced one and hung waitAttached, and a
		// hook firing more than once per caller would show up here.
		if n := len(attached); n != 0 {
			t.Errorf("attach signals = %d beyond the expected 2, want 0", n)
		}
		if n := spy.callCount(); n != 1 {
			t.Fatalf("switch flights = %d, want 1 — the callers did not coalesce, so the count above "+
				"was two flights rather than two attachments to one", n)
		}
	})

	t.Run("the empty-key refusal attaches nobody", func(t *testing.T) {
		t.Parallel()

		// release stays open: a flight that wrongly started would block inside
		// the spy rather than completing and hiding itself.
		spy := newSwitchSpy()
		s, attached := newSwitchBudgetServerWithAttach(t, spy.switchFn)
		defer close(spy.release)

		if _, err := s.executeAccountSwitch(context.Background(), "sess-1", "", "acct-2", false); err == nil {
			t.Fatal("an empty agentSessionID was accepted as a singleflight key")
		}
		if n := len(attached); n != 0 {
			t.Errorf("attach signals = %d, want 0 — the refusal returns before DoChan, so there is "+
				"no flight to have attached to", n)
		}
	})

	t.Run("the already-dead caller attaches nobody", func(t *testing.T) {
		t.Parallel()

		spy := newSwitchSpy()
		s, attached := newSwitchBudgetServerWithAttach(t, spy.switchFn)
		defer close(spy.release)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if _, err := s.executeAccountSwitch(ctx, "sess-1", "agent-dead", "acct-2", false); err == nil {
			t.Fatal("expected an error for an already-cancelled caller")
		}
		if n := len(attached); n != 0 {
			t.Errorf("attach signals = %d, want 0 — the dead-caller fence also returns before DoChan", n)
		}
	})

	t.Run("a nil hook still completes a switch", func(t *testing.T) {
		t.Parallel()

		// Constructed inline rather than through newSwitchBudgetServer so this
		// subtest's evidence does not rest on what a shared constructor happens
		// to wire: the literal below is visibly the production shape, where
		// executeAccountSwitch passes detach.Flight no option at all.
		spy := newSwitchSpy()
		close(spy.release)
		s := &Server{switchAccountFn: spy.switchFn, logger: zerolog.Nop()}
		if s.switchFlightAttached != nil {
			t.Fatal("this subtest is only evidence with the hook unset")
		}

		res, err := s.executeAccountSwitch(context.Background(), "sess-1", "agent-1", "acct-2", false)
		if err != nil {
			t.Fatalf("executeAccountSwitch with no attach hook: %v", err)
		}
		if res.TargetLabel != "leader" {
			t.Errorf("TargetLabel = %q, want %q", res.TargetLabel, "leader")
		}
		if n := spy.callCount(); n != 1 {
			t.Errorf("switch calls = %d, want 1", n)
		}
	})
}

// switchOutcome pairs a switch's two returns so a goroutine can hand both back
// over one channel.
type switchOutcome struct {
	res session.SwitchAccountResult
	err error
}

func waitOutcome(t *testing.T, ch chan switchOutcome) switchOutcome {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a switch to return")
		panic("unreachable")
	}
}

// newSwitchPublishServer is newSwitchBudgetServer plus the wiring the rebound
// publish needs: a session to load, a repo store for sessionProtoWithRepo, and
// a counting onSessionUpdated. published receives one value per delta emitted.
// It too leaves switchFlightAttached nil; the attach-waiting test takes the
// *WithAttach variant below.
func newSwitchPublishServer(
	t *testing.T,
	fn func(context.Context, session.SwitchAccountParams) (session.SwitchAccountResult, error),
	published chan<- string,
) *Server {
	t.Helper()
	return &Server{
		switchAccountFn:  fn,
		logger:           zerolog.Nop(),
		sessions:         &lifecycleSessionStoreFake{session: &models.Session{ID: "sess-1"}},
		repos:            updateSessionRepoStoreFake{},
		onSessionUpdated: func(_ context.Context, p *pb.Session) { published <- p.GetId() },
	}
}

// newSwitchPublishServerWithAttach is newSwitchPublishServer for the test that
// waits on attachment, on the same "separate constructor, untouched call sites"
// footing as newSwitchBudgetServerWithAttach.
func newSwitchPublishServerWithAttach(
	t *testing.T,
	fn func(context.Context, session.SwitchAccountParams) (session.SwitchAccountResult, error),
	published chan<- string,
) (*Server, chan string) {
	t.Helper()
	return withSwitchAttach(t, newSwitchPublishServer(t, fn, published))
}

// TestExecuteAccountSwitch_AbandonedSwitchStillPublishes covers the state the
// detach newly made reachable: every caller leaves, and the flight succeeds
// anyway.
//
// The publish used to sit below the select, which the <-ctx.Done() arm returns
// past. So this switch changed the durable sessions.account_id and streamed
// NOTHING, leaving the web account badge on the old account until the next full
// daemon snapshot. Before the flight was detached the state could not arise —
// the abandoned switch died with its caller — so this is a regression the
// branch itself introduced, not a pre-existing gap.
func TestExecuteAccountSwitch_AbandonedSwitchStillPublishes(t *testing.T) {
	t.Parallel()

	spy := newSwitchSpy()
	published := make(chan string, 4)
	s := newSwitchPublishServer(t, spy.switchFn, published)

	ctx, cancel := context.WithCancel(context.Background())
	callerDone := make(chan switchOutcome, 1)
	go func() {
		res, err := s.executeAccountSwitch(ctx, "sess-1", "agent-abandon", "acct-2", false)
		callerDone <- switchOutcome{res, err}
	}()

	<-spy.entered // the flight is running
	cancel()      // ...and now nobody is waiting for it

	got := waitOutcome(t, callerDone)
	if connect.CodeOf(got.err) != connect.CodeCanceled {
		t.Fatalf("abandoning caller err = %v (code %v), want CodeCanceled", got.err, connect.CodeOf(got.err))
	}

	close(spy.release) // let the detached flight finish

	select {
	case id := <-published:
		if id != "sess-1" {
			t.Errorf("published session id = %q, want sess-1", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the switch succeeded after every caller left but published NO session delta; " +
			"sessions.account_id changed durably and the web account badge stays stale until the " +
			"next full daemon snapshot — publish from inside the flight, not below the select")
	}
}

// TestExecuteAccountSwitch_JoinedSwitchPublishesExactlyOnce is the other half of
// moving the publish inside the flight. Below the select every caller ran it,
// so N callers coalescing into ONE switch still emitted N identical deltas.
// Inside the flight the leader emits one and joiners emit none.
func TestExecuteAccountSwitch_JoinedSwitchPublishesExactlyOnce(t *testing.T) {
	t.Parallel()

	spy := newSwitchSpy()
	published := make(chan string, 8)
	s, attached := newSwitchPublishServerWithAttach(t, spy.switchFn, published)

	leaderDone := make(chan switchOutcome, 1)
	go func() {
		res, err := s.executeAccountSwitch(context.Background(), "sess-1", "agent-once", "acct-2", false)
		leaderDone <- switchOutcome{res, err}
	}()
	<-spy.entered

	joinerDone := make(chan switchOutcome, 1)
	go func() {
		res, err := s.executeAccountSwitch(context.Background(), "sess-1", "agent-once", "acct-2", false)
		joinerDone <- switchOutcome{res, err}
	}()
	// Both callers attached, so releasing the leader now cannot let the joiner
	// miss the flight and start its own.
	waitBothAttached(t, attached)
	close(spy.release)

	for _, got := range []switchOutcome{waitOutcome(t, leaderDone), waitOutcome(t, joinerDone)} {
		if got.err != nil {
			t.Fatalf("switch returned %v, want success", got.err)
		}
	}
	if n := spy.callCount(); n != 1 {
		t.Fatalf("switch flights = %d, want 1 (the joiner should have coalesced)", n)
	}

	// No drain wait: the publish runs INSIDE the flight body and
	// publishReboundSession is fully synchronous, and singleflight delivers on
	// a caller's result channel only after that body has returned. So by the
	// time both callers above have returned, every publish this switch will
	// ever make has already happened — a second delta, if the publish were
	// still per-caller, is already queued rather than in flight.
	if n := len(published); n != 1 {
		t.Errorf("published deltas = %d, want exactly 1 — one switch must stream one delta, "+
			"however many callers coalesced into it", n)
	}
}
