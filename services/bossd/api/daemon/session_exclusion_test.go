package server

import (
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/session"
)

func intPtr(i int) *int { return &i }

func TestNewActiveSessionKeys_IndexesAllKeys(t *testing.T) {
	keys := newActiveSessionKeys([]*models.Session{
		{ID: "a", PRNumber: intPtr(723), BranchName: "improve-existing-pr-filter", TrackerID: strPtr("BOS-43")},
		{ID: "b", BranchName: "lonely-branch"},
		nil,                              // must be skipped, not panic
		{ID: "c", TrackerID: strPtr("")}, // empty tracker id must be skipped
	})

	if _, ok := keys.prNumbers[723]; !ok {
		t.Errorf("pr 723 not indexed")
	}
	if _, ok := keys.branches["improve-existing-pr-filter"]; !ok {
		t.Errorf("branch not indexed")
	}
	if _, ok := keys.branches["lonely-branch"]; !ok {
		t.Errorf("branch-only session not indexed")
	}
	if _, ok := keys.trackerIDs["BOS-43"]; !ok {
		t.Errorf("tracker id not indexed")
	}
	if _, ok := keys.trackerIDs[""]; ok {
		t.Errorf("empty tracker id must not be indexed")
	}
}

func TestActiveSessionKeys_PlanningSessionSkipsOnlyTrackerKey(t *testing.T) {
	keys := newActiveSessionKeys([]*models.Session{
		{
			ID:         "planning",
			Plan:       " \n\t/boss-plan BOS-912\n\nBuild handoff",
			PRNumber:   intPtr(912),
			BranchName: "bos-912-plan",
			TrackerID:  strPtr("BOS-912"),
		},
	})

	if _, ok := keys.trackerIDs["BOS-912"]; ok {
		t.Fatalf("planning session tracker id was indexed")
	}
	if id, _, kind, ok := keys.duplicateSessionID(strPtr("BOS-912"), nil, ""); ok {
		t.Fatalf("duplicateSessionID matched planning tracker = (%q, %q, true), want miss", id, kind)
	}
	if id, _, kind, ok := keys.duplicateSessionID(nil, intPtr(912), ""); !ok || id != "planning" || kind != "PR" {
		t.Fatalf("duplicateSessionID PR = (%q, %q, %v), want (planning, PR, true)", id, kind, ok)
	}
	if id, _, kind, ok := keys.duplicateSessionID(nil, nil, "bos-912-plan"); !ok || id != "planning" || kind != "branch" {
		t.Fatalf("duplicateSessionID branch = (%q, %q, %v), want (planning, branch, true)", id, kind, ok)
	}
}

func TestIsPlanningSessionPlan_MatchesOnlyLeadingBossPlanCommand(t *testing.T) {
	tests := []struct {
		name string
		plan string
		want bool
	}{
		{name: "bare command", plan: "/boss-plan", want: true},
		{name: "leading whitespace and args", plan: " \n\t/boss-plan BOS-912", want: true},
		{name: "mentioned later", plan: "please run /boss-plan BOS-912", want: false},
		{name: "different command", plan: "/boss-build BOS-912", want: false},
		{name: "empty", plan: "", want: false},
		{name: "case sensitive", plan: "/Boss-plan BOS-912", want: false},
		{name: "prefix lookalike", plan: "/boss-planning BOS-912", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := session.IsPlanningSessionPlan(tt.plan); got != tt.want {
				t.Fatalf("IsPlanningSessionPlan(%q) = %v, want %v", tt.plan, got, tt.want)
			}
		})
	}
}

func TestExcludePRs_DropsByNumberAndBranch(t *testing.T) {
	keys := newActiveSessionKeys([]*models.Session{
		{ID: "a", PRNumber: intPtr(723)},
		{ID: "b", BranchName: "feature-x"},
	})
	prs := []vcs.PRSummary{
		{Number: 723, HeadBranch: "improve-existing-pr-filter"}, // dropped by number
		{Number: 999, HeadBranch: "feature-x"},                  // dropped by branch
		{Number: 100, HeadBranch: "keep-me"},                    // kept
	}

	got := keys.excludePRs(prs)

	if len(got) != 1 {
		t.Fatalf("got %d PRs, want 1: %+v", len(got), got)
	}
	if got[0].Number != 100 {
		t.Errorf("kept PR = %d, want 100", got[0].Number)
	}
}

func TestExcludePRs_EmptyKeysKeepsAll(t *testing.T) {
	keys := newActiveSessionKeys(nil)
	prs := []vcs.PRSummary{{Number: 1}, {Number: 2}}

	if got := keys.excludePRs(prs); len(got) != 2 {
		t.Fatalf("got %d PRs, want 2", len(got))
	}
}

