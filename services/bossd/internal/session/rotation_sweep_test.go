package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/rotation"
)

// fixedClock returns a clock seam that always reports t.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// newParkedRotationFixture reuses the Task 2 rotation fixture but parks the
// session (Blocked + a persisted resume-at) so the resume-at-T sweep can act on
// it. The fixture's adapters/runner/decider are all still injectable.
func newParkedRotationFixture(t *testing.T, resumeAt time.Time) *rotationFixture {
	t.Helper()
	f := newRotationFixture(t)
	s := f.sessions.sessions[f.sessionID]
	s.State = machine.Blocked
	ra := resumeAt
	s.RotationResumeAt = &ra
	return f
}

// --- Re-dispatch at T ---
//
// The CRITICAL correctness point: the sweep drives a restart on
// OutcomeRotate+NextAccount even though CooldownApplied is false (the capped
// account is already cooling by resume time). The initial-cap CooldownApplied
// dedupe must NOT gate the sweep.
func TestSweepParkedRotations_RedispatchAtResumeTime(t *testing.T) {
	past := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	f := newParkedRotationFixture(t, past)
	f.decider.outcome = rotation.Outcome{
		Kind:            rotation.OutcomeRotate,
		NextAccount:     &models.Account{ID: "acct-next"},
		CooldownApplied: false, // already cooling at resume — MUST still rotate
	}
	f.lc.SetClockForTest(fixedClock(past.Add(time.Minute)))

	n := f.lc.SweepParkedRotations(context.Background())
	if n != 1 {
		t.Fatalf("redispatched = %d, want 1", n)
	}

	s := f.sessions.sessions[f.sessionID]
	if s.State != machine.ImplementingPlan {
		t.Errorf("state = %v, want ImplementingPlan", s.State)
	}
	if s.RotationResumeAt != nil {
		t.Errorf("resume_at should be cleared on redispatch, got %v", s.RotationResumeAt)
	}
	if len(f.runner.started) != 1 {
		t.Fatalf("StartByAgent calls = %d, want 1", len(f.runner.started))
	}
	call := f.runner.started[0]
	wantPrefix := "You were interrupted mid-task by an account switch. Verify current workspace/git/PR state before repeating any action (commits, pushes, comments may already exist)."
	if !strings.HasPrefix(call.plan, wantPrefix) {
		t.Errorf("restarted prompt missing steering prefix:\n got: %q", call.plan)
	}
	if call.plan != wantPrefix+"\n\nORIGINAL PLAN BODY" {
		t.Errorf("restarted prompt = %q, want steering + \\n\\n + plan", call.plan)
	}
	if call.resume == nil || *call.resume != "agent-old" {
		t.Errorf("resume = %v, want agent-old", call.resume)
	}
	if s.AgentSessionID == nil || *s.AgentSessionID != "agent-new" {
		t.Errorf("AgentSessionID = %v, want agent-new", s.AgentSessionID)
	}
	if s.RotationAttemptCount != 1 {
		t.Errorf("RotationAttemptCount = %d, want 1", s.RotationAttemptCount)
	}
}

// --- Not yet due ---

func TestSweepParkedRotations_NotYetDue(t *testing.T) {
	future := time.Date(2026, 7, 5, 18, 0, 0, 0, time.UTC)
	f := newParkedRotationFixture(t, future)
	f.decider.outcome = rotation.Outcome{Kind: rotation.OutcomeRotate, NextAccount: &models.Account{ID: "x"}}
	f.lc.SetClockForTest(fixedClock(future.Add(-time.Hour)))

	n := f.lc.SweepParkedRotations(context.Background())
	if n != 0 {
		t.Fatalf("redispatched = %d, want 0", n)
	}
	if f.decider.calls != 0 {
		t.Errorf("decider must not be consulted for a not-yet-due session, got %d calls", f.decider.calls)
	}
	if f.sessions.sessions[f.sessionID].State != machine.Blocked {
		t.Errorf("state changed for a not-yet-due session")
	}
	if len(f.runner.started) != 0 {
		t.Errorf("no restart expected, got %d", len(f.runner.started))
	}
}

