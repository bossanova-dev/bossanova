package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
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

// TestRefreshLinkedPRDisplayReplacesTheClosedEnd pins the display half of
// reviving a session by linking its replacement PR. The previous PR's closed
// end in the tracker is a settled end the display poller skips, and it makes
// the session read as done, so linking must replace it with the linked PR's
// open status, or drop it when that read fails.
func TestRefreshLinkedPRDisplayReplacesTheClosedEnd(t *testing.T) {
	linked := &models.Session{ID: "s1", RepoID: "r1", Title: "s1", PRNumber: intPtr(6606)}

	t.Run("fresh read", func(t *testing.T) {
		tracker := status.NewDisplayTracker()
		tracker.Set("s1", vcs.DisplayInfo{Status: vcs.DisplayStatusClosed, HeadSHA: "old-sha"})
		provider := &refreshPRProvider{prStatus: &vcs.PRStatus{State: vcs.PRStateOpen, Draft: true, HeadSHA: "new-sha"}}
		newRefreshPRServer(tracker, provider, linked).refreshLinkedPRDisplay(context.Background(), linked)

		if provider.lastPR != 6606 {
			t.Fatalf("read PR #%d, want the linked #6606", provider.lastPR)
		}
		if got := tracker.Get("s1"); got == nil || got.Status != vcs.DisplayStatusDraft || got.HeadSHA != "new-sha" {
			t.Fatalf("tracker entry = %+v, want the linked PR's draft at new-sha", got)
		}
	})

	t.Run("failed read", func(t *testing.T) {
		tracker := status.NewDisplayTracker()
		tracker.Set("s1", vcs.DisplayInfo{Status: vcs.DisplayStatusClosed, HeadSHA: "old-sha"})
		provider := &refreshPRProvider{prStatusErr: errors.New("github unavailable")}
		newRefreshPRServer(tracker, provider, linked).refreshLinkedPRDisplay(context.Background(), linked)

		if got := tracker.Get("s1"); got != nil {
			t.Fatalf("tracker entry = %+v, want the stale closed end dropped", got)
		}
	})
}

// TestLinkSessionPRMovesToThePRBranchAndReportsTheWorktree pins the operator's
// case end to end through the RPC: a session on its original branch is linked
// to a PR whose head is another branch. The session must move to the PR's
// branch, and the response must name the worktree's branch when the checkout
// is still elsewhere, so the CLI can say what to check out.
func TestLinkSessionPRMovesToThePRBranchAndReportsTheWorktree(t *testing.T) {
	const oldBranch, prBranch = "dave-madverts/mad-1594", "fix/source-led-planner-structured-output"
	for _, worktreeBranch := range []string{oldBranch, prBranch} {
		t.Run("worktree on "+worktreeBranch, func(t *testing.T) {
			sessions := &linkPRSessionStore{refreshPRSessionStore: refreshPRSessionStore{sessions: []*models.Session{{
				ID: "s1", RepoID: "r1", Title: "s1", BranchName: oldBranch, WorktreePath: "/wt/s1", PRNumber: intPtr(6395),
			}}}}
			provider := &refreshPRProvider{prStatus: &vcs.PRStatus{State: vcs.PRStateOpen, HeadBranch: prBranch}}
			srv := newRefreshPRServer(nil, provider)
			srv.sessions = sessions
			srv.lifecycle = session.NewLifecycle(sessions, srv.repos.(db.RepoStore), nil, nil, nil, nil, nil, provider, zerolog.Nop())
			srv.lifecycle.SetBranchResolver(staticBranchResolver(worktreeBranch))

			resp, err := srv.LinkSessionPR(context.Background(), connect.NewRequest(&pb.LinkSessionPRRequest{Id: "s1", Pr: "6606"}))
			if err != nil {
				t.Fatalf("LinkSessionPR: %v", err)
			}
			if got := resp.Msg.GetSession().GetBranchName(); got != prBranch {
				t.Fatalf("branch_name = %q, want %q", got, prBranch)
			}
			wantWorktree := ""
			if worktreeBranch != prBranch {
				wantWorktree = worktreeBranch
			}
			if got := resp.Msg.GetWorktreeBranch(); got != wantWorktree {
				t.Fatalf("worktree_branch = %q, want %q", got, wantWorktree)
			}
		})
	}
}

