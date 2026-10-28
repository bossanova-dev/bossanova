package main

import (
	"context"
	"errors"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/recurser/bossd/internal/accountwiring"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/rotation"
)

// usageSnapshotRecorder persists a probe snapshot and, for a confirmed
// suspension, fails the account's health. MarkSuspended lets the periodic
// usage refresh proactively sideline an account the probe confirms is suspended
// (an org/billing block), so rotation stops selecting it before a session is
// ever bound to it.
type usageSnapshotRecorder interface {
	RecordUsageProbe(context.Context, string, domain.UsageSnapshot) error
	MarkSuspended(context.Context, string, string) error
}

// probeThrottleFloor is the minimum time the periodic usage refresh waits
// before probing an account again after its usage endpoint returned a throttle.
//
// It is a floor, not a guess to be overridden downwards. The endpoint enforces
// roughly 28-30 requests per identity per rolling 60-MINUTE TRAILING window, so
// a saturated identity does not regain headroom gradually — and a stated
// Retry-After frequently under-reports when the block actually clears. Six
// minutes caps a single account at ~10 polls/hour, comfortably inside that
// budget even with several accounts and the other on-demand probe callers
// sharing it. A longer server-stated horizon always wins over this value, up to
// probeThrottleCeiling.
const probeThrottleFloor = 6 * time.Minute

// probeThrottleCeiling caps how long a server-stated Retry-After may park an
// account's periodic usage refresh.
//
// Retry-After is upstream input this daemon does not control, and before
// BOS-828 nothing read it at all — so honouring it verbatim is a new trust
// relationship that needs a bound. A bogus or buggy `Retry-After: 86400` would
// otherwise stop refreshing that account for a day, and rotation's
// selectDefault only trusts a snapshot inside the staleness window: every
// decision for that account would silently degrade to a stale snapshot, with a
// single WARN at the moment of throttle and nothing afterwards. Overlong values
// are a much cheaper failure to absorb than a starved rotation, because the
// worst case of re-probing early is one more 429.
//
// This BOUNDS that overshoot rather than eliminating it, and the difference
// matters to anyone tuning these values. At default config the staleness window
// is 30m and the refresh tick 15m, so a throttle at T leaves the newest good
// snapshot dated T-15m, the T+15m tick skips, and the next probe lands at T+30m
// — a 45-minute-old snapshot, still outside the window. Eliminating the
// overshoot means deriving this constant from
// settings.ManagedAccounts.UsageStalenessWindow() and threading it in, which is
// the same config-threading change probeThrottleFloor deliberately does not
// make; both belong to one follow-up, not here.
//
// Thirty minutes is comfortably above any horizon this endpoint states in
// practice while still bounded by the 60-minute trailing window the budget is
// measured over, so the clamp only ever engages on an implausible value.
const probeThrottleCeiling = 30 * time.Minute

