package rotation

import (
	"testing"
	"time"
)

func TestFutureWeeklyReset(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)

	cases := []struct {
		name    string
		reset   *time.Time
		wantOK  bool
		wantVal time.Time
	}{
		{name: "nil reset ⇒ not urgent", reset: nil, wantOK: false},
		{name: "past reset ⇒ not urgent (window rolled)", reset: &past, wantOK: false},
		{name: "exactly now ⇒ not urgent (strictly future only)", reset: &now, wantOK: false},
		{name: "future reset ⇒ urgent", reset: &future, wantOK: true, wantVal: future},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := FutureWeeklyReset(c.reset, now)
			if ok != c.wantOK {
				t.Fatalf("FutureWeeklyReset ok = %v, want %v", ok, c.wantOK)
			}
			if ok && !got.Equal(c.wantVal) {
				t.Errorf("FutureWeeklyReset t = %v, want %v", got, c.wantVal)
			}
			if !ok && !got.IsZero() {
				t.Errorf("FutureWeeklyReset t = %v, want zero when ok=false", got)
			}
		})
	}
}

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
