// Package status provides an in-memory cache for chat status heartbeats
// reported by boss CLI clients. The daemon uses this to share process status
// across multiple CLI instances.
package status

import (
	"sync"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// StaleThreshold is how long since the last heartbeat before a chat is
// considered stale (and thus stopped). Set to 5x the 3s heartbeat interval.
const StaleThreshold = 15 * time.Second

// AuthFailedConsecutivePollsRequired is the number of consecutive poll ticks
// that must observe the login-required pane shape before AuthFailed reports it.
// This adds at most one poll interval of latency to a real auth failure, while
// preventing a one-tick compaction/capture gap from surfacing as terminal.
const AuthFailedConsecutivePollsRequired = 2

// Entry is a cached status heartbeat for a single Claude chat process.
type Entry struct {
	Status       pb.ChatStatus
	LastOutputAt time.Time
	ReceivedAt   time.Time
	// ResetAt is the usage-limit reset time, set only when Status is
	// CHAT_STATUS_LIMITED and the banner carried a parseable reset time.
	// Zero when unknown or the chat is not limited. Plumbed for the display
	// layer; human-readable rendering is BOS-167.
	ResetAt time.Time
}

// livenessMarker is the poller-derived liveness state for one chat (BOS-805).
// See Tracker.liveness for why it is not an Entry field.
type livenessMarker struct {
	// observedAt is when the poller last wrote any liveness reading for this
	// chat. It distinguishes "we looked and saw no substantive change" from
	// "nothing observed this chat during the window".
	observedAt time.Time
	// spinnerAt is when a live agent spinner was last observed on the chat's
	// pane. Zero means the last observation saw none.
	spinnerAt time.Time
	// substantiveAt is when the captured pane last changed in a way that was not
	// purely a spinner redraw. Zero when nothing is known.
	substantiveAt time.Time
	// seeded records that the chat's LastOutputAt (and substantiveAt) is a
	// placeholder stamped when the poller first saw the chat, not an observed
	// content change. It is what lets a consumer tell two chats registered in the
	// same poll tick — which share a timestamp to the nanosecond — from a real
	// collision of observations.
	seeded bool
}

// LivenessObservation is the raw poller-derived marker used by bounded
// observers that need to reason about whether a fresh poll tick landed.
type LivenessObservation struct {
	ObservedAt                time.Time
	LastSubstantiveOutputAt   time.Time
	LastSubstantiveOutputSeed bool
	Present                   bool
}

type authFailedMarker struct {
	observedAt  time.Time
	effectiveAt time.Time
	consecutive int
}

// Tracker is a thread-safe in-memory cache of chat process statuses.
type Tracker struct {
	mu      sync.RWMutex
	entries map[string]*Entry // claude_id -> entry

	// authFailed records, per agent session ID, the consecutive poll streak for
	// the login-required terminal shape ("Not logged in" / "Please run /login")
	// on the chat's pane. It is kept separate from entries because Update
	// recreates the Entry on every heartbeat (which would otherwise wipe the
	// marker); this map is only written by the poller's dedicated SetAuthFailed
	// path. A marker older than StaleThreshold is treated as absent so a chat
	// that logged back in (or died) stops flagging — fail toward NOT flagging.
	authFailed map[string]authFailedMarker // agent_session_id -> last observation streak

	// transientAPIError records, per agent session ID, the time the transient
	// API-failure terminal shape (an end-of-turn "API Error:" banner naming a
	// retryable 5xx / gateway-overload condition) was last observed on the
	// chat's pane. Like authFailed it lives outside entries because Update
	// recreates the Entry on every heartbeat, which would wipe a marker the
	// poller writes on its own cadence; only the dedicated SetTransientAPIError
	// path writes it. A marker older than StaleThreshold reads as absent so a
	// chat whose banner scrolled away (it retried, was resumed, or died) stops
	// flagging — fail toward NOT flagging, because a false positive resumes a
	// session that is legitimately blocked.
	transientAPIError map[string]time.Time // agent_session_id -> last observed

	// stalled records, per agent session ID, the time the poller last concluded
	// the chat is claimed-working while its agent has made no semantic progress
	// (no new transcript record) for longer than its phase's threshold (BOS-667).
	// It lives outside entries for the same reason authFailed does — Update
	// recreates the Entry on every heartbeat and would wipe a marker the poller
	// writes on its own cadence. A marker older than StaleThreshold reads as
	// absent, so a chat whose agent resumed (or died, or was reaped) stops
	// flagging without anyone having to clear it. This fails toward NOT flagging:
	// a false "your session is dead" banner on a healthy long build burns
	// operator trust faster than the missed detection does.
	stalled map[string]time.Time // agent_session_id -> last observed stalled

	// liveness holds, per agent session ID, the spinner-aware liveness signals the
	// tmux poller derives from each captured pane (BOS-805): whether a live
	// spinner is on screen, when the pane last changed in a way that was NOT
	// purely a spinner redraw, and whether the chat's LastOutputAt is a seeded
	// placeholder rather than an observed change.
	//
	// It lives outside entries for the same reason authFailed and stalled do:
	// update recreates the Entry on every heartbeat, which would wipe values the
	// poller writes on its own cadence. Keeping it out of Update's signature also
	// keeps the non-poller callers honest — ReportChatStatus (a CLI client), the
	// headless session lifecycle and the test harness all call Update with no
	// pane content in hand, and forcing them to supply a spinner verdict would
	// mean inventing one.
	//
	// Only spinnerAt carries StaleThreshold "reads as absent" semantics, because
	// only it asserts something about NOW. substantiveAt is a historical
	// observation whose whole point is that it may be old, and seeded describes
	// the provenance of a value that is served for as long as that value is.
	liveness map[string]*livenessMarker // agent_session_id -> derived liveness

	// waiting holds, per agent session ID, the canonical human-readable reason a
	// chat is parked on an external event rather than actively working
	// (BOS-668) — e.g. "awaiting checks_passed_ready on acme/widget#123". Empty
	// / absent means the chat is not waiting. It lives outside entries for the
	// same reason authFailed and stalled do: Update recreates the Entry on every
	// heartbeat and would wipe a marker written on its own cadence.
	//
	// Unlike those two it carries NO freshness stamp and no StaleThreshold gate.
	// The auth/transient/stalled markers are re-observed by the 3s tmux poller,
	// so a timestamp is what lets a marker nobody is re-asserting decay. This one
	// is derived event-driven inside DisplayStatusComputer.Recompute directly
	// from the callback store, and that derivation writes the CURRENT truth on
	// every pass — including writing "" the moment the callback drains. A
	// timestamp would add a second, slower way to clear a marker that already
	// clears itself, and would let a genuinely-still-parked chat silently stop
	// showing its reason merely because nothing recomputed for 15 seconds.
	waiting map[string]string // agent_session_id -> waiting reason

	// capturedOutput holds, per agent session ID, the bounded final tail the
	// tmux poller grabbed from a chat pane at process death before reaping it
	// (BOS-477). It is an ephemeral diagnostic — not durable state — read by the
	// DescribeChatLaunch RPC to surface a fast-exiting agent's own error in the
	// attach view. Guarded by mu like authFailed because the poller (writer) and
	// the server (reader) share the Tracker.
	capturedOutput map[string]string // agent_session_id -> bounded final tail

	// onUpdate, when non-nil, is invoked after every Update with the
	// claude_id whose status changed. The hook resolves claude_id →
	// sessionID and triggers DisplayStatusComputer.Recompute. Kept as a
	// loose function to avoid a status → db dependency on a concrete
	// resolver type, and to keep this package free of cross-package
	// imports for chat lookup.
	onUpdate func(agentSessionID string)

	// onLimitTransition, when non-nil, is fired exactly once when a chat enters
	// CHAT_STATUS_LIMITED and exactly once when it leaves. entered is true on the
	// enter transition (not-limited → limited) and false on the leave transition
	// (limited → not-limited); it never fires on same-limited-state repeated
	// polls. Wired in cmd/main.go to emit a structured audit log per transition;
	// the durable sink is Epic 4.4.
	onLimitTransition func(agentSessionID string, entered bool)

	// onAuthChange, when non-nil, is invoked after SetAuthFailed whenever the
	// chat's EFFECTIVE auth-failed state flips (absent/stale → fresh, or
	// present → cleared). It is separate from onUpdate because the auth-failed
	// overlay is an attention_status change that need not coincide with a chat
	// STATUS change — a WORKING chat can go login-required without Update ever
	// firing. The hook resolves agent_session_id → session and emits a
	// SessionDelta so the cloud/web read model (fed only by the reverse stream)
	// receives ATTENTION_REASON_AGENT_AUTH_FAILED. Fired only on a transition,
	// so the poller's per-tick SetAuthFailed calls don't storm the stream.
	onAuthChange func(agentSessionID string)

	// onTransientAPIErrorChange, when non-nil, is invoked after
	// SetTransientAPIError whenever the chat's EFFECTIVE transient-API-error
	// state flips (absent/stale → fresh, or present → cleared). It is separate
	// from onUpdate for the same reason onAuthChange is: a chat can go
	// transient-failed without its ChatStatus moving, so Update may never fire.
	// Gating on the transition is what makes the hook safe to wire to an action
	// (an auto-resume, an audit log) — the poller calls SetTransientAPIError on
	// EVERY tick, so an ungated hook would re-fire for as long as the banner
	// stays on screen.
	onTransientAPIErrorChange func(agentSessionID string)

	// onStalledChange, when non-nil, is invoked after SetStalled whenever the
	// chat's EFFECTIVE stalled state flips (absent/stale → fresh, or present →
	// cleared). Separate from onUpdate for the same reason onAuthChange is: a
	// stalled chat is by definition still reporting CHAT_STATUS_WORKING, so
	// Update never fires on the transition that matters. The hook resolves
	// agent_session_id → session and emits a SessionDelta so the cloud/web read
	// model receives ATTENTION_REASON_AGENT_STALLED. Fired only on a transition,
	// so the poller's per-tick SetStalled calls don't storm the stream.
	onStalledChange func(agentSessionID string)

	// onWaitingChange, when non-nil, is invoked after SetWaiting whenever the
	// chat's waiting REASON actually changes (absent → present, present →
	// different reason, present → cleared). Separate from onUpdate for the same
	// reason onStalledChange is: a parked chat still reports
	// CHAT_STATUS_WORKING, so Update never fires on the transition that matters.
	// The hook resolves agent_session_id → session and publishes a
	// ChatStatusDelta so the cloud/web read model sees the chat flip to
	// CHAT_STATUS_WAITING. Gating on the change is what keeps a PR that sits in
	// CI for forty minutes from emitting one delta per recompute.
	onWaitingChange func(agentSessionID string)
}

// NewTracker creates a new empty Tracker.
func NewTracker() *Tracker {
	return &Tracker{
		entries:           make(map[string]*Entry),
		authFailed:        make(map[string]authFailedMarker),
		transientAPIError: make(map[string]time.Time),
		stalled:           make(map[string]time.Time),
		liveness:          make(map[string]*livenessMarker),
		waiting:           make(map[string]string),
		capturedOutput:    make(map[string]string),
	}
}

// SetLiveness records the spinner-aware liveness signals the tmux poller derived
// from a chat's captured pane (BOS-805). Called once per tick per chat with the
// current verdict; the poller is the only writer.
//
// Unlike SetAuthFailed / SetStalled this fires no hook. Those markers gate an
// attention overlay a human must see, so a transition has to be published; these
// values are a liveness READING a caller polls for over get_chat_statuses, and
// spinnerPresent flips several times a minute on a healthy chat — publishing
// each flip would storm the stream with an oscillation nobody acts on.
func (t *Tracker) SetLiveness(agentSessionID string, spinnerPresent bool, lastSubstantiveOutputAt time.Time, lastOutputSeeded bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	m := &livenessMarker{observedAt: time.Now(), substantiveAt: lastSubstantiveOutputAt, seeded: lastOutputSeeded}
	if spinnerPresent {
		m.spinnerAt = m.observedAt
	}
	t.liveness[agentSessionID] = m
}

// Liveness returns the chat's spinner-aware liveness signals.
//
// spinnerPresent reads as false once the marker is staler than StaleThreshold,
// exactly like AuthFailed and Stalled: a spinner nobody is re-observing (the
// poller stopped, the chat died, the pane went quiet) is not evidence the agent
// is working now, and this fails toward NOT claiming liveness.
//
// lastSubstantiveOutputAt and lastOutputSeeded carry no freshness gate. The
// timestamp's whole purpose is to be allowed to age — that age IS the signal a
// caller gates on — and blanking it after fifteen seconds would erase precisely
// the reading that distinguishes a wedged chat from a working one. Both are
// reclaimed by Remove and Cleanup along with the chat's other state.
func (t *Tracker) Liveness(agentSessionID string) (spinnerPresent bool, lastSubstantiveOutputAt time.Time, lastOutputSeeded bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	m, ok := t.liveness[agentSessionID]
	if !ok {
		return false, time.Time{}, false
	}
	spinnerPresent = !m.spinnerAt.IsZero() && time.Since(m.spinnerAt) <= StaleThreshold
	return spinnerPresent, m.substantiveAt, m.seeded
}

// LivenessObservation returns the raw liveness marker for agentSessionID.
// Unlike Liveness, it does not freshness-gate spinnerAt or collapse absent into
// zero values, because its callers need to know whether the poller wrote a
// reading inside their observation window.
func (t *Tracker) LivenessObservation(agentSessionID string) LivenessObservation {
	t.mu.RLock()
	defer t.mu.RUnlock()
	m, ok := t.liveness[agentSessionID]
	if !ok {
		return LivenessObservation{}
	}
	return LivenessObservation{
		ObservedAt:                m.observedAt,
		LastSubstantiveOutputAt:   m.substantiveAt,
		LastSubstantiveOutputSeed: m.seeded,
		Present:                   true,
	}
}

// SetCapturedOutput stores (or, when tail is empty, clears) the bounded final
// output the poller captured from a chat pane at process death, keyed by agent
// session ID (BOS-477). Guarded by mu because the poller writes it while the
// server reads it.
func (t *Tracker) SetCapturedOutput(agentSessionID, tail string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if tail == "" {
		delete(t.capturedOutput, agentSessionID)
		return
	}
	t.capturedOutput[agentSessionID] = tail
}

// CapturedOutput returns the stored final tail for the chat, or "" when none
// was captured. Read by the DescribeChatLaunch RPC to surface a fast-exiting
// agent's own error in the attach diagnostic.
func (t *Tracker) CapturedOutput(agentSessionID string) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.capturedOutput[agentSessionID]
}

