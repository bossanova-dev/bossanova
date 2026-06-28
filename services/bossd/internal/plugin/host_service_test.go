package plugin

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
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
	"google.golang.org/protobuf/types/known/timestamppb"
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
	sessions                map[string]*models.Session
	updateRepairDiagnostics *db.UpdateRepairDiagnosticsParams
}

func (f *fakeSessionStore) Get(_ context.Context, id string) (*models.Session, error) {
	if s, ok := f.sessions[id]; ok {
		return s, nil
	}
	return nil, errors.New("session not found")
}

func (f *fakeSessionStore) UpdateRepairDiagnostics(_ context.Context, params db.UpdateRepairDiagnosticsParams) error {
	f.updateRepairDiagnostics = &params
	if s, ok := f.sessions[params.SessionID]; ok {
		if params.ReviewFingerprint != nil {
			s.LastRepairReviewFingerprint = *params.ReviewFingerprint
		}
	}
	return nil
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
	running        map[string]bool
	exitErrors     map[string]string
	completed      map[string]bool
	resolveResp    *bossanovav1.ResolveInteractiveSessionIDResponse
	resolveErr     error
	resolveReqs    []*bossanovav1.ResolveInteractiveSessionIDRequest
	removeHookReqs []*bossanovav1.RemoveAgentRunHookRequest
}

var _ agent.AgentRunnerClient = (*fakeAgentClient)(nil)

func newFakeAgentClient() *fakeAgentClient {
	return &fakeAgentClient{
		running:    make(map[string]bool),
		exitErrors: make(map[string]string),
		completed:  make(map[string]bool),
	}
}

