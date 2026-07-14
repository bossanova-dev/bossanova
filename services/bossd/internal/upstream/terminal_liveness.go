// Package upstream — terminal_liveness.go holds the BOS-376 positive
// terminal-liveness machinery that layers on top of the TerminalStream
// reconnect/attach core in terminal_stream.go: the per-attempt readiness /
// heartbeat signalling (terminalStreamSession), the liveness tunables, the
// heartbeat watchdog (runHeartbeat), the wedged-stream escalation
// (escalateWedged), and the cross-service not-co-located rejection matcher.
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

// escalateWedged is the watchdog's forced self-heal: after K consecutive
// ready-timeouts it rotates the DaemonStream registration (reRegister) and
// drops pooled HTTP/2 connections (closeIdle) so both reverse streams
// re-dial together on a fresh connection and co-locate on one bosso pod.
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
	if c.reRegister != nil {
		c.reRegister(ctx)
	}
	if c.closeIdle != nil {
		c.closeIdle()
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
