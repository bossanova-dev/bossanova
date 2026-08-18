// Package resume contains bossd's bounded auto-resume consumer for chats that
// stalled on a transient (retryable) API failure. It is trigger + policy glue
// only: pane lifecycle stays owned by the chat tracker and the prompt delivery
// primitive (Server.SendChatMessage), which the bossd wiring adapts into the
// Deliver seam. Fail-safe throughout — any error, any missing seam, and any
// unknown state means do nothing, leaving the chat exactly as a human would
// find it.
package resume

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/safego"
)

// ResumeMessage is the verbatim prompt delivered on every auto-resume attempt.
// It is deliberately a plain instruction rather than a slash command: the target
// chat may be any agent runner, and the only thing this consumer knows is that a
// turn ended without finishing. The wording steers the agent back to its
// committed state instead of restarting finished work, because the pane's own
// scrollback is the only record of how far the interrupted turn got.
//
// The parenthetical says "connection lost mid-response" rather than naming a
// status code. Two of the three stalls this lane fires on never had one: a
// connection-loss banner carries no code (BOS-889), and a stream severed by a
// daemon restart (BOS-890) never rendered a banner at all. This is also the
// exact text verified by hand during the BOS-890 investigation, which recovered
// 3/3 stalled panes — so it is what an agent has actually been observed to act
// on correctly, not a plausible rewrite.
const ResumeMessage = "Transient API error detected (connection lost mid-response). Resume the interrupted work: continue from committed state; do not restart completed work."

const (
	// DefaultSettleWindow is the wait before the FIRST attempt, and also the
	// minimum quiet period a pane must show before an attempt lands. A banner can
	// appear while the agent is still retrying internally; waiting two minutes
	// means the common self-healing case never sees a prompt at all.
	DefaultSettleWindow = 2 * time.Minute
	// DefaultBackoffBase is the backoff unit between retries. Retries double it, so
	// the whole cycle is over inside ten minutes — long enough to ride out a
	// provider blip, short enough that an operator watching the pane sees the
	// daemon give up rather than nag forever.
	DefaultBackoffBase = 1 * time.Minute
	// MaxAttempts bounds auto-resume attempts per banner cycle. Three is the point
	// past which the failure is no longer plausibly transient; continuing would
	// just be an unbounded prompt loop against a wedged chat.
	MaxAttempts = 3
	// DefaultRefillCooldown is how long a chat must go WITHOUT a charged attempt
	// before it earns a fresh MaxAttempts budget.
	//
	// Without it, MaxAttempts is not a bound at all. Delivering a resume prompt is
	// itself pane output: the prompt and the agent's reply push the banner out of
	// the detector's tail window within seconds, the marker clears, and a
	// clear-means-reset rule would hand the chat three more attempts — so a
	// sustained upstream outage becomes "3 prompts, scroll, 3 more prompts,
	// forever", at a rate set by nothing more principled than pane scroll timing.
	// Making the refill depend on ELAPSED QUIET instead means the cap holds inside
	// any one window, while a genuinely new incident hours later still gets a full
	// budget. 30m is ~3.5x the full attempt ladder (banner +2m/+4m/+8m), so it
	// cannot be satisfied by the ladder's own gaps.
	DefaultRefillCooldown = 30 * time.Minute
	// MaxGateRechecks bounds how many times ONE banner cycle may re-check a chat
	// whose gates transiently said "not now" (auth marker set, tracker entry not
	// fresh, chat momentarily not IDLE).
	//
	// Re-checking at all is required, not optional. The tracker hook fires only on
	// an EFFECTIVE marker flip, and the poller re-stamps the marker every tick for
	// as long as the banner is on screen — so while a chat is genuinely stalled the
	// marker never goes stale and never flips again. A gate that abandoned the cycle
	// outright would therefore have no re-entry at all: one unlucky read (a chat
	// that reads WORKING for a beat, a tracker entry that just aged out) would
	// silently forfeit recovery for a pane that is still showing the 5xx banner and
	// goes IDLE seconds later.
	//
	// It is bounded because the opposite failure is a chat that is legitimately
	// never going to be IDLE — one parked on a QUESTION for hours — being re-polled
	// (and re-hitting the session-resumable lookup) forever. Three re-checks spans
	// ~6 minutes of settle windows, long enough to ride out a status flap and short
	// enough that a chat owned by a human or another lane is left alone.
	MaxGateRechecks = 3
)

