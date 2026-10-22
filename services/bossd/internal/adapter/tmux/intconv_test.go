package tmux

import (
	"math"
	"testing"
)

func TestClampUint16(t *testing.T) {
	tests := []struct {
		name string
		in   uint32
		want uint16
	}{
		{"zero", 0, 0},
		{"in-range", 80, 80},
		{"max", math.MaxUint16, math.MaxUint16},
		{"just-above-max", math.MaxUint16 + 1, math.MaxUint16},
		{"far-above-max", math.MaxUint32, math.MaxUint16},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampUint16(tt.in); got != tt.want {
				t.Errorf("clampUint16(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestSafeInt32(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int32
	}{
		{"below-min", math.MinInt64, math.MinInt32},
		{"zero", 0, 0},
		{"in-range", 137, 137},
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
