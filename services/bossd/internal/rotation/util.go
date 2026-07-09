package rotation

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
