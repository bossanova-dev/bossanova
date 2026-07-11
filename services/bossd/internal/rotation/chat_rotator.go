package rotation

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/safego"
)

// rotateTimeout bounds one stop→rebind→respawn orchestration.
const rotateTimeout = 2 * time.Minute

// Audit trigger strings stamped on every rotation audit event (stored as a free
// string in the rotation_event_store; no whitelist, no migration).
const (
	triggerUsageLimited    = "ROTATION_TRIGGER_USAGE_LIMITED"
	triggerAuthInvalidated = "ROTATION_TRIGGER_AUTH_INVALIDATED"
)

// ChatContext is the minimal session context a chat rotation needs.
type ChatContext struct {
	SessionID string
	RepoID    string
	Provider  string // Session.AgentName ("claude", "codex", ...)
	AccountID string // persisted account binding; empty machine account-0 is not probeable
}

// DecideRequest is the rotator-side view of a rotation decision request. The
// main.go adapter converts it into the BOS-173 engine's real rotation.Signal.
type DecideRequest struct {
	Provider       string
	SessionID      string
	AgentSessionID string
	AccountID      string
	ResetAt        time.Time // zero when the banner had no parseable reset
	// Kind selects the engine SignalKind. The zero value is UsageLimited, so the
	// existing usage-cap callers are unchanged; the auth path sets AuthInvalidated.
	Kind SignalKind
}

// DecisionKind classifies the rotator-side decision.
type DecisionKind int

const (
	// DecisionSwitch means AccountID is the account to rotate to.
	DecisionSwitch DecisionKind = iota + 1
	// DecisionAllExhausted means no account is available now (at least one is
	// cooling): the chat stays limited. ResumeAt is the earliest recovery time.
	DecisionAllExhausted
	// DecisionStatusOnly means the agent/provider cannot rotate at all
	// (capability short-circuit): do nothing. Remedy is agent/plugin-side.
	DecisionStatusOnly
	// DecisionNoEligibleAccount means rotation is capable but no account is
	// eligible to switch to and none will recover (all disabled/failed). Distinct
	// from DecisionStatusOnly so the audit steers operators to the real remedy —
	// enabling an account — rather than the agent's rotation capability (BOS-327).
	DecisionNoEligibleAccount
)

// NoEligibleAccountDetail is the actionable audit detail recorded for the
// no-eligible-account outcome. Exported and single-sourced so every rotation
// consumer of the engine's OutcomeNoEligibleAccount — the ChatRotator's
// usage-limited and auth-invalidated paths here AND the session Lifecycle's
// headless usage-limit intercept in services/bossd/internal/session — records
// the identical detail and cannot drift (BOS-327).
const NoEligibleAccountDetail = "no eligible account to rotate to — enable or re-authenticate one (e.g. `boss account update <id> --status active`)"

// Decision is the rotator-side view of the engine's Outcome, produced by the
// main.go adapter from the real rotation.Outcome.
type Decision struct {
	Kind      DecisionKind
	AccountID string    // set for DecisionSwitch
	Label     string    // human label of the chosen account, for logs
	ResumeAt  time.Time // set for DecisionAllExhausted (min future cooldown)
}

// ProactiveDecideRequest asks the engine adapter for the idlest eligible switch
// target for a bound account that is nearing (not at) its cap. (BOS-318)
type ProactiveDecideRequest struct {
	Provider       string
	SessionID      string
	AgentSessionID string
	BoundAccountID string
}

// ProactiveDecisionKind classifies the proactive-selection result.
type ProactiveDecisionKind int

const (
	// ProactiveNone means no materially-idler candidate exists: do nothing.
	ProactiveNone ProactiveDecisionKind = iota
	// ProactiveSwitch means AccountID is a candidate; CandidateUtil is its probed
	// utilization for the hysteresis gate.
	ProactiveSwitch
)

// ProactiveDecision is the rotator-side view of a proactive selection.
type ProactiveDecision struct {
	Kind          ProactiveDecisionKind
	AccountID     string
	Label         string
	CandidateUtil float64
}

// proactiveUtilTrigger is the Util7d level at/above which a bound account is
// "approaching cap" and its IDLE chats become proactive-rotation candidates.
const proactiveUtilTrigger = 0.8

// proactiveHysteresis is the minimum utilization gap (bound − candidate) that
// makes a candidate "materially idler" and worth a proactive switch. It damps
// churn between similarly-loaded accounts.
const proactiveHysteresis = 0.25

// UsageUtil reduces a usage snapshot to the single worst-case utilization
// fraction used to compare account load on one scale: max of Util5h/Util7d,
// bumped to 1 when the snapshot is explicitly rate-limited. It is the canonical
// reduction shared by the rotation package and the cmd wiring (candidate
// utilization probing), so the two cannot drift. (BOS-318)
func UsageUtil(snap models.UsageSnapshot) float64 {
	util := MaxUtilization(snap.Util5h, snap.Util7d)
	if UsageSnapshotRateLimited(snap) && util < 1 {
		return 1
	}
	return util
}