func (f *fakeAgentClient) GetInfo(_ context.Context) (*bossanovav1.PluginInfo, error) {
	return &bossanovav1.PluginInfo{Name: "claude"}, nil
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
func (f *fakeAgentClient) ResolveInteractiveSessionID(_ context.Context, req *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolveReqs = append(f.resolveReqs, req)
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	if f.resolveResp != nil {
		return f.resolveResp, nil
	}
	return &bossanovav1.ResolveInteractiveSessionIDResponse{}, nil
}
func (f *fakeAgentClient) ListIgnoredDirtyFiles(_ context.Context, _ *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) {
	return &bossanovav1.ListIgnoredDirtyFilesResponse{}, nil
}
func (f *fakeAgentClient) SuggestPRTitle(context.Context, *bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error) {
	return &bossanovav1.SuggestPRTitleResponse{}, nil
}

func (f *fakeAgentClient) GetChatTitle(_ context.Context, _ *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) {
	return &bossanovav1.GetChatTitleResponse{}, nil
}
func (f *fakeAgentClient) HasQuestionPrompt(_ context.Context, _ *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
	return &bossanovav1.HasQuestionPromptResponse{}, nil
}
func (f *fakeAgentClient) LastTurnIsUser(_ context.Context, _ *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error) {
	return &bossanovav1.LastTurnIsUserResponse{}, nil
}
func (f *fakeAgentClient) TranscriptExists(_ context.Context, _ *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) {
	return &bossanovav1.TranscriptExistsResponse{}, nil
}
func (f *fakeAgentClient) ReadTranscript(_ context.Context, _ *bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error) {
	return &bossanovav1.ReadTranscriptResponse{}, nil
}
func (f *fakeAgentClient) RemoveAgentRunHook(_ context.Context, req *bossanovav1.RemoveAgentRunHookRequest) (*bossanovav1.RemoveAgentRunHookResponse, error) {
	f.mu.Lock()
	f.removeHookReqs = append(f.removeHookReqs, req)
	f.mu.Unlock()
	return &bossanovav1.RemoveAgentRunHookResponse{IsSupported: true}, nil
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
		Prompt:    "fix this bug",
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
	if client.startCall.plan != "fix this bug" {
		t.Errorf("plan = %q, want fix this bug", client.startCall.plan)
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

func TestStartAgentRun_RejectsBossRepairPrompt(t *testing.T) {
	client := newFakeAgentClient()
	client.startResp = "claude-abc"
	srv := newRepairTestServer(client, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})

	_, err := srv.StartAgentRun(t.Context(), &bossanovav1.StartAgentRunHostRequest{
		SessionId: "sess-1",
		Prompt:    "/boss-repair",
	})
	if grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", grpcstatus.Code(err))
	}
	if !strings.Contains(err.Error(), "StartChatRun") {
		t.Fatalf("error = %q, want StartChatRun guidance", err.Error())
	}
	if client.startCall.plan != "" {
		t.Fatalf("StartRun plan = %q, want no headless repair launch", client.startCall.plan)
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
		Prompt:    "fix this bug",
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
	chatsByAgent   map[string]*models.AgentChat
	updatedTmux    []string
}

func (f *fakeAgentChatStore) ListBySession(_ context.Context, sessionID string) ([]*models.AgentChat, error) {
	return f.chatsBySession[sessionID], nil
}

func (f *fakeAgentChatStore) GetByAgentSessionID(_ context.Context, agentSessionID string) (*models.AgentChat, error) {
	return f.chatsByAgent[agentSessionID], nil
}

func (f *fakeAgentChatStore) UpdateProviderSessionID(_ context.Context, agentSessionID string, providerSessionID *string) error {
	if chat := f.chatsByAgent[agentSessionID]; chat != nil {
		chat.ProviderSessionID = providerSessionID
	}
	return nil
}

func TestListSessions_PopulatesPRMetadata(t *testing.T) {
	repoID := "repo-1"
	sessID := "sess-1"
	repoOriginURL := "https://github.com/owner/repo"
	prNumber := 42

	srv := NewHostServiceServer(&mockVCSProvider{})
	srv.repoStore = &fakeRepoStore{
		repos: []*models.Repo{{ID: repoID, DisplayName: "Test Repo", OriginURL: repoOriginURL}},
	}
	srv.sessionStore = &fakeSessionStoreWithListActive{
		sessionsByRepo: map[string][]*models.Session{
			repoID: {{ID: sessID, RepoID: repoID, Title: "my session", PRNumber: &prNumber}},
		},
	}
	srv.displayTracker = status.NewDisplayTracker()
	srv.agentChats = &fakeAgentChatStore{}

	resp, err := srv.ListSessions(t.Context(), &bossanovav1.HostServiceListSessionsRequest{})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	sessions := resp.GetSessions()
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	s := sessions[0]
	if got := s.GetRepoOriginUrl(); got != repoOriginURL {
		t.Fatalf("RepoOriginUrl=%q, want %q", got, repoOriginURL)
	}
	if s.PrNumber == nil {
		t.Fatal("PrNumber=nil, want 42")
	}
	if got := s.GetPrNumber(); got != int32(prNumber) {
		t.Fatalf("PrNumber=%d, want %d", got, prNumber)
	}
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
			repoID: {{ID: sessID, RepoID: repoID, Title: "my session", LastRepairReviewFingerprint: "review-fp-1"}},
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
	if got := s.GetLastRepairReviewFingerprint(); got != "review-fp-1" {
		t.Fatalf("LastRepairReviewFingerprint=%q, want review-fp-1", got)
	}
	gotTime := s.GetLastChatActivityAt().AsTime()
	// 1ms tolerance for any rounding through timestamppb.
	if gotTime.Sub(newerTime).Abs() > time.Millisecond {
		t.Errorf("LastChatActivityAt = %v, want %v (the newer time)", gotTime, newerTime)
	}
}

func TestRecordRepairOutcomePersistsReviewFingerprint(t *testing.T) {
	repoID := "repo-1"
	store := &fakeSessionStore{sessions: map[string]*models.Session{
		"sess-1": {ID: "sess-1", RepoID: repoID},
	}}
	sessionStore := &fakeSessionStoreWithListActive{
		fakeSessionStore: *store,
		sessionsByRepo: map[string][]*models.Session{
			repoID: {store.sessions["sess-1"]},
		},
	}

	srv := NewHostServiceServer(&mockVCSProvider{})
	srv.repoStore = &fakeRepoStore{
		repos: []*models.Repo{{ID: repoID, DisplayName: "Test Repo"}},
	}
	srv.sessionStore = sessionStore
	srv.displayTracker = status.NewDisplayTracker()
	srv.agentChats = &fakeAgentChatStore{}

	reviewFingerprint := "review-fp-2"
	_, err := srv.RecordRepairOutcome(t.Context(), &bossanovav1.RecordRepairOutcomeRequest{
		SessionId:         "sess-1",
		StartedAtUnix:     1779238800,
		HeadSha:           "abc123",
		DisplayStatus:     bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED,
		ReviewFingerprint: &reviewFingerprint,
	})
	if err != nil {
		t.Fatalf("RecordRepairOutcome: %v", err)
	}
	if sessionStore.updateRepairDiagnostics == nil {
		t.Fatal("UpdateRepairDiagnostics was not called")
	}
	if got := sessionStore.updateRepairDiagnostics.ReviewFingerprint; got == nil || *got != "review-fp-2" {
		t.Fatalf("ReviewFingerprint=%v, want review-fp-2", got)
	}

	listResp, err := srv.ListSessions(t.Context(), &bossanovav1.HostServiceListSessionsRequest{})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if got := listResp.GetSessions()[0].GetLastRepairReviewFingerprint(); got != "review-fp-2" {
		t.Fatalf("LastRepairReviewFingerprint=%q, want review-fp-2", got)
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

// TestListSessions_IgnoresFailedStartChatsForActiveCheck verifies that an
// agent_chats row whose StartError is non-empty is excluded from the
// HasActiveChat / LastChatActivityAt computation, even when the chat
// tracker still has a fresh entry for that agent_session_id.
//
// Regression: a pre-fix repair attempt left an orphan tmux pane alive.
// The post-fix code preserves the failed-start agent_chats row (good),
// but the row's AgentSessionID was still being looked up in the chat
// tracker — and the tmux poller kept that entry warm because the pane
// hadn't been killed. Result: HasActiveChat=true forever, repair idle
// gate defers every sweep, session never auto-repairs.
func TestListSessions_IgnoresFailedStartChatsForActiveCheck(t *testing.T) {
	repoID := "repo-1"
	sessID := "sess-1"
	failedAgentSessionID := "chat-failed-start"

	// Fresh tracker entry for the failed chat's agent_session_id — this
	// simulates an orphan tmux pane that the tmux_poller is still
	// stamping heartbeats against.
	tracker := status.NewTracker()
	tracker.Update(failedAgentSessionID, bossanovav1.ChatStatus_CHAT_STATUS_IDLE, time.Now())

	startErr := "send plan failed: ready marker not seen in pane"

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
				{AgentSessionID: failedAgentSessionID, StartError: &startErr},
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
	if s.GetHasActiveChat() {
		t.Error("HasActiveChat should be false when the only chat has StartError set")
	}
	if s.GetLastChatActivityAt() != nil {
		t.Errorf("LastChatActivityAt should be nil for failed-start chats, got %v", s.GetLastChatActivityAt())
	}
}

// TestListSessions_IgnoresStaleRepairChatsForActiveCheck verifies that a
// repair-owned tmux chat with durable stale evidence does not make the parent
// session look user-active. StartChatRun and ReclaimRepairChat own repair-chat
// concurrency; ListSessions' HasActiveChat field is the user-idle gate that
// decides whether repair may start.
func TestListSessions_IgnoresStaleRepairChatsForActiveCheck(t *testing.T) {
	repoID := "repo-1"
	sessID := "sess-1"
	repairAgentSessionID := "chat-repair-stale"
	tmuxName := "boss-repair-stale"

	tracker := status.NewTracker()
	tracker.Update(repairAgentSessionID, bossanovav1.ChatStatus_CHAT_STATUS_IDLE, time.Now())

	agentLogsDir := t.TempDir()
	logPath := filepath.Join(agentLogsDir, repairAgentSessionID+".log")
	if err := os.WriteFile(logPath, []byte("old repair output\n"), 0o600); err != nil {
		t.Fatalf("write repair log: %v", err)
	}
	old := time.Now().Add(-31 * time.Minute)
	if err := os.Chtimes(logPath, old, old); err != nil {
		t.Fatalf("age repair log: %v", err)
	}

	srv := NewHostServiceServer(&mockVCSProvider{})
	srv.agentLogsDir = agentLogsDir
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
				{AgentSessionID: repairAgentSessionID, Title: "Repair: my session", TmuxSessionName: &tmuxName},
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
	if s.GetHasActiveChat() {
		t.Error("HasActiveChat should be false when the only active chat is a repair chat")
	}
	if s.GetLastChatActivityAt() != nil {
		t.Errorf("LastChatActivityAt should be nil for stale repair chats, got %v", s.GetLastChatActivityAt())
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

// TestSignalRunCompleteSignalsChannelWithoutAuth verifies that
// SignalRunComplete (the in-process completion path used by the poll
// fallback for hookless agents) fires the run-completion channel and
// performs the same activeRuns/displayTracker cleanup as CompleteAgentRun
// — without any bearer-token check.
func TestSignalRunCompleteSignalsChannelWithoutAuth(t *testing.T) {
	srv := newRunCompleteServer()
	srv.displayTracker.SetRepairing("session-1", true)

	const agentSessionID = "abc"
	ch := srv.registerRun("session-1", agentSessionID, "ignored-because-internal")
	srv.runMu.Lock()
	srv.activeRuns["session-1"] = activeRun{agentName: "codex", agentSessionID: agentSessionID}
	srv.agentSessionByID[agentSessionID] = "codex"
	srv.runMu.Unlock()

	srv.SignalRunComplete("session-1", agentSessionID, "exit-1")

	select {
	case res := <-ch:
		if res.exitError != "exit-1" {
			t.Errorf("exitError = %q", res.exitError)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("channel never signaled")
	}

	// runCompletion entry cleared (token + reverse-session intentionally
	// survive — WaitChatRun owns their cleanup).
	srv.runMu.Lock()
	_, hasComp := srv.runCompletion[agentSessionID]
	_, hasActive := srv.activeRuns["session-1"]
	_, hasReverse := srv.agentSessionByID[agentSessionID]
	_, hasTok := srv.runHookTokens[agentSessionID]
	_, hasSess := srv.runSessionByID[agentSessionID]
	srv.runMu.Unlock()
	if hasComp {
		t.Error("runCompletion[abc] should be cleared after SignalRunComplete")
	}
	if hasActive {
		t.Error("activeRuns[session-1] should be cleared when its agent_session_id matches")
	}
	if hasReverse {
		t.Error("agentSessionByID[abc] should be cleared on completion")
	}
	if !hasTok {
		t.Error("runHookTokens[abc] should survive — WaitChatRun owns cleanup")
	}
	if !hasSess {
		t.Error("runSessionByID[abc] should survive — WaitChatRun owns cleanup")
	}
	if entry := srv.displayTracker.Get("session-1"); entry != nil && entry.IsRepairing {
		t.Error("IsRepairing should be cleared after SignalRunComplete")
	}
}

// TestSignalRunCompleteIdempotentForUnknownID verifies that signalling an
// already-cleaned-up id is a silent no-op (no panic, no double-signal).
func TestSignalRunCompleteIdempotentForUnknownID(t *testing.T) {
	srv := newRunCompleteServer()
	srv.SignalRunComplete("session-1", "never-registered", "")
	srv.SignalRunComplete("session-1", "never-registered", "again")
}

// TestSignalRunCompleteParksEarlyCompletionForPendingChatRun reproduces the
// race the hookless completion poll can lose: it observes the agent exit and
// signals completion before StartChatRun has registered the run. With the
// session marked pending, the result must be parked and then delivered to the
// channel the subsequent registration installs — not dropped, which would
// strand WaitChatRun until its deadline.
func TestSignalRunCompleteParksEarlyCompletionForPendingChatRun(t *testing.T) {
	srv := newRunCompleteServer()

	const (
		sessionID      = "session-1"
		agentSessionID = "agent-early"
	)

	// StartChatRun marks the session pending before it spawns (and arms the
	// poll). Simulate the poll firing in the gap before registration.
	srv.markPendingChatRun(sessionID)
	srv.SignalRunComplete(sessionID, agentSessionID, "early-exit")

	// Registration lands after the signal; it must drain the parked result.
	ch := srv.registerRunWithCompletionMode(sessionID, agentSessionID, "tok", chatRunCompletionExplicit)

	select {
	case res := <-ch:
		if res.exitError != "early-exit" {
			t.Errorf("exitError = %q, want %q", res.exitError, "early-exit")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("early completion was not delivered to the registered channel")
	}

	srv.runMu.Lock()
	_, stillPending := srv.pendingChatRuns[sessionID]
	_, stillParked := srv.earlyChatCompletions[sessionID]
	srv.runMu.Unlock()
	if stillPending {
		t.Error("pendingChatRuns[session-1] should be cleared after registration")
	}
	if stillParked {
		t.Error("earlyChatCompletions[session-1] should be drained after registration")
	}
}

// TestSignalRunCompleteDropsEarlyCompletionWithoutPendingMarker verifies the
// leak guard: cron / finalize / bootstrap runs never mark the session pending,
// so an early SignalRunComplete for an unregistered run must leave nothing
// behind in earlyChatCompletions.
func TestSignalRunCompleteDropsEarlyCompletionWithoutPendingMarker(t *testing.T) {
	srv := newRunCompleteServer()

	srv.SignalRunComplete("cron-session", "agent-cron", "exit")

	srv.runMu.Lock()
	_, parked := srv.earlyChatCompletions["cron-session"]
	n := len(srv.earlyChatCompletions)
	srv.runMu.Unlock()
	if parked || n != 0 {
		t.Errorf("earlyChatCompletions should stay empty without a pending marker, got %d entries", n)
	}
}

// TestClearPendingChatRunDiscardsUndrainedEarlyCompletion verifies the
// deferred cleanup in StartChatRun: if a spawn fails (or takes the
// AlreadyExists path) after an early completion was parked, clearing the
// pending marker also discards the orphaned result so it can't leak or be
// misattributed to a later run on the same session.
func TestClearPendingChatRunDiscardsUndrainedEarlyCompletion(t *testing.T) {
	srv := newRunCompleteServer()

	srv.markPendingChatRun("session-1")
	srv.SignalRunComplete("session-1", "agent-orphan", "exit")
	srv.clearPendingChatRun("session-1")

	srv.runMu.Lock()
	_, pending := srv.pendingChatRuns["session-1"]
	_, parked := srv.earlyChatCompletions["session-1"]
	srv.runMu.Unlock()
	if pending {
		t.Error("pendingChatRuns[session-1] should be cleared")
	}
	if parked {
		t.Error("earlyChatCompletions[session-1] should be discarded on clear")
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

func (s *HostServiceServer) registerChatRunForTest(agentSessionID string, mode chatRunCompletionMode) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	s.runCompletion[agentSessionID] = activeChatRun{
		done:           make(chan completionResult, 1),
		completionMode: mode,
	}
}

// --- StartChatRun / WaitChatRun (Task 4) ---

// fakeChatLifecycle is a programmable ChatLifecycle for the StartChatRun
// tests. It records the last call (so tests can assert prompt/title/token
// were forwarded) and returns whatever startResp / startErr was scripted.
type fakeChatLifecycle struct {
	mu         sync.Mutex
	startCalls int
	killCalls  int
	lastReq    struct {
		sessionID string
		input     session.ChatInput
		title     string
		hookOpts  session.HookOpts
	}
	lastKill struct {
		sessionID      string
		agentSessionID string
	}
	startResp             string
	startErr              error
	killErr               error
	reclaimRepairChatFunc func(ctx context.Context, sessionID, agentSessionID, reason string) (session.ReclaimRepairChatResult, error)
	reclaimRepairChatReqs []struct {
		sessionID      string
		agentSessionID string
		reason         string
	}
	replaceBlockingChatForRepairFunc func(ctx context.Context, sessionID, agentSessionID, reason string) (session.ReclaimRepairChatResult, error)
	replaceBlockingChatForRepairReqs []struct {
		sessionID      string
		agentSessionID string
		reason         string
	}
	// startFunc, when non-nil, takes precedence over startResp/startErr.
	// Used by the AlreadyExists test to have the lifecycle return the
	// existing agent_session_id alongside a typed gRPC error.
	startFunc func(ctx context.Context, sessionID string, input session.ChatInput, title string, hookOpts session.HookOpts) (string, error)
}

func (f *fakeChatLifecycle) StartTmuxChat(ctx context.Context, sessionID string, input session.ChatInput, title string, hookOpts session.HookOpts) (string, error) {
	f.mu.Lock()
	f.startCalls++
	f.lastReq.sessionID = sessionID
	f.lastReq.input = input
	f.lastReq.title = title
	f.lastReq.hookOpts = hookOpts
	fn := f.startFunc
	resp, err := f.startResp, f.startErr
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, sessionID, input, title, hookOpts)
	}
	return resp, err
}

func (f *fakeChatLifecycle) KillChatTmuxSession(_ context.Context, sessionID, agentSessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killCalls++
	f.lastKill.sessionID = sessionID
	f.lastKill.agentSessionID = agentSessionID
	return f.killErr
}

func (f *fakeChatLifecycle) ReclaimRepairChat(ctx context.Context, sessionID, agentSessionID, reason string) (session.ReclaimRepairChatResult, error) {
	f.mu.Lock()
	f.reclaimRepairChatReqs = append(f.reclaimRepairChatReqs, struct {
		sessionID      string
		agentSessionID string
		reason         string
	}{sessionID: sessionID, agentSessionID: agentSessionID, reason: reason})
	fn := f.reclaimRepairChatFunc
	f.mu.Unlock()

	if fn != nil {
		return fn(ctx, sessionID, agentSessionID, reason)
	}
	return session.ReclaimRepairChatResult{}, nil
}

func (f *fakeChatLifecycle) ReplaceBlockingChatForRepair(ctx context.Context, sessionID, agentSessionID, reason string) (session.ReclaimRepairChatResult, error) {
	f.mu.Lock()
	f.replaceBlockingChatForRepairReqs = append(f.replaceBlockingChatForRepairReqs, struct {
		sessionID      string
		agentSessionID string
		reason         string
	}{sessionID: sessionID, agentSessionID: agentSessionID, reason: reason})
	fn := f.replaceBlockingChatForRepairFunc
	f.mu.Unlock()

	if fn != nil {
		return fn(ctx, sessionID, agentSessionID, reason)
	}
	return session.ReclaimRepairChatResult{}, nil
}

func (*dbWritingChatLifecycle) ReplaceBlockingChatForRepair(context.Context, string, string, string) (session.ReclaimRepairChatResult, error) {
	return session.ReclaimRepairChatResult{}, nil
}

func TestHostServiceReclaimRepairChat_DelegatesToLifecycle(t *testing.T) {
	srv := NewHostServiceServer(&mockVCSProvider{})
	lc := &fakeChatLifecycle{}
	lc.reclaimRepairChatFunc = func(_ context.Context, sessionID, agentSessionID, reason string) (session.ReclaimRepairChatResult, error) {
		if sessionID != "s1" {
			t.Fatalf("sessionID = %q, want s1", sessionID)
		}
		if agentSessionID != "agent-1" {
			t.Fatalf("agentSessionID = %q, want agent-1", agentSessionID)
		}
		if reason != "stale repair chat" {
			t.Fatalf("reason = %q, want stale repair chat", reason)
		}
		return session.ReclaimRepairChatResult{Reclaimed: true, TmuxSessionName: "boss-repair-1"}, nil
	}
	srv.SetLifecycle(lc)

	resp, err := srv.ReclaimRepairChat(t.Context(), &bossanovav1.ReclaimRepairChatHostRequest{
		SessionId:      "s1",
		AgentSessionId: "agent-1",
		Reason:         "stale repair chat",
	})
	if err != nil {
		t.Fatalf("ReclaimRepairChat: %v", err)
	}
	if !resp.GetReclaimed() {
		t.Fatal("expected Reclaimed=true")
	}
	if resp.GetTmuxSessionName() != "boss-repair-1" {
		t.Fatalf("TmuxSessionName = %q, want boss-repair-1", resp.GetTmuxSessionName())
	}
}

func TestHostServiceReclaimRepairChat_RefusesActiveRunState(t *testing.T) {
	lc := &fakeChatLifecycle{}
	lc.reclaimRepairChatFunc = func(context.Context, string, string, string) (session.ReclaimRepairChatResult, error) {
		t.Fatal("lifecycle reclaim called for active daemon-owned run")
		return session.ReclaimRepairChatResult{}, nil
	}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", AgentName: "claude", WorktreePath: "/tmp/wt"})
	client := srv.agentClients["claude"].(*fakeAgentClient)
	client.running["agent-active-1"] = true

	srv.runMu.Lock()
	srv.activeRuns["sess-1"] = activeRun{agentName: "claude", agentSessionID: "agent-active-1"}
	srv.agentSessionByID["agent-active-1"] = "claude"
	srv.runMu.Unlock()

	_, err := srv.ReclaimRepairChat(t.Context(), &bossanovav1.ReclaimRepairChatHostRequest{
		SessionId:      "sess-1",
		AgentSessionId: "agent-active-1",
		Reason:         "must not kill active repair",
	})
	if err == nil {
		t.Fatal("expected active-run reclaim refusal")
	}
	if got := grpcstatus.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("code = %s, want FailedPrecondition", got)
	}
	if len(lc.reclaimRepairChatReqs) != 0 {
		t.Fatalf("lifecycle reclaim calls = %d, want 0", len(lc.reclaimRepairChatReqs))
	}
}

func TestHostServiceReclaimRepairChat_RefusesDaemonOwnedInteractiveRun(t *testing.T) {
	lc := &fakeChatLifecycle{}
	lc.reclaimRepairChatFunc = func(context.Context, string, string, string) (session.ReclaimRepairChatResult, error) {
		t.Fatal("lifecycle reclaim called for daemon-owned interactive run")
		return session.ReclaimRepairChatResult{}, nil
	}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", AgentName: "claude", WorktreePath: "/tmp/wt"})

	srv.runMu.Lock()
	srv.activeRuns["sess-1"] = activeRun{agentName: "claude", agentSessionID: "agent-interactive-1"}
	srv.agentSessionByID["agent-interactive-1"] = "claude"
	srv.runMu.Unlock()

	_, err := srv.ReclaimRepairChat(t.Context(), &bossanovav1.ReclaimRepairChatHostRequest{
		SessionId:      "sess-1",
		AgentSessionId: "agent-interactive-1",
		Reason:         "must not reclaim owned interactive repair",
	})
	if err == nil {
		t.Fatal("expected daemon-owned interactive reclaim refusal")
	}
	if got := grpcstatus.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("code = %s, want FailedPrecondition", got)
	}
	if len(lc.reclaimRepairChatReqs) != 0 {
		t.Fatalf("lifecycle reclaim calls = %d, want 0", len(lc.reclaimRepairChatReqs))
	}
}

func TestHostServiceReclaimRepairChat_RefusesNonRepairChat(t *testing.T) {
	srv := NewHostServiceServer(&mockVCSProvider{})
	lc := &fakeChatLifecycle{}
	lc.reclaimRepairChatFunc = func(context.Context, string, string, string) (session.ReclaimRepairChatResult, error) {
		return session.ReclaimRepairChatResult{}, session.ErrRepairChatNotReclaimable
	}
	srv.SetLifecycle(lc)

	_, err := srv.ReclaimRepairChat(t.Context(), &bossanovav1.ReclaimRepairChatHostRequest{
		SessionId:      "s1",
		AgentSessionId: "agent-1",
		Reason:         "manual chat",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := grpcstatus.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("code = %s, want FailedPrecondition", got)
	}
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
	srv.chatTracker = status.NewTracker()
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
	if lc.lastReq.sessionID != "sess-1" || lc.lastReq.input.Prompt != "/boss-repair" || lc.lastReq.title != "Repair: sess-1" {
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

func TestStartChatRun_CommandPath(t *testing.T) {
	lc := &fakeChatLifecycle{startResp: "agent-abc"}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})

	_, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId: "sess-1",
		Command:   "boss-repair",
		Title:     "Repair: sess-1",
	})
	if err != nil {
		t.Fatalf("StartChatRun: %v", err)
	}

	if lc.lastReq.input.Command != "boss-repair" || lc.lastReq.input.Prompt != "" {
		t.Fatalf("StartTmuxChat input = %+v, want command boss-repair", lc.lastReq.input)
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

func TestStartChatRun_PromptAndCommand(t *testing.T) {
	lc := &fakeChatLifecycle{startResp: "agent-x"}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})

	_, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId: "sess-1",
		Prompt:    "please inspect this branch",
		Command:   "boss-repair",
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

func TestHostServiceStartChatRun_ReplacesActiveRunGuardForRepair(t *testing.T) {
	const reason = "auto-repair replacing idle chat after 6m0s of silence; threshold 5m0s"

	lc := &fakeChatLifecycle{}
	lc.startFunc = func(_ context.Context, _ string, _ session.ChatInput, _ string, _ session.HookOpts) (string, error) {
		if lc.startCalls == 1 {
			return "agent-blocking", nil
		}
		return "agent-repair", nil
	}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})

	if _, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId: "sess-1", Command: "/boss-repair", Title: "Repair: old",
	}); err != nil {
		t.Fatalf("first StartChatRun: %v", err)
	}
	srv.runMu.Lock()
	oldDone := srv.runCompletion["agent-blocking"].done
	srv.runMu.Unlock()
	srv.agentClients["claude"].(*fakeAgentClient).mu.Lock()
	srv.agentClients["claude"].(*fakeAgentClient).running["agent-blocking"] = true
	srv.agentClients["claude"].(*fakeAgentClient).mu.Unlock()
	observed := time.Now().Add(-6 * time.Minute)
	srv.chatTracker.Update("agent-blocking", bossanovav1.ChatStatus_CHAT_STATUS_IDLE, observed)

	resp, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId:             "sess-1",
		Command:               "/boss-repair",
		Title:                 "Repair: Improve conflict detection",
		ReplaceExistingChat:   true,
		ReplaceExistingReason: reason,
		ReplaceExistingObservedLastChatActivityAt: timestamppb.New(observed),
	})
	if err != nil {
		t.Fatalf("StartChatRun: %v", err)
	}
	if resp.GetAgentSessionId() != "agent-repair" {
		t.Fatalf("AgentSessionId = %q, want agent-repair", resp.GetAgentSessionId())
	}
	if lc.startCalls != 2 {
		t.Fatalf("StartTmuxChat calls = %d, want 2", lc.startCalls)
	}
	if len(lc.replaceBlockingChatForRepairReqs) != 1 {
		t.Fatalf("ReplaceBlockingChatForRepair calls = %d, want 1", len(lc.replaceBlockingChatForRepairReqs))
	}
	got := lc.replaceBlockingChatForRepairReqs[0]
	if got.sessionID != "sess-1" || got.agentSessionID != "agent-blocking" || got.reason != reason {
		t.Fatalf("ReplaceBlockingChatForRepair req = %+v, want sess-1 / agent-blocking / %q", got, reason)
	}

	srv.runMu.Lock()
	active := srv.activeRuns["sess-1"]
	_, oldCompletion := srv.runCompletion["agent-blocking"]
	_, oldToken := srv.runHookTokens["agent-blocking"]
	_, oldSession := srv.runSessionByID["agent-blocking"]
	srv.runMu.Unlock()
	if active.agentSessionID != "agent-repair" {
		t.Fatalf("activeRuns[sess-1] = %+v, want agent-repair", active)
	}
	if oldCompletion || oldToken || oldSession {
		t.Fatalf("old run state still present: completion=%v token=%v session=%v", oldCompletion, oldToken, oldSession)
	}
	select {
	case res := <-oldDone:
		if res.exitError != "chat run replaced by newer repair attempt" {
			t.Fatalf("old run exitError = %q, want replacement notice", res.exitError)
		}
	default:
		t.Fatal("old run completion channel was not signaled before cleanup")
	}
}

