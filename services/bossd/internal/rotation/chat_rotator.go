package rotation

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
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

// Respawn-in-place audit outcome strings (BOS-482). Free strings like the rest of
// the rotation outcomes; their apiversion down-convert lives in bossalib/apiversion
// (V20260723). RESPAWNED_SAME_ACCOUNT is the healthy-probe wedge remedy; CAP is its
// runaway guard.
const (
	outcomeRespawnedSameAccount = "ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT"
	outcomeRespawnCapExhausted  = "ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED"
)

// Respawn-in-place policy constants (BOS-482). All in-memory: a daemon restart
// resets every counter and timer, which is correct because the restart is itself
// the event that wedges the in-proxy token.
const (
	// reprobeInterval is how long the rotator waits before re-probing a pane that is
	// auth-failed while its bound account probes healthy. status.Tracker.SetAuthFailed
	// is edge-triggered, so a wedged pane dispatches rotateAuth exactly once; this
	// timer re-drives the auth path so consecutive Healthy confirmations can
	// accumulate to the respawn threshold instead of the pane staying wedged forever.
	reprobeInterval = 5 * time.Minute
	// healthyRespawnThreshold is the number of consecutive Healthy probes required
	// before respawning in place. One probe could be a transient blip; two confirms
	// the stable plumbing-failure signature.
	healthyRespawnThreshold = 2
	// healthyCountTTL bounds how long a partial Healthy streak survives without a
	// fresh confirmation: a gap longer than this resets the streak.
	healthyCountTTL = 30 * time.Minute
	// respawnCap and respawnCapWindow bound respawns to at most respawnCap per chat
	// per window so a pane that respawns without ever recovering cannot loop.
	respawnCap       = 2
	respawnCapWindow = time.Hour
)

// ErrSwitchAborted is the rotation-owned sentinel the Switch adapter returns (in
// place of session.ErrChatMidTurn, which the rotation package cannot import) when a
// switch was refused fail-safe: the chat recovered to WORKING / was mid-turn, so the
// pane was deliberately not interrupted. On the respawn-in-place path this is NOT a
// failure — the rotator leaves the chat as-is, keeps the respawn attempt charged, and
// re-probes later — so it is never recorded as ROTATION_OUTCOME_FAILED. (BOS-482)
var ErrSwitchAborted = errors.New("rotation: switch aborted (chat mid-turn); left as-is")

// AuthProbeResult classifies an authoritative probe of the bound account on the
// auth-invalidation path. It replaces the old (confirmed bool, err error) pair so
// the rotator can distinguish a real 401 (rotate to another account) from a healthy
// account behind a wedged pane (respawn in place on the same account) from an
// inconclusive probe (do nothing, re-probe later). (BOS-482)
type AuthProbeResult int

const (
	// AuthProbeUnknown means the probe could not classify the account: it errored,
	// is unsupported, or returned an ambiguous snapshot. Never advances the respawn
	// streak and never drives a rotation. Zero value so an absent/failed probe is
	// safe by default.
	AuthProbeUnknown AuthProbeResult = iota
	// AuthProbeConfirmed401 means the bound account itself returned a typed 401
	// (codes.Unauthenticated / agenterr.KindAuthInvalidated): the credential is
	// genuinely invalid, so rotate to another account.
	AuthProbeConfirmed401
	// AuthProbeHealthy means the bound account probed healthy while the pane stayed
	// auth-failed — the plumbing-failure signature (a stale proxy token after a
	// daemon restart). Advances the respawn-in-place streak.
	AuthProbeHealthy
)

// healthyStreak tracks a chat's run of consecutive Healthy probes for the
// respawn-in-place path; at records the last increment for the healthyCountTTL reset.
type healthyStreak struct {
	count int
	at    time.Time
}

// respawnWindow bounds respawns per chat to respawnCap per respawnCapWindow.
type respawnWindow struct {
	count       int
	windowStart time.Time
}

