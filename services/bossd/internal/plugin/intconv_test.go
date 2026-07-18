package plugin

import (
	"math"
	"testing"
)

func TestSafeInt32(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int32
	}{
		{"below-min", math.MinInt64, math.MinInt32},
		{"just-below-min", int(math.MinInt32) - 1, math.MinInt32},
		{"min", math.MinInt32, math.MinInt32},
		{"zero", 0, 0},
		{"in-range", 1234, 1234},
		{"max", math.MaxInt32, math.MaxInt32},
		{"just-above-max", int(math.MaxInt32) + 1, math.MaxInt32},
		{"above-max", math.MaxInt64, math.MaxInt32},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeInt32(tt.in); got != tt.want {
				t.Errorf("safeInt32(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}
