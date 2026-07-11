package server

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
	"github.com/rs/zerolog"
)

func TestListSessions_RecomputesDisplayCompositeFromLiveTracker(t *testing.T) {
	sess := &models.Session{
		ID:             "sess-1",
		RepoID:         "repo-1",
		Title:          "Fix status mismatch",
		State:          machine.ImplementingPlan,
		DisplayLabel:   "⨯ rejected",
		DisplayIntent:  int32(pb.DisplayIntent_DISPLAY_INTENT_DANGER),
		DisplaySpinner: false,
		CreatedAt:      time.Now(),
	}
	displayTracker := status.NewDisplayTracker()
	displayTracker.Set(sess.ID, vcs.DisplayInfo{
		Status:      vcs.DisplayStatusChecking,
		HasFailures: true,
	})
	s := newListSessionsDisplayStatusTestServer([]*models.Session{sess}, nil, displayTracker, nil)

	resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	got := onlySession(t, resp.Msg.Sessions)

	if got.DisplayStatus != pb.DisplayStatus_DISPLAY_STATUS_CHECKING {
		t.Fatalf("DisplayStatus = %v, want CHECKING", got.DisplayStatus)
	}
	if got.DisplayLabel != "checking" {
		t.Fatalf("DisplayLabel = %q, want %q", got.DisplayLabel, "checking")
	}
	if !got.DisplaySpinner {
		t.Fatalf("DisplaySpinner = false, want true")
	}
	if got.DisplayIntent != pb.DisplayIntent_DISPLAY_INTENT_DANGER {
		t.Fatalf("DisplayIntent = %v, want DANGER", got.DisplayIntent)
	}
	if sess.DisplayLabel != "⨯ rejected" {
		t.Fatalf("persisted session label mutated to %q", sess.DisplayLabel)
	}
}