func TestHostServiceStartChatRun_RejectsNonRepairReplaceExistingChat(t *testing.T) {
	const reason = "auto-repair replacing idle chat after 6m0s of silence; threshold 5m0s"

	lc := &fakeChatLifecycle{}
	lc.startFunc = func(_ context.Context, _ string, _ session.ChatInput, _ string, _ session.HookOpts) (string, error) {
		if lc.startCalls == 1 {
			return "agent-blocking", nil
		}
		return "agent-repair", nil
	}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})

	if _, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId: "sess-1", Command: "/boss-repair", Title: "Repair: old",
	}); err != nil {
		t.Fatalf("first StartChatRun: %v", err)
	}
	srv.agentClients["claude"].(*fakeAgentClient).mu.Lock()
	srv.agentClients["claude"].(*fakeAgentClient).running["agent-blocking"] = true
	srv.agentClients["claude"].(*fakeAgentClient).mu.Unlock()

	_, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId:             "sess-1",
		Command:               "boss-run",
		Title:                 "Run: Improve conflict detection",
		ReplaceExistingChat:   true,
		ReplaceExistingReason: reason,
	})
	if grpcstatus.Code(err) != codes.InvalidArgument {
		t.Fatalf("StartChatRun code = %v, want InvalidArgument (err=%v)", grpcstatus.Code(err), err)
	}
	if lc.startCalls != 1 {
		t.Fatalf("StartTmuxChat calls = %d, want 1", lc.startCalls)
	}
	if len(lc.replaceBlockingChatForRepairReqs) != 0 {
		t.Fatalf("ReplaceBlockingChatForRepair calls = %d, want 0", len(lc.replaceBlockingChatForRepairReqs))
	}
}

