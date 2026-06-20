package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
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
