package pty

import (
	"os/exec"
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
