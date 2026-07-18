package testharness

import "math"

// safeInt32 clamps an int to the int32 range so that narrowing conversions are
// provably in bounds (silences gosec G115 without a suppression). The values
// converted here (PR numbers, bounded enum ordinals) never approach these
// bounds; the clamp is a defensive guard, not an expected code path.
func safeInt32(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	if n < math.MinInt32 {
		return math.MinInt32
	}
	return int32(n)
}
