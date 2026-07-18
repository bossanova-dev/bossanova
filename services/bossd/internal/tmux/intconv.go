package tmux

import "math"

// clampUint16 clamps a uint32 to the uint16 range so terminal winsize
// conversions are provably in bounds (silences gosec G115 without a
// suppression). Terminal dimensions never legitimately exceed 65535; the clamp
// is a defensive guard against a malformed resize request, not an expected path.
func clampUint16(v uint32) uint16 {
	if v > math.MaxUint16 {
		return math.MaxUint16
	}
	return uint16(v)
}

// safeInt32 clamps an int to the int32 range so narrowing conversions are
// provably in bounds (silences gosec G115 without a suppression). Process exit
// codes are always small; the clamp is a defensive guard.
func safeInt32(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	if n < math.MinInt32 {
		return math.MinInt32
	}
	return int32(n)
}