// gateTimeout bounds the two seams that can touch I/O (the session-resumable
// lookup and the prompt delivery). A hung delivery must not pin the timer
// goroutine, and a resume that takes longer than this is no longer timely.
const gateTimeout = 30 * time.Second

// TransientResumerDeps carries the injected seams. Every one is nil-guarded: a
// daemon built without the capability must stay healthy and simply never resume,
// so a missing seam degrades the resumer to a no-op rather than panicking.
type TransientResumerDeps struct {
	Logger zerolog.Logger
	// MarkerSet reports whether the chat's pane STILL shows the transient banner.
	MarkerSet func(agentSessionID string) bool
	// AuthFailed reports the competing auth marker — the rotation lane wins.
	AuthFailed func(agentSessionID string) bool
	// ChatState returns the chat's current status and last-output time; ok=false
	// when the tracker has no fresh entry (unknown ⇒ never fire).
	ChatState func(agentSessionID string) (status bossanovav1.ChatStatus, lastOutputAt time.Time, ok bool)
	// ChatLiveness reports the SPINNER-INSENSITIVE substantive-output stamp for a
	// chat (status.Tracker.Liveness, BOS-805) plus whether that stamp is still
	// only a bootstrap SEED rather than a real observation. ok=false when the
	// tracker holds no liveness reading at all.
	//
	// It exists because ChatState's LastOutputAt cannot answer the restart lane's
	// question. That stamp moves on ANY pane repaint — a cosmetic spinner frame
	// included — and on a restart it is SEEDED to a synthetic past time by the
	// poller's Bootstrap, so its value relative to a severance stamp says nothing
	// about whether the chat produced output. This seam distinguishes both cases:
	// the stamp ignores spinner-only redraws, and the seeded flag says "the
	// poller has observed nothing real yet", which is exactly the state a chat
	// left parked by a severed stream is in.
	//
	// Deliberately NOT part of capable(): a daemon that never wires it keeps the
	// banner lane (BOS-518) working in full and merely never resumes a
	// restart-severed chat, which is the fail-safe direction. Making it mandatory
	// would let one unwired seam silently switch the whole lane off, including
	// the older trigger that does not need it.
	ChatLiveness func(agentSessionID string) (lastSubstantiveOutputAt time.Time, lastOutputSeeded bool, ok bool)
	// SessionResumable reports whether the owning session is still live (not
	// archived, not stopped/closed). false ⇒ never fire.
	SessionResumable func(ctx context.Context, agentSessionID string) bool
	// Deliver sends the resume prompt into the chat's live agent (the bossd
	// wiring adapts Server.SendChatMessage with wake_if_asleep + submit).
	Deliver func(ctx context.Context, agentSessionID, message string) error
	// Now is the clock seam. nil ⇒ time.Now.
	Now func() time.Time
	// Schedule runs f after d and returns a cancel func: the injectable timer seam
	// for the settle/backoff scheduler. nil ⇒ a time.AfterFunc wrapper; tests
	// inject a controllable fake so no test ever sleeps.
	Schedule func(d time.Duration, f func()) (cancel func())
	// SettleWindow overrides DefaultSettleWindow. 0 ⇒ DefaultSettleWindow.
	SettleWindow time.Duration
	// BackoffBase overrides DefaultBackoffBase. 0 ⇒ DefaultBackoffBase.
	BackoffBase time.Duration
	// RefillCooldown overrides DefaultRefillCooldown. 0 ⇒ DefaultRefillCooldown.
	RefillCooldown time.Duration
}

// timerSlot is one armed attempt for a chat. The pointer identity is what lets a
// fired callback tell "I am the timer this chat is currently waiting on" from "I
// was cancelled/superseded and fired anyway" — a cancel func is best-effort, so
// the callback must be able to no-op itself.
type timerSlot struct {
	cancel    func()
	cancelled bool
}