type staticBranchResolver string

func (b staticBranchResolver) CurrentBranch(context.Context, string) (string, error) {
	return string(b), nil
}

// TestLinkSessionPRRefusesAPRAnotherLiveSessionOwns pins the RPC's answer when
// the PR is already another live session's: AlreadyExists, carrying the plain
// message the CLI prints, and the session is left unlinked.
func TestLinkSessionPRRefusesAPRAnotherLiveSessionOwns(t *testing.T) {
	sessions := &linkPRSessionStore{refreshPRSessionStore: refreshPRSessionStore{sessions: []*models.Session{
		{ID: "s1", RepoID: "r1", Title: "s1", BranchName: "dave/old", State: machine.AwaitingChecks},
		{ID: "s2", RepoID: "r1", Title: "Consolidated work", BranchName: "fix/pr-head", State: machine.AwaitingChecks, PRNumber: intPtr(6606)},
	}}}
	provider := &refreshPRProvider{prStatus: &vcs.PRStatus{State: vcs.PRStateOpen, HeadBranch: "fix/pr-head"}}
	srv := newRefreshPRServer(nil, provider)
	srv.sessions = sessions
	srv.lifecycle = session.NewLifecycle(sessions, srv.repos.(db.RepoStore), nil, nil, nil, nil, nil, provider, zerolog.Nop())

	_, err := srv.LinkSessionPR(context.Background(), connect.NewRequest(&pb.LinkSessionPRRequest{Id: "s1", Pr: "6606"}))
	if got := connect.CodeOf(err); got != connect.CodeAlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists (err: %v)", got, err)
	}
	var connectErr *connect.Error
	want := `PR #6606 is already linked to session s2 ("Consolidated work"); archive that session first`
	if !errors.As(err, &connectErr) || connectErr.Message() != want {
		t.Fatalf("message = %v, want %q", err, want)
	}
	if s1 := sessions.sessions[0]; s1.PRNumber != nil || s1.BranchName != "dave/old" {
		t.Fatalf("refused link changed s1: PR %v, branch %q", s1.PRNumber, s1.BranchName)
	}
}