func TestListSessions_RecomputedDisplayCompositeKeepsChatPrecedence(t *testing.T) {
	agentSessionID := "agent-1"
	sess := &models.Session{
		ID:             "sess-1",
		RepoID:         "repo-1",
		Title:          "Fix status mismatch",
		State:          machine.ImplementingPlan,
		AgentSessionID: &agentSessionID,
		DisplayLabel:   "⨯ rejected",
		DisplayIntent:  int32(pb.DisplayIntent_DISPLAY_INTENT_DANGER),
		DisplaySpinner: false,
		CreatedAt:      time.Now(),
	}
	displayTracker := status.NewDisplayTracker()
	displayTracker.Set(sess.ID, vcs.DisplayInfo{
		Status:      vcs.DisplayStatusChecking,
		HasFailures: true,
	})
	chatStatus := status.NewTracker()
	chatStatus.Update(agentSessionID, pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	chats := map[string][]*models.AgentChat{
		sess.ID: {{
			ID:             "chat-1",
			SessionID:      sess.ID,
			AgentSessionID: agentSessionID,
		}},
	}
	s := newListSessionsDisplayStatusTestServer([]*models.Session{sess}, chats, displayTracker, chatStatus)

	resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	got := onlySession(t, resp.Msg.Sessions)

	if got.DisplayStatus != pb.DisplayStatus_DISPLAY_STATUS_CHECKING {
		t.Fatalf("DisplayStatus = %v, want CHECKING", got.DisplayStatus)
	}
	if got.DisplayLabel != "working" {
		t.Fatalf("DisplayLabel = %q, want %q", got.DisplayLabel, "working")
	}
	if !got.DisplaySpinner {
		t.Fatalf("DisplaySpinner = false, want true")
	}
	if got.DisplayIntent != pb.DisplayIntent_DISPLAY_INTENT_DANGER {
		t.Fatalf("DisplayIntent = %v, want DANGER because checking has failures", got.DisplayIntent)
	}
}

func TestListSessions_CheckingCompositeOverridesLiveReviewStatus(t *testing.T) {
	sess := &models.Session{
		ID:             "sess-1",
		RepoID:         "repo-1",
		Title:          "Fix status mismatch",
		State:          machine.ImplementingPlan,
		DisplayLabel:   "checking",
		DisplayIntent:  int32(pb.DisplayIntent_DISPLAY_INTENT_WARNING),
		DisplaySpinner: true,
		CreatedAt:      time.Now(),
	}
	displayTracker := status.NewDisplayTracker()
	displayTracker.Set(sess.ID, vcs.DisplayInfo{
		Status: vcs.DisplayStatusReview,
	})
	s := newListSessionsDisplayStatusTestServer([]*models.Session{sess}, nil, displayTracker, status.NewTracker())

	resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	got := onlySession(t, resp.Msg.Sessions)

	if got.DisplayStatus != pb.DisplayStatus_DISPLAY_STATUS_REVIEW {
		t.Fatalf("DisplayStatus = %v, want REVIEW", got.DisplayStatus)
	}
	if got.DisplayLabel != "checking" {
		t.Fatalf("DisplayLabel = %q, want %q", got.DisplayLabel, "checking")
	}
	if !got.DisplaySpinner {
		t.Fatalf("DisplaySpinner = false, want true")
	}
	if got.DisplayIntent != pb.DisplayIntent_DISPLAY_INTENT_WARNING {
		t.Fatalf("DisplayIntent = %v, want WARNING", got.DisplayIntent)
	}
}

func TestListSessions_LimitedChatOverridesCheckingCompositeReviewGuard(t *testing.T) {
	agentSessionID := "agent-1"
	sess := &models.Session{
		ID:             "sess-1",
		RepoID:         "repo-1",
		Title:          "Fix status mismatch",
		State:          machine.ImplementingPlan,
		AgentSessionID: &agentSessionID,
		DisplayLabel:   "checking",
		DisplayIntent:  int32(pb.DisplayIntent_DISPLAY_INTENT_WARNING),
		DisplaySpinner: true,
		CreatedAt:      time.Now(),
	}
	displayTracker := status.NewDisplayTracker()
	displayTracker.Set(sess.ID, vcs.DisplayInfo{
		Status: vcs.DisplayStatusReview,
	})
	chatStatus := status.NewTracker()
	chatStatus.UpdateLimited(agentSessionID, time.Time{}, time.Now())
	chats := map[string][]*models.AgentChat{
		sess.ID: {{
			ID:             "chat-1",
			SessionID:      sess.ID,
			AgentSessionID: agentSessionID,
		}},
	}
	s := newListSessionsDisplayStatusTestServer([]*models.Session{sess}, chats, displayTracker, chatStatus)

	resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	got := onlySession(t, resp.Msg.Sessions)

	// A LIMITED chat ranks above the PR "checking"/review composite, so the
	// keep-checking guard must not suppress the recompute — the label flips to
	// "usage-limited" (matching the DisplayStatusComputer.Recompute path).
	if got.DisplayLabel != "usage-limited" {
		t.Fatalf("DisplayLabel = %q, want %q", got.DisplayLabel, "usage-limited")
	}
	if got.DisplaySpinner {
		t.Fatalf("DisplaySpinner = true, want false")
	}
	if got.DisplayIntent != pb.DisplayIntent_DISPLAY_INTENT_WARNING {
		t.Fatalf("DisplayIntent = %v, want WARNING", got.DisplayIntent)
	}
}

func TestListSessions_RepairingOverridesCheckingCompositeReviewGuard(t *testing.T) {
	sess := &models.Session{
		ID:             "sess-1",
		RepoID:         "repo-1",
		Title:          "Fix status mismatch",
		State:          machine.ImplementingPlan,
		DisplayLabel:   "checking",
		DisplayIntent:  int32(pb.DisplayIntent_DISPLAY_INTENT_WARNING),
		DisplaySpinner: true,
		CreatedAt:      time.Now(),
	}
	displayTracker := status.NewDisplayTracker()
	displayTracker.Set(sess.ID, vcs.DisplayInfo{
		Status: vcs.DisplayStatusReview,
	})
	displayTracker.SetRepairing(sess.ID, true)
	s := newListSessionsDisplayStatusTestServer([]*models.Session{sess}, nil, displayTracker, status.NewTracker())

	resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	got := onlySession(t, resp.Msg.Sessions)

	if got.DisplayLabel != "repairing" {
		t.Fatalf("DisplayLabel = %q, want %q", got.DisplayLabel, "repairing")
	}
	if !got.DisplaySpinner {
		t.Fatalf("DisplaySpinner = false, want true")
	}
	if got.DisplayIntent != pb.DisplayIntent_DISPLAY_INTENT_WARNING {
		t.Fatalf("DisplayIntent = %v, want WARNING", got.DisplayIntent)
	}
}

func TestListSessions_KeepsPersistedDisplayCompositeWhenChatStatusLookupFails(t *testing.T) {
	sess := &models.Session{
		ID:             "sess-1",
		RepoID:         "repo-1",
		Title:          "Fix status mismatch",
		State:          machine.ImplementingPlan,
		DisplayLabel:   "working",
		DisplayIntent:  int32(pb.DisplayIntent_DISPLAY_INTENT_SUCCESS),
		DisplaySpinner: true,
		CreatedAt:      time.Now(),
	}
	displayTracker := status.NewDisplayTracker()
	displayTracker.Set(sess.ID, vcs.DisplayInfo{
		Status: vcs.DisplayStatusChecking,
	})
	s := newListSessionsDisplayStatusTestServer([]*models.Session{sess}, nil, displayTracker, status.NewTracker())
	s.agentChats = &listSessionsAgentChatStoreFake{listErr: errors.New("database unavailable")}

	resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	got := onlySession(t, resp.Msg.Sessions)

	if got.DisplayStatus != pb.DisplayStatus_DISPLAY_STATUS_CHECKING {
		t.Fatalf("DisplayStatus = %v, want CHECKING", got.DisplayStatus)
	}
	if got.DisplayLabel != "working" {
		t.Fatalf("DisplayLabel = %q, want persisted %q", got.DisplayLabel, "working")
	}
	if got.DisplayIntent != pb.DisplayIntent_DISPLAY_INTENT_SUCCESS {
		t.Fatalf("DisplayIntent = %v, want persisted SUCCESS", got.DisplayIntent)
	}
	if !got.DisplaySpinner {
		t.Fatalf("DisplaySpinner = false, want persisted true")
	}
}

func TestListSessions_KeepsPersistedDisplayCompositeWhenLiveDisplayEntryMissing(t *testing.T) {
	sess := &models.Session{
		ID:             "sess-1",
		RepoID:         "repo-1",
		Title:          "Fix status mismatch",
		State:          machine.ImplementingPlan,
		DisplayLabel:   "✓ passing",
		DisplayIntent:  int32(pb.DisplayIntent_DISPLAY_INTENT_SUCCESS),
		DisplaySpinner: false,
		CreatedAt:      time.Now(),
	}
	displayTracker := status.NewDisplayTracker()
	s := newListSessionsDisplayStatusTestServer([]*models.Session{sess}, nil, displayTracker, status.NewTracker())

	resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	got := onlySession(t, resp.Msg.Sessions)

	if got.DisplayLabel != "✓ passing" {
		t.Fatalf("DisplayLabel = %q, want persisted %q", got.DisplayLabel, "✓ passing")
	}
	if got.DisplayIntent != pb.DisplayIntent_DISPLAY_INTENT_SUCCESS {
		t.Fatalf("DisplayIntent = %v, want persisted SUCCESS", got.DisplayIntent)
	}
	if got.DisplaySpinner {
		t.Fatalf("DisplaySpinner = true, want persisted false")
	}
}

func TestListSessions_BatchesChatStatusLookup(t *testing.T) {
	sessions := []*models.Session{
		{
			ID:        "sess-1",
			RepoID:    "repo-1",
			Title:     "Fix status mismatch",
			State:     machine.ImplementingPlan,
			CreatedAt: time.Now(),
		},
		{
			ID:        "sess-2",
			RepoID:    "repo-1",
			Title:     "Fix another mismatch",
			State:     machine.ImplementingPlan,
			CreatedAt: time.Now(),
		},
	}
	displayTracker := status.NewDisplayTracker()
	displayTracker.Set("sess-1", vcs.DisplayInfo{Status: vcs.DisplayStatusChecking})
	displayTracker.Set("sess-2", vcs.DisplayInfo{Status: vcs.DisplayStatusChecking})
	chatStore := &listSessionsAgentChatStoreFake{chats: map[string][]*models.AgentChat{}}
	s := newListSessionsDisplayStatusTestServer(sessions, nil, displayTracker, status.NewTracker())
	s.agentChats = chatStore

	if _, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{})); err != nil {
		t.Fatalf("ListSessions: %v", err)
	}

	if chatStore.listBatchCalls != 1 {
		t.Fatalf("ListBySessions calls = %d, want 1", chatStore.listBatchCalls)
	}
	if chatStore.listCalls != 0 {
		t.Fatalf("ListBySession calls = %d, want 0", chatStore.listCalls)
	}
}

