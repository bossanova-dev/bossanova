package plugin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"google.golang.org/grpc/codes"
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