// ChatContext is the minimal session context a chat rotation needs.
type ChatContext struct {
	SessionID string
	RepoID    string
	Provider  string // Session.AgentName ("claude", "codex", ...)
	AccountID string // persisted account binding; empty machine account-0 is not probeable
	FromLabel string // human label of the bound (from) account, resolved by the adapter; audited as the rotation's from-side so the TUI renders an email, not a raw id
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
	// RespawnSameAccount marks the BOS-482 respawn-in-place path: the target
	// AccountID equals the currently bound account and the intent is only to refresh
	// the injected auth wiring (stop → same rebind → respawn with resume), not to
	// change accounts. The Switch adapter bypasses the current==target no-op guard
	// and the cross-account resume-feasibility gate for this request.
	RespawnSameAccount bool
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
	// AuthProbe authoritatively classifies the bound account on the auth path:
	// AuthProbeConfirmed401 (a typed 401 — codes.Unauthenticated /
	// agenterr.KindAuthInvalidated — rotate to another account), AuthProbeHealthy
	// (the account is fine but the pane stayed auth-failed — the plumbing-failure
	// signature; respawn in place), or AuthProbeUnknown (unsupported / errored /
	// ambiguous — do nothing, re-probe later). This is what makes the auth path
	// proceed only on a real 401 (no false positives) while still healing a wedge.
	AuthProbe func(ctx context.Context, accountID string) AuthProbeResult
	Decide    func(ctx context.Context, req DecideRequest) (Decision, error)
	Switch    func(ctx context.Context, req SwitchRequest) (SwitchResult, error)
	Now       func() time.Time // nil = time.Now
	// Schedule runs f after d and returns a cancel func: the injectable timer seam
	// for the re-probe scheduler (BOS-482). nil defaults to a time.AfterFunc wrapper;
	// tests inject a controllable fake so the reprobeInterval re-probe is
	// deterministic.
	Schedule func(d time.Duration, f func()) (cancel func())
	// LiveChatStatuses returns the current non-stale live chats keyed by
	// agentSessionID → status (backed by status.Tracker.Snapshot()). nil ⇒ no
	// chats; the proactive sweep is a no-op without this seam. (BOS-318)
	LiveChatStatuses func() map[string]bossanovav1.ChatStatus
	// ProactiveCandidate selects the idlest eligible account to pre-cap-rotate
	// onto, with its probed utilization, WITHOUT cooling the bound account. Kind ==
	// ProactiveNone when no candidate qualifies. (BOS-318)
	ProactiveCandidate func(ctx context.Context, req ProactiveDecideRequest) (ProactiveDecision, error)
	// CaptureProactiveRotation records a successful pre-cap account switch. It
	// is optional so rotation policy tests stay independent of telemetry wiring.
	CaptureProactiveRotation func(ctx context.Context, provider string)
	// CaptureReactiveRotation records a successful reactive account switch.
	CaptureReactiveRotation func(ctx context.Context, provider, reason string)
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
	// healthy tracks the consecutive-Healthy-probe streak per chat for the
	// respawn-in-place path (BOS-482).
	healthy map[string]healthyStreak
	// respawns bounds respawn-in-place attempts per chat (respawnCap per
	// respawnCapWindow); charged before the Switch so a failing respawn still counts.
	respawns map[string]respawnWindow
	// reprobeCancel holds the per-chat cancel func for a scheduled re-probe timer,
	// deduped so a chat has at most one pending re-probe. Cancelled on recovery, a
	// confirmed-401 rotation, a successful respawn, deregistration and shutdown.
	reprobeCancel map[string]func()
	// active counts in-flight rotation goroutines; observed by idleForTest.
	// It is an atomic counter rather than a sync.WaitGroup because idleForTest
	// polls it: a WaitGroup may only be Add-ed from zero once every prior Wait
	// has returned, and a polling observer cannot guarantee that ordering.
	active atomic.Int64
}

