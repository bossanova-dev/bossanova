package upstream

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/rs/zerolog"
)

// healthHarness bundles a TerminalStreamClient wired with a TerminalHealth
// signal and small liveness timeouts so the ready-handshake + heartbeat +
// watchdog paths are drivable with a real clock in well under a second.
type healthHarness struct {
	opener      *fakeTerminalOpener
	chats       *fakeChatLookup
	factory     *fakeAttachFactory
	health      *TerminalHealth
	client      *TerminalStreamClient
	reRegisters *atomic.Int32
	closeIdles  *atomic.Int32
}

func newHealthHarness(t *testing.T, mutate func(*TerminalStreamClientConfig)) *healthHarness {
	t.Helper()
	opener := &fakeTerminalOpener{}
	tmuxName := "boss-rep-chat-1"
	chats := &fakeChatLookup{rows: map[string]chatRow{"claude-1": {TmuxSessionName: &tmuxName}}}
	factory := newFakeAttachFactory()
	cmdRec := &recordingCmdFactory{}
	tmuxClient := tmux.NewClient(tmux.WithCommandFactory(cmdRec.factory))
	health := NewTerminalHealth()
	reRegisters := &atomic.Int32{}
	closeIdles := &atomic.Int32{}

	cfg := TerminalStreamClientConfig{
		Opener:        opener,
		Chats:         chats,
		TmuxClient:    tmuxClient,
		AttachFactory: factory.build,
		Logger:        zerolog.Nop(),
		Health:        health,
		// Long enough not to fire spuriously; individual tests shorten
		// ReadyTimeout / PingInterval when they exercise those paths.
		ReadyTimeout:       2 * time.Second,
		PingInterval:       2 * time.Second,
		MissedBeatsBudget:  3,
		ReadyTimeoutBudget: 2,
		ReRegister:         func(context.Context) { reRegisters.Add(1) },
		CloseIdle:          func() { closeIdles.Add(1) },
	}
	if mutate != nil {
		mutate(&cfg)
	}
	client := NewTerminalStreamClient(cfg)
	return &healthHarness{
		opener:      opener,
		chats:       chats,
		factory:     factory,
		health:      health,
		client:      client,
		reRegisters: reRegisters,
		closeIdles:  closeIdles,
	}
}

// TestTerminalStreamHealth_HealthyOnReady asserts the daemon only treats
// the stream as healthy after it receives a TerminalReady frame.
func TestTerminalStreamHealth_HealthyOnReady(t *testing.T) {
	t.Parallel()
	h := newHealthHarness(t, nil)
	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)

	// Before ready: not healthy.
	if h.health.Healthy() {
		t.Fatal("stream should not be healthy before TerminalReady")
	}

	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Ready{Ready: &pb.TerminalReady{}},
	}
	waitFor(t, "healthy after TerminalReady", func() bool {
		return h.health.Healthy()
	})
	if got := h.health.Snapshot().ReadyConfirmed; got < 1 {
		t.Fatalf("ReadyConfirmed = %d, want >= 1", got)
	}
}

// TestTerminalStreamHealth_ReadyTimeoutReconnects asserts that a stream
// which never receives TerminalReady within the deadline is torn down and
// the client reconnects (the cross-pod-split signature).
func TestTerminalStreamHealth_ReadyTimeoutReconnects(t *testing.T) {
	t.Parallel()
	h := newHealthHarness(t, func(c *TerminalStreamClientConfig) {
		c.ReadyTimeout = 30 * time.Millisecond
		// Keep the budget high so this test isolates reconnect (no escalation).
		c.ReadyTimeoutBudget = 100
	})
	stop := runClient(t, h.client)
	defer stop()

	// First stream opens, never gets Ready → times out → reconnect.
	waitForStream(t, h.opener)
	waitForN(t, "reconnect after ready-timeout", 3*time.Second, func() bool {
		return h.opener.calls() >= 2
	})
	if h.health.Healthy() {
		t.Fatal("stream must not be healthy when no TerminalReady arrived")
	}
	if got := h.health.Snapshot().ReadyTimeouts; got < 1 {
		t.Fatalf("ReadyTimeouts = %d, want >= 1", got)
	}
}

// TestTerminalStreamHealth_MissedHeartbeatTeardown asserts that once a
// stream is healthy, a run of missed heartbeats (no TerminalPing) tears it
// down and reconnects.
func TestTerminalStreamHealth_MissedHeartbeatTeardown(t *testing.T) {
	t.Parallel()
	h := newHealthHarness(t, func(c *TerminalStreamClientConfig) {
		c.ReadyTimeout = 2 * time.Second
		c.PingInterval = 15 * time.Millisecond
		c.MissedBeatsBudget = 2
	})
	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Ready{Ready: &pb.TerminalReady{}},
	}
	waitFor(t, "healthy after ready", func() bool { return h.health.Healthy() })

	// No pings sent → after MissedBeatsBudget*PingInterval the heartbeat
	// watchdog tears the stream down and the client reconnects.
	waitForN(t, "reconnect after missed heartbeats", 3*time.Second, func() bool {
		return h.opener.calls() >= 2
	})
}

