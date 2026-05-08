package plugin

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"
)

// mockVCSProvider is a minimal mock for vcs.Provider used in host_service tests.
type mockVCSProvider struct {
	openPRs      []vcs.PRSummary
	checkResults []vcs.CheckResult
	prStatus     *vcs.PRStatus
	err          error
}

var _ vcs.Provider = (*mockVCSProvider)(nil)

func (m *mockVCSProvider) ListOpenPRs(_ context.Context, _ string) ([]vcs.PRSummary, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.openPRs, nil
}

func (m *mockVCSProvider) GetCheckResults(_ context.Context, _ string, _ int) ([]vcs.CheckResult, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.checkResults, nil
}

func (m *mockVCSProvider) GetPRStatus(_ context.Context, _ string, _ int) (*vcs.PRStatus, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.prStatus, nil
}

func (m *mockVCSProvider) CreateDraftPR(_ context.Context, _ vcs.CreatePROpts) (*vcs.PRInfo, error) {
	return nil, nil
}
func (m *mockVCSProvider) GetFailedCheckLogs(_ context.Context, _ string, _ string) (string, error) {
	return "", nil
}
func (m *mockVCSProvider) MarkReadyForReview(_ context.Context, _ string, _ int) error { return nil }
func (m *mockVCSProvider) GetReviewComments(_ context.Context, _ string, _ int) ([]vcs.ReviewComment, error) {
	return nil, nil
}
func (m *mockVCSProvider) ListClosedPRs(_ context.Context, _ string) ([]vcs.PRSummary, error) {
	if m.err != nil {
		return nil, m.err
	}
	return nil, nil
}
func (m *mockVCSProvider) MergePR(_ context.Context, _ string, _ int, _ string) error { return nil }
func (m *mockVCSProvider) UpdatePRTitle(_ context.Context, _ string, _ int, _ string) error {
	return nil
}
func (m *mockVCSProvider) GetPRMergeCommit(_ context.Context, _ string, _ int) (string, error) {
	return "", nil
}
func (m *mockVCSProvider) GetAllowedMergeStrategies(_ context.Context, _ string) ([]string, error) {
	return []string{"merge", "squash", "rebase"}, nil
}

func TestHostServiceListOpenPRs(t *testing.T) {
	mock := &mockVCSProvider{
		openPRs: []vcs.PRSummary{
			{Number: 1, Title: "PR One", HeadBranch: "feat/one", State: vcs.PRStateOpen, Author: "alice"},
			{Number: 2, Title: "PR Two", HeadBranch: "feat/two", State: vcs.PRStateOpen, Author: "bob"},
		},
	}
	srv := NewHostServiceServer(mock)

	resp, err := srv.ListOpenPRs(context.Background(), &bossanovav1.ListOpenPRsRequest{
		RepoOriginUrl: "https://github.com/foo/bar",
	})
	if err != nil {
		t.Fatalf("ListOpenPRs: %v", err)
	}
	prs := resp.GetPrs()
	if len(prs) != 2 {
		t.Fatalf("expected 2 PRs, got %d", len(prs))
	}
	if prs[0].GetNumber() != 1 || prs[0].GetTitle() != "PR One" {
		t.Errorf("PR[0] = #%d %q, want #1 %q", prs[0].GetNumber(), prs[0].GetTitle(), "PR One")
	}
	if prs[0].GetAuthor() != "alice" {
		t.Errorf("PR[0].Author = %q, want %q", prs[0].GetAuthor(), "alice")
	}
	if prs[1].GetState() != bossanovav1.PRState_PR_STATE_OPEN {
		t.Errorf("PR[1].State = %v, want OPEN", prs[1].GetState())
	}
}

func TestHostServiceGetCheckResults(t *testing.T) {
	success := vcs.CheckConclusionSuccess
	mock := &mockVCSProvider{
		checkResults: []vcs.CheckResult{
			{ID: "check-1", Name: "CI", Status: vcs.CheckStatusCompleted, Conclusion: &success},
			{ID: "check-2", Name: "Lint", Status: vcs.CheckStatusInProgress},
		},
	}
	srv := NewHostServiceServer(mock)

	resp, err := srv.GetCheckResults(context.Background(), &bossanovav1.GetCheckResultsRequest{
		RepoOriginUrl: "https://github.com/foo/bar",
		PrNumber:      42,
	})
	if err != nil {
		t.Fatalf("GetCheckResults: %v", err)
	}
	checks := resp.GetChecks()
	if len(checks) != 2 {
		t.Fatalf("expected 2 checks, got %d", len(checks))
	}
	if checks[0].GetName() != "CI" {
		t.Errorf("checks[0].Name = %q, want %q", checks[0].GetName(), "CI")
	}
	if checks[0].GetStatus() != bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED {
		t.Errorf("checks[0].Status = %v, want COMPLETED", checks[0].GetStatus())
	}
	if checks[0].Conclusion == nil || *checks[0].Conclusion != bossanovav1.CheckConclusion_CHECK_CONCLUSION_SUCCESS {
		t.Error("checks[0].Conclusion should be SUCCESS")
	}
	if checks[1].GetStatus() != bossanovav1.CheckStatus_CHECK_STATUS_IN_PROGRESS {
		t.Errorf("checks[1].Status = %v, want IN_PROGRESS", checks[1].GetStatus())
	}
	if checks[1].Conclusion != nil {
		t.Error("checks[1].Conclusion should be nil")
	}
}

func TestHostServiceGetPRStatus(t *testing.T) {
	mergeable := true
	mock := &mockVCSProvider{
		prStatus: &vcs.PRStatus{
			State:      vcs.PRStateOpen,
			Mergeable:  &mergeable,
			Title:      "My PR",
			HeadBranch: "feat/thing",
			BaseBranch: "main",
		},
	}
	srv := NewHostServiceServer(mock)

	resp, err := srv.GetPRStatus(context.Background(), &bossanovav1.GetPRStatusRequest{
		RepoOriginUrl: "https://github.com/foo/bar",
		PrNumber:      42,
	})
	if err != nil {
		t.Fatalf("GetPRStatus: %v", err)
	}
	status := resp.GetStatus()
	if status.GetState() != bossanovav1.PRState_PR_STATE_OPEN {
		t.Errorf("State = %v, want OPEN", status.GetState())
	}
	if status.GetTitle() != "My PR" {
		t.Errorf("Title = %q, want %q", status.GetTitle(), "My PR")
	}
	if status.GetHeadBranch() != "feat/thing" {
		t.Errorf("HeadBranch = %q, want %q", status.GetHeadBranch(), "feat/thing")
	}
	if status.Mergeable == nil || !*status.Mergeable {
		t.Error("Mergeable should be true")
	}
}

// fakeSessionStore is a minimal db.SessionStore stub for tests that only
// need Get(id). All other methods panic so a regression that starts using
// them shows up immediately rather than silently returning zero values.
type fakeSessionStore struct {
	db.SessionStore
	sessions map[string]*models.Session
}