// TransientResumer auto-resumes chats whose turn ended without finishing. It has
// two triggers and ONE cycle: status.Tracker's transient-API-error transition
// hook, for a pane that ends on a retryable API-failure banner (BOS-518), and
// OnStreamSevered, for a chat whose proxied stream was cut by this daemon's own
// death (BOS-890). Both are bounded on every axis: at most one armed timer per
// chat, at most MaxAttempts deliveries per cycle, and a re-check of every gate
// against LIVE state immediately before each delivery.
//
// The blast radius of a false positive is deliberately tiny: one extra prompt in
// an idle chat, which the agent can simply answer. That asymmetry — a missed
// detection costs a human a manual resume, a false one costs a no-op prompt — is
// what justifies acting automatically at all.
type TransientResumer struct {
	deps TransientResumerDeps

	mu sync.Mutex
	// pending holds the single armed timer per chat. Its presence is also the
	// dedupe key: the tracker hook can re-fire (marker cleared then re-set) and
	// must not stack a second cycle on top of a live one.
	pending map[string]*timerSlot
	// attempts counts deliveries charged against the chat's current budget. It is
	// refilled only by elapsed quiet (see expireBudget) — neither a suppression
	// nor a marker clear is evidence the chat recovered, so neither may hand it a
	// fresh budget on its own.
	attempts map[string]int
	// lastAttemptAt is when the most recent attempt was charged, the clock the
	// refill cooldown is measured from.
	lastAttemptAt map[string]time.Time
	// rechecks counts consecutive gate re-checks in the current cycle (see
	// MaxGateRechecks). It is cleared by anything that ends or advances the cycle:
	// a delivered attempt, a cleared marker, or Deregister.
	rechecks map[string]int
	// severedAt marks a cycle as RESTART-SOURCED (BOS-890) and stamps when this
	// daemon learned the stream was cut. Its presence is what lets stillStalled
	// substitute a pane-independent predicate for the pane marker on this one
	// source; its absence keeps every banner-sourced cycle on the original
	// MarkerSet gate byte for byte. Cleared wherever a cycle ends, so a chat can
	// never carry a stale severance into a later banner incident.
	severedAt map[string]time.Time
}

// NewTransientResumer builds a TransientResumer from its dependency seams,
// filling the clock, timer and tuning defaults.
func NewTransientResumer(deps TransientResumerDeps) *TransientResumer {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Schedule == nil {
		logger := deps.Logger
		deps.Schedule = func(d time.Duration, f func()) func() {
			// time.AfterFunc runs f on its own goroutine, and f reaches tmux through
			// Deliver — an unrecovered panic there would take the whole daemon down.
			// safego.Go is the repo convention for exactly this (ChatRotator dispatches
			// its work bodies the same way). Tests inject their own synchronous
			// Schedule, so this indirection is production-only.
			t := time.AfterFunc(d, func() { safego.Go(logger, f) })
			return func() { t.Stop() }
		}
	}
	return &TransientResumer{
		deps:          deps,
		pending:       map[string]*timerSlot{},
		attempts:      map[string]int{},
		lastAttemptAt: map[string]time.Time{},
		rechecks:      map[string]int{},
		severedAt:     map[string]time.Time{},
	}
}

// OnTransientAPIError is chained into status.Tracker's transient-API-error
// transition hook, which fires only on an effective state flip. Like the
// rotation triggers it must return fast: every gate that needs I/O is deferred to
// the scheduled attempt, so this entry point only touches in-memory state and
// arms a timer.
func (r *TransientResumer) OnTransientAPIError(agentSessionID string) {
	if !r.capable() {
		return // no capability wired ⇒ inert, never scheduling work it cannot finish
	}
	if !r.deps.MarkerSet(agentSessionID) {
		// The CLEAR half of the transition: the pane produced output that scrolled
		// the banner away. Cancel the cycle — but do NOT read that as recovery. A
		// delivered resume prompt is itself pane output, so during a sustained
		// outage this edge arrives seconds after every attempt; refilling here is
		// what would turn MaxAttempts into an unbounded prompt loop. The budget is
		// refilled by elapsed quiet alone.
		r.stop(agentSessionID)
		r.clearRechecks(agentSessionID)
		r.clearSevered(agentSessionID)
		r.expireBudget(agentSessionID)
		return
	}
	// A quiet chat earns its budget back here too, so a genuinely new incident
	// hours later starts from a full ladder even without an intervening clear.
	r.expireBudget(agentSessionID)
	// A cycle is already armed for this chat: a re-fired transition must not
	// double-schedule (that would compress the backoff and burn the budget early).
	r.mu.Lock()
	_, armed := r.pending[agentSessionID]
	r.mu.Unlock()
	if armed {
		return
	}
	r.arm(agentSessionID, r.settleWindow())
}