// Update upserts a heartbeat for the given claude ID. ResetAt is cleared —
// use UpdateLimited for the CHAT_STATUS_LIMITED path that carries a reset time.
func (t *Tracker) Update(agentSessionID string, status pb.ChatStatus, lastOutputAt time.Time) {
	t.update(agentSessionID, status, lastOutputAt, time.Time{})
}

// UpdateLimited upserts a CHAT_STATUS_LIMITED heartbeat carrying the usage-limit
// reset time (zero when the banner had no parseable reset). Separate from Update
// so the common status path stays a two-value call and only the limit path
// threads a reset time. The onUpdate hook fires on transitions into and out of
// LIMITED because those change Status.
func (t *Tracker) UpdateLimited(agentSessionID string, resetAt, lastOutputAt time.Time) {
	t.update(agentSessionID, pb.ChatStatus_CHAT_STATUS_LIMITED, lastOutputAt, resetAt)
}

func (t *Tracker) update(agentSessionID string, status pb.ChatStatus, lastOutputAt, resetAt time.Time) {
	t.mu.Lock()
	prev, hadPrev := t.entries[agentSessionID]
	t.entries[agentSessionID] = &Entry{
		Status:       status,
		LastOutputAt: lastOutputAt,
		ReceivedAt:   time.Now(),
		ResetAt:      resetAt,
	}
	hook := t.onUpdate
	limitHook := t.onLimitTransition
	wasLimited := hadPrev && prev.Status == pb.ChatStatus_CHAT_STATUS_LIMITED
	isLimited := status == pb.ChatStatus_CHAT_STATUS_LIMITED
	t.mu.Unlock()

	// Fire the hook only when the status actually changed — avoids burning
	// a recompute on every 3-second heartbeat when nothing's moved.
	if hook != nil && (!hadPrev || prev.Status != status) {
		hook(agentSessionID)
	}

	// Fire the limit-transition hook exactly once on the false→true flip and
	// once on the true→false flip; never on same-limited-state repeated polls.
	if limitHook != nil && wasLimited != isLimited {
		limitHook(agentSessionID, isLimited)
	}
}

