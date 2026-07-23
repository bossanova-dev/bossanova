package callback

import "math"

// safeInt32 clamps an int to the int32 range so that narrowing conversions are
// provably in bounds (silences gosec G115 without a suppression). Values above
// math.MaxInt32 clamp to math.MaxInt32; values below math.MinInt32 clamp to
// math.MinInt32. In practice the values converted here are GitHub PR numbers,
// which are small positive ints validated at the store boundary; the clamp is a
// defensive guard, not an expected code path.
func safeInt32(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	if n < math.MinInt32 {
		return math.MinInt32
	}
	return int32(n)
}