// OnStreamSevered is the RESTART-SOURCED trigger (BOS-890): the daemon has just
// come back up and found, in the in-flight stream record the previous process
// left behind, that this chat was mid-response when that process died. The
// failover proxy is in-process, so the death cut the stream — but unlike a 5xx
// the pane may show nothing at all, because the REPL was still waiting on bytes
// that will never arrive. There is therefore no marker to flip and no poller
// tick that will ever produce one; this call is the only evidence that exists.
//
// It joins the SAME cycle as the banner trigger — same settle window, same
// gates, same attempt budget, same backoff ladder. The only difference is which
// predicate answers "is this chat still stalled?" at fire time (see
// stillStalled), because the pane marker cannot answer for a stall that never
// rendered.
//
// That one difference changes how far the ladder is USUALLY walked, and the
// distinction matters enough to state: the restart lane's predicate is "has this
// chat produced substantive output since the severance?", and a prompt that
// LANDS is itself substantive output. So a successful delivery is the very
// evidence that ends the cycle, and the normal restart-lane outcome is exactly
// ONE prompt — the ladder is never walked, because there is nothing left to
// nag. The budget and the backoff rungs are reached only when a delivery did NOT
// land (Deliver errored, so the pane never changed and the chat is still parked
// where the severed stream left it), which is precisely when retrying is right.
// The banner lane behaves differently for the same reason inverted: its marker
// is re-stamped by the poller for as long as the banner is on screen, so a
// delivery that does not fix anything leaves the gate satisfied and the ladder
// runs to MaxAttempts.
//
// Callers should invoke this AFTER the startup pane-token re-adoption sweep, so
// a pane that is about to reconnect on its own has already been given its token
// back before the settle window starts running. Unlike the poller-bootstrap
// ordering (which stillStalled no longer depends on), this one is a preference
// rather than a correctness requirement: marking early would only mean the
// settle window starts a few seconds sooner, and every gate is re-evaluated
// against live state at fire time anyway.
func (r *TransientResumer) OnStreamSevered(agentSessionID string) {
	if agentSessionID == "" || !r.capable() {
		return
	}
	// A restart is by definition a long quiet period, so any budget spent by the
	// previous process's incident has expired; this refills it when it has.
	r.expireBudget(agentSessionID)

	// Stamp and claim in ONE lock acquisition (armStamped). Stamping first and
	// arming after would leave a window in which a banner trigger claims the slot
	// in between: this call would then bail, having already written a
	// restart-severance stamp onto a cycle it does not own. That cycle would keep
	// the stamp for its whole life and fall back to the pane-independent
	// predicate the moment its marker cleared — loosening a gate for a chat that
	// had real on-screen evidence, which is the wrong direction for a lane that
	// must fail toward NOT resuming. Whoever claims the slot owns the predicate.
	r.armStamped(agentSessionID, r.settleWindow(), r.deps.Now())
}