func probeUsageSnapshotForRotation(
	ctx context.Context,
	logger zerolog.Logger,
	prober any,
	recorder usageSnapshotRecorder,
	accountID string,
	throttleUntil map[string]time.Time,
) (domain.UsageSnapshot, bool) {
	if accountID == "" || recorder == nil {
		return domain.UsageSnapshot{}, false
	}
	usageProbe, ok := prober.(usageSnapshotProber)
	if !ok {
		return domain.UsageSnapshot{}, false
	}
	snap, err := usageProbe.ProbeUsageSnapshot(ctx, accountID)
	if err != nil {
		// The usage endpoint throttled our POLLING (BOS-828) — distinct from the
		// suspension below and disjoint from it by gRPC code. It is emphatically
		// not evidence about the account's quota, so nothing is written to the
		// account: no cooldown, no health change. The only reaction is that the
		// caller-owned backoff map, when the caller keeps one, stops the periodic
		// loop re-probing this account until the deadline. throttleUntil is nil
		// for the on-demand rotation/failover callers, which need a fresh answer
		// every time and deliberately do not honour the backoff.
		if accountwiring.IsProbeThrottled(err) {
			retryAfter, stated := accountwiring.ProbeRetryAfter(err)
			backoff := min(max(retryAfter, probeThrottleFloor), probeThrottleCeiling)
			if throttleUntil != nil {
				throttleUntil[accountID] = time.Now().Add(backoff)
			}
			// Deliberately its own message, not the generic "usage probe failed"
			// below, so an operator can grep the two apart: this one is our
			// poller running hot, that one is the account or the network.
			logger.Warn().Str("account_id", accountID).
				Dur("retry_after", retryAfter).
				Bool("retry_after_stated", stated).
				Dur("backoff", backoff).
				Bool("backoff_honored", throttleUntil != nil).
				Msg("auto-rotate: usage endpoint throttled the poll; backing off")
			return domain.UsageSnapshot{}, false
		}
		// A confirmed suspension (org/billing block) is a permanent condition,
		// unlike a transient probe failure: proactively fail the account's health
		// so rotation excludes it before any session binds to it. Scoped narrowly
		// to the suspension signal — a generic auth/transient error keeps the
		// conservative log-and-skip below to avoid false positives from blips.
		if handled, reason, merr := accountwiring.MarkSuspendedIfConfirmed(ctx, recorder, accountID, err); handled {
			if merr != nil {
				logger.Warn().Err(merr).Str("account_id", accountID).
					Msg("auto-rotate: mark account suspended failed")
			} else {
				logger.Warn().Str("account_id", accountID).Str("reason", reason).
					Msg("auto-rotate: account suspended; health failed")
			}
			return domain.UsageSnapshot{}, false
		}
		logger.Warn().Err(err).Str("account_id", accountID).
			Msg("auto-rotate: usage probe failed")
		return domain.UsageSnapshot{}, false
	}
	if snap.FetchedAt == nil {
		return domain.UsageSnapshot{}, false
	}
	if err := recorder.RecordUsageProbe(ctx, accountID, snap); err != nil {
		logger.Warn().Err(err).Str("account_id", accountID).
			Msg("auto-rotate: usage probe cache write failed")
	}
	return snap, true
}

// refreshActiveAccountUsage probes and persists a fresh usage snapshot for every
// active account, so the util-aware default-account selector
// (account.Resolver.selectDefault via freshCapped/isFresh) always has fresh data
// to sideline capped accounts. Without a periodic refresh, snapshots only update
// on a rotation event, go stale past the staleness window, and selection
// degrades to priority/LRU — which can bind a brand-new session to a fully-capped
// account (the "new chats pick an exhausted account" symptom, BOS-320).
//
// Cooling accounts are probed too (BOS-584). They were skipped on the premise
// that their unavailability was "already known and time-bounded" — that premise
// is false: a transient upstream 429 could bench a perfectly healthy account for
// a flat hour, and nothing in production ever cleared cooldown_until, so expiry
// was purely wall-clock. Re-probing them bounds a mis-cooling to one refresh
// interval: a fresh, available probe that contradicts the cooldown clears it
// (see reconcileContradictedCooldown).
//
// Scope that claim precisely — it is about ACCOUNT SELECTION, not parked work.
// Clearing cooldown_until returns the account to the selectable pool (rotation's
// selectCandidate and the default-account resolver both key off it), so new and
// rotating sessions can use it immediately. It does NOT wake sessions already
// parked by the wrong bench: parkAllCooling stamped sessions.rotation_resume_at
// from the cooldown that was current at park time, and SweepParkedRotations is
// driven purely off that stamp, so a parked session still sleeps until its own
// resume-at falls due. Pulling those stamps forward is a separate change to the
// parked sweep, deliberately not made here. An operator reading this during an
// incident should expect freed capacity, not resumed sessions.
//
// Fail-soft — a list, probe, or cooldown-clear error is logged and skipped,
// never fatal. Returns the number of accounts refreshed.
func refreshActiveAccountUsage(
	ctx context.Context,
	logger zerolog.Logger,
	prober any,
	recorder usageSnapshotRecorder,
	throttleUntil map[string]time.Time,
) int {
	store, ok := recorder.(accountListStore)
	if !ok {
		return 0
	}
	accounts, err := store.List(ctx)
	if err != nil {
		logger.Warn().Err(err).Msg("usage-refresh: account list failed")
		return 0
	}
	now := time.Now()
	refreshed := 0
	for _, acct := range accounts {
		if acct == nil || acct.Status != domain.AgentAccountStatusActive {
			continue
		}
		// Skip BEFORE the RPC — the whole point of the backoff is to stop
		// spending requests from a budget we already exhausted, so a guard
		// placed after the probe would be no guard at all. Reads and deletes on
		// a nil map are both no-ops, so an on-demand caller passing nil simply
		// never skips.
		if until, backingOff := throttleUntil[acct.ID]; backingOff {
			if until.After(now) {
				continue
			}
			// The window closed; drop the entry so the map cannot grow without
			// bound across a long-lived daemon.
			delete(throttleUntil, acct.ID)
		}
		snap, ok := probeUsageSnapshotForRotation(ctx, logger, prober, recorder, acct.ID, throttleUntil)
		if !ok {
			continue
		}
		refreshed++
		if acct.CooldownUntil == nil || !acct.CooldownUntil.After(now) {
			continue
		}
		reconcileContradictedCooldown(ctx, logger, recorder, acct, snap)
	}
	return refreshed
}

