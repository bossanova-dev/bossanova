package session

import (
	"context"
	"fmt"
	"time"

	"github.com/recurser/bossalib/agenterr"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dotenv"
	"github.com/recurser/bossd/internal/rotation"
)

// steeringNotice is the verbatim single-line prefix prepended to the resumed
// plan when a headless run is rotated onto a fresh account mid-task. It is a
// NAMED acceptance criterion (BOS-174) — the exact wording tells the resumed
// agent that its previous turn was interrupted by an account switch, so it must
// re-check the workspace before repeating side effects. Do NOT reword.
const steeringNotice = "You were interrupted mid-task by an account switch. Verify current workspace/git/PR state before repeating any action (commits, pushes, comments may already exist)."

// rotationDecider is the narrow decision function consumed by usage-limit
// rotation. Nil ⇒ rotation degrades to today's Block path (fail-safe).
type rotationDecider func(ctx context.Context, sig rotation.Signal) (rotation.Outcome, error)

// rateLimitProbe authoritatively probes a bound account before the lifecycle
// cools it. Nil/error/unfetched/healthy snapshots fall through to today's Block
// path without changing account cooldowns.
type rateLimitProbe func(ctx context.Context, accountID string) (models.UsageSnapshot, error)

// accountMaterializer resolves the env overlay for the account to switch to.
// The real implementation forwards the account's keyring credential blob to the
// agent plugin's MaterializeAccount RPC (BOS-169); tests inject a synthetic
// map. Never logs the returned values.
type accountMaterializer interface {
	Materialize(ctx context.Context, account *models.Account) (map[string]string, error)
}

// rotationBinding resolves the session→account binding needed to build a
// rotation signal: which account is currently capped, the provider, and whether
// the agent can rotate at all. Production wires the BOS-170 binding adapter;
// nil or unbound still degrades to today's Block path (fail-safe).
type rotationBinding interface {
	CurrentBinding(ctx context.Context, session *models.Session) (RotationBinding, bool, error)
}

// RotationBinding is the session→account binding snapshot consumed when building
// a rotation.Signal.
type RotationBinding struct {
	CappedAccountID string
	Provider        string
	RotationCapable bool
}

// SetRotationDecider injects the rotation decision function. Safe to leave
// unset: a nil decider makes attemptUsageLimitRotation fall through to today's
// Block.
func (l *Lifecycle) SetRotationDecider(d rotationDecider) { l.rotationDecider = d }

// SetAccountMaterializer injects the account env materializer. Safe to leave
// unset (nil ⇒ Block fallback).
func (l *Lifecycle) SetAccountMaterializer(m accountMaterializer) { l.accountMaterializer = m }

// SetRotationBinding injects the session→account binding resolver. Safe to
// leave unset (nil ⇒ Block fallback).
func (l *Lifecycle) SetRotationBinding(b rotationBinding) { l.rotationBinding = b }

// SetRateLimitProbe injects the authoritative confirm-before-cool probe.
func (l *Lifecycle) SetRateLimitProbe(p rateLimitProbe) { l.rateLimitProbe = p }

// SetAccountGetter injects the account-by-id resolver used by CurrentBearer to
// materialize the bound account's bearer for first-leg proxy translation
// (BOS-326). Safe to leave unset (nil ⇒ CurrentBearer returns "", so the proxy
// forwards the client's own header).
func (l *Lifecycle) SetAccountGetter(g func(ctx context.Context, id string) (*models.Account, error)) {
	l.accountGetter = g
}

// HasLiveRotationSeams reports whether the lifecycle has both live seams needed
// for headless/session-lifecycle auto-rotation.
func (l *Lifecycle) HasLiveRotationSeams() bool {
	return l.rotationBinding != nil && l.accountMaterializer != nil && l.rateLimitProbe != nil
}

// SetRotationConfig installs the rotation policy knobs (kill switch, max
// rotations). The zero value enables rotation (ManagedAccountsEnabled()==true) with the
// default max, so leaving it unset is a safe production default.
func (l *Lifecycle) SetRotationConfig(c config.ManagedAccountsConfig) { l.rotationConfig = c }