// stillStalled answers the first gate of every attempt: is this chat STILL in
// the state that armed the cycle? It is the one place BOS-890 touches the gate
// ladder, and it strictly widens nothing for the pre-existing source.
//
// For a banner-sourced cycle the answer is the live pane marker, exactly as
// before — the marker is re-stamped by the poller every tick a banner is on
// screen, so a set marker is current evidence, not a memory.
//
// A restart-sourced cycle has no such marker and never will, so it needs a
// pane-independent substitute: has the poller OBSERVED any real output since it
// restored this pane? Output it observed — the agent's own retry succeeding, a
// human typing, another lane acting — ends the cycle untouched, which is what
// keeps a chat that recovered under this daemon's watch from being prompted.
//
// Note the limit precisely, because it is narrower than "since the stream was
// severed". The severance happened when the PREVIOUS daemon died, and the gap
// between that death and this poller's Bootstrap is unbounded — a reboot, an
// upgrade, an overnight stop. Anything that happened inside that gap left no
// trace this process can read: Bootstrap captures the pane in whatever state it
// is already in and seeds it, so a chat that recovered before we started
// looking is indistinguishable from one still parked mid-turn, and it will
// receive one resume prompt. That cost is chosen, not overlooked. The
// alternative is the pre-fix behaviour, where the lane never fired at all for
// this ordering — the bug this ticket exists to fix. One spurious prompt to an
// already-recovered chat, telling it to continue from committed state, is the
// deliberately accepted price of the lane firing at all.
//
// The evidence is ChatLiveness, not ChatState.LastOutputAt, because that stamp
// answers a different question and gets this one wrong twice over:
//
//   - It advances on ANY pane repaint, a cosmetic spinner frame included, so one
//     spinner redraw during the settle window would read as "recovered" and end
//     the cycle before a single prompt was ever delivered.
//   - After a restart it is not an observation at all. The poller's Bootstrap
//     SEEDS every restored pane with a synthetic `now - IdleThreshold - 1s`, and
//     bootstrap runs after the severance is marked — so on any workspace where
//     the startup work between the two takes longer than that margin, the
//     synthetic seed lands AFTER the severance stamp and every severed chat
//     reads as recovered. The feature would no-op under exactly the load that
//     makes a restart likely.
//
// Liveness fixes both: its stamp ignores spinner-only redraws, and lastOutputSeeded
// says outright "this is still the bootstrap placeholder, nothing real has been
// observed". A seeded stamp therefore reads as STILL STALLED whatever its value,
// which removes the ordering dependency between the severance mark and the
// poller's bootstrap entirely rather than leaving it as an unenforced contract.
//
// Unknown state (no seam, no liveness reading, a zero unseeded stamp) reads as
// NOT stalled, so every uncertainty still falls toward leaving the chat alone.
func (r *TransientResumer) stillStalled(agentSessionID string) bool {
	if r.deps.MarkerSet(agentSessionID) {
		return true
	}
	r.mu.Lock()
	severedAt, restartSourced := r.severedAt[agentSessionID]
	r.mu.Unlock()
	if !restartSourced || severedAt.IsZero() {
		return false
	}
	if r.deps.ChatLiveness == nil {
		return false
	}
	lastSubstantiveAt, seeded, ok := r.deps.ChatLiveness(agentSessionID)
	if !ok {
		return false
	}
	if seeded {
		// The poller has observed no substantive output since it restored this
		// pane, so the only "output" on record is its own placeholder. A synthetic
		// seed is not evidence of recovery, whichever side of severedAt it fell on.
		//
		// This says nothing about the window BEFORE Bootstrap ran, which is where
		// the honest limit of this predicate sits: a chat that recovered between
		// the previous daemon's death and this poller's first look is captured by
		// Bootstrap already recovered, marked seeded all the same, and reads as
		// still stalled here — so it gets one prompt. Accepted, per the trade-off
		// spelled out on this function: the alternative was a lane that never
		// fired for this ordering at all.
		return true
	}
	if lastSubstantiveAt.IsZero() {
		return false
	}
	return !lastSubstantiveAt.After(severedAt)
}

// clearSevered drops a chat's restart-severance stamp. Called wherever a cycle
// ends, so the next cycle for that chat is classified by its own trigger rather
// than inheriting this one's relaxed predicate.
func (r *TransientResumer) clearSevered(agentSessionID string) {
	r.mu.Lock()
	delete(r.severedAt, agentSessionID)
	r.mu.Unlock()
}

// Deregister cancels any pending timer and drops all in-memory state for a chat
// that is going away (its row was deleted / the daemon is draining). Safe to call
// for an unknown chat.
func (r *TransientResumer) Deregister(agentSessionID string) {
	r.reset(agentSessionID)
}