// SwitchRequest is the rotator-side view of the BOS-171 SwitchAccount primitive
// (stop pane → rebind → respawn with resume → in-chat notice). Auto marks the
// automatic path: there is no operator to confirm a mid-turn interrupt, so a chat
// that recovered to WORKING before the switch executes aborts fail-safe instead of
// being interrupted; the notice uses session.AutoRotateNotice wording with
// PreviousResetAt.
type SwitchRequest struct {
	SessionID       string
	AgentSessionID  string
	AccountID       string
	Auto            bool
	PreviousResetAt time.Time
}

// SwitchResult reports the switch outcome.
type SwitchResult struct {
	SwitchedToLabel string
	Fresh           bool // restarted fresh (stale/unsupported resume)
}

// RateLimitProbe authoritatively probes the currently bound account. Any error
// or non-confirming snapshot means do not cool/rotate.
type RateLimitProbe func(ctx context.Context, accountID string) (models.UsageSnapshot, error)

// ChatRotatorDeps carries the injected seams. All are required.
type ChatRotatorDeps struct {
	Logger         zerolog.Logger
	LoadConfig     func() (config.ManagedAccountsConfig, error)
	ChatContext    func(ctx context.Context, agentSessionID string) (ChatContext, error)
	CurrentStatus  func(agentSessionID string) bossanovav1.ChatStatus
	RateLimitProbe RateLimitProbe
	// CurrentAuthFailed re-checks, at dispatch time, that the pane is still
	// auth-failed (mirror of CurrentStatus for the BOS-316 auth path). A pane that
	// recovered between the transition and the dispatched goroutine aborts.
	CurrentAuthFailed func(agentSessionID string) bool
	// AuthProbe authoritatively confirms a typed 401 on the bound account. It
	// returns confirmed=true ONLY when the account's probe fails with
	// codes.Unauthenticated / agenterr.KindAuthInvalidated; a healthy or
	// usage-limited snapshot, an unsupported probe, or any other error returns
	// confirmed=false. This is what makes the auth path proceed only on a real
	// 401 (no false positives).
	AuthProbe func(ctx context.Context, accountID string) (confirmed bool, err error)
	Decide    func(ctx context.Context, req DecideRequest) (Decision, error)
	Switch    func(ctx context.Context, req SwitchRequest) (SwitchResult, error)
	Now       func() time.Time // nil = time.Now
	// LiveChatStatuses returns the current non-stale live chats keyed by
	// agentSessionID → status (backed by status.Tracker.Snapshot()). nil ⇒ no
	// chats; the proactive sweep is a no-op without this seam. (BOS-318)
	LiveChatStatuses func() map[string]bossanovav1.ChatStatus
	// ProactiveCandidate selects the idlest eligible account to pre-cap-rotate
	// onto, with its probed utilization, WITHOUT cooling the bound account. Kind ==
	// ProactiveNone when no candidate qualifies. (BOS-318)
	ProactiveCandidate func(ctx context.Context, req ProactiveDecideRequest) (ProactiveDecision, error)
	// Recorder audits each rotation decision outcome (BOS-176). Nil is safe: the
	// Recorder's methods are nil-receiver no-ops.
	Recorder *Recorder
}

// ChatRotator auto-rotates interactive chats on CHAT_STATUS_LIMITED transitions
// (Epic 4.3, D4). Trigger + policy glue only: pane lifecycle stays owned by the
// BOS-171 switch primitive / chat tracker (BOS-153). Fail-safe: any error ⇒ no
// rotation, the chat stays LIMITED.
type ChatRotator struct {
	deps ChatRotatorDeps

	mu          sync.Mutex
	inFlight    map[string]bool      // agentSessionID → rotation running
	lastAttempt map[string]time.Time // agentSessionID → last attempt (rate limit)
	// proactiveLastAttempt rate-limits the proactive pre-cap sweep per chat,
	// keyed by ProactiveSweepInterval. It is deliberately separate from
	// lastAttempt so proactive cadence is not entangled with the reactive
	// ChatRotateMinInterval and neither path can suppress the other. (BOS-318)
	proactiveLastAttempt map[string]time.Time
	active               sync.WaitGroup // observed by idleForTest
}

// NewChatRotator builds a ChatRotator from its dependency seams.
func NewChatRotator(deps ChatRotatorDeps) *ChatRotator {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &ChatRotator{
		deps:                 deps,
		inFlight:             map[string]bool{},
		lastAttempt:          map[string]time.Time{},
		proactiveLastAttempt: map[string]time.Time{},
	}
}

// OnChatStatus is chained into the chat-status tracker's on-update hook
// (main.go), which only fires on real transitions. It must return fast: policy
// checks that need I/O run in a dispatched goroutine.
func (r *ChatRotator) OnChatStatus(agentSessionID string, st bossanovav1.ChatStatus, resetAt time.Time) {
	if st != bossanovav1.ChatStatus_CHAT_STATUS_LIMITED {
		return // WORKING/IDLE/QUESTION/STOPPED chats are NEVER rotated.
	}
	if !r.reserveAttempt(agentSessionID) {
		return
	}
	r.active.Add(1)
	safego.Go(r.deps.Logger, func() {
		defer r.active.Done()
		defer r.releaseInFlight(agentSessionID)
		r.rotate(agentSessionID, resetAt)
	})
}

