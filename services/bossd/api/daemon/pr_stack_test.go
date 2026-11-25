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

// Every read surface serves the open PR's live author and base as
// pr_author_login and pr_target_branch, and the stored values once the PR
// leaves the listing or the detector restarts. A live result for another PR
// number, or with empty values, never hides the stored ones.
func TestPRAuthorAndTargetServedOnEveryReadSurface_Sessions_AC277501(t *testing.T) {
	live := prstack.Result{PRNumber: 12, Author: "octocat", BaseBranch: "dev", DefaultBranch: "main"}
	for _, tc := range []struct {
		name                 string
		source               StackSource
		wantAuthor, wantBase string
	}{
		{"open PR serves the live values", stackResults{"s": live}, "octocat", "dev"},
		{"retarget not yet stored serves the live base", stackResults{"s": {PRNumber: 12, Author: "octocat", BaseBranch: "release", DefaultBranch: "main"}}, "octocat", "release"},
		{"PR left the listing serves the stored values", stackResults{}, "stored-author", "stored-target"},
		{"fresh daemon serves the stored values", nil, "stored-author", "stored-target"},
		{"result for another PR serves the stored values", stackResults{"s": {PRNumber: 11, Author: "hubot", BaseBranch: "old"}}, "stored-author", "stored-target"},
		{"empty live values keep the stored ones", stackResults{"s": {PRNumber: 12}}, "stored-author", "stored-target"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := 12
			row := &models.Session{ID: "s", RepoID: "r", State: machine.ReadyForReview, UpdatedAt: time.Now(), BaseBranch: "main", PRNumber: &n, PRAuthorLogin: "stored-author", PRTargetBranch: "stored-target"}
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
			streamed := SessionToProto(row)
			s.HydrateAttentionForStream(t.Context(), streamed)
			for _, got := range []*pb.Session{onlySession(t, listed.Msg.Sessions), fetched.Msg.Session, streamed} {
				if got.GetPrAuthorLogin() != tc.wantAuthor || got.GetPrTargetBranch() != tc.wantBase || got.GetBaseBranch() != "main" {
					t.Fatalf("served (author, target, base) = (%q, %q, %q); want (%q, %q, main)", got.GetPrAuthorLogin(), got.GetPrTargetBranch(), got.GetBaseBranch(), tc.wantAuthor, tc.wantBase)
				}
			}
		})
	}
}