// SetRotationRecorder injects the rotation audit recorder (BOS-176). Safe to
// leave unset: the Recorder's methods are nil-receiver no-ops, so an unwired
// daemon records no audit events.
func (l *Lifecycle) SetRotationRecorder(r *rotation.Recorder) { l.rotationRecorder = r }

// SetRotationConfigLoader installs the live rotation-policy re-loader used by
// rotation decisions and parked sweeps (BOS-176). Production wires a
// config.Load-backed adapter; leaving it unset falls back to the cached config
// from SetRotationConfig.
func (l *Lifecycle) SetRotationConfigLoader(load func() (config.ManagedAccountsConfig, error)) {
	l.rotationConfigLoader = load
}

// currentRotationConfig returns the live rotation policy when a loader is wired,
// otherwise the cached config injected at startup. It fails safe: a load error
// disables automatic rotation for that decision.
func (l *Lifecycle) currentRotationConfig() (config.ManagedAccountsConfig, bool) {
	if l.rotationConfigLoader == nil {
		return l.rotationConfig, true
	}
	cfg, err := l.rotationConfigLoader()
	if err != nil {
		l.logger.Warn().Err(err).
			Msg("rotation gate: settings load failed; treating rotation as disabled")
		return config.ManagedAccountsConfig{}, false
	}
	return cfg, true
}

// autoRotateAllowed reports whether automatic rotation may act, re-reading
// settings.json live per decision when a loader is wired (kill-switch flips take
// effect without a daemon restart). It fails safe: a load error disables
// automatic rotation. With no loader it uses the cached injected config.
func (l *Lifecycle) autoRotateAllowed() bool {
	cfg, ok := l.currentRotationConfig()
	return ok && cfg.ManagedAccountsEnabled()
}

// recordRotation is the headless-intercept audit helper. It builds an
// AuditEvent from the current session + signal and records the given outcome.
// Auditing never fails a rotation (the Recorder swallows insert errors).
func (l *Lifecycle) recordRotation(ctx context.Context, session *models.Session, b RotationBinding, outcome, toAccount, detail string, resetAt *time.Time) {
	l.rotationRecorder.Record(ctx, rotation.AuditEvent{
		SessionID:   session.ID,
		Provider:    b.Provider,
		Trigger:     "ROTATION_TRIGGER_USAGE_LIMITED",
		FromAccount: b.CappedAccountID,
		ToAccount:   toAccount,
		ResetAt:     resetAt,
		Outcome:     outcome,
		Detail:      detail,
	})
}

// classifyUsageLimit reports whether a headless run's exit-error string is a
// usage/quota cap, and the parsed reset time when the banner carried one. It is
// deliberately fail-safe: an empty string or any non-usage classification
// yields ok=false so the caller runs today's Block path unchanged. Classifying
// from the flattened exit string (not structured proto fields) keeps the
// SignalSessionRunComplete signature untouched.
func classifyUsageLimit(exitError string) (resetAt time.Time, ok bool) {
	if exitError == "" {
		return time.Time{}, false
	}
	c := agenterr.Classify(exitError, time.Now())
	if c.Kind != agenterr.KindUsageExhausted {
		return time.Time{}, false
	}
	if c.ResetAt != nil {
		return *c.ResetAt, true
	}
	return time.Time{}, true
}

