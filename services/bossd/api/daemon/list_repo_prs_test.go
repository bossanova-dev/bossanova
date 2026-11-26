package server

import (
	"context"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/status"
	"github.com/rs/zerolog"
)

func TestListRepoPRs_ExcludesPRsWithActiveSession(t *testing.T) {
	repo := &models.Repo{ID: "repo-1", OriginURL: "https://github.com/test/repo"}
	provider := &listSessionsVCSProviderFake{
		openPRs: []vcs.PRSummary{
			{Number: 723, HeadBranch: "improve-existing-pr-filter", Title: "taken by number", State: vcs.PRStateOpen},
			{Number: 999, HeadBranch: "feature-x", Title: "taken by branch", State: vcs.PRStateOpen},
			{Number: 100, HeadBranch: "keep-me", Title: "free", State: vcs.PRStateOpen},
		},
	}
	s := &Server{
		repos: &listSessionsRepoStoreFake{repos: map[string]*models.Repo{"repo-1": repo}},
		sessions: &listSessionsSessionStoreFake{sessions: []*models.Session{
			{ID: "a", RepoID: "repo-1", PRNumber: intPtr(723)},
			{ID: "b", RepoID: "repo-1", BranchName: "feature-x"},
		}},
		provider: provider,
		logger:   zerolog.Nop(),
	}

	resp, err := s.ListRepoPRs(context.Background(), connect.NewRequest(&pb.ListRepoPRsRequest{RepoId: "repo-1"}))
	if err != nil {
		t.Fatalf("ListRepoPRs: %v", err)
	}

	got := resp.Msg.PullRequests
	if len(got) != 1 {
		t.Fatalf("got %d PRs, want 1: %+v", len(got), got)
	}
	if got[0].Number != 100 {
		t.Errorf("kept PR = %d, want 100", got[0].Number)
	}
}

// listRepoPRsServer serves prs for repo-1 with sessions as its active sessions.
func listRepoPRsServer(prs []vcs.PRSummary, sessions ...*models.Session) *Server {
	repo := &models.Repo{ID: "repo-1", OriginURL: "https://github.com/test/repo"}
	return &Server{
		repos:          &listSessionsRepoStoreFake{repos: map[string]*models.Repo{"repo-1": repo}},
		sessions:       &listSessionsSessionStoreFake{sessions: sessions},
		provider:       &listSessionsVCSProviderFake{openPRs: prs},
		displayTracker: status.NewDisplayTracker(),
		logger:         zerolog.Nop(),
	}
}

func listRepoPRs(t *testing.T, s *Server, query *pb.PRListQuery) []*pb.PRSummary {
	t.Helper()
	resp, err := s.ListRepoPRs(context.Background(), connect.NewRequest(&pb.ListRepoPRsRequest{RepoId: "repo-1", Query: query}))
	if err != nil {
		t.Fatalf("ListRepoPRs(%v): %v", query, err)
	}
	return resp.Msg.PullRequests
}

func prNumbers(prs []*pb.PRSummary) []int32 {
	out := make([]int32, len(prs))
	for i, pr := range prs {
		out[i] = pr.Number
	}
	return out
}

func TestListRepoPRs_OwnerFilterReportsSession(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	prs := []vcs.PRSummary{
		{Number: 1, HeadBranch: "owned-live"},
		{Number: 2, HeadBranch: "owned-merged"},
		{Number: 3, HeadBranch: "free"},
		{Number: 4, HeadBranch: "branch-of-other"},
	}
	s := listRepoPRsServer(prs,
		&models.Session{ID: "live", PRNumber: intPtr(1), State: machine.ImplementingPlan},
		&models.Session{ID: "merged", PRNumber: intPtr(2), State: machine.Merged},
		// #4 matches "by-branch" on its head branch but "by-number" on its number.
		&models.Session{ID: "by-branch", BranchName: "branch-of-other", State: machine.Merged, UpdatedAt: base.Add(time.Hour)},
		&models.Session{ID: "by-number", PRNumber: intPtr(4), State: machine.Merged, UpdatedAt: base},
	)
	s.displayTracker.Set("live", vcs.DisplayInfo{Status: vcs.DisplayStatusFailing})

	all := listRepoPRs(t, s, &pb.PRListQuery{OwnerFilter: pb.PRListOwnerFilter_PR_LIST_OWNER_FILTER_ALL})
	wantOwners := map[int32]string{1: "live", 2: "merged", 3: "", 4: "by-number"}
	if len(all) != 4 {
		t.Fatalf("ALL returned %v, want 4 PRs", prNumbers(all))
	}
	for _, pr := range all {
		if pr.SessionId != wantOwners[pr.Number] {
			t.Errorf("ALL: PR #%d session_id = %q, want %q", pr.Number, pr.SessionId, wantOwners[pr.Number])
		}
	}
	if all[0].SessionDisplayStatus != pb.DisplayStatus_DISPLAY_STATUS_FAILING {
		t.Errorf("PR #1 session_display_status = %v, want FAILING", all[0].SessionDisplayStatus)
	}
	if all[2].SessionDisplayStatus != pb.DisplayStatus_DISPLAY_STATUS_UNSPECIFIED {
		t.Errorf("unowned PR #3 session_display_status = %v, want UNSPECIFIED", all[2].SessionDisplayStatus)
	}

	def := listRepoPRs(t, s, nil)
	if got := prNumbers(def); !slices.Equal(got, []int32{2, 3, 4}) || def[0].SessionId != "merged" {
		t.Errorf("default returned %v (first owner %q), want [2 3 4] with #2 owned by merged", got, def[0].SessionId)
	}

	unowned := listRepoPRs(t, s, &pb.PRListQuery{OwnerFilter: pb.PRListOwnerFilter_PR_LIST_OWNER_FILTER_UNOWNED})
	if got := prNumbers(unowned); !slices.Equal(got, []int32{3}) || unowned[0].SessionId != "" {
		t.Errorf("UNOWNED returned %v, want only unowned #3", got)
	}
}