// arm schedules the next attempt for a chat, deduped to one armed timer. The slot
// is reserved under the lock BEFORE Schedule is called so two concurrent
// triggers cannot both arm; Schedule itself runs outside the lock because it may
// take its own lock or start a goroutine.
func (r *TransientResumer) arm(agentSessionID string, d time.Duration) {
	r.armStamped(agentSessionID, d, time.Time{})
}

// armStamped is arm plus the restart-severance stamp, written under the SAME
// lock acquisition that claims the pending slot. A zero severedStamp writes
// nothing, which is every non-restart caller. Coupling the two is what makes
// "the trigger that armed the cycle chooses the stalled-predicate" an invariant
// rather than a race the callers happen to usually win.
func (r *TransientResumer) armStamped(agentSessionID string, d time.Duration, severedStamp time.Time) {
	slot := &timerSlot{}
	r.mu.Lock()
	if _, exists := r.pending[agentSessionID]; exists {
		r.mu.Unlock()
		return
	}
	r.pending[agentSessionID] = slot
	if !severedStamp.IsZero() {
		r.severedAt[agentSessionID] = severedStamp
	}
	r.mu.Unlock()

	cancel := r.deps.Schedule(d, func() { r.attempt(agentSessionID, slot) })

	r.mu.Lock()
	if slot.cancelled {
		// Cancelled (Deregister / marker clear) while Schedule was in flight: stop
		// the timer we just created rather than leaking it.
		r.mu.Unlock()
		cancel()
		return
	}
	slot.cancel = cancel
	r.mu.Unlock()
}

// claim drops the pending handle for a fired timer and reports whether the timer
// is still the one the chat is waiting on. A cancel func is best-effort — a fake
// or racing timer can fire after cancellation — so this identity check, not the
// cancel, is what guarantees a superseded timer delivers nothing.
func (r *TransientResumer) claim(agentSessionID string, slot *timerSlot) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending[agentSessionID] != slot {
		return false
	}
	delete(r.pending, agentSessionID)
	return true
}

