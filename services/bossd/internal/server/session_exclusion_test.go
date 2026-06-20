package server

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
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
