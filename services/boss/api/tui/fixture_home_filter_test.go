package tui

import (
	"context"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func filteredHome(t *testing.T) HomeModel {
	t.Helper()
	h := NewHomeModel(nil, context.Background(), nil)
	h.Loading = false
	h.Width, h.Height = 120, 30
	h.RepoCount = 1
	h.Sessions = []*pb.Session{
		{Id: "hidden", Title: "Hidden", RepoDisplayName: "other"},
		{Id: "review", Title: "Alpha review", RepoDisplayName: "app", BranchName: "feature", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, Sentence: "Please check the diff"}},
		{Id: "progress", Title: "Alpha build", RepoDisplayName: "app", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: "building"}},
	}
	h.BuildTableRows()
	return h
}
