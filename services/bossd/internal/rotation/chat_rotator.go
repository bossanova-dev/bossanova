package rotation

import (
	"context"
	"errors"
	"strings"
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
	// outcomeRespawnCapExhausted has a SECOND producer since BOS-981: it also parks a
	// chat whose respawn was repeatedly refused before the pane was touched, once the
	// separate not-attempt retry budget is spent. Its proto comment says "cap hit for
	// this chat/window"; read that as "cap or retry budget exhausted for this chat's
	// respawn-in-place healer" — the audit Detail disambiguates which one, and no
	// other enum value fits (FAILED is wrong because nothing was attempted, EXHAUSTED
	// means cooling, NO_ELIGIBLE_ACCOUNT means the account list is empty of targets).
	outcomeRespawnCapExhausted = "ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED"
	// outcomeRespawnNotAttempted records a respawn the Switch refused before the pane
	// was touched, for a reason that is NOT the bound account being ineligible (an
	// ineligible account rotates instead, see respawnInPlace). It is deliberately NOT
	// ROTATION_OUTCOME_FAILED: nothing was attempted, so nothing failed — and the cap
	// charge is refunded. Like ROTATION_OUTCOME_STATUS_ONLY_RECOVERED and
	// ROTATION_OUTCOME_STATUS_ONLY_PROBE_UNCONFIRMED it is a rotator-internal free
	// string with no bossanova.v1 RotationOutcome enum value, so it needs no API
	// version bump; consumers render it through their documented default arm, which
	// surfaces the Detail — and the Detail names the actual cause. (BOS-981)
	outcomeRespawnNotAttempted = "ROTATION_OUTCOME_RESPAWN_NOT_ATTEMPTED"
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

// Auth-episode latch constants (BOS-980). The healer needs two Healthy probes
// reprobeInterval apart, but the pane it is healing flaps: Claude Code's auth retry
// countdown redraws the login banner out of statusdetect's 20-line tail for a tick, so
// a single poll reads clean. status.Tracker deletes its auth-failed marker on that one
// clean poll — deliberately, and NOT something to change here (see the BOS-808 note on
// status/tracker.go) — and rebuilding the marker then costs another full rise-debounce.
// A re-probe landing anywhere in that trough used to read "recovered" and destroy the
// episode. The rotator therefore latches the episode on its own side: a clean reading is
// held, not believed, until the pane has been SUSTAINEDLY clear.
//
// Scope of the latch, stated for reviewers who may read it as the sticky blocked-reason
// BOS-808 removed: it lives entirely in ChatRotator's in-memory respawn state.
// status.Tracker.SetAuthFailed is untouched, Tracker.AuthFailed still falls instantly,
// and the agent-auth-failed overlay users see still clears on the first clean poll. The
// only thing held is this healer's private opinion of whether its episode is over.
//
// The window is derived from the two cadences that produce the trough, not picked round.
const (
	// authPollInterval mirrors status.PollInterval, the tmux pane poll cadence.
	// Duplicated rather than imported: status imports db which imports rotation, so
	// rotation must not import status (module-boundary convention: duplicate small
	// constants). TestAuthLatchConstantsMirrorTheTracker pins the two together.
	authPollInterval = 3 * time.Second
	// authRisePolls mirrors status.AuthFailedConsecutivePollsRequired: the number of
	// consecutive login-required polls needed to re-establish a cleared marker.
	authRisePolls = 2
	// authRetryCycle is the period of Claude Code's auth retry countdown
	// ("… · Retrying in 15s · attempt N/10"), the redraw that produces the clean poll
	// in the first place. It is also status.StaleThreshold, so a window shorter than
	// this cannot outlast one retry cycle and the latch would not hold at all.
	authRetryCycle = 15 * time.Second
	// authClearGraceWindow is how long a clean pane is held before its auth-failed
	// episode is believed over: one full retry cycle, the cost of re-establishing the
	// rise-debounced marker, and one further poll of slack (15s + 6s + 3s = 24s).
	// Shorter and a flapping pane escapes the latch through the trough it creates every
	// cycle; much longer and a pane that genuinely recovered keeps a pointless timer
	// armed.
	//
	// The slack poll is load-bearing, not padding. Without it the window equals the
	// worst-case re-assertion time exactly, and the two race: a second trough that
	// swallows the poll at clearedAt+authRetryCycle pushes the two rise polls out to
	// clearedAt+18s and clearedAt+21s, so a 21s window can expire in the same instant
	// the marker comes back and clear the streak as RECOVERED — reproducing the reset
	// this latch exists to prevent, and jitter in either cadence decides the winner.
	// Strictly longer, and a pane still wedged at the end of the window has necessarily
	// re-asserted its marker first. Troughs are one retry cycle apart, so no third one
	// can land inside the window to restart the debounce.
	authClearGraceWindow = authRetryCycle + authPollInterval*(authRisePolls+1)
)

// ErrSwitchAborted is the rotation-owned sentinel the Switch adapter returns (in
// place of session.ErrChatMidTurn, which the rotation package cannot import) when a
// switch was refused fail-safe: the chat recovered to WORKING / was mid-turn, so the
// pane was deliberately not interrupted. On the respawn-in-place path this is NOT a
// failure — the rotator leaves the chat as-is — so it is never recorded as
// ROTATION_OUTCOME_FAILED. (BOS-482)
//
// What happens to the respawn-cap charge is LANE-DEPENDENT (BOS-982), because the
// charge is what terminates one lane and not the other:
//   - authRespawnLane KEEPS the charge and re-arms its re-probe timer. That lane
//     re-enters itself on a timer that charges no rate limiter, so the cap is the only
//     thing that ever stops it.
//   - proxyTokenRespawnLane REFUNDS it (refundRespawn), because Switch declined before
//     touching the pane and that lane's re-entry is bounded by ChatRotateMinInterval
//     instead of by the cap.
//
// The switch is respawnLane.refundAbortedCharge; read its doc before changing either
// lane's answer.
var ErrSwitchAborted = errors.New("rotation: switch aborted (chat mid-turn); left as-is")

// ErrSwitchNotAttempted is the rotation-owned sentinel the Switch adapter returns
// (in place of session.ErrSwitchNotAttempted, which the rotation package cannot
// import) when the switch was refused BEFORE it touched the chat's pane. Nothing was
// consumed, so the respawn-in-place path refunds the cap charge it took before the
// call rather than spending a life on an attempt that never happened. It is a
// structural marker, not a cause — it is always wrapped around the underlying error,
// and ErrSwitchAborted is checked first so the mid-turn refusal keeps its own
// fail-safe branch (charge kept, streak preserved, re-probe armed). (BOS-981)
var ErrSwitchNotAttempted = errors.New("rotation: switch not attempted (pane untouched)")

// ErrSwitchAccountIneligible accompanies ErrSwitchNotAttempted when the refusal was
// the target account itself being ineligible — disabled, health-failed, or cooling.
// On the respawn-in-place path the "target" IS the bound account, so this says the
// chat's own account can never host a respawn: retrying it is guaranteed to be
// refused again, and the remedy is a real rotation to an eligible account (the same
// operation `boss account switch` performs manually). (BOS-981)
var ErrSwitchAccountIneligible = errors.New("rotation: target account is not eligible")

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
	// episodeSince is the tracker's anchor for the episode as of the last confirmation
	// (status.Tracker.AuthFailedSince). It is carried for observability, NOT as the
	// streak's identity: within one wedge the anchor re-pins every time the pane flaps
	// clean for a poll, because the tracker deletes its marker on that poll and the next
	// rise-debounce pins a fresh effectiveAt. Logging it is what makes that flap visible
	// in the daemon log; keying the streak on it would reset the streak on exactly the
	// panes the latch exists to protect. The streak entry itself is the episode identity:
	// it is created by the first confirmation and destroyed by clearRespawnState when the
	// episode is believed over. Zero when the seam is absent. (BOS-980)
	episodeSince time.Time
	// clearedAt is the rotator clock reading of the FIRST re-probe that found the pane
	// clean while this streak was open; zero whenever the most recent observation still
	// saw the episode. It is what authClearGraceWindow is measured from. (BOS-980)
	clearedAt time.Time
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
	// SuppressHealthFail asks the engine to pick a rotation target for an
	// AuthInvalidated request WITHOUT recording a permanent health failure on
	// AccountID. Set by the respawn healer, which rotates off an account that is
	// merely ineligible to host a respawn rather than one whose auth is broken.
	// See rotation.Signal.SuppressHealthFail. (BOS-981)
	SuppressHealthFail bool
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

// ProactiveDecideRequest asks the engine adapter for the banded consume-first
// switch target — soonest in-band weekly reset, else the idlest (BOS-830) — for
// a bound account that is nearing (not at) its cap. (BOS-318)
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
//
// It can now suppress a rotation the pre-BOS-830 rule would have made, and that
// is deliberate. The gate was written when Engine.SelectProactiveCandidate
// returned the LOWEST-utilization candidate — the one most likely to clear it.
// A banded consume-first winner is instead bounded only by
// rotation.MinRotationHeadroom, so a bound account just over
// proactiveUtilTrigger paired with an in-band winner just inside the band can
// fail this gate and the sweep skips, where the old rule would have picked an
// idler account and switched. Exempting in-band picks from the gate is a
// proactive-rotation policy change beyond BOS-830's acceptance criteria, so it
// is left to a follow-up. The exposure is small: proactive rotation is off
// unless ProactiveRotationEnabled, and it cannot thrash, because a switch
// requires the new bound account to sit below proactiveUtilTrigger.
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
	// AuthFailedSince reports the start of the pane's current auth-failed episode
	// (status.Tracker.AuthFailedSince), plus whether one is in effect. The BOS-980 latch
	// records it on the healthy streak and logs it, so a daemon log shows when the anchor
	// re-pinned mid-episode — the visible fingerprint of the pane flapping clean for a
	// poll. It is deliberately not the latch's episode identity (see
	// healthyStreak.episodeSince). Optional; nil only drops the anchor from the logs.
	AuthFailedSince func(agentSessionID string) (time.Time, bool)
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
	// ProactiveCandidate selects the banded consume-first account to pre-cap-rotate
	// onto — soonest in-band weekly reset, else the idlest; see
	// Engine.SelectProactiveCandidate (BOS-830) — with its probed utilization,
	// WITHOUT cooling the bound account. Note SweepProactive still applies its own
	// proactiveHysteresis gate on top, so what it switches to is always materially
	// idler than the bound account even though what this returns may not be. Kind ==
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
	// OnAuthDecisionComplete runs after an auth dispatch finishes its
	// auth probe/decision path. Stream publishers use it to republish after the
	// audit row that corroborates AGENT_AUTH_FAILED has been recorded.
	OnAuthDecisionComplete func(agentSessionID string)
}