// attemptUsageLimitRotation intercepts a usage-limited headless exit at the top
// of SignalSessionRunComplete. On a genuine cap for a live, rotatable plan run
// it either rotates-and-restarts under the next account (with the steering
// notice prefixed to the resumed plan) or parks the session, returning
// handled=true so the caller skips the normal finalize/block fan-out. Every
// other case — non-cap exit, missing/ineligible session, disabled rotation,
// unbound account, missing adapter, decider error, or a status-only outcome —
// returns false so today's Block path runs unchanged.
func (l *Lifecycle) attemptUsageLimitRotation(ctx context.Context, sessionID, _ /*agentSessionID*/, exitError string) (handled bool) {
	_, ok := classifyUsageLimit(exitError)
	if !ok {
		return false
	}
	if sessionID == "" || l.sessions == nil {
		return false
	}
	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil || session == nil {
		return false
	}
	// Only a live plan run rotates (covers both cron and non-cron headless).
	if session.State != machine.ImplementingPlan {
		return false
	}

	// Gates: kill switch and per-repo opt-out. The kill switch is re-read live
	// per decision (BOS-176) so a flip takes effect without a daemon restart;
	// a disabled flip records a STATUS_ONLY_DISABLED audit event and falls back
	// to today's Block path (no swap).
	rotationConfig, ok := l.currentRotationConfig()
	if !ok || !rotationConfig.ManagedAccountsEnabled() {
		l.rotationRecorder.Record(ctx, rotation.AuditEvent{
			SessionID: sessionID, Provider: session.AgentName,
			Trigger: "ROTATION_TRIGGER_USAGE_LIMITED",
			Outcome: "ROTATION_OUTCOME_STATUS_ONLY_DISABLED",
			Detail:  "automatic rotation disabled (rotation.enabled=false)",
		})
		return false
	}
	if l.repos == nil {
		return false
	}
	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil || repo == nil || !repo.CanAutoRotate {
		return false
	}

	// Require all adapters; any missing seam degrades to today's Block.
	if l.rotationBinding == nil || l.rotationDecider == nil || l.accountMaterializer == nil || l.rateLimitProbe == nil {
		return false
	}
	b, bound, err := l.rotationBinding.CurrentBinding(ctx, session)
	if err != nil || !bound {
		// Unbound / account-0 (machine login) ⇒ nothing to rotate.
		return false
	}

	snap, err := l.rateLimitProbe(ctx, b.CappedAccountID)
	if err != nil {
		l.logger.Warn().Err(err).Str("session", sessionID).Str("account_id", b.CappedAccountID).
			Msg("usage-limit rotation: usage probe failed; falling back to block")
		return false
	}
	if !rotation.UsageSnapshotConfirmsLimited(snap) {
		l.logger.Info().Str("session", sessionID).Str("account_id", b.CappedAccountID).
			Str("status", snap.Status).Float64("util_5h", snap.Util5h).Float64("util_7d", snap.Util7d).
			Msg("usage-limit rotation: usage probe says account is not limited; falling back to block")
		return false
	}

	// Bounded-exhaustion parks only after the authoritative probe confirms the
	// current account is genuinely limited. A loose false-positive exit string
	// must still fall through to today's Block/finalize path without rotation
	// bookkeeping.
	maxRotations := rotationConfig.MaxRotations()
	if session.RotationAttemptCount >= maxRotations {
		l.parkRotatedSession(ctx, sessionID,
			fmt.Sprintf("usage-limited: max rotations (%d) reached", maxRotations), nil)
		return true
	}

	var resetPtr *time.Time
	if probedReset := rotation.UsageSnapshotResetAt(snap); probedReset != nil {
		resetPtr = probedReset
	}
	sig := rotation.Signal{
		Provider:        b.Provider,
		CappedAccountID: b.CappedAccountID,
		Kind:            rotation.UsageLimited,
		ResetAt:         resetPtr,
		RotationCapable: b.RotationCapable,
	}
	outcome, err := l.rotationDecider(ctx, sig)
	if err != nil {
		l.logger.Warn().Err(err).Str("session", sessionID).
			Msg("usage-limit rotation: decide failed; falling back to block")
		return false
	}

	switch outcome.Kind {
	case rotation.OutcomeRotate:
		// Gate the real restart on CooldownApplied (NOT NextAccount): a
		// redelivered/duplicate cap the engine already handled reports
		// CooldownApplied==false, so we swallow it without a second restart —
		// exactly-one-rotation on duplicate signals. This CooldownApplied dedupe
		// gate is the INITIAL-cap path's alone; the resume-at-T sweep gates on
		// NextAccount != nil (the account is already cooling by then, so
		// CooldownApplied is false) and calls rotateAndRestart directly.
		if outcome.CooldownApplied && outcome.NextAccount != nil {
			if err := l.rotateAndRestart(ctx, session, outcome.NextAccount); err != nil {
				// Any restart failure degrades to today's Block (fail-safe).
				l.recordRotation(ctx, session, b, "ROTATION_OUTCOME_FAILED",
					outcome.NextAccount.ID, "restart failed", resetPtr)
				return false
			}
			detail := ""
			if resetPtr != nil {
				detail = "resets " + resetPtr.Format("15:04")
			}
			l.recordRotation(ctx, session, b, "ROTATION_OUTCOME_ROTATED",
				outcome.NextAccount.ID, detail, resetPtr)
			return true
		}
		l.logger.Info().Str("session", sessionID).
			Msg("usage-limit rotation: duplicate cap already handled by engine; no restart")
		return true
	case rotation.OutcomeAllExhausted:
		resumePtr := outcome.ResumeAt
		l.rotationRecorder.RecordExhausted(ctx, rotation.AuditEvent{
			SessionID:   session.ID,
			Provider:    b.Provider,
			Trigger:     "ROTATION_TRIGGER_USAGE_LIMITED",
			FromAccount: b.CappedAccountID,
			ResetAt:     &resumePtr,
			Outcome:     "ROTATION_OUTCOME_EXHAUSTED",
			Detail:      "all accounts cooling until " + outcome.ResumeAt.Format("15:04"),
		})
		l.parkAllCooling(ctx, sessionID, outcome.ResumeAt)
		return true
	case rotation.OutcomeStatusOnly:
		// Agent not rotation-capable ⇒ today's Block path.
		l.recordRotation(ctx, session, b, "ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY",
			"", "agent cannot rotate", resetPtr)
		return false
	default:
		return false
	}
}