// OnAuthFailed is chained into the chat-status tracker's on-auth-change hook
// (main.go), which fires only on an effective auth-failed transition. Like
// OnChatStatus it must return fast: the confirming probe + switch run in a
// dispatched goroutine. It reuses the SAME inFlight/rate-limit guard as the
// usage-limit path so a chat is never double-rotated by a near-simultaneous
// limit+auth signal, and a persistent auth-failed marker cannot storm rotations.
func (r *ChatRotator) OnAuthFailed(agentSessionID string) {
	if !r.reserveAttempt(agentSessionID) {
		return
	}
	r.active.Add(1)
	safego.Go(r.deps.Logger, func() {
		defer r.active.Done()
		defer r.releaseInFlight(agentSessionID)
		r.rotateAuth(agentSessionID)
	})
}

// reserveAttempt applies the shared per-chat inFlight + rate-limit guard for both
// rotation triggers. It returns true — marking the chat in-flight and charging
// the limiter at attempt time (belt-and-braces vs banner flap, not at success
// time) — when a fresh rotation attempt may proceed; a true result MUST be paired
// with a deferred releaseInFlight. It returns false when a rotation is already
// running for the chat or when the chat rotated inside the rate-limit window.
func (r *ChatRotator) reserveAttempt(agentSessionID string) bool {
	// Default to the canonical rate-limit window (single-sourced in config) when
	// the config load fails, so the fallback can never drift from the real default.
	// LoadConfig runs outside the lock so its I/O cannot block other triggers.
	minInterval := config.ManagedAccountsConfig{}.ChatRotateMinInterval()
	if cfg, err := r.deps.LoadConfig(); err == nil {
		minInterval = cfg.ChatRotateMinInterval()
	}

	now := r.deps.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	// Evict stale rate-limit entries so lastAttempt does not grow unbounded over a
	// long-lived daemon: an entry older than the rate-limit window no longer
	// suppresses anything, so dropping it is behaviour-neutral. This bounds the map
	// to chats that hit a rotation trigger within the last minInterval. Evicting an
	// entry whose rotation is somehow still in flight (only reachable if minInterval
	// is configured below the 2m rotate timeout) is harmless: the separate inFlight
	// guard below, not lastAttempt, is what prevents a concurrent second rotation.
	for id, last := range r.lastAttempt {
		if now.Sub(last) >= minInterval {
			delete(r.lastAttempt, id)
		}
	}
	if r.inFlight[agentSessionID] {
		return false
	}
	if last, ok := r.lastAttempt[agentSessionID]; ok && now.Sub(last) < minInterval {
		r.deps.Logger.Debug().Str("agent_session_id", agentSessionID).
			Msg("auto-rotate suppressed by per-chat rate limit")
		return false
	}
	r.inFlight[agentSessionID] = true
	r.lastAttempt[agentSessionID] = now
	return true
}

// releaseInFlight clears the in-flight marker for a chat once its dispatched
// rotation goroutine finishes.
func (r *ChatRotator) releaseInFlight(agentSessionID string) {
	r.mu.Lock()
	delete(r.inFlight, agentSessionID)
	r.mu.Unlock()
}