// --- Survives daemon restart (level-triggered off persisted resume-at) ---
//
// A FRESH Lifecycle over the SAME store re-arms the parked session with no
// carried-over in-memory timer, proving the sweep is level-triggered.
func TestSweepParkedRotations_SurvivesDaemonRestart(t *testing.T) {
	past := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	f := newParkedRotationFixture(t, past)

	runner := newMockAgentRunner()
	runner.nextID = "agent-new2"
	fresh := &Lifecycle{
		sessions:    f.sessions, // SAME store holding the parked session
		repos:       f.lc.repos,
		agentRunner: runner,
		logger:      zerolog.Nop(),
	}
	fresh.SetProofEnvResolver(fakeProofEnvResolver{env: map[string]string{}})
	fresh.SetRotationDecider(&fakeRotationDecider{
		outcome: rotation.Outcome{Kind: rotation.OutcomeRotate, NextAccount: &models.Account{ID: "acct-next"}},
	})
	fresh.SetRotationBinding(&fakeRotationBinding{
		binding: RotationBinding{CappedAccountID: "acct-capped", Provider: "claude", RotationCapable: true},
		bound:   true,
	})
	fresh.SetAccountMaterializer(&fakeAccountMaterializer{env: map[string]string{}})
	fresh.SetClockForTest(fixedClock(past.Add(time.Minute)))

	n := fresh.SweepParkedRotations(context.Background())
	if n != 1 {
		t.Fatalf("fresh lifecycle redispatched = %d, want 1", n)
	}
	s := f.sessions.sessions[f.sessionID]
	if s.State != machine.ImplementingPlan {
		t.Errorf("fresh lifecycle did not advance parked session: state=%v", s.State)
	}
	if s.RotationResumeAt != nil {
		t.Errorf("resume_at should be cleared after restart-driven redispatch")
	}
	if len(runner.started) != 1 {
		t.Errorf("fresh runner StartByAgent = %d, want 1", len(runner.started))
	}
}

// --- Still all-cooling: re-park, no spam ---

func TestSweepParkedRotations_StillCoolingReparksNoSpam(t *testing.T) {
	past := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	f := newParkedRotationFixture(t, past)
	newResume := time.Date(2026, 7, 5, 20, 0, 0, 0, time.UTC)
	f.decider.outcome = rotation.Outcome{Kind: rotation.OutcomeAllExhausted, ResumeAt: newResume}
	f.lc.SetClockForTest(fixedClock(past.Add(time.Minute)))

	n := f.lc.SweepParkedRotations(context.Background())
	if n != 0 {
		t.Fatalf("redispatched = %d, want 0 while all cooling", n)
	}
	s := f.sessions.sessions[f.sessionID]
	if s.State != machine.Blocked {
		t.Errorf("state = %v, want Blocked (no Blocked->ImplementingPlan flip on still-cooling)", s.State)
	}
	if len(f.runner.started) != 0 {
		t.Errorf("no restart expected while all cooling, got %d", len(f.runner.started))
	}
	if s.RotationResumeAt == nil || !s.RotationResumeAt.Equal(newResume) {
		t.Errorf("resume_at = %v, want re-stamped to %v", s.RotationResumeAt, newResume)
	}
}

// --- Kill switch disables the sweep ---

func TestSweepParkedRotations_KillSwitchDisables(t *testing.T) {
	past := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	f := newParkedRotationFixture(t, past)
	disabled := false
	f.lc.SetRotationConfig(config.RotationConfig{Enabled: &disabled})
	f.decider.outcome = rotation.Outcome{Kind: rotation.OutcomeRotate, NextAccount: &models.Account{ID: "x"}}
	f.lc.SetClockForTest(fixedClock(past.Add(time.Hour)))

	n := f.lc.SweepParkedRotations(context.Background())
	if n != 0 {
		t.Fatalf("redispatched = %d, want 0 (kill switch)", n)
	}
	if f.decider.calls != 0 {
		t.Errorf("decider must not run with the kill switch engaged")
	}
	if f.sessions.sessions[f.sessionID].State != machine.Blocked {
		t.Errorf("state changed while kill switch engaged")
	}
}