// SetAuthFailed records or clears the login-required marker for a chat. When
// failed is true it stamps the current time and advances the consecutive
// observation streak; when false it clears any existing marker immediately (so a
// chat that logged back in stops flagging on the next poll tick). Called by the
// tmux poller every tick with the current pane state.
//
// It fires onAuthChange only when the EFFECTIVE auth-failed state flips — a
// consecutive marker becoming reportable, or a reportable marker being cleared.
// Because the poller calls this every tick, gating on the transition keeps the
// hook from re-emitting a SessionDelta on every poll while the state holds
// steady.
func (t *Tracker) SetAuthFailed(agentSessionID string, failed bool) {
	t.mu.Lock()
	prev, had := t.authFailed[agentSessionID]
	wasFresh := had && time.Since(prev.observedAt) <= StaleThreshold
	wasFailed := wasFresh && prev.consecutive >= AuthFailedConsecutivePollsRequired
	now := time.Now()
	nowFailed := false
	if failed {
		consecutive := 1
		effectiveAt := time.Time{}
		if wasFresh {
			consecutive = prev.consecutive + 1
			effectiveAt = prev.effectiveAt
		}
		if consecutive >= AuthFailedConsecutivePollsRequired && effectiveAt.IsZero() {
			effectiveAt = now
		}
		t.authFailed[agentSessionID] = authFailedMarker{observedAt: now, effectiveAt: effectiveAt, consecutive: consecutive}
		nowFailed = consecutive >= AuthFailedConsecutivePollsRequired
	} else {
		delete(t.authFailed, agentSessionID)
	}
	hook := t.onAuthChange
	shouldFire := (!failed && had && prev.consecutive >= AuthFailedConsecutivePollsRequired) || (failed && !wasFailed && nowFailed)
	t.mu.Unlock()

	if hook != nil && shouldFire {
		hook(agentSessionID)
	}
}