func TestListSessions_AttachesOpenPRForNoPRBranch(t *testing.T) {
	sess := &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Fresh claim PR failed",
		BranchName: "freshclaim-pr-failed",
		State:      machine.AwaitingChecks,
		CreatedAt:  time.Now(),
	}
	provider := &listSessionsVCSProviderFake{
		openPRs: []vcs.PRSummary{
			{
				Number:     497,
				HeadBranch: "freshclaim-pr-failed",
				State:      vcs.PRStateOpen,
			},
		},
	}
	s := newListSessionsDisplayStatusTestServer(
		[]*models.Session{sess},
		nil,
		status.NewDisplayTracker(),
		status.NewTracker(),
	)
	s.prResolver = session.NewPRAssociationResolver(s.sessions, s.repos, provider, zerolog.Nop())

	resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}

	got := onlySession(t, resp.Msg.Sessions)
	if got.PrNumber == nil || got.GetPrNumber() != 497 {
		t.Fatalf("PRNumber = %v, want 497", got.PrNumber)
	}
	const wantURL = "https://github.com/recurser/repo/pull/497"
	if got.PrUrl == nil || got.GetPrUrl() != wantURL {
		t.Fatalf("PRURL = %v, want %q", got.PrUrl, wantURL)
	}
	if provider.listOpenCalls != 1 {
		t.Fatalf("ListOpenPRs calls = %d, want 1", provider.listOpenCalls)
	}
	if provider.listClosedCalls != 0 {
		t.Fatalf("ListClosedPRs calls = %d, want 0", provider.listClosedCalls)
	}
}

