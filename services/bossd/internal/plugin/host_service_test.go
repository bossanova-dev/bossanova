package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/migrate"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/claude"
	"github.com/recurser/bossd/internal/db"
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

// --- Mock ClaudeRunner for attempt tests ---

type mockClaudeRunner struct {
	mu          sync.Mutex
	sessions    map[string]bool // sessionID → running
	exitErrs    map[string]error
	history     map[string][]claude.OutputLine
	lastWorkDir string // captured from most recent Start call
	nextID      int
	startErr    error
}

var _ claude.ClaudeRunner = (*mockClaudeRunner)(nil)

func newMockClaudeRunner() *mockClaudeRunner {
	return &mockClaudeRunner{
		sessions: make(map[string]bool),
		exitErrs: make(map[string]error),
		history:  make(map[string][]claude.OutputLine),
	}
}

func (m *mockClaudeRunner) Start(_ context.Context, workDir, _ string, _ *string) (string, error) {
	if m.startErr != nil {
		return "", m.startErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastWorkDir = workDir
	m.nextID++
	id := fmt.Sprintf("mock-session-%d", m.nextID)
	m.sessions[id] = true
	m.history[id] = []claude.OutputLine{
		{Text: "Starting..."},
		{Text: "Working on plan..."},
	}
	return id, nil
}

func (m *mockClaudeRunner) Stop(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[sessionID] = false
	return nil
}

func (m *mockClaudeRunner) IsRunning(sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[sessionID]
}

func (m *mockClaudeRunner) ExitError(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.exitErrs[sessionID]
}

func (m *mockClaudeRunner) Subscribe(_ context.Context, sessionID string) (<-chan claude.OutputLine, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[sessionID]; !ok {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}
	ch := make(chan claude.OutputLine)
	close(ch)
	return ch, nil
}

func (m *mockClaudeRunner) History(sessionID string) []claude.OutputLine {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.history[sessionID]
}

// --- Test helpers for workflow tests ---

func migrationsDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
}