// AuthFailed reports whether the chat's pane has shown the login-required
// terminal shape on enough consecutive polls and the latest marker is fresh
// (observed within StaleThreshold). A stale marker — the poller stopped
// re-observing it (the chat logged in, its pane changed, or it died) — is
// treated as absent so the flag clears itself and never sticks. This fails
// toward NOT flagging.
func (t *Tracker) AuthFailed(agentSessionID string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	marker, ok := t.authFailed[agentSessionID]
	if !ok {
		return false
	}
	return marker.consecutive >= AuthFailedConsecutivePollsRequired && time.Since(marker.observedAt) <= StaleThreshold
}

// AuthFailedSince reports when the current fresh auth-failed episode became
// effective for the chat. It is stable across later poll observations in the
// same episode, unlike observedAt, which advances every tick.
func (t *Tracker) AuthFailedSince(agentSessionID string) (time.Time, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	marker, ok := t.authFailed[agentSessionID]
	if !ok || marker.consecutive < AuthFailedConsecutivePollsRequired || time.Since(marker.observedAt) > StaleThreshold {
		return time.Time{}, false
	}
	if marker.effectiveAt.IsZero() {
		return marker.observedAt, true
	}
	return marker.effectiveAt, true
}

// SetTransientAPIError records or clears the transient-API-failure marker for a
// chat. When present is true it stamps the current time; when false it clears
// any existing marker immediately (so a chat whose banner redrew away — it
// retried, or was resumed — stops flagging on the very next poll tick). Called
// by the tmux poller every tick with the current pane state.
//
// It fires onTransientAPIErrorChange only when the EFFECTIVE state flips — a
// fresh marker appearing where there was none (or only a stale one), or a fresh
// marker being cleared. The poller calls this once per tick per chat, so
// transition gating is what keeps a hook that takes an ACTION (auto-resume)
// from re-firing every three seconds for as long as the banner is on screen.
func (t *Tracker) SetTransientAPIError(agentSessionID string, present bool) {
	t.mu.Lock()
	prevAt, had := t.transientAPIError[agentSessionID]
	wasPresent := had && time.Since(prevAt) <= StaleThreshold
	if present {
		t.transientAPIError[agentSessionID] = time.Now()
	} else {
		delete(t.transientAPIError, agentSessionID)
	}
	hook := t.onTransientAPIErrorChange
	shouldFire := (!present && had) || (present && !wasPresent)
	t.mu.Unlock()

	if hook != nil && shouldFire {
		hook(agentSessionID)
	}
}

