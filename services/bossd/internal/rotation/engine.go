// Package rotation implements the pure-logic policy engine that decides which
// registered account a session should switch to when a usage-cap or
// auth-invalidation signal fires. The engine does NOT perform the swap and does
// NOT own any storage: it is a deterministic function over an injected
// AccountRepository port. The real SQLite implementation of that port lives in
// the bossd db package; tests use an in-memory fake.
package rotation

import (
	"context"
	"fmt"
	"time"

	"github.com/recurser/bossalib/models"
	libtelemetry "github.com/recurser/bossalib/telemetry"
	daemontelemetry "github.com/recurser/bossd/internal/telemetry"
	"golang.org/x/sync/singleflight"
)

// defaultCooldownDuration is the fallback cooldown applied to a usage-limited
// account when the signal carries no explicit reset time.
const defaultCooldownDuration = 60 * time.Minute

// SignalKind distinguishes a quota cap from an auth invalidation.
type SignalKind int

const (
	// UsageLimited is a quota/usage cap that puts the account on a timed cooldown.
	UsageLimited SignalKind = iota
	// AuthInvalidated is an auth invalidation that fails the account's health
	// permanently (a persistent sink) until it is re-registered.
	AuthInvalidated
)

// Signal is the input describing a cap/invalidation event for one account.
type Signal struct {
	Provider        string
	CappedAccountID string
	Kind            SignalKind
	// ResetAt is the already-parsed cooldown expiry; nil means use the engine's
	// default cooldown. Only consulted for UsageLimited signals.
	ResetAt *time.Time
	// RotationCapable reports whether the provider/session can actually rotate.
	// When false the engine is status-only and performs no store access.
	RotationCapable bool
	// Utilization optionally supplies authoritative current usage per account,
	// probed outside the transaction. nil/empty preserves repository order unless
	// CandidateProbeRequired is true. Whenever this map is non-empty — or
	// CandidateProbeRequired is set — accounts with util >= 1.0 are skipped and
	// unprobed accounts are not selected; among the rest the winner is chosen
	// banded consume-first: soonest future weekly reset within the
	// MinRotationHeadroom band, else lowest utilization. See selectCandidate.
	Utilization map[string]float64
	// CandidateProbeRequired means a UsageLimited dispatcher already relied on an
	// authoritative probe for the capped account, so candidate selection must not
	// fall back to an unprobed account when candidate probes are unavailable.
	CandidateProbeRequired bool
	// SuppressCooldown tells the engine to rotate WITHOUT benching the capped
	// account: skip the cooldown write only, leaving candidate selection and
	// health exactly as they would otherwise be. It exists for the
	// dispatcher that saw a cap-shaped signal it could not confirm — a transient
	// upstream 429, or a 429 raised while the authoritative usage probe was itself
	// unreachable (BOS-584). Benching on an unverifiable signal costs a healthy
	// account a full DefaultCooldown, so those callers rotate the request away and
	// leave the account selectable. Only consulted for UsageLimited signals; the
	// resulting Outcome reports CooldownApplied: false because no cooldown was in
	// fact applied.
	//
	// It CAN change the outcome kind, in one case. Kind is resolved after the
	// write, from the account list, and minFutureCooldown only sees rows with a
	// future cooldown_until. So when the capped account is the last selectable
	// one for its provider — the single-account deployment — the cooldown this
	// signal did not write is also the cooldown that would have produced
	// OutcomeAllExhausted, and the result is OutcomeNoEligibleAccount instead.
	// That is the truthful answer (nothing is cooling, and there is genuinely no
	// alternate), but callers that map the kind to operator-facing advice must
	// account for it — session.recordNonRotateAudit does.
	SuppressCooldown bool
	// SuppressHealthFail tells the engine to select a rotation target for an
	// AuthInvalidated signal WITHOUT recording the permanent health failure on the
	// capped account: skip the FailHealth write only, leaving candidate selection
	// exactly as it would otherwise be. Only consulted for AuthInvalidated signals.
	//
	// It exists for the respawn-in-place healer (BOS-981), which reaches this engine
	// only AFTER an authoritative probe classified the bound account HEALTHY. That
	// caller rotates because the bound account is administratively ineligible to host
	// a respawn (disabled / cooling / already health-failed), not because its auth is
	// broken, so writing health = failed would both contradict the probe and outlive
	// the operator's remedy — re-enabling an account restores status, never health.
	// The confirmed-401 caller leaves this false and still benches the account.
	//
	// Like SuppressCooldown it CAN change the outcome kind. Candidate selection is
	// unchanged, but Kind is resolved after the write from the account list, and
	// minFutureCooldown only counts selectable (Active + HealthOK) rows. So a bound
	// account that is merely COOLING stays selectable here where the unsuppressed
	// path would have benched it out of consideration: with no other eligible
	// account the result is OutcomeAllExhausted ("all accounts cooling until …")
	// rather than OutcomeNoEligibleAccount. That is still truthful — the account
	// really does become selectable again at ResumeAt — but callers that map the
	// kind to operator-facing advice must account for it.
	//
	// Like SuppressCooldown it is part of the single-flight key (see Decide), so a
	// suppressing and a non-suppressing signal for the same account can never
	// coalesce onto one another's verdict.
	SuppressHealthFail bool
}