func (r *ChatRotator) rotate(agentSessionID string, resetAt time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), rotateTimeout)
	defer cancel()
	log := r.deps.Logger.With().Str("agent_session_id", agentSessionID).Logger()

	// These two pre-context failures deliberately record NO audit row: the
	// session context (cc) is not yet loaded, so a meaningful AuditEvent cannot be
	// built. The "exactly one audit row per forced-LIMITED transition" invariant
	// therefore covers post-context paths only (everything below the ChatContext
	// load). See BOS-315.
	cfg, err := r.deps.LoadConfig()
	if err != nil {
		log.Warn().Err(err).Msg("auto-rotate: config load failed; leaving chat limited")
		r.forgetAttempt(agentSessionID)
		return
	}
	cc, err := r.deps.ChatContext(ctx, agentSessionID)
	if err != nil {
		log.Warn().Err(err).Msg("auto-rotate: chat context lookup failed")
		r.forgetAttempt(agentSessionID)
		return
	}
	if !cfg.ManagedAccountsEnabled() || !cfg.AutoRotateChatsEnabled(cc.RepoID) {
		log.Debug().Str("repo_id", cc.RepoID).Msg("auto-rotate: opted out; leaving chat limited")
		var disabledReset *time.Time
		if !resetAt.IsZero() {
			disabledReset = &resetAt
		}
		r.deps.Recorder.Record(ctx, AuditEvent{
			SessionID: cc.SessionID, ChatID: agentSessionID, Provider: cc.Provider,
			Trigger: triggerUsageLimited, ResetAt: disabledReset,
			Outcome: "ROTATION_OUTCOME_STATUS_ONLY_DISABLED",
			Detail:  "automatic rotation disabled",
		})
		return
	}
	// Re-check: only act if the chat is STILL limited right now (the pane may have
	// redrawn/recovered between the transition and this dispatch).
	if st := r.deps.CurrentStatus(agentSessionID); st != bossanovav1.ChatStatus_CHAT_STATUS_LIMITED {
		log.Debug().Stringer("status", st).Msg("auto-rotate: chat no longer limited; aborting")
		var recoveredReset *time.Time
		if !resetAt.IsZero() {
			recoveredReset = &resetAt
		}
		r.deps.Recorder.Record(ctx, AuditEvent{
			SessionID: cc.SessionID, ChatID: agentSessionID, Provider: cc.Provider,
			Trigger: triggerUsageLimited, ResetAt: recoveredReset,
			Outcome: "ROTATION_OUTCOME_STATUS_ONLY_RECOVERED",
			Detail:  "chat recovered before rotation",
		})
		return
	}
	unbound := cc.AccountID == ""
	probedReset := time.Time{}
	if unbound {
		// Unbound session (account-0, BOS-320): there is no bound account to probe.
		// Managed accounts is already confirmed enabled above (:317). Trust the
		// detected banner and use its reset time; rotate onto an eligible
		// registered account.
		if !resetAt.IsZero() {
			probedReset = resetAt
		}
		log.Info().Msg("auto-rotate: unbound session; trusting banner (no bound account to probe)")
	} else {
		if r.deps.RateLimitProbe == nil {
			log.Warn().Msg("auto-rotate: no usage probe available; leaving chat limited")
			r.recordProbeStatusOnly(ctx, cc, agentSessionID, nil, "usage probe unavailable")
			r.forgetAttempt(agentSessionID)
			return
		}
		snap, err := r.deps.RateLimitProbe(ctx, cc.AccountID)
		if err != nil {
			log.Warn().Err(err).Str("account_id", cc.AccountID).
				Msg("auto-rotate: usage probe failed; leaving chat limited")
			r.recordProbeStatusOnly(ctx, cc, agentSessionID, nil, "usage probe failed")
			r.forgetAttempt(agentSessionID)
			return
		}
		if !UsageSnapshotConfirmsLimited(snap) {
			// Warn (BOS-315): a banner that trips LIMITED while the authoritative probe
			// disagrees is exactly the disagreement an operator wants surfaced.
			log.Warn().Str("account_id", cc.AccountID).Str("status", snap.Status).
				Float64("util_5h", snap.Util5h).Float64("util_7d", snap.Util7d).
				Msg("auto-rotate: usage probe says account is not limited; ignoring loose trigger")
			r.recordProbeStatusOnly(ctx, cc, agentSessionID, UsageSnapshotResetAt(snap), "usage probe did not confirm limit")
			return
		}
		if reset := UsageSnapshotResetAt(snap); reset != nil {
			probedReset = *reset
		}
	}

	// Build auditBase before Decide so the Decide-failure path (and every
	// post-decision path below) records exactly one audit row (BOS-315).
	var resetPtr *time.Time
	if !probedReset.IsZero() {
		resetPtr = &probedReset
	}
	auditBase := AuditEvent{
		SessionID: cc.SessionID,
		ChatID:    agentSessionID,
		Provider:  cc.Provider,
		Trigger:   triggerUsageLimited,
		ResetAt:   resetPtr,
	}

	decision, err := r.deps.Decide(ctx, DecideRequest{
		Provider:       cc.Provider,
		SessionID:      cc.SessionID,
		AgentSessionID: agentSessionID,
		AccountID:      cc.AccountID,
		ResetAt:        probedReset,
	})
	if err != nil {
		log.Warn().Err(err).Msg("auto-rotate: engine decide failed; leaving chat limited")
		failed := auditBase
		failed.Outcome = "ROTATION_OUTCOME_FAILED"
		failed.Detail = "engine decide failed"
		r.deps.Recorder.Record(ctx, failed)
		return
	}

	switch decision.Kind {
	case DecisionSwitch:
		res, err := r.deps.Switch(ctx, SwitchRequest{
			SessionID:       cc.SessionID,
			AgentSessionID:  agentSessionID,
			AccountID:       decision.AccountID,
			Auto:            true,
			PreviousResetAt: probedReset,
		})
		if err != nil {
			// A failed switch is fail-safe: leave the chat as-is. This also covers
			// the benign race where the chat recovered to WORKING before the switch
			// executed (SwitchAccount aborts with ErrChatMidTurn rather than kill a
			// live turn), so the message avoids asserting the chat is still limited.
			log.Warn().Err(err).Str("account_id", decision.AccountID).
				Msg("auto-rotate: switch did not complete; leaving chat as-is")
			failed := auditBase
			failed.ToAccount = decision.Label
			failed.Outcome = "ROTATION_OUTCOME_FAILED"
			failed.Detail = "switch did not complete"
			r.deps.Recorder.Record(ctx, failed)
			return
		}
		log.Info().Str("account", res.SwitchedToLabel).Bool("fresh", res.Fresh).
			Msg("auto-rotate: chat rotated to next account")
		rotated := auditBase
		rotated.ToAccount = res.SwitchedToLabel
		rotated.Outcome = "ROTATION_OUTCOME_ROTATED"
		// Distinguish the unbound (banner-trusted) rotation from a probe-confirmed
		// one so logs record which signal drove the switch (BOS-320). Keep this a
		// single Record call (BOS-315: one audit row per forced-LIMITED transition).
		if resetPtr != nil {
			rotated.Detail = "resets " + resetPtr.Format("15:04")
			if unbound {
				rotated.Detail += " (unbound; banner-trusted)"
			}
		} else if unbound {
			rotated.Detail = "unbound; banner-trusted"
		}
		r.deps.Recorder.Record(ctx, rotated)
	case DecisionAllExhausted:
		// Terminal park: the chat keeps CHAT_STATUS_LIMITED; parked-timer UX +
		// single notification are BOS-176. One log line, no loop.
		log.Info().Time("resume_at", decision.ResumeAt).
			Msg("auto-rotate: all accounts cooling; chat stays limited")
		exhausted := auditBase
		exhausted.ResetAt = &decision.ResumeAt
		exhausted.Outcome = "ROTATION_OUTCOME_EXHAUSTED"
		exhausted.Detail = "all accounts cooling until " + decision.ResumeAt.Format("15:04")
		r.deps.Recorder.RecordExhausted(ctx, exhausted)
	case DecisionStatusOnly:
		log.Debug().Msg("auto-rotate: agent has no rotation capability; status only")
		statusOnly := auditBase
		statusOnly.Outcome = "ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY"
		statusOnly.Detail = "agent cannot rotate"
		r.deps.Recorder.Record(ctx, statusOnly)
	case DecisionNoEligibleAccount:
		log.Debug().Msg("auto-rotate: no eligible account to rotate to; status only")
		noEligible := auditBase
		noEligible.Outcome = "ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT"
		noEligible.Detail = NoEligibleAccountDetail
		r.deps.Recorder.Record(ctx, noEligible)
	default:
		log.Warn().Int("kind", int(decision.Kind)).
			Msg("auto-rotate: unknown decision kind; leaving chat limited")
		unknown := auditBase
		unknown.Outcome = "ROTATION_OUTCOME_FAILED"
		unknown.Detail = "unknown decision kind"
		r.deps.Recorder.Record(ctx, unknown)
	}
}