func TestHostServiceStartChatRun_ActiveRunReplacementDoesNotReplaceAgainOnLifecycleAlreadyExists(t *testing.T) {
	const reason = "auto-repair replacing idle chat after 6m0s of silence; threshold 5m0s"

	lc := &fakeChatLifecycle{}
	lc.startFunc = func(_ context.Context, _ string, _ session.ChatInput, _ string, _ session.HookOpts) (string, error) {
		if lc.startCalls == 1 {
			return "agent-blocking", nil
		}
		return "agent-lifecycle", grpcstatus.Error(codes.AlreadyExists, "tmux chat already active")
	}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})

	if _, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId: "sess-1", Command: "/boss-repair", Title: "Repair: old",
	}); err != nil {
		t.Fatalf("first StartChatRun: %v", err)
	}
	srv.agentClients["claude"].(*fakeAgentClient).mu.Lock()
	srv.agentClients["claude"].(*fakeAgentClient).running["agent-blocking"] = true
	srv.agentClients["claude"].(*fakeAgentClient).mu.Unlock()
	observed := time.Now().Add(-6 * time.Minute)
	srv.chatTracker.Update("agent-blocking", bossanovav1.ChatStatus_CHAT_STATUS_IDLE, observed)

	resp, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId:             "sess-1",
		Command:               "/boss-repair",
		Title:                 "Repair: Improve conflict detection",
		ReplaceExistingChat:   true,
		ReplaceExistingReason: reason,
		ReplaceExistingObservedLastChatActivityAt: timestamppb.New(observed),
	})
	if grpcstatus.Code(err) != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists", grpcstatus.Code(err))
	}
	if resp == nil || resp.GetAgentSessionId() != "agent-lifecycle" {
		t.Fatalf("AgentSessionId = %v, want agent-lifecycle echoed alongside AlreadyExists", resp)
	}
	if len(lc.replaceBlockingChatForRepairReqs) != 1 {
		t.Fatalf("ReplaceBlockingChatForRepair calls = %d, want 1", len(lc.replaceBlockingChatForRepairReqs))
	}
	if lc.replaceBlockingChatForRepairReqs[0].agentSessionID != "agent-blocking" {
		t.Fatalf("ReplaceBlockingChatForRepair req = %+v, want agent-blocking only", lc.replaceBlockingChatForRepairReqs[0])
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
		startFunc: func(_ context.Context, _ string, _ session.ChatInput, _ string, _ session.HookOpts) (string, error) {
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

func TestHostServiceStartChatRun_ReplacesBlockingChatForRepair(t *testing.T) {
	const reason = "auto-repair replacing idle chat after 6m0s of silence; threshold 5m0s"

	lc := &fakeChatLifecycle{}
	lc.startFunc = func(_ context.Context, _ string, _ session.ChatInput, _ string, _ session.HookOpts) (string, error) {
		if lc.startCalls == 1 {
			return "agent-finalize", grpcstatus.Error(codes.AlreadyExists, "tmux chat already active")
		}
		return "agent-repair", nil
	}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})
	observed := time.Now().Add(-6 * time.Minute)
	srv.chatTracker.Update("agent-finalize", bossanovav1.ChatStatus_CHAT_STATUS_IDLE, observed)

	resp, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId:             "sess-1",
		Command:               "/boss-repair",
		Title:                 "Repair: Improve conflict detection",
		ReplaceExistingChat:   true,
		ReplaceExistingReason: reason,
		ReplaceExistingObservedLastChatActivityAt: timestamppb.New(observed),
	})
	if err != nil {
		t.Fatalf("StartChatRun: %v", err)
	}
	if resp.GetAgentSessionId() != "agent-repair" {
		t.Fatalf("AgentSessionId = %q, want agent-repair", resp.GetAgentSessionId())
	}
	if lc.startCalls != 2 {
		t.Fatalf("StartTmuxChat calls = %d, want 2", lc.startCalls)
	}
	if len(lc.replaceBlockingChatForRepairReqs) != 1 {
		t.Fatalf("ReplaceBlockingChatForRepair calls = %d, want 1", len(lc.replaceBlockingChatForRepairReqs))
	}
	got := lc.replaceBlockingChatForRepairReqs[0]
	if got.sessionID != "sess-1" || got.agentSessionID != "agent-finalize" || got.reason != reason {
		t.Fatalf("ReplaceBlockingChatForRepair req = %+v, want sess-1 / agent-finalize / %q", got, reason)
	}
	if len(lc.reclaimRepairChatReqs) != 0 {
		t.Fatalf("ReclaimRepairChat calls = %d, want 0", len(lc.reclaimRepairChatReqs))
	}
}

