package vcs

import (
	"reflect"
	"strings"
	"testing"
)

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
		wantChangesRequestedBy  []string
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
			name:       "closed draft PR",
			pr:         &PRStatus{State: PRStateClosed, Draft: true},
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
			name:       "rebase-only block is not a strategy-independent conflict",
			pr:         &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true), Rebaseable: boolPtr(false)},
			wantStatus: DisplayStatusIdle,
		},
		{
			name: "review required block is not a conflict",
			pr: &PRStatus{
				State:             PRStateOpen,
				Mergeable:         boolPtr(true),
				Rebaseable:        boolPtr(false),
				MergeStateStatus:  MergeStateStatusBlocked,
				LatestReviewState: ReviewStateRequired,
			},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			wantStatus: DisplayStatusReview,
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
			// Real-world case from PR #234: 3 successful jobs and one
			// CANCELLED job (a sibling job in the same workflow failed
			// first). Previously we treated CANCELLED as benign and the
			// PR showed as passing — invisible to repair. Treat it as a
			// failure so repair fires.
			name: "cancelled check is treated as failure",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionCancelled)},
			},
			wantStatus: DisplayStatusFailing,
		},
		{
			name: "timed-out check is treated as failure",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionTimedOut)},
			},
			wantStatus: DisplayStatusFailing,
		},
		{
			name: "neutral and skipped do NOT count as failures",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionNeutral)},
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSkipped)},
			},
			wantStatus: DisplayStatusPassing,
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
			wantStatus:             DisplayStatusRejected,
			wantChangesRequestedBy: []string{"alice"},
		},
		{
			name: "review required decision blocks passing",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true), LatestReviewState: ReviewStateChangesRequested},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			// changes-requested is signalled via pr.LatestReviewState, which
			// carries no author login — the list stays empty.
			wantStatus:             DisplayStatusRejected,
			wantChangesRequestedBy: nil,
		},
		{
			name: "multiple distinct authors requesting changes are sorted and deduped",
			pr:   &PRStatus{State: PRStateOpen, Mergeable: boolPtr(true)},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			reviews: []ReviewComment{
				{Author: "charlie", State: ReviewStateChangesRequested},
				{Author: "alice", State: ReviewStateChangesRequested},
				{Author: "alice", State: ReviewStateChangesRequested},
			},
			wantStatus:             DisplayStatusRejected,
			wantChangesRequestedBy: []string{"alice", "charlie"},
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
			wantStatus:             DisplayStatusRejected,
			wantChangesRequestedBy: []string{"alice"},
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
			name: "review required block overrides partial approval",
			pr: &PRStatus{
				State:               PRStateOpen,
				Mergeable:           boolPtr(true),
				MergeStateStatus:    MergeStateStatusBlocked,
				LatestReviewState:   ReviewStateApproved,
				ReviewDecisionState: ReviewStateRequired,
			},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			reviews: []ReviewComment{
				{Author: "alice", State: ReviewStateApproved},
			},
			wantStatus: DisplayStatusReview,
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
			wantChangesRequestedBy:  []string{"alice"},
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
			wantChangesRequestedBy:  []string{"alice"},
		},
		{
			name: "failing checks override review required",
			pr: &PRStatus{
				State:             PRStateOpen,
				Mergeable:         boolPtr(true),
				Rebaseable:        boolPtr(false),
				MergeStateStatus:  MergeStateStatusBlocked,
				LatestReviewState: ReviewStateRequired,
			},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionFailure)},
			},
			wantStatus: DisplayStatusFailing,
		},
		{
			name: "running checks override review required",
			pr: &PRStatus{
				State:             PRStateOpen,
				Mergeable:         boolPtr(true),
				Rebaseable:        boolPtr(false),
				MergeStateStatus:  MergeStateStatusBlocked,
				LatestReviewState: ReviewStateRequired,
			},
			checks: []CheckResult{
				{Status: CheckStatusInProgress},
			},
			wantStatus: DisplayStatusChecking,
		},
		{
			name: "changes requested overrides review required",
			pr: &PRStatus{
				State:             PRStateOpen,
				Mergeable:         boolPtr(true),
				Rebaseable:        boolPtr(false),
				MergeStateStatus:  MergeStateStatusBlocked,
				LatestReviewState: ReviewStateRequired,
			},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			reviews: []ReviewComment{
				{Author: "alice", State: ReviewStateChangesRequested},
			},
			wantStatus:             DisplayStatusRejected,
			wantChangesRequestedBy: []string{"alice"},
		},
		{
			name: "unknown mergeability does not show review",
			pr: &PRStatus{
				State:             PRStateOpen,
				MergeStateStatus:  MergeStateStatusBlocked,
				LatestReviewState: ReviewStateRequired,
			},
			checks: []CheckResult{
				{Status: CheckStatusCompleted, Conclusion: conclusionPtr(CheckConclusionSuccess)},
			},
			wantStatus: DisplayStatusChecking,
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
			wantStatus:             DisplayStatusRejected,
			wantChangesRequestedBy: []string{"alice"},
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
			if len(got.ChangesRequestedBy) != 0 || len(tt.wantChangesRequestedBy) != 0 {
				if !reflect.DeepEqual(got.ChangesRequestedBy, tt.wantChangesRequestedBy) {
					t.Errorf("ChangesRequestedBy = %v, want %v", got.ChangesRequestedBy, tt.wantChangesRequestedBy)
				}
			}
		})
	}
}