// chatState is one chat's rotation state across every lane, guarded by
// ChatRotator.mu. Absence of a chats entry and an all-zero chatState are
// deliberately the same thing — see isZero and gcChatLocked — so a lane reads its
// field without caring which of the two it is looking at.
type chatState struct {
	// inFlight marks a rotation running for the chat.
	inFlight bool
	// lastAttempt is the chat's last reactive rotation attempt (rate limit).
	lastAttempt time.Time
	// proactiveLastAttempt rate-limits the proactive pre-cap sweep per chat,
	// keyed by ProactiveSweepInterval. It is deliberately separate from
	// lastAttempt so proactive cadence is not entangled with the reactive
	// ChatRotateMinInterval and neither path can suppress the other. (BOS-318)
	proactiveLastAttempt time.Time
	// healthy tracks the consecutive-Healthy-probe streak for the
	// respawn-in-place path (BOS-482).
	healthy healthyStreak
	// respawns bounds respawn-in-place attempts for the chat (respawnCap per
	// respawnCapWindow); charged before the Switch so a failing respawn still counts,
	// and refunded — on the lanes that opted in — for the one outcome where Switch
	// declined before touching the pane at all (ErrSwitchAborted — see refundRespawn
	// and respawnLane.refundAbortedCharge).
	respawns respawnWindow
	// notAttempts bounds the RETRIES of a respawn whose Switch was refused before it
	// touched the pane for a reason with no account-side remedy. Those refusals refund
	// their respawn charge (BOS-981), which takes the respawn cap out of play as the
	// stopping condition, so they need a stopping condition of their own. Same shape and
	// same window as respawns; deliberately separate so a chat that alternates between a
	// real respawn and a refusal cannot escape either bound. Only a lane that opted into
	// the not-attempted machinery ever charges it (respawnLane.handlesNotAttempted).
	notAttempts respawnWindow
	// proxyRepairPending records a BOS-982 proxy-token repair signal that arrived
	// while a rotation was already in flight for the chat. The two lanes share one
	// reservation so they can never respawn a pane twice, but the auth lane can
	// finish without applying any remedy (a below-threshold healthy probe, an
	// inconclusive probe, no eligible account) — and simply dropping the proxy
	// signal there would strand the pane for a whole ChatRotateMinInterval, which
	// is the stall BOS-982 exists to remove. Set under the same lock that observed
	// the in-flight rotation, so the winning lane cannot finish in between and lose
	// the wakeup; drained by releaseInFlight.
	proxyRepairPending bool
	// proxyRepairSettled records that the reservation currently held for a chat has
	// already settled the question a queued proxy-token repair would ask, so that
	// repair is dropped rather than drained on release. Two things settle it: a
	// pane-level remedy was applied (a respawn-in-place attempt, or a completed
	// rotation to another account) — dispatching behind that is the second respawn
	// the shared reservation exists to prevent — or the repair itself reached a
	// terminal no-op decision that will read the same on every retry (the repo opted
	// out of automatic rotation; the chat has no bound account). Draining into a
	// standing decision only re-derives it, once per pane retry, forever.
	proxyRepairSettled bool
	// reprobeCancel holds the cancel func for a scheduled re-probe timer, deduped
	// so a chat has at most one pending re-probe. Cancelled on recovery, a
	// confirmed-401 rotation, a successful respawn, deregistration and shutdown.
	reprobeCancel func()
}

// isZero reports whether every lane has nothing recorded for the chat, i.e. the
// entry carries exactly what an absent entry would.
//
// It is written field by field because it HAS to be: chatState holds a func()
// (reprobeCancel), which makes the struct non-comparable, so `*s == chatState{}`
// does not compile and no whole-struct comparison is available to fall back on.
//
// The compiler will NOT catch a field added to chatState later. Go never errors on
// a struct field nothing reads, and .golangci.yml enables no field-coverage linter
// (exhaustive covers switches, not struct literals), so a new field simply drops out
// of this predicate in silence. The guards are instead
// TestChatStateFieldCountIsGuarded, which fails the moment a field is added, and
// TestChatStateIsZeroSeesEveryField, which fails until this predicate actually reads
// the new field — so bumping the count constant alone does not buy a green suite.
//
// Both directions of a missed field are real, and the more dangerous one is not the
// leak. A field this predicate ignores reads as zero, so gcChatLocked reclaims an
// entry that still holds LIVE state for that lane — a premature reclaim, which
// silently discards a pending re-probe, an in-flight marker or a rate-limit stamp.
// The other direction — a field wrongly kept out of the reset paths — leaks entries
// for chats no lane is tracking. Extending isZero is what rules out the first;
// extending it is also what keeps the second bounded.
func (s *chatState) isZero() bool {
	return !s.inFlight &&
		s.lastAttempt.IsZero() &&
		s.proactiveLastAttempt.IsZero() &&
		s.healthy.count == 0 &&
		s.healthy.at.IsZero() &&
		s.healthy.episodeSince.IsZero() &&
		s.healthy.clearedAt.IsZero() &&
		s.respawns.count == 0 &&
		s.respawns.windowStart.IsZero() &&
		s.notAttempts.count == 0 &&
		s.notAttempts.windowStart.IsZero() &&
		!s.proxyRepairPending &&
		!s.proxyRepairSettled &&
		s.reprobeCancel == nil
}

// ChatRotator auto-rotates interactive chats on CHAT_STATUS_LIMITED transitions
// (Epic 4.3, D4). Trigger + policy glue only: pane lifecycle stays owned by the
// BOS-171 switch primitive / chat tracker (BOS-153). Fail-safe: any error ⇒ no
// rotation, the chat stays LIMITED.
type ChatRotator struct {
	deps ChatRotatorDeps

	mu sync.Mutex
	// chats holds every lane's per-chat state in ONE entry keyed by
	// agentSessionID, replacing the parallel per-lane maps this used to be. Each
	// lane owns its own field, so a chat's lifecycle is kept consistent in one
	// place rather than in eight independently-mutated maps where a missed
	// reclaim in one is invisible in the others.
	//
	// The entry is reclaimed by gcChatLocked the moment every field is back at
	// its zero value; every lane's zero value means exactly "nothing recorded for
	// this chat", which is what makes reclaiming safe. A lane that is finished
	// therefore zeroes ITS OWN field and calls gcChatLocked — it must never
	// delete the entry outright, which would destroy the other lanes' state.
	chats map[string]*chatState
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
		deps:  deps,
		chats: map[string]*chatState{},
	}
}

// chatLocked returns the chat's state, creating a zero entry when none exists.
// Callers must hold r.mu AND be about to write: a get-or-create on a pure read
// would materialise entries for chats no lane is tracking, which is the unbounded
// growth gcChatLocked exists to prevent. Use lookupChatLocked to read.
func (r *ChatRotator) chatLocked(agentSessionID string) *chatState {
	cs := r.chats[agentSessionID]
	if cs == nil {
		cs = &chatState{}
		r.chats[agentSessionID] = cs
	}
	return cs
}

// lookupChatLocked returns the chat's state, or nil when nothing is recorded for
// it. Callers must hold r.mu. A nil result reads identically to an all-zero
// chatState, so a reader may treat the two the same.
func (r *ChatRotator) lookupChatLocked(agentSessionID string) *chatState {
	return r.chats[agentSessionID]
}

// gcChatLocked reclaims the chat's entry once every lane has zeroed its field.
// Callers must hold r.mu. Every lane that finishes zeroes its own field and then
// calls this, which is what keeps chats bounded to the chats some lane is
// actually tracking — the promise the per-lane eviction comments in reserve and
// proactiveSuppressed make. Deleting the entry outright instead would destroy
// the other lanes' state for the chat.
func (r *ChatRotator) gcChatLocked(agentSessionID string) {
	if cs := r.chats[agentSessionID]; cs != nil && cs.isZero() {
		delete(r.chats, agentSessionID)
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
		// An auth-failed edge means the episode is live RIGHT NOW, so any grace window
		// opened by an earlier clean reading is stale. Clear it before re-arming, or the
		// re-probe this schedules replaces the short grace timer while leaving the old
		// clearedAt stamp behind — and the next clean reading, measured against that
		// stale stamp, escapes the latch on the very timeline BOS-980 targets. (BOS-980)
		r.markEpisodeLive(agentSessionID)
		r.scheduleReprobe(agentSessionID)
		return
	}
	r.active.Add(1)
	safego.Go(r.deps.Logger, func() {
		defer r.active.Add(-1)
		defer r.releaseInFlight(agentSessionID)
		defer r.finishAuthDecision(agentSessionID)
		r.rotateAuth(agentSessionID)
	})
}

// OnProxyTokenUnresolved is the failover proxy's DIRECT route from "I minted
// this 401" to "repair this pane" (BOS-982). It is called only when the proxy
// itself rejected a request because it could not resolve the pane's path token,
// and only after the caller has attributed that token to this chat's LIVE pane.
//
// It deliberately does NOT probe the account, and that omission is the whole
// point of this entry. The pre-BOS-982 route was indirect and self-defeating:
// the proxy's own 401 rendered in the pane, statusdetect scraped the resulting
// login banner, the pane was flagged auth-failed, and rotateAuth probed the
// bound account — which came back Healthy every time, because the credential
// was never the problem. Two Healthy probes five minutes apart were then needed
// to reach respawnInPlace, the remedy that was correct from the first instant.
// Here the daemon does not have to infer the cause from a banner: the component
// that minted the 401 is the caller, so an account-invalidation diagnosis the
// evidence can never satisfy is skipped and the pane is repaired directly.
//
// Because this bypasses the probe it must stay narrow. It is reachable ONLY
// from the proxy's unknown-token branch — an UPSTREAM 401 is a real credential
// signal and keeps taking OnAuthFailed, probe included.
//
// Like OnAuthFailed it must return fast (the caller writes a 401 before it
// dispatches this), and it shares the SAME per-chat reservation, so a
// near-simultaneous proxy signal and pane-scrape signal for one chat produce
// exactly one respawn attempt rather than two. "Exactly one" is two properties,
// not one: the reservation stops the second dispatch, and the queue below stops
// the loser from being dropped when the winner finishes without touching the
// pane at all — which for the auth lane is the ordinary outcome, not an edge.
func (r *ChatRotator) OnProxyTokenUnresolved(agentSessionID string) {
	if agentSessionID == "" {
		return
	}
	ok, stamp := r.reserve(agentSessionID, true)
	if !ok {
		// Either a rotation is already running for this chat — in which case the line
		// above has queued this signal, so that rotation drains it on the way out
		// unless it acted on the pane itself — or the chat rotated inside the
		// rate-limit window, where a second attempt is exactly what the limiter is
		// for. Nothing further either way: unlike the auth-failed edge there is no
		// scraped marker whose staleness needs clearing, and re-arming a re-probe
		// would re-enter the probing lane this entry exists to avoid.
		return
	}
	r.active.Add(1)
	safego.Go(r.deps.Logger, func() {
		defer r.active.Add(-1)
		defer r.releaseInFlight(agentSessionID)
		defer r.finishAuthDecision(agentSessionID)
		r.repairProxyPane(agentSessionID, &stamp)
	})
}