func (f *fakeSessionStore) Get(_ context.Context, id string) (*models.Session, error) {
	if s, ok := f.sessions[id]; ok {
		return s, nil
	}
	return nil, errors.New("session not found")
}

// fakeAgentClient records StartRun calls and lets tests script IsRunning /
// ExitStatus responses per session ID. Mirrors agent.AgentRunnerClient just
// enough for the host RPC tests; the methods we don't exercise return zero
// values.
type fakeAgentClient struct {
	mu        sync.Mutex
	startResp string
	startErr  error
	startCall struct {
		workDir string
		plan    string
		logPath string
	}
	running    map[string]bool
	exitErrors map[string]string
	completed  map[string]bool
}

var _ agent.AgentRunnerClient = (*fakeAgentClient)(nil)

func newFakeAgentClient() *fakeAgentClient {
	return &fakeAgentClient{
		running:    make(map[string]bool),
		exitErrors: make(map[string]string),
		completed:  make(map[string]bool),
	}
}

func (f *fakeAgentClient) StartRun(_ context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCall.workDir = req.GetWorkDir()
	f.startCall.plan = req.GetPlan()
	f.startCall.logPath = req.GetLogPath()
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.running[f.startResp] = true
	return &bossanovav1.StartAgentRunResponse{SessionId: f.startResp}, nil
}

func (f *fakeAgentClient) StopRun(_ context.Context, _ *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error) {
	return &bossanovav1.StopAgentRunResponse{}, nil
}

func (f *fakeAgentClient) IsRunning(_ context.Context, req *bossanovav1.IsAgentRunningRequest) (*bossanovav1.IsAgentRunningResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &bossanovav1.IsAgentRunningResponse{Running: f.running[req.GetSessionId()]}, nil
}

func (f *fakeAgentClient) ExitStatus(_ context.Context, req *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &bossanovav1.AgentExitStatusResponse{
		IsComplete: f.completed[req.GetSessionId()],
		ExitError:  f.exitErrors[req.GetSessionId()],
	}, nil
}

func (f *fakeAgentClient) ConfigureFinalizeHook(_ context.Context, _ *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error) {
	return &bossanovav1.ConfigureFinalizeHookResponse{}, nil
}
func (f *fakeAgentClient) BuildInteractiveCommand(_ context.Context, _ *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	return &bossanovav1.BuildInteractiveCommandResponse{}, nil
}
func (f *fakeAgentClient) ListIgnoredDirtyFiles(_ context.Context, _ *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) {
	return &bossanovav1.ListIgnoredDirtyFilesResponse{}, nil
}
func (f *fakeAgentClient) GetChatTitle(_ context.Context, _ *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) {
	return &bossanovav1.GetChatTitleResponse{}, nil
}

// finish marks a session as no-longer-running with the given exit error.
func (f *fakeAgentClient) finish(sessionID, exitErr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running[sessionID] = false
	f.completed[sessionID] = true
	f.exitErrors[sessionID] = exitErr
}

func newRepairTestServer(client agent.AgentRunnerClient, sessions ...*models.Session) *HostServiceServer {
	srv := NewHostServiceServer(&mockVCSProvider{})
	store := &fakeSessionStore{sessions: make(map[string]*models.Session)}
	for _, s := range sessions {
		// Default to the "claude" agent for sessions that don't set one
		// explicitly — preserves the historical single-agent assumption
		// so existing tests stay focused on whatever they were already
		// asserting (queueing, exit handling, etc.).
		if s.AgentName == "" {
			s.AgentName = "claude"
		}
		store.sessions[s.ID] = s
	}
	srv.sessionStore = store
	srv.agentClients = map[string]agent.AgentRunnerClient{"claude": client}
	srv.agentLogsDir = "/tmp/agent-logs"
	return srv
}

func TestStartAgentRun_HappyPath(t *testing.T) {
	client := newFakeAgentClient()
	client.startResp = "claude-abc"
	srv := newRepairTestServer(client, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})

	resp, err := srv.StartAgentRun(t.Context(), &bossanovav1.StartAgentRunHostRequest{
		SessionId: "sess-1",
		Prompt:    "/boss-repair",
	})
	if err != nil {
		t.Fatalf("StartAgentRun: %v", err)
	}
	if resp.GetAgentSessionId() != "claude-abc" {
		t.Fatalf("AgentSessionId = %q, want %q", resp.GetAgentSessionId(), "claude-abc")
	}
	if client.startCall.workDir != "/tmp/wt" {
		t.Errorf("workDir = %q, want /tmp/wt", client.startCall.workDir)
	}
	if client.startCall.plan != "/boss-repair" {
		t.Errorf("plan = %q, want /boss-repair", client.startCall.plan)
	}
	if client.startCall.logPath == "" {
		t.Error("logPath should be set from agentLogsDir")
	}
	if got := srv.activeRuns["sess-1"]; got.agentSessionID != "claude-abc" {
		t.Errorf("activeRuns[sess-1].agentSessionID = %q, want claude-abc", got.agentSessionID)
	}
	if got := srv.activeRuns["sess-1"]; got.agentName != "claude" {
		t.Errorf("activeRuns[sess-1].agentName = %q, want claude", got.agentName)
	}
}

func TestStartAgentRun_NoClient(t *testing.T) {
	srv := NewHostServiceServer(&mockVCSProvider{})
	_, err := srv.StartAgentRun(t.Context(), &bossanovav1.StartAgentRunHostRequest{SessionId: "x", Prompt: "p"})
	if grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", grpcstatus.Code(err))
	}
}

func TestStartAgentRun_SessionNotFound(t *testing.T) {
	client := newFakeAgentClient()
	client.startResp = "claude-xyz"
	srv := newRepairTestServer(client) // no sessions registered

	_, err := srv.StartAgentRun(t.Context(), &bossanovav1.StartAgentRunHostRequest{
		SessionId: "missing",
		Prompt:    "/boss-repair",
	})
	if grpcstatus.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", grpcstatus.Code(err))
	}
}

func TestStartAgentRun_AlreadyActive(t *testing.T) {
	client := newFakeAgentClient()
	client.startResp = "claude-1"
	srv := newRepairTestServer(client, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})

	if _, err := srv.StartAgentRun(t.Context(), &bossanovav1.StartAgentRunHostRequest{
		SessionId: "sess-1", Prompt: "p",
	}); err != nil {
		t.Fatalf("first StartAgentRun: %v", err)
	}

	// Second call while the first is still IsRunning must return AlreadyExists.
	_, err := srv.StartAgentRun(t.Context(), &bossanovav1.StartAgentRunHostRequest{
		SessionId: "sess-1", Prompt: "p",
	})
	if grpcstatus.Code(err) != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists", grpcstatus.Code(err))
	}
}