func TestListSessions_BoundsPRAssociationReconciliation(t *testing.T) {
	originalTimeout := listSessionsPRAssociationTimeout
	listSessionsPRAssociationTimeout = 10 * time.Millisecond
	t.Cleanup(func() {
		listSessionsPRAssociationTimeout = originalTimeout
	})

	sess := &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Fresh claim PR failed",
		BranchName: "freshclaim-pr-failed",
		State:      machine.AwaitingChecks,
		CreatedAt:  time.Now(),
	}
	provider := &listSessionsVCSProviderFake{
		listOpenDelay:          2 * time.Second,
		listOpenIgnoresContext: true,
		openPRs: []vcs.PRSummary{
			{
				Number:     497,
				HeadBranch: "freshclaim-pr-failed",
				State:      vcs.PRStateOpen,
			},
		},
	}
	s := newListSessionsDisplayStatusTestServer(
		[]*models.Session{sess},
		nil,
		status.NewDisplayTracker(),
		status.NewTracker(),
	)
	s.prResolver = session.NewPRAssociationResolver(s.sessions, s.repos, provider, zerolog.Nop())

	start := time.Now()
	resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("ListSessions took %s, want bounded below 1s", elapsed)
	}

	got := onlySession(t, resp.Msg.Sessions)
	if got.PrNumber != nil {
		t.Fatalf("PRNumber = %v, want nil after timed out reconciliation", got.PrNumber)
	}
	if got := provider.listOpenCallCount(); got != 1 {
		t.Fatalf("ListOpenPRs calls = %d, want 1", got)
	}
	if got := provider.listClosedCallCount(); got != 0 {
		t.Fatalf("ListClosedPRs calls = %d, want 0", got)
	}
}

