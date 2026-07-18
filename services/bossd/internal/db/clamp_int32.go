package db

import "math"

// clampInt32 converts n to int32, clamping to the int32 range instead of
// wrapping. bossd counts/enums never exceed int32 in practice; the clamp is a
// gosec-G115-satisfying guard, not a behaviour change on real inputs.
func clampInt32(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	if n < math.MinInt32 {
		return math.MinInt32
	}
	return int32(n)
}
