package pty

import (
	"math"
	"testing"
)

// clampUint16 narrows terminal dimensions to uint16 for creackpty.Resize. The
// guard added for gosec G115 must pass small in-range values through unchanged
// and clamp anything out of [0, math.MaxUint16] rather than wrapping.
func TestClampUint16(t *testing.T) {
	tests := []struct {
		in   int
		want uint16
	}{
		{0, 0},
		{80, 80},
		{24, 24},
		{math.MaxUint16, math.MaxUint16},
		{math.MaxUint16 + 1, math.MaxUint16},
		{1 << 20, math.MaxUint16},
		{-1, 0},
		{math.MinInt32, 0},
	}
	for _, tc := range tests {
		if got := clampUint16(tc.in); got != tc.want {
			t.Errorf("clampUint16(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
