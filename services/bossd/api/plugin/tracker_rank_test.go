package plugin

import (
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestListSessionsTrackerRank_Sessions_AC239501(t *testing.T) {
	rankedAt := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	deadline := "2026-11-08"
	rank := &models.TrackerRank{Tier: 1, TierSource: "priority", DeadlineOn: &deadline, DeadlineSource: "issue", IsScheduled: true, UnblockedTicketCount: 7, RankedAt: rankedAt}
	srv := newTestHostServiceServer(&mockVCSProvider{})
	srv.repoStore = &fakeRepoStore{repos: []*models.Repo{{ID: "repo"}}}
	srv.sessionStore = &fakeSessionStoreWithListActive{sessionsByRepo: map[string][]*models.Session{"repo": {
		{ID: "ranked", RepoID: "repo", TrackerRank: rank}, {ID: "unranked", RepoID: "repo"},
	}}}
	srv.displayTracker = status.NewDisplayTracker()
	srv.agentChats = &fakeAgentChatStore{}
	resp, err := srv.ListSessions(t.Context(), &pb.HostServiceListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetSessions()) != 2 {
		t.Fatalf("sessions=%v", resp.GetSessions())
	}
	want := &pb.TrackerRank{Tier: 1, TierSource: "priority", DeadlineOn: &deadline, DeadlineSource: "issue", IsScheduled: true, UnblockedTicketCount: 7, RankedAt: timestamppb.New(rankedAt)}
	for _, sess := range resp.GetSessions() {
		if sess.GetId() == "ranked" && !proto.Equal(sess.GetTrackerRank(), want) {
			t.Fatalf("rank=%v want %v", sess.GetTrackerRank(), want)
		}
		if sess.GetId() == "unranked" && sess.GetTrackerRank() != nil {
			t.Fatal("unranked session gained a rank")
		}
	}
}