func TestStartAgentRun_StaleEntryReplaced(t *testing.T) {
	client := newFakeAgentClient()
	client.startResp = "claude-1"
	srv := newRepairTestServer(client, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})

	if _, err := srv.StartAgentRun(t.Context(), &bossanovav1.StartAgentRunHostRequest{
		SessionId: "sess-1", Prompt: "p",
	}); err != nil {
		t.Fatalf("first StartAgentRun: %v", err)
	}
	client.finish("claude-1", "")
	client.startResp = "claude-2"

	resp, err := srv.StartAgentRun(t.Context(), &bossanovav1.StartAgentRunHostRequest{
		SessionId: "sess-1", Prompt: "p",
	})
	if err != nil {
		t.Fatalf("second StartAgentRun after first finished: %v", err)
	}
	if resp.GetAgentSessionId() != "claude-2" {
		t.Errorf("AgentSessionId = %q, want claude-2", resp.GetAgentSessionId())
	}
}

func TestWaitAgentRun_CleanExit(t *testing.T) {
	client := newFakeAgentClient()
	client.finish("claude-x", "")
	srv := newRepairTestServer(client)
	// Seed the reverse-index so WaitAgentRun can route by agent_session_id
	// alone — production seeds this from StartAgentRun, but these unit
	// tests exercise WaitAgentRun in isolation.
	srv.agentSessionByID["claude-x"] = "claude"

	resp, err := srv.WaitAgentRun(t.Context(), &bossanovav1.WaitAgentRunHostRequest{AgentSessionId: "claude-x"})
	if err != nil {
		t.Fatalf("WaitAgentRun: %v", err)
	}
	if resp.GetExitError() != "" {
		t.Errorf("ExitError = %q, want empty", resp.GetExitError())
	}
}

func TestWaitAgentRun_NonZeroExit(t *testing.T) {
	client := newFakeAgentClient()
	client.finish("claude-x", "exit status 1")
	srv := newRepairTestServer(client)
	srv.agentSessionByID["claude-x"] = "claude"

	resp, err := srv.WaitAgentRun(t.Context(), &bossanovav1.WaitAgentRunHostRequest{AgentSessionId: "claude-x"})
	if err != nil {
		t.Fatalf("WaitAgentRun: %v", err)
	}
	if resp.GetExitError() != "exit status 1" {
		t.Errorf("ExitError = %q, want %q", resp.GetExitError(), "exit status 1")
	}
}

func TestWaitAgentRun_ContextCancel(t *testing.T) {
	client := newFakeAgentClient()
	client.running["claude-x"] = true // never completes
	srv := newRepairTestServer(client)
	srv.agentSessionByID["claude-x"] = "claude"

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := srv.WaitAgentRun(ctx, &bossanovav1.WaitAgentRunHostRequest{AgentSessionId: "claude-x"})
	if grpcstatus.Code(err) != codes.Canceled {
		t.Fatalf("code = %v, want Canceled", grpcstatus.Code(err))
	}
}

// TestStartAgentRun_RoutesByAgentName verifies that when multiple agent
// clients are registered, StartAgentRun forwards to the one whose name
// matches the session's AgentName. The other clients must remain
// untouched — that's the per-session routing the multi-agent migration
// is selling.
func TestStartAgentRun_RoutesByAgentName(t *testing.T) {
	claudeClient := newFakeAgentClient()
	claudeClient.startResp = "claude-1"
	openCodeClient := newFakeAgentClient()
	openCodeClient.startResp = "opencode-1"

	srv := NewHostServiceServer(&mockVCSProvider{})
	store := &fakeSessionStore{sessions: map[string]*models.Session{
		"sess-opencode": {ID: "sess-opencode", WorktreePath: "/tmp/wt", AgentName: "opencode"},
	}}
	srv.sessionStore = store
	srv.agentClients = map[string]agent.AgentRunnerClient{
		"claude":   claudeClient,
		"opencode": openCodeClient,
	}
	srv.agentLogsDir = "/tmp/agent-logs"

	resp, err := srv.StartAgentRun(t.Context(), &bossanovav1.StartAgentRunHostRequest{
		SessionId: "sess-opencode",
		Prompt:    "/repair",
	})
	if err != nil {
		t.Fatalf("StartAgentRun: %v", err)
	}
	if resp.GetAgentSessionId() != "opencode-1" {
		t.Errorf("AgentSessionId = %q, want opencode-1", resp.GetAgentSessionId())
	}
	if openCodeClient.startCall.workDir == "" {
		t.Error("expected opencode StartRun to be called")
	}
	if claudeClient.startCall.workDir != "" {
		t.Error("did not expect claude StartRun to be called")
	}
	if got := srv.agentSessionByID["opencode-1"]; got != "opencode" {
		t.Errorf("reverse index = %q, want opencode", got)
	}
}

// TestStartAgentRun_UnknownAgent verifies that StartAgentRun returns a
// FailedPrecondition error when the session's AgentName has no entry in
// the registry. Defense in depth against drift between persisted
// agent_name and the loaded plugin set.
func TestStartAgentRun_UnknownAgent(t *testing.T) {
	client := newFakeAgentClient()
	srv := NewHostServiceServer(&mockVCSProvider{})
	store := &fakeSessionStore{sessions: map[string]*models.Session{
		"sess-1": {ID: "sess-1", WorktreePath: "/tmp/wt", AgentName: "ghost"},
	}}
	srv.sessionStore = store
	srv.agentClients = map[string]agent.AgentRunnerClient{"claude": client}
	srv.agentLogsDir = "/tmp/agent-logs"

	_, err := srv.StartAgentRun(t.Context(), &bossanovav1.StartAgentRunHostRequest{
		SessionId: "sess-1", Prompt: "p",
	})
	if grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", grpcstatus.Code(err))
	}
}

// fakeRepoStore is a minimal db.RepoStore stub that serves a fixed list of
// repos from List(). All other methods panic to surface unexpected usage.
type fakeRepoStore struct {
	db.RepoStore
	repos []*models.Repo
}

func (f *fakeRepoStore) List(_ context.Context) ([]*models.Repo, error) {
	return f.repos, nil
}

// fakeSessionStoreWithListActive extends fakeSessionStore with ListActive so
// that ListSessions tests can control which sessions are returned per repo.
type fakeSessionStoreWithListActive struct {
	fakeSessionStore
	// sessionsByRepo maps repoID → sessions returned by ListActive.
	sessionsByRepo map[string][]*models.Session
}

func (f *fakeSessionStoreWithListActive) ListActive(_ context.Context, repoID string) ([]*models.Session, error) {
	return f.sessionsByRepo[repoID], nil
}

// fakeAgentChatStore is a minimal db.AgentChatStore stub that returns a
// pre-configured list of chats for a given session ID.
type fakeAgentChatStore struct {
	db.AgentChatStore
	chatsBySession map[string][]*models.AgentChat
}

func (f *fakeAgentChatStore) ListBySession(_ context.Context, sessionID string) ([]*models.AgentChat, error) {
	return f.chatsBySession[sessionID], nil
}

