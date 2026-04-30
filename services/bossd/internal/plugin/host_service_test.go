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
	"github.com/recurser/bossd/internal/claude"
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

// fakeClaudeRunner records Start calls and lets tests script IsRunning /
// ExitError responses per claudeID. Mirrors claude.ClaudeRunner just enough
// for the host RPC tests; embedding the interface gives nil-method behavior
// for everything we don't override.
type fakeClaudeRunner struct {
	mu        sync.Mutex
	startResp string
	startErr  error
	startCall struct {
		workDir string
		plan    string
	}
	running    map[string]bool
	exitErrors map[string]error
}

var _ claude.ClaudeRunner = (*fakeClaudeRunner)(nil)

func newFakeClaudeRunner() *fakeClaudeRunner {
	return &fakeClaudeRunner{
		running:    make(map[string]bool),
		exitErrors: make(map[string]error),
	}
}

func (f *fakeClaudeRunner) Start(_ context.Context, workDir, plan string, _ *string, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCall.workDir = workDir
	f.startCall.plan = plan
	if f.startErr != nil {
		return "", f.startErr
	}
	f.running[f.startResp] = true
	return f.startResp, nil
}

func (f *fakeClaudeRunner) Stop(_ string) error { return nil }
func (f *fakeClaudeRunner) Subscribe(_ context.Context, _ string) (<-chan claude.OutputLine, error) {
	return nil, nil
}
func (f *fakeClaudeRunner) History(_ string) []claude.OutputLine { return nil }

func (f *fakeClaudeRunner) IsRunning(claudeID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running[claudeID]
}

func (f *fakeClaudeRunner) ExitError(claudeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exitErrors[claudeID]
}

func (f *fakeClaudeRunner) finish(claudeID string, exitErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running[claudeID] = false
	f.exitErrors[claudeID] = exitErr
}

func newRepairTestServer(runner claude.ClaudeRunner, sessions ...*models.Session) *HostServiceServer {
	srv := NewHostServiceServer(&mockVCSProvider{})
	store := &fakeSessionStore{sessions: make(map[string]*models.Session)}
	for _, s := range sessions {
		store.sessions[s.ID] = s
	}
	srv.sessionStore = store
	srv.claudeRunner = runner
	return srv
}

func TestStartClaudeRun_HappyPath(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.startResp = "claude-abc"
	srv := newRepairTestServer(runner, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})

	resp, err := srv.StartClaudeRun(t.Context(), &bossanovav1.StartClaudeRunRequest{
		SessionId: "sess-1",
		Prompt:    "/boss-repair",
	})
	if err != nil {
		t.Fatalf("StartClaudeRun: %v", err)
	}
	if resp.GetClaudeId() != "claude-abc" {
		t.Fatalf("ClaudeId = %q, want %q", resp.GetClaudeId(), "claude-abc")
	}
	if runner.startCall.workDir != "/tmp/wt" {
		t.Errorf("workDir = %q, want /tmp/wt", runner.startCall.workDir)
	}
	if runner.startCall.plan != "/boss-repair" {
		t.Errorf("plan = %q, want /boss-repair", runner.startCall.plan)
	}
	if got := srv.activeRuns["sess-1"]; got != "claude-abc" {
		t.Errorf("activeRuns[sess-1] = %q, want claude-abc", got)
	}
}

func TestStartClaudeRun_NoRunner(t *testing.T) {
	srv := NewHostServiceServer(&mockVCSProvider{})
	_, err := srv.StartClaudeRun(t.Context(), &bossanovav1.StartClaudeRunRequest{SessionId: "x", Prompt: "p"})
	if grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", grpcstatus.Code(err))
	}
}

func TestStartClaudeRun_SessionNotFound(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.startResp = "claude-xyz"
	srv := newRepairTestServer(runner) // no sessions registered

	_, err := srv.StartClaudeRun(t.Context(), &bossanovav1.StartClaudeRunRequest{
		SessionId: "missing",
		Prompt:    "/boss-repair",
	})
	if grpcstatus.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", grpcstatus.Code(err))
	}
}

func TestStartClaudeRun_AlreadyActive(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.startResp = "claude-1"
	srv := newRepairTestServer(runner, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})

	if _, err := srv.StartClaudeRun(t.Context(), &bossanovav1.StartClaudeRunRequest{
		SessionId: "sess-1", Prompt: "p",
	}); err != nil {
		t.Fatalf("first StartClaudeRun: %v", err)
	}

	// Second call while the first is still IsRunning must return AlreadyExists.
	_, err := srv.StartClaudeRun(t.Context(), &bossanovav1.StartClaudeRunRequest{
		SessionId: "sess-1", Prompt: "p",
	})
	if grpcstatus.Code(err) != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists", grpcstatus.Code(err))
	}
}

func TestStartClaudeRun_StaleEntryReplaced(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.startResp = "claude-1"
	srv := newRepairTestServer(runner, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})

	if _, err := srv.StartClaudeRun(t.Context(), &bossanovav1.StartClaudeRunRequest{
		SessionId: "sess-1", Prompt: "p",
	}); err != nil {
		t.Fatalf("first StartClaudeRun: %v", err)
	}
	runner.finish("claude-1", nil)
	runner.startResp = "claude-2"

	resp, err := srv.StartClaudeRun(t.Context(), &bossanovav1.StartClaudeRunRequest{
		SessionId: "sess-1", Prompt: "p",
	})
	if err != nil {
		t.Fatalf("second StartClaudeRun after first finished: %v", err)
	}
	if resp.GetClaudeId() != "claude-2" {
		t.Errorf("ClaudeId = %q, want claude-2", resp.GetClaudeId())
	}
}

func TestWaitClaudeRun_CleanExit(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.running["claude-x"] = false
	srv := newRepairTestServer(runner)

	resp, err := srv.WaitClaudeRun(t.Context(), &bossanovav1.WaitClaudeRunRequest{ClaudeId: "claude-x"})
	if err != nil {
		t.Fatalf("WaitClaudeRun: %v", err)
	}
	if resp.GetExitError() != "" {
		t.Errorf("ExitError = %q, want empty", resp.GetExitError())
	}
}

func TestWaitClaudeRun_NonZeroExit(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.running["claude-x"] = false
	runner.exitErrors["claude-x"] = errors.New("exit status 1")
	srv := newRepairTestServer(runner)

	resp, err := srv.WaitClaudeRun(t.Context(), &bossanovav1.WaitClaudeRunRequest{ClaudeId: "claude-x"})
	if err != nil {
		t.Fatalf("WaitClaudeRun: %v", err)
	}
	if resp.GetExitError() != "exit status 1" {
		t.Errorf("ExitError = %q, want %q", resp.GetExitError(), "exit status 1")
	}
}

func TestWaitClaudeRun_ContextCancel(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.running["claude-x"] = true // never exits
	srv := newRepairTestServer(runner)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := srv.WaitClaudeRun(ctx, &bossanovav1.WaitClaudeRunRequest{ClaudeId: "claude-x"})
	if grpcstatus.Code(err) != codes.Canceled {
		t.Fatalf("code = %v, want Canceled", grpcstatus.Code(err))
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