// rotateAndRestart is the shared rotate-and-restart primitive used by BOTH the
// initial usage-limit intercept (attemptUsageLimitRotation) and the resume-at-T
// parked sweep (SweepParkedRotations). It materializes the next account's env
// overlay, restarts the run with the steering-prefixed plan resuming the prior
// agent session, persists the new run + incremented rotation count (clearing any
// parked resume-at stamp), and re-arms completion tracking. It restarts
// UNCONDITIONALLY given a non-nil next account — the CooldownApplied duplicate
// dedupe lives in attemptUsageLimitRotation, never here, so the sweep (where the
// capped account is already cooling) can drive a restart. Any failure logs
// (never secrets) and returns an error so callers degrade safely (initial path →
// today's Block; sweep → leave parked and retry next tick).
func (l *Lifecycle) rotateAndRestart(ctx context.Context, session *models.Session, next *models.Account) error {
	envOverlay, err := l.accountMaterializer.Materialize(ctx, next)
	if err != nil {
		l.logger.Warn().Err(err).Str("session", session.ID).
			Msg("usage-limit rotation: materialize account failed")
		return fmt.Errorf("materialize account: %w", err)
	}

	// Precedence managed>account>proof (managed is nil here), then wrap with the
	// worktree dotenv overlay exactly as the resurrect spawn does — the account
	// overlay wins over proof, proof wins over the worktree .env, and the repo's
	// stored LINEAR_API_KEY / SENTRY_* secrets fill beneath the .env so the
	// rotated run keeps its own repo's Linear workspace. A repo lookup failure
	// is non-fatal (OverlayWithRepo still guarantees LINEAR_API_KEY). Never logged.
	repo := RepoForSessionEnv(ctx, l.repos, session.RepoID, session.ID, "usage-limit rotation", l.logger)
	mergedEnv := dotenv.OverlayWithRepo(mergeSessionEnv(nil, envOverlay, l.resolveProofEnv()), session.WorktreePath, repo)

	resumedPrompt := steeringNotice + "\n\n" + session.Plan

	var resume *string
	if session.AgentSessionID != nil {
		resume = session.AgentSessionID
	}
	newID, err := l.agentRunner.StartByAgent(ctx, session.AgentName, session.WorktreePath, resumedPrompt, resume, "", session.Model, mergedEnv)
	if err != nil {
		l.logger.Warn().Err(err).Str("session", session.ID).
			Msg("usage-limit rotation: restart failed")
		return fmt.Errorf("restart run: %w", err)
	}

	implementingState := int(machine.ImplementingPlan)
	newCount := session.RotationAttemptCount + 1
	nextAccountID := next.ID
	nextAccountIDPtr := &nextAccountID
	// Clear any parked resume-at stamp atomically with the restart persistence so
	// the sweep never re-dispatches this run a second time.
	var clearResumeAt *string
	if _, err := l.sessions.Update(ctx, session.ID, db.UpdateSessionParams{
		AgentSessionID:       strPtr(newID),
		State:                &implementingState,
		RotationAttemptCount: &newCount,
		RotationResumeAt:     &clearResumeAt,
		AccountID:            &nextAccountIDPtr,
	}); err != nil {
		l.logger.Error().Err(err).Str("session", session.ID).
			Msg("usage-limit rotation: persist restart failed")
		return fmt.Errorf("persist restart: %w", err)
	}

	l.rearmCompletionForRotatedRun(session.ID, newID, session)

	l.logger.Info().
		Str("session", session.ID).
		Str("resume_id", newID).
		Int("rotation_attempt", newCount).
		Str("next_account_id", next.ID).
		Msg("usage-limit rotation: cooldown→select→restart→ImplementingPlan")
	return nil
}