// repairProxyPane runs the probe-skipping repair dispatched by
// OnProxyTokenUnresolved: load config, load the chat context, honour the
// automatic-rotation opt-out, then respawn in place on the currently bound
// account. Fail-safe throughout — any error leaves the pane exactly as it is,
// where the existing pane-scrape lane remains the (slower) backstop. (BOS-982)
func (r *ChatRotator) repairProxyPane(agentSessionID string, stamp *attemptStamp) {
	ctx, cancel := context.WithTimeout(context.Background(), rotateTimeout)
	defer cancel()
	log := r.deps.Logger.With().Str("agent_session_id", agentSessionID).Logger()

	// releaseLimiter gives back the chat's rate-limit slot so the pane's NEXT
	// unknown-token 401 — the proxy keeps minting them while the pane stays
	// wedged — re-enters this lane immediately instead of being refused for a
	// whole ChatRotateMinInterval.
	//
	// It is a no-op unless THIS entry charged the limiter (stamp == nil on the
	// drain path). dispatchProxyRepair deliberately does not charge it, so there
	// the stamp in lastAttempt belongs to the auth/limit lane that won the
	// reservation; touching it would silently un-charge that lane and let the
	// chat's next CHAT_STATUS_LIMITED or auth edge rotate immediately. The drained
	// variant therefore leaves it alone and falls back on the slower status-scrape
	// lane, mirroring the charge asymmetry dispatchProxyRepair already establishes.
	//
	// Where it does fire it RESTORES rather than deletes. lastAttempt is one
	// shared window across every lane, so an unconditional delete would un-suppress
	// the auth and usage-limit lanes as well — see releaseAttempt. (BOS-982)
	releaseLimiter := func() {
		if stamp != nil {
			r.releaseAttempt(agentSessionID, *stamp)
		}
	}

	// As in rotate/rotateAuth, these two pre-context failures record NO audit
	// row: the chat context is not loaded, so a meaningful AuditEvent cannot be
	// built. Unlike the two terminal decisions below they are NOT settled: a config
	// read or a chat-context lookup that failed once is expected to succeed on the
	// next pass, and suppressing the drain would strand a pane over a transient
	// error. Their retry cadence is the pane's own 401 rate, which the proxy's
	// passthrough-warn window already throttles.
	cfg, err := r.deps.LoadConfig()
	if err != nil {
		log.Warn().Err(err).Msg("proxy-token repair: config load failed; leaving pane as-is")
		releaseLimiter()
		return
	}
	cc, err := r.deps.ChatContext(ctx, agentSessionID)
	if err != nil {
		log.Warn().Err(err).Msg("proxy-token repair: chat context lookup failed; leaving pane as-is")
		releaseLimiter()
		return
	}
	if !cfg.ManagedAccountsEnabled() || !cfg.AutoRotateChatsEnabled(cc.RepoID) {
		// An operator who turned automatic rotation off for this repo did not ask
		// the daemon to restart their pane either, however well-diagnosed the cause.
		log.Debug().Str("repo_id", cc.RepoID).Msg("proxy-token repair: opted out; leaving pane as-is")
		// Terminal, and stable: the operator's opt-out will read the same on the next
		// pass. Settle the reservation so a 401 that arrived mid-run is dropped on
		// release instead of drained into a re-derivation of this same decision. The
		// drain path charges no rate limit (dispatchProxyRepair says why), so nothing
		// else bounds that loop: a wedged pane in an opted-out repo would emit one
		// LoadConfig + ChatContext + STATUS_ONLY_DISABLED row per retry, forever. The
		// limiter slot this entry may hold is deliberately NOT released either. (BOS-982)
		r.settleProxyRepair(agentSessionID)
		r.deps.Recorder.Record(ctx, AuditEvent{
			SessionID: cc.SessionID, ChatID: agentSessionID, Provider: cc.Provider,
			Trigger:     triggerAuthInvalidated,
			FromAccount: cc.FromLabel,
			Outcome:     "ROTATION_OUTCOME_STATUS_ONLY_DISABLED",
			Detail:      proxyRepairDetailPrefix + "automatic rotation disabled",
		})
		return
	}
	if cc.AccountID == "" {
		// respawnInPlace rebinds to cc.AccountID; with nothing bound there is no
		// same-account respawn to perform. Terminal for the same reason as the opt-out
		// above — an unbound chat does not acquire an account because the proxy 401'd
		// again — so settle rather than drain, and hold the rate-limit slot rather than
		// releasing it into a retry that can only reach this same line. (BOS-982)
		log.Warn().Msg("proxy-token repair: chat has no bound account; leaving pane as-is")
		r.settleProxyRepair(agentSessionID)
		return
	}
	log.Info().Msg("proxy-token repair: failover proxy could not resolve this pane's token; respawning in place without an account probe")
	// Only a genuine Switch FAILURE reopens the lane early. The two
	// pane-untouched outcomes are deliberately not the same story:
	//
	//   respawnIncomplete — the Switch was attempted and errored. Whatever broke
	//     (a tmux hiccup, a transient stop failure) may well not break again, so
	//     the pane's next unresolved-token 401 is worth letting straight back in;
	//     reserve()'s rate-limit branch would otherwise refuse it for a whole
	//     ChatRotateMinInterval and queue nothing, and this lane arms no re-probe
	//     to cover that window.
	//
	//   respawnAborted — the chat was mid-turn, so Switch declined to interrupt
	//     it. A mid-turn pane STAYS mid-turn and the proxy re-mints an
	//     unresolved-token 401 within seconds, so releasing the slot here would put
	//     the retry on the pane's 401 cadence: a LoadConfig + ChatContext + Switch
	//     round trip every few seconds, all of them ending in the same abort.
	//     Holding the slot makes the mid-turn pane wait out ChatRotateMinInterval,
	//     which is exactly the throttle the limiter exists to provide, and the
	//     lane's retryNote still holds: the 401 after that window re-enters here.
	//     The respawn cap is not doing any of that work — respawnInPlace refunds an
	//     aborted attempt's charge (refundRespawn), so a pane that only ever aborts
	//     still has its full budget the moment it goes idle. (BOS-982)
	switch r.respawnInPlace(ctx, cc, agentSessionID, proxyTokenRespawnLane) {
	case respawnIncomplete:
		releaseLimiter()
	case respawnAborted, respawnCapped, respawnCompleted:
		// Hold the slot: a mid-turn pane needs the throttle, a capped chat can only
		// meet the same cap, and a completed respawn needs no retry at all.
	}
}

func (r *ChatRotator) finishAuthDecision(agentSessionID string) {
	if r.deps.OnAuthDecisionComplete != nil {
		r.deps.OnAuthDecisionComplete(agentSessionID)
	}
}

// reserveAttempt applies the shared per-chat inFlight + rate-limit guard for both
// rotation triggers. It returns true — marking the chat in-flight and charging
// the limiter at attempt time (belt-and-braces vs banner flap, not at success
// time) — when a fresh rotation attempt may proceed; a true result MUST be paired
// with a deferred releaseInFlight. It returns false when a rotation is already
// running for the chat or when the chat rotated inside the rate-limit window.
func (r *ChatRotator) reserveAttempt(agentSessionID string) bool {
	ok, _ := r.reserve(agentSessionID, false)
	return ok
}

// attemptStamp records what one reservation did to the SHARED reactive
// rate-limit stamp (lastAttempt), so a lane that gives its reservation back can
// restore the map to what it found rather than deleting the entry outright.
//
// lastAttempt is read and written by reserve() for EVERY lane — the auth lane,
// the usage-limit lane and the BOS-982 proxy-token lane all share that one
// window on purpose (unlike proactiveLastAttempt, BOS-318, which is a genuinely
// separate cadence). So an unconditional delete on release does not merely
// un-charge the releasing lane: it wipes whatever suppression the entry was
// carrying. Capturing the previous value makes the release exact. (BOS-982)
type attemptStamp struct {
	// stamped is the value this reservation wrote into lastAttempt. The release
	// restores nothing unless the map still holds exactly this value.
	stamped time.Time
	// prior / hadPrior are the entry the reservation replaced, if any.
	prior    time.Time
	hadPrior bool
}

// reserve is reserveAttempt's shared body. queueProxyRepair asks it to record a
// pending BOS-982 proxy-token repair when the refusal is an in-flight rotation
// (as opposed to the rate limit, where an attempt for this chat already ran
// inside the window and a second one is exactly what the limiter is for). The
// queueing happens under the SAME lock acquisition that observed the in-flight
// marker, so the rotation holding the reservation cannot release between the
// two and leave a pending signal nobody drains.
func (r *ChatRotator) reserve(agentSessionID string, queueProxyRepair bool) (bool, attemptStamp) {
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
	for id, cs := range r.chats {
		if !cs.lastAttempt.IsZero() && now.Sub(cs.lastAttempt) >= minInterval {
			cs.lastAttempt = time.Time{}
			r.gcChatLocked(id)
		}
	}
	cs := r.lookupChatLocked(agentSessionID)
	if cs != nil && cs.inFlight {
		if queueProxyRepair {
			cs.proxyRepairPending = true
		}
		return false, attemptStamp{}
	}
	var prior time.Time
	if cs != nil {
		prior = cs.lastAttempt
	}
	hadPrior := !prior.IsZero()
	if hadPrior && now.Sub(prior) < minInterval {
		r.deps.Logger.Debug().Str("agent_session_id", agentSessionID).
			Msg("auto-rotate suppressed by per-chat rate limit")
		return false, attemptStamp{}
	}
	held := r.chatLocked(agentSessionID)
	held.inFlight = true
	held.lastAttempt = now
	return true, attemptStamp{stamped: now, prior: prior, hadPrior: hadPrior}
}

// releaseAttempt undoes exactly one reservation's charge against the shared
// reactive rate-limit stamp, restoring whatever the reservation replaced instead
// of deleting the entry. An absent prior is restored by deleting; a present one
// by writing the old value back. The effect is that the other lanes see
// precisely the suppression they would have seen had this reservation never
// happened — which forgetAttempt, a lane-blind delete, cannot promise.
//
// The restore is guarded on the stamp still being the one this reservation
// wrote: if any other writer has moved lastAttempt since, that writer's entry is
// the live one and this release leaves it alone. inFlight serialises reservations
// for a chat, so the guard is uncontended on the ordinary path; it is there to
// make the invariant local and checkable rather than a claim about callers. (BOS-982)
func (r *ChatRotator) releaseAttempt(agentSessionID string, stamp attemptStamp) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cs := r.lookupChatLocked(agentSessionID)
	if cs == nil || cs.lastAttempt.IsZero() || !cs.lastAttempt.Equal(stamp.stamped) {
		return
	}
	if stamp.hadPrior {
		cs.lastAttempt = stamp.prior
		return
	}
	cs.lastAttempt = time.Time{}
	r.gcChatLocked(agentSessionID)
}