// TransientAPIError reports whether the chat's pane currently ends on a
// retryable API-failure banner and the marker is fresh (observed within
// StaleThreshold). A stale marker — the poller stopped re-observing it because
// the pane moved on, the chat was resumed, or it died — is treated as absent so
// the flag clears itself and never sticks. This fails toward NOT flagging: a
// missed transient failure costs one manual resume, a false one resumes a
// session that is legitimately blocked.
func (t *Tracker) TransientAPIError(agentSessionID string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	at, ok := t.transientAPIError[agentSessionID]
	if !ok {
		return false
	}
	return time.Since(at) <= StaleThreshold
}

// SetStalled records or clears the agent-stalled marker for a chat (BOS-667).
// When stalled is true it stamps the current time; when false it clears any
// existing marker immediately, so a chat whose agent resumed writing transcript
// records stops flagging on the very next poll tick. Called by the tmux poller
// every tick with the current progress-liveness verdict.
//
// It fires onStalledChange only when the EFFECTIVE stalled state flips — a fresh
// marker appearing where there was none (or only a stale one), or a fresh marker
// being cleared. Because the poller calls this every tick, gating on the
// transition is what keeps a 34-minute stall from emitting ~680 identical
// SessionDeltas. Exactly the SetAuthFailed shape.
func (t *Tracker) SetStalled(agentSessionID string, stalled bool) {
	t.mu.Lock()
	prevAt, had := t.stalled[agentSessionID]
	wasStalled := had && time.Since(prevAt) <= StaleThreshold
	if stalled {
		t.stalled[agentSessionID] = time.Now()
	} else {
		delete(t.stalled, agentSessionID)
	}
	hook := t.onStalledChange
	shouldFire := (!stalled && had) || (stalled && !wasStalled)
	t.mu.Unlock()

	if hook != nil && shouldFire {
		hook(agentSessionID)
	}
}