// setupWorkflowTestServer creates a HostServiceServer backed by a real
// in-memory SQLite WorkflowStore and a mock ClaudeRunner. It also creates
// a repo and session for foreign key satisfaction, returning their IDs.
func setupWorkflowTestServer(t *testing.T) (srv *HostServiceServer, runner *mockClaudeRunner, sessionID, repoID string) {
	t.Helper()

	sqlDB, err := db.OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	if err := migrate.Run(sqlDB, os.DirFS(migrationsDir())); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	// Create repo and session for foreign key constraints.
	repoStore := db.NewRepoStore(sqlDB)
	repo, err := repoStore.Create(context.Background(), db.CreateRepoParams{
		DisplayName:       "test-repo",
		LocalPath:         "/tmp/test-repo",
		OriginURL:         "https://github.com/test/repo.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}

	sessionStore := db.NewSessionStore(sqlDB)
	sess, err := sessionStore.Create(context.Background(), db.CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Workflow test session",
		WorktreePath: "/tmp/wt/workflow-test",
		BranchName:   "feat/workflow-test",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	workflowStore := db.NewWorkflowStore(sqlDB)
	runner = newMockClaudeRunner()

	srv = NewHostServiceServer(&mockVCSProvider{})
	srv.SetWorkflowDeps(workflowStore, sessionStore, runner)

	return srv, runner, sess.ID, repo.ID
}

// --- Workflow RPC tests ---

func TestHostServiceCreateWorkflow(t *testing.T) {
	srv, _, sessionID, repoID := setupWorkflowTestServer(t)
	ctx := context.Background()

	resp, err := srv.CreateWorkflow(ctx, &bossanovav1.CreateWorkflowRequest{
		SessionId:      sessionID,
		RepoId:         repoID,
		PlanPath:       "docs/plans/test-plan.md",
		MaxLegs:        10,
		StartCommitSha: "abc123",
		ConfigJson:     `{"confirm_land": true}`,
	})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	w := resp.GetWorkflow()
	if w.GetId() == "" {
		t.Error("workflow ID should not be empty")
	}
	if w.GetSessionId() != sessionID {
		t.Errorf("session_id = %q, want %q", w.GetSessionId(), sessionID)
	}
	if w.GetRepoId() != repoID {
		t.Errorf("repo_id = %q, want %q", w.GetRepoId(), repoID)
	}
	if w.GetPlanPath() != "docs/plans/test-plan.md" {
		t.Errorf("plan_path = %q, want %q", w.GetPlanPath(), "docs/plans/test-plan.md")
	}
	if w.GetStatus() != "pending" {
		t.Errorf("status = %q, want %q", w.GetStatus(), "pending")
	}
	if w.GetCurrentStep() != "plan" {
		t.Errorf("current_step = %q, want %q", w.GetCurrentStep(), "plan")
	}
	if w.GetMaxLegs() != 10 {
		t.Errorf("max_legs = %d, want 10", w.GetMaxLegs())
	}
	if w.GetStartCommitSha() != "abc123" {
		t.Errorf("start_commit_sha = %q, want %q", w.GetStartCommitSha(), "abc123")
	}
	if w.GetConfigJson() != `{"confirm_land": true}` {
		t.Errorf("config_json = %q, want %q", w.GetConfigJson(), `{"confirm_land": true}`)
	}
}

func TestHostServiceGetWorkflow(t *testing.T) {
	srv, _, sessionID, repoID := setupWorkflowTestServer(t)
	ctx := context.Background()

	created, err := srv.CreateWorkflow(ctx, &bossanovav1.CreateWorkflowRequest{
		SessionId: sessionID,
		RepoId:    repoID,
		PlanPath:  "docs/plans/get-test.md",
		MaxLegs:   20,
	})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	resp, err := srv.GetWorkflow(ctx, &bossanovav1.GetWorkflowRequest{
		Id: created.GetWorkflow().GetId(),
	})
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}

	w := resp.GetWorkflow()
	if w.GetId() != created.GetWorkflow().GetId() {
		t.Errorf("ID = %q, want %q", w.GetId(), created.GetWorkflow().GetId())
	}
	if w.GetPlanPath() != "docs/plans/get-test.md" {
		t.Errorf("plan_path = %q, want %q", w.GetPlanPath(), "docs/plans/get-test.md")
	}
}

func TestHostServiceUpdateWorkflow(t *testing.T) {
	srv, _, sessionID, repoID := setupWorkflowTestServer(t)
	ctx := context.Background()

	created, err := srv.CreateWorkflow(ctx, &bossanovav1.CreateWorkflowRequest{
		SessionId: sessionID,
		RepoId:    repoID,
		PlanPath:  "docs/plans/update-test.md",
		MaxLegs:   20,
	})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	wID := created.GetWorkflow().GetId()

	// Update status, step, and leg.
	newStatus := "running"
	newStep := "implement"
	var newLeg int32 = 1
	resp, err := srv.UpdateWorkflow(ctx, &bossanovav1.UpdateWorkflowRequest{
		Id:          wID,
		Status:      &newStatus,
		CurrentStep: &newStep,
		FlightLeg:   &newLeg,
	})
	if err != nil {
		t.Fatalf("UpdateWorkflow: %v", err)
	}

	w := resp.GetWorkflow()
	if w.GetStatus() != "running" {
		t.Errorf("status = %q, want %q", w.GetStatus(), "running")
	}
	if w.GetCurrentStep() != "implement" {
		t.Errorf("current_step = %q, want %q", w.GetCurrentStep(), "implement")
	}
	if w.GetFlightLeg() != 1 {
		t.Errorf("flight_leg = %d, want 1", w.GetFlightLeg())
	}

	// Update with error message.
	errMsg := "plan validation failed"
	resp, err = srv.UpdateWorkflow(ctx, &bossanovav1.UpdateWorkflowRequest{
		Id:        wID,
		LastError: &errMsg,
	})
	if err != nil {
		t.Fatalf("UpdateWorkflow with error: %v", err)
	}
	if resp.GetWorkflow().GetLastError() != "plan validation failed" {
		t.Errorf("last_error = %q, want %q", resp.GetWorkflow().GetLastError(), "plan validation failed")
	}
}

func TestHostServiceListWorkflows(t *testing.T) {
	srv, _, sessionID, repoID := setupWorkflowTestServer(t)
	ctx := context.Background()

	// Empty list.
	resp, err := srv.ListWorkflows(ctx, &bossanovav1.ListWorkflowsRequest{})
	if err != nil {
		t.Fatalf("ListWorkflows empty: %v", err)
	}
	if len(resp.GetWorkflows()) != 0 {
		t.Errorf("expected 0 workflows, got %d", len(resp.GetWorkflows()))
	}

	// Create two workflows.
	_, err = srv.CreateWorkflow(ctx, &bossanovav1.CreateWorkflowRequest{
		SessionId: sessionID, RepoId: repoID,
		PlanPath: "docs/plans/plan-1.md", MaxLegs: 20,
	})
	if err != nil {
		t.Fatalf("CreateWorkflow 1: %v", err)
	}
	w2, err := srv.CreateWorkflow(ctx, &bossanovav1.CreateWorkflowRequest{
		SessionId: sessionID, RepoId: repoID,
		PlanPath: "docs/plans/plan-2.md", MaxLegs: 10,
	})
	if err != nil {
		t.Fatalf("CreateWorkflow 2: %v", err)
	}

	// List all.
	resp, err = srv.ListWorkflows(ctx, &bossanovav1.ListWorkflowsRequest{})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(resp.GetWorkflows()) != 2 {
		t.Fatalf("expected 2 workflows, got %d", len(resp.GetWorkflows()))
	}

	// Update w2 to running and filter.
	runningStatus := "running"
	_, err = srv.UpdateWorkflow(ctx, &bossanovav1.UpdateWorkflowRequest{
		Id: w2.GetWorkflow().GetId(), Status: &runningStatus,
	})
	if err != nil {
		t.Fatalf("UpdateWorkflow: %v", err)
	}

	resp, err = srv.ListWorkflows(ctx, &bossanovav1.ListWorkflowsRequest{
		StatusFilter: "running",
	})
	if err != nil {
		t.Fatalf("ListWorkflows filtered: %v", err)
	}
	if len(resp.GetWorkflows()) != 1 {
		t.Fatalf("expected 1 running workflow, got %d", len(resp.GetWorkflows()))
	}
	if resp.GetWorkflows()[0].GetId() != w2.GetWorkflow().GetId() {
		t.Errorf("filtered workflow ID = %q, want %q", resp.GetWorkflows()[0].GetId(), w2.GetWorkflow().GetId())
	}
}

func TestHostServiceWorkflowNilStore(t *testing.T) {
	srv := NewHostServiceServer(&mockVCSProvider{})
	// Don't call SetWorkflowDeps — store is nil.
	ctx := context.Background()

	_, err := srv.CreateWorkflow(ctx, &bossanovav1.CreateWorkflowRequest{
		SessionId: "s1", RepoId: "r1", PlanPath: "plan.md", MaxLegs: 10,
	})
	if err == nil {
		t.Fatal("expected error when workflow store not configured")
	}

	_, err = srv.GetWorkflow(ctx, &bossanovav1.GetWorkflowRequest{Id: "any"})
	if err == nil {
		t.Fatal("expected error when workflow store not configured")
	}

	_, err = srv.UpdateWorkflow(ctx, &bossanovav1.UpdateWorkflowRequest{Id: "any"})
	if err == nil {
		t.Fatal("expected error when workflow store not configured")
	}

	_, err = srv.ListWorkflows(ctx, &bossanovav1.ListWorkflowsRequest{})
	if err == nil {
		t.Fatal("expected error when workflow store not configured")
	}
}

// --- Attempt RPC tests ---

func TestHostServiceCreateAttempt(t *testing.T) {
	srv, runner, _, _ := setupWorkflowTestServer(t)
	ctx := context.Background()

	resp, err := srv.CreateAttempt(ctx, &bossanovav1.CreateAttemptRequest{
		WorkflowId: "wf-1",
		SkillName:  "boss-plan",
		Input:      "/boss-plan docs/plans/test.md",
		WorkDir:    "/tmp/workdir",
	})
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	attemptID := resp.GetAttemptId()
	if attemptID == "" {
		t.Fatal("attempt ID should not be empty")
	}

	// Verify the runner started a session.
	if !runner.IsRunning(attemptID) {
		t.Error("runner should report session as running")
	}
}

func TestHostServiceCreateAttemptResolvesWorkDir(t *testing.T) {
	srv, runner, sessionID, repoID := setupWorkflowTestServer(t)
	ctx := context.Background()

	// Create a workflow linked to the session.
	wfResp, err := srv.CreateWorkflow(ctx, &bossanovav1.CreateWorkflowRequest{
		SessionId: sessionID,
		RepoId:    repoID,
		PlanPath:  "docs/plans/test.md",
		MaxLegs:   3,
	})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	workflowID := wfResp.GetWorkflow().GetId()

	// CreateAttempt with empty WorkDir — should resolve from session.
	_, err = srv.CreateAttempt(ctx, &bossanovav1.CreateAttemptRequest{
		WorkflowId: workflowID,
		Input:      "/boss-plan docs/plans/test.md",
		WorkDir:    "", // should be resolved to session's WorktreePath
	})
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	runner.mu.Lock()
	got := runner.lastWorkDir
	runner.mu.Unlock()

	want := "/tmp/wt/workflow-test" // from setupWorkflowTestServer session
	if got != want {
		t.Errorf("workDir = %q, want %q (resolved from session)", got, want)
	}
}

func TestHostServiceGetAttemptStatus_Running(t *testing.T) {
	srv, _, _, _ := setupWorkflowTestServer(t)
	ctx := context.Background()

	// Create an attempt first.
	createResp, err := srv.CreateAttempt(ctx, &bossanovav1.CreateAttemptRequest{
		WorkflowId: "wf-1",
		Input:      "test input",
		WorkDir:    "/tmp/workdir",
	})
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	// Get status — should be running with history lines.
	resp, err := srv.GetAttemptStatus(ctx, &bossanovav1.GetAttemptStatusRequest{
		AttemptId: createResp.GetAttemptId(),
	})
	if err != nil {
		t.Fatalf("GetAttemptStatus: %v", err)
	}
	if resp.GetStatus() != bossanovav1.AttemptRunStatus_ATTEMPT_RUN_STATUS_RUNNING {
		t.Errorf("status = %v, want RUNNING", resp.GetStatus())
	}
	if len(resp.GetOutputLines()) != 2 {
		t.Errorf("output_lines count = %d, want 2", len(resp.GetOutputLines()))
	}
	if resp.GetOutputLines()[0] != "Starting..." {
		t.Errorf("output_lines[0] = %q, want %q", resp.GetOutputLines()[0], "Starting...")
	}
}

func TestHostServiceGetAttemptStatus_Completed(t *testing.T) {
	srv, runner, _, _ := setupWorkflowTestServer(t)
	ctx := context.Background()

	createResp, err := srv.CreateAttempt(ctx, &bossanovav1.CreateAttemptRequest{
		WorkflowId: "wf-1",
		Input:      "test input",
		WorkDir:    "/tmp/workdir",
	})
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	// Stop the session to simulate completion.
	_ = runner.Stop(createResp.GetAttemptId())

	resp, err := srv.GetAttemptStatus(ctx, &bossanovav1.GetAttemptStatusRequest{
		AttemptId: createResp.GetAttemptId(),
	})
	if err != nil {
		t.Fatalf("GetAttemptStatus: %v", err)
	}
	if resp.GetStatus() != bossanovav1.AttemptRunStatus_ATTEMPT_RUN_STATUS_COMPLETED {
		t.Errorf("status = %v, want COMPLETED", resp.GetStatus())
	}
}

func TestHostServiceGetAttemptStatus_Failed(t *testing.T) {
	srv, runner, _, _ := setupWorkflowTestServer(t)
	ctx := context.Background()

	createResp, err := srv.CreateAttempt(ctx, &bossanovav1.CreateAttemptRequest{
		WorkflowId: "wf-1",
		Input:      "test input",
		WorkDir:    "/tmp/workdir",
	})
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	attemptID := createResp.GetAttemptId()

	// Stop the session and set an exit error to simulate a crash.
	_ = runner.Stop(attemptID)
	runner.mu.Lock()
	runner.exitErrs[attemptID] = fmt.Errorf("signal: killed")
	runner.mu.Unlock()

	resp, err := srv.GetAttemptStatus(ctx, &bossanovav1.GetAttemptStatusRequest{
		AttemptId: attemptID,
	})
	if err != nil {
		t.Fatalf("GetAttemptStatus: %v", err)
	}
	if resp.GetStatus() != bossanovav1.AttemptRunStatus_ATTEMPT_RUN_STATUS_FAILED {
		t.Errorf("status = %v, want FAILED", resp.GetStatus())
	}
	if resp.GetError() != "signal: killed" {
		t.Errorf("error = %q, want %q", resp.GetError(), "signal: killed")
	}
}

func TestHostServiceCreateAttemptNilRunner(t *testing.T) {
	srv := NewHostServiceServer(&mockVCSProvider{})
	// Don't call SetWorkflowDeps — runner is nil.
	ctx := context.Background()

	_, err := srv.CreateAttempt(ctx, &bossanovav1.CreateAttemptRequest{
		WorkflowId: "wf-1",
		Input:      "test",
		WorkDir:    "/tmp",
	})
	if err == nil {
		t.Fatal("expected error when claude runner not configured")
	}
}

func TestHostServiceCreateAttemptStartError(t *testing.T) {
	srv, runner, _, _ := setupWorkflowTestServer(t)
	runner.startErr = errors.New("claude binary not found")
	ctx := context.Background()

	_, err := srv.CreateAttempt(ctx, &bossanovav1.CreateAttemptRequest{
		WorkflowId: "wf-1",
		Input:      "test",
		WorkDir:    "/tmp",
	})
	if err == nil {
		t.Fatal("expected error when claude start fails")
	}
}

func TestHostServiceGetAttemptStatusUnknownSession(t *testing.T) {
	srv, _, _, _ := setupWorkflowTestServer(t)
	ctx := context.Background()

	// GetAttemptStatus for a nonexistent session should return COMPLETED
	// (since IsRunning returns false for unknown sessions).
	resp, err := srv.GetAttemptStatus(ctx, &bossanovav1.GetAttemptStatusRequest{
		AttemptId: "nonexistent-session",
	})
	if err != nil {
		t.Fatalf("GetAttemptStatus: %v", err)
	}
	if resp.GetStatus() != bossanovav1.AttemptRunStatus_ATTEMPT_RUN_STATUS_COMPLETED {
		t.Errorf("status = %v, want COMPLETED (unknown sessions report as completed)", resp.GetStatus())
	}
}