// accountCooldownClearer reads an account back and writes cooldown_until on it.
// The store's Update supports clearing it to NULL (db.AgentAccountStore.Update);
// until BOS-584 nothing in production ever invoked that branch, which is why a
// wrong bench could only be outlived, never corrected. Get is here so the clear
// can re-check the row it is about to overwrite (see
// reconcileContradictedCooldown).
type accountCooldownClearer interface {
	Get(context.Context, string) (*domain.AgentAccount, error)
	Update(context.Context, string, db.UpdateAgentAccountParams) (*domain.AgentAccount, error)
}

// The reconciliation sweep reaches the store through a runtime assertion, so a
// signature drift on db.AgentAccountStore.Update would silently turn the whole
// safety net into a no-op — and every test injects a fake that satisfies the
// interface, so nothing would go red. Pin the production shape at compile time
// instead.
var _ accountCooldownClearer = (db.AgentAccountStore)(nil)

// reconcileContradictedCooldown clears a cooling account's cooldown_until when
// the authoritative usage probe says the account is not limited after all.
//
// The guard rails are the whole point, and they are deliberately asymmetric: a
// cooldown is cleared ONLY on a probe that is fresh, available, and says "not
// limited". A probe error never reaches here (probeUsageSnapshotForRotation
// already returned !ok), and an explicitly unavailable probe
// (UNSUPPORTED/UNSPECIFIED) leaves the bench exactly as it is. Clearing on an
// unverifiable probe would be the mirror image of the bug this fixes — benching
// on an unverifiable 429. A failed clear is logged and skipped so one bad write
// never aborts the sweep.
//
// "Fresh" is structural, not a timestamp comparison: snap comes from
// materializerAdapter.ProbeUsageSnapshot, which always issues a live
// ProbeRateLimit RPC and stamps FetchedAt with the instant of that call — it
// never returns a cached snapshot, so the nil check below is a
// probe-produced-nothing guard, not an age guard. Wiring in a prober that
// caches would break that guarantee and would need an explicit age check here.
//
// The re-read before the write is the concurrency guard. acct was listed BEFORE
// a network probe, so a genuine, probe-confirmed cap may have landed in that
// window; overwriting it would be this sweep re-creating the very bug it exists
// to undo. The store exposes no compare-and-set, so this is not atomic — but it
// shrinks the clobber window from the probe's whole duration to two adjacent
// SQLite statements, and any bench that still slips through is re-applied by
// the next 429.
func reconcileContradictedCooldown(
	ctx context.Context,
	logger zerolog.Logger,
	recorder usageSnapshotRecorder,
	acct *domain.AgentAccount,
	snap domain.UsageSnapshot,
) {
	if snap.FetchedAt == nil || rotation.UsageSnapshotProbeUnavailable(snap) ||
		rotation.UsageSnapshotConfirmsLimited(snap) {
		return
	}
	store, ok := recorder.(accountCooldownClearer)
	if !ok {
		logger.Warn().Str("account_id", acct.ID).
			Msg("usage-refresh: recorder cannot clear cooldowns; contradicted bench left in place")
		return
	}
	current, err := store.Get(ctx, acct.ID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		// The account was deleted between the list and the re-read. Nothing to
		// undo, and emphatically not a store fault — SQLiteAgentAccountStore.Get
		// surfaces a missing row this way, so without this arm an operator
		// deleting an account would read as an error in the log.
		return
	case err != nil:
		logger.Warn().Err(err).Str("account_id", acct.ID).
			Msg("usage-refresh: re-reading the contradicted account failed; bench left in place")
		return
	}
	switch {
	case current == nil || current.CooldownUntil == nil:
		// Already cleared or expired out from under us — nothing to undo.
		return
	case acct.CooldownUntil == nil || !current.CooldownUntil.Equal(*acct.CooldownUntil):
		logger.Info().Str("account_id", acct.ID).
			Msg("usage-refresh: cooldown changed during the probe; leaving the newer bench in place")
		return
	}
	var cleared *time.Time
	if _, err := store.Update(ctx, acct.ID, db.UpdateAgentAccountParams{CooldownUntil: &cleared}); err != nil {
		logger.Warn().Err(err).Str("account_id", acct.ID).
			Msg("usage-refresh: clearing contradicted cooldown failed")
		return
	}
	logger.Info().
		Str("account_id", acct.ID).
		Str("status", snap.Status).
		Float64("util_5h", snap.Util5h).
		Float64("util_7d", snap.Util7d).
		Msg("usage-refresh: cooldown contradicted by usage probe; bench cleared")
}