// TestTerminalStreamHealth_PingElicitsPong asserts the daemon answers a
// TerminalPing with a matching TerminalPong.
func TestTerminalStreamHealth_PingElicitsPong(t *testing.T) {
	t.Parallel()
	h := newHealthHarness(t, nil)
	stop := runClient(t, h.client)
	defer stop()

	stream := waitForStream(t, h.opener)
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Ready{Ready: &pb.TerminalReady{}},
	}
	stream.recv <- &pb.TerminalClientMessage{
		Msg: &pb.TerminalClientMessage_Ping{Ping: &pb.TerminalPing{Seq: 7}},
	}
	msg := waitForServerMessage(t, stream)
	pong := msg.GetPong()
	if pong == nil {
		t.Fatalf("expected TerminalPong, got %+v", msg)
	}
	if pong.GetSeq() != 7 {
		t.Errorf("pong.seq = %d, want 7", pong.GetSeq())
	}
}

// TestTerminalStreamHealth_ReadyTimeoutBudgetForcesReRegister asserts that
// after K consecutive ready-timeouts the watchdog forces a paired
// DaemonStream re-register (reRegister) and drops pooled connections
// (CloseIdleConnections) so both reverse streams re-dial and co-locate.
func TestTerminalStreamHealth_ReadyTimeoutBudgetForcesReRegister(t *testing.T) {
	t.Parallel()
	h := newHealthHarness(t, func(c *TerminalStreamClientConfig) {
		c.ReadyTimeout = 20 * time.Millisecond
		c.ReadyTimeoutBudget = 2
	})
	stop := runClient(t, h.client)
	defer stop()

	waitForN(t, "reRegister after K ready-timeouts", 5*time.Second, func() bool {
		return h.reRegisters.Load() >= 1
	})
	if got := h.closeIdles.Load(); got < 1 {
		t.Fatalf("CloseIdle calls = %d, want >= 1 (paired re-dial on fresh connection)", got)
	}
	if got := h.health.Snapshot().ForcedReRegisters; got < 1 {
		t.Fatalf("ForcedReRegisters = %d, want >= 1", got)
	}
}

// TestTerminalStreamHealth_EscalationRetiresStaleConnectionFirst guards the
// recovery ordering: re-registration must not reuse the idle HTTP/2 connection
// that just routed TerminalStream to the wrong bosso pod.
func TestTerminalStreamHealth_EscalationRetiresStaleConnectionFirst(t *testing.T) {
	t.Parallel()
	var calls []string
	h := newHealthHarness(t, func(c *TerminalStreamClientConfig) {
		c.CloseIdle = func() { calls = append(calls, "close-idle") }
		c.ReRegister = func(context.Context) { calls = append(calls, "re-register") }
	})

	h.client.escalateWedged(context.Background())

	if len(calls) != 2 || calls[0] != "close-idle" || calls[1] != "re-register" {
		t.Fatalf("recovery calls = %v, want [close-idle re-register]", calls)
	}
}

// TestTerminalStreamHealth_NotColocatedRejectionCountsTowardEscalation
// asserts that bosso's co-location-tagged CodeFailedPrecondition rejection
// (the daemon's TerminalStream landed on a pod whose DaemonStream is not
// local-and-Ready) is treated like a ready-timeout: it feeds the
// forced-re-register streak, so K of them escalate to a paired re-register.
// A plain transport error must NOT (covered implicitly by the reconnect
// tests, which never escalate).
func TestTerminalStreamHealth_NotColocatedRejectionCountsTowardEscalation(t *testing.T) {
	t.Parallel()
	h := newHealthHarness(t, func(c *TerminalStreamClientConfig) {
		c.ReadyTimeout = 2 * time.Second // long: the rejection, not a timeout, drives it
		c.ReadyTimeoutBudget = 2
	})
	h.opener.nextStream = func() *fakeTerminalStream {
		s := newFakeTerminalStream()
		s.recvErr = connect.NewError(connect.CodeFailedPrecondition,
			errors.New(terminalNotColocatedTag+": daemon not registered on this instance"))
		close(s.recv) // deliver the rejection immediately
		return s
	}
	stop := runClient(t, h.client)
	defer stop()

	waitForN(t, "escalation from tagged co-location rejections", 6*time.Second, func() bool {
		return h.reRegisters.Load() >= 1 && h.closeIdles.Load() >= 1
	})
}

// TestIsTerminalNotColocated_OnlyMatchesTaggedFailedPrecondition guards the
// classifier: only a CodeFailedPrecondition error carrying the tag counts;
// an untagged precondition error or a different code does not.
func TestIsTerminalNotColocated_OnlyMatchesTaggedFailedPrecondition(t *testing.T) {
	t.Parallel()
	tagged := connect.NewError(connect.CodeFailedPrecondition, errors.New(terminalNotColocatedTag+": x"))
	if !isTerminalNotColocated(tagged) {
		t.Error("tagged FailedPrecondition should match")
	}
	untagged := connect.NewError(connect.CodeFailedPrecondition, errors.New("daemon not registered"))
	if isTerminalNotColocated(untagged) {
		t.Error("untagged FailedPrecondition must not match")
	}
	otherCode := connect.NewError(connect.CodeUnavailable, errors.New(terminalNotColocatedTag))
	if isTerminalNotColocated(otherCode) {
		t.Error("non-FailedPrecondition code must not match even with the tag text")
	}
	if isTerminalNotColocated(errors.New("plain error")) {
		t.Error("plain error must not match")
	}
}
