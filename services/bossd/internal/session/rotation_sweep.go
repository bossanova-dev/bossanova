package session

import (
	"context"
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/rotation"
)

// now returns the sweep's current time. It defaults to time.Now in production
// and to an injected fake (SetClockForTest) under test so the resume-at-T
// boundary is deterministic. classifyUsageLimit deliberately does NOT route
// through here — it stays on the wall clock.
func (l *Lifecycle) now() time.Time {
	if l.clock != nil {
		return l.clock()
	}
	return time.Now()
}

// SetClockForTest overrides the sweep's time source. Test-only; production
// leaves it nil so now() uses the wall clock.
func (l *Lifecycle) SetClockForTest(fn func() time.Time) { l.clock = fn }

// SweepParkedRotations is the level-triggered resume-at-T sweep. It re-dispatches
// headless runs that were parked (Blocked with a persisted rotation_resume_at)
// while their provider's accounts were all cooling, once an account is available
// again. Because it is driven entirely off the persisted rotation_resume_at
// stamp — never an in-memory timer — a daemon restart re-arms every parked run:
// a fresh Lifecycle over the same store resumes them on its next tick (mirrors
// recoverStrandedCronSessions).
//
// The kill switch (ManagedAccountsEnabled) also disables the sweep. Returns the number
// of sessions re-dispatched this pass.
func (l *Lifecycle) SweepParkedRotations(ctx context.Context) (redispatched int) {
	rotationConfig, ok := l.currentRotationConfig()
	if !ok || !rotationConfig.ManagedAccountsEnabled() {
		return 0
	}
	if l.sessions == nil {
		return 0
	}

	parked, err := l.sessions.ListByState(ctx, int(machine.Blocked))
	if err != nil {
		l.logger.Warn().Err(err).Msg("parked-rotation sweep: list blocked sessions failed")
		return 0
	}

	now := l.now()
	for _, session := range parked {
		// Only sessions with a due resume-at stamp are candidates; everything
		// else (ordinary Blocked, or a not-yet-due park) is left untouched.
		if session.RotationResumeAt == nil || now.Before(*session.RotationResumeAt) {
			continue
		}
		if l.redispatchParkedRotation(ctx, session) {
			redispatched++
		}
	}
	return redispatched
}

// redispatchParkedRotation evaluates one due parked session and, only if an
// account is now available, transitions it Blocked→ImplementingPlan and restarts
// under that account. The outcome is computed while the session is still Blocked
// and the CAS transition happens ONLY when it will actually rotate — a still-all-
// cooling session is merely re-stamped and stays Blocked, so a parked run never
// flaps Blocked→ImplementingPlan→Blocked and never emits a second notification.
// Any missing seam, ineligible repo, unbound session, decider error, or non-
// rotating outcome leaves the session parked for the next sweep. Returns true
// only when the session was actually re-dispatched.
func (l *Lifecycle) redispatchParkedRotation(ctx context.Context, session *models.Session) bool {
	// Every seam must be wired; nil test/legacy wiring leaves parked runs in
	// place (fail-safe — a human unblocks).
	if l.rotationBinding == nil || l.rotationDecider == nil || l.accountMaterializer == nil {
		return false
	}
	if l.repos == nil {
		return false
	}
	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil || repo == nil || !repo.CanAutoRotate {
		return false
	}

	b, bound, err := l.rotationBinding.CurrentBinding(ctx, session)
	if err != nil || !bound {
		return false
	}

	// No ResetAt at resume time: the banner that carried it is long gone; the
	// engine decides purely on current account cooldown state.
	outcome, err := l.rotationDecider(ctx, rotation.Signal{
		Provider:        b.Provider,
		CappedAccountID: b.CappedAccountID,
		Kind:            rotation.UsageLimited,
		ResetAt:         nil,
		RotationCapable: b.RotationCapable,
	})
	if err != nil {
		l.logger.Warn().Err(err).Str("session", session.ID).
			Msg("parked-rotation sweep: decide failed; leaving parked")
		return false
	}

	switch outcome.Kind {
	case rotation.OutcomeRotate:
		if outcome.NextAccount == nil {
			// Rotate-shaped but nothing un-cooled yet — leave parked.
			return false
		}
		// The CAS Blocked→ImplementingPlan is the idempotency gate: a concurrent
		// sweep or a late completion signal that already advanced this session
		// loses the CAS and this pass no-ops.
		advanced, err := l.sessions.UpdateStateConditional(ctx, session.ID, int(machine.ImplementingPlan), int(machine.Blocked))
		if err != nil {
			l.logger.Error().Err(err).Str("session", session.ID).
				Msg("parked-rotation sweep: transition to implementing failed")
			return false
		}
		if !advanced {
			return false
		}
		if err := l.rotateAndRestart(ctx, session, outcome.NextAccount); err != nil {
			// Restart failed after the transition. Re-park (revert to Blocked) so
			// the next sweep retries; rotateAndRestart clears rotation_resume_at
			// only on its successful persist, so the stamp is still armed here.
			if _, rerr := l.sessions.UpdateStateConditional(ctx, session.ID, int(machine.Blocked), int(machine.ImplementingPlan)); rerr != nil {
				l.logger.Error().Err(rerr).Str("session", session.ID).
					Msg("parked-rotation sweep: re-park after failed restart failed")
			}
			return false
		}
		l.logger.Info().Str("session", session.ID).Str("next_account_id", outcome.NextAccount.ID).
			Msg("parked-rotation sweep: resume-at reached; redispatched under fresh account")
		return true

	case rotation.OutcomeAllExhausted:
		// Still all cooling: re-stamp the new resume-at and stay Blocked. No
		// state transition and no BlockedReason rewrite ⇒ no second notification.
		l.restampParkedResumeAt(ctx, session, outcome.ResumeAt)
		return false

	default:
		// OutcomeStatusOnly (not rotation-capable) and OutcomeNoEligibleAccount
		// (capable but no eligible target) — nothing to redispatch onto, so leave
		// parked. The initial usage-limit intercept (rotation.go) owns recording
		// these outcomes; the sweep only retries when a target becomes available.
		return false
	}
}

// restampParkedResumeAt updates a still-cooling parked session's persisted
// resume-at to the engine's freshly computed time without touching its state, so
// the next sweep re-checks at the new boundary. Never notifies (the session
// stays Blocked).
func (l *Lifecycle) restampParkedResumeAt(ctx context.Context, session *models.Session, resumeAt time.Time) {
	iso := resumeAt.Format(time.RFC3339)
	resumePtr := &iso
	if _, err := l.sessions.Update(ctx, session.ID, db.UpdateSessionParams{RotationResumeAt: &resumePtr}); err != nil {
		l.logger.Error().Err(err).Str("session", session.ID).
			Msg("parked-rotation sweep: re-stamp resume-at failed")
	}
}
