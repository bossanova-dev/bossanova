package main

import "math"

// safeInt32 clamps an int to the int32 range so that narrowing conversions are
// provably in bounds (silences gosec G115 without a suppression). The values
// converted here (delivery-fan-out counts for the proto-shaped JSON schemas
// this package emits) never approach these bounds; the clamp is a defensive
// guard, not an expected code path.
//
// Mirrors the identical package-local helpers in services/bossd/internal/plugin
// and services/bossd/internal/testharness. Keep the plain `int` parameter and
// both bounds: a generic `[]T`/len() variant does not satisfy gosec's analysis.
func safeInt32(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	if n < math.MinInt32 {
		return math.MinInt32
	}
	return int32(n)
}