// TestListSessions_PopulatesLastChatActivityAt verifies that ListSessions sets
// LastChatActivityAt to the maximum LastOutputAt across all live chat tracker
// entries for a session, and that HasActiveChat remains true.
func TestListSessions_PopulatesLastChatActivityAt(t *testing.T) {
	olderTime := time.Now().Add(-30 * time.Second)
	newerTime := time.Now().Add(-5 * time.Second)

	repoID := "repo-1"
	sessID := "sess-1"

	// Build the real Tracker and seed two live entries (both within
	// StaleThreshold via Update, which stamps ReceivedAt = time.Now()).
	tracker := status.NewTracker()
	tracker.Update("chat-old", bossanovav1.ChatStatus_CHAT_STATUS_IDLE, olderTime)
	tracker.Update("chat-new", bossanovav1.ChatStatus_CHAT_STATUS_IDLE, newerTime)

	srv := NewHostServiceServer(&mockVCSProvider{})
	srv.repoStore = &fakeRepoStore{
		repos: []*models.Repo{{ID: repoID, DisplayName: "Test Repo"}},
	}
	srv.sessionStore = &fakeSessionStoreWithListActive{
		sessionsByRepo: map[string][]*models.Session{
			repoID: {{ID: sessID, RepoID: repoID, Title: "my session"}},
		},
	}
	srv.displayTracker = status.NewDisplayTracker()
	srv.agentChats = &fakeAgentChatStore{
		chatsBySession: map[string][]*models.AgentChat{
			sessID: {
				// Order matters: newer-first ensures a buggy "last-wins"
				// implementation would fail (it would keep the older value).
				{AgentSessionID: "chat-new"},
				{AgentSessionID: "chat-old"},
			},
		},
	}
	srv.chatTracker = tracker

	resp, err := srv.ListSessions(t.Context(), &bossanovav1.HostServiceListSessionsRequest{})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	sessions := resp.GetSessions()
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	s := sessions[0]
	if !s.GetHasActiveChat() {
		t.Error("HasActiveChat should be true")
	}
	if s.GetLastChatActivityAt() == nil {
		t.Fatal("LastChatActivityAt should be populated")
	}
	gotTime := s.GetLastChatActivityAt().AsTime()
	// 1ms tolerance for any rounding through timestamppb.
	if gotTime.Sub(newerTime).Abs() > time.Millisecond {
		t.Errorf("LastChatActivityAt = %v, want %v (the newer time)", gotTime, newerTime)
	}
}

// TestListSessions_LeavesLastChatActivityNilWhenNoLiveChat verifies that when a
// session has no chats registered against it, ListSessions leaves
// LastChatActivityAt nil and HasActiveChat false.
func TestListSessions_LeavesLastChatActivityNilWhenNoLiveChat(t *testing.T) {
	repoID := "repo-1"
	sessID := "sess-1"

	srv := NewHostServiceServer(&mockVCSProvider{})
	srv.repoStore = &fakeRepoStore{
		repos: []*models.Repo{{ID: repoID, DisplayName: "Test Repo"}},
	}
	srv.sessionStore = &fakeSessionStoreWithListActive{
		sessionsByRepo: map[string][]*models.Session{
			repoID: {{ID: sessID, RepoID: repoID, Title: "my session"}},
		},
	}
	srv.displayTracker = status.NewDisplayTracker()
	// No chats registered for this session.
	srv.agentChats = &fakeAgentChatStore{
		chatsBySession: map[string][]*models.AgentChat{},
	}
	srv.chatTracker = status.NewTracker()

	resp, err := srv.ListSessions(t.Context(), &bossanovav1.HostServiceListSessionsRequest{})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	sessions := resp.GetSessions()
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	s := sessions[0]
	if s.GetHasActiveChat() {
		t.Error("HasActiveChat should be false when no chats exist")
	}
	if s.GetLastChatActivityAt() != nil {
		t.Errorf("LastChatActivityAt should be nil, got %v", s.GetLastChatActivityAt())
	}
}

func TestHostServiceProviderErrorPropagates(t *testing.T) {
	mock := &mockVCSProvider{
		err: errors.New("GitHub API rate limit exceeded"),
	}
	srv := NewHostServiceServer(mock)
	ctx := context.Background()

	_, err := srv.ListOpenPRs(ctx, &bossanovav1.ListOpenPRsRequest{RepoOriginUrl: "https://github.com/foo/bar"})
	if err == nil {
		t.Fatal("expected error from ListOpenPRs")
	}

	_, err = srv.GetCheckResults(ctx, &bossanovav1.GetCheckResultsRequest{RepoOriginUrl: "https://github.com/foo/bar", PrNumber: 1})
	if err == nil {
		t.Fatal("expected error from GetCheckResults")
	}

	_, err = srv.GetPRStatus(ctx, &bossanovav1.GetPRStatusRequest{RepoOriginUrl: "https://github.com/foo/bar", PrNumber: 1})
	if err == nil {
		t.Fatal("expected error from GetPRStatus")
	}
}

// --- CompleteAgentRun (Task 3) ---

// newRunCompleteServer builds a HostServiceServer wired with a real
// DisplayTracker so CompleteAgentRun's SetRepairing(false) path is
// observable by the test.
func newRunCompleteServer() *HostServiceServer {
	srv := NewHostServiceServer(&mockVCSProvider{})
	srv.displayTracker = status.NewDisplayTracker()
	return srv
}

func TestCompleteAgentRun_HappyPath(t *testing.T) {
	srv := newRunCompleteServer()
	srv.displayTracker.SetRepairing("sess-1", true)
	ch := srv.registerRun("sess-1", "agent-1", "tok-1")

	sessionID, err := srv.CompleteAgentRun(t.Context(), "agent-1", "tok-1", "")
	if err != nil {
		t.Fatalf("CompleteAgentRun: %v", err)
	}
	if sessionID != "sess-1" {
		t.Errorf("sessionID = %q, want sess-1", sessionID)
	}
	select {
	case res := <-ch:
		if res.exitError != "" {
			t.Errorf("exitError = %q, want empty", res.exitError)
		}
	case <-time.After(time.Second):
		t.Fatal("completion channel did not receive a value")
	}
	// Channel entry cleared on first signal so a duplicate POST won't
	// double-signal. Token + reverse-session entries intentionally survive
	// (WaitChatRun owns their cleanup) so duplicate POSTs still authenticate
	// and short-circuit to 200.
	srv.runMu.Lock()
	_, hasComp := srv.runCompletion["agent-1"]
	_, hasTok := srv.runHookTokens["agent-1"]
	_, hasSess := srv.runSessionByID["agent-1"]
	srv.runMu.Unlock()
	if hasComp {
		t.Error("runCompletion[agent-1] should be cleared after first signal")
	}
	if !hasTok {
		t.Error("runHookTokens[agent-1] should survive — WaitChatRun owns cleanup")
	}
	if !hasSess {
		t.Error("runSessionByID[agent-1] should survive — WaitChatRun owns cleanup")
	}
	// IsRepairing flag cleared.
	if entry := srv.displayTracker.Get("sess-1"); entry != nil && entry.IsRepairing {
		t.Error("IsRepairing should be cleared after CompleteAgentRun")
	}
}

