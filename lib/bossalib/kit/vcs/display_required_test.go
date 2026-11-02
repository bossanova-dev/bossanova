package vcs

import "testing"

func TestComputeDisplayStatusWith_Sessions_AC223101(t *testing.T) {
	required := RequiredChecks{Names: []string{"go-test"}, ExemptBases: []string{"staging"}}
	light := CheckResult{Name: "lint", Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)}
	goTest := CheckResult{Name: "go-test", Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)}
	skippedGoTest := CheckResult{Name: "go-test", Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSkipped)}
	failingLight := CheckResult{Name: "lint", Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionFailure)}
	approved := []ReviewComment{{Author: "alice", State: ReviewStateApproved}}

	for _, tc := range []struct {
		name      string
		draft     bool
		held      bool
		base      string
		mergeable *bool
		checks    []CheckResult
		reviews   []ReviewComment
		want      DisplayStatus
	}{
		{name: "non-draft light-only green is not passing", base: "main", mergeable: boolPtr(true), checks: []CheckResult{light}, want: DisplayStatusIdle},
		{name: "non-draft skipped required is not passing", base: "main", mergeable: boolPtr(true), checks: []CheckResult{light, skippedGoTest}, want: DisplayStatusIdle},
		{name: "non-draft approved without required run is not approved", base: "main", mergeable: boolPtr(true), checks: []CheckResult{light}, reviews: approved, want: DisplayStatusIdle},
		{name: "non-draft mergeable unknown is checking", base: "main", checks: []CheckResult{light}, want: DisplayStatusChecking},
		{name: "non-draft required ran is passing", base: "main", mergeable: boolPtr(true), checks: []CheckResult{light, goTest}, want: DisplayStatusPassing},
		{name: "exempt base light-only is passing", base: "staging", mergeable: boolPtr(true), checks: []CheckResult{light}, want: DisplayStatusPassing},
		{name: "non-draft failing light is failing", base: "main", mergeable: boolPtr(true), checks: []CheckResult{failingLight}, want: DisplayStatusFailing},
		{name: "draft light-only green is draft", draft: true, base: "main", mergeable: boolPtr(true), checks: []CheckResult{light}, want: DisplayStatusDraft},
		{name: "bossd-held draft failing light is failing", draft: true, held: true, base: "main", mergeable: boolPtr(true), checks: []CheckResult{failingLight}, want: DisplayStatusFailing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := &PRStatus{State: PRStateOpen, Draft: tc.draft, BaseBranch: tc.base, Mergeable: tc.mergeable, HeadSHA: "sha"}
			got := ComputeDisplayStatusWith(pr, tc.checks, tc.reviews, DisplayOptions{DraftHeldByBossd: tc.held, Required: required})
			if got.Status != tc.want {
				t.Fatalf("status = %v, want %v", got.Status, tc.want)
			}
		})
	}
}
