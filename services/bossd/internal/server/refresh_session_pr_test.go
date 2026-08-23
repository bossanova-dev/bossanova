package server

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
	"github.com/rs/zerolog"
)

func TestRefreshSessionPRUpdatesStaleDraftSnapshot(t *testing.T) {
	tracker := status.NewDisplayTracker()
	tracker.Set("s1", vcs.DisplayInfo{Status: vcs.DisplayStatusDraft})
	provider := &refreshPRProvider{
		prStatus: &vcs.PRStatus{
			State:            vcs.PRStateOpen,
			Mergeable:        boolPtr(true),
			MergeStateStatus: vcs.MergeStateStatusClean,
			HeadSHA:          "ready-sha",
		},
		checks: []vcs.CheckResult{{
			Status:     vcs.CheckStatusCompleted,
			Conclusion: checkConclusionPtr(vcs.CheckConclusionSuccess),
		}},
	}
	srv := newRefreshPRServer(tracker, provider, refreshPRSession())

	resp, err := srv.RefreshSessionPR(context.Background(), connect.NewRequest(&pb.RefreshSessionPRRequest{Id: strPtr("s1")}))
	if err != nil {
		t.Fatalf("RefreshSessionPR: %v", err)
	}
	if got := resp.Msg.GetSession().GetDisplayStatus(); got != pb.DisplayStatus_DISPLAY_STATUS_PASSING {
		t.Fatalf("display_status = %v, want PASSING", got)
	}
	if got := tracker.Get("s1"); got == nil || got.Status != vcs.DisplayStatusPassing || got.HeadSHA != "ready-sha" {
		t.Fatalf("tracker entry = %+v, want passing at ready-sha", got)
	}
	if provider.prStatusCalls != 1 || provider.checkCalls != 1 || provider.reviewCalls != 1 {
		t.Fatalf("provider calls = status:%d checks:%d reviews:%d, want 1/1/1",
			provider.prStatusCalls, provider.checkCalls, provider.reviewCalls)
	}
}

func TestRefreshSessionPRProviderErrorPreservesCachedSnapshot(t *testing.T) {
	tracker := status.NewDisplayTracker()
	tracker.Set("s1", vcs.DisplayInfo{Status: vcs.DisplayStatusDraft, HeadSHA: "old-sha"})
	srv := newRefreshPRServer(tracker, &refreshPRProvider{prStatusErr: errors.New("github unavailable")}, refreshPRSession())

	_, err := srv.RefreshSessionPR(context.Background(), connect.NewRequest(&pb.RefreshSessionPRRequest{Id: strPtr("s1")}))
	if err == nil {
		t.Fatal("RefreshSessionPR returned nil error")
	}
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("code = %v, want Unavailable (err: %v)", got, err)
	}
	if got := tracker.Get("s1"); got == nil || got.Status != vcs.DisplayStatusDraft || got.HeadSHA != "old-sha" {
		t.Fatalf("tracker entry = %+v, want old draft snapshot preserved", got)
	}
}

func TestRefreshSessionPRResolvesByPRNumber(t *testing.T) {
	tracker := status.NewDisplayTracker()
	provider := &refreshPRProvider{prStatus: &vcs.PRStatus{State: vcs.PRStateOpen, Draft: true, HeadSHA: "draft-sha"}}
	srv := newRefreshPRServer(tracker, provider, refreshPRSession())

	resp, err := srv.RefreshSessionPR(context.Background(), connect.NewRequest(&pb.RefreshSessionPRRequest{PrNumber: int32Ptr(42)}))
	if err != nil {
		t.Fatalf("RefreshSessionPR: %v", err)
	}
	if resp.Msg.GetSession().GetId() != "s1" {
		t.Fatalf("session id = %q, want s1", resp.Msg.GetSession().GetId())
	}
	if got := resp.Msg.GetSession().GetDisplayStatus(); got != pb.DisplayStatus_DISPLAY_STATUS_DRAFT {
		t.Fatalf("display_status = %v, want DRAFT", got)
	}
	if provider.lastPR != 42 {
		t.Fatalf("provider PR = %d, want 42", provider.lastPR)
	}
}

