package runmetrics

import (
	"testing"
	"time"
)

func TestParentOnlyDurationSubtractsUnionedChildren(t *testing.T) {
	base := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	parent := Span{Start: base, Stop: base.Add(10 * time.Minute)}
	children := []Span{
		{Start: base.Add(-time.Minute), Stop: base.Add(4 * time.Minute)},
		{Start: base.Add(3 * time.Minute), Stop: base.Add(8 * time.Minute)},
		{Start: base.Add(9 * time.Minute), Stop: base.Add(12 * time.Minute)},
	}

	if got, want := UnionDuration(parent, children), 9*time.Minute; got != want {
		t.Fatalf("UnionDuration = %s, want %s", got, want)
	}
	if got, want := ParentOnlyDuration(parent, children), time.Minute; got != want {
		t.Fatalf("ParentOnlyDuration = %s, want %s", got, want)
	}
}

func TestParallelismUsesSummedChildDurationsOverUnion(t *testing.T) {
	base := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	parent := Span{Start: base, Stop: base.Add(10 * time.Minute)}
	children := []Span{
		{Start: base.Add(time.Minute), Stop: base.Add(5 * time.Minute)},
		{Start: base.Add(3 * time.Minute), Stop: base.Add(7 * time.Minute)},
	}

	got, ok := Parallelism(parent, children)
	if !ok {
		t.Fatal("Parallelism reported no interpretable child span")
	}
	if got != 8.0/6.0 {
		t.Fatalf("Parallelism = %f, want %f", got, 8.0/6.0)
	}
	coverage, ok := Coverage(parent, children)
	if !ok {
		t.Fatal("Coverage reported no parent span")
	}
	if coverage != 0.6 {
		t.Fatalf("Coverage = %f, want 0.6", coverage)
	}
}

func TestMedianDurationInterpolatesEvenLength(t *testing.T) {
	got, ok := MedianDuration([]time.Duration{4 * time.Second, time.Second, 3 * time.Second, 2 * time.Second})
	if !ok {
		t.Fatal("MedianDuration returned false")
	}
	if got != 2500*time.Millisecond {
		t.Fatalf("MedianDuration = %s, want 2.5s", got)
	}
}

func TestMedianInt64(t *testing.T) {
	tests := []struct {
		name   string
		values []int64
		want   int64
		wantOK bool
	}{
		{name: "empty is not a median", values: nil, want: 0, wantOK: false},
		{name: "single value", values: []int64{4}, want: 4, wantOK: true},
		{name: "odd count takes the middle", values: []int64{7, 1, 3}, want: 3, wantOK: true},
		{name: "even count averages the middle pair", values: []int64{1, 2, 3, 6}, want: 2, wantOK: true},
		{name: "even count truncates rather than rounding", values: []int64{1, 2}, want: 1, wantOK: true},
		{name: "zeros are real samples", values: []int64{0, 0, 0, 5}, want: 0, wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := MedianInt64(tt.values)
			if ok != tt.wantOK {
				t.Fatalf("MedianInt64(%v) ok = %v, want %v", tt.values, ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("MedianInt64(%v) = %d, want %d", tt.values, got, tt.want)
			}
		})
	}
}

func TestMedianInt64DoesNotMutateInput(t *testing.T) {
	values := []int64{9, 2, 5}
	if _, ok := MedianInt64(values); !ok {
		t.Fatal("MedianInt64 reported no median for three samples")
	}
	want := []int64{9, 2, 5}
	for i := range want {
		if values[i] != want[i] {
			t.Fatalf("MedianInt64 reordered its caller's slice: got %v, want %v", values, want)
		}
	}
}

func TestTerminalStateMix(t *testing.T) {
	tests := []struct {
		name   string
		states []string
		want   map[string]int64
	}{
		{name: "no runs yields no mix", states: nil, want: nil},
		{
			name:   "counts each state",
			states: []string{"REVIEW_READY", "BLOCKED", "REVIEW_READY"},
			want:   map[string]int64{"REVIEW_READY": 2, "BLOCKED": 1},
		},
		{
			// Unrecorded runs get their own bucket so "no boss-build runs in this
			// window" stays distinguishable from "every run blocked".
			name:   "unrecorded is its own bucket",
			states: []string{"", "", "PARTIAL"},
			want:   map[string]int64{"": 2, "PARTIAL": 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TerminalStateMix(tt.states)
			if len(got) != len(tt.want) {
				t.Fatalf("TerminalStateMix(%v) = %v, want %v", tt.states, got, tt.want)
			}
			for state, count := range tt.want {
				if got[state] != count {
					t.Errorf("TerminalStateMix(%v)[%q] = %d, want %d", tt.states, state, got[state], count)
				}
			}
		})
	}
}
