package tuidriver

import (
	"math"
	"testing"
)

// clampUint16 narrows the Options width/height to the uint16 fields of
// creackpty.Winsize. The guard added for gosec G115 passes small in-range
// terminal dimensions through unchanged and clamps out-of-range values into
// [0, math.MaxUint16] rather than wrapping.
func TestClampUint16(t *testing.T) {
	tests := []struct {
		in   int
		want uint16
	}{
		{0, 0},
		{120, 120},
		{30, 30},
		{math.MaxUint16, math.MaxUint16},
		{math.MaxUint16 + 1, math.MaxUint16},
		{-1, 0},
		{math.MinInt32, 0},
	}
	for _, tc := range tests {
		if got := clampUint16(tc.in); got != tc.want {
			t.Errorf("clampUint16(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