func TestRefreshSessionPRByPRNumberIgnoresArchivedSessions(t *testing.T) {
	tracker := status.NewDisplayTracker()
	provider := &refreshPRProvider{prStatus: &vcs.PRStatus{State: vcs.PRStateOpen, Draft: true, HeadSHA: "draft-sha"}}
	archivedAt := time.Now()
	archived := refreshPRSession()
	archived.ID = "archived"
	archived.ArchivedAt = &archivedAt
	active := refreshPRSession()
	active.ID = "active"
	srv := newRefreshPRServer(tracker, provider, archived, active)

	resp, err := srv.RefreshSessionPR(context.Background(), connect.NewRequest(&pb.RefreshSessionPRRequest{PrNumber: int32Ptr(42)}))
	if err != nil {
		t.Fatalf("RefreshSessionPR: %v", err)
	}
	if got := resp.Msg.GetSession().GetId(); got != "active" {
		t.Fatalf("session id = %q, want active", got)
	}
}

func TestRefreshSessionPRRejectsMismatchedSessionAndPR(t *testing.T) {
	srv := newRefreshPRServer(status.NewDisplayTracker(), &refreshPRProvider{}, refreshPRSession())

	_, err := srv.RefreshSessionPR(context.Background(), connect.NewRequest(&pb.RefreshSessionPRRequest{
		Id:       strPtr("s1"),
		PrNumber: int32Ptr(43),
	}))
	if err == nil {
		t.Fatal("RefreshSessionPR returned nil error")
	}
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err: %v)", got, err)
	}
}

func TestRefreshSessionPRUnknownIDIsNotFound(t *testing.T) {
	srv := newRefreshPRServer(status.NewDisplayTracker(), &refreshPRProvider{}, refreshPRSession())

	_, err := srv.RefreshSessionPR(context.Background(), connect.NewRequest(&pb.RefreshSessionPRRequest{Id: strPtr("missing")}))
	if err == nil {
		t.Fatal("RefreshSessionPR returned nil error")
	}
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("code = %v, want NotFound (err: %v)", got, err)
	}
}

func newRefreshPRServer(tracker *status.DisplayTracker, provider vcs.Provider, sessions ...*models.Session) *Server {
	return &Server{
		sessions:       &refreshPRSessionStore{sessions: sessions},
		repos:          &refreshPRRepoStore{repo: &models.Repo{ID: "r1", DisplayName: "repo", OriginURL: "https://github.com/acme/repo"}},
		provider:       provider,
		displayTracker: tracker,
		logger:         zerolog.Nop(),
	}
}

func refreshPRSession() *models.Session {
	return &models.Session{ID: "s1", RepoID: "r1", Title: "s1", PRNumber: intPtr(42)}
}

type refreshPRSessionStore struct {
	db.SessionStore
	sessions []*models.Session
}

func (s *refreshPRSessionStore) Get(_ context.Context, id string) (*models.Session, error) {
	for _, sess := range s.sessions {
		if sess.ID == id {
			return sess, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (s *refreshPRSessionStore) ListWithRepo(context.Context, string) ([]*db.SessionWithRepo, error) {
	rows := make([]*db.SessionWithRepo, 0, len(s.sessions))
	for _, sess := range s.sessions {
		rows = append(rows, &db.SessionWithRepo{Session: sess, RepoDisplayName: "repo", RepoOriginURL: "https://github.com/acme/repo"})
	}
	return rows, nil
}

func (s *refreshPRSessionStore) ListActive(context.Context, string) ([]*models.Session, error) {
	rows := make([]*models.Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		if sess.ArchivedAt == nil {
			rows = append(rows, sess)
		}
	}
	return rows, nil
}

type refreshPRRepoStore struct {
	db.RepoStore
	repo *models.Repo
}

func (s *refreshPRRepoStore) Get(_ context.Context, id string) (*models.Repo, error) {
	if s.repo != nil && s.repo.ID == id {
		return s.repo, nil
	}
	return nil, sql.ErrNoRows
}

type refreshPRProvider struct {
	vcs.Provider
	prStatus    *vcs.PRStatus
	checks      []vcs.CheckResult
	reviews     []vcs.ReviewComment
	prStatusErr error
	checksErr   error
	reviewsErr  error

	prStatusCalls int
	checkCalls    int
	reviewCalls   int
	lastPR        int
}

func (p *refreshPRProvider) GetPRStatus(_ context.Context, _ string, pr int) (*vcs.PRStatus, error) {
	p.prStatusCalls++
	p.lastPR = pr
	return p.prStatus, p.prStatusErr
}

func (p *refreshPRProvider) GetCheckResults(context.Context, string, int) ([]vcs.CheckResult, error) {
	p.checkCalls++
	return p.checks, p.checksErr
}

func (p *refreshPRProvider) GetReviewComments(context.Context, string, int) ([]vcs.ReviewComment, error) {
	p.reviewCalls++
	return p.reviews, p.reviewsErr
}

func int32Ptr(i int32) *int32 { return &i }