// NewChatRotator builds a ChatRotator from its dependency seams.
func NewChatRotator(deps ChatRotatorDeps) *ChatRotator {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Schedule == nil {
		deps.Schedule = func(d time.Duration, f func()) func() {
			t := time.AfterFunc(d, f)
			return func() { t.Stop() }
		}
	}
	return &ChatRotator{
		deps:                 deps,
		inFlight:             map[string]bool{},
		lastAttempt:          map[string]time.Time{},
		proactiveLastAttempt: map[string]time.Time{},
		healthy:              map[string]healthyStreak{},
		respawns:             map[string]respawnWindow{},
		reprobeCancel:        map[string]func(){},
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
		defer r.active.Add(-1)
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
		defer r.active.Add(-1)
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
		SessionID:   cc.SessionID,
		ChatID:      agentSessionID,
		Provider:    cc.Provider,
		Trigger:     triggerUsageLimited,
		FromAccount: cc.FromLabel,
		ResetAt:     resetPtr,
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
		if r.deps.CaptureReactiveRotation != nil {
			r.deps.CaptureReactiveRotation(ctx, cc.Provider, "usage_limit")
		}
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
		r.clearRespawnState(agentSessionID)
		r.deps.Recorder.Record(ctx, AuditEvent{
			SessionID: cc.SessionID, ChatID: agentSessionID, Provider: cc.Provider,
			Trigger:     triggerAuthInvalidated,
			FromAccount: cc.FromLabel,
			Outcome:     "ROTATION_OUTCOME_STATUS_ONLY_DISABLED",
			Detail:      "automatic rotation disabled",
		})
		return
	}
	// Re-check: only act if the pane is STILL auth-failed right now (it may have
	// recovered between the transition and this dispatch — including between two
	// re-probes on the respawn-in-place path).
	if r.deps.CurrentAuthFailed == nil || !r.deps.CurrentAuthFailed(agentSessionID) {
		log.Debug().Msg("auto-rotate(auth): pane no longer auth-failed; aborting")
		r.clearRespawnState(agentSessionID)
		r.deps.Recorder.Record(ctx, AuditEvent{
			SessionID: cc.SessionID, ChatID: agentSessionID, Provider: cc.Provider,
			Trigger:     triggerAuthInvalidated,
			FromAccount: cc.FromLabel,
			Outcome:     "ROTATION_OUTCOME_STATUS_ONLY_RECOVERED",
			Detail:      "pane recovered before rotation",
		})
		return
	}
	if cc.AccountID == "" || r.deps.AuthProbe == nil {
		log.Warn().Msg("auto-rotate(auth): no auth probe available; leaving chat as-is")
		r.recordAuthProbeStatusOnly(ctx, cc, agentSessionID, "auth probe unavailable")
		r.forgetAttempt(agentSessionID)
		return
	}
	switch r.deps.AuthProbe(ctx, cc.AccountID) {
	case AuthProbeHealthy:
		// The pane is auth-failed but the bound account itself probes healthy — the
		// plumbing-failure signature (a stale in-proxy token after a daemon restart, not
		// an invalid credential). Rotating to another account would not help; accumulate
		// consecutive Healthy confirmations and, at the threshold, respawn the chat in
		// place on the SAME account with freshly injected env (BOS-482).
		r.handleHealthyAuthProbe(ctx, cc, agentSessionID)
		return
	case AuthProbeUnknown:
		// The probe could not classify the account (transport error / unsupported /
		// ambiguous): never rotate on the loose trigger and never advance the respawn
		// streak. Re-probe later in case this was a transient blip.
		log.Warn().Str("account_id", cc.AccountID).
			Msg("auto-rotate(auth): probe inconclusive; leaving chat as-is, will re-probe")
		r.recordAuthProbeStatusOnly(ctx, cc, agentSessionID, "auth probe did not confirm invalidation")
		r.forgetAttempt(agentSessionID)
		r.scheduleReprobe(agentSessionID)
		return
	case AuthProbeConfirmed401:
		// A real typed 401 on the bound account: the credential is genuinely invalid.
		// Supersede any healthy streak / pending re-probe and take the rotate path below.
		r.clearRespawnState(agentSessionID)
	}

	auditBase := AuditEvent{
		SessionID:   cc.SessionID,
		ChatID:      agentSessionID,
		Provider:    cc.Provider,
		Trigger:     triggerAuthInvalidated,
		FromAccount: cc.FromLabel,
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
		if r.deps.CaptureReactiveRotation != nil {
			r.deps.CaptureReactiveRotation(ctx, cc.Provider, "error")
		}
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
			Trigger: triggerUsageLimited, FromAccount: cc.FromLabel, ToAccount: dec.Label,
			Outcome: "ROTATION_OUTCOME_FAILED", Detail: "proactive pre-cap switch did not complete",
		}
		r.deps.Recorder.Record(ctx, failed)
		return false
	}
	log.Info().Str("account", res.SwitchedToLabel).Bool("fresh", res.Fresh).
		Msg("proactive-sweep: rotated idle chat off soon-to-cap account")
	if r.deps.CaptureProactiveRotation != nil {
		r.deps.CaptureProactiveRotation(ctx, cc.Provider)
	}
	rotated := AuditEvent{
		SessionID: cc.SessionID, ChatID: agentSessionID, Provider: cc.Provider,
		Trigger: triggerUsageLimited, FromAccount: cc.FromLabel, ToAccount: res.SwitchedToLabel,
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
		FromAccount: cc.FromLabel,
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
		FromAccount: cc.FromLabel,
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

// handleHealthyAuthProbe advances the consecutive-Healthy streak for a pane that is
// auth-failed while its bound account probes healthy. Below the threshold it records a
// status-only audit and schedules a re-probe; at the threshold it respawns in place on
// the SAME account. (BOS-482)
func (r *ChatRotator) handleHealthyAuthProbe(ctx context.Context, cc ChatContext, agentSessionID string) {
	now := r.deps.Now()
	r.mu.Lock()
	streak := r.healthy[agentSessionID]
	if streak.count == 0 || now.Sub(streak.at) >= healthyCountTTL {
		streak = healthyStreak{count: 1, at: now}
	} else {
		streak.count++
		streak.at = now
	}
	r.healthy[agentSessionID] = streak
	count := streak.count
	r.mu.Unlock()

	log := r.deps.Logger.With().Str("agent_session_id", agentSessionID).Logger()
	if count < healthyRespawnThreshold {
		log.Info().Int("healthy_streak", count).
			Msg("auto-rotate(auth): account healthy but pane auth-failed; awaiting confirmation before respawn")
		// Match the pre-BOS-482 healthy branch: record status-only but do NOT forget the
		// attempt (the re-probe timer, not the reactive rate limiter, drives the retry).
		r.recordAuthProbeStatusOnly(ctx, cc, agentSessionID, "auth probe did not confirm invalidation")
		r.scheduleReprobe(agentSessionID)
		return
	}
	r.respawnInPlace(ctx, cc, agentSessionID)
}

// respawnInPlace stops → same-account rebinds → respawns the chat to refresh its
// injected auth wiring, charging the per-chat respawn cap BEFORE the Switch so a
// failing respawn still counts. A cap-exhausted attempt records
// ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED and goes quiet; an ErrSwitchAborted refusal
// is fail-safe (leave as-is, re-probe, no FAILED audit); any other Switch error is a
// FAILED audit. Success records ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT and clears the
// streak + pending re-probe. (BOS-482)
func (r *ChatRotator) respawnInPlace(ctx context.Context, cc ChatContext, agentSessionID string) {
	log := r.deps.Logger.With().Str("agent_session_id", agentSessionID).Logger()
	auditBase := AuditEvent{
		SessionID:   cc.SessionID,
		ChatID:      agentSessionID,
		Provider:    cc.Provider,
		Trigger:     triggerAuthInvalidated,
		FromAccount: cc.FromLabel,
		ToAccount:   cc.FromLabel, // same account
	}
	if !r.chargeRespawn(agentSessionID) {
		log.Warn().Msg("auto-rotate(auth): respawn-in-place cap reached; leaving chat as-is")
		capped := auditBase
		capped.Outcome = outcomeRespawnCapExhausted
		capped.Detail = "respawn-in-place cap reached — re-authenticate or restart the daemon"
		r.deps.Recorder.Record(ctx, capped)
		// Go quiet: cancel the re-probe and drop the streak. A fresh auth-failed edge
		// (or a later window) reopens the path.
		r.clearRespawnState(agentSessionID)
		return
	}
	res, err := r.deps.Switch(ctx, SwitchRequest{
		SessionID:          cc.SessionID,
		AgentSessionID:     agentSessionID,
		AccountID:          cc.AccountID,
		Auto:               true,
		RespawnSameAccount: true,
	})
	if err != nil {
		if errors.Is(err, ErrSwitchAborted) {
			// Fail-safe: the chat recovered to WORKING / was mid-turn, so the pane was
			// deliberately not interrupted. NOT a failure — leave as-is, keep the charged
			// attempt, and re-probe later (the streak is preserved so the next re-probe
			// retries immediately once the chat is idle again).
			log.Info().Msg("auto-rotate(auth): respawn aborted (chat mid-turn); leaving chat as-is, will re-probe")
			r.scheduleReprobe(agentSessionID)
			return
		}
		log.Warn().Err(err).Msg("auto-rotate(auth): respawn-in-place did not complete; leaving chat as-is")
		failed := auditBase
		failed.Outcome = "ROTATION_OUTCOME_FAILED"
		failed.Detail = "respawn-in-place did not complete"
		r.deps.Recorder.Record(ctx, failed)
		r.scheduleReprobe(agentSessionID)
		return
	}
	log.Info().Bool("fresh", res.Fresh).Msg("auto-rotate(auth): respawned chat in place on same account to refresh auth wiring")
	done := auditBase
	done.Outcome = outcomeRespawnedSameAccount
	if res.Fresh {
		done.Detail = "respawned in place to refresh auth wiring — started fresh"
	} else {
		done.Detail = "respawned in place to refresh auth wiring — resumed"
	}
	r.deps.Recorder.Record(ctx, done)
	// Success: drop the streak and cancel any pending re-probe. If the respawn did not
	// actually clear the wedge, the pane flips auth-failed again and the edge-triggered
	// OnAuthFailed starts a fresh cycle (still bounded by the respawn cap window).
	r.clearRespawnState(agentSessionID)
}

// chargeRespawn records one respawn-in-place attempt against the per-chat window and
// reports whether it is within the cap. Charged before the Switch so a failed respawn
// still counts (a pane that can never respawn cannot loop). Returns false when the cap
// is already reached. (BOS-482)
func (r *ChatRotator) chargeRespawn(agentSessionID string) bool {
	now := r.deps.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.respawns[agentSessionID]
	if w.count == 0 || now.Sub(w.windowStart) >= respawnCapWindow {
		w = respawnWindow{count: 0, windowStart: now}
	}
	if w.count >= respawnCap {
		return false
	}
	w.count++
	r.respawns[agentSessionID] = w
	return true
}

// scheduleReprobe arms (or re-arms) the single per-chat re-probe timer. Because
// SetAuthFailed is edge-triggered, a wedged pane dispatches rotateAuth only once; this
// timer re-drives the auth path after reprobeInterval so consecutive Healthy probes can
// accumulate to the respawn threshold. Deduped: an existing pending re-probe is
// cancelled first so a chat has at most one armed timer. (BOS-482)
func (r *ChatRotator) scheduleReprobe(agentSessionID string) {
	// Create the timer outside the lock (Schedule may take its own lock / start a
	// goroutine), then swap it in under the lock, cancelling any prior pending timer.
	cancel := r.deps.Schedule(reprobeInterval, func() { r.reprobeAuth(agentSessionID) })
	r.mu.Lock()
	if old := r.reprobeCancel[agentSessionID]; old != nil {
		old()
	}
	r.reprobeCancel[agentSessionID] = cancel
	r.mu.Unlock()
}

// reprobeAuth is the timer-driven re-entry into the auth path. Unlike OnAuthFailed it
// does NOT charge the reactive rate limiter (re-probing on the reprobeInterval cadence
// is the whole point); it takes only the inFlight guard so it can never run
// concurrently with a reactive rotation for the same chat. If a rotation is already in
// flight it silently drops — the next scheduled re-probe covers the gap. (BOS-482)
func (r *ChatRotator) reprobeAuth(agentSessionID string) {
	r.mu.Lock()
	if r.inFlight[agentSessionID] {
		r.mu.Unlock()
		return
	}
	r.inFlight[agentSessionID] = true
	r.mu.Unlock()
	r.active.Add(1)
	defer r.active.Add(-1)
	defer r.releaseInFlight(agentSessionID)
	r.rotateAuth(agentSessionID)
}

// clearRespawnState drops the consecutive-Healthy streak and cancels any pending
// re-probe timer for a chat. Called on recovery, on a confirmed-401 rotation, on a
// successful respawn, on a cap-exhausted stop, and on deregistration. The respawn cap
// window is deliberately NOT cleared here so the per-hour cap survives across streaks
// (a pane that respawns twice then recovers-and-re-wedges cannot escape the cap by
// bouncing). (BOS-482)
func (r *ChatRotator) clearRespawnState(agentSessionID string) {
	r.mu.Lock()
	delete(r.healthy, agentSessionID)
	cancel := r.reprobeCancel[agentSessionID]
	delete(r.reprobeCancel, agentSessionID)
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Deregister tears down all in-memory respawn state for a chat that is going away
// (pane removed / daemon draining): it cancels any pending re-probe and drops the
// streak AND the respawn-cap window. Safe to call for an unknown chat. (BOS-482)
func (r *ChatRotator) Deregister(agentSessionID string) {
	r.mu.Lock()
	delete(r.healthy, agentSessionID)
	delete(r.respawns, agentSessionID)
	cancel := r.reprobeCancel[agentSessionID]
	delete(r.reprobeCancel, agentSessionID)
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// idleForTest reports whether no rotation goroutine is active. Test-only.
//
// This is a non-blocking load: callers (waitIdle) poll it. An earlier version
// raced by spawning a goroutine that blocked in sync.WaitGroup.Wait and leaking
// it on timeout — a subsequent OnChatStatus Add(1) from zero then overlapped the
// still-running Wait, which the race detector correctly flagged.
func (r *ChatRotator) idleForTest() bool {
	return r.active.Load() == 0
}
