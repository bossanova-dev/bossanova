// Package upstream — terminal_liveness.go holds the BOS-376 positive
// terminal-liveness machinery that layers on top of the TerminalStream
// reconnect/attach core in terminal_stream.go: the per-attempt readiness /
// heartbeat signalling (terminalStreamSession), the liveness tunables, the
// heartbeat watchdog (runHeartbeat), the wedged-stream escalation
// (escalateWedged), the bounded reader join that keeps a wedged stream from
// hanging the Run loop (awaitReaderAfterCancel), and the cross-service
// not-co-located rejection matcher.
// Split out so terminal_stream.go stays focused on the reconnect loop and the
// per-attach pump bookkeeping; these pieces are self-contained and only touch
// the core through small hooks on *TerminalStreamClient.
package upstream

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/safego"
)

// Terminal liveness handshake / heartbeat defaults (BOS-376). These are
// only active when the client is wired with a TerminalHealth signal
// (production); the legacy unit tests that omit it keep the pre-BOS-376
// behaviour untouched.
//
//   - terminalReadyTimeout: how long openStream waits for the bosso
//     TerminalReady frame after flushing headers before declaring the
//     attempt "not confirmed" and reconnecting. This detects a stream that
//     opened but landed on a bosso pod that is not the DaemonStream owner
//     (the deploy cross-pod split), which HTTP/2 keepalive cannot see.
//   - terminalPingInterval / terminalMissedBeatsBudget: the daemon expects
//     a bosso TerminalPing at roughly this cadence; missing this many in a
//     row tears the stream down.
//   - terminalReadyTimeoutBudget (K): after this many consecutive
//     ready-timeouts the watchdog escalates to a forced paired
//     DaemonStream re-register + fresh-connection re-dial.
const (
	terminalReadyTimeout       = 10 * time.Second
	terminalPingInterval       = 15 * time.Second
	terminalMissedBeatsBudget  = 3
	terminalReadyTimeoutBudget = 3
)

// terminalReaderAbandonGrace is the second stage of openStream's bounded
// reader join (BOS-1375): how long to wait after dispatching the connection
// close before abandoning the reader. The first stage is one ping interval
// (c.pingInterval), the heartbeat budget's own unit, so a stream the watchdog
// has already given up on is held for at most one more silent beat before the
// teardown escalates. The grace only has to cover the HTTP/2 read loop
// noticing its connection is gone, which is prompt; it is kept short because
// it is time the Run loop spends not reconnecting.
const terminalReaderAbandonGrace = 5 * time.Second

// errTerminalNotConfirmed is returned by openStream when the stream opened
// and flushed headers but no TerminalReady frame arrived within
// terminalReadyTimeout. The Run loop counts these to drive the self-heal
// watchdog: a run of them while the daemon is otherwise talking to bosso is
// the alive-but-wrongly-bound signature.
var errTerminalNotConfirmed = errors.New("terminal stream: readiness not confirmed within deadline")

// terminalStreamSession carries the per-attempt liveness signalling between
// the reader goroutine (which observes TerminalReady / TerminalPing frames)
// and openStream's ready-gate + heartbeat watchdog. A fresh one is built on
// every openStream so signals never leak across reconnects.
type terminalStreamSession struct {
	readyOnce sync.Once
	readyCh   chan struct{} // closed once when TerminalReady is first seen
	pingCh    chan struct{} // pulsed (coalesced) on every TerminalPing
}

func newTerminalStreamSession() *terminalStreamSession {
	return &terminalStreamSession{
		readyCh: make(chan struct{}),
		pingCh:  make(chan struct{}, 1),
	}
}

func (s *terminalStreamSession) signalReady() {
	s.readyOnce.Do(func() { close(s.readyCh) })
}

func (s *terminalStreamSession) signalPing() {
	select {
	case s.pingCh <- struct{}{}:
	default:
	}
}

// runHeartbeat watches for bosso's TerminalPing frames while the stream is
// open. Each ping resets the missed-beats counter; MissedBeatsBudget
// consecutive silent intervals mean the stream is no longer being serviced
// (the alive-but-wrongly-bound failure mode), so it marks the health signal
// unhealthy and cancels the stream, which unblocks the reader and drives a
// reconnect. Returns when the stream context is cancelled.
func (c *TerminalStreamClient) runHeartbeat(ctx context.Context, sess *terminalStreamSession, cancel context.CancelFunc) {
	missed := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-sess.pingCh:
			missed = 0
		case <-c.clock.After(c.pingInterval):
			missed++
			if missed >= c.missedBeatsBudget {
				if c.health != nil {
					c.health.MarkUnhealthy()
				}
				c.logger.Warn().Int("missed", missed).Msg("terminal stream: missed heartbeat budget exceeded; tearing down")
				cancel()
				return
			}
		}
	}
}