func TestListRepoPRs_PrefersDuplicateBlockingOwner(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := listRepoPRsServer([]vcs.PRSummary{{Number: 7, HeadBranch: "b"}},
		&models.Session{ID: "newer-merged", PRNumber: intPtr(7), State: machine.Merged, UpdatedAt: base.Add(2 * time.Hour)},
		&models.Session{ID: "older-live", PRNumber: intPtr(7), State: machine.AwaitingChecks, UpdatedAt: base},
		&models.Session{ID: "newest-live", PRNumber: intPtr(7), State: machine.AwaitingChecks, UpdatedAt: base.Add(time.Hour)},
	)
	got := listRepoPRs(t, s, &pb.PRListQuery{OwnerFilter: pb.PRListOwnerFilter_PR_LIST_OWNER_FILTER_ALL})
	if len(got) != 1 || got[0].SessionId != "newest-live" {
		t.Fatalf("owner = %v, want newest-live", got)
	}
}

func TestListRepoPRs_FiltersOrdersAndLimits(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }
	prs := []vcs.PRSummary{
		{Number: 10, Author: "Alice", BaseBranch: "main", CreatedAt: day(3), UpdatedAt: day(4)},
		{Number: 11, Author: "app/dependabot", BaseBranch: "main", CreatedAt: day(1), UpdatedAt: day(9)},
		{Number: 12, Author: "alice", BaseBranch: "release", CreatedAt: day(2), UpdatedAt: day(2)},
		{Number: 13, Author: "bob", BaseBranch: "main", CreatedAt: day(3), UpdatedAt: day(5)},
	}
	s := listRepoPRsServer(prs)
	cases := []struct {
		name  string
		query *pb.PRListQuery
		want  []int32
	}{
		{"no query keeps provider order", nil, []int32{10, 11, 12, 13}},
		{"author is case-insensitive", &pb.PRListQuery{Author: "ALICE"}, []int32{10, 12}},
		{"bot matches bare login", &pb.PRListQuery{Author: "dependabot"}, []int32{11}},
		{"bot matches app form", &pb.PRListQuery{Author: "app/dependabot"}, []int32{11}},
		{"base branch is exact", &pb.PRListQuery{BaseBranch: "release"}, []int32{12}},
		{"created desc, ties by higher number", &pb.PRListQuery{Order: pb.PRListOrder_PR_LIST_ORDER_CREATED_DESC}, []int32{13, 10, 12, 11}},
		{"created asc", &pb.PRListQuery{Order: pb.PRListOrder_PR_LIST_ORDER_CREATED_ASC}, []int32{11, 12, 13, 10}},
		{"updated desc", &pb.PRListQuery{Order: pb.PRListOrder_PR_LIST_ORDER_UPDATED_DESC}, []int32{11, 13, 10, 12}},
		{"updated asc", &pb.PRListQuery{Order: pb.PRListOrder_PR_LIST_ORDER_UPDATED_ASC}, []int32{12, 10, 13, 11}},
		{"limit applies after filter and order", &pb.PRListQuery{BaseBranch: "main", Order: pb.PRListOrder_PR_LIST_ORDER_UPDATED_ASC, Limit: 2}, []int32{10, 13}},
		{"zero or negative limit is unlimited", &pb.PRListQuery{Limit: -1}, []int32{10, 11, 12, 13}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := prNumbers(listRepoPRs(t, s, tc.query)); !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestListRepoPRs_ReturnsNewFields(t *testing.T) {
	created := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	updated := created.Add(36 * time.Hour)
	s := listRepoPRsServer([]vcs.PRSummary{
		{Number: 5, Title: "t", HeadBranch: "h", BaseBranch: "main", State: vcs.PRStateOpen, Author: "a", HeadSHA: "abc123", IsDraft: true, CreatedAt: created, UpdatedAt: updated},
		{Number: 6, HeadBranch: "no-dates"},
	})
	got := listRepoPRs(t, s, nil)
	pr := got[0]
	if pr.HeadSha != "abc123" || !pr.IsDraft || pr.BaseBranch != "main" || pr.Title != "t" || pr.Author != "a" || pr.State != pb.PRState_PR_STATE_OPEN {
		t.Errorf("PR #5 = %+v, want provider fields", pr)
	}
	if !pr.GetCreatedAt().AsTime().Equal(created) || !pr.GetUpdatedAt().AsTime().Equal(updated) {
		t.Errorf("PR #5 times = %v / %v, want %v / %v", pr.GetCreatedAt(), pr.GetUpdatedAt(), created, updated)
	}
	if got[1].CreatedAt != nil || got[1].UpdatedAt != nil {
		t.Errorf("PR #6 times = %v / %v, want unset for zero provider times", got[1].CreatedAt, got[1].UpdatedAt)
	}
}

func TestListRepoPRs_RejectsUnknownOrder(t *testing.T) {
	s := listRepoPRsServer(nil)
	for name, query := range map[string]*pb.PRListQuery{
		"order":        {Order: pb.PRListOrder(99)},
		"owner_filter": {OwnerFilter: pb.PRListOwnerFilter(99)},
	} {
		_, err := s.ListRepoPRs(context.Background(), connect.NewRequest(&pb.ListRepoPRsRequest{RepoId: "repo-1", Query: query}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("undefined %s: err = %v, want InvalidArgument", name, err)
		}
	}
}