// rotateAuth is the auth-invalidation sibling of rotate: structurally parallel
// but confirms a typed 401 (rather than a usage cap) before driving the engine
// with the AuthInvalidated signal kind. Fail-safe throughout: any error, any
// non-401 probe result, or a pane that recovered before the switch executes ⇒ no
// rotation, chat left as-is. Every audit event on this path is stamped
// triggerAuthInvalidated.
func (r *ChatRotator) rotateAuth(agentSessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), rotateTimeout)
	defer cancel()
	log := r.deps.Logger.With().Str("agent_session_id", agentSessionID).Logger()

	// As in rotate, these two pre-context failures record NO audit row: the
	// session context is not yet loaded, so a meaningful AuditEvent cannot be built.
	cfg, err := r.deps.LoadConfig()
	if err != nil {
		log.Warn().Err(err).Msg("auto-rotate(auth): config load failed; leaving chat as-is")
		r.forgetAttempt(agentSessionID)
		return
	}
	cc, err := r.deps.ChatContext(ctx, agentSessionID)
	if err != nil {
		log.Warn().Err(err).Msg("auto-rotate(auth): chat context lookup failed")
		r.forgetAttempt(agentSessionID)
		return
	}
	if !cfg.ManagedAccountsEnabled() || !cfg.AutoRotateChatsEnabled(cc.RepoID) {
		log.Debug().Str("repo_id", cc.RepoID).Msg("auto-rotate(auth): opted out; leaving chat as-is")
		r.deps.Recorder.Record(ctx, AuditEvent{
			SessionID: cc.SessionID, ChatID: agentSessionID, Provider: cc.Provider,
			Trigger: triggerAuthInvalidated,
			Outcome: "ROTATION_OUTCOME_STATUS_ONLY_DISABLED",
			Detail:  "automatic rotation disabled",
		})
		return
	}
	// Re-check: only act if the pane is STILL auth-failed right now (it may have
	// recovered between the transition and this dispatch).
	if r.deps.CurrentAuthFailed == nil || !r.deps.CurrentAuthFailed(agentSessionID) {
		log.Debug().Msg("auto-rotate(auth): pane no longer auth-failed; aborting")
		r.deps.Recorder.Record(ctx, AuditEvent{
			SessionID: cc.SessionID, ChatID: agentSessionID, Provider: cc.Provider,
			Trigger: triggerAuthInvalidated,
			Outcome: "ROTATION_OUTCOME_STATUS_ONLY_RECOVERED",
			Detail:  "pane recovered before rotation",
		})
		return
	}
	if cc.AccountID == "" || r.deps.AuthProbe == nil {
		log.Warn().Msg("auto-rotate(auth): no auth probe available; leaving chat as-is")
		r.recordAuthProbeStatusOnly(ctx, cc, agentSessionID, "auth probe unavailable")
		r.forgetAttempt(agentSessionID)
		return
	}
	confirmed, err := r.deps.AuthProbe(ctx, cc.AccountID)
	if err != nil {
		log.Warn().Err(err).Str("account_id", cc.AccountID).
			Msg("auto-rotate(auth): auth probe failed; leaving chat as-is")
		r.recordAuthProbeStatusOnly(ctx, cc, agentSessionID, "auth probe failed")
		r.forgetAttempt(agentSessionID)
		return
	}
	if !confirmed {
		// The pane tripped auth-failed but the authoritative probe does not confirm a
		// typed 401: never rotate on the loose trigger (no false positive).
		log.Warn().Str("account_id", cc.AccountID).
			Msg("auto-rotate(auth): probe did not confirm auth invalidation; ignoring loose trigger")
		r.recordAuthProbeStatusOnly(ctx, cc, agentSessionID, "auth probe did not confirm invalidation")
		return
	}

	auditBase := AuditEvent{
		SessionID: cc.SessionID,
		ChatID:    agentSessionID,
		Provider:  cc.Provider,
		Trigger:   triggerAuthInvalidated,
	}

	decision, err := r.deps.Decide(ctx, DecideRequest{
		Provider:       cc.Provider,
		SessionID:      cc.SessionID,
		AgentSessionID: agentSessionID,
		AccountID:      cc.AccountID,
		Kind:           AuthInvalidated,
	})
	if err != nil {
		log.Warn().Err(err).Msg("auto-rotate(auth): engine decide failed; leaving chat as-is")
		failed := auditBase
		failed.Outcome = "ROTATION_OUTCOME_FAILED"
		failed.Detail = "engine decide failed"
		r.deps.Recorder.Record(ctx, failed)
		return
	}

	switch decision.Kind {
	case DecisionSwitch:
		res, err := r.deps.Switch(ctx, SwitchRequest{
			SessionID:      cc.SessionID,
			AgentSessionID: agentSessionID,
			AccountID:      decision.AccountID,
			Auto:           true,
		})
		if err != nil {
			// Fail-safe: leave the chat as-is (also covers the benign race where the
			// chat recovered to WORKING and SwitchAccount aborts rather than kill a
			// live turn).
			log.Warn().Err(err).Str("account_id", decision.AccountID).
				Msg("auto-rotate(auth): switch did not complete; leaving chat as-is")
			failed := auditBase
			failed.ToAccount = decision.Label
			failed.Outcome = "ROTATION_OUTCOME_FAILED"
			failed.Detail = "switch did not complete"
			r.deps.Recorder.Record(ctx, failed)
			return
		}
		log.Info().Str("account", res.SwitchedToLabel).Bool("fresh", res.Fresh).
			Msg("auto-rotate(auth): chat rotated to next account")
		rotated := auditBase
		rotated.ToAccount = res.SwitchedToLabel
		rotated.Outcome = "ROTATION_OUTCOME_ROTATED"
		r.deps.Recorder.Record(ctx, rotated)
	case DecisionAllExhausted:
		// Terminal park: the pane stays as-is. One log line, no loop.
		log.Info().Time("resume_at", decision.ResumeAt).
			Msg("auto-rotate(auth): all accounts cooling; chat stays as-is")
		exhausted := auditBase
		exhausted.ResetAt = &decision.ResumeAt
		exhausted.Outcome = "ROTATION_OUTCOME_EXHAUSTED"
		exhausted.Detail = "all accounts cooling until " + decision.ResumeAt.Format("15:04")
		r.deps.Recorder.RecordExhausted(ctx, exhausted)
	case DecisionStatusOnly:
		// The provider/agent cannot rotate at all (capability short-circuit): park,
		// no loop. Remedy is agent/plugin-side, so keep the NO_CAPABILITY label.
		log.Debug().Msg("auto-rotate(auth): no rotation capability; status only")
		statusOnly := auditBase
		statusOnly.Outcome = "ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY"
		statusOnly.Detail = "no rotation target available"
		r.deps.Recorder.Record(ctx, statusOnly)
	case DecisionNoEligibleAccount:
		// Rotation is capable but every other account is disabled/failed and none
		// will recover: park, no loop. Steer the operator to the real remedy.
		log.Debug().Msg("auto-rotate(auth): no eligible account to rotate to; status only")
		noEligible := auditBase
		noEligible.Outcome = "ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT"
		noEligible.Detail = NoEligibleAccountDetail
		r.deps.Recorder.Record(ctx, noEligible)
	default:
		log.Warn().Int("kind", int(decision.Kind)).
			Msg("auto-rotate(auth): unknown decision kind; leaving chat as-is")
		unknown := auditBase
		unknown.Outcome = "ROTATION_OUTCOME_FAILED"
		unknown.Detail = "unknown decision kind"
		r.deps.Recorder.Record(ctx, unknown)
	}
}