// Stalled reports whether the chat is currently claimed-working with no semantic
// agent progress past its phase's threshold, and the marker is fresh (observed
// within StaleThreshold). A stale marker — the poller stopped re-observing it
// because the agent resumed, the chat was reaped, or the runner stopped
// answering — is treated as absent so the flag clears itself and never sticks.
// This fails toward NOT flagging.
func (t *Tracker) Stalled(agentSessionID string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	at, ok := t.stalled[agentSessionID]
	if !ok {
		return false
	}
	return time.Since(at) <= StaleThreshold
}

// SetWaiting records (or, when reason is empty, clears) the reason a chat is
// parked on an external event rather than actively working (BOS-668). The
// reason is the canonical wording produced by displaystatus.CallbackWaitingReason
// — callers must not spell it themselves.
//
// It fires onWaitingChange only when the stored reason actually changes, which
// includes absent → present, one reason → a different reason, and present →
// cleared. The derivation calls this on every recompute, so gating on the change
// is what stops a long-running wait from emitting an identical delta per pass.
func (t *Tracker) SetWaiting(agentSessionID, reason string) {
	t.mu.Lock()
	prev := t.waiting[agentSessionID]
	if reason == "" {
		delete(t.waiting, agentSessionID)
	} else {
		t.waiting[agentSessionID] = reason
	}
	hook := t.onWaitingChange
	changed := prev != reason
	t.mu.Unlock()

	if hook != nil && changed {
		hook(agentSessionID)
	}
}

// Waiting returns the reason the chat is parked on an external event, or "" when
// it is not waiting. Unlike Stalled there is no freshness gate — see the waiting
// field's comment for why.
func (t *Tracker) Waiting(agentSessionID string) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.waiting[agentSessionID]
}

// PromoteWaiting is the single definition of the status a chat is SERVED with
// once the derivation layer has recorded (or cleared) its waiting reason. Every
// read surface — the chat/session status RPCs, the stream deltas and the
// snapshot reader — routes through it so the rule cannot drift between them.
//
// Waiting is strictly a refinement of WORKING: a reason on any other reported
// status is discarded rather than applied, so a marker left behind by a chat
// that has since asked a question or hit a usage limit can never mask a signal
// that demands human action.
func PromoteWaiting(reported pb.ChatStatus, reason string) (pb.ChatStatus, string) {
	if reason == "" {
		return reported, ""
	}
	switch reported {
	case pb.ChatStatus_CHAT_STATUS_WORKING, pb.ChatStatus_CHAT_STATUS_WAITING:
		return pb.ChatStatus_CHAT_STATUS_WAITING, reason
	default:
		return reported, ""
	}
}