// TestLinkSessionPRAnswersABusyStartPathLockAsUnavailable pins that a link
// which cannot take the repo's start-path lock tells the caller to retry, as
// the create paths do, rather than blaming its arguments.
func TestLinkSessionPRAnswersABusyStartPathLockAsUnavailable(t *testing.T) {
	sessions := &linkPRSessionStore{refreshPRSessionStore: refreshPRSessionStore{sessions: []*models.Session{
		{ID: "s1", RepoID: "r1", Title: "s1", BranchName: "dave/old", State: machine.AwaitingChecks},
	}}}
	provider := &refreshPRProvider{prStatus: &vcs.PRStatus{State: vcs.PRStateOpen, HeadBranch: "fix/pr-head"}}
	srv := newRefreshPRServer(nil, provider)
	srv.sessions = sessions
	srv.lifecycle = session.NewLifecycle(sessions, srv.repos.(db.RepoStore), nil, nil, nil, nil, nil, provider, zerolog.Nop())

	release, _, err := session.AcquireStartPath(context.Background(), "r1")
	if err != nil {
		t.Fatalf("hold start-path lock: %v", err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err = srv.LinkSessionPR(ctx, connect.NewRequest(&pb.LinkSessionPRRequest{Id: "s1", Pr: "6606"}))
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("code = %v, want Unavailable (err: %v)", got, err)
	}
	if s1 := sessions.sessions[0]; s1.PRNumber != nil || s1.BranchName != "dave/old" {
		t.Fatalf("link without the lock changed s1: PR %v, branch %q", s1.PRNumber, s1.BranchName)
	}
}

type linkPRSessionStore struct{ refreshPRSessionStore }

func (s *linkPRSessionStore) ListByRepoAndPR(_ context.Context, repoID string, prNumber int) ([]*db.SessionWithRepo, error) {
	var rows []*db.SessionWithRepo
	for _, sess := range s.sessions {
		if sess.RepoID == repoID && sess.PRNumber != nil && *sess.PRNumber == prNumber && sess.ArchivedAt == nil {
			rows = append(rows, &db.SessionWithRepo{Session: sess})
		}
	}
	return rows, nil
}

func (s *linkPRSessionStore) ListActiveWithRepo(_ context.Context, repoID string) ([]*db.SessionWithRepo, error) {
	var rows []*db.SessionWithRepo
	for _, sess := range s.sessions {
		if sess.RepoID == repoID && sess.ArchivedAt == nil {
			rows = append(rows, &db.SessionWithRepo{Session: sess})
		}
	}
	return rows, nil
}

func (s *linkPRSessionStore) Update(_ context.Context, id string, params db.UpdateSessionParams) (*models.Session, error) {
	for _, sess := range s.sessions {
		if sess.ID != id {
			continue
		}
		if params.PRNumber != nil {
			sess.PRNumber = *params.PRNumber
		}
		if params.PRURL != nil {
			sess.PRURL = *params.PRURL
		}
		if params.BranchName != nil {
			sess.BranchName = *params.BranchName
		}
		return sess, nil
	}
	return nil, domain.ErrNotFound
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
	return nil, domain.ErrNotFound
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
	return nil, domain.ErrNotFound
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

// TestRefreshSessionPRHydratesVerifyParkAttention pins the RefreshSessionPR
// wiring of the BOS-1382 overlay end to end from a live provider read: green
// ordinary CI plus a pending boss/verify park yields NEEDS_HUMAN and the
// AWAITING_HUMAN_INPUT attention carrying the reason.
func TestRefreshSessionPRHydratesVerifyParkAttention(t *testing.T) {
	tracker := status.NewDisplayTracker()
	provider := &refreshPRProvider{
		prStatus: &vcs.PRStatus{
			State:            vcs.PRStateOpen,
			Mergeable:        boolPtr(true),
			MergeStateStatus: vcs.MergeStateStatusClean,
			HeadSHA:          "parked-sha",
		},
		checks: []vcs.CheckResult{
			{Name: "build", Status: vcs.CheckStatusCompleted, Conclusion: checkConclusionPtr(vcs.CheckConclusionSuccess)},
			{Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusQueued, Description: "needs human: always-human-path"},
		},
	}
	srv := newRefreshPRServer(tracker, provider, refreshPRSession())

	resp, err := srv.RefreshSessionPR(context.Background(), connect.NewRequest(&pb.RefreshSessionPRRequest{Id: strPtr("s1")}))
	if err != nil {
		t.Fatalf("RefreshSessionPR: %v", err)
	}
	got := resp.Msg.GetSession()
	if got.GetDisplayStatus() != pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN {
		t.Fatalf("display_status = %v, want NEEDS_HUMAN", got.GetDisplayStatus())
	}
	if att := got.GetAttentionStatus(); att.GetReason() != pb.AttentionReason_ATTENTION_REASON_AWAITING_HUMAN_INPUT ||
		att.GetSummary() != "needs human: always-human-path" {
		t.Fatalf("attention = %+v, want AWAITING_HUMAN_INPUT / needs human: always-human-path", att)
	}
	if detail := got.GetMergeBlock().GetDetail(); detail != "the verify stage parked this head for a human: always-human-path" {
		t.Errorf("merge_block.detail = %q", detail)
	}
}

// The capability must replace the legacy check read, never add a second read.
type receiptRefreshPRProvider struct {
	*refreshPRProvider
	hasReceipt bool
}

func (p *receiptRefreshPRProvider) GetCheckSet(ctx context.Context, repo string, pr int) (vcs.CheckSet, error) {
	checks, err := p.GetCheckResults(ctx, repo, pr)
	return vcs.CheckSet{Checks: checks, HasBuildReceipt: p.hasReceipt}, err
}

func TestRefreshSessionPRBuildReceiptIsRefreshedAndTransportOnly(t *testing.T) {
	provider := &receiptRefreshPRProvider{refreshPRProvider: &refreshPRProvider{
		prStatus: &vcs.PRStatus{State: vcs.PRStateOpen, Mergeable: boolPtr(true), HeadSHA: "head-a"},
		checks:   []vcs.CheckResult{{Status: vcs.CheckStatusCompleted, Conclusion: checkConclusionPtr(vcs.CheckConclusionSuccess)}},
	}, hasReceipt: true}
	tracker := status.NewDisplayTracker()
	srv := newRefreshPRServer(tracker, provider, refreshPRSession())
	for _, tc := range []struct {
		name           string
		receipt, draft bool
		state          vcs.PRState
		want           bool
	}{
		{"build handoff", true, false, vcs.PRStateOpen, true},
		{"human push", false, false, vcs.PRStateOpen, false},
		{"repair repost", true, false, vcs.PRStateOpen, true},
		{"draft", true, true, vcs.PRStateOpen, true},
		{"merged", true, false, vcs.PRStateMerged, false},
		{"closed", true, false, vcs.PRStateClosed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider.hasReceipt = tc.receipt
			provider.prStatus.Draft, provider.prStatus.State = tc.draft, tc.state
			before := provider.checkCalls
			resp, err := srv.RefreshSessionPR(context.Background(), connect.NewRequest(&pb.RefreshSessionPRRequest{Id: strPtr("s1")}))
			if err != nil {
				t.Fatal(err)
			}
			if got := resp.Msg.Session.GetHasBuildReceipt(); got != tc.want {
				t.Fatalf("wire receipt = %v, want %v", got, tc.want)
			}
			if got := tracker.Get("s1").HasBuildReceipt; got != tc.want {
				t.Fatalf("tracker receipt = %v, want %v", got, tc.want)
			}
			wantReads := 1
			if tc.state != vcs.PRStateOpen {
				wantReads = 0
			}
			if got := provider.checkCalls - before; got != wantReads {
				t.Fatalf("check reads = %d, want %d", got, wantReads)
			}
		})
	}
}

func TestRefreshSessionPRHeldDraft_Sessions_AC160(t *testing.T) {
	for _, tc := range []struct {
		name               string
		state              machine.State
		conflict, sendBack bool
		want               vcs.DisplayStatus
	}{
		{"held conflict", machine.FixingChecks, true, false, vcs.DisplayStatusConflict},
		{"held ordinary failure", machine.GreenDraft, false, false, vcs.DisplayStatusFailing},
		{"held send-back", machine.ReadyForReview, false, true, vcs.DisplayStatusFailing},
		{"person conflict", machine.ReadyForReview, true, false, vcs.DisplayStatusDraft},
		{"person failure", machine.ReadyForReview, false, false, vcs.DisplayStatusDraft},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracker := status.NewDisplayTracker()
			provider := &refreshPRProvider{prStatus: &vcs.PRStatus{State: vcs.PRStateOpen, Draft: true, Mergeable: boolPtr(!tc.conflict), HeadSHA: "head"}}
			if !tc.conflict {
				name, description := "ci", ""
				if tc.sendBack {
					name, description = vcs.VerifyStatusContext, "defect: x"
				}
				provider.checks = []vcs.CheckResult{{Name: name, Status: vcs.CheckStatusCompleted, Conclusion: checkConclusionPtr(vcs.CheckConclusionFailure), Description: description}}
			}
			sess := refreshPRSession()
			sess.State = tc.state
			tracker.Set(sess.ID, vcs.DisplayInfo{Status: tc.want})
			srv := newRefreshPRServer(tracker, provider, sess)
			resp, err := srv.RefreshSessionPR(context.Background(), connect.NewRequest(&pb.RefreshSessionPRRequest{Id: strPtr(sess.ID)}))
			if err != nil {
				t.Fatal(err)
			}
			if got := resp.Msg.GetSession().GetDisplayStatus(); int(got) != int(tc.want) {
				t.Fatalf("status=%v, want %v", got, tc.want)
			}
			if got := tracker.Get(sess.ID); got == nil || got.Status != tc.want {
				t.Fatalf("entry=%+v", got)
			}
			if provider.reviewCalls != 0 || provider.checkCalls != 1 {
				t.Fatalf("checks=%d reviews=%d", provider.checkCalls, provider.reviewCalls)
			}
		})
	}
}
