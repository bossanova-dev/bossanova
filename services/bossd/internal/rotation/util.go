package rotation

import (
	"time"

	"github.com/recurser/bossalib/models"
)

// FutureWeeklyReset reports the account's weekly-quota reset instant when it is
// known AND still strictly in the future; ok=false means "no urgent expiry"
// (a nil reset — never probed — or a reset already at/before now, since an
// already-rolled window is a fresh full week, not urgent). It is the single
// semantic source the bind-time comparator (account.moreEligible) and the
// rotation fake share, so both stay in lockstep with the SQL
// "usage_reset_7d IS NOT NULL AND usage_reset_7d > now" rank in
// account_decide_tx.go's ListByProvider. Keeping the three ordering surfaces on
// one predicate is the "never drift" contract BOS-429 depends on.
func FutureWeeklyReset(reset7d *time.Time, now time.Time) (time.Time, bool) {
	if reset7d == nil {
		return time.Time{}, false
	}
	if !reset7d.After(now) {
		return time.Time{}, false
	}
	return *reset7d, true
}

// UtilizationCapThreshold is the utilization fraction at or above which a window
// is considered exhausted (fully capped). 1.0 == 100% of the window consumed.
const UtilizationCapThreshold = 1.0

// UtilizationCapped reports whether a single utilization fraction is at or above
// the cap threshold. It is the single source of the "this window is exhausted"
// predicate shared by engine.selectCandidate and the binding-time resolver.
func UtilizationCapped(util float64) bool { return util >= UtilizationCapThreshold }

// LowerUtilization reports whether a is strictly less utilized than b. It is the
// single source of the "prefer the less-utilized account" comparator shared by
// engine.selectCandidate and the binding-time resolver.
func LowerUtilization(a, b float64) bool { return a < b }

// MinRotationHeadroom is the least remaining-quota fraction (1 - utilization) a
// candidate must still have before rotation will prefer it for its perishable
// weekly quota rather than for being idle (BOS-830). It bounds consume-first:
// a soon-resetting account is only worth switching onto while it has room to do
// real work, so rotation never lands on an account at 95% that re-caps within
// minutes and burns another attempt from the per-run rotation budget.
//
// Note the deliberate window asymmetry: the band is measured against the
// WORST-case window (the utilization map carries UsageUtil, i.e.
// MaxUtilization(5h, 7d)) while the urgency it gates is the 7d reset. So an
// account whose weekly quota is perishable but whose 5h window is nearly spent
// stays out of the band. That is the conservative pairing and the intended one —
// it is exactly the "re-caps within minutes" case this floor exists to exclude.
//
// A package constant, deliberately not a config key: 0.25 is a starting point
// with no field evidence behind it, and it lives in exactly one place so it can
// be tuned — or promoted to ManagedAccountsConfig — additively once there is.
// Because the floor is compared with >=, the "exactly at the floor is in band"
// guarantee is exact only while the value is binary-representable (0.25 is;
// 0.3 would not be) — worth re-checking the boundary test when tuning it.
const MinRotationHeadroom = 0.25

// HasRotationHeadroom reports whether util leaves at least MinRotationHeadroom
// of the window unspent, i.e. whether the account is inside the consume-first
// band. The comparison is >=, so headroom exactly at the floor is INSIDE the
// band. It is the single source of that predicate for engine.selectCandidate.
func HasRotationHeadroom(util float64) bool { return 1-util >= MinRotationHeadroom }

// MaxUtilization reduces a 5h/7d utilization pair to the single worst-case
// fraction. It carries no rate-limit semantics; callers that need the
// rate-limited→1 bump layer it on top (see UsageUtil).
func MaxUtilization(u5h, u7d float64) float64 {
	if u7d > u5h {
		return u7d
	}
	return u5h
}

// Selectable reports whether an account may be chosen to run: active, healthy,
// and not benched by durable credential-verification state. It is the exported
// form of the engine's own predicate, so binding-validation surfaces outside
// this package share it instead of re-deriving eligibility and drifting.
func Selectable(a *models.Account) bool { return isSelectable(a) }

// BindableNow reports whether an account may be bound to a session right now:
// Selectable, and not inside a cooldown window at the given instant.
//
// Explicit binding paths (CreateSession with an account id, manual account
// switch) need this stricter form because they must refuse BEFORE mutating a
// session — a spawn-time refusal arrives after the chat has already been
// stopped and rebound.
func BindableNow(a *models.Account, now time.Time) bool {
	if !Selectable(a) {
		return false
	}
	return a.CooldownUntil == nil || !a.CooldownUntil.After(now)
}
