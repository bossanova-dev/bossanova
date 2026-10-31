package status

import (
	"fmt"
	"time"
)

// BackgroundWorkHoldMax bounds a static background-only pane after its last change.
const BackgroundWorkHoldMax = 15 * time.Minute

// SetBackgroundHold records the background-only deadline; zero clears it.
func (t *Tracker) SetBackgroundHold(id string, until time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if until.IsZero() {
		delete(t.backgroundHolds, id)
	} else {
		t.backgroundHolds[id] = until
	}
}

// BackgroundHold returns the active hold's deadline, or zero when absent or expired.
func (t *Tracker) BackgroundHold(id string) time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	until := t.backgroundHolds[id]
	if !until.After(time.Now()) {
		return time.Time{}
	}
	return until
}

func (t *Tracker) backgroundAgentsUntil(id string) time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	report, ok := t.backgroundAgents[id]
	if !ok || report.count <= 0 {
		return time.Time{}
	}
	return report.at.Add(BackgroundAgentsMaxAge)
}

// DisplayPhase prefers an active background hold to the main turn's skill stage.
func (t *Tracker) DisplayPhase(id string) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if remaining := time.Until(t.backgroundHolds[id]); remaining > 0 {
		return backgroundHoldLabel(remaining)
	}
	return t.phases[id]
}

func backgroundHoldLabel(remaining time.Duration) string {
	unit, suffix := time.Minute, "m"
	if remaining >= time.Hour {
		unit, suffix = time.Hour, "h"
	}
	return fmt.Sprintf("background ≤%d%s", (remaining+unit-1)/unit, suffix)
}

func backgroundHoldVerdict(paneBusy, spinnerPresent bool, agentsUntil time.Time, paneStaticSince, now time.Time) (bool, time.Time, bool) {
	if paneBusy && spinnerPresent {
		return true, time.Time{}, false
	}
	until := agentsUntil
	if paneBusy {
		paneUntil := paneStaticSince.Add(BackgroundWorkHoldMax)
		if paneUntil.After(until) {
			until = paneUntil
		}
	}
	if until.After(now) {
		return true, until, true
	}
	return false, time.Time{}, false
}

func (t *Tracker) cleanupBackgroundWorkLocked() {
	for id := range t.backgroundHolds {
		if _, exists := t.entries[id]; !exists {
			delete(t.backgroundHolds, id)
		}
	}
	for id := range t.backgroundAgents {
		if _, exists := t.entries[id]; !exists {
			delete(t.backgroundAgents, id)
		}
	}
}

// BackgroundAgentsMaxAge bounds how long a turn-ended report of running
// background subagents keeps a chat WORKING without a fresh report. Background
// agents legitimately run for hours, so it is generous; it exists only so a
// subagent whose end never reached bossd stops claiming live work eventually.
const BackgroundAgentsMaxAge = 4 * time.Hour

// backgroundAgentsReport is one turn-ended report for a chat.
type backgroundAgentsReport struct {
	count int
	at    time.Time
}

// SetBackgroundAgents records how many background subagents the chat's last
// finished turn left running (zero clears the record). Written by the hook
// server from Claude's Stop and UserPromptSubmit hooks.
func (t *Tracker) SetBackgroundAgents(agentSessionID string, count int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if count <= 0 {
		delete(t.backgroundAgents, agentSessionID)
		return
	}
	t.backgroundAgents[agentSessionID] = backgroundAgentsReport{count: count, at: time.Now()}
}

// BackgroundAgentsRunning reports whether the chat's last finished turn left a
// background subagent running, per a report younger than
// BackgroundAgentsMaxAge.
func (t *Tracker) BackgroundAgentsRunning(agentSessionID string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	r, ok := t.backgroundAgents[agentSessionID]
	return ok && r.count > 0 && time.Since(r.at) < BackgroundAgentsMaxAge
}