// SweepProactive periodically pre-empts a cap: for each IDLE live chat whose
// bound account is approaching its 7-day cap (Util7d ≥ proactiveUtilTrigger),
// it rotates to a materially-idler candidate (bound util − candidate util ≥
// proactiveHysteresis) via the Auto switch path. It is a strict no-op unless
// ProactiveRotation is explicitly enabled AND the global kill switch is on.
// WORKING chats are never considered; every error/missing seam skips that chat.
// It returns the number of chats actually switched. (BOS-318)
func (r *ChatRotator) SweepProactive(ctx context.Context) (switched int) {
	cfg, err := r.deps.LoadConfig()
	if err != nil {
		r.deps.Logger.Warn().Err(err).Msg("proactive-sweep: config load failed; skipping")
		return 0
	}
	if !cfg.ManagedAccountsEnabled() || !cfg.ProactiveRotationEnabled() {
		return 0 // default OFF ⇒ complete no-op
	}
	if r.deps.LiveChatStatuses == nil || r.deps.ProactiveCandidate == nil {
		return 0
	}
	now := r.deps.Now()
	minInterval := cfg.ProactiveSweepInterval()
	for agentSessionID, st := range r.deps.LiveChatStatuses() {
		if st != bossanovav1.ChatStatus_CHAT_STATUS_IDLE {
			continue // WORKING/QUESTION/STOPPED/LIMITED are never proactively rotated
		}
		if r.proactiveSuppressed(agentSessionID, now, minInterval) {
			continue
		}
		if r.tryProactiveOne(ctx, agentSessionID, cfg, now) {
			switched++
		}
	}
	return switched
}