func TestListSessions_ReloadsRowsAfterPartialPRDiscoveryError(t *testing.T) {
	first := &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "First session",
		BranchName: "first-branch",
		State:      machine.AwaitingChecks,
		CreatedAt:  time.Now(),
	}
	second := &models.Session{
		ID:         "sess-2",
		RepoID:     "repo-2",
		Title:      "Second session",
		BranchName: "second-branch",
		State:      machine.AwaitingChecks,
		CreatedAt:  time.Now(),
	}
	const firstOrigin = "https://github.com/recurser/first.git"
	const secondOrigin = "https://github.com/recurser/second.git"
	provider := &listSessionsVCSProviderFake{
		openPRsByOrigin: map[string][]vcs.PRSummary{
			firstOrigin: {
				{
					Number:     497,
					HeadBranch: "first-branch",
					State:      vcs.PRStateOpen,
				},
			},
		},
		listOpenErrByOrigin: map[string]error{
			secondOrigin: errors.New("provider unavailable"),
		},
	}
	s := newListSessionsDisplayStatusTestServer(
		[]*models.Session{first, second},
		nil,
		status.NewDisplayTracker(),
		status.NewTracker(),
	)
	s.sessions.(*listSessionsSessionStoreFake).cloneRows = true
	s.repos = &listSessionsRepoStoreFake{
		repos: map[string]*models.Repo{
			"repo-1": {
				ID:                "repo-1",
				DisplayName:       "first",
				OriginURL:         firstOrigin,
				DefaultBaseBranch: "main",
			},
			"repo-2": {
				ID:                "repo-2",
				DisplayName:       "second",
				OriginURL:         secondOrigin,
				DefaultBaseBranch: "main",
			},
		},
	}
	s.prResolver = session.NewPRAssociationResolver(s.sessions, s.repos, provider, zerolog.Nop())

	resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(resp.Msg.Sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(resp.Msg.Sessions))
	}
	got := resp.Msg.Sessions[0]
	if got.GetId() != "sess-1" {
		t.Fatalf("first session ID = %q, want sess-1", got.GetId())
	}
	if got.PrNumber == nil || got.GetPrNumber() != 497 {
		t.Fatalf("PRNumber = %v, want 497", got.PrNumber)
	}
	const wantURL = "https://github.com/recurser/first/pull/497"
	if got.PrUrl == nil || got.GetPrUrl() != wantURL {
		t.Fatalf("PRURL = %v, want %q", got.PrUrl, wantURL)
	}
	if provider.listOpenCalls != 2 {
		t.Fatalf("ListOpenPRs calls = %d, want 2", provider.listOpenCalls)
	}
	if provider.listClosedCalls != 0 {
		t.Fatalf("ListClosedPRs calls = %d, want 0", provider.listClosedCalls)
	}
	if len(provider.listOpenOrigins) != 2 || provider.listOpenOrigins[0] != firstOrigin || provider.listOpenOrigins[1] != secondOrigin {
		t.Fatalf("ListOpenPRs origins = %v, want [%q %q]", provider.listOpenOrigins, firstOrigin, secondOrigin)
	}
	// ListActiveWithRepo is called twice: once for the initial load and once
	// to reload rows after reconciliation attached a PR association.
	if s.sessions.(*listSessionsSessionStoreFake).listActiveWithRepoCalls != 2 {
		t.Fatalf("ListActiveWithRepo calls = %d, want 2", s.sessions.(*listSessionsSessionStoreFake).listActiveWithRepoCalls)
	}
}