func TestHostServiceStartChatRun_RefusesReplacementAfterNewChatActivity(t *testing.T) {
	const reason = "auto-repair replacing idle chat after 6m0s of silence; threshold 5m0s"

	lc := &fakeChatLifecycle{
		startFunc: func(_ context.Context, _ string, _ session.ChatInput, _ string, _ session.HookOpts) (string, error) {
			return "agent-finalize", grpcstatus.Error(codes.AlreadyExists, "tmux chat already active")
		},
	}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})
	observed := time.Now().Add(-6 * time.Minute)
	srv.chatTracker.Update("agent-finalize", bossanovav1.ChatStatus_CHAT_STATUS_IDLE, observed.Add(time.Minute))

	_, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId:             "sess-1",
		Command:               "/boss-repair",
		Title:                 "Repair: Improve conflict detection",
		ReplaceExistingChat:   true,
		ReplaceExistingReason: reason,
		ReplaceExistingObservedLastChatActivityAt: timestamppb.New(observed),
	})
	if grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", grpcstatus.Code(err), err)
	}
	if len(lc.replaceBlockingChatForRepairReqs) != 0 {
		t.Fatalf("ReplaceBlockingChatForRepair calls = %d, want 0", len(lc.replaceBlockingChatForRepairReqs))
	}
}