func TestCompleteAgentRun_PropagatesExitError(t *testing.T) {
	srv := newRunCompleteServer()
	ch := srv.registerRun("sess-1", "agent-1", "tok-1")

	const exitMsg = "claude crashed: signal: killed"
	if _, err := srv.CompleteAgentRun(t.Context(), "agent-1", "tok-1", exitMsg); err != nil {
		t.Fatalf("CompleteAgentRun: %v", err)
	}
	select {
	case res := <-ch:
		if res.exitError != exitMsg {
			t.Errorf("exitError = %q, want %q", res.exitError, exitMsg)
		}
	case <-time.After(time.Second):
		t.Fatal("completion channel did not receive a value")
	}
}

func TestCompleteAgentRun_AuthMismatch(t *testing.T) {
	srv := newRunCompleteServer()
	ch := srv.registerRun("sess-1", "agent-1", "right-token")

	sessionID, err := srv.CompleteAgentRun(t.Context(), "agent-1", "wrong-token", "")
	if !errors.Is(err, ErrAuthMismatch) {
		t.Fatalf("err = %v, want ErrAuthMismatch", err)
	}
	if sessionID != "sess-1" {
		t.Errorf("sessionID = %q, want sess-1 (caller may want to log it)", sessionID)
	}
	// Channel still empty — auth failure must not signal.
	select {
	case <-ch:
		t.Error("completion channel signalled despite auth mismatch")
	case <-time.After(50 * time.Millisecond):
	}
	// Run state still present so a retried POST with the right token works.
	srv.runMu.Lock()
	_, hasTok := srv.runHookTokens["agent-1"]
	srv.runMu.Unlock()
	if !hasTok {
		t.Error("run state cleared on auth mismatch; should remain so retries can succeed")
	}
}

func TestCompleteAgentRun_UnknownAgentSession(t *testing.T) {
	srv := newRunCompleteServer()
	sessionID, err := srv.CompleteAgentRun(t.Context(), "unknown", "tok", "")
	if !errors.Is(err, ErrAgentRunNotFound) {
		t.Fatalf("err = %v, want ErrAgentRunNotFound", err)
	}
	if sessionID != "" {
		t.Errorf("sessionID = %q, want empty for unknown id", sessionID)
	}
}

func TestCompleteAgentRun_DuplicatePOSTIdempotent(t *testing.T) {
	srv := newRunCompleteServer()
	ch := srv.registerRun("sess-1", "agent-1", "tok-1")

	// First POST signals the channel.
	sessionID, err := srv.CompleteAgentRun(t.Context(), "agent-1", "tok-1", "first")
	if err != nil {
		t.Fatalf("first CompleteAgentRun: %v", err)
	}
	if sessionID != "sess-1" {
		t.Fatalf("first CompleteAgentRun: sessionID=%q, want sess-1", sessionID)
	}
	// Second POST: token + reverse-session entries still present, channel
	// entry was cleared on the first call. Spec says duplicate POSTs are
	// idempotent and return 200 → err=nil, sessionID populated.
	sessionID2, err2 := srv.CompleteAgentRun(t.Context(), "agent-1", "tok-1", "second")
	if err2 != nil {
		t.Fatalf("second CompleteAgentRun: %v (expected nil — duplicate POST is idempotent per spec)", err2)
	}
	if sessionID2 != "sess-1" {
		t.Errorf("second CompleteAgentRun sessionID=%q, want sess-1", sessionID2)
	}
	// Channel received exactly the first signal.
	select {
	case res := <-ch:
		if res.exitError != "first" {
			t.Errorf("exitError = %q, want %q (only the first POST should have signalled)", res.exitError, "first")
		}
	case <-time.After(time.Second):
		t.Fatal("first POST did not signal channel")
	}
	// No second value — duplicate POST must not double-signal.
	select {
	case res, ok := <-ch:
		if ok {
			t.Errorf("unexpected second value on channel: %+v", res)
		}
	case <-time.After(50 * time.Millisecond):
		// Channel is empty (and not closed), which is the spec'd behavior.
	}
}

// TestCompleteAgentRun_DuplicatePOSTWrongTokenAfterSignal a wrong-Bearer
// POST that arrives after the legitimate completion still returns
// ErrAuthMismatch (HTTP 401). The token entry must survive past the first
// successful signal so spoofed retries can't masquerade as 404s.
func TestCompleteAgentRun_DuplicatePOSTWrongTokenAfterSignal(t *testing.T) {
	srv := newRunCompleteServer()
	srv.registerRun("sess-1", "agent-1", "right-token")

	if _, err := srv.CompleteAgentRun(t.Context(), "agent-1", "right-token", ""); err != nil {
		t.Fatalf("first CompleteAgentRun: %v", err)
	}
	sessionID, err := srv.CompleteAgentRun(t.Context(), "agent-1", "wrong-token", "")
	if !errors.Is(err, ErrAuthMismatch) {
		t.Fatalf("err = %v, want ErrAuthMismatch (token entry survived first signal)", err)
	}
	if sessionID != "sess-1" {
		t.Errorf("sessionID = %q, want sess-1", sessionID)
	}
}

// TestCompleteAgentRun_ClearsActiveRunsWhenMatchingSession verifies that
// CompleteAgentRun also clears the activeRuns entry for the originating
// boss session when the agent_session_id matches the recorded one. Task 4
// will populate activeRuns from StartChatRun; this test stages it by hand
// to lock in the cleanup contract now.
func TestCompleteAgentRun_ClearsActiveRunsWhenMatchingSession(t *testing.T) {
	srv := newRunCompleteServer()
	srv.activeRuns["sess-1"] = activeRun{agentName: "claude", agentSessionID: "agent-1"}
	srv.agentSessionByID["agent-1"] = "claude"
	srv.registerRun("sess-1", "agent-1", "tok-1")

	if _, err := srv.CompleteAgentRun(t.Context(), "agent-1", "tok-1", ""); err != nil {
		t.Fatalf("CompleteAgentRun: %v", err)
	}
	srv.runMu.Lock()
	_, hasActive := srv.activeRuns["sess-1"]
	_, hasReverse := srv.agentSessionByID["agent-1"]
	srv.runMu.Unlock()
	if hasActive {
		t.Error("activeRuns[sess-1] should be cleared when its agent_session_id matches")
	}
	if hasReverse {
		t.Error("agentSessionByID[agent-1] should be cleared after completion")
	}
}

// TestCompleteAgentRun_LeavesActiveRunsWhenSessionRecordsDifferentAgent
// verifies that if a *new* repair already replaced the activeRuns entry
// with a different agent_session_id (e.g. previous run was stale, sweep
// fired again), an in-flight POST for the *old* id doesn't tear the new
// run's state down by accident.
func TestCompleteAgentRun_LeavesActiveRunsWhenSessionRecordsDifferentAgent(t *testing.T) {
	srv := newRunCompleteServer()
	srv.activeRuns["sess-1"] = activeRun{agentName: "claude", agentSessionID: "agent-2"}
	srv.agentSessionByID["agent-2"] = "claude"
	srv.registerRun("sess-1", "agent-1", "tok-1") // old run

	if _, err := srv.CompleteAgentRun(t.Context(), "agent-1", "tok-1", ""); err != nil {
		t.Fatalf("CompleteAgentRun: %v", err)
	}
	srv.runMu.Lock()
	cur, hasActive := srv.activeRuns["sess-1"]
	_, hasNewReverse := srv.agentSessionByID["agent-2"]
	srv.runMu.Unlock()
	if !hasActive || cur.agentSessionID != "agent-2" {
		t.Errorf("activeRuns[sess-1] = %+v, want agent_session_id=agent-2 untouched", cur)
	}
	if !hasNewReverse {
		t.Error("agentSessionByID[agent-2] (new run) should remain")
	}
}