// OutcomeKind classifies the engine's decision.
type OutcomeKind int

const (
	// OutcomeRotate means NextAccount is set to the account to switch to.
	OutcomeRotate OutcomeKind = iota
	// OutcomeAllExhausted means no account is currently available but at least
	// one is merely cooling; ResumeAt holds the earliest future recovery time.
	OutcomeAllExhausted
	// OutcomeStatusOnly means the agent/provider cannot rotate at all: the
	// capability short-circuit fired (!sig.RotationCapable), so no store access
	// happened and no swap target was even considered. The remedy is agent/plugin
	// side, not account side. Contrast OutcomeNoEligibleAccount below.
	OutcomeStatusOnly
	// OutcomeNoEligibleAccount means rotation IS capable and the store was
	// consulted, but no account is eligible to switch to now and none will recover
	// by cooling. The dominant cause is that every other account is disabled or
	// permanently failed; on the probe-required path it can also mean no other
	// account has a usable (probeable, under-cap) candidate slot. Distinct from
	// OutcomeStatusOnly so operators are steered to the account-side remedy —
	// typically enabling or re-authenticating an account (`boss account update
	// <id> --status active`) — rather than the agent's rotation capability
	// (BOS-327).
	OutcomeNoEligibleAccount
)

// Outcome is the engine's decision for a single Signal.
type Outcome struct {
	Kind            OutcomeKind
	CappedAccountID string
	// NextAccount is set iff Kind == OutcomeRotate. It is ALWAYS derived from the
	// current committed provider state, never replayed from a prior decision (the
	// engine is stateless). So on a duplicate/late signal (CooldownApplied ==
	// false) NextAccount reflects the provider as it stands now: if a different
	// account was independently cooled since the original signal, the target may
	// legitimately differ from the first decision's — and a now-cooling account is
	// never returned (returning the stale original target would hand the caller a
	// capped account). Callers MUST therefore gate an actual rotation on
	// CooldownApplied, not on NextAccount alone, so a redelivered cap event cannot
	// drive a second rotation.
	NextAccount *models.Account
	// ResumeAt is set iff Kind == OutcomeAllExhausted.
	ResumeAt time.Time
	// CooldownApplied reports whether THIS decision wrote a cooldown. It is false
	// in five cases, and a caller that reads it as "someone else already handled
	// this cap" must be sure it can only see the first:
	//   - the cap signal was a duplicate (the account was already cooling);
	//   - the signal was an auth invalidation (health is failed, not cooled);
	//   - the cap was unbound, with no account to cool (BOS-320);
	//   - the dispatcher could not confirm the cap and set SuppressCooldown
	//     (BOS-584), so the account is deliberately left selectable;
	//   - the signal was not RotationCapable, short-circuiting to OutcomeStatusOnly
	//     before any store access.
	// The headless initial-cap path (session.attemptUsageLimitRotation) does read
	// it as the duplicate signal, and is safe for three independent reasons: it
	// reads the flag only inside case OutcomeRotate, which excludes the
	// not-capable producer outright (that one short-circuits to
	// OutcomeStatusOnly before any store access); it only ever sends
	// Kind: UsageLimited, so it never sees the auth-invalidation producer — note
	// that producer is NOT excluded by OutcomeRotate, since decide's
	// AuthInvalidated case falls through to the shared selectCandidate and so
	// pairs CooldownApplied: false with a non-nil NextAccount; and it never sends
	// an unbound or suppressed signal itself, nor can it inherit another caller's
	// suppression, because SuppressCooldown is part of the single-flight key.
	// Keep all three true if you add a sixth producer of false.
	//
	// It is a per-decision flag: concurrent callers whose identical signals
	// coalesce via single-flight all receive the winning decision's value, so this
	// flag is best-effort — the authoritative single-apply invariant is that
	// exactly one cooldown row write reaches the store (guaranteed by
	// SetCooldownIfNotCooling).
	CooldownApplied bool
}

