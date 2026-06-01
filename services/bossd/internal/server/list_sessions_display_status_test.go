package server

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
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
	sessions []*models.Session
}

func (f *listSessionsSessionStoreFake) List(_ context.Context, _ string) ([]*models.Session, error) {
	return f.sessions, nil
}

func (f *listSessionsSessionStoreFake) ListActive(_ context.Context, _ string) ([]*models.Session, error) {
	return f.sessions, nil
}

type listSessionsRepoStoreFake struct {
	db.RepoStore
}

func (f *listSessionsRepoStoreFake) Get(_ context.Context, id string) (*models.Repo, error) {
	if id == "" {
		return nil, sql.ErrNoRows
	}
	return &models.Repo{
		ID:                id,
		DisplayName:       "repo",
		OriginURL:         "https://github.com/recurser/repo.git",
		DefaultBaseBranch: "main",
	}, nil
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
