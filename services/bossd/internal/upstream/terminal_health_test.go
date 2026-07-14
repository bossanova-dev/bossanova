package upstream

import "testing"

// TestTerminalHealth_StartsUnhealthy asserts a fresh signal is unhealthy:
// the daemon must not treat a just-opened TerminalStream as connected
// until a TerminalReady frame confirms it.
func TestTerminalHealth_StartsUnhealthy(t *testing.T) {
	t.Parallel()
	h := NewTerminalHealth()
	if h.Healthy() {
		t.Fatal("new TerminalHealth should start unhealthy (no ready confirmed yet)")
	}
}

// TestTerminalHealth_MarkHealthyTransitions asserts MarkHealthy flips the
// state and reports the transition exactly once (so callers log once, not
// on every ready frame), and bumps the ready-confirmed counter each call.
func TestTerminalHealth_MarkHealthyTransitions(t *testing.T) {
	t.Parallel()
	h := NewTerminalHealth()
	if !h.MarkHealthy() {
		t.Fatal("first MarkHealthy should report a transition")
	}
	if !h.Healthy() {
		t.Fatal("after MarkHealthy, Healthy() should be true")
	}
	if h.MarkHealthy() {
		t.Fatal("second MarkHealthy (already healthy) should not report a transition")
	}
	if got := h.Snapshot().ReadyConfirmed; got != 2 {
		t.Fatalf("ReadyConfirmed = %d, want 2 (counter bumps on every ready frame)", got)
	}
}

// TestTerminalHealth_MarkUnhealthyTransitions asserts MarkUnhealthy flips
// back and reports the transition once.
func TestTerminalHealth_MarkUnhealthyTransitions(t *testing.T) {
	t.Parallel()
	h := NewTerminalHealth()
	h.MarkHealthy()
	if !h.MarkUnhealthy() {
		t.Fatal("MarkUnhealthy from healthy should report a transition")
	}
	if h.Healthy() {
		t.Fatal("after MarkUnhealthy, Healthy() should be false")
	}
	if h.MarkUnhealthy() {
		t.Fatal("second MarkUnhealthy (already unhealthy) should not report a transition")
	}
}

// TestTerminalHealth_Counters asserts the observability counters advance
// independently so the wedge is visible without log spelunking.
func TestTerminalHealth_Counters(t *testing.T) {
	t.Parallel()
	h := NewTerminalHealth()
	h.NoteReadyTimeout()
	h.NoteReadyTimeout()
	h.NoteForcedReRegister()
	snap := h.Snapshot()
	if snap.ReadyTimeouts != 2 {
		t.Errorf("ReadyTimeouts = %d, want 2", snap.ReadyTimeouts)
	}
	if snap.ForcedReRegisters != 1 {
		t.Errorf("ForcedReRegisters = %d, want 1", snap.ForcedReRegisters)
	}
}

// TestTerminalHealth_NilSafe mirrors AuthState's nil-safety contract so
// production wiring never has to nil-check before signalling.
func TestTerminalHealth_NilSafe(t *testing.T) {
	t.Parallel()
	var h *TerminalHealth
	if h.Healthy() {
		t.Fatal("nil TerminalHealth should report unhealthy")
	}
	if h.MarkHealthy() {
		t.Fatal("nil MarkHealthy should report no transition")
	}
	if h.MarkUnhealthy() {
		t.Fatal("nil MarkUnhealthy should report no transition")
	}
	h.NoteReadyTimeout()     // must not panic
	h.NoteForcedReRegister() // must not panic
	_ = h.Snapshot()         // must not panic
}