// tryProactiveOne runs the proactive-rotation guards for one IDLE chat, in order,
// each miss skipping the chat fail-safe. It charges the per-chat limiter at
// attempt time (just before the switch), so a chat that reaches the switch is not
// retried next sweep even if the switch races a mid-turn recovery. It returns
// true only when the chat was actually switched. (BOS-318)
func (r *ChatRotator) tryProactiveOne(ctx context.Context, agentSessionID string, cfg config.ManagedAccountsConfig, now time.Time) bool {
	log := r.deps.Logger.With().Str("agent_session_id", agentSessionID).Logger()

	cc, err := r.deps.ChatContext(ctx, agentSessionID)
	if err != nil {
		log.Debug().Err(err).Msg("proactive-sweep: chat context lookup failed; skipping")
		return false
	}
	if !cfg.AutoRotateChatsEnabled(cc.RepoID) {
		log.Debug().Str("repo_id", cc.RepoID).Msg("proactive-sweep: repo opted out; skipping")
		return false
	}
	if cc.AccountID == "" || r.deps.RateLimitProbe == nil {
		return false
	}
	snap, err := r.deps.RateLimitProbe(ctx, cc.AccountID)
	if err != nil {
		log.Debug().Err(err).Str("account_id", cc.AccountID).
			Msg("proactive-sweep: usage probe failed; skipping")
		return false
	}
	// Fail safe: an unfetched or provider-unsupported snapshot never counts as
	// "near cap" (mirrors the reactive path's authoritative-probe requirement).
	if snap.FetchedAt == nil || UsageSnapshotProbeUnavailable(snap) {
		return false
	}
	if snap.Util7d < proactiveUtilTrigger {
		return false // not approaching the 7-day cap
	}
	boundUtil := UsageUtil(snap)

	dec, err := r.deps.ProactiveCandidate(ctx, ProactiveDecideRequest{
		Provider:       cc.Provider,
		SessionID:      cc.SessionID,
		AgentSessionID: agentSessionID,
		BoundAccountID: cc.AccountID,
	})
	if err != nil {
		log.Debug().Err(err).Msg("proactive-sweep: candidate selection failed; skipping")
		return false
	}
	if dec.Kind != ProactiveSwitch {
		return false // no materially-idler candidate
	}
	if boundUtil-dec.CandidateUtil < proactiveHysteresis {
		log.Debug().Float64("bound_util", boundUtil).Float64("candidate_util", dec.CandidateUtil).
			Msg("proactive-sweep: candidate not materially idler; skipping")
		return false
	}
	// Reserve the shared per-chat in-flight marker so a proactive switch is mutually
	// exclusive with a reactive (usage-limit / auth) rotation on the same chat.
	// SwitchAccount has no internal per-chat serialization, so this marker — the same
	// one reserveAttempt sets for the reactive paths — is the only thing that prevents
	// two concurrent stop→rebind→respawn cycles on one pane. The IDLE re-check below
	// does NOT cover this on its own: the auth path (rotateAuth) re-checks
	// CurrentAuthFailed, which is orthogonal to IDLE, so an auth rotation can run
	// concurrently with a proactive switch on the same IDLE chat. If a reactive
	// rotation already holds the marker, skip fail-safe. (BOS-318)
	r.mu.Lock()
	if r.inFlight[agentSessionID] {
		r.mu.Unlock()
		log.Debug().Msg("proactive-sweep: rotation already in flight for chat; skipping")
		return false
	}
	r.inFlight[agentSessionID] = true
	r.mu.Unlock()
	defer r.releaseInFlight(agentSessionID)

	// Re-check under the reservation: the pane may have started a turn between
	// enumeration and here. Holding inFlight first closes the window in which a
	// reactive trigger could start a concurrent rotation after this check passes.
	if st := r.deps.CurrentStatus(agentSessionID); st != bossanovav1.ChatStatus_CHAT_STATUS_IDLE {
		log.Debug().Stringer("status", st).Msg("proactive-sweep: chat no longer idle; aborting")
		return false
	}
	// Charge the limiter before the switch (belt-and-braces against churn): a chat
	// that reaches this point is not retried next sweep even if the switch fails.
	r.mu.Lock()
	r.proactiveLastAttempt[agentSessionID] = now
	r.mu.Unlock()

	res, err := r.deps.Switch(ctx, SwitchRequest{
		SessionID:      cc.SessionID,
		AgentSessionID: agentSessionID,
		AccountID:      dec.AccountID,
		Auto:           true,
		// PreviousResetAt stays zero: the account is nearing, not at, its cap.
	})
	if err != nil {
		// Fail-safe: leave the chat as-is (also covers the benign race where the
		// chat recovered to WORKING and SwitchAccount aborts rather than kill a live
		// turn). The limiter is already charged, so it still counts as an attempt.
		log.Warn().Err(err).Str("account_id", dec.AccountID).
			Msg("proactive-sweep: switch did not complete; leaving chat as-is")
		failed := AuditEvent{
			SessionID: cc.SessionID, ChatID: agentSessionID, Provider: cc.Provider,
			Trigger: triggerUsageLimited, FromAccount: cc.AccountID, ToAccount: dec.Label,
			Outcome: "ROTATION_OUTCOME_FAILED", Detail: "proactive pre-cap switch did not complete",
		}
		r.deps.Recorder.Record(ctx, failed)
		return false
	}
	log.Info().Str("account", res.SwitchedToLabel).Bool("fresh", res.Fresh).
		Msg("proactive-sweep: rotated idle chat off soon-to-cap account")
	rotated := AuditEvent{
		SessionID: cc.SessionID, ChatID: agentSessionID, Provider: cc.Provider,
		Trigger: triggerUsageLimited, FromAccount: cc.AccountID, ToAccount: res.SwitchedToLabel,
		Outcome: "ROTATION_OUTCOME_ROTATED", Detail: "proactive pre-cap",
	}
	r.deps.Recorder.Record(ctx, rotated)
	return true
}