func TestExcludeIssues_DropsByTrackerPRAndBranch(t *testing.T) {
	keys := newActiveSessionKeys([]*models.Session{
		{ID: "a", TrackerID: strPtr("BOS-1")},
		{ID: "b", PRNumber: intPtr(55)},
		{ID: "c", BranchName: "bos-3-branch"},
	})
	issues := []*pb.TrackerIssue{
		{ExternalId: "BOS-1"},                                 // dropped by tracker id
		{ExternalId: "BOS-2", PrNumber: 55},                   // dropped by matched PR
		{ExternalId: "BOS-3", ExistingBranch: "bos-3-branch"}, // dropped by branch
		{ExternalId: "BOS-4"},                                 // kept
		nil,                                                   // skipped, no panic
	}

	got := keys.excludeIssues(issues)

	if len(got) != 1 {
		t.Fatalf("got %d issues, want 1: %+v", len(got), got)
	}
	if got[0].ExternalId != "BOS-4" {
		t.Errorf("kept issue = %s, want BOS-4", got[0].ExternalId)
	}
}

func TestExcludeIssues_KeepsIssueWhenOnlyActiveSessionIsPlanning(t *testing.T) {
	keys := newActiveSessionKeys([]*models.Session{
		{ID: "planning", Plan: "/boss-plan BOS-912", TrackerID: strPtr("BOS-912")},
	})
	issues := []*pb.TrackerIssue{
		{ExternalId: "BOS-912"},
	}

	got := keys.excludeIssues(issues)

	if len(got) != 1 {
		t.Fatalf("got %d issues, want 1: %+v", len(got), got)
	}
	if got[0].ExternalId != "BOS-912" {
		t.Errorf("kept issue = %s, want BOS-912", got[0].ExternalId)
	}
}

func TestExcludeTargets_KeepsItemsForTerminalSessions(t *testing.T) {
	keys := newActiveSessionKeys([]*models.Session{
		{ID: "blocked", State: machine.Blocked, PRNumber: intPtr(10), BranchName: "blocked-branch", TrackerID: strPtr("BOS-10")},
		{ID: "merged", State: machine.Merged, PRNumber: intPtr(11), BranchName: "merged-branch", TrackerID: strPtr("BOS-11")},
		{ID: "closed", State: machine.Closed, PRNumber: intPtr(12), BranchName: "closed-branch", TrackerID: strPtr("BOS-12")},
	})

	prs := []vcs.PRSummary{
		{Number: 10, HeadBranch: "blocked-branch"},
		{Number: 11, HeadBranch: "merged-branch"},
		{Number: 12, HeadBranch: "closed-branch"},
	}
	if got := keys.excludePRs(prs); len(got) != len(prs) {
		t.Fatalf("got %d PRs, want %d: %+v", len(got), len(prs), got)
	}

	issues := []*pb.TrackerIssue{
		{ExternalId: "BOS-10", PrNumber: 10, ExistingBranch: "blocked-branch"},
		{ExternalId: "BOS-11", PrNumber: 11, ExistingBranch: "merged-branch"},
		{ExternalId: "BOS-12", PrNumber: 12, ExistingBranch: "closed-branch"},
	}
	if got := keys.excludeIssues(issues); len(got) != len(issues) {
		t.Fatalf("got %d issues, want %d: %+v", len(got), len(issues), got)
	}
}

func TestDuplicateSessionID_TrackerHitReturnsID(t *testing.T) {
	keys := newActiveSessionKeys([]*models.Session{
		{ID: "sess-1", TrackerID: strPtr("BOS-236")},
	})
	id, _, kind, ok := keys.duplicateSessionID(strPtr("BOS-236"), nil, "")
	if !ok || id != "sess-1" || kind != "tracker issue" {
		t.Fatalf("duplicateSessionID = (%q, %q, %v), want (sess-1, tracker issue, true)", id, kind, ok)
	}
}

func TestDuplicateSessionID_NonPlanningTrackerStillReturnsDuplicate(t *testing.T) {
	keys := newActiveSessionKeys([]*models.Session{
		{ID: "build", Plan: "/boss-build BOS-912", TrackerID: strPtr("BOS-912")},
	})

	id, _, kind, ok := keys.duplicateSessionID(strPtr("BOS-912"), nil, "")

	if !ok || id != "build" || kind != "tracker issue" {
		t.Fatalf("duplicateSessionID = (%q, %q, %v), want (build, tracker issue, true)", id, kind, ok)
	}
}

func TestDuplicateSessionID_PRHitReturnsID(t *testing.T) {
	keys := newActiveSessionKeys([]*models.Session{
		{ID: "sess-pr", PRNumber: intPtr(42)},
	})
	id, _, kind, ok := keys.duplicateSessionID(nil, intPtr(42), "")
	if !ok || id != "sess-pr" || kind != "PR" {
		t.Fatalf("duplicateSessionID = (%q, %q, %v), want (sess-pr, PR, true)", id, kind, ok)
	}
}

