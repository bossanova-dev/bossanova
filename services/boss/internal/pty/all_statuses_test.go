package pty

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestAllStatuses_ExcludesExitedProcesses locks in that the heartbeat
// snapshot drops processes whose backing PTY child has already exited.
//
// Real-world scenario: the boss TUI launches `tmux attach` as a PTY child
// when the user attaches to a session. When they detach, the attach
// command exits and Process.done closes — but the entry stays in
// m.processes (Cleanup is only called from tests today). Without this
// guard, AllStatuses keeps reporting StatusStopped for that chat on every
// 3 s heartbeat, which races against the daemon's TmuxStatusPoller (which
// correctly reports QUESTION/WORKING/IDLE from the same tmux pane) and
// makes the session display label flash between "? question" and "draft"
// once per tick.
func TestAllStatuses_ExcludesExitedProcesses(t *testing.T) {
	m := NewManager()

	cmd := exec.Command("sh", "-c", "exit 0")
	p, err := m.GetOrStart("attached-then-detached", cmd)
	if err != nil {
		t.Fatalf("GetOrStart: %v", err)
	}
	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("process did not exit in time")
	}

	statuses := m.AllStatuses()
	if _, present := statuses["attached-then-detached"]; present {
		t.Fatalf("AllStatuses returned exited process; expected it to be omitted")
	}
}

// TestAllStatuses_RemovesExitedProcessFromMap verifies the cleanup is
// persistent: the entry is gone from m.processes after AllStatuses runs
// once, so subsequent ticks don't keep paying the iteration cost.
func TestAllStatuses_RemovesExitedProcessFromMap(t *testing.T) {
	m := NewManager()

	cmd := exec.Command("sh", "-c", "exit 0")
	p, err := m.GetOrStart("oneshot", cmd)
	if err != nil {
		t.Fatalf("GetOrStart: %v", err)
	}
	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("process did not exit in time")
	}

	_ = m.AllStatuses()

	m.mu.Lock()
	_, present := m.processes["oneshot"]
	m.mu.Unlock()
	if present {
		t.Fatalf("m.processes still contains exited entry; AllStatuses should have evicted it")
	}
}

// staleProcessWithBuffer builds a live (non-exited) Process whose last PTY
// output is older than activeThreshold and whose ring buffer holds the given
// text. It is registered directly in the manager (no real subprocess) so the
// three status paths can be exercised deterministically without waiting out
// the 5 s activeThreshold.
func staleProcessWithBuffer(t *testing.T, m *Manager, agentSessionID, sessionID, buf string) {
	t.Helper()
	p := &Process{
		buf:  NewRingBuffer(defaultBufSize),
		done: make(chan struct{}),
	}
	if _, err := p.buf.Write([]byte(buf)); err != nil {
		t.Fatalf("buf.Write: %v", err)
	}
	p.lastWrite = time.Now().Add(-10 * time.Second) // older than activeThreshold
	m.mu.Lock()
	m.processes[agentSessionID] = p
	m.mu.Unlock()
	m.RegisterSession(agentSessionID, sessionID)
}

// TestStatus_WorkingIndicatorOverridesIdle is the BOS-152 regression for the
// boss TUI in-process PTY monitor: a live process with stale LastWrite whose
// ring buffer shows a background-shell marker must report `working`, not
// `idle`, across all three status functions.
func TestStatus_WorkingIndicatorOverridesIdle(t *testing.T) {
	m := NewManager()
	staleProcessWithBuffer(t, m, "claude-1", "sess-1",
		"✻ Cooked for 48s · 1 shell still running")

	if got := m.ProcessStatus("claude-1"); got != StatusWorking {
		t.Errorf("ProcessStatus = %q, want %q", got, StatusWorking)
	}
	if got := m.SessionStatus("sess-1"); got != StatusWorking {
		t.Errorf("SessionStatus = %q, want %q", got, StatusWorking)
	}
	if info, ok := m.AllStatuses()["claude-1"]; !ok || info.Status != StatusWorking {
		t.Errorf("AllStatuses status = %q (ok=%v), want %q", info.Status, ok, StatusWorking)
	}
}

// TestStatus_NoMarkerStaleReportsIdle proves the negative path: a stale process
// with no working marker still reports `idle`.
func TestStatus_NoMarkerStaleReportsIdle(t *testing.T) {
	m := NewManager()
	staleProcessWithBuffer(t, m, "claude-2", "sess-2", "⏺ Done. Nothing else running.")

	if got := m.ProcessStatus("claude-2"); got != StatusIdle {
		t.Errorf("ProcessStatus = %q, want %q", got, StatusIdle)
	}
	if got := m.SessionStatus("sess-2"); got != StatusIdle {
		t.Errorf("SessionStatus = %q, want %q", got, StatusIdle)
	}
}

// TestStatus_StaleWorkingMarkerReportsIdle is the review regression for
// BOS-152: the ring buffer retains a background-shell marker from an earlier
// re-render frame, but the current screen has returned to an idle prompt. The
// stale marker must NOT keep the process pinned WORKING once its output is older
// than activeThreshold; all three status paths must report idle.
func TestStatus_StaleWorkingMarkerReportsIdle(t *testing.T) {
	m := NewManager()
	var b strings.Builder
	b.WriteString("✻ Cooked for 48s · 1 shell still running\n")
	// The shell finished and Claude re-rendered the idle screen; this fresh
	// content pushes the stale footer out of the current-screen window.
	for range 35 {
		b.WriteString("⏺ finished a step\n")
	}
	b.WriteString("╭──────────────╮\n│ ❯            │\n╰──────────────╯\n")
	staleProcessWithBuffer(t, m, "claude-stale", "sess-stale", b.String())

	if got := m.ProcessStatus("claude-stale"); got != StatusIdle {
		t.Errorf("ProcessStatus = %q, want %q", got, StatusIdle)
	}
	if got := m.SessionStatus("sess-stale"); got != StatusIdle {
		t.Errorf("SessionStatus = %q, want %q", got, StatusIdle)
	}
	if info, ok := m.AllStatuses()["claude-stale"]; !ok || info.Status != StatusIdle {
		t.Errorf("AllStatuses status = %q (ok=%v), want %q", info.Status, ok, StatusIdle)
	}
}

// TestStatus_QuestionWinsOverWorkingIndicator proves QUESTION still precedes
// WORKING: a buffer carrying BOTH an AskUserQuestion footer and a working
// marker reports `question`.
func TestStatus_QuestionWinsOverWorkingIndicator(t *testing.T) {
	m := NewManager()
	buf := "  1. Keep it\n  2. Type something.\n  ────────\n  3. Chat about this\n\n  1 shell still running\n"
	staleProcessWithBuffer(t, m, "claude-3", "sess-3", buf)

	if got := m.ProcessStatus("claude-3"); got != StatusQuestion {
		t.Errorf("ProcessStatus = %q, want %q", got, StatusQuestion)
	}
	if got := m.SessionStatus("sess-3"); got != StatusQuestion {
		t.Errorf("SessionStatus = %q, want %q", got, StatusQuestion)
	}
}
