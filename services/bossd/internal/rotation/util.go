package rotation

import "time"

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

// MaxUtilization reduces a 5h/7d utilization pair to the single worst-case
// fraction. It carries no rate-limit semantics; callers that need the
// rate-limited→1 bump layer it on top (see UsageUtil).
func MaxUtilization(u5h, u7d float64) float64 {
	if u7d > u5h {
		return u7d
	}
	return u5h
}