// stop cancels any pending timer for a chat WITHOUT clearing its attempt count.
// Used for suppressions: the chat did not demonstrably recover, so its spent
// budget must survive until the marker actually clears.
func (r *TransientResumer) stop(agentSessionID string) {
	r.mu.Lock()
	slot := r.pending[agentSessionID]
	delete(r.pending, agentSessionID)
	var cancel func()
	if slot != nil {
		slot.cancelled = true
		cancel = slot.cancel
	}
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// reset cancels any pending timer AND unconditionally drops all state for a
// chat, returning it to pristine. Only for chats that are GOING AWAY
// (Deregister): an unconditional refill is safe when there is no longer a chat
// to prompt, and it is what keeps the maps from growing with dead chats.
func (r *TransientResumer) reset(agentSessionID string) {
	r.stop(agentSessionID)
	r.mu.Lock()
	delete(r.attempts, agentSessionID)
	delete(r.lastAttemptAt, agentSessionID)
	delete(r.rechecks, agentSessionID)
	delete(r.severedAt, agentSessionID)
	r.mu.Unlock()
}

// clearRechecks drops a chat's gate-recheck count, used wherever the cycle ends
// or genuinely advances so the next incident starts from a full allowance.
func (r *TransientResumer) clearRechecks(agentSessionID string) {
	r.mu.Lock()
	delete(r.rechecks, agentSessionID)
	r.mu.Unlock()
}

// gateNotYet handles a gate that said "not now" but may well say yes shortly: it
// re-arms the cycle one settle window out, up to MaxGateRechecks times, and
// reports whether the cycle survives. Nothing is charged — no prompt was
// delivered, so no attempt may be spent (that budget belongs to deliveries).
//
// This is deliberately not a stop(): after claim() the fired timer has already
// surrendered the chat's pending slot, so there is nothing of ours left to
// cancel, and cancelling would only reach a NEWER cycle armed by a racing
// marker flip.
func (r *TransientResumer) gateNotYet(log zerolog.Logger, agentSessionID, reason string) {
	r.mu.Lock()
	n := r.rechecks[agentSessionID] + 1
	if n > MaxGateRechecks {
		delete(r.rechecks, agentSessionID)
		delete(r.severedAt, agentSessionID)
		r.mu.Unlock()
		log.Debug().Str("reason", reason).Int("max_rechecks", MaxGateRechecks).
			Msg("auto-resume: gate still unsatisfied after max re-checks; abandoning cycle")
		return
	}
	r.rechecks[agentSessionID] = n
	r.mu.Unlock()

	d := r.settleWindow()
	log.Debug().Str("reason", reason).Int("recheck", n).Dur("recheck_in", d).
		Msg("auto-resume: gate not satisfied; re-checking")
	r.arm(agentSessionID, d)
}

// expireBudget refills a chat's attempt budget IFF the refill cooldown has
// elapsed since its last charged attempt, dropping the bookkeeping entirely so
// the maps do not accumulate long-quiet chats. A chat that has never been
// charged has nothing to expire and is left alone.
func (r *TransientResumer) expireBudget(agentSessionID string) {
	now := r.deps.Now() // outside the lock: the clock seam is caller-supplied
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, charged := r.lastAttemptAt[agentSessionID]; charged && now.Sub(last) < r.refillCooldown() {
		return // still inside the cooldown: the spent budget stands
	}
	delete(r.attempts, agentSessionID)
	delete(r.lastAttemptAt, agentSessionID)
}

// attempt is the timer-driven body: re-check every gate against LIVE state, then
// either deliver, reschedule, or abandon the cycle. Re-checking here rather than
// trusting the state observed at arming time is the whole point of the settle
// window — minutes have passed and the chat has very likely moved on.
func (r *TransientResumer) attempt(agentSessionID string, slot *timerSlot) {
	if !r.claim(agentSessionID, slot) {
		return // superseded or cancelled: this timer no longer speaks for the chat
	}
	if !r.capable() {
		return
	}
	log := r.deps.Logger.With().Str("agent_session_id", agentSessionID).Logger()

	if !r.stillStalled(agentSessionID) {
		// Recovered while we waited — the common case, and the reason the settle
		// window exists. Treat it exactly like the clear transition. For a
		// banner-sourced cycle this IS the marker check it always was; for a
		// restart-sourced one it is the pane-independent substitute (see
		// stillStalled), and either way "recovered" ends the cycle silently.
		log.Debug().Msg("auto-resume: chat is no longer stalled before the attempt; abandoning cycle")
		r.clearRechecks(agentSessionID)
		r.clearSevered(agentSessionID)
		r.expireBudget(agentSessionID)
		return
	}
	if r.deps.AuthFailed != nil && r.deps.AuthFailed(agentSessionID) {
		// The rotation lane owns an auth-failed pane: it will stop/rebind/respawn the
		// chat, and a resume prompt typed into a pane that is about to be replaced is
		// noise at best. Yield — but keep re-checking, because rotation may clear the
		// auth marker and hand back a chat still sitting on the 5xx banner.
		r.gateNotYet(log, agentSessionID, "chat is auth-failed; deferring to rotation")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), gateTimeout)
	defer cancel()

	if !r.deps.SessionResumable(ctx, agentSessionID) {
		// Archived / stopped / closed session: resuming it would resurrect work a
		// human deliberately ended.
		// Archived / stopped / closed sessions do not come back, so unlike the other
		// gates this one is genuinely terminal: re-checking would poll forever.
		log.Debug().Msg("auto-resume: session is not resumable; abandoning cycle")
		r.clearRechecks(agentSessionID)
		r.clearSevered(agentSessionID)
		return
	}
	st, lastOutputAt, ok := r.deps.ChatState(agentSessionID)
	if !ok {
		// No fresh tracker entry: the daemon has no current evidence about this pane
		// at all, and an unknown pane is never prompted. The poller usually restores
		// one within a tick or two, so re-check rather than forfeit the cycle.
		r.gateNotYet(log, agentSessionID, "no fresh chat state")
		return
	}
	if st != bossanovav1.ChatStatus_CHAT_STATUS_IDLE {
		// IDLE-only, by design. WORKING means the turn resumed itself; QUESTION means
		// a human owes the chat an answer; LIMITED belongs to the rotation lane; and
		// STOPPED has no live agent to deliver into. Prompting any of them is at best
		// wasted and at worst destroys a pending human decision.
		//
		// Bounded re-checking, not abandonment: status is a poll-derived observation
		// that can flap, and a chat still showing the banner will not produce another
		// marker flip to bring us back.
		r.gateNotYet(log.With().Stringer("status", st).Logger(), agentSessionID, "chat is not idle")
		return
	}
	settle := r.settleWindow()
	if idle := r.deps.Now().Sub(lastOutputAt); idle < settle {
		// The pane went idle only recently (the banner may have landed mid-retry).
		// Wait out the remainder instead of delivering, and do NOT charge an attempt:
		// nothing was tried, so nothing should be spent.
		remaining := settle - idle
		log.Debug().Dur("remaining", remaining).Msg("auto-resume: pane not settled yet; rescheduling")
		r.arm(agentSessionID, remaining)
		return
	}

	n, ok := r.charge(agentSessionID)
	if !ok {
		// Defensive: the scheduler never arms a timer past the budget, so reaching
		// here means state drifted. Go quiet loudly.
		log.Warn().Int("max_attempts", MaxAttempts).
			Msg("auto-resume: attempt budget already exhausted; not resuming")
		r.clearSevered(agentSessionID)
		return
	}

	// The attempt is charged BEFORE the delivery result is known, so a delivery
	// error still consumes budget: a chat whose prompt can never be delivered must
	// not retry forever, and the retry ladder is the same either way.
	if err := r.deps.Deliver(ctx, agentSessionID, ResumeMessage); err != nil {
		log.Warn().Err(err).Int("attempt", n).
			Msg("auto-resume: resume prompt delivery failed; attempt charged")
	} else {
		log.Info().Int("attempt", n).Int("max_attempts", MaxAttempts).
			Msg("auto-resume: delivered resume prompt after transient API error")
	}

	if n < MaxAttempts {
		// Exponential ladder off the settle window: with the defaults, attempts land
		// at T+2m, T+4m and T+8m after the banner was first seen.
		r.arm(agentSessionID, r.backoffBase()<<n)
		return
	}
	// Terminal: no more timers. This log IS the escalation signal — a chat that
	// burned every attempt needs a human, and nothing else will say so.
	r.clearSevered(agentSessionID)
	log.Warn().Int("attempt", n).Int("max_attempts", MaxAttempts).
		Msg("auto-resume: max attempts reached; chat needs manual attention")
}