func TestHostServiceStartChatRun_RefusesReplacementWhenChatNoLongerIdle(t *testing.T) {
	const reason = "auto-repair replacing idle chat after 6m0s of silence; threshold 5m0s"

	lc := &fakeChatLifecycle{
		startFunc: func(_ context.Context, _ string, _ session.ChatInput, _ string, _ session.HookOpts) (string, error) {
			return "agent-finalize", grpcstatus.Error(codes.AlreadyExists, "tmux chat already active")
		},
	}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})
	observed := time.Now().Add(-6 * time.Minute)
	srv.chatTracker.Update("agent-finalize", bossanovav1.ChatStatus_CHAT_STATUS_WORKING, observed)

	_, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId:             "sess-1",
		Command:               "/boss-repair",
		Title:                 "Repair: Improve conflict detection",
		ReplaceExistingChat:   true,
		ReplaceExistingReason: reason,
		ReplaceExistingObservedLastChatActivityAt: timestamppb.New(observed),
	})
	if grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", grpcstatus.Code(err), err)
	}
	if len(lc.replaceBlockingChatForRepairReqs) != 0 {
		t.Fatalf("ReplaceBlockingChatForRepair calls = %d, want 0", len(lc.replaceBlockingChatForRepairReqs))
	}
}

func TestHostServiceStartChatRun_ReplacesBlockingChatForRepairFromErrorMessageFallback(t *testing.T) {
	const reason = "auto-repair replacing idle chat after 6m0s of silence; threshold 5m0s"

	lc := &fakeChatLifecycle{}
	lc.startFunc = func(_ context.Context, _ string, _ session.ChatInput, _ string, _ session.HookOpts) (string, error) {
		if lc.startCalls == 1 {
			return "", grpcstatus.Error(codes.AlreadyExists, "tmux chat already active (agent_session_id=agent-finalize)")
		}
		return "agent-repair", nil
	}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})
	observed := time.Now().Add(-6 * time.Minute)
	srv.chatTracker.Update("agent-finalize", bossanovav1.ChatStatus_CHAT_STATUS_IDLE, observed)

	resp, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId:             "sess-1",
		Command:               "/boss-repair",
		Title:                 "Repair: Improve conflict detection",
		ReplaceExistingChat:   true,
		ReplaceExistingReason: reason,
		ReplaceExistingObservedLastChatActivityAt: timestamppb.New(observed),
	})
	if err != nil {
		t.Fatalf("StartChatRun: %v", err)
	}
	if resp.GetAgentSessionId() != "agent-repair" {
		t.Fatalf("AgentSessionId = %q, want agent-repair", resp.GetAgentSessionId())
	}
	if lc.startCalls != 2 {
		t.Fatalf("StartTmuxChat calls = %d, want 2", lc.startCalls)
	}
	if len(lc.replaceBlockingChatForRepairReqs) != 1 {
		t.Fatalf("ReplaceBlockingChatForRepair calls = %d, want 1", len(lc.replaceBlockingChatForRepairReqs))
	}
	got := lc.replaceBlockingChatForRepairReqs[0]
	if got.sessionID != "sess-1" || got.agentSessionID != "agent-finalize" || got.reason != reason {
		t.Fatalf("ReplaceBlockingChatForRepair req = %+v, want sess-1 / agent-finalize / %q", got, reason)
	}
}

