package vcs

import "testing"

func boolPtr(b bool) *bool { return &b }

func conclusionPtr(c CheckConclusion) *CheckConclusion { return &c }

func TestComputeDisplayStatus(t *testing.T) {
	tests := []struct {
		name                    string
		pr                      *PRStatus
		checks                  []CheckResult
		reviews                 []ReviewComment
		wantStatus              DisplayStatus
		wantHasFailure          bool
		wantHasChangesRequested bool
	}{
		{
			name:       "nil PR returns Idle",
			pr:         nil,
			wantStatus: DisplayStatusIdle,
		},
		{
			name:       "merged PR",
			pr:         &PRStatus{State: PRStateMerged},
			wantStatus: DisplayStatusMerged,
		},
		{
			name:       "closed PR",
			pr:         &PRStatus{State: PRStateClosed},
			wantStatus: DisplayStatusClosed,
		},
		{
			name:       "draft PR",
			pr:         &PRStatus{State: PRStateOpen, Draft: true, Mergeable: boolPtr(true)},
			wantStatus: DisplayStatusDraft,
		},
		{
			name: "draft takes priority over passing checks",
			pr:   &PRStatus{State: PRStateOpen, Draft: true, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			wantStatus: DisplayStatusDraft,
		},
		{
			name:       "conflict (not mergeable)",
			pr:         &PRStatus{State: PRStateOpen, Mergeable: boolPtr(false)},
			wantStatus: DisplayStatusConflict,
		},
		{
			name: "all checks failed",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionFailure)},
			},
			wantStatus: DisplayStatusFailing,
		},
		{
			name: "mixed: some passed, some failed, all completed",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionFailure)},
			},
			wantStatus: DisplayStatusFailing,
		},
		{
			name: "checks running, none failed yet",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusInProgress},
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			wantStatus:     DisplayStatusChecking,
			wantHasFailure: false,
		},
		{
			name: "checks running with some failures",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusInProgress},
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionFailure)},
			},
			wantStatus:     DisplayStatusChecking,
			wantHasFailure: true,
		},
		{
			name: "changes requested (rejected)",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			reviews: []ReviewComment{
				{Author: "alice", State: ReviewStateChangesRequested},
			},
			wantStatus: DisplayStatusRejected,
		},
		{
			name: "changes requested then approved by same author = passing",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			reviews: []ReviewComment{
				{Author: "alice", State: ReviewStateChangesRequested},
				{Author: "alice", State: ReviewStateApproved},
			},
			wantStatus: DisplayStatusApproved,
		},
		{
			name: "changes requested by one author, approved by different author = rejected",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			reviews: []ReviewComment{
				{Author: "alice", State: ReviewStateChangesRequested},
				{Author: "bob", State: ReviewStateApproved},
			},
			wantStatus: DisplayStatusRejected,
		},
		{
			name: "changes requested then dismissed by same author = passing",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			reviews: []ReviewComment{
				{Author: "alice", State: ReviewStateChangesRequested},
				{Author: "alice", State: ReviewStateDismissed},
			},
			wantStatus: DisplayStatusPassing,
		},
		{
			name: "all checks green, no outstanding reviews = passing",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			wantStatus: DisplayStatusPassing,
		},
		{
			name: "all checks green with approved review = approved",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			reviews: []ReviewComment{
				{Author: "alice", State: ReviewStateApproved},
			},
			wantStatus: DisplayStatusApproved,
		},
		{
			name:       "open PR, no checks = idle",
			pr:         &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			wantStatus: DisplayStatusIdle,
		},
		{
			name:       "open PR, mergeable unknown, no checks = checking",
			pr:         &PRStatus{State: PRStateOpen},
			wantStatus: DisplayStatusChecking,
		},
		{
			name: "conflict takes priority over failing checks",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(false)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionFailure)},
			},
			wantStatus: DisplayStatusConflict,
		},
		{
			name: "merged takes priority over everything",
			pr:   &PRStatus{State: PRStateMerged, Mergeable: boolPtr(false)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionFailure)},
			},
			reviews: []ReviewComment{
				{State: ReviewStateChangesRequested},
			},
			wantStatus: DisplayStatusMerged,
		},
		{
			name: "queued checks count as running",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusQueued},
			},
			wantStatus:     DisplayStatusChecking,
			wantHasFailure: false,
		},
		{
			name: "changes requested with checks still running",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusInProgress},
			},
			reviews: []ReviewComment{
				{Author: "alice", State: ReviewStateChangesRequested},
			},
			wantStatus:              DisplayStatusChecking,
			wantHasChangesRequested: true,
		},
		{
			name: "changes requested with some failures while checking",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusInProgress},
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionFailure)},
			},
			reviews: []ReviewComment{
				{Author: "alice", State: ReviewStateChangesRequested},
			},
			wantStatus:              DisplayStatusChecking,
			wantHasFailure:          true,
			wantHasChangesRequested: true,
		},
		{
			name: "mergeable unknown with passing checks = checking (not passing)",
			pr:   &PRStatus{State: PRStateOpen},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			wantStatus: DisplayStatusChecking,
		},
		{
			name: "mergeable unknown with approval = checking (not approved)",
			pr:   &PRStatus{State: PRStateOpen},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			reviews: []ReviewComment{
				{Author: "alice", State: ReviewStateApproved},
			},
			wantStatus: DisplayStatusChecking,
		},
		{
			name: "mergeable true with passing checks = passing (unchanged)",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			wantStatus: DisplayStatusPassing,
		},
		{
			name: "mergeable unknown with approval and no checks = checking (not idle)",
			pr:   &PRStatus{State: PRStateOpen},
			reviews: []ReviewComment{
				{Author: "alice", State: ReviewStateApproved},
			},
			wantStatus: DisplayStatusChecking,
		},
		{
			name: "mergeable unknown with failing checks = failing (not affected)",
			pr:   &PRStatus{State: PRStateOpen},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionFailure)},
			},
			wantStatus: DisplayStatusFailing,
		},
		{
			name: "mergeable unknown with changes requested = rejected (not affected)",
			pr:   &PRStatus{State: PRStateOpen},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			reviews: []ReviewComment{
				{Author: "alice", State: ReviewStateChangesRequested},
			},
			wantStatus: DisplayStatusRejected,
		},
		{
			name: "neutral conclusion is not a failure",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionNeutral)},
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			wantStatus: DisplayStatusPassing,
		},
		{
			name: "skipped conclusion is not a failure",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSkipped)},
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			wantStatus: DisplayStatusPassing,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeDisplayStatus(tt.pr, tt.checks, tt.reviews)
			if got.Status != tt.wantStatus {
				t.Errorf("Status = %d, want %d", got.Status, tt.wantStatus)
			}
			if got.HasFailures != tt.wantHasFailure {
				t.Errorf("HasFailures = %v, want %v", got.HasFailures, tt.wantHasFailure)
			}
			if got.HasChangesRequested != tt.wantHasChangesRequested {
				t.Errorf("HasChangesRequested = %v, want %v", got.HasChangesRequested, tt.wantHasChangesRequested)
			}
		})
	}
}