// setWaitChatRunDeadline is a test-only helper that overrides the default
// 30m WaitChatRun deadline. Lives in *_test.go (rather than host_service.go)
// so the production binary doesn't expose a knob whose only legitimate
// caller is the deadline test below. A zero or negative duration is
// silently ignored — the existing deadline stays in place.
func (s *HostServiceServer) setWaitChatRunDeadline(d time.Duration) {
	if d <= 0 {
		return
	}
	s.waitChatRunDeadline = d
}

// --- StartChatRun / WaitChatRun (Task 4) ---

// fakeChatLifecycle is a programmable ChatLifecycle for the StartChatRun
// tests. It records the last call (so tests can assert prompt/title/token
// were forwarded) and returns whatever startResp / startErr was scripted.
type fakeChatLifecycle struct {
	mu         sync.Mutex
	startCalls int
	lastReq    struct {
		sessionID string
		prompt    string
		title     string
		hookOpts  session.HookOpts
	}
	startResp string
	startErr  error
	// startFunc, when non-nil, takes precedence over startResp/startErr.
	// Used by the AlreadyExists test to have the lifecycle return the
	// existing agent_session_id alongside a typed gRPC error.
	startFunc func(ctx context.Context, sessionID, prompt, title string, hookOpts session.HookOpts) (string, error)
}

func (f *fakeChatLifecycle) StartTmuxChat(ctx context.Context, sessionID, prompt, title string, hookOpts session.HookOpts) (string, error) {
	f.mu.Lock()
	f.startCalls++
	f.lastReq.sessionID = sessionID
	f.lastReq.prompt = prompt
	f.lastReq.title = title
	f.lastReq.hookOpts = hookOpts
	fn := f.startFunc
	resp, err := f.startResp, f.startErr
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, sessionID, prompt, title, hookOpts)
	}
	return resp, err
}

// newChatRunTestServer builds a HostServiceServer wired with a fake
// ChatLifecycle and a session store, plus the agent registries
// StartChatRun's preconditions look at. Mirrors newRepairTestServer's
// shape so tests stay readable.
func newChatRunTestServer(lc *fakeChatLifecycle, sessions ...*models.Session) *HostServiceServer {
	srv := NewHostServiceServer(&mockVCSProvider{})
	store := &fakeSessionStore{sessions: make(map[string]*models.Session)}
	for _, s := range sessions {
		if s.AgentName == "" {
			s.AgentName = "claude"
		}
		store.sessions[s.ID] = s
	}
	srv.sessionStore = store
	// agentClients is required for StartChatRun's precondition check; the
	// fake has no methods that get called from StartChatRun directly, but
	// the existence check must pass.
	srv.agentClients = map[string]agent.AgentRunnerClient{"claude": newFakeAgentClient()}
	srv.agentLogsDir = "/tmp/agent-logs"
	srv.lifecycle = lc
	srv.displayTracker = status.NewDisplayTracker()
	return srv
}

func TestStartChatRun_HappyPath(t *testing.T) {
	lc := &fakeChatLifecycle{startResp: "agent-abc"}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})

	resp, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId: "sess-1",
		Prompt:    "/boss-repair",
		Title:     "Repair: sess-1",
	})
	if err != nil {
		t.Fatalf("StartChatRun: %v", err)
	}
	if resp.GetAgentSessionId() != "agent-abc" {
		t.Fatalf("AgentSessionId = %q, want agent-abc", resp.GetAgentSessionId())
	}

	if lc.startCalls != 1 {
		t.Errorf("StartTmuxChat call count = %d, want 1", lc.startCalls)
	}
	if lc.lastReq.sessionID != "sess-1" || lc.lastReq.prompt != "/boss-repair" || lc.lastReq.title != "Repair: sess-1" {
		t.Errorf("StartTmuxChat req = %+v, want sess-1 / /boss-repair / Repair: sess-1", lc.lastReq)
	}
	if lc.lastReq.hookOpts.Token == "" {
		t.Error("expected non-empty HookOpts.Token forwarded to StartTmuxChat")
	}

	// All five run-state maps were populated under the agent_session_id.
	srv.runMu.Lock()
	defer srv.runMu.Unlock()
	if got := srv.activeRuns["sess-1"]; got.agentSessionID != "agent-abc" {
		t.Errorf("activeRuns[sess-1].agentSessionID = %q, want agent-abc", got.agentSessionID)
	}
	if got := srv.agentSessionByID["agent-abc"]; got != "claude" {
		t.Errorf("agentSessionByID[agent-abc] = %q, want claude", got)
	}
	if _, ok := srv.runCompletion["agent-abc"]; !ok {
		t.Error("runCompletion[agent-abc] should be present")
	}
	if got := srv.runHookTokens["agent-abc"]; got == "" {
		t.Error("runHookTokens[agent-abc] should be populated")
	}
	if got := srv.runHookTokens["agent-abc"]; got != lc.lastReq.hookOpts.Token {
		t.Errorf("runHookTokens[agent-abc] = %q, want token forwarded to StartTmuxChat (%q)", got, lc.lastReq.hookOpts.Token)
	}
	if got := srv.runSessionByID["agent-abc"]; got != "sess-1" {
		t.Errorf("runSessionByID[agent-abc] = %q, want sess-1", got)
	}
}

func TestStartChatRun_EmptySessionID(t *testing.T) {
	lc := &fakeChatLifecycle{startResp: "agent-x"}
	srv := newChatRunTestServer(lc)

	_, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		Prompt: "p",
		Title:  "T",
	})
	if grpcstatus.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", grpcstatus.Code(err))
	}
}

func TestStartChatRun_EmptyPrompt(t *testing.T) {
	lc := &fakeChatLifecycle{startResp: "agent-x"}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})

	_, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId: "sess-1",
		Title:     "T",
	})
	if grpcstatus.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", grpcstatus.Code(err))
	}
}

func TestStartChatRun_EmptyTitle(t *testing.T) {
	lc := &fakeChatLifecycle{startResp: "agent-x"}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})

	_, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId: "sess-1",
		Prompt:    "p",
	})
	if grpcstatus.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", grpcstatus.Code(err))
	}
}

func TestStartChatRun_SessionNotFound(t *testing.T) {
	lc := &fakeChatLifecycle{startResp: "agent-x"}
	srv := newChatRunTestServer(lc) // no sessions registered

	_, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId: "missing",
		Prompt:    "p",
		Title:     "T",
	})
	if grpcstatus.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", grpcstatus.Code(err))
	}
}

