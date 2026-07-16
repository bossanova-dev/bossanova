package models

import "testing"

func TestMergeStrategyValid(t *testing.T) {
	cases := []struct {
		in   MergeStrategy
		want bool
	}{
		{MergeStrategyMerge, true},
		{MergeStrategyRebase, true},
		{MergeStrategySquash, true},
		{MergeStrategy(""), false},
		{MergeStrategy("bogus"), false},
		{MergeStrategy("MERGE"), false},
	}
	for _, tc := range cases {
		if got := tc.in.Valid(); got != tc.want {
			t.Errorf("MergeStrategy(%q).Valid() = %v, want %v", string(tc.in), got, tc.want)
		}
	}
}

func TestParseMergeStrategy(t *testing.T) {
	cases := []struct {
		in   string
		want MergeStrategy
	}{
		{"merge", MergeStrategyMerge},
		{"rebase", MergeStrategyRebase},
		{"squash", MergeStrategySquash},
		{"", MergeStrategyMerge},
		{"bogus", MergeStrategyMerge},
		{"Merge", MergeStrategyMerge}, // unknown casing normalizes to the default
	}
	for _, tc := range cases {
		if got := ParseMergeStrategy(tc.in); got != tc.want {
			t.Errorf("ParseMergeStrategy(%q) = %q, want %q", tc.in, string(got), string(tc.want))
		}
	}
}

func TestMergeStrategiesOrder(t *testing.T) {
	got := MergeStrategies()
	want := []MergeStrategy{MergeStrategyMerge, MergeStrategyRebase, MergeStrategySquash}
	if len(got) != len(want) {
		t.Fatalf("MergeStrategies() len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("MergeStrategies()[%d] = %q, want %q", i, string(got[i]), string(want[i]))
		}
	}
}
