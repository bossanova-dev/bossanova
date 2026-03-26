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
		wantStatus              PRDisplayStatus
		wantHasFailure          bool
		wantHasChangesRequested bool
	}{
		{
			name:       "nil PR returns Idle",
			pr:         nil,
			wantStatus: PRDisplayStatusIdle,
		},
		{
			name:       "merged PR",
			pr:         &PRStatus{State: PRStateMerged},
			wantStatus: PRDisplayStatusMerged,
		},
		{
			name:       "closed PR",
			pr:         &PRStatus{State: PRStateClosed},
			wantStatus: PRDisplayStatusClosed,
		},
		{
			name:       "draft PR",
			pr:         &PRStatus{State: PRStateOpen, Draft: true, Mergeable: boolPtr(true)},
			wantStatus: PRDisplayStatusDraft,
		},
		{
			name: "draft takes priority over passing checks",
			pr:   &PRStatus{State: PRStateOpen, Draft: true, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			wantStatus: PRDisplayStatusDraft,
		},
		{
			name:       "conflict (not mergeable)",
			pr:         &PRStatus{State: PRStateOpen, Mergeable: boolPtr(false)},
			wantStatus: PRDisplayStatusConflict,
		},
		{
			name: "all checks failed",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionFailure)},
			},
			wantStatus: PRDisplayStatusFailing,
		},
		{
			name: "mixed: some passed, some failed, all completed",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionFailure)},
			},
			wantStatus: PRDisplayStatusFailing,
		},
		{
			name: "checks running, none failed yet",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusInProgress},
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			wantStatus:     PRDisplayStatusChecking,
			wantHasFailure: false,
		},
		{
			name: "checks running with some failures",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusInProgress},
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionFailure)},
			},
			wantStatus:     PRDisplayStatusChecking,
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
			wantStatus: PRDisplayStatusRejected,
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
			wantStatus: PRDisplayStatusApproved,
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
			wantStatus: PRDisplayStatusRejected,
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
			wantStatus: PRDisplayStatusPassing,
		},
		{
			name: "all checks green, no outstanding reviews = passing",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			wantStatus: PRDisplayStatusPassing,
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
			wantStatus: PRDisplayStatusApproved,
		},
		{
			name:       "open PR, no checks = idle",
			pr:         &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			wantStatus: PRDisplayStatusIdle,
		},
		{
			name:       "open PR, mergeable unknown, no checks = idle",
			pr:         &PRStatus{State: PRStateOpen},
			wantStatus: PRDisplayStatusIdle,
		},
		{
			name: "conflict takes priority over failing checks",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(false)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionFailure)},
			},
			wantStatus: PRDisplayStatusConflict,
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
			wantStatus: PRDisplayStatusMerged,
		},
		{
			name: "queued checks count as running",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusQueued},
			},
			wantStatus:     PRDisplayStatusChecking,
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
			wantStatus:              PRDisplayStatusChecking,
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
			wantStatus:              PRDisplayStatusChecking,
			wantHasFailure:          true,
			wantHasChangesRequested: true,
		},
		{
			name: "neutral conclusion is not a failure",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionNeutral)},
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			wantStatus: PRDisplayStatusPassing,
		},
		{
			name: "skipped conclusion is not a failure",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSkipped)},
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			wantStatus: PRDisplayStatusPassing,
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
