package rotation

import "testing"

func TestUtilizationCapped(t *testing.T) {
	cases := []struct {
		util float64
		want bool
	}{
		{0, false},
		{0.99, false},
		{1.0, true},
		{1.5, true},
	}
	for _, c := range cases {
		if got := UtilizationCapped(c.util); got != c.want {
			t.Errorf("UtilizationCapped(%v) = %v, want %v", c.util, got, c.want)
		}
	}
	if UtilizationCapThreshold != 1.0 {
		t.Errorf("UtilizationCapThreshold = %v, want 1.0", UtilizationCapThreshold)
	}
}

func TestLowerUtilization(t *testing.T) {
	if !LowerUtilization(0.1, 0.2) {
		t.Error("LowerUtilization(0.1, 0.2) = false, want true")
	}
	if LowerUtilization(0.2, 0.1) {
		t.Error("LowerUtilization(0.2, 0.1) = true, want false")
	}
	if LowerUtilization(0.1, 0.1) {
		t.Error("LowerUtilization(0.1, 0.1) = true, want false (strict)")
	}
}

func TestMaxUtilization(t *testing.T) {
	cases := []struct {
		u5h, u7d, want float64
	}{
		{0.4, 0.9, 0.9},
		{0.9, 0.4, 0.9},
		{0.5, 0.5, 0.5},
		{0, 0, 0},
	}
	for _, c := range cases {
		if got := MaxUtilization(c.u5h, c.u7d); got != c.want {
			t.Errorf("MaxUtilization(%v, %v) = %v, want %v", c.u5h, c.u7d, got, c.want)
		}
	}
}