func TestDuplicateSessionID_BranchHitReturnsID(t *testing.T) {
	keys := newActiveSessionKeys([]*models.Session{
		{ID: "sess-br", BranchName: "feature-x"},
	})
	id, _, kind, ok := keys.duplicateSessionID(nil, nil, "feature-x")
	if !ok || id != "sess-br" || kind != "branch" {
		t.Fatalf("duplicateSessionID = (%q, %q, %v), want (sess-br, branch, true)", id, kind, ok)
	}
}

func TestDuplicateSessionID_TrackerWinsOverPRAndBranch(t *testing.T) {
	keys := newActiveSessionKeys([]*models.Session{
		{ID: "tracker-owner", TrackerID: strPtr("BOS-9")},
		{ID: "pr-owner", PRNumber: intPtr(7)},
		{ID: "branch-owner", BranchName: "shared-branch"},
	})
	id, _, kind, ok := keys.duplicateSessionID(strPtr("BOS-9"), intPtr(7), "shared-branch")
	if !ok || id != "tracker-owner" || kind != "tracker issue" {
		t.Fatalf("duplicateSessionID = (%q, %q, %v), want (tracker-owner, tracker issue, true)", id, kind, ok)
	}
}

func TestDuplicateSessionID_MissReturnsFalse(t *testing.T) {
	keys := newActiveSessionKeys([]*models.Session{
		{ID: "sess-1", TrackerID: strPtr("BOS-1"), PRNumber: intPtr(1), BranchName: "b1"},
	})
	if id, _, kind, ok := keys.duplicateSessionID(strPtr("BOS-2"), intPtr(2), "b2"); ok {
		t.Fatalf("duplicateSessionID = (%q, %q, true), want miss", id, kind)
	}
}

func TestDuplicateSessionID_EmptySignalsNeverHit(t *testing.T) {
	keys := newActiveSessionKeys([]*models.Session{
		{ID: "sess-1", TrackerID: strPtr("BOS-1"), PRNumber: intPtr(1), BranchName: "b1"},
	})
	// An all-empty create request (no tracker/pr/branch) must never false-hit.
	if id, _, kind, ok := keys.duplicateSessionID(nil, nil, ""); ok {
		t.Fatalf("duplicateSessionID(nil,nil,\"\") = (%q, %q, true), want miss", id, kind)
	}
	// An empty-string tracker id is treated as absent.
	if id, _, kind, ok := keys.duplicateSessionID(strPtr(""), nil, ""); ok {
		t.Fatalf("duplicateSessionID(empty tracker) = (%q, %q, true), want miss", id, kind)
	}
}

func TestDuplicateSessionID_TerminalStateDoesNotContribute(t *testing.T) {
	keys := newActiveSessionKeys([]*models.Session{
		{ID: "blocked", State: machine.Blocked, TrackerID: strPtr("BOS-99"), PRNumber: intPtr(99), BranchName: "blocked-branch"},
	})
	if id, _, kind, ok := keys.duplicateSessionID(strPtr("BOS-99"), intPtr(99), "blocked-branch"); ok {
		t.Fatalf("duplicateSessionID matched terminal session = (%q, %q, true), want miss", id, kind)
	}
}

func TestDuplicateSessionAlreadyExistsErrorIncludesBoundedFirstLinePlan(t *testing.T) {
	firstLine := strings.Repeat("a", duplicateSessionPlanSummaryLimit+5)

	tests := []struct {
		name string
		plan string
	}{
		{name: "crlf", plan: firstLine + "\r\nsecond line"},
		{name: "bare carriage return", plan: firstLine + "\rsecond line"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := duplicateSessionAlreadyExistsError("sess-1", tt.plan, "tracker issue", "repo-1")

			wantPlan := strings.Repeat("a", duplicateSessionPlanSummaryLimit) + ellipsis
			want := "active session sess-1 (plan: " + wantPlan + ") already exists for this tracker issue in repo repo-1; pass force to create another"
			if got := err.Error(); got != want {
				t.Fatalf("error = %q, want %q", got, want)
			}
			if strings.Contains(err.Error(), "second line") {
				t.Fatalf("error included second plan line: %q", err.Error())
			}
			if strings.Contains(err.Error(), "\r") {
				t.Fatalf("error included carriage return control character: %q", err.Error())
			}
		})
	}
}

func TestExcludeIssues_DropsByIssueBranchName(t *testing.T) {
	keys := newActiveSessionKeys([]*models.Session{
		{ID: "a", BranchName: "bos-5-branch"},
	})
	issues := []*pb.TrackerIssue{
		{ExternalId: "BOS-5", BranchName: "bos-5-branch"},
		{ExternalId: "BOS-6", BranchName: "keep-me"},
	}

	got := keys.excludeIssues(issues)

	if len(got) != 1 {
		t.Fatalf("got %d issues, want 1: %+v", len(got), got)
	}
	if got[0].ExternalId != "BOS-6" {
		t.Errorf("kept issue = %s, want BOS-6", got[0].ExternalId)
	}
}