// authProbeConfirmsInvalidation reports whether a usage-probe error is a typed
// 401 / auth invalidation. The claude plugin's probe returns
// codes.Unauthenticated (agenterr.KindAuthInvalidated) on HTTP 401; the host
// wraps it once via fmt.Errorf(... %w), and grpcstatus.Code unwraps the %w chain,
// so classification is robust against the wrapping and never relies on the error
// string. A nil error (healthy / usage-limited snapshot) or any non-Unauthenticated
// error is NOT an auth invalidation.
func authProbeConfirmsInvalidation(err error) bool {
	return err != nil && grpcstatus.Code(err) == codes.Unauthenticated
}

func cacheUsageProbeForRotationSignal(
	ctx context.Context,
	logger zerolog.Logger,
	prober any,
	recorder usageSnapshotRecorder,
	sig rotation.Signal,
) rotation.Signal {
	if sig.Kind != rotation.UsageLimited {
		return sig
	}
	sig.CandidateProbeRequired = true
	sig.CandidateUsage = probeCandidateUsageForRotationSignal(ctx, logger, prober, recorder, sig)
	return sig
}

type accountListStore interface {
	List(context.Context) ([]*domain.AgentAccount, error)
}

func probeCandidateUsageForRotationSignal(
	ctx context.Context,
	logger zerolog.Logger,
	prober any,
	recorder usageSnapshotRecorder,
	sig rotation.Signal,
) map[string]domain.UsageSnapshot {
	if sig.Provider == "" {
		return nil
	}
	store, ok := recorder.(accountListStore)
	if !ok {
		return nil
	}
	accounts, err := store.List(ctx)
	if err != nil {
		logger.Warn().Err(err).Str("provider", sig.Provider).
			Msg("auto-rotate: candidate account list failed")
		return nil
	}
	now := time.Now()
	usage := map[string]domain.UsageSnapshot{}
	sawCandidate := false
	for _, acct := range accounts {
		if acct == nil || string(acct.Provider) != sig.Provider || acct.ID == sig.CappedAccountID {
			continue
		}
		// Mirrors rotation.BindableNow so the probe budget is not spent on an
		// account the engine will not select. It must stay in step with
		// isSelectable: an empty candidate usage map makes selectCandidate fall
		// through to its unguarded list-order branch, so probing MORE widely
		// than the engine selects is the safe direction and probing less is
		// not.
		if !rotation.BindableNow(acct, now) {
			continue
		}
		sawCandidate = true
		snap, ok := probeUsageSnapshotForRotation(ctx, logger, prober, recorder, acct.ID, nil)
		if !ok {
			continue
		}
		if rotation.UsageSnapshotProbeUnavailable(snap) {
			continue
		}
		usage[acct.ID] = snap
	}
	if !sawCandidate {
		return nil
	}
	return usage
}