// releaseInFlight clears the in-flight marker for a chat once its dispatched
// rotation goroutine finishes, and drains any BOS-982 proxy-token repair that
// lost the reservation to it. The drain is inside releaseInFlight rather than a
// sibling defer because it MUST observe the cleared marker: a drained repair
// takes the in-flight guard for itself.
func (r *ChatRotator) releaseInFlight(agentSessionID string) {
	r.mu.Lock()
	var pending, remedied bool
	if cs := r.lookupChatLocked(agentSessionID); cs != nil {
		cs.inFlight = false
		pending, cs.proxyRepairPending = cs.proxyRepairPending, false
		remedied, cs.proxyRepairSettled = cs.proxyRepairSettled, false
		r.gcChatLocked(agentSessionID)
	}
	r.mu.Unlock()
	if pending && !remedied {
		r.dispatchProxyRepair(agentSessionID)
	}
}

// settleProxyRepair records that the reservation currently held for a chat has
// already answered what a queued proxy-token repair would ask, so releaseInFlight
// drops that repair instead of draining it.
//
// Callers are of two kinds. The pane was acted on — a respawn-in-place attempt
// (any outcome: a cap-exhausted one still means respawn-in-place was consulted and
// declined, and re-dispatching would only meet the same cap) or a completed
// rotation onto another account, which restarts the pane with freshly injected
// wiring. That is what holds "concurrent arrival of the two lanes produces exactly
// one respawn attempt". Or the repair reached a terminal no-op decision — opted
// out, no bound account — which is a property of the repo or the chat, not of this
// attempt, and so reads the same however many times it is re-derived. (BOS-982)
func (r *ChatRotator) settleProxyRepair(agentSessionID string) {
	r.mu.Lock()
	r.chatLocked(agentSessionID).proxyRepairSettled = true
	r.mu.Unlock()
}

// dispatchProxyRepair runs a proxy-token repair that was queued while another
// rotation held the chat's reservation and that rotation then finished without
// acting on the pane.
//
// Like reprobeAuth it takes only the in-flight guard and does NOT charge the
// reactive rate limiter: the limiter was already charged by the lane that won
// the reservation, for a signal that produced no remedy, and re-charging it here
// would suppress the very dispatch this drain exists to deliver. The respawn cap
// still bounds what the repair can actually do.
func (r *ChatRotator) dispatchProxyRepair(agentSessionID string) {
	r.mu.Lock()
	cs := r.chatLocked(agentSessionID)
	if cs.inFlight {
		// Another trigger claimed the chat in the instant since the release. Hand the
		// signal to that new holder rather than dropping it: releaseInFlight had to
		// delete the pending flag before calling here (it drops the lock in between),
		// so without this re-queue the signal exists nowhere and the new holder's own
		// release has nothing to drain. That new holder is very often a lane that
		// touches no pane at all — a below-threshold healthy probe, DecisionAllExhausted,
		// a usage-limit rotation that finds no eligible account — which would leave the
		// wedged pane with nothing scheduled. Re-queueing cannot produce a second
		// respawn: if the new holder DOES act on the pane it marks the remedy applied
		// and its release drops this signal exactly as it drops any other. (BOS-982)
		cs.proxyRepairPending = true
		r.mu.Unlock()
		return
	}
	cs.inFlight = true
	r.mu.Unlock()
	r.active.Add(1)
	safego.Go(r.deps.Logger, func() {
		defer r.active.Add(-1)
		defer r.releaseInFlight(agentSessionID)
		defer r.finishAuthDecision(agentSessionID)
		r.repairProxyPane(agentSessionID, nil)
	})
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
		r.settleProxyRepair(agentSessionID)
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
		log.Warn().Err(err).Msg("auto-rotate(auth): config load failed; leaving chat as-is, will re-probe")
		r.forgetAttempt(agentSessionID)
		r.scheduleReprobe(agentSessionID)
		return
	}
	cc, err := r.deps.ChatContext(ctx, agentSessionID)
	if err != nil {
		log.Warn().Err(err).Msg("auto-rotate(auth): chat context lookup failed; will re-probe")
		r.forgetAttempt(agentSessionID)
		r.scheduleReprobe(agentSessionID)
		return
	}
	if !cfg.ManagedAccountsEnabled() || !cfg.AutoRotateChatsEnabled(cc.RepoID) {
		log.Debug().Str("repo_id", cc.RepoID).Msg("auto-rotate(auth): opted out; leaving chat as-is")
		r.clearRespawnState(agentSessionID, "auto-rotation opted out")
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
		// BOS-980: a pane mid-heal flaps clean for a poll at a time. Hold an open episode
		// through the trough instead of believing the first clean reading; only a pane
		// that stays clear for authClearGraceWindow has actually recovered. holdEpisode
		// is a no-op unless a healthy streak is open, so a pane that recovered before the
		// healer ever engaged still takes the RECOVERED path below on the first reading.
		hold, heldFor := r.holdEpisode(agentSessionID)
		if hold {
			log.Debug().Dur("grace_window", authClearGraceWindow).Dur("held_for", heldFor).
				Msg("auto-rotate(auth): pane reads clean but its auth-failed episode is still latched; re-probing after the grace window")
			r.scheduleReprobeIn(agentSessionID, authClearGraceWindow)
			return
		}
		// Distinguish "clean on the first reading" from "held for the whole grace window
		// and still clean". Both end the episode, but only the second one is evidence
		// about whether authClearGraceWindow is long enough — and authRetryCycle mirrors
		// a countdown bossd does not control, so that evidence has to be greppable rather
		// than inferred from a symptom that looks identical to the pre-BOS-980 bug.
		reason, detail := "pane recovered", "pane recovered before rotation"
		if heldFor > 0 {
			reason = "held past grace window; pane sustainedly clear"
			detail = "pane sustainedly clear for the auth-failed grace window"
		}
		log.Debug().Dur("held_for", heldFor).Str("reason", reason).
			Msg("auto-rotate(auth): pane no longer auth-failed; aborting")
		r.clearRespawnState(agentSessionID, reason)
		r.deps.Recorder.Record(ctx, AuditEvent{
			SessionID: cc.SessionID, ChatID: agentSessionID, Provider: cc.Provider,
			Trigger:     triggerAuthInvalidated,
			FromAccount: cc.FromLabel,
			Outcome:     "ROTATION_OUTCOME_STATUS_ONLY_RECOVERED",
			Detail:      detail,
		})
		return
	}
	// The episode is live at this instant, so any held clean reading is stale: restart the
	// grace window from the next clean reading rather than an older one. (BOS-980)
	r.markEpisodeLive(agentSessionID)
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
		r.clearRespawnState(agentSessionID, "confirmed 401 supersedes respawn-in-place")
	}

	auditBase := AuditEvent{
		SessionID:   cc.SessionID,
		ChatID:      agentSessionID,
		Provider:    cc.Provider,
		Trigger:     triggerAuthInvalidated,
		FromAccount: cc.FromLabel,
	}

	r.rotateToEligibleAccount(ctx, log, cc, agentSessionID, auditBase, rotateOptions{})
}

// rotateOptions lets a caller of rotateToEligibleAccount tune the two audit Details
// whose wording depends on WHY the rotation was driven, and say whether the bound
// account's health should be failed as part of the decision. The zero value keeps the
// confirmed-401 path's original behaviour and wording. (BOS-981)
type rotateOptions struct {
	// RotatedDetail is the Detail recorded on ROTATION_OUTCOME_ROTATED ("" = none, the
	// confirmed-401 default: the structured From/To fields already say what happened).
	RotatedDetail string
	// NoEligibleDetail is the Detail recorded on
	// ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT ("" = the shared
	// NoEligibleAccountDetail operator remedy).
	NoEligibleDetail string
	// SuppressHealthFail rotates WITHOUT benching the bound account's health. False
	// (the confirmed-401 default) keeps today's behaviour: a 401 that the probe
	// confirmed really does invalidate the account. True is for the respawn healer,
	// whose probe just called the same account healthy — see
	// rotation.Signal.SuppressHealthFail.
	SuppressHealthFail bool
	// AbortIsBenign treats an ErrSwitchAborted refusal of the switch to the NEW target
	// as rotateAborted instead of a fault: no FAILED audit row, and the caller is told
	// to spend no retry budget. False (the confirmed-401 default) keeps that path's
	// original behaviour, where the abort falls into the shared fail-safe below.
	// True is for the respawn healer, whose retry budget is bounded and whose park is
	// visible — charging it for a chat that just recovered to WORKING would eventually
	// park a healthy chat as RESPAWN_CAP_EXHAUSTED. (BOS-981)
	AbortIsBenign bool
}

// rotateResult says how rotateToEligibleAccount finished, for a caller that carries
// retry state of its own. (BOS-981)
type rotateResult int

const (
	// rotateFault: the rotation hit a transient fault (engine decide failed, the switch
	// to the NEW target failed, an unknown decision kind). The chat is still wedged with
	// nothing re-driving it — OnAuthFailed is edge-triggered and the pane is already
	// auth-failed — so a caller that can retry must do so rather than clear its state,
	// or one DB blip parks the chat for good.
	rotateFault rotateResult = iota
	// rotateEnded: a real end state was recorded (rotated / exhausted / status-only).
	// A caller that armed respawn state should clear it.
	rotateEnded
	// rotateAborted: the switch to the NEW target was refused because the chat is
	// mid-turn, under AbortIsBenign. The pane was deliberately not interrupted and
	// nothing was consumed, so this is neither a fault nor an end state: no failure
	// audit is written and the caller must NOT spend retry budget on it.
	rotateAborted
)

