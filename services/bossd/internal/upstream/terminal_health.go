// Package upstream — terminal_health.go houses the shared "is the web
// terminal path actually reachable end-to-end" signal between the
// TerminalStream opener/reader (which discover readiness via the
// application-level TerminalReady frame and liveness via heartbeats) and
// the Run loop's watchdog (which forces a paired DaemonStream re-register
// when the terminal path stays wedged).
//
// It mirrors AuthState in auth_state.go: a tiny mutex-guarded struct with
// a boolean state plus transition-reporting mutators so callers log once
// on a change rather than on every frame. Unlike AuthState there is no
// wait/block surface. The Run-loop watchdog escalates off its own local
// ready-timeout streak (not off Healthy()); this signal's role is
// observability of the self-heal loop — MarkHealthy/MarkUnhealthy drive the
// one-shot ready-confirmed / teardown log lines, and the counters
// (ready-confirmed, ready-timeout, forced-re-register) are surfaced on the
// escalation warn log via Snapshot(). Healthy() is exercised by the unit
// tests to assert the readiness gate flips state correctly.
package upstream

import "sync"

// TerminalHealth is the positive-liveness signal for the daemon's
// TerminalStream. It starts unhealthy: the daemon must NOT treat a
// freshly-opened stream as connected until a TerminalReady frame confirms
// the stream landed on a co-located, Ready bosso pod. MarkHealthy flips it
// on TerminalReady; MarkUnhealthy flips it on teardown / missed heartbeats
// / ready-timeout. All methods are nil-safe so wiring never has to
// nil-check before signalling (matches AuthState).
type TerminalHealth struct {
	mu      sync.Mutex
	healthy bool

	// Observability counters. readyConfirmed bumps on every TerminalReady
	// (not just transitions) so a flapping stream is visible; readyTimeouts
	// bumps each time openStream gives up waiting for TerminalReady;
	// forcedReRegisters bumps each time the watchdog escalates to a paired
	// re-register.
	readyConfirmed    uint64
	readyTimeouts     uint64
	forcedReRegisters uint64
}

// TerminalHealthSnapshot is a value copy of the counters for logging and
// tests. Taken under the lock so readers never observe a torn count.
type TerminalHealthSnapshot struct {
	Healthy           bool
	ReadyConfirmed    uint64
	ReadyTimeouts     uint64
	ForcedReRegisters uint64
}

// NewTerminalHealth returns a signal in the unhealthy (not-yet-confirmed)
// state.
func NewTerminalHealth() *TerminalHealth {
	return &TerminalHealth{}
}

// MarkHealthy records a TerminalReady confirmation. Returns true iff this
// was a real unhealthy → healthy transition (so the caller logs once).
// Bumps ReadyConfirmed on every call regardless of prior state.
func (h *TerminalHealth) MarkHealthy() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.readyConfirmed++
	if h.healthy {
		return false
	}
	h.healthy = true
	return true
}

// MarkUnhealthy records a teardown / missed-heartbeat / ready-timeout.
// Returns true iff this was a real healthy → unhealthy transition.
func (h *TerminalHealth) MarkUnhealthy() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.healthy {
		return false
	}
	h.healthy = false
	return true
}

// Healthy reports whether the last confirmed signal was TerminalReady with
// no subsequent teardown. A nil signal reports unhealthy.
func (h *TerminalHealth) Healthy() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.healthy
}

// NoteReadyTimeout bumps the ready-timeout counter (openStream gave up
// waiting for TerminalReady).
func (h *TerminalHealth) NoteReadyTimeout() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.readyTimeouts++
	h.mu.Unlock()
}

// NoteForcedReRegister bumps the forced-re-register counter (the watchdog
// escalated to a paired DaemonStream re-register).
func (h *TerminalHealth) NoteForcedReRegister() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.forcedReRegisters++
	h.mu.Unlock()
}

// Snapshot returns a consistent copy of the state and counters.
func (h *TerminalHealth) Snapshot() TerminalHealthSnapshot {
	if h == nil {
		return TerminalHealthSnapshot{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return TerminalHealthSnapshot{
		Healthy:           h.healthy,
		ReadyConfirmed:    h.readyConfirmed,
		ReadyTimeouts:     h.readyTimeouts,
		ForcedReRegisters: h.forcedReRegisters,
	}
}