func TestMergeGateSlug(t *testing.T) {
	tests := []struct {
		gate MergeGate
		want string
	}{
		{MergeGateUnspecified, "unspecified"},
		{MergeGateNone, "none"},
		{MergeGateReview, "review"},
		{MergeGateCI, "ci"},
		{MergeGatePending, "pending"},
		{MergeGateConflict, "conflict"},
		{MergeGateBaseSync, "base_sync"},
		{MergeGateDraft, "draft"},
	}
	for _, tt := range tests {
		if got := tt.gate.Slug(); got != tt.want {
			t.Errorf("MergeGate(%d).Slug() = %q, want %q", tt.gate, got, tt.want)
		}
	}
}

func TestDeriveMergeBlock(t *testing.T) {
	tests := []struct {
		name               string
		status             DisplayStatus
		hasFailures        bool
		changesRequestedBy []string
		wantGate           MergeGate
		wantDetailEmpty    bool
		wantReviewers      []string
	}{
		{
			name:               "rejected with known reviewer",
			status:             DisplayStatusRejected,
			changesRequestedBy: []string{"alice"},
			wantGate:           MergeGateReview,
			wantReviewers:      []string{"alice"},
		},
		{
			name:          "rejected with unknown reviewer",
			status:        DisplayStatusRejected,
			wantGate:      MergeGateReview,
			wantReviewers: nil,
		},
		{
			name:        "failing maps to ci",
			status:      DisplayStatusFailing,
			hasFailures: true,
			wantGate:    MergeGateCI,
		},
		{
			name:     "checking maps to pending",
			status:   DisplayStatusChecking,
			wantGate: MergeGatePending,
		},
		{
			name:     "conflict maps to conflict",
			status:   DisplayStatusConflict,
			wantGate: MergeGateConflict,
		},
		{
			name:     "draft maps to draft",
			status:   DisplayStatusDraft,
			wantGate: MergeGateDraft,
		},
		{
			name:            "passing is not blocked",
			status:          DisplayStatusPassing,
			wantGate:        MergeGateNone,
			wantDetailEmpty: true,
		},
		{
			name:            "approved is not blocked",
			status:          DisplayStatusApproved,
			wantGate:        MergeGateNone,
			wantDetailEmpty: true,
		},
		{
			name:            "review is not blocked",
			status:          DisplayStatusReview,
			wantGate:        MergeGateNone,
			wantDetailEmpty: true,
		},
		{
			name:            "idle is not blocked",
			status:          DisplayStatusIdle,
			wantGate:        MergeGateNone,
			wantDetailEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DeriveMergeBlock(tt.status, tt.hasFailures, tt.changesRequestedBy)
			if got.Gate != tt.wantGate {
				t.Errorf("Gate = %d, want %d", got.Gate, tt.wantGate)
			}
			if got.Status != tt.status {
				t.Errorf("Status = %d, want %d", got.Status, tt.status)
			}
			if tt.wantDetailEmpty && got.Detail != "" {
				t.Errorf("Detail = %q, want empty", got.Detail)
			}
			if !tt.wantDetailEmpty && got.Detail == "" {
				t.Errorf("Detail is empty, want non-empty")
			}
			if !reflect.DeepEqual(got.BlockingReviewers, tt.wantReviewers) {
				t.Errorf("BlockingReviewers = %v, want %v", got.BlockingReviewers, tt.wantReviewers)
			}
		})
	}
}