func TestListSessions_DoesNotReconcileWhenAllSessionsHavePRs(t *testing.T) {
	prNumber := 123
	prURL := "https://github.com/recurser/repo/pull/123"
	sess := &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Already linked",
		BranchName: "already-linked",
		State:      machine.AwaitingChecks,
		CreatedAt:  time.Now(),
		PRNumber:   &prNumber,
		PRURL:      &prURL,
	}
	provider := &listSessionsVCSProviderFake{}
	s := newListSessionsDisplayStatusTestServer(
		[]*models.Session{sess},
		nil,
		status.NewDisplayTracker(),
		status.NewTracker(),
	)
	s.prResolver = session.NewPRAssociationResolver(s.sessions, s.repos, provider, zerolog.Nop())

	resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}

	got := onlySession(t, resp.Msg.Sessions)
	if got.PrNumber == nil || got.GetPrNumber() != 123 {
		t.Fatalf("PRNumber = %v, want 123", got.PrNumber)
	}
	if got.PrUrl == nil || got.GetPrUrl() != prURL {
		t.Fatalf("PRURL = %v, want %q", got.PrUrl, prURL)
	}
	if provider.listOpenCalls != 0 {
		t.Fatalf("ListOpenPRs calls = %d, want 0", provider.listOpenCalls)
	}
}

func onlySession(t *testing.T, sessions []*pb.Session) *pb.Session {
	t.Helper()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	return sessions[0]
}

func newListSessionsDisplayStatusTestServer(
	sessions []*models.Session,
	chats map[string][]*models.AgentChat,
	displayTracker *status.DisplayTracker,
	chatStatus *status.Tracker,
) *Server {
	return &Server{
		repos:          &listSessionsRepoStoreFake{},
		sessions:       &listSessionsSessionStoreFake{sessions: sessions},
		agentChats:     &listSessionsAgentChatStoreFake{chats: chats},
		displayTracker: displayTracker,
		chatStatus:     chatStatus,
	}
}

type listSessionsSessionStoreFake struct {
	db.SessionStore
	sessions                []*models.Session
	cloneRows               bool
	listCalls               int
	listActiveCalls         int
	listWithRepoCalls       int
	listActiveWithRepoCalls int
}

func (f *listSessionsSessionStoreFake) List(_ context.Context, _ string) ([]*models.Session, error) {
	f.listCalls++
	return f.sessionModels(), nil
}

func (f *listSessionsSessionStoreFake) ListActive(_ context.Context, _ string) ([]*models.Session, error) {
	f.listActiveCalls++
	return f.sessionModels(), nil
}

func (f *listSessionsSessionStoreFake) ListWithRepo(_ context.Context, _ string) ([]*db.SessionWithRepo, error) {
	f.listWithRepoCalls++
	return f.sessionRows(), nil
}

func (f *listSessionsSessionStoreFake) ListActiveWithRepo(_ context.Context, _ string) ([]*db.SessionWithRepo, error) {
	f.listActiveWithRepoCalls++
	return f.sessionRows(), nil
}

func (f *listSessionsSessionStoreFake) Update(_ context.Context, id string, params db.UpdateSessionParams) (*models.Session, error) {
	for _, sess := range f.sessions {
		if sess == nil || sess.ID != id {
			continue
		}
		if params.PRNumber != nil {
			sess.PRNumber = *params.PRNumber
		}
		if params.PRURL != nil {
			sess.PRURL = *params.PRURL
		}
		if params.BlockedReason != nil {
			sess.BlockedReason = *params.BlockedReason
		}
		return sess, nil
	}
	return nil, sql.ErrNoRows
}

func (f *listSessionsSessionStoreFake) sessionModels() []*models.Session {
	if !f.cloneRows {
		return f.sessions
	}
	sessions := make([]*models.Session, 0, len(f.sessions))
	for _, sess := range f.sessions {
		if sess == nil {
			continue
		}
		clone := *sess
		sessions = append(sessions, &clone)
	}
	return sessions
}