func TestStartChatRun_NoAgentLogsDir(t *testing.T) {
	lc := &fakeChatLifecycle{startResp: "agent-x"}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})
	srv.agentLogsDir = ""

	_, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId: "sess-1",
		Prompt:    "p",
		Title:     "T",
	})
	if grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", grpcstatus.Code(err))
	}
}

func TestStartChatRun_NoAgentClients(t *testing.T) {
	lc := &fakeChatLifecycle{startResp: "agent-x"}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})
	srv.agentClients = map[string]agent.AgentRunnerClient{} // empty

	_, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId: "sess-1",
		Prompt:    "p",
		Title:     "T",
	})
	if grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", grpcstatus.Code(err))
	}
}

func TestStartChatRun_UnknownAgentForSession(t *testing.T) {
	lc := &fakeChatLifecycle{startResp: "agent-x"}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt", AgentName: "ghost"})

	_, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId: "sess-1",
		Prompt:    "p",
		Title:     "T",
	})
	if grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", grpcstatus.Code(err))
	}
}

func TestStartChatRun_NoLifecycle(t *testing.T) {
	srv := NewHostServiceServer(&mockVCSProvider{})
	srv.sessionStore = &fakeSessionStore{sessions: map[string]*models.Session{
		"sess-1": {ID: "sess-1", WorktreePath: "/tmp/wt", AgentName: "claude"},
	}}
	srv.agentClients = map[string]agent.AgentRunnerClient{"claude": newFakeAgentClient()}
	srv.agentLogsDir = "/tmp/agent-logs"
	// Deliberately don't call SetLifecycle.

	_, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId: "sess-1",
		Prompt:    "p",
		Title:     "T",
	})
	if grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", grpcstatus.Code(err))
	}
}

// TestStartChatRun_ConcurrentSecondReturnsAlreadyExists pins the
// activeRuns precondition: a second StartChatRun for the same session,
// while the first run is still alive, gets AlreadyExists from the daemon.
//
// We simulate "still alive" by hand-wiring activeRuns + IsRunning via the
// fakeAgentClient.running map that the precondition check consults.
func TestStartChatRun_ConcurrentSecondReturnsAlreadyExists(t *testing.T) {
	lc := &fakeChatLifecycle{startResp: "agent-1"}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})

	if _, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId: "sess-1", Prompt: "p", Title: "T",
	}); err != nil {
		t.Fatalf("first StartChatRun: %v", err)
	}

	// Make IsRunning report the first run as still active so the
	// activeRuns precondition fires AlreadyExists.
	srv.agentClients["claude"].(*fakeAgentClient).mu.Lock()
	srv.agentClients["claude"].(*fakeAgentClient).running["agent-1"] = true
	srv.agentClients["claude"].(*fakeAgentClient).mu.Unlock()

	_, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId: "sess-1", Prompt: "p", Title: "T",
	})
	if grpcstatus.Code(err) != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists", grpcstatus.Code(err))
	}
}

// TestStartChatRun_LifecycleAlreadyExistsPropagated verifies that when
// Lifecycle.StartTmuxChat itself returns AlreadyExists (eg. tmux from a
// previous daemon session is still alive), the host RPC echoes the
// existing agent_session_id in the response AND encodes it into the
// gRPC error message so cross-process callers (whose response body is
// dropped when err != nil) can still parse it back out.
//
// In-process callers see the response body directly. Cross-process
// callers see only the status — see TestStartChatRun_AlreadyExistsAcrossWire
// below for the wire-level guarantee.
func TestStartChatRun_LifecycleAlreadyExistsPropagated(t *testing.T) {
	const existing = "agent-existing-1234"
	lc := &fakeChatLifecycle{
		startFunc: func(_ context.Context, _, _, _ string, _ session.HookOpts) (string, error) {
			return existing, grpcstatus.Errorf(codes.AlreadyExists, "tmux chat already active")
		},
	}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})

	resp, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId: "sess-1", Prompt: "p", Title: "T",
	})
	if grpcstatus.Code(err) != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists", grpcstatus.Code(err))
	}
	if resp == nil || resp.GetAgentSessionId() != existing {
		t.Errorf("AgentSessionId = %v, want %q echoed alongside AlreadyExists", resp, existing)
	}
	// The error message must also carry the existing id in the documented
	// `agent_session_id=<id>` shape so cross-process callers can read it
	// back from the gRPC status (see TestStartChatRun_AlreadyExistsAcrossWire).
	if msg := grpcstatus.Convert(err).Message(); !strings.Contains(msg, "agent_session_id="+existing) {
		t.Errorf("error message = %q, want to contain agent_session_id=%s", msg, existing)
	}
}

// TestStartChatRun_AlreadyExistsAcrossWire confirms that the existing
// agent_session_id round-trips through a real gRPC server when the
// lifecycle returns AlreadyExists. The reviewer's concern: gRPC drops
// the response body when err != nil, so the in-process echo doesn't
// reach a cross-process plugin caller. Mitigation: the host RPC encodes
// the existing id into the gRPC error message in a parseable shape
// (`agent_session_id=<id>`).
//
// This test stands up an in-process gRPC server bound to a Unix socket,
// dials it as a real client, and verifies the parseable encoding survives
// the wire round-trip.
func TestStartChatRun_AlreadyExistsAcrossWire(t *testing.T) {
	const existing = "agent-existing-wire-9999"
	lc := &fakeChatLifecycle{
		startFunc: func(_ context.Context, _, _, _ string, _ session.HookOpts) (string, error) {
			return existing, grpcstatus.Errorf(codes.AlreadyExists, "tmux chat already active")
		},
	}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})

	// Stand up an in-process gRPC server on loopback TCP. Mirrors the
	// way go-plugin's broker dispenses HostService in production — a
	// raw gRPC server that the plugin client dials over its own
	// channel. The transport details don't matter; what matters is that
	// the server-side response body is dropped when err != nil, which
	// happens for any gRPC implementation. (We avoid Unix sockets here
	// because t.TempDir paths on macOS exceed the sun_path limit.)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	gs := grpc.NewServer()
	srv.Register(gs)
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient(
		ln.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// Use Invoke directly (matches the project pattern: no generated
	// HostServiceClient because we use connect-go, not protoc-gen-go-grpc).
	resp := new(bossanovav1.StartChatRunHostResponse)
	wireErr := conn.Invoke(t.Context(), "/bossanova.v1.HostService/StartChatRun", &bossanovav1.StartChatRunHostRequest{
		SessionId: "sess-1", Prompt: "p", Title: "T",
	}, resp)
	if grpcstatus.Code(wireErr) != codes.AlreadyExists {
		t.Fatalf("wire code = %v, want AlreadyExists; err = %v", grpcstatus.Code(wireErr), wireErr)
	}
	// gRPC drops the response body when err != nil. The id must therefore
	// be recoverable from the error message — that's the promise the host
	// RPC makes to cross-process callers (Task 5's repair plugin).
	msg := grpcstatus.Convert(wireErr).Message()
	re := regexp.MustCompile(`agent_session_id=([^)]+)`)
	matches := re.FindStringSubmatch(msg)
	if len(matches) != 2 {
		t.Fatalf("error message = %q, want to match %q", msg, re.String())
	}
	if matches[1] != existing {
		t.Errorf("parsed agent_session_id = %q, want %q", matches[1], existing)
	}
}