func TestHostServiceStartChatRun_ReplaceExistingWithoutAgentSessionIDDoesNotReplace(t *testing.T) {
	lc := &fakeChatLifecycle{
		startFunc: func(_ context.Context, _ string, _ session.ChatInput, _ string, _ session.HookOpts) (string, error) {
			return "", grpcstatus.Error(codes.AlreadyExists, "tmux chat already active")
		},
	}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})

	resp, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId:             "sess-1",
		Command:               "/boss-repair",
		Title:                 "Repair: Improve conflict detection",
		ReplaceExistingChat:   true,
		ReplaceExistingReason: "replace stale repair chat",
		ReplaceExistingObservedLastChatActivityAt: timestamppb.New(time.Now().Add(-6 * time.Minute)),
	})
	if grpcstatus.Code(err) != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists", grpcstatus.Code(err))
	}
	if resp != nil {
		t.Fatalf("response = %v, want nil", resp)
	}
	if lc.startCalls != 1 {
		t.Fatalf("StartTmuxChat calls = %d, want 1", lc.startCalls)
	}
	if len(lc.replaceBlockingChatForRepairReqs) != 0 {
		t.Fatalf("ReplaceBlockingChatForRepair calls = %d, want 0", len(lc.replaceBlockingChatForRepairReqs))
	}
}

func TestStartChatRun_RepairLifecycleAlreadyExistsDoesNotKillOrRetry(t *testing.T) {
	const existing = "agent-existing-repair-1234"

	lc := &fakeChatLifecycle{
		startFunc: func(_ context.Context, _ string, _ session.ChatInput, _ string, _ session.HookOpts) (string, error) {
			return existing, grpcstatus.Error(codes.AlreadyExists, "tmux chat already active")
		},
	}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})

	resp, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId: "sess-1",
		Prompt:    "/boss-repair",
		Title:     "$boss-repair",
	})
	if grpcstatus.Code(err) != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists", grpcstatus.Code(err))
	}
	if resp == nil || resp.GetAgentSessionId() != existing {
		t.Fatalf("AgentSessionId = %v, want %q echoed alongside AlreadyExists", resp, existing)
	}
	if lc.startCalls != 1 {
		t.Fatalf("StartTmuxChat calls = %d, want 1", lc.startCalls)
	}
	if lc.killCalls != 0 {
		t.Fatalf("KillChatTmuxSession calls = %d, want 0", lc.killCalls)
	}
	if len(lc.replaceBlockingChatForRepairReqs) != 0 {
		t.Fatalf("ReplaceBlockingChatForRepair calls = %d, want 0", len(lc.replaceBlockingChatForRepairReqs))
	}
}

func TestStartChatRun_RepairLifecycleAlreadyExistsAcrossRegistrationGap(t *testing.T) {
	const existing = "agent-starting-repair-1234"

	lc := &fakeChatLifecycle{
		startFunc: func(_ context.Context, _ string, _ session.ChatInput, _ string, _ session.HookOpts) (string, error) {
			return existing, grpcstatus.Error(codes.AlreadyExists, "tmux chat already active")
		},
	}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})

	resp, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId: "sess-1",
		Prompt:    "/boss-repair",
		Title:     "$boss-repair",
	})
	if grpcstatus.Code(err) != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists", grpcstatus.Code(err))
	}
	if resp == nil || resp.GetAgentSessionId() != existing {
		t.Fatalf("AgentSessionId = %v, want %q echoed alongside AlreadyExists", resp, existing)
	}
	if lc.startCalls != 1 {
		t.Fatalf("StartTmuxChat calls = %d, want 1", lc.startCalls)
	}
	if lc.killCalls != 0 {
		t.Fatalf("KillChatTmuxSession calls = %d, want 0", lc.killCalls)
	}
}

