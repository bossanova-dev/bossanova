package main

import (
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestFilterDependabotPRs(t *testing.T) {
	prs := []*bossanovav1.PRSummary{
		nil,
		{Number: 1, Author: "human-dev"},
		{Number: 2, Author: dependabotAuthor},
		{Number: 3, Author: "dependabot[bot]"},
		{Number: 4, Author: dependabotAuthor},
	}

	got := filterDependabotPRs(prs)
	if len(got) != 2 {
		t.Fatalf("filterDependabotPRs returned %d PRs, want 2", len(got))
	}

	wantNumbers := []int32{2, 4}
	for i, want := range wantNumbers {
		if got[i].GetNumber() != want {
			t.Errorf("filtered PR %d number = %d, want %d", i, got[i].GetNumber(), want)
		}
	}
}