// rotateToEligibleAccount asks the engine for an AuthInvalidated rotation target and
// applies the decision, recording exactly one audit row on every branch. Extracted
// from rotateAuth so the respawn-in-place path can reach a REAL rotation when the
// bound account itself cannot host a respawn (BOS-981) instead of carrying a second
// copy of this switch that could drift from the confirmed-401 one.
//
// It never schedules a re-probe. The returned rotateResult says which of the three
// shapes the branch it took had — a real end state, a transient fault, or (under
// opts.AbortIsBenign) a mid-turn abort that consumed nothing; see rotateResult for
// what a caller owes each. rotateAuth ignores it: the confirmed-401 path has no
// retry budget of its own and keeps its original leave-as-is behaviour.
func (r *ChatRotator) rotateToEligibleAccount(
	ctx context.Context,
	log zerolog.Logger,
	cc ChatContext,
	agentSessionID string,
	auditBase AuditEvent,
	opts rotateOptions,
) rotateResult {
	decision, err := r.deps.Decide(ctx, DecideRequest{
		Provider:           cc.Provider,
		SessionID:          cc.SessionID,
		AgentSessionID:     agentSessionID,
		AccountID:          cc.AccountID,
		Kind:               AuthInvalidated,
		SuppressHealthFail: opts.SuppressHealthFail,
	})
	if err != nil {
		log.Warn().Err(err).Msg("auto-rotate(auth): engine decide failed; leaving chat as-is")
		failed := auditBase
		failed.Outcome = "ROTATION_OUTCOME_FAILED"
		failed.Detail = "engine decide failed"
		r.deps.Recorder.Record(ctx, failed)
		return rotateFault
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
			if opts.AbortIsBenign && errors.Is(err, ErrSwitchAborted) {
				// The chat recovered to WORKING before this switch could run, so the pane
				// was deliberately not interrupted and nothing was consumed. Recording it
				// as FAILED and reporting a fault would make the caller spend a life of its
				// bounded retry budget on a chat that is now healthy, and enough of those
				// park it as RESPAWN_CAP_EXHAUSTED. Write no audit row and say so. (BOS-981)
				log.Info().Str("account_id", decision.AccountID).
					Msg("auto-rotate(auth): rotation aborted (chat mid-turn); leaving chat as-is")
				return rotateAborted
			}
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
			return rotateFault
		}
		log.Info().Str("account", res.SwitchedToLabel).Bool("fresh", res.Fresh).
			Msg("auto-rotate(auth): chat rotated to next account")
		r.settleProxyRepair(agentSessionID)
		if r.deps.CaptureReactiveRotation != nil {
			r.deps.CaptureReactiveRotation(ctx, cc.Provider, "error")
		}
		rotated := auditBase
		rotated.ToAccount = res.SwitchedToLabel
		rotated.Outcome = "ROTATION_OUTCOME_ROTATED"
		rotated.Detail = opts.RotatedDetail
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
		if opts.NoEligibleDetail != "" {
			noEligible.Detail = opts.NoEligibleDetail
		}
		r.deps.Recorder.Record(ctx, noEligible)
	default:
		log.Warn().Int("kind", int(decision.Kind)).
			Msg("auto-rotate(auth): unknown decision kind; leaving chat as-is")
		unknown := auditBase
		unknown.Outcome = "ROTATION_OUTCOME_FAILED"
		unknown.Detail = "unknown decision kind"
		r.deps.Recorder.Record(ctx, unknown)
		return rotateFault
	}
	return rotateEnded
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
	cs := r.chatLocked(agentSessionID)
	if cs.inFlight {
		r.mu.Unlock()
		log.Debug().Msg("proactive-sweep: rotation already in flight for chat; skipping")
		return false
	}
	cs.inFlight = true
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
	r.chatLocked(agentSessionID).proactiveLastAttempt = now
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
	// A completed switch stops, rebinds and respawns the pane with freshly injected
	// wiring — a pane-level remedy, and the fourth Switch call site that has to say
	// so. This lane takes the SAME shared reservation as the reactive ones, so a
	// BOS-982 proxy-token 401 arriving mid-switch is queued behind it; without this
	// mark releaseInFlight would drain that queued repair into a same-account
	// respawn of the pane this sweep just restarted. (BOS-982)
	r.settleProxyRepair(agentSessionID)
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
	for id, cs := range r.chats {
		if !cs.proactiveLastAttempt.IsZero() && now.Sub(cs.proactiveLastAttempt) >= minInterval {
			cs.proactiveLastAttempt = time.Time{}
			r.gcChatLocked(id)
		}
	}
	if cs := r.lookupChatLocked(agentSessionID); cs != nil &&
		!cs.proactiveLastAttempt.IsZero() && now.Sub(cs.proactiveLastAttempt) < minInterval {
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
	if cs := r.lookupChatLocked(agentSessionID); cs != nil {
		cs.lastAttempt = time.Time{}
		r.gcChatLocked(agentSessionID)
	}
	r.mu.Unlock()
}

// handleHealthyAuthProbe advances the consecutive-Healthy streak for a pane that is
// auth-failed while its bound account probes healthy. Below the threshold it records a
// status-only audit and schedules a re-probe; at the threshold it respawns in place on
// the SAME account. (BOS-482)
func (r *ChatRotator) handleHealthyAuthProbe(ctx context.Context, cc ChatContext, agentSessionID string) {
	now := r.deps.Now()
	// The pane's current episode anchor, recorded on the streak and logged so an operator
	// can see when it re-pinned mid-episode — the fingerprint of the flap this latch
	// absorbs. Deliberately NOT used to reset the streak; see healthyStreak.episodeSince.
	// (BOS-980)
	var episodeSince time.Time
	if r.deps.AuthFailedSince != nil {
		if since, ok := r.deps.AuthFailedSince(agentSessionID); ok {
			episodeSince = since
		}
	}
	r.mu.Lock()
	cs := r.chatLocked(agentSessionID)
	streak := cs.healthy
	if streak.count == 0 || now.Sub(streak.at) >= healthyCountTTL {
		streak = healthyStreak{count: 1, at: now}
	} else {
		streak.count++
		streak.at = now
	}
	streak.episodeSince = episodeSince
	// The pane is auth-failed right now, so no grace window is running.
	streak.clearedAt = time.Time{}
	cs.healthy = streak
	count := streak.count
	r.mu.Unlock()

	log := r.deps.Logger.With().Str("agent_session_id", agentSessionID).Logger()
	if count < healthyRespawnThreshold {
		log.Info().
			Int("healthy_streak", count).
			Int("healthy_streak_threshold", healthyRespawnThreshold).
			Time("episode_since", episodeSince).
			Msg("auto-rotate(auth): account healthy but pane auth-failed; awaiting confirmation before respawn")
		// Match the pre-BOS-482 healthy branch: record status-only but do NOT forget the
		// attempt (the re-probe timer, not the reactive rate limiter, drives the retry).
		r.recordAuthProbeStatusOnly(ctx, cc, agentSessionID, "auth probe did not confirm invalidation")
		r.scheduleReprobe(agentSessionID)
		return
	}
	// BOS-980 AC5: the below-threshold branch above can only ever print healthy_streak=1,
	// so without this line "the streak reached the threshold" is indistinguishable in the
	// log from "the streak was reset before it got there".
	log.Info().
		Int("healthy_streak", count).
		Int("healthy_streak_threshold", healthyRespawnThreshold).
		Time("episode_since", episodeSince).
		Msg("auto-rotate(auth): healthy streak reached threshold; respawning in place")
	r.respawnInPlace(ctx, cc, agentSessionID, authRespawnLane)
}

// respawnLane identifies which caller reached respawn-in-place. Two lanes
// converge on respawnInPlace and they are NOT the same story: the auth lane
// arrives after two consecutive Healthy probes on a pane the status scrape
// flagged auth-failed, and an attempt that does not complete belongs back on the
// re-probe timer; the proxy-token lane (BOS-982) arrives from a 401 the failover
// proxy minted itself, has deliberately skipped the probe, and must never hand
// the chat to the probing lane on its way out.
//
// The audit Trigger is deliberately NOT a lane field. Trigger strings are proto
// enum NAMES: stored rows are hydrated back with
// pb.RotationTrigger_value[ev.Trigger] (server/account_binding.go), so a
// Go-only "ROTATION_TRIGGER_PROXY_TOKEN_UNRESOLVED" would travel over
// bossanova.v1 as ROTATION_TRIGGER_UNSPECIFIED — strictly less legible than the
// coarse-but-true AUTH_INVALIDATED it would replace, and adding the enum entry
// is a proto change this branch is not allowed to make. The lane is therefore
// carried in the two fields that stay free text end to end: the audit Detail and
// the log prefix, so `SELECT detail` and a log grep both separate the lanes.
type respawnLane struct {
	// logPrefix opens every log line respawnInPlace writes for this lane.
	logPrefix string
	// detailPrefix opens every audit Detail respawnInPlace records for this lane.
	detailPrefix string
	// reprobe says whether an incomplete respawn (mid-turn abort or a failed
	// Switch) hands the chat back to the auth-probe timer.
	reprobe bool
	// ownsRespawnState says whether this lane owns the BOS-482 respawn state —
	// the healthy[] streak and the reprobeCancel[] timer. Only the auth lane ever
	// writes either: it is the lane that runs AuthProbe, accumulates the streak
	// and arms the re-probe. The proxy-token lane skips the probe by design, so it
	// opens no streak and arms no timer, and must not tear down state it did not
	// build (see the capped branch of respawnInPlace).
	//
	// It tracks reprobe exactly today, and is a separate field on purpose: reprobe
	// answers "does an incomplete respawn re-arm the timer", ownsRespawnState
	// answers "may a cap-exhausted stop delete the streak". A future lane could
	// well want one without the other, and collapsing them would make that a
	// silent behaviour change rather than a decision. (BOS-982)
	ownsRespawnState bool
	// refundAbortedCharge says whether an ErrSwitchAborted refusal hands its
	// respawn-cap charge back (refundRespawn). It is NOT a tidiness preference and
	// must not be widened to every lane: a lane may refund an aborted charge only
	// when something OTHER THAN THE CAP bounds its re-entry.
	//
	// The proxy-token lane qualifies. It arms no timer, so its re-entry is the pane's
	// own next unresolved-token 401, and repairProxyPane holds the reactive rate-limit
	// slot on respawnAborted — so ChatRotateMinInterval, not the cap, is what stops a
	// permanently mid-turn pane from spinning. Refunding there costs nothing and keeps
	// the shared budget available for the repair that can actually land once the pane
	// goes idle.
	//
	// The auth lane does NOT qualify, and this is the whole reason the field exists.
	// It re-arms scheduleReprobe on every abort (reprobe: true) and reprobeAuth
	// deliberately charges no reactive limiter, while healthyCountTTL (30m) far
	// outlasts reprobeInterval (5m), so a streak at the threshold never decays between
	// re-probes. The cap is therefore that lane's ONLY termination condition: refunding
	// means it is never reached, the respawnCapped branch never runs, clearRespawnState
	// never fires, and an auth-failed pane that probes Healthy while staying mid-turn
	// polls forever at 5-minute cadence — one real upstream AuthProbe plus one Switch
	// attempt per pass. That is exactly the loop BOS-482's "a pane that can never
	// respawn cannot loop" invariant exists to rule out. (BOS-982)
	refundAbortedCharge bool
	// handlesNotAttempted decides whether this lane runs BOS-981's not-attempted
	// machinery when Switch refuses BEFORE touching the pane (ErrSwitchNotAttempted).
	// The refund itself is unconditional on every lane — nothing was consumed — so what
	// this gates is only what happens NEXT, and it gates two things together on purpose:
	//
	//   - rotating away from a bound account that is itself ineligible, which is an
	//     ACCOUNT-level remedy; and
	//   - charging chargeNotAttempt, a replacement stopping condition the refund made
	//     necessary.
	//
	// They move together on today's two lanes for one reason each rather than one shared
	// reason, so a future lane may well want one without the other — split this field
	// then, and give TestRespawnLanePredicatesAreConsistent the rule for each half.
	// The auth lane wants both: rotating away is exactly what an operator would do by
	// hand, and its re-probe timer is unmetered, so without a budget it would retry a
	// persistent refusal forever. The proxy-token lane wants neither: rotating the
	// account is the action BOS-982 exists to avoid taking for a 401 this proxy minted
	// itself, and its re-entry is already paced by the reactive rate limiter, so a
	// budget would only park a chat the limiter was already holding back. A lane that
	// sets this false must therefore be bounded from outside, exactly as
	// refundAbortedCharge requires — see the rule in
	// TestRespawnLanePredicatesAreConsistent.
	handlesNotAttempted bool
	// retryNote is the log tail describing what drives the next attempt, so an
	// operator is not told "will re-probe" by a lane that arms no re-probe.
	retryNote string
}