// charge records one attempt against the chat's budget, returning the attempt
// number and whether it was within MaxAttempts.
func (r *TransientResumer) charge(agentSessionID string) (int, bool) {
	now := r.deps.Now() // outside the lock: the clock seam is caller-supplied
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.attempts[agentSessionID] + 1
	if n > MaxAttempts {
		return n, false
	}
	r.attempts[agentSessionID] = n
	r.lastAttemptAt[agentSessionID] = now
	// Every gate passed, so the re-check allowance has served its purpose; the
	// backoff ladder bounds what happens from here.
	delete(r.rechecks, agentSessionID)
	return n, true
}

// capable reports whether every seam required to reach a delivery is wired. A
// daemon missing any of them can never legitimately resume, so it must not arm
// timers or evaluate gates at all.
func (r *TransientResumer) capable() bool {
	return r.deps.MarkerSet != nil &&
		r.deps.ChatState != nil &&
		r.deps.SessionResumable != nil &&
		r.deps.Deliver != nil
}

func (r *TransientResumer) settleWindow() time.Duration {
	if r.deps.SettleWindow > 0 {
		return r.deps.SettleWindow
	}
	return DefaultSettleWindow
}

func (r *TransientResumer) backoffBase() time.Duration {
	if r.deps.BackoffBase > 0 {
		return r.deps.BackoffBase
	}
	return DefaultBackoffBase
}

func (r *TransientResumer) refillCooldown() time.Duration {
	if r.deps.RefillCooldown > 0 {
		return r.deps.RefillCooldown
	}
	return DefaultRefillCooldown
}