// SetOnWaitingChange wires the callback fired when a chat's waiting reason
// changes. The wiring lives in cmd/main.go and publishes a ChatStatusDelta.
func (t *Tracker) SetOnWaitingChange(fn func(agentSessionID string)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onWaitingChange = fn
}

// SetOnUpdate wires a callback fired after Update when the chat's status
// changes. The wiring lives in cmd/main.go and resolves claude_id →
// sessionID before delegating to DisplayStatusComputer.Recompute. Tests
// usually leave this nil.
func (t *Tracker) SetOnUpdate(fn func(agentSessionID string)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onUpdate = fn
}

// SetOnLimitTransition wires a callback fired once when a chat enters
// CHAT_STATUS_LIMITED and once when it leaves. The wiring lives in cmd/main.go
// and emits a structured audit log per transition. Tests usually leave this nil.
func (t *Tracker) SetOnLimitTransition(fn func(agentSessionID string, entered bool)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onLimitTransition = fn
}

// SetOnAuthChange wires a callback fired after SetAuthFailed when the chat's
// effective auth-failed state flips. The wiring lives in cmd/main.go and
// resolves agent_session_id → session before emitting a SessionDelta carrying
// the AGENT_AUTH_FAILED overlay. Tests usually leave this nil.
func (t *Tracker) SetOnAuthChange(fn func(agentSessionID string)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onAuthChange = fn
}

// SetOnTransientAPIErrorChange wires a callback fired after
// SetTransientAPIError when the chat's effective transient-API-error state
// flips. The wiring lives in cmd/main.go, which resolves agent_session_id →
// session before acting on the transition. Tests usually leave this nil.
func (t *Tracker) SetOnTransientAPIErrorChange(fn func(agentSessionID string)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onTransientAPIErrorChange = fn
}

// SetOnStalledChange wires a callback fired after SetStalled when the chat's
// effective stalled state flips. The wiring lives in cmd/main.go and resolves
// agent_session_id → session before emitting a SessionDelta carrying the
// AGENT_STALLED overlay. Tests usually leave this nil.
func (t *Tracker) SetOnStalledChange(fn func(agentSessionID string)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onStalledChange = fn
}

// Get returns the cached entry for the given claude ID, or nil if not found
// or stale (older than StaleThreshold).
func (t *Tracker) Get(agentSessionID string) *Entry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.entries[agentSessionID]
	if !ok {
		return nil
	}
	if time.Since(e.ReceivedAt) > StaleThreshold {
		return nil
	}
	return e
}

// GetBatch returns entries for multiple claude IDs. Stale entries are
// returned as stopped.
func (t *Tracker) GetBatch(agentSessionIDs []string) map[string]*Entry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := make(map[string]*Entry, len(agentSessionIDs))
	now := time.Now()
	for _, id := range agentSessionIDs {
		e, ok := t.entries[id]
		if !ok {
			continue
		}
		if now.Sub(e.ReceivedAt) > StaleThreshold {
			result[id] = &Entry{
				Status:       pb.ChatStatus_CHAT_STATUS_STOPPED,
				LastOutputAt: e.LastOutputAt,
				ReceivedAt:   e.ReceivedAt,
			}
		} else {
			// Return a copy to prevent callers from mutating the cached entry.
			result[id] = &Entry{
				Status:       e.Status,
				LastOutputAt: e.LastOutputAt,
				ReceivedAt:   e.ReceivedAt,
				ResetAt:      e.ResetAt,
			}
		}
	}
	return result
}

// Snapshot returns a copy of every fresh (non-stale) entry, keyed by
// claude_id. Stale entries are filtered out — callers receive only chats
// whose tracker heartbeat is recent enough to trust. Used by the upstream
// stream's DaemonSnapshot path so a freshly-connected orchestrator inherits
// the daemon's current per-chat status without waiting for the next
// status transition (Update suppresses the OnUpdate hook on no-op
// heartbeats, so long-running chats whose state hasn't moved would
// otherwise be invisible until they next change).
func (t *Tracker) Snapshot() map[string]*Entry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	now := time.Now()
	out := make(map[string]*Entry, len(t.entries))
	for id, e := range t.entries {
		if now.Sub(e.ReceivedAt) > StaleThreshold {
			continue
		}
		out[id] = &Entry{
			Status:       e.Status,
			LastOutputAt: e.LastOutputAt,
			ReceivedAt:   e.ReceivedAt,
			ResetAt:      e.ResetAt,
		}
	}
	return out
}