func TestDeriveMergeBlockReviewDetail(t *testing.T) {
	// With a known reviewer the detail names the login and count and warns
	// that the daemon's own tracker may diverge from GitHub's mergeability.
	withLogin := DeriveMergeBlock(DisplayStatusRejected, false, []string{"alice"})
	if !strings.Contains(withLogin.Detail, "alice") {
		t.Errorf("detail %q should name the reviewer", withLogin.Detail)
	}
	if !strings.Contains(withLogin.Detail, "trusts its own") || !strings.Contains(withLogin.Detail, "GitHub may") {
		t.Errorf("detail %q should carry the divergence note", withLogin.Detail)
	}

	// Without a login the detail degrades gracefully but keeps the note.
	noLogin := DeriveMergeBlock(DisplayStatusRejected, false, nil)
	if strings.Contains(noLogin.Detail, "from ") {
		t.Errorf("detail %q should omit the 'from ...' clause when no login is known", noLogin.Detail)
	}
	if !strings.Contains(noLogin.Detail, "trusts its own") || !strings.Contains(noLogin.Detail, "GitHub may") {
		t.Errorf("detail %q should carry the divergence note", noLogin.Detail)
	}
}

// TestReviewBlockDetailSingularReviewer pins the singular boundary: one
// outstanding reviewer must be described as one "review", not "reviews".
// This catches display.go's len(changesRequestedBy) > 1 conditional.
func TestReviewBlockDetailSingularReviewer(t *testing.T) {
	got := reviewBlockDetail([]string{"alice"})
	want := "1 outstanding changes-requested review from alice; " + divergenceNote + "; " + remedyNote
	if got != want {
		t.Errorf("reviewBlockDetail() = %q, want %q", got, want)
	}
}

// TestReviewBlockDetailRemedyClause pins the remedy clause an operator needs to
// act on a review block: what clears it, and — since BOS-665 made an outdated
// thread non-blocking — that only the unresolved threads GitHub has NOT marked
// outdated need resolving. It must not claim that any later review clears the
// block: a later empty COMMENTED review does not.
//
// The absent-substring check is the load-bearing half. The detail used to tell
// operators that outdated threads GitHub hides still block, sending them
// hunting for threads that are now irrelevant; that instruction must not
// survive the behaviour change.
func TestReviewBlockDetailRemedyClause(t *testing.T) {
	for _, tc := range []struct {
		name   string
		logins []string
	}{
		{name: "named reviewer", logins: []string{"alice"}},
		{name: "no login known", logins: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := reviewBlockDetail(tc.logins)
			for _, want := range []string{
				divergenceNote,
				"every unresolved, non-outdated review thread",
				"outdated threads are already non-blocking",
				"newer approving/dismissing review",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("reviewBlockDetail(%v) = %q, want it to contain %q", tc.logins, got, want)
				}
			}
			if strings.Contains(got, "outdated threads GitHub hides") {
				t.Errorf("reviewBlockDetail(%v) = %q, must no longer tell operators that outdated threads block", tc.logins, got)
			}
		})
	}
}

// TestReviewBlockDetailScopesThreadResolutionRemedy pins that the two remedies
// are not offered as interchangeable. Thread resolution only clears a block
// SYNTHESIZED out of a bot's unresolved threads; a native CHANGES_REQUESTED
// review (an ordinary human request, or a bot that submits one directly) is
// returned with that state regardless of thread state, so resolving its threads
// changes nothing and the merge stays blocked. Stating it unconditionally sends
// an operator to a remedy that cannot work for their block.
//
// The approval/dismissal remedy is the universally valid one, so it must be
// stated unconditionally and must come FIRST — an operator who reads only the
// opening clause must still be told something true.
func TestReviewBlockDetailScopesThreadResolutionRemedy(t *testing.T) {
	for _, logins := range [][]string{{"alice"}, {"cursor[bot]"}, nil} {
		got := reviewBlockDetail(logins)

		threadIdx := strings.Index(got, "every unresolved, non-outdated review thread")
		approvalIdx := strings.Index(got, "newer approving/dismissing review")
		if threadIdx < 0 || approvalIdx < 0 {
			t.Fatalf("reviewBlockDetail(%v) = %q, want both remedies present", logins, got)
		}
		if approvalIdx > threadIdx {
			t.Errorf("reviewBlockDetail(%v) = %q: the unconditional approval/dismissal remedy must precede the conditional thread-resolution one", logins, got)
		}

		// The thread-resolution remedy must be guarded by a condition rather
		// than offered flatly. Without this the clause reads as universal.
		guard := got[:threadIdx]
		if !strings.Contains(guard, "when the block was synthesized") {
			t.Errorf("reviewBlockDetail(%v) = %q: thread-resolution remedy is not scoped to synthesized bot blocks", logins, got)
		}
	}
}