// AccountRepository is the narrow persistence port the engine declares
// (dependency inversion). The db package provides the real SQLite
// implementation; tests use a fake.
type AccountRepository interface {
	// DecideTx runs fn inside ONE transaction, giving fn a tx-scoped view plus
	// guarded writer. A crash/error before commit leaves NO cooldown/health
	// change written (atomic rollback).
	DecideTx(ctx context.Context, provider string, fn func(tx TxAccountView) error) error
}

// TxAccountView is the transactional view handed to the decide closure.
type TxAccountView interface {
	// ListByProvider returns all accounts for the provider (any health/status),
	// ordered priority ASC -> health-rank ASC (ok before failed) -> weekly-expiry
	// rank (accounts whose usage_reset_7d is a known instant strictly after now
	// sort first, soonest-reset first; nil/past resets fall through) -> last_used_at
	// ASC (NULLS FIRST) -> id ASC. The weekly-expiry rank uses the same
	// future-only rule as FutureWeeklyReset, keeping this SQL surface, the
	// bind-time comparator (account.moreEligible), and the test fake in sync
	// (BOS-429). now is threaded in so the "strictly future" cut is evaluated
	// against the caller's clock.
	ListByProvider(ctx context.Context, now time.Time) ([]*models.Account, error)
	// SetCooldownIfNotCooling sets cooldown_until on accountID ONLY when it is
	// not already cooling (cooldown_until IS NULL OR <= now). Returns
	// applied=false when the row was already cooling (idempotency gate).
	SetCooldownIfNotCooling(ctx context.Context, accountID string, until, now time.Time) (applied bool, err error)
	// FailHealth sets health=failed on accountID (persistent auth-invalidated sink).
	FailHealth(ctx context.Context, accountID string) error
}

// Engine decides account rotation. It holds no rotation state of its own: every
// decision is derived from the repository, so a fresh Engine over the same store
// behaves identically to a long-lived one (restart-safe).
type Engine struct {
	accounts        AccountRepository
	defaultCooldown time.Duration
	group           singleflight.Group
	clock           func() time.Time
	telemetry       libtelemetry.Client
}

type decisionResult struct {
	outcome Outcome
}

func WithTelemetry(client libtelemetry.Client) Option {
	return func(e *Engine) { e.telemetry = client }
}

// CaptureProactiveRotation records a successful pre-cap switch. Candidate
// selection alone is not a rotation, so the caller invokes this only after the
// chat switch succeeds.
func (e *Engine) CaptureProactiveRotation(ctx context.Context, provider string) {
	daemontelemetry.Capture(ctx, e.telemetry, libtelemetry.EventAccountRotated, map[string]any{
		"rotation_reason": "proactive", "provider": telemetryProvider(provider), "status": "rotated",
	})
}

// CaptureReactiveRotation records a successful reactive account switch. The
// caller invokes it only after the account has been applied.
func (e *Engine) CaptureReactiveRotation(ctx context.Context, provider, reason string) {
	if reason == "" {
		reason = "usage_limit"
	}
	daemontelemetry.Capture(ctx, e.telemetry, libtelemetry.EventAccountRotated, map[string]any{
		"rotation_reason": reason, "provider": telemetryProvider(provider), "status": "rotated",
	})
}

func telemetryProvider(provider string) string {
	switch provider {
	case "claude", "codex", "opencode":
		return provider
	default:
		return "other"
	}
}

// Option configures an Engine.
type Option func(*Engine)

// WithDefaultCooldown overrides the fallback cooldown used when a UsageLimited
// signal carries no ResetAt. Non-positive durations are ignored (the 60m
// default is kept).
func WithDefaultCooldown(d time.Duration) Option {
	return func(e *Engine) {
		if d > 0 {
			e.defaultCooldown = d
		}
	}
}

// WithClock overrides the engine's time source (for deterministic tests). A nil
// clock is ignored.
func WithClock(clock func() time.Time) Option {
	return func(e *Engine) {
		if clock != nil {
			e.clock = clock
		}
	}
}