// Remove deletes the entry for the given claude ID.
func (t *Tracker) Remove(agentSessionID string) {
	t.mu.Lock()
	authMarker, hadAuthMarker := t.authFailed[agentSessionID]
	_, hadTransientMarker := t.transientAPIError[agentSessionID]
	_, hadStalledMarker := t.stalled[agentSessionID]
	_, hadWaitingMarker := t.waiting[agentSessionID]
	delete(t.entries, agentSessionID)
	delete(t.authFailed, agentSessionID)
	delete(t.transientAPIError, agentSessionID)
	delete(t.stalled, agentSessionID)
	delete(t.liveness, agentSessionID)
	delete(t.waiting, agentSessionID)
	delete(t.capturedOutput, agentSessionID)
	hook := t.onAuthChange
	transientHook := t.onTransientAPIErrorChange
	stalledHook := t.onStalledChange
	waitingHook := t.onWaitingChange
	t.mu.Unlock()

	if hook != nil && hadAuthMarker && authMarker.consecutive >= AuthFailedConsecutivePollsRequired {
		hook(agentSessionID)
	}
	if transientHook != nil && hadTransientMarker {
		transientHook(agentSessionID)
	}
	if stalledHook != nil && hadStalledMarker {
		stalledHook(agentSessionID)
	}
	if waitingHook != nil && hadWaitingMarker {
		waitingHook(agentSessionID)
	}
}

// Cleanup removes all stale entries (older than StaleThreshold).
func (t *Tracker) Cleanup() {
	t.mu.Lock()
	now := time.Now()
	var clearedWaitingMarkers []string
	for id, e := range t.entries {
		if now.Sub(e.ReceivedAt) > StaleThreshold {
			delete(t.entries, id)
			// The captured tail (BOS-477) is set alongside the STOPPED heartbeat
			// at pane death, so once that entry goes stale its ephemeral
			// diagnostic is stale too — drop it here rather than leak it for the
			// daemon's lifetime.
			delete(t.capturedOutput, id)
			// The spinner-aware liveness reading (BOS-805) is collected here for
			// the same reason: its substantiveAt/seeded halves carry no clock of
			// their own by design, so a chat that stopped heartbeating would
			// otherwise leave them for the daemon's lifetime.
			delete(t.liveness, id)
			// The waiting reason (BOS-668) has no clock of its own, so this is
			// where it gets collected: a chat that stopped heartbeating is no
			// longer parked on anything, and nothing would recompute it to "".
			if _, hadWaiting := t.waiting[id]; hadWaiting {
				delete(t.waiting, id)
				clearedWaitingMarkers = append(clearedWaitingMarkers, id)
			}
		}
	}
	var clearedAuthMarkers []string
	for id, marker := range t.authFailed {
		if now.Sub(marker.observedAt) > StaleThreshold {
			delete(t.authFailed, id)
			if marker.consecutive >= AuthFailedConsecutivePollsRequired {
				clearedAuthMarkers = append(clearedAuthMarkers, id)
			}
		}
	}
	var clearedTransientMarkers []string
	for id, at := range t.transientAPIError {
		if now.Sub(at) > StaleThreshold {
			delete(t.transientAPIError, id)
			clearedTransientMarkers = append(clearedTransientMarkers, id)
		}
	}
	var clearedStalledMarkers []string
	for id, at := range t.stalled {
		if now.Sub(at) > StaleThreshold {
			delete(t.stalled, id)
			clearedStalledMarkers = append(clearedStalledMarkers, id)
		}
	}
	hook := t.onAuthChange
	transientHook := t.onTransientAPIErrorChange
	stalledHook := t.onStalledChange
	waitingHook := t.onWaitingChange
	t.mu.Unlock()

	if hook != nil {
		for _, id := range clearedAuthMarkers {
			hook(id)
		}
	}
	if waitingHook != nil {
		for _, id := range clearedWaitingMarkers {
			waitingHook(id)
		}
	}
	if transientHook != nil {
		for _, id := range clearedTransientMarkers {
			transientHook(id)
		}
	}
	if stalledHook != nil {
		for _, id := range clearedStalledMarkers {
			stalledHook(id)
		}
	}
}