func (f *listSessionsSessionStoreFake) sessionRows() []*db.SessionWithRepo {
	rows := make([]*db.SessionWithRepo, 0, len(f.sessions))
	for _, sess := range f.sessions {
		if sess == nil {
			continue
		}
		rowSession := sess
		if f.cloneRows {
			clone := *sess
			rowSession = &clone
		}
		rows = append(rows, &db.SessionWithRepo{
			Session:         rowSession,
			RepoDisplayName: "repo",
			RepoOriginURL:   "https://github.com/recurser/repo.git",
		})
	}
	return rows
}

type listSessionsRepoStoreFake struct {
	db.RepoStore
	repos map[string]*models.Repo
}

func (f *listSessionsRepoStoreFake) Get(_ context.Context, id string) (*models.Repo, error) {
	if id == "" {
		return nil, sql.ErrNoRows
	}
	if f.repos != nil {
		if repo, ok := f.repos[id]; ok {
			clone := *repo
			return &clone, nil
		}
		return nil, sql.ErrNoRows
	}
	return &models.Repo{
		ID:                id,
		DisplayName:       "repo",
		OriginURL:         "https://github.com/recurser/repo.git",
		DefaultBaseBranch: "main",
	}, nil
}

type listSessionsVCSProviderFake struct {
	vcs.Provider
	mu sync.Mutex

	openPRs                []vcs.PRSummary
	openPRsByOrigin        map[string][]vcs.PRSummary
	closedPRsByOrigin      map[string][]vcs.PRSummary
	listOpenErrByOrigin    map[string]error
	listClosedErrByOrigin  map[string]error
	listOpenDelay          time.Duration
	listOpenIgnoresContext bool
	listOpenCalls          int
	listClosedCalls        int
	listOpenOrigins        []string
	listClosedOrigins      []string
}

func (f *listSessionsVCSProviderFake) ListOpenPRs(ctx context.Context, originURL string) ([]vcs.PRSummary, error) {
	f.mu.Lock()
	f.listOpenCalls++
	f.listOpenOrigins = append(f.listOpenOrigins, originURL)
	delay := f.listOpenDelay
	ignoreContext := f.listOpenIgnoresContext
	f.mu.Unlock()
	if delay > 0 {
		if ignoreContext {
			time.Sleep(delay)
		} else {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.listOpenErrByOrigin[originURL]; ok {
		return nil, err
	}
	if prs, ok := f.openPRsByOrigin[originURL]; ok {
		return prs, nil
	}
	return f.openPRs, nil
}

func (f *listSessionsVCSProviderFake) ListClosedPRs(_ context.Context, originURL string) ([]vcs.PRSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listClosedCalls++
	f.listClosedOrigins = append(f.listClosedOrigins, originURL)
	if err, ok := f.listClosedErrByOrigin[originURL]; ok {
		return nil, err
	}
	if prs, ok := f.closedPRsByOrigin[originURL]; ok {
		return prs, nil
	}
	return nil, nil
}

func (f *listSessionsVCSProviderFake) SearchPRsByTitleTag(_ context.Context, _, _ string) ([]vcs.PRSummary, error) {
	return nil, nil
}

func (f *listSessionsVCSProviderFake) listOpenCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listOpenCalls
}

func (f *listSessionsVCSProviderFake) listClosedCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listClosedCalls
}

type listSessionsAgentChatStoreFake struct {
	db.AgentChatStore
	chats          map[string][]*models.AgentChat
	listErr        error
	listCalls      int
	listBatchCalls int
}

func (f *listSessionsAgentChatStoreFake) ListBySession(_ context.Context, sessionID string) ([]*models.AgentChat, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.chats[sessionID], nil
}

func (f *listSessionsAgentChatStoreFake) ListBySessions(_ context.Context, sessionIDs []string) (map[string][]*models.AgentChat, error) {
	f.listBatchCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	chats := make(map[string][]*models.AgentChat, len(sessionIDs))
	for _, id := range sessionIDs {
		chats[id] = f.chats[id]
	}
	return chats, nil
}