// NewEngine builds an Engine over the given repository. Defaults: 60m cooldown,
// time.Now clock.
func NewEngine(accounts AccountRepository, opts ...Option) *Engine {
	e := &Engine{
		accounts:        accounts,
		defaultCooldown: defaultCooldownDuration,
		clock:           time.Now,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Decide computes the rotation outcome for sig. It is safe for concurrent use:
// identical signals for the same (provider, capped account, kind, suppression)
// are coalesced via single-flight so a burst of duplicate signals collapses to
// one execution, and the persistent cooldown guard makes any late duplicate a
// no-op write.
//
// The single-flight key is deliberately the (provider, capped account, kind,
// SuppressCooldown) tuple, NOT the provider alone: coalescing distinct capped
// accounts on one provider would drop the second account's cooldown and hand its
// caller the first account's decision. Cross-account serialization is instead
// provided by the single SQLite transaction in DecideTx (SQLite serializes
// writers), so concurrent caps on different accounts each get their own cooldown
// write.
//
// SuppressCooldown is part of the key because it selects a DIFFERENT write, not
// a duplicate of the same one (BOS-584). Two signals that disagree about whether
// the account may be benched must not coalesce: a suppressing signal winning
// would hand a non-suppressing caller CooldownApplied=false, and the headless
// initial-cap path reads that as "a duplicate the engine already handled" and
// skips its restart (see session.attemptUsageLimitRotation) — stalling the
// session instead of rotating it. A non-suppressing signal winning would bench
// an account whose probe said it was healthy, i.e. the very bug this suppression
// exists to prevent. Keying on it makes each caller's own verdict authoritative
// for its own outcome; genuine duplicates still coalesce because they agree.
//
// Serialization gives each cap its own committed write but NOT cross-signal
// visibility: with concurrent caps for distinct accounts A and B, A's
// transaction commits before B's is processed, so A's selection sees B still
// healthy and may return B moments before B's own cap commits. That is a
// bounded, self-correcting transient inherent to asynchronous cap arrival, not
// a lost or incorrect write — B's cooldown is still applied exactly once, and a
// session mis-targeted onto B simply re-caps and re-rotates off it, so the
// eventual state is correct. The engine is a pure function over COMMITTED store
// state and deliberately holds no registry of in-flight signals; excluding an
// account whose cap is still in-flight is impossible here by transaction
// isolation (an uncommitted write is invisible) and belongs to the dispatcher
// that owns signal arrival, not this stateless engine.
func (e *Engine) Decide(ctx context.Context, sig Signal) (Outcome, error) {
	// Capability short-circuit: no capability => status-only, no store access.
	if !sig.RotationCapable {
		return Outcome{Kind: OutcomeStatusOnly, CappedAccountID: sig.CappedAccountID}, nil
	}

	key := fmt.Sprintf("%s\x00%s\x00%d\x00%t\x00%t",
		sig.Provider, sig.CappedAccountID, sig.Kind, sig.SuppressCooldown, sig.SuppressHealthFail)
	r, err, _ := e.group.Do(key, func() (any, error) {
		out, err := e.decide(ctx, sig)
		return &decisionResult{outcome: out}, err
	})
	if err != nil {
		return Outcome{}, err
	}
	result, ok := r.(*decisionResult)
	if !ok {
		return Outcome{}, fmt.Errorf("rotation: singleflight returned unexpected type %T", r)
	}
	return result.outcome, nil
}

// SelectProactiveCandidate returns the account eligible to receive a proactive
// pre-cap rotation off boundAccountID, or nil when none qualifies. Unlike Decide
// it is READ-ONLY: the bound account is not exhausted, so no cooldown or health
// change is ever written. Selection reuses the same util-aware predicate as
// reactive rotation (probed util < 1.0, active, healthy, not cooling, not the
// bound account) and the same banded consume-first winner rule — pre-empting a
// cap is exactly when moving onto perishable quota pays off (BOS-830). An empty
// utilization map yields nil because a proactive switch must never target an
// unprobed account. (BOS-318)
//
// Caller interaction worth knowing about: SweepProactive does NOT switch onto
// whatever this returns — it applies its own hysteresis gate on top, which a
// banded winner can fail where the old lowest-utilization winner would have
// cleared it. That gate and its consequences are documented where its constants
// live, beside proactiveHysteresis in chat_rotator.go; deliberately not restated
// here, so a retune cannot leave a stale worked example in this file.
func (e *Engine) SelectProactiveCandidate(ctx context.Context, provider, boundAccountID string, utilization map[string]float64) (*models.Account, error) {
	now := e.clock()
	var chosen *models.Account
	err := e.accounts.DecideTx(ctx, provider, func(tx TxAccountView) error {
		accts, err := tx.ListByProvider(ctx, now)
		if err != nil {
			return fmt.Errorf("list accounts: %w", err)
		}
		chosen = selectCandidate(accts, boundAccountID, now, utilization, true)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("proactive select tx: %w", err)
	}
	return chosen, nil
}

// decide runs the transactional core of a rotation decision.
func (e *Engine) decide(ctx context.Context, sig Signal) (Outcome, error) {
	now := e.clock()
	out := Outcome{CappedAccountID: sig.CappedAccountID}
	var accts []*models.Account

	err := e.accounts.DecideTx(ctx, sig.Provider, func(tx TxAccountView) error {
		switch sig.Kind {
		case AuthInvalidated:
			// BOS-981: a caller that is rotating off an ineligible-but-not-broken
			// account suppresses this write; see Signal.SuppressHealthFail.
			if !sig.SuppressHealthFail {
				if err := tx.FailHealth(ctx, sig.CappedAccountID); err != nil {
					return fmt.Errorf("fail health: %w", err)
				}
			}
			out.CooldownApplied = false
		default: // UsageLimited
			switch {
			case sig.CappedAccountID == "":
				// Unbound cap (BOS-320): an unbound session has no bound account to
				// cool — select a rotation target only, write no cooldown.
				out.CooldownApplied = false
			case sig.SuppressCooldown:
				// Unconfirmed cap (BOS-584): rotate the request away but leave the
				// account selectable. Deliberately skips ONLY this write — the
				// candidate selection below still runs. SuppressCooldown is part of
				// the single-flight key (see Decide), so a suppressing and a
				// non-suppressing signal for the same account never coalesce onto one
				// another's verdict.
				out.CooldownApplied = false
			default:
				until := now.Add(e.defaultCooldown)
				if sig.ResetAt != nil {
					until = *sig.ResetAt
				}
				applied, err := tx.SetCooldownIfNotCooling(ctx, sig.CappedAccountID, until, now)
				if err != nil {
					return fmt.Errorf("set cooldown: %w", err)
				}
				out.CooldownApplied = applied
			}
		}

		list, err := tx.ListByProvider(ctx, now)
		if err != nil {
			return fmt.Errorf("list accounts: %w", err)
		}
		accts = list
		out.NextAccount = selectCandidate(accts, sig.CappedAccountID, now, sig.Utilization, sig.CandidateProbeRequired)
		return nil
	})
	if err != nil {
		return Outcome{}, fmt.Errorf("rotation decide tx: %w", err)
	}

	// Resolve the outcome kind AFTER the write commits, from the account list.
	if out.NextAccount != nil {
		out.Kind = OutcomeRotate
		return out, nil
	}
	if resumeAt, ok := minFutureCooldown(accts, now); ok {
		out.Kind = OutcomeAllExhausted
		out.ResumeAt = resumeAt
		return out, nil
	}
	// No candidate and none recovering: rotation was capable and the store was
	// consulted, so this is "no eligible account" (all disabled/failed), NOT the
	// capability short-circuit's OutcomeStatusOnly (BOS-327).
	out.Kind = OutcomeNoEligibleAccount
	return out, nil
}

// isSelectable reports whether an account is eligible to run: active and
// healthy. It is the single source of the eligibility predicate shared by
// selectCandidate (who can rotate in now) and minFutureCooldown (who will be
// selectable once a cooldown expires), so the two never drift.
func isSelectable(a *models.Account) bool {
	return a.Status == models.AccountStatusActive && a.Health == models.AccountHealthOK
}

// selectCandidate returns the first account (list is pre-ordered) that is
// active, healthy, not cooling, and not the capped account.
//
// When utilization is available the winner is picked BANDED CONSUME-FIRST
// (BOS-830). Eligibility is unchanged — selectable, probed, and under
// UtilizationCapped — but among those candidates:
//
//   - candidates with at least MinRotationHeadroom left form the band, and the
//     band's winner is the one whose weekly quota expires soonest, because that
//     quota is perishable and rolls over unspent otherwise;
//   - otherwise — the band is empty (every candidate is nearly exhausted), or no
//     in-band candidate has a known future reset to rank by — selection falls back
//     to the lowest-utilization rule, so rotation still lands on the account with
//     the most room rather than one that would re-cap within minutes.
//
// Utilization therefore decides whenever there is no urgency signal, and never
// otherwise: inside a rankable band the whole point is to prefer the perishable
// account even though it is the more utilized one. Note that ANY known future
// reset outranks an unknown one however distant it is — an unknown reset is not
// "far away", it is no information, and the account carrying it is reachable
// again on the next probe.
//
// Ordering inside the band goes through the shared FutureWeeklyReset predicate,
// the same one account.moreEligible and the SQL ListByProvider rank use, so the
// three surfaces cannot drift (BOS-429). Comparisons are strictly-better, so
// equal resets keep the incoming list order (priority → health → weekly-expiry →
// LRU → id) and selection stays reproducible across restarts.
func selectCandidate(accts []*models.Account, cappedID string, now time.Time, utilization map[string]float64, probeRequired bool) *models.Account {
	if probeRequired || len(utilization) > 0 {
		var (
			banded      *models.Account
			bandedReset time.Time
			bandedKnown bool
			leastUsed   *models.Account
			leastUtil   float64
		)
		for _, a := range accts {
			if !candidateSelectable(a, cappedID, now) {
				continue
			}
			util, ok := utilization[a.ID]
			if !ok {
				continue
			}
			if UtilizationCapped(util) {
				continue
			}
			if leastUsed == nil || LowerUtilization(util, leastUtil) {
				leastUsed = a
				leastUtil = util
			}
			if !HasRotationHeadroom(util) {
				continue
			}
			reset, known := weeklyReset(a, now)
			if banded == nil || soonerWeeklyReset(reset, known, bandedReset, bandedKnown) {
				banded, bandedReset, bandedKnown = a, reset, known
			}
		}
		// Consume-first only earns its keep when there is perishable quota to
		// reclaim, so it needs a KNOWN future reset to rank the band by. Where no
		// in-band candidate has one there is no urgency signal at all, and falling
		// through to lowest utilization keeps today's rule rather than silently
		// demoting it to bare list order.
		if banded != nil && bandedKnown {
			return banded
		}
		return leastUsed
	}
	for _, a := range accts {
		if candidateSelectable(a, cappedID, now) {
			return a
		}
	}
	return nil
}

// weeklyReset nil-safely resolves an account's weekly-quota reset instant
// through the shared FutureWeeklyReset predicate: ok=false covers a never-probed
// account (nil Usage), a never-observed reset (nil Reset7d), and an
// already-rolled one (a past reset is a fresh full week, the opposite of urgent).
func weeklyReset(a *models.Account, now time.Time) (time.Time, bool) {
	if a.Usage == nil {
		return time.Time{}, false
	}
	return FutureWeeklyReset(a.Usage.Reset7d, now)
}

// soonerWeeklyReset reports whether (reset, known) is STRICTLY more urgent than
// the incumbent (bestReset, bestKnown): a known future reset beats an unknown
// one, and between two known resets the earlier instant wins. Equal urgency
// returns false so the incumbent — the earlier list-order candidate — is kept.
func soonerWeeklyReset(reset time.Time, known bool, bestReset time.Time, bestKnown bool) bool {
	if known != bestKnown {
		return known
	}
	if !known {
		return false
	}
	return reset.Before(bestReset)
}

func candidateSelectable(a *models.Account, cappedID string, now time.Time) bool {
	if a.ID == cappedID {
		return false
	}
	if !isSelectable(a) {
		return false
	}
	return a.CooldownUntil == nil || !a.CooldownUntil.After(now)
}

// minFutureCooldown returns the earliest cooldown_until strictly after now over
// the accounts that will actually be selectable when that cooldown expires
// (active ∧ healthy), and whether any such future cooldown exists. A cooling
// account that is also failed or disabled is NOT a genuine recovery, so it is
// excluded — otherwise ResumeAt could point at a slot that stays unusable.
func minFutureCooldown(accts []*models.Account, now time.Time) (time.Time, bool) {
	var earliest time.Time
	found := false
	for _, a := range accts {
		if !isSelectable(a) {
			continue
		}
		if a.CooldownUntil == nil || !a.CooldownUntil.After(now) {
			continue
		}
		if !found || a.CooldownUntil.Before(earliest) {
			earliest = *a.CooldownUntil
			found = true
		}
	}
	return earliest, found
}
