package server

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/evidence"
	"github.com/recurser/bossd/internal/status"
	"github.com/rs/zerolog"
)

// publishedProofSessions serves the listed sessions to GetSession and to the
// upload's repo and PR lookup.
type publishedProofSessions struct {
	*listSessionsSessionStoreFake
}

func (f publishedProofSessions) Get(_ context.Context, id string) (*models.Session, error) {
	for _, sess := range f.sessions {
		if sess.ID == id {
			return sess, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (f publishedProofSessions) ListByRepoAndPR(_ context.Context, repoID string, pr int) ([]*db.SessionWithRepo, error) {
	var rows []*db.SessionWithRepo
	for _, row := range f.sessionRows() {
		if row.RepoID == repoID && row.PRNumber != nil && *row.PRNumber == pr {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// unreadComments is a PR comment source with no proof note yet; it counts
// the reads that reach GitHub.
type unreadComments struct{ reads int }

func (c *unreadComments) FindPRCommentsWithMarker(context.Context, string, int, string) ([]vcs.PRComment, error) {
	c.reads++
	return nil, nil
}

// publishedProofServer is an upload server whose sessions "bound" (PR pr) and
// "other" (PR pr+1) live in the evidence directory's worktree, with a real
// evidence reader.
func publishedProofServer(t *testing.T, s *Server, dir string, pr int) (*evidence.Reader, *unreadComments) {
	t.Helper()
	worktree := filepath.Dir(filepath.Dir(dir))
	other := pr + 1
	rows := []*models.Session{
		{ID: "bound", RepoID: "repo", WorktreePath: worktree, State: machine.ReadyForReview, PRNumber: &pr, UpdatedAt: time.Now()},
		{ID: "other", RepoID: "repo", WorktreePath: worktree, State: machine.ReadyForReview, PRNumber: &other, UpdatedAt: time.Now()},
	}
	listed := newListSessionsDisplayStatusTestServer(rows, nil, status.NewDisplayTracker(), status.NewTracker())
	repo := s.repos.(evidenceRepoList).repo
	s.repos = evidenceRepoList{repoStore: &listSessionsRepoStoreFake{repos: map[string]*models.Repo{repo.ID: repo}}, repo: repo}
	s.sessions = publishedProofSessions{listSessionsSessionStoreFake: listed.sessions.(*listSessionsSessionStoreFake)}
	s.agentChats, s.displayTracker, s.chatStatus = listed.agentChats, listed.displayTracker, listed.chatStatus
	comments := &unreadComments{}
	reader := evidence.NewReader(comments, zerolog.Nop())
	s.evidence = reader
	return reader, comments
}

// servedProofURLs reads pr_proof_url for each session from ListSessions and
// GetSession, failing when the two disagree.
func servedProofURLs(t *testing.T, s *Server) map[string]string {
	t.Helper()
	listed, err := s.ListSessions(t.Context(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	urls := map[string]string{}
	for _, sess := range listed.Msg.Sessions {
		fetched, err := s.GetSession(t.Context(), connect.NewRequest(&pb.GetSessionRequest{Id: sess.Id}))
		if err != nil {
			t.Fatal(err)
		}
		if fetched.Msg.Session.GetPrProofUrl() != sess.GetPrProofUrl() {
			t.Fatalf("%s: GetSession pr_proof_url %q, ListSessions %q", sess.Id, fetched.Msg.Session.GetPrProofUrl(), sess.GetPrProofUrl())
		}
		urls[sess.Id] = sess.GetPrProofUrl()
	}
	return urls
}

// An upload that publishes a proof page serves it on the session bound to the
// PR at once, with no GitHub read; a later comment read that has no proof
// note keeps it, and a new head clears it. An upload without a page records
// nothing.
func TestUploadEvidenceRecordsThePublishedPage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setup    func(t *testing.T) (*Server, string)
		request  func(dir string) *pb.UploadEvidenceRequest
		pr       int
		wantPage string
	}{
		{
			name: "local page",
			setup: func(t *testing.T) (*Server, string) {
				return pagesServer(t, newRecordingEndpoint(t).server.URL, true)
			},
			request: func(dir string) *pb.UploadEvidenceRequest {
				return &pb.UploadEvidenceRequest{EvidenceDir: dir, PrNumber: 3, HeadSha: evidenceTestSHA, Objects: []*pb.EvidenceObject{{Name: "index.html", Path: "proof.html"}}}
			},
			pr:       3,
			wantPage: "https://proof.example/acme/widgets/3/" + evidenceTestSHA + "/index.html",
		},
		{
			name: "hosted page",
			setup: func(t *testing.T) (*Server, string) {
				s, dir := evidenceServer(t, newRecordingEndpoint(t).server.URL)
				s.evidenceBucketOpener = &fakeHostedOpener{local: s.evidenceBucketOpener}
				return s, dir
			},
			request: func(dir string) *pb.UploadEvidenceRequest {
				return &pb.UploadEvidenceRequest{EvidenceDir: dir, PrNumber: 42, HeadSha: evidenceTestSHA, Destination: pb.EvidenceDestination_EVIDENCE_DESTINATION_HOSTED, Objects: []*pb.EvidenceObject{{Name: "index.html", Path: "index.html"}}}
			},
			pr:       42,
			wantPage: "https://app.example/review/acme/widgets/42?head=" + evidenceTestSHA,
		},
		{
			name: "no page",
			setup: func(t *testing.T) (*Server, string) {
				return evidenceServer(t, newRecordingEndpoint(t).server.URL)
			},
			request: func(dir string) *pb.UploadEvidenceRequest {
				return &pb.UploadEvidenceRequest{EvidenceDir: dir, PrNumber: 3, HeadSha: evidenceTestSHA, Objects: []*pb.EvidenceObject{{Name: "manifest.json", Path: "manifest.json"}}}
			},
			pr: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, dir := tc.setup(t)
			hostedPage(t, dir, hostedContents, "index.html")
			reader, comments := publishedProofServer(t, s, dir, tc.pr)
			if _, err := s.UploadEvidence(t.Context(), connect.NewRequest(tc.request(dir))); err != nil {
				t.Fatal(err)
			}
			if got := servedProofURLs(t, s); got["bound"] != tc.wantPage || got["other"] != "" {
				t.Fatalf("pr_proof_url = %v; want bound %q and other empty", got, tc.wantPage)
			}
			if comments.reads != 0 {
				t.Fatalf("serving the page read GitHub %d times", comments.reads)
			}
			reader.Observe(t.Context(), evidence.Observation{SessionID: "bound", PRNumber: tc.pr, HeadSHA: evidenceTestSHA, IsPROpen: true})
			if got := servedProofURLs(t, s)["bound"]; comments.reads != 1 || got != tc.wantPage {
				t.Fatalf("after a comment read with no proof note: pr_proof_url %q, reads %d; want %q after one read", got, comments.reads, tc.wantPage)
			}
			reader.Observe(t.Context(), evidence.Observation{SessionID: "bound", PRNumber: tc.pr, HeadSHA: strings.Repeat("e", 40), IsPROpen: true})
			if got := servedProofURLs(t, s)["bound"]; got != "" {
				t.Fatalf("after a new head: pr_proof_url %q; want it cleared", got)
			}
		})
	}
}
