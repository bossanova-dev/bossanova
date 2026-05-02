package cron

import (
	"testing"
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
)

// TestTick_RunsElapsedJob verifies that Tick fires a job whose next scheduled
// time is ≤ the supplied now, and does NOT fire a job whose next scheduled
// time is still in the future relative to now.
func TestTick_RunsElapsedJob(t *testing.T) {
	store := newFakeStore()
	store.put(makeJob("due", "@every 1m", true))
	store.put(makeJob("future", "@every 1h", true))
	creator := newFakeCreator()
	s := newTestScheduler(t, store, newFakeSessionStore(), creator)

	if err := s.AddJob(store.jobs["due"]); err != nil {
		t.Fatalf("AddJob due: %v", err)
	}
	if err := s.AddJob(store.jobs["future"]); err != nil {
		t.Fatalf("AddJob future: %v", err)
	}

	// lastRun starts at zero (time.Time{}).
	// "@every 1m" → next = zero+1m; "@every 1h" → next = zero+1h.
	// Tick at zero+2m: "due" fires, "future" does not.
	now := time.Time{}.Add(2 * time.Minute)
	s.Tick(now)

	if got := len(creator.calls); got != 1 {
		t.Fatalf("creator calls after Tick = %d, want 1 (only 'due')", got)
	}
	if creator.calls[0].CronJobID != "due" {
		t.Errorf("fired job = %q, want 'due'", creator.calls[0].CronJobID)
	}
}

// TestTick_DoesNotRunFutureJob verifies that Tick does not fire a job whose
// next scheduled time has not yet elapsed.
func TestTick_DoesNotRunFutureJob(t *testing.T) {
	store := newFakeStore()
	store.put(makeJob("j", "@every 1h", true))
	creator := newFakeCreator()
	s := newTestScheduler(t, store, newFakeSessionStore(), creator)

	if err := s.AddJob(store.jobs["j"]); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Tick only 1 second forward — the 1-hour schedule has not elapsed.
	s.Tick(time.Time{}.Add(time.Second))

	if got := len(creator.calls); got != 0 {
		t.Errorf("creator calls = %d, want 0 (job not due yet)", got)
	}
}

// TestTick_RespectsOverlapSkip verifies that if the previous session is still
// active, Tick's synchronous fire skips (via the existing overlap logic).
func TestTick_RespectsOverlapSkip(t *testing.T) {
	store := newFakeStore()
	job := makeJob("j", "@every 1m", true)
	prev := "sess-running"
	job.LastRunSessionID = &prev
	store.put(job)

	sessions := newFakeSessionStore()
	sessions.put(&models.Session{ID: prev, State: machine.ImplementingPlan}) // non-terminal

	creator := newFakeCreator()
	s := newTestScheduler(t, store, sessions, creator)

	if err := s.AddJob(store.jobs["j"]); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	s.Tick(time.Time{}.Add(2 * time.Minute))

	if got := len(creator.calls); got != 0 {
		t.Errorf("creator calls = %d, want 0 (overlap skip)", got)
	}
}

// TestTick_AdvancesLastRun verifies that after Tick fires a job, a second Tick
// at the same timestamp does NOT re-fire (lastRun was updated to now).
func TestTick_AdvancesLastRun(t *testing.T) {
	store := newFakeStore()
	store.put(makeJob("j", "@every 1m", true))
	creator := newFakeCreator()
	s := newTestScheduler(t, store, newFakeSessionStore(), creator)

	if err := s.AddJob(store.jobs["j"]); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	now := time.Time{}.Add(2 * time.Minute)
	s.Tick(now)
	s.Tick(now) // second call at the same time must not re-fire

	if got := len(creator.calls); got != 1 {
		t.Errorf("creator calls after two identical Ticks = %d, want 1", got)
	}
}