var (
	// authRespawnLane is the BOS-482 healthy-streak caller: it owns the re-probe
	// timer, so an incomplete respawn re-arms it. refundAbortedCharge is false
	// because that same re-arming makes the cap this lane's only way to stop —
	// see the field's doc.
	authRespawnLane = respawnLane{
		logPrefix:           "auto-rotate(auth)",
		reprobe:             true,
		ownsRespawnState:    true,
		handlesNotAttempted: true,
		retryNote:           "will re-probe",
	}
	// proxyTokenRespawnLane is the BOS-982 caller. reprobe is false on purpose:
	// scheduleReprobe runs rotateAuth, i.e. the AuthProbe + account-rotation lane
	// OnProxyTokenUnresolved exists to skip, and the pane reaching it that way was
	// typically never flagged auth-failed — so rotateAuth would take its
	// recovered branch and record a RECOVERED row for a pane that is still wedged.
	// No timer is armed instead. The pane keeps retrying against a token the proxy
	// still cannot resolve, so its next unknown-token 401 re-enters this lane on its
	// own: immediately when a Switch actually failed (respawnIncomplete), where
	// repairProxyPane gives the rate-limit slot back, and after
	// ChatRotateMinInterval when the chat was merely mid-turn (respawnAborted),
	// where holding the slot keeps a still-mid-turn pane on the limiter's cadence
	// rather than its own 401 cadence.
	//
	// The status-scrape lane is the slower route back INTO a repair, but it is not a
	// second budget behind this one: handleHealthyAuthProbe converges on the same
	// per-chat respawnWindow, so whatever this lane spends is spent for both. That is
	// what makes refundAbortedCharge on the mid-turn abort load-bearing rather than
	// tidy — without it a wedged mid-turn pane exhausts the shared cap and closes the
	// backstop as well as this lane. The refund is safe HERE, and only here, because
	// this lane arms no retry timer: see refundAbortedCharge's doc for why the auth
	// lane must keep the charge instead.
	proxyTokenRespawnLane = respawnLane{
		logPrefix:           "auto-rotate(proxy-token)",
		detailPrefix:        proxyRepairDetailPrefix,
		refundAbortedCharge: true,
		retryNote:           "the pane's next unresolved-token 401 re-enters this lane",
	}
)

// respawnOutcome tells respawnInPlace's caller what happened to the PANE, which
// is what a lane's retry bookkeeping turns on. The audit row already says this
// to an operator; this says it to the code. (BOS-982)
type respawnOutcome int

const (
	// respawnCompleted: the Switch landed and the pane was restarted.
	respawnCompleted respawnOutcome = iota
	// respawnIncomplete: a respawn was attempted and the Switch ERRORED. The pane
	// is untouched and the error may not repeat, so a fresh attempt is worth making
	// soon.
	respawnIncomplete
	// respawnAborted: the Switch declined to interrupt a mid-turn chat
	// (ErrSwitchAborted). The pane is untouched too, but the reason is a state that
	// persists — a mid-turn pane stays mid-turn — so this is emphatically NOT a case
	// for clearing a retry guard: an immediate re-entry only meets the same abort,
	// burning a LoadConfig + ChatContext + Switch round trip per 401. Kept distinct
	// from respawnIncomplete for exactly that reason. On the proxy-token lane — the
	// only lane that reads this value — the respawn cap is NOT what bounds it:
	// refundRespawn gives that charge back, because Switch declined before touching
	// the pane, and the rate limiter bounds the re-entry instead. (BOS-982)
	respawnAborted
	// respawnCapped: the per-chat respawn cap was already exhausted, so nothing was
	// attempted. Re-entering would only meet the same cap, so this is NOT a case
	// for clearing a retry guard.
	respawnCapped
)

// proxyRepairDetailPrefix marks every audit row the BOS-982 proxy-token lane
// writes. It is the only lane marker the audit table can carry (see respawnLane).
const proxyRepairDetailPrefix = "proxy-token repair: "