func TestWaitChatRun_HookSignalsCleanExit(t *testing.T) {
	srv := newRunCompleteServer()
	srv.runHookTokens["agent-x"] = "tok"
	srv.runSessionByID["agent-x"] = "sess-1"
	srv.runCompletion["agent-x"] = make(chan completionResult, 1)

	// Drive the hook from a goroutine so the wait actually sleeps before
	// the signal arrives (mirrors the production race window).
	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = srv.CompleteAgentRun(context.Background(), "agent-x", "tok", "")
	}()

	resp, err := srv.WaitChatRun(t.Context(), &bossanovav1.WaitChatRunHostRequest{AgentSessionId: "agent-x"})
	if err != nil {
		t.Fatalf("WaitChatRun: %v", err)
	}
	if resp.GetExitError() != "" {
		t.Errorf("ExitError = %q, want empty", resp.GetExitError())
	}

	// All maps cleaned up.
	srv.runMu.Lock()
	defer srv.runMu.Unlock()
	if _, ok := srv.runCompletion["agent-x"]; ok {
		t.Error("runCompletion[agent-x] should be cleared after WaitChatRun returns")
	}
	if _, ok := srv.runHookTokens["agent-x"]; ok {
		t.Error("runHookTokens[agent-x] should be cleared after WaitChatRun returns")
	}
	if _, ok := srv.runSessionByID["agent-x"]; ok {
		t.Error("runSessionByID[agent-x] should be cleared after WaitChatRun returns")
	}
}

func TestWaitChatRun_PropagatesExitError(t *testing.T) {
	srv := newRunCompleteServer()
	srv.runHookTokens["agent-x"] = "tok"
	srv.runSessionByID["agent-x"] = "sess-1"
	srv.runCompletion["agent-x"] = make(chan completionResult, 1)

	const wantErr = "claude crashed: signal: killed"
	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = srv.CompleteAgentRun(context.Background(), "agent-x", "tok", wantErr)
	}()

	resp, err := srv.WaitChatRun(t.Context(), &bossanovav1.WaitChatRunHostRequest{AgentSessionId: "agent-x"})
	if err != nil {
		t.Fatalf("WaitChatRun: %v", err)
	}
	if resp.GetExitError() != wantErr {
		t.Errorf("ExitError = %q, want %q", resp.GetExitError(), wantErr)
	}
}

func TestWaitChatRun_UnknownAgentSessionID(t *testing.T) {
	srv := newRunCompleteServer()

	_, err := srv.WaitChatRun(t.Context(), &bossanovav1.WaitChatRunHostRequest{AgentSessionId: "unknown"})
	if grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", grpcstatus.Code(err))
	}
}

func TestWaitChatRun_EmptyAgentSessionID(t *testing.T) {
	srv := newRunCompleteServer()
	_, err := srv.WaitChatRun(t.Context(), &bossanovav1.WaitChatRunHostRequest{})
	if grpcstatus.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", grpcstatus.Code(err))
	}
}

// TestWaitChatRun_DeadlineExpires verifies that when no Stop hook
// arrives, the synthetic exit_error fires and all maps are cleaned up
// so a future StartChatRun on the same session isn't shadowed by stale
// state.
func TestWaitChatRun_DeadlineExpires(t *testing.T) {
	srv := newRunCompleteServer()
	srv.setWaitChatRunDeadline(50 * time.Millisecond)
	srv.activeRuns["sess-1"] = activeRun{agentName: "claude", agentSessionID: "agent-x"}
	srv.agentSessionByID["agent-x"] = "claude"
	srv.runHookTokens["agent-x"] = "tok"
	srv.runSessionByID["agent-x"] = "sess-1"
	srv.runCompletion["agent-x"] = make(chan completionResult, 1)
	srv.displayTracker.SetRepairing("sess-1", true)

	resp, err := srv.WaitChatRun(t.Context(), &bossanovav1.WaitChatRunHostRequest{AgentSessionId: "agent-x"})
	if err != nil {
		t.Fatalf("WaitChatRun: %v", err)
	}
	if resp.GetExitError() == "" {
		t.Error("ExitError should be non-empty on deadline")
	}

	// All five maps cleaned up.
	srv.runMu.Lock()
	_, hasComp := srv.runCompletion["agent-x"]
	_, hasTok := srv.runHookTokens["agent-x"]
	_, hasSess := srv.runSessionByID["agent-x"]
	_, hasReverse := srv.agentSessionByID["agent-x"]
	_, hasActive := srv.activeRuns["sess-1"]
	srv.runMu.Unlock()
	if hasComp || hasTok || hasSess || hasReverse || hasActive {
		t.Errorf("expected all maps cleared after deadline; comp=%v tok=%v sess=%v reverse=%v active=%v",
			hasComp, hasTok, hasSess, hasReverse, hasActive)
	}

	// IsRepairing flag cleared too — the synthetic exit_error path is
	// otherwise a silent leak from the TUI's perspective.
	if entry := srv.displayTracker.Get("sess-1"); entry != nil && entry.IsRepairing {
		t.Error("IsRepairing should be cleared after WaitChatRun deadline")
	}
}

// TestWaitChatRun_ContextCancelled verifies that a caller-cancelled
// context returns the ctx error AND tears down run state, mirroring the
// deadline cleanup path. Without this, a prematurely-cancelled wait
// would leave runHookTokens populated and shadow a fresh repair attempt.
func TestWaitChatRun_ContextCancelled(t *testing.T) {
	srv := newRunCompleteServer()
	srv.activeRuns["sess-1"] = activeRun{agentName: "claude", agentSessionID: "agent-x"}
	srv.agentSessionByID["agent-x"] = "claude"
	srv.runHookTokens["agent-x"] = "tok"
	srv.runSessionByID["agent-x"] = "sess-1"
	srv.runCompletion["agent-x"] = make(chan completionResult, 1)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := srv.WaitChatRun(ctx, &bossanovav1.WaitChatRunHostRequest{AgentSessionId: "agent-x"})
	if grpcstatus.Code(err) != codes.Canceled {
		t.Fatalf("code = %v, want Canceled", grpcstatus.Code(err))
	}

	// Maps cleared on ctx cancel — the same invariant as the deadline path.
	srv.runMu.Lock()
	_, hasComp := srv.runCompletion["agent-x"]
	_, hasTok := srv.runHookTokens["agent-x"]
	_, hasActive := srv.activeRuns["sess-1"]
	srv.runMu.Unlock()
	if hasComp || hasTok || hasActive {
		t.Errorf("expected maps cleared after ctx cancel; comp=%v tok=%v active=%v", hasComp, hasTok, hasActive)
	}
}
