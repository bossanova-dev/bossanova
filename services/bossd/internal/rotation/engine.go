// Package rotation implements the pure-logic policy engine that decides which
// registered account a session should switch to when a usage-cap or
// auth-invalidation signal fires. The engine does NOT perform the swap and does
// NOT own any storage: it is a deterministic function over an injected
// AgentAccountRepository port. The real SQLite implementation of that port lives in
// the bossd db package; tests use an in-memory fake.
package rotation

import (
	"context"
	"fmt"
	"time"

	"github.com/recurser/bossd/internal/domain"

	"github.com/recurser/bossalib/kit/config"
	libtelemetry "github.com/recurser/bossalib/kit/telemetry"
	"github.com/recurser/bossd/internal/db"
	daemontelemetry "github.com/recurser/bossd/internal/domain/telemetry"
	"golang.org/x/sync/singleflight"
)

// defaultCooldownDuration is the fallback cooldown applied to a usage-limited
// account when the signal carries no explicit reset time.
const defaultCooldownDuration = 60 * time.Minute

// SignalKind is the shared rotation signal discriminator.
type SignalKind = domain.SignalKind

const (
	UsageLimited    = domain.UsageLimited
	AuthInvalidated = domain.AuthInvalidated
)

// Signal is the input describing a cap/invalidation event for one account.
type Signal struct {
	Model           string
	Provider        string
	CappedAccountID string
	Kind            SignalKind
	// ResetAt is the already-parsed cooldown expiry; nil means use the engine's
	// default cooldown. Only consulted for UsageLimited signals.
	ResetAt *time.Time
	// RotationCapable reports whether the provider/session can actually rotate.
	// When false the engine is status-only and performs no store access.
	RotationCapable bool
	// CandidateUsage holds each candidate's live probe snapshot, authoritative
	// over stored usage for ranking and the five-hour threshold. Probes run
	// outside the transaction. nil/empty preserves repository order unless
	// CandidateProbeRequired is true. Whenever this map is non-empty — or
	// CandidateProbeRequired is set — accounts with util >= 1.0 are skipped and
	// unprobed accounts are not selected; among the rest the winner is chosen
	// priority, then consume-first within the MinRotationHeadroom band. Fresh
	// accounts over the configured 5h threshold are excluded. See selectCandidate.
	CandidateUsage map[string]domain.UsageSnapshot
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
	// OutcomeAllExhausted, and the result is OutcomeNoEligibleAgentAccount instead.
	// That is the truthful answer (nothing is cooling, and there is genuinely no
	// alternate), but callers that map the kind to operator-facing advice must
	// account for it — session.recordNonRotateAudit does.
	SuppressCooldown bool
	// SuppressHealthFail tells the engine to select a rotation target for an
	// AuthInvalidated signal WITHOUT recording the permanent health failure on the
	// capped account: skip the SetHealthFailed write only, leaving candidate selection
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
	// rather than OutcomeNoEligibleAgentAccount. That is still truthful — the account
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
	// side, not account side. Contrast OutcomeNoEligibleAgentAccount below.
	OutcomeStatusOnly
	// OutcomeNoEligibleAgentAccount means rotation IS capable and the store was
	// consulted, but no account is eligible to switch to now and none will recover
	// by cooling. The dominant cause is that every other account is disabled or
	// permanently failed; on the probe-required path it can also mean no other
	// account has a usable (probeable, under-cap) candidate slot. Distinct from
	// OutcomeStatusOnly so operators are steered to the account-side remedy —
	// typically enabling or re-authenticating an account (`boss account update
	// <id> --status active`) — rather than the agent's rotation capability
	// (BOS-327).
	OutcomeNoEligibleAgentAccount
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
	NextAccount *domain.AgentAccount
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

// AgentAccountRepository is the narrow persistence port the engine declares
// (dependency inversion). The db package provides the real SQLite
// implementation; tests use a fake.
var _ AgentAccountRepository = (*db.SQLiteAgentAccountStore)(nil)

type AgentAccountRepository interface {
	// UpdateInTransaction runs fn inside ONE transaction, giving fn a tx-scoped view plus
	// guarded writer. A crash/error before commit leaves NO cooldown/health
	// change written (atomic rollback).
	UpdateInTransaction(ctx context.Context, provider string, fn func(tx db.TxAgentAccountView) error) error
}

// Engine decides account rotation. It holds no rotation state of its own: every
// decision is derived from the repository, so a fresh Engine over the same store
// behaves identically to a long-lived one (restart-safe).
type Engine struct {
	accounts             AgentAccountRepository
	defaultCooldown      time.Duration
	group                singleflight.Group
	clock                func() time.Time
	telemetry            libtelemetry.Client
	proactive5hThreshold func() float64
	usageStalenessWindow time.Duration
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
	daemontelemetry.Capture(ctx, e.telemetry, libtelemetry.EventAgentAccountRotated, map[string]any{
		"rotation_reason": "proactive", "provider": telemetryProvider(provider), "status": "rotated",
	})
}

// CaptureReactiveRotation records a successful reactive account switch. The
// caller invokes it only after the account has been applied.
func (e *Engine) CaptureReactiveRotation(ctx context.Context, provider, reason string) {
	if reason == "" {
		reason = "usage_limit"
	}
	daemontelemetry.Capture(ctx, e.telemetry, libtelemetry.EventAgentAccountRotated, map[string]any{
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

// WithProactive5hThreshold supplies the current configured 5h exclusion
// threshold. The getter runs outside the account transaction; nil keeps 90%.
func WithProactive5hThreshold(get func() float64) Option {
	return func(e *Engine) {
		if get != nil {
			e.proactive5hThreshold = get
		}
	}
}

// WithUsageStalenessWindow sets how long cached model windows may gate selection.
func WithUsageStalenessWindow(window time.Duration) Option {
	return func(e *Engine) {
		if window > 0 {
			e.usageStalenessWindow = window
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
func NewEngine(accounts AgentAccountRepository, opts ...Option) *Engine {
	e := &Engine{
		accounts:             accounts,
		usageStalenessWindow: (config.ManagedAgentAccountsConfig{}).UsageStalenessWindow(),
		defaultCooldown:      defaultCooldownDuration,
		clock:                time.Now,
		proactive5hThreshold: func() float64 { return .90 },
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
// provided by the single SQLite transaction in UpdateInTransaction (SQLite serializes
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

	key := fmt.Sprintf("%s\x00%s\x00%d\x00%t\x00%t\x00%s",
		sig.Provider, sig.CappedAccountID, sig.Kind, sig.SuppressCooldown, sig.SuppressHealthFail, sig.Model)
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
// reactive rotation (probed util < 1.0, below the configured 5h threshold,
// active, healthy, not cooling, not the bound account) and the shared consume-first
// comparator. An empty usage map yields nil because a proactive switch
// must never target an unprobed account. SweepProactive additionally requires the
// selected candidate to clear proactiveHysteresis before switching. The ceiling
// filters before ranking so a preferred but insufficiently idle account cannot
// hide another qualifying account.
func (e *Engine) SelectProactiveCandidate(ctx context.Context, provider, model, boundAccountID string, usage map[string]domain.UsageSnapshot, candidateUtilCeiling float64) (*domain.AgentAccount, error) {
	now := e.clock()
	threshold := e.proactive5hThreshold()
	var chosen *domain.AgentAccount
	err := e.accounts.UpdateInTransaction(ctx, provider, func(tx db.TxAgentAccountView) error {
		accts, err := tx.ListByProvider(ctx, now)
		if err != nil {
			return fmt.Errorf("list accounts: %w", err)
		}
		eligible := make(map[string]domain.UsageSnapshot, len(usage))
		for id, snap := range usage {
			if UsageUtilForModel(snap, model) <= candidateUtilCeiling {
				eligible[id] = snap
			}
		}
		chosen = e.selectCandidate(accts, boundAccountID, now, eligible, true, threshold, model)
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
	threshold := e.proactive5hThreshold()
	out := Outcome{CappedAccountID: sig.CappedAccountID}
	var accts []*domain.AgentAccount

	err := e.accounts.UpdateInTransaction(ctx, sig.Provider, func(tx db.TxAgentAccountView) error {
		switch sig.Kind {
		case AuthInvalidated:
			// BOS-981: a caller that is rotating off an ineligible-but-not-broken
			// account suppresses this write; see Signal.SuppressHealthFail.
			if !sig.SuppressHealthFail {
				if err := tx.SetHealthFailed(ctx, sig.CappedAccountID); err != nil {
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
		out.NextAccount = e.selectCandidate(accts, sig.CappedAccountID, now, sig.CandidateUsage, sig.CandidateProbeRequired, threshold, sig.Model)
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
	out.Kind = OutcomeNoEligibleAgentAccount
	return out, nil
}

// isSelectable reports whether an account is eligible to run: active, healthy,
// and not benched by durable credential-verification state. It is the single
// source of the eligibility predicate shared by selectCandidate (who can rotate
// in now) and minFutureCooldown (who will be selectable once a cooldown
// expires), so the two never drift.
//
// The auth-invalid clause is load-bearing and cannot be inferred from Health:
// RecordAuthCheck writes only the auth_check_* columns and deliberately leaves
// Health alone, so a status+health predicate selects credentials already known
// to be rejected. Downstream that is not a loud failure — resolveSpawnEnv
// returns a nil overlay, and the session silently runs on the agent CLI's
// ambient login (BOS-973) — while the failover proxy materializes the account
// directly and never consults the resolver at all.
func isSelectable(a *domain.AgentAccount) bool {
	return a.Status == domain.AgentAccountStatusActive &&
		a.Health == domain.AgentAccountHealthOK &&
		!a.IsAuthInvalid()
}

// selectCandidate ranks probed eligible accounts through the shared consume-first
// comparator. Without probes it preserves ListByProvider's ordering among
// candidates whose fresh matching model window is not exhausted.
func (e *Engine) selectCandidate(accts []*domain.AgentAccount, cappedID string, now time.Time, usage map[string]domain.UsageSnapshot, probeRequired bool, threshold float64, model string) *domain.AgentAccount {
	if probeRequired || len(usage) > 0 {
		var chosen *domain.AgentAccount
		var best ConsumeFirstCandidate
		for _, a := range accts {
			if !candidateSelectable(a, cappedID, now) {
				continue
			}
			snap, ok := usage[a.ID]
			if !ok || UtilizationCapped(UsageUtilForModel(snap, model)) {
				continue
			}
			candidate := liveConsumeFirstCandidate(a, snap, model)
			if Over5hThreshold(candidate, threshold, now) {
				continue
			}
			if chosen == nil || MoreConsumeFirst(candidate, best, now) {
				chosen, best = a, candidate
			}
		}
		return chosen
	}
	for _, a := range accts {
		if candidateSelectable(a, cappedID, now) && !freshModelCapped(a.Usage, model, now, e.usageStalenessWindow) {
			return a
		}
	}
	return nil
}

func liveConsumeFirstCandidate(a *domain.AgentAccount, snap domain.UsageSnapshot, model string) ConsumeFirstCandidate {
	w, _ := ModelWindowFor(snap.ModelWindows, model)
	return ConsumeFirstCandidate{
		ID: a.ID, Priority: a.Priority, LastUsedAt: a.LastUsedAt, Fresh: true,
		ModelUtil: w.Util, ModelReset: w.ResetsAt,
		Util7d: snap.Util7d, Util5h: snap.Util5h, Reset5h: snap.Reset5h, Reset7d: snap.Reset7d,
	}
}

func candidateSelectable(a *domain.AgentAccount, cappedID string, now time.Time) bool {
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
func minFutureCooldown(accts []*domain.AgentAccount, now time.Time) (time.Time, bool) {
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