func TestStartChatRun_NonRepairLifecycleAlreadyExistsDoesNotClearOrRetry(t *testing.T) {
	const existing = "agent-existing-manual-1234"

	lc := &fakeChatLifecycle{
		startFunc: func(_ context.Context, _ string, _ session.ChatInput, _ string, _ session.HookOpts) (string, error) {
			return existing, grpcstatus.Error(codes.AlreadyExists, "tmux chat already active")
		},
	}
	chatStore := &fakeAgentChatStore{}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})
	srv.agentChats = chatStore

	resp, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId: "sess-1",
		Prompt:    "please inspect this branch",
		Title:     "Manual inspection",
	})
	if grpcstatus.Code(err) != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists", grpcstatus.Code(err))
	}
	if resp == nil || resp.GetAgentSessionId() != existing {
		t.Fatalf("AgentSessionId = %v, want %q echoed alongside AlreadyExists", resp, existing)
	}
	if lc.startCalls != 1 {
		t.Fatalf("StartTmuxChat calls = %d, want 1", lc.startCalls)
	}
	if lc.killCalls != 0 {
		t.Fatalf("KillChatTmuxSession calls = %d, want 0", lc.killCalls)
	}
	if len(chatStore.updatedTmux) != 0 {
		t.Fatalf("tmux name updates = %d, want 0", len(chatStore.updatedTmux))
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
		startFunc: func(_ context.Context, _ string, _ session.ChatInput, _ string, _ session.HookOpts) (string, error) {
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
	srv.registerRun("sess-1", "agent-x", "tok")

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

func TestWaitChatRun_ReturnsProviderSessionID(t *testing.T) {
	srv := newRunCompleteServer()
	providerSessionID := "codex-provider-123"
	srv.agentChats = &fakeAgentChatStore{
		chatsByAgent: map[string]*models.AgentChat{
			"agent-x": {AgentSessionID: "agent-x", ProviderSessionID: &providerSessionID},
		},
	}
	srv.registerRun("sess-1", "agent-x", "tok")

	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = srv.CompleteAgentRun(context.Background(), "agent-x", "tok", "")
	}()

	resp, err := srv.WaitChatRun(t.Context(), &bossanovav1.WaitChatRunHostRequest{AgentSessionId: "agent-x"})
	if err != nil {
		t.Fatalf("WaitChatRun: %v", err)
	}
	if resp.GetProviderSessionId() != providerSessionID {
		t.Fatalf("ProviderSessionId = %q, want %q", resp.GetProviderSessionId(), providerSessionID)
	}
}

func TestWaitChatRun_DiscoversCodexProviderSessionIDBeforeResponse(t *testing.T) {
	srv := newRunCompleteServer()
	client := newFakeAgentClient()
	client.resolveResp = &bossanovav1.ResolveInteractiveSessionIDResponse{
		Found:     true,
		SessionId: "codex-provider-456",
	}
	srv.agentClients = map[string]agent.AgentRunnerClient{"codex": client}
	srv.sessionStore = &fakeSessionStore{
		sessions: map[string]*models.Session{
			"sess-1": {ID: "sess-1", WorktreePath: "/tmp/worktree"},
		},
	}
	chat := &models.AgentChat{
		SessionID:      "sess-1",
		AgentSessionID: "agent-x",
		AgentName:      "codex",
		CreatedAt:      time.Now().Add(-time.Minute),
	}
	srv.agentChats = &fakeAgentChatStore{
		chatsByAgent: map[string]*models.AgentChat{"agent-x": chat},
	}
	srv.registerRun("sess-1", "agent-x", "tok")

	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = srv.CompleteAgentRun(context.Background(), "agent-x", "tok", "")
	}()

	resp, err := srv.WaitChatRun(t.Context(), &bossanovav1.WaitChatRunHostRequest{AgentSessionId: "agent-x"})
	if err != nil {
		t.Fatalf("WaitChatRun: %v", err)
	}
	if resp.GetProviderSessionId() != "codex-provider-456" {
		t.Fatalf("ProviderSessionId = %q, want codex-provider-456", resp.GetProviderSessionId())
	}
	if chat.ProviderSessionID == nil || *chat.ProviderSessionID != "codex-provider-456" {
		t.Fatalf("persisted provider session id = %v, want codex-provider-456", chat.ProviderSessionID)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.resolveReqs) != 1 {
		t.Fatalf("ResolveInteractiveSessionID calls = %d, want 1", len(client.resolveReqs))
	}
	req := client.resolveReqs[0]
	if req.GetWorkDir() != "/tmp/worktree" {
		t.Fatalf("resolver WorkDir = %q, want /tmp/worktree", req.GetWorkDir())
	}
	if req.GetRequestedSessionId() != "agent-x" {
		t.Fatalf("resolver RequestedSessionId = %q, want agent-x", req.GetRequestedSessionId())
	}
	if !req.GetAllowLegacyBackfill() {
		t.Fatal("resolver AllowLegacyBackfill = false, want true")
	}
	if req.GetChatCreatedAt() == nil {
		t.Fatal("resolver ChatCreatedAt is nil")
	}
}

func TestWaitChatRun_PropagatesExitError(t *testing.T) {
	srv := newRunCompleteServer()
	srv.registerRun("sess-1", "agent-x", "tok")

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

func TestWaitChatRun_HooklessInteractiveRunReturnsExplicitTimeout(t *testing.T) {
	t.Parallel()

	srv := newRunCompleteServer()
	agentSessionID := "codex-chat-1"
	srv.registerChatRunForTest(agentSessionID, chatRunCompletionExplicit)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	resp, err := srv.WaitChatRun(ctx, &bossanovav1.WaitChatRunHostRequest{
		AgentSessionId: agentSessionID,
	})
	if err != nil {
		t.Fatalf("WaitChatRun returned RPC error: %v", err)
	}
	if resp.GetExitError() == "" {
		t.Fatal("expected explicit exit error for hookless interactive run")
	}
	if !strings.Contains(resp.GetExitError(), "interactive chat did not report completion") {
		t.Fatalf("unexpected exit error: %q", resp.GetExitError())
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
	srv.registerRun("sess-1", "agent-x", "tok")
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
	srv.registerRun("sess-1", "agent-x", "tok")

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

// TestWaitChatRun_RemovesRunHookOnCompletion verifies that WaitChatRun fires
// RemoveAgentRunHook even when completion goes through the real CompleteAgentRun
// path, which deletes agentSessionByID before WaitChatRun's done-branch runs.
// The fix resolves the agent client via sess.AgentName (from the session store)
// using runSessionByID, which intentionally survives as a tombstone.
func TestWaitChatRun_RemovesRunHookOnCompletion(t *testing.T) {
	fake := newFakeAgentClient()
	srv := newRepairTestServer(fake, &models.Session{
		ID:           "sess-1",
		AgentName:    "claude",
		WorktreePath: "/tmp/wt-1",
	})

	srv.registerRun("sess-1", "run-1", "tok-1")
	// Mirror what StartChatRun does: record the active run so CompleteAgentRun's
	// matching-session branch (host_service.go:1211) fires and deletes
	// agentSessionByID["run-1"] — the exact condition that exposed the bug.
	srv.runMu.Lock()
	srv.activeRuns["sess-1"] = activeRun{agentName: "claude", agentSessionID: "run-1"}
	srv.agentSessionByID["run-1"] = "claude"
	srv.runMu.Unlock()

	// Drive completion through the real path concurrently: WaitChatRun must
	// start first (grabbing run from runCompletion), then CompleteAgentRun
	// signals the buffered channel and deletes agentSessionByID. The goroutine
	// sleep gives WaitChatRun time to enter its select before the signal fires.
	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = srv.CompleteAgentRun(context.Background(), "run-1", "tok-1", "")
	}()

	if _, err := srv.WaitChatRun(t.Context(), &bossanovav1.WaitChatRunHostRequest{AgentSessionId: "run-1"}); err != nil {
		t.Fatalf("WaitChatRun: %v", err)
	}

	// Verify agentSessionByID was deleted by CompleteAgentRun, proving the fix
	// exercised the real bug condition (old helper would have returned early here).
	srv.runMu.Lock()
	_, stillIndexed := srv.agentSessionByID["run-1"]
	srv.runMu.Unlock()
	if stillIndexed {
		t.Fatal("agentSessionByID[run-1] was not deleted by CompleteAgentRun; bug condition not triggered")
	}

	fake.mu.Lock()
	reqs := fake.removeHookReqs
	fake.mu.Unlock()
	if len(reqs) != 1 {
		t.Fatalf("RemoveAgentRunHook called %d times, want 1", len(reqs))
	}
	if got := reqs[0]; got.WorkDir != "/tmp/wt-1" || got.AgentSessionId != "run-1" {
		t.Errorf("remove req = %+v, want {WorkDir:/tmp/wt-1 AgentSessionId:run-1}", got)
	}
}
