package server

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/prstack"
	"github.com/recurser/bossd/internal/status"
	"google.golang.org/protobuf/proto"
)

type stackResults map[string]prstack.Result

func (r stackResults) Stack(sessionID string) (prstack.Result, bool) {
	got, ok := r[sessionID]
	return got, ok
}

// Every read surface serves the stack source's result for the session's
// open PR, and clears stale values when it has none.
func TestPRStackServedOnEveryReadSurface(t *testing.T) {
	stacked := prstack.Result{BaseBranch: "feat-a", DefaultBranch: "main", StackedOn: []prstack.Parent{
		{Number: 10, Title: "Parent", URL: "https://github.com/acme/widgets/pull/10", HeadBranch: "feat-a", SharedCommits: 2, Commits: 3, IsBaseBranch: true},
		{Number: 7, Title: "Older", URL: "https://github.com/acme/widgets/pull/7", HeadBranch: "feat-old", SharedCommits: 1, Commits: 1},
	}}
	full := &pb.Session{PrBaseBranch: "feat-a", RepoDefaultBranch: "main", PrStackedOn: []*pb.StackedPullRequest{
		{PrNumber: 10, Title: "Parent", Url: "https://github.com/acme/widgets/pull/10", HeadBranch: "feat-a", SharedCommitCount: 2, CommitCount: 3, IsBaseBranch: true},
		{PrNumber: 7, Title: "Older", Url: "https://github.com/acme/widgets/pull/7", HeadBranch: "feat-old", SharedCommitCount: 1, CommitCount: 1},
	}}
	for _, tc := range []struct {
		name   string
		source StackSource
		pr     bool
		want   *pb.Session
	}{
		{"stacked", stackResults{"s": stacked}, true, full},
		{"production base", stackResults{"s": {BaseBranch: "production", DefaultBranch: "main"}}, true, &pb.Session{PrBaseBranch: "production", RepoDefaultBranch: "main"}},
		{"no result", stackResults{}, true, &pb.Session{}},
		{"no PR", stackResults{"s": stacked}, false, &pb.Session{}},
		{"no source", nil, true, &pb.Session{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := &models.Session{ID: "s", RepoID: "r", State: machine.ReadyForReview, UpdatedAt: time.Now()}
			if tc.pr {
				n := 12
				row.PRNumber = &n
			}
			s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, status.NewDisplayTracker(), status.NewTracker())
			s.stacks = tc.source
			listed, err := s.ListSessions(t.Context(), connect.NewRequest(&pb.ListSessionsRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			s.sessions = &sessionStoreFake{sess: row}
			fetched, err := s.GetSession(t.Context(), connect.NewRequest(&pb.GetSessionRequest{Id: row.ID}))
			if err != nil {
				t.Fatal(err)
			}
			streamed := &pb.Session{Id: row.ID, PrBaseBranch: "stale", RepoDefaultBranch: "stale", PrStackedOn: []*pb.StackedPullRequest{{PrNumber: 1}}}
			s.HydrateAttentionForStream(t.Context(), streamed)
			for _, got := range []*pb.Session{onlySession(t, listed.Msg.Sessions), fetched.Msg.Session, streamed} {
				served := &pb.Session{PrBaseBranch: got.GetPrBaseBranch(), RepoDefaultBranch: got.GetRepoDefaultBranch(), PrStackedOn: got.GetPrStackedOn()}
				if !proto.Equal(served, tc.want) {
					t.Fatalf("served %v; want %v", served, tc.want)
				}
			}
		})
	}
}