// rearmCompletionForRotatedRun re-arms completion tracking for the restarted
// run. The rotated run is always restarted headless (StartByAgent print) — never
// tmux-hosted — so only the poll-fallback arm applies (the hookless-tmux branch
// used at initial start is structurally unreachable for a print run). Arming it
// drives the restarted run's completion back through SignalSessionRunComplete,
// which re-enters this same rotation intercept plus the cron gate and
// finalize/block fan-out, covering both cron and non-cron sessions. Without it a
// rotated run strands in ImplementingPlan — this step is load-bearing. The arm
// helpers nil-guard pollArmer/agent client and skip quietly when unset.
func (l *Lifecycle) rearmCompletionForRotatedRun(sessionID, newID string, session *models.Session) {
	if l.shouldArmHeadlessPollFallback(true, "", newID, nil) {
		l.armHeadlessPollFallback(sessionID, newID, session)
	}
}

// parkAllCooling parks a session whose provider has no available account but at
// least one is cooling, stamping the earliest recovery time so the resume-at-T
// sweep (Task 3) can wake it.
func (l *Lifecycle) parkAllCooling(ctx context.Context, sessionID string, resumeAt time.Time) {
	reason := fmt.Sprintf("usage-limited: all accounts cooling until %s", resumeAt.Format("15:04"))
	iso := resumeAt.Format(time.RFC3339)
	l.parkRotatedSession(ctx, sessionID, reason, &iso)
}

// parkRotatedSession CAS-transitions a live plan run to Blocked (the atomic CAS
// is the idempotency gate) and, on the winning transition, stamps a truncated
// non-secret reason plus the optional resume-at time. A non-winning CAS means a
// concurrent signal already advanced the session — idempotent no-op. The single
// park notification is emitted automatically by the DisplayTracker→Host
// NotifyStatusChange seam on the Blocked transition; no bespoke notify here.
func (l *Lifecycle) parkRotatedSession(ctx context.Context, sessionID, reason string, resumeAtISO *string) {
	advanced, err := l.sessions.UpdateStateConditional(ctx, sessionID, int(machine.Blocked), int(machine.ImplementingPlan))
	if err != nil {
		l.logger.Error().Err(err).Str("session", sessionID).
			Msg("usage-limit rotation park: transition to blocked failed")
		return
	}
	if !advanced {
		return
	}
	reason = truncateBlockedReason(reason)
	reasonPtr := &reason
	params := db.UpdateSessionParams{BlockedReason: &reasonPtr}
	if resumeAtISO != nil {
		params.RotationResumeAt = &resumeAtISO
	}
	if _, err := l.sessions.Update(ctx, sessionID, params); err != nil {
		l.logger.Error().Err(err).Str("session", sessionID).
			Msg("usage-limit rotation park: persist blocked reason failed")
	}
}