// proactiveSuppressed applies the per-chat proactive rate limit over
// proactiveLastAttempt, mirroring reserveAttempt's eviction+window shape but
// keyed by ProactiveSweepInterval. It evicts stale entries so the map stays
// bounded to chats that attempted a proactive switch within the last interval.
func (r *ChatRotator) proactiveSuppressed(agentSessionID string, now time.Time, minInterval time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, last := range r.proactiveLastAttempt {
		if now.Sub(last) >= minInterval {
			delete(r.proactiveLastAttempt, id)
		}
	}
	if last, ok := r.proactiveLastAttempt[agentSessionID]; ok && now.Sub(last) < minInterval {
		r.deps.Logger.Debug().Str("agent_session_id", agentSessionID).
			Msg("proactive-sweep suppressed by per-chat rate limit")
		return true
	}
	return false
}

func (r *ChatRotator) recordAuthProbeStatusOnly(ctx context.Context, cc ChatContext, agentSessionID string, detail string) {
	r.deps.Recorder.Record(ctx, AuditEvent{
		SessionID:   cc.SessionID,
		ChatID:      agentSessionID,
		Provider:    cc.Provider,
		Trigger:     triggerAuthInvalidated,
		FromAccount: cc.AccountID,
		Outcome:     "ROTATION_OUTCOME_STATUS_ONLY_PROBE_UNCONFIRMED",
		Detail:      detail,
	})
}

func (r *ChatRotator) recordProbeStatusOnly(ctx context.Context, cc ChatContext, agentSessionID string, resetAt *time.Time, detail string) {
	r.deps.Recorder.Record(ctx, AuditEvent{
		SessionID:   cc.SessionID,
		ChatID:      agentSessionID,
		Provider:    cc.Provider,
		Trigger:     triggerUsageLimited,
		FromAccount: cc.AccountID,
		ResetAt:     resetAt,
		Outcome:     "ROTATION_OUTCOME_STATUS_ONLY_PROBE_UNCONFIRMED",
		Detail:      detail,
	})
}

func (r *ChatRotator) forgetAttempt(agentSessionID string) {
	r.mu.Lock()
	delete(r.lastAttempt, agentSessionID)
	r.mu.Unlock()
}

// idleForTest reports whether no rotation goroutine is active. Test-only.
func (r *ChatRotator) idleForTest() bool {
	done := make(chan struct{})
	go func() { r.active.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(time.Millisecond):
		return false
	}
}