// awaitReaderAfterCancel bounds openStream's join on its reader goroutine
// once the stream context has been cancelled, and reports whether the reader
// exited (true) or was abandoned (false). BOS-1375: a reader parked on a
// half-open HTTP/2 stream once held openStream — and so the whole Run loop,
// its reconnects and its CycleStream rebinds — for ten hours. Cancellation now
// aborts the transport stream (abortStreamOnCancel), so the first stage is
// expected to be the only one ever reached; the later stages exist so that no
// other library path that blocks the reader can recreate the wedge.
//
// This bounds the wait, not the work. Stage by stage:
//
//  1. Wait one ping interval for the reader to exit on its own.
//  2. If the stream offers terminalConnCloser, close the stream's connection
//     on its own goroutine and do not wait for that call: the close reaches
//     back into the HTTP/2 library whose blocking this bound exists to
//     contain (x/net notes its write mutex "can block indefinitely"). The
//     grace timer starts at dispatch, not when the close returns. The close
//     goroutine is deliberately not joined — if Close itself wedges there is
//     nothing left to escalate to — and closing the connection also drops
//     the co-located DaemonStream for one reconnect, which is accepted on a
//     path the cancel hook should make unreachable.
//  3. Wait terminalReaderAbandonGrace more. If the reader is still running,
//     abandon it: log at error naming the goroutine and attempt, count it,
//     and return. The reader is NOT reclaimed. Nothing — teardown, a later
//     attempt, daemon shutdown — ever joins it: it is abandoned precisely
//     because it does not return, so any join would move the wedge into the
//     daemon restart that is the only manual recovery.
//
// Every wait goes through c.clock so tests drive the stages with fakeClock.
func (c *TerminalStreamClient) awaitReaderAfterCancel(readerDone <-chan struct{}, stream terminalBidiStream, attempt uint64) bool {
	// Fast path: the reader normally exits as soon as the cancel lands, and
	// checking first keeps the common teardown from arming a timer at all.
	select {
	case <-readerDone:
		return true
	default:
	}
	select {
	case <-readerDone:
		return true
	case <-c.clock.After(c.pingInterval):
	}

	if closer, ok := stream.(terminalConnCloser); ok {
		c.health.NoteReaderConnClose()
		c.logger.Warn().
			Str("stream", "TerminalStream").
			Uint64("attempt", attempt).
			Dur("waited", c.pingInterval).
			Msg("terminal stream: reader did not exit after cancel; closing the stream's connection")
		_ = safego.Go(c.logger, func() {
			if err := closer.CloseConn(); err != nil {
				c.logger.Warn().Err(err).
					Str("stream", "TerminalStream").
					Uint64("attempt", attempt).
					Msg("terminal stream: closing the stream's connection failed")
			}
		})
	} else {
		c.logger.Warn().
			Str("stream", "TerminalStream").
			Uint64("attempt", attempt).
			Dur("waited", c.pingInterval).
			Msg("terminal stream: reader did not exit after cancel and the stream has no connection to close")
	}

	select {
	case <-readerDone:
		return true
	case <-c.clock.After(terminalReaderAbandonGrace):
	}

	c.health.NoteReaderAbandoned()
	snap := c.health.Snapshot()
	c.logger.Error().
		Str("stream", "TerminalStream").
		Str("goroutine", "terminal stream reader").
		Uint64("attempt", attempt).
		Dur("waited", c.pingInterval+terminalReaderAbandonGrace).
		Uint64("reader_conn_closes", snap.ReaderConnCloses).
		Uint64("readers_abandoned", snap.ReadersAbandoned).
		Msg("terminal stream: abandoning reader goroutine that did not exit after cancel; it is not reclaimed")
	return false
}

// escalateWedged is the watchdog's forced self-heal: after K consecutive
// ready-timeouts it first drops pooled HTTP/2 connections (closeIdle), then
// rotates the DaemonStream registration (reRegister), so registration cannot
// reuse the idle connection that routed TerminalStream to the wrong bosso pod.
// Both reverse streams then re-dial together and co-locate on one bosso pod.
// "Instigate restart until they know they are connected."
func (c *TerminalStreamClient) escalateWedged(ctx context.Context) {
	if c.health != nil {
		c.health.NoteForcedReRegister()
	}
	// Surface the self-heal counters on the escalation log so the wedge is
	// visible without log spelunking (the plan's Observability goal). This
	// is the production consumer of the TerminalHealth Snapshot counters.
	snap := c.health.Snapshot()
	c.logger.Warn().
		Int("budget", c.readyTimeoutBudget).
		Uint64("ready_confirmed", snap.ReadyConfirmed).
		Uint64("ready_timeouts", snap.ReadyTimeouts).
		Uint64("forced_re_registers", snap.ForcedReRegisters).
		Msg("terminal stream: ready-timeout budget exceeded; forcing paired DaemonStream re-register and fresh-connection re-dial")
	if c.closeIdle != nil {
		c.closeIdle()
	}
	if c.reRegister != nil {
		c.reRegister(ctx)
	}
}

// terminalNotColocatedTag mirrors the token bosso embeds in its
// not-co-located CodeFailedPrecondition rejection
// (services/bosso/internal/server/terminal_stream.go). Kept as a literal in
// both packages because plugin/module boundaries forbid a shared import.
const terminalNotColocatedTag = "terminal-not-colocated"

// isTerminalNotColocated reports whether err is bosso's tagged
// "DaemonStream not local-and-Ready on this pod" rejection.
func isTerminalNotColocated(err error) bool {
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		return false
	}
	return strings.Contains(err.Error(), terminalNotColocatedTag)
}