// respawnInPlace stops → same-account rebinds → respawns the chat to refresh its
// injected auth wiring, charging the per-chat respawn cap BEFORE the Switch so a
// failing respawn still counts. Whether an ErrSwitchAborted refusal hands that charge
// back is LANE-DEPENDENT and must stay that way: only a lane that opted in via
// respawnLane.refundAbortedCharge is refunded, and authRespawnLane deliberately does
// not opt in, because the cap is the only thing that stops it. A cap-exhausted attempt records
// ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED and goes quiet; an ErrSwitchAborted refusal
// is fail-safe (leave as-is, no FAILED audit); any other Switch error is a
// FAILED audit. Success records ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT and clears the
// streak + pending re-probe. (BOS-482)
//
// BOS-981 splits the remaining error space in two, on the lanes that opt in. A Switch
// that returned BEFORE it touched the pane (ErrSwitchNotAttempted) consumed nothing, so
// its charge is refunded on EVERY lane. What happens next is lane-owned
// (handlesNotAttempted). On a lane that handles it: if the bound account itself is
// ineligible the healer rotates to an eligible account instead (the automatic form of
// `boss account switch`) and parks terminally — except when that rotation is itself
// aborted mid-turn, which is the same benign nothing-was-consumed shape as the
// same-account abort and so costs no budget; any other pre-pane-touch refusal has no
// account-side remedy and is usually transient, so it records outcomeRespawnNotAttempted
// naming the cause and re-probes, bounded by its own per-chat budget (chargeNotAttempt)
// since the refund removed the respawn cap as the stopping condition, parking on the
// enum-backed outcomeRespawnCapExhausted once that budget is spent. On a lane that does
// not handle it, the same refusal is simply an incomplete respawn. Anything else reached
// far enough to disturb the chat and keeps today's behaviour — charge kept, FAILED
// audit, re-probe armed where the lane arms one.
//
// lane decides the log prefix, the audit Detail prefix, the retry note appended when an
// ATTEMPTED respawn does not complete (retryNote — the capped branch attempts nothing
// and logs none), and — the behavioural part — whether an incomplete respawn re-arms the
// auth re-probe (reprobe), whether a cap-exhausted stop tears the respawn state down
// (ownsRespawnState), whether an aborted attempt refunds its cap charge
// (refundAbortedCharge), and whether the lane runs BOS-981's not-attempted machinery at
// all (handlesNotAttempted). The returned
// respawnOutcome carries the rest of that decision back to the caller, because
// the proxy-token lane's answer to an incomplete respawn is not a timer but a
// released rate-limit slot, which only repairProxyPane knows how to give. (BOS-982)
func (r *ChatRotator) respawnInPlace(ctx context.Context, cc ChatContext, agentSessionID string, lane respawnLane) respawnOutcome {
	log := r.deps.Logger.With().Str("agent_session_id", agentSessionID).Logger()
	r.settleProxyRepair(agentSessionID)
	auditBase := AuditEvent{
		SessionID:   cc.SessionID,
		ChatID:      agentSessionID,
		Provider:    cc.Provider,
		Trigger:     triggerAuthInvalidated,
		FromAccount: cc.FromLabel,
		ToAccount:   cc.FromLabel, // same account
	}
	if !r.chargeRespawn(agentSessionID) {
		log.Warn().Msg(lane.logPrefix + ": respawn-in-place cap reached; leaving chat as-is")
		capped := auditBase
		capped.Outcome = outcomeRespawnCapExhausted
		capped.Detail = lane.detailPrefix + "respawn-in-place cap reached — re-authenticate or restart the daemon"
		r.deps.Recorder.Record(ctx, capped)
		// Go quiet: cancel the re-probe and drop the streak. A fresh auth-failed edge
		// (or a later window) reopens the path.
		//
		// Lane-gated, unlike the success path below. healthy[] and reprobeCancel[]
		// belong to the BOS-482 AUTH lane — it is the only lane that opens a streak or
		// arms a re-probe — and here nothing happened to the pane at all, so a
		// proxy-token cap burn clearing them would cancel a pending re-probe and reset
		// a streak that this lane neither built nor invalidated. Since the two lanes
		// share one respawn cap, that is reachable in ordinary operation: a wedged pane
		// emits 401s far faster than the auth lane re-probes. The success path is
		// deliberately NOT gated — there the pane really was restarted, which is what
		// makes any standing streak or armed re-probe stale for both lanes (BOS-980).
		// (BOS-982)
		if lane.ownsRespawnState {
			r.clearRespawnState(agentSessionID, "respawn cap exhausted")
		}
		return respawnCapped
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
			// deliberately not interrupted. NOT a failure — leave as-is. On the auth lane
			// the streak is preserved so the next re-probe retries immediately once the
			// chat is idle again.
			//
			// On a refunding lane, give the cap charge back. Switch declined BEFORE
			// touching the pane, so this attempt is the one case chargeRespawn's
			// charge-before-Switch rule was not written for: keeping it spends a slot of
			// the shared per-chat budget on a respawn that provably never happened, and a
			// proxy-lane pane that keeps aborting would exhaust respawnCap without ever
			// being repaired — including once it finally goes idle, and including for the
			// auth lane, which shares the same window.
			//
			// LANE-GATED, and the gate is the load-bearing part. On the proxy-token lane
			// the refund does not reopen the loop chargeRespawn guards against: that lane
			// arms no timer and repairProxyPane holds its rate-limit slot on this outcome,
			// so a permanently mid-turn pane re-enters once per ChatRotateMinInterval
			// forever and every such pass ends here without touching the pane — a slow
			// poll bounded by the REACTIVE RATE LIMITER, not a hot loop. The auth lane has
			// no such bound: it re-arms scheduleReprobe just below and reprobeAuth charges
			// no limiter, so its cap is its only termination condition and refunding there
			// would make it poll forever (see respawnLane.refundAbortedCharge). The
			// genuine Switch-error path below keeps its charge on every lane, precisely
			// because that one may well have touched the pane.
			if lane.refundAbortedCharge {
				r.refundRespawn(agentSessionID)
			}
			log.Info().Msg(lane.logPrefix + ": respawn aborted (chat mid-turn); leaving chat as-is, " + lane.retryNote)
			if lane.reprobe {
				r.scheduleReprobe(agentSessionID)
			}
			return respawnAborted
		}
		if errors.Is(err, ErrSwitchNotAttempted) {
			// The switch was refused BEFORE it touched the pane, so nothing was
			// consumed: refund the charge we took above. (BOS-981)
			//
			// UNGATED, unlike the ErrSwitchAborted refund above, and the difference is
			// the stopping condition each refund leaves behind rather than a change of
			// mind about charge-before-Switch. The abort refund removes the cap without
			// replacing it, which is why only an externally-bounded lane may take it
			// (respawnLane.refundAbortedCharge). This refund is paired with
			// chargeNotAttempt below on the lane that needs a replacement bound, and on a
			// lane that does not handle not-attempted at all the caller's own bound is
			// already what paces re-entry — so no lane is left unbounded by taking it.
			r.refundRespawn(agentSessionID)
			if !lane.handlesNotAttempted {
				// This lane offers no account-side remedy and its re-entry is bounded
				// from outside (see respawnLane.handlesNotAttempted), so a pre-pane-touch
				// refusal is just an incomplete respawn: record what happened and hand the
				// outcome back. Rotating away from the bound account here would be this
				// lane doing the account rotation it exists to avoid, and spending a
				// not-attempted budget would eventually park a chat whose refusal the
				// caller recovers from on its own cadence.
				log.Warn().Err(err).Msg(lane.logPrefix + ": respawn-in-place refused before the pane was touched; leaving chat as-is, " + lane.retryNote)
				notAttempted := auditBase
				notAttempted.Outcome = outcomeRespawnNotAttempted
				notAttempted.Detail = lane.detailPrefix + "respawn-in-place refused before the pane was touched: " + notAttemptedCause(err)
				r.deps.Recorder.Record(ctx, notAttempted)
				return respawnIncomplete
			}
			if errors.Is(err, ErrSwitchAccountIneligible) {
				// The bound account itself cannot host a respawn (disabled / health-failed
				// / cooling). Respawning in place can only ever be refused, so do what an
				// operator would do by hand: rotate to an eligible enabled account.
				//
				// SuppressHealthFail: we got here from a HEALTHY auth probe (this branch is
				// reachable only from a handlesNotAttempted lane, i.e. handleHealthyAuthProbe),
				// so the bound account is administratively ineligible, not auth-broken.
				// Benching its health would contradict the probe and outlive the operator's
				// remedy — re-enabling an account restores status, never health.
				log.Info().Err(err).Msg(lane.logPrefix + ": bound account is not eligible; rotating to an eligible account instead of respawning")
				rotateBase := auditBase
				rotateBase.ToAccount = ""
				switch r.rotateToEligibleAccount(ctx, log, cc, agentSessionID, rotateBase, rotateOptions{
					RotatedDetail:      boundAccountPhrase(cc.FromLabel) + " is not eligible — rotated to an eligible account",
					NoEligibleDetail:   boundAccountPhrase(cc.FromLabel) + " is not eligible and no other account is — " + NoEligibleAccountDetail,
					SuppressHealthFail: true,
					AbortIsBenign:      true,
				}) {
				case rotateEnded:
					// A real end state was recorded, so a fresh auth-failed edge — not a
					// re-probe — is what reopens this chat.
					if lane.ownsRespawnState {
						r.clearRespawnState(agentSessionID, "bound account not eligible for respawn-in-place")
					}
					return respawnCapped
				case rotateAborted:
					// The chat recovered to WORKING before the rotation could run. Nothing was
					// consumed, so take the same fail-safe the same-account abort above takes:
					// keep the respawn state, spend no budget, and re-probe once the chat is
					// idle again. Charging here would eventually park a recovered chat as
					// RESPAWN_CAP_EXHAUSTED. (BOS-981)
					log.Info().Msg(lane.logPrefix + ": rotation to an eligible account aborted (chat mid-turn); leaving chat as-is, " + lane.retryNote)
					if lane.reprobe {
						r.scheduleReprobe(agentSessionID)
					}
					return respawnAborted
				case rotateFault:
					// Handled below, outside the switch, so the charge/park sequence stays in
					// one straight line rather than nested another level deep.
				}
				// The rotation itself hit a transient fault and already recorded its own
				// FAILED row. Clearing state here would wedge the chat exactly the way the
				// refusal below would: charge the same bounded budget and re-probe instead,
				// then park once it is spent.
				if r.chargeNotAttempt(agentSessionID) {
					if lane.reprobe {
						r.scheduleReprobe(agentSessionID)
					}
					return respawnIncomplete
				}
				log.Warn().Msg(lane.logPrefix + ": rotating away from the ineligible bound account kept failing; leaving chat as-is")
				// rotateBase, not auditBase: this park is about the failed ROTATION, so it
				// must not inherit the respawn's ToAccount (the bound account) and render
				// as a same-account event under a "rotating away kept failing" Detail.
				rotateCapped := rotateBase
				rotateCapped.Outcome = outcomeRespawnCapExhausted
				rotateCapped.Detail = boundAccountPhrase(cc.FromLabel) +
					" is not eligible and rotating to an eligible account kept failing"
				r.deps.Recorder.Record(ctx, rotateCapped)
				if lane.ownsRespawnState {
					r.clearRespawnState(agentSessionID, "rotate-away from the ineligible bound account retry budget spent")
				}
				return respawnCapped
			}
			// No account-side remedy: the switch could not even read the rows it needed
			// (store not configured / chat row unreadable / session gone). Those causes
			// are mostly transient, so retry on the re-probe cadence — but the refund
			// just removed the respawn cap as the stopping condition, so bound the
			// retries separately and park on a REAL end state when that budget is spent.
			cause := notAttemptedCause(err)
			if r.chargeNotAttempt(agentSessionID) {
				log.Warn().Err(err).Msg(lane.logPrefix + ": respawn-in-place refused before the pane was touched; " + lane.retryNote)
				notAttempted := auditBase
				notAttempted.Outcome = outcomeRespawnNotAttempted
				notAttempted.Detail = lane.detailPrefix + "respawn-in-place refused before the pane was touched: " + cause
				r.deps.Recorder.Record(ctx, notAttempted)
				if lane.reprobe {
					r.scheduleReprobe(agentSessionID)
				}
				return respawnIncomplete
			}
			// Budget spent. Park on outcomeRespawnCapExhausted rather than the free-string
			// outcome, because the two corroboration families disagree about free strings
			// and the enum-backed value satisfies both. The store-side check filters on the
			// RAW outcome text and excludes only ''/UNSPECIFIED/STATUS_ONLY_DISABLED, so it
			// would accept the free string; but the proto-side check walks decoded events
			// and drops UNSPECIFIED, which is what any unknown string decodes to — and it
			// only ever inspects the NEWEST auth-invalidated row per chat, so parking on a
			// free string would leave that path uncorroborated and the attention banner
			// dark. A silently quiet chat with no banner is the failure mode this ticket
			// exists to end.
			log.Warn().Err(err).Msg(lane.logPrefix + ": respawn-in-place kept being refused before the pane was touched; leaving chat as-is")
			capped := auditBase
			capped.Outcome = outcomeRespawnCapExhausted
			capped.Detail = lane.detailPrefix + "respawn-in-place kept being refused before the pane was touched: " + cause
			r.deps.Recorder.Record(ctx, capped)
			if lane.ownsRespawnState {
				r.clearRespawnState(agentSessionID, "respawn-in-place refusal retry budget spent")
			}
			return respawnCapped
		}
		// The switch got far enough to disturb the pane (or we cannot tell): the attempt
		// was real, so it keeps its charge and stays a FAILED audit.
		log.Warn().Err(err).Msg(lane.logPrefix + ": respawn-in-place did not complete; leaving chat as-is, " + lane.retryNote)
		failed := auditBase
		failed.Outcome = "ROTATION_OUTCOME_FAILED"
		failed.Detail = lane.detailPrefix + "respawn-in-place did not complete"
		r.deps.Recorder.Record(ctx, failed)
		if lane.reprobe {
			r.scheduleReprobe(agentSessionID)
		}
		return respawnIncomplete
	}
	log.Info().Bool("fresh", res.Fresh).Msg(lane.logPrefix + ": respawned chat in place on same account to refresh auth wiring")
	done := auditBase
	done.Outcome = outcomeRespawnedSameAccount
	if res.Fresh {
		done.Detail = lane.detailPrefix + "respawned in place to refresh auth wiring — started fresh"
	} else {
		done.Detail = lane.detailPrefix + "respawned in place to refresh auth wiring — resumed"
	}
	r.deps.Recorder.Record(ctx, done)
	// Success: drop the streak and cancel any pending re-probe. If the respawn did not
	// actually clear the wedge, the pane flips auth-failed again and the edge-triggered
	// OnAuthFailed starts a fresh cycle (still bounded by the respawn cap window).
	//
	// This is also what keeps the BOS-980 latch from looping: respawn-in-place reuses the
	// tmux pane WITHOUT clearing the screen, so the pre-respawn login banner is still in
	// statusdetect's tail and the tracker's marker (and therefore AuthFailedSince) never
	// falls. Deleting the whole streak entry here — latch fields included — means the
	// still-visible banner cannot resurrect the old confirmations: the next edge starts a
	// fresh streak at 1, and the respawn cap bounds how far that can go.
	r.clearRespawnState(agentSessionID, "respawned in place")
	return respawnCompleted
}

// chargeRespawn records one respawn-in-place attempt against the per-chat window and
// reports whether it is within the cap. Charged before the Switch so a failed respawn
// still counts (a pane that can never respawn cannot loop). Returns false when the cap
// is already reached. (BOS-482)
//
// Two refusals are refunded rather than exempted, and they are gated differently.
// An ErrSwitchAborted refusal means Switch never touched the pane, so on
// proxyTokenRespawnLane refundRespawn gives that charge back; authRespawnLane keeps it,
// because there this cap is the only thing that ends the retry — see
// respawnLane.refundAbortedCharge. (BOS-982) An ErrSwitchNotAttempted refusal likewise
// never touched the pane and is refunded on EVERY lane, because that refund does not
// leave a lane unbounded: the lane that would otherwise be unbounded charges
// chargeNotAttempt in its place — see respawnLane.handlesNotAttempted. (BOS-981)
func (r *ChatRotator) chargeRespawn(agentSessionID string) bool {
	now := r.deps.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	var w respawnWindow
	if cs := r.lookupChatLocked(agentSessionID); cs != nil {
		w = cs.respawns
	}
	if w.count == 0 || now.Sub(w.windowStart) >= respawnCapWindow {
		w = respawnWindow{count: 0, windowStart: now}
	}
	if w.count >= respawnCap {
		return false
	}
	w.count++
	r.chatLocked(agentSessionID).respawns = w
	return true
}

// refundRespawn gives back exactly one charge chargeRespawn made for the chat, for a
// respawn that provably never touched the pane. Callers must NOT hold r.mu. (BOS-982)
//
// It is the ErrSwitchAborted path's counterpart to chargeRespawn's charge-before-Switch
// rule, not a repeal of it. That rule exists so a respawn that MIGHT have touched the
// pane still counts — "a pane that can never respawn cannot loop" (BOS-482) — which is
// why a genuine Switch error keeps its charge. A mid-turn abort is the one outcome where
// Switch declined before doing anything at all, and it is the LIKELY outcome on the
// BOS-982 proxy-token lane: the pane minted that 401 by making an API request, so it is
// mid-turn almost by construction. Leaving those charges standing spends the whole
// respawnCap budget on a pane nothing was done to, so the pane cannot be repaired even
// once it goes idle — and because handleHealthyAuthProbe converges on this SAME
// respawnWindow, it takes the auth lane's budget down with it.
//
// Only a lane whose re-entry is bounded by something other than the cap may call this
// FOR AN ABORT; respawnLane.refundAbortedCharge is that gate and states why. The
// ErrSwitchNotAttempted caller added by BOS-981 is the second entry point and is
// deliberately UNGATED: it hands the same charge back for the same reason, but it does
// not leave the lane unbounded, because chargeNotAttempt supplies a replacement bound on
// the lane that needs one and a lane that runs no not-attempted machinery is bounded by
// its caller instead (respawnLane.handlesNotAttempted).
//
// What makes the refund EXACT is not a timing argument, it is the inFlight marker.
// respawnInPlace only ever runs while the chat's inFlight marker is held — reserve sets
// it before dispatching OnAuthFailed/OnProxyTokenUnresolved, dispatchProxyRepair sets it
// before the drained repair, and reprobeAuth sets it before the timer-driven re-entry —
// and it is cleared only by releaseInFlight, after respawnInPlace has returned. So no
// other chargeRespawn for this chat can interleave between the charge this call is
// giving back and the call itself, however long Switch blocks, and regardless of a clock
// jump or a Switch that ignores its context.
//
// Exactness rules, in the same spirit as releaseAttempt:
//   - decrement by one, never reset to zero: an earlier charge in the same window is not
//     this call's to give back. Floored at 0.
//   - never move windowStart forward: a refund must not extend the window.
//   - skip a window that has already rolled over. This one is a cheap guard, NOT a
//     precise test, and it errs in the safe direction: chargeRespawn only re-stamps
//     windowStart when count is 0 or the window had already rolled, so a second charge
//     inside a live window inherits a windowStart up to respawnCapWindow-epsilon old.
//     A charge at t=0, a second at t=59m58s and an abort at t=60m03s therefore reads as
//     rolled over and skips a refund it could legitimately have made. That is harmless:
//     a rolled-over window is discarded wholesale by the next chargeRespawn, so the
//     un-refunded charge suppresses nothing. Missing a refund costs one slot; making one
//     against a window this call did not charge would hand back budget that is not
//     there, which is why the guard stays.
//
// Clearing windowStart along with the last charge is behaviour-neutral rather than a
// window reset: chargeRespawn ignores windowStart entirely when count is 0 and stamps a
// fresh one. Zeroing it is what keeps the entry ELIGIBLE for reclamation.
func (r *ChatRotator) refundRespawn(agentSessionID string) {
	now := r.deps.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	cs := r.lookupChatLocked(agentSessionID)
	if cs == nil || cs.respawns.count == 0 {
		return
	}
	if now.Sub(cs.respawns.windowStart) >= respawnCapWindow {
		return
	}
	cs.respawns.count--
	if cs.respawns.count == 0 {
		cs.respawns.windowStart = time.Time{}
	}
	// Defence in depth, not the reclaim itself: cs.inFlight is necessarily true here
	// (see the inFlight paragraph above), so isZero cannot be satisfied on this call
	// and nothing is dropped. The zeroing above is what makes the entry eligible for
	// the reclaim releaseInFlight performs once the marker clears.
	r.gcChatLocked(agentSessionID)
}

// chargeNotAttempt records one pre-pane-touch refusal that has no account-side remedy
// against the per-chat window and reports whether a retry is still within budget.
// Mirrors chargeRespawn exactly (same cap, same window, same rollover rule) but on its
// own counter, because those refusals refund their respawn charge and so cannot be
// bounded by the respawn cap. Returns false once the budget is spent. (BOS-981)
//
// Its counter lives on chatState alongside every other lane's, so the entry is reclaimed
// by the same rule: a spent-and-cleared budget zeroes the window and calls gcChatLocked
// rather than deleting the entry, which would destroy the other lanes' state. (BOS-982)
func (r *ChatRotator) chargeNotAttempt(agentSessionID string) bool {
	now := r.deps.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	cs := r.chatLocked(agentSessionID)
	if cs.notAttempts.count == 0 || now.Sub(cs.notAttempts.windowStart) >= respawnCapWindow {
		cs.notAttempts = respawnWindow{count: 0, windowStart: now}
	}
	if cs.notAttempts.count >= respawnCap {
		return false
	}
	cs.notAttempts.count++
	return true
}

// boundAccountPhrase renders the bound account for an operator-facing audit Detail.
// ChatContext.FromLabel is empty for a legacy or unresolved binding, where naming a
// blank account reads worse than not naming one at all. (BOS-981)
func boundAccountPhrase(label string) string {
	if label == "" {
		return "the bound account"
	}
	return "bound account " + label
}

// notAttemptedCause renders the operator-facing cause of a pre-pane-touch refusal. The
// adapter wraps the cause behind ErrSwitchNotAttempted, whose text ("switch not
// attempted…") only restates the sentence the Detail already opens with, so strip that
// prefix and keep the part that actually names what went wrong. (BOS-981)
func notAttemptedCause(err error) string {
	msg := err.Error()
	if trimmed := strings.TrimPrefix(msg, ErrSwitchNotAttempted.Error()+": "); trimmed != msg {
		return trimmed
	}
	return msg
}

// scheduleReprobe arms (or re-arms) the single per-chat re-probe timer. Because
// SetAuthFailed is edge-triggered, a wedged pane dispatches rotateAuth only once; this
// timer re-drives the auth path after reprobeInterval so consecutive Healthy probes can
// accumulate to the respawn threshold. Deduped: an existing pending re-probe is
// cancelled first so a chat has at most one armed timer. (BOS-482)
func (r *ChatRotator) scheduleReprobe(agentSessionID string) {
	r.scheduleReprobeIn(agentSessionID, reprobeInterval)
}

// scheduleReprobeIn is scheduleReprobe with an explicit delay, so the BOS-980 latch can
// re-check a held episode at the end of its grace window instead of waiting a further
// reprobeInterval to learn whether the pane really recovered.
func (r *ChatRotator) scheduleReprobeIn(agentSessionID string, d time.Duration) {
	// Create the timer outside the lock (Schedule may take its own lock / start a
	// goroutine), then swap it in under the lock, cancelling any prior pending timer.
	cancel := r.deps.Schedule(d, func() { r.reprobeAuth(agentSessionID) })
	r.mu.Lock()
	cs := r.chatLocked(agentSessionID)
	if old := cs.reprobeCancel; old != nil {
		old()
	}
	cs.reprobeCancel = cancel
	r.mu.Unlock()
}

// reprobeAuth is the timer-driven re-entry into the auth path. Unlike OnAuthFailed it
// does NOT charge the reactive rate limiter (re-probing on the reprobeInterval cadence
// is the whole point); it takes only the inFlight guard so it can never run
// concurrently with a reactive rotation for the same chat. If a rotation is already in
// flight it silently drops — the next scheduled re-probe covers the gap. (BOS-482)
func (r *ChatRotator) reprobeAuth(agentSessionID string) {
	r.mu.Lock()
	cs := r.chatLocked(agentSessionID)
	if cs.inFlight {
		r.mu.Unlock()
		return
	}
	cs.inFlight = true
	r.mu.Unlock()
	r.active.Add(1)
	defer r.active.Add(-1)
	defer r.releaseInFlight(agentSessionID)
	defer r.finishAuthDecision(agentSessionID)
	r.rotateAuth(agentSessionID)
}

// clearRespawnState drops the consecutive-Healthy streak and cancels any pending
// re-probe timer for a chat. Called on recovery, on a confirmed-401 rotation, on a
// successful respawn, on a cap-exhausted stop, and on deregistration. The respawn cap
// window is deliberately NOT cleared here so the per-hour cap survives across streaks
// (a pane that respawns twice then recovers-and-re-wedges cannot escape the cap by
// bouncing). (BOS-482)
// reason is logged (BOS-980) so a reset streak is visible in the daemon log rather than
// inferable only from the absence of further healthy_streak lines.
func (r *ChatRotator) clearRespawnState(agentSessionID string, reason string) {
	r.mu.Lock()
	var streak healthyStreak
	var cancel func()
	if cs := r.lookupChatLocked(agentSessionID); cs != nil {
		streak, cs.healthy = cs.healthy, healthyStreak{}
		cancel, cs.reprobeCancel = cs.reprobeCancel, nil
		r.gcChatLocked(agentSessionID)
	}
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if streak.count > 0 || cancel != nil {
		r.deps.Logger.Info().
			Str("agent_session_id", agentSessionID).
			Int("healthy_streak_reset_from", streak.count).
			Int("healthy_streak_threshold", healthyRespawnThreshold).
			Bool("reprobe_cancelled", cancel != nil).
			Str("reason", reason).
			Msg("auto-rotate(auth): healthy streak reset; auth-failed episode closed")
	}
}

// holdEpisode reports whether an OPEN auth-failed episode should survive a reading that
// says the pane is clean (BOS-980). It returns false unless a healthy streak is actually
// open, so it changes nothing for a pane the healer never engaged.
//
// The first clean reading only starts the clock — it is held, and clearedAt records when.
// Subsequent clean readings are held until authClearGraceWindow has elapsed since that
// first one, after which the pane counts as sustainedly clear and the caller takes the
// ordinary RECOVERED path. markEpisodeLive resets clearedAt whenever the pane is observed
// auth-failed again, so a flapping pane never accumulates grace across troughs.
//
// heldFor is how long the grace window had already been running when this reading
// arrived — zero when no window was open. A false return with a NON-zero heldFor is a
// grace-window EXPIRY (the latch held and then let go), which the caller must be able to
// tell apart from "clean on the very first reading": whether authClearGraceWindow is
// sized correctly is only answerable from the log if those two look different.
func (r *ChatRotator) holdEpisode(agentSessionID string) (bool, time.Duration) {
	now := r.deps.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	cs := r.lookupChatLocked(agentSessionID)
	if cs == nil || cs.healthy.count == 0 {
		return false, 0
	}
	if cs.healthy.clearedAt.IsZero() {
		cs.healthy.clearedAt = now
		return true, 0
	}
	heldFor := now.Sub(cs.healthy.clearedAt)
	return heldFor < authClearGraceWindow, heldFor
}

// markEpisodeLive clears any pending grace-window clock for a chat whose pane has just
// been observed auth-failed. No-op when no streak is open. (BOS-980)
func (r *ChatRotator) markEpisodeLive(agentSessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cs := r.lookupChatLocked(agentSessionID)
	if cs == nil || cs.healthy.clearedAt.IsZero() {
		return
	}
	cs.healthy.clearedAt = time.Time{}
	r.gcChatLocked(agentSessionID)
}

// Deregister tears down all in-memory respawn state for a chat that is going away
// (pane removed / daemon draining): it cancels any pending re-probe and drops the
// streak AND the respawn-cap window. Safe to call for an unknown chat. (BOS-482)
func (r *ChatRotator) Deregister(agentSessionID string) {
	r.mu.Lock()
	var cancel func()
	if cs := r.lookupChatLocked(agentSessionID); cs != nil {
		cs.healthy = healthyStreak{}
		cs.respawns = respawnWindow{}
		cs.notAttempts = respawnWindow{}
		cs.proxyRepairPending = false
		cs.proxyRepairSettled = false
		cancel, cs.reprobeCancel = cs.reprobeCancel, nil
		// Only the lanes above are torn down here: an in-flight rotation's marker and
		// the rate-limit stamps stay, exactly as they did when each lane owned its own
		// map, so the entry survives until those lanes release too.
		r.gcChatLocked(agentSessionID)
	}
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
