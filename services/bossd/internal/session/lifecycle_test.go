package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/claude"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/tmux"
)

// Compile-time interface assertions for test mocks.
var (
	_ db.SessionStore        = (*mockSessionStore)(nil)
	_ db.RepoStore           = (*mockRepoStore)(nil)
	_ db.ClaudeChatStore     = (*mockClaudeChatStore)(nil)
	_ gitpkg.WorktreeManager = (*mockWorktreeManager)(nil)
	_ claude.ClaudeRunner    = (*mockClaudeRunner)(nil)
	_ vcs.Provider           = (*mockVCSProvider)(nil)
)

// --- Mock SessionStore ---

type mockSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*models.Session
}

func newMockSessionStore() *mockSessionStore {
	return &mockSessionStore{sessions: make(map[string]*models.Session)}
}

func (m *mockSessionStore) Create(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &models.Session{
		ID:           "sess-1",
		RepoID:       params.RepoID,
		Title:        params.Title,
		Plan:         params.Plan,
		WorktreePath: params.WorktreePath,
		BranchName:   params.BranchName,
		BaseBranch:   params.BaseBranch,
		State:        machine.CreatingWorktree,
	}
	m.sessions[s.ID] = s
	return s, nil
}

func (m *mockSessionStore) Get(_ context.Context, id string) (*models.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %s not found", id)
	}
	return s, nil
}

func (m *mockSessionStore) List(_ context.Context, _ string) ([]*models.Session, error) {
	var result []*models.Session
	for _, s := range m.sessions {
		result = append(result, s)
	}
	return result, nil
}

func (m *mockSessionStore) ListActive(_ context.Context, _ string) ([]*models.Session, error) {
	return m.List(context.Background(), "")
}

func (m *mockSessionStore) ListActiveWithRepo(_ context.Context, _ string) ([]*db.SessionWithRepo, error) {
	var result []*db.SessionWithRepo
	for _, s := range m.sessions {
		result = append(result, &db.SessionWithRepo{Session: s})
	}
	return result, nil
}

func (m *mockSessionStore) ListWithRepo(_ context.Context, _ string) ([]*db.SessionWithRepo, error) {
	var result []*db.SessionWithRepo
	for _, s := range m.sessions {
		result = append(result, &db.SessionWithRepo{Session: s})
	}
	return result, nil
}

func (m *mockSessionStore) ListArchived(_ context.Context, _ string) ([]*models.Session, error) {
	return nil, nil
}

func (m *mockSessionStore) Update(_ context.Context, id string, params db.UpdateSessionParams) (*models.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %s not found", id)
	}
	if params.State != nil {
		s.State = machine.State(*params.State)
	}
	if params.WorktreePath != nil {
		s.WorktreePath = *params.WorktreePath
	}
	if params.BranchName != nil {
		s.BranchName = *params.BranchName
	}
	if params.ClaudeSessionID != nil {
		s.ClaudeSessionID = *params.ClaudeSessionID
	}
	if params.PRNumber != nil {
		s.PRNumber = *params.PRNumber
	}
	if params.PRURL != nil {
		s.PRURL = *params.PRURL
	}
	if params.LastCheckState != nil {
		s.LastCheckState = machine.CheckState(*params.LastCheckState)
	}
	if params.AttemptCount != nil {
		s.AttemptCount = *params.AttemptCount
	}
	if params.BlockedReason != nil {
		s.BlockedReason = *params.BlockedReason
	}
	if params.TmuxSessionName != nil {
		s.TmuxSessionName = *params.TmuxSessionName
	}
	if params.CronJobID != nil {
		s.CronJobID = *params.CronJobID
	}
	if params.HookToken != nil {
		s.HookToken = *params.HookToken
	}
	return s, nil
}

func (m *mockSessionStore) Archive(_ context.Context, id string) error {
	s, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("session %s not found", id)
	}
	_ = s
	return nil
}

func (m *mockSessionStore) Resurrect(_ context.Context, id string) error {
	s, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("session %s not found", id)
	}
	s.ArchivedAt = nil
	return nil
}

func (m *mockSessionStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	return nil
}

func (m *mockSessionStore) AdvanceOrphanedSessions(_ context.Context) (int64, error) {
	return 0, nil
}

func (m *mockSessionStore) ListByState(_ context.Context, state int) ([]*models.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*models.Session
	for _, s := range m.sessions {
		if int(s.State) == state {
			result = append(result, s)
		}
	}
	return result, nil
}

func (m *mockSessionStore) UpdateStateConditional(_ context.Context, id string, newState, expectedState int) (bool, error) {
	// Mirror the SQL `UPDATE ... WHERE state = ?` atomic check-and-set so the
	// FinalizeSession idempotency test (concurrent goroutines) sees the same
	// race-free behavior the real SQLite store provides.
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return false, nil
	}
	if int(s.State) != expectedState {
		return false, nil
	}
	s.State = machine.State(newState)
	return true, nil
}

// --- Mock RepoStore ---

type mockRepoStore struct {
	repos map[string]*models.Repo
}

func newMockRepoStore() *mockRepoStore {
	return &mockRepoStore{repos: make(map[string]*models.Repo)}
}

func (m *mockRepoStore) Create(_ context.Context, params db.CreateRepoParams) (*models.Repo, error) {
	r := &models.Repo{
		ID:                "repo-1",
		DisplayName:       params.DisplayName,
		LocalPath:         params.LocalPath,
		DefaultBaseBranch: params.DefaultBaseBranch,
		WorktreeBaseDir:   params.WorktreeBaseDir,
		SetupScript:       params.SetupScript,
	}
	m.repos[r.ID] = r
	return r, nil
}

func (m *mockRepoStore) Get(_ context.Context, id string) (*models.Repo, error) {
	r, ok := m.repos[id]
	if !ok {
		return nil, fmt.Errorf("repo %s not found", id)
	}
	return r, nil
}

func (m *mockRepoStore) GetByPath(_ context.Context, _ string) (*models.Repo, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockRepoStore) List(_ context.Context) ([]*models.Repo, error) {
	var result []*models.Repo
	for _, r := range m.repos {
		result = append(result, r)
	}
	return result, nil
}

func (m *mockRepoStore) Update(_ context.Context, id string, params db.UpdateRepoParams) (*models.Repo, error) {
	r, ok := m.repos[id]
	if !ok {
		return nil, fmt.Errorf("repo %s not found", id)
	}
	if params.OriginURL != nil {
		r.OriginURL = *params.OriginURL
	}
	if params.DisplayName != nil {
		r.DisplayName = *params.DisplayName
	}
	return r, nil
}

func (m *mockRepoStore) Delete(_ context.Context, id string) error {
	delete(m.repos, id)
	return nil
}

// --- Mock ClaudeChatStore ---

// mockClaudeChatStore satisfies db.ClaudeChatStore for lifecycle tests. By
// default Create / UpdateTmuxSessionName / DeleteByClaudeID succeed and
// record their parameters so tests can assert on them. Setting createErr,
// updateTmuxNameErr, etc. forces the corresponding method to return that
// error instead — used by failure-mode tests for the cron tmux path.
type mockClaudeChatStore struct {
	mu                sync.Mutex
	createCalls       []db.CreateClaudeChatParams
	tmuxNameUpdates   []tmuxNameUpdate
	deletedClaudeIDs  []string
	chatsBySession    map[string][]*models.ClaudeChat // returned by ListBySession when set
	createErr         error
	updateTmuxNameErr error
}

type tmuxNameUpdate struct {
	claudeID string
	name     *string
}

func (m *mockClaudeChatStore) Create(_ context.Context, params db.CreateClaudeChatParams) (*models.ClaudeChat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCalls = append(m.createCalls, params)
	if m.createErr != nil {
		return nil, m.createErr
	}
	return &models.ClaudeChat{
		ID:        "chat-" + params.ClaudeID,
		SessionID: params.SessionID,
		ClaudeID:  params.ClaudeID,
		Title:     params.Title,
	}, nil
}

func (m *mockClaudeChatStore) GetByClaudeID(_ context.Context, _ string) (*models.ClaudeChat, error) {
	return nil, nil
}

func (m *mockClaudeChatStore) ListBySession(_ context.Context, sessionID string) ([]*models.ClaudeChat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.chatsBySession == nil {
		return nil, nil
	}
	return m.chatsBySession[sessionID], nil
}

func (m *mockClaudeChatStore) UpdateTitle(_ context.Context, _, _ string) error {
	return nil
}

func (m *mockClaudeChatStore) UpdateTitleByClaudeID(_ context.Context, _, _ string) error {
	return nil
}

func (m *mockClaudeChatStore) UpdateTmuxSessionName(_ context.Context, claudeID string, name *string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tmuxNameUpdates = append(m.tmuxNameUpdates, tmuxNameUpdate{claudeID: claudeID, name: name})
	return m.updateTmuxNameErr
}

func (m *mockClaudeChatStore) DeleteByClaudeID(_ context.Context, claudeID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedClaudeIDs = append(m.deletedClaudeIDs, claudeID)
	return nil
}

func (m *mockClaudeChatStore) ListWithTmuxSession(_ context.Context) ([]*models.ClaudeChat, error) {
	return nil, nil
}

// --- Mock WorktreeManager ---

type mockWorktreeManager struct {
	created                     []gitpkg.CreateOpts
	createdFromExisting         []gitpkg.CreateFromExistingBranchOpts
	createFromExistingBranchErr error // if set, CreateFromExistingBranch returns this error
	archived                    []string
	archiveErr                  error // if set, Archive returns this error
	resurrected                 []gitpkg.ResurrectOpts
	pushed                      []string
	pushErr                     error    // if set, Push returns this error
	emptyCommits                []string // worktree paths on which EmptyCommit was invoked
	originURL                   string   // returned by DetectOriginURL
	statusOut                   string   // returned by Status
	statusErr                   error    // if set, Status returns this error
	worktreePath                string   // override for Create's returned WorktreePath; empty uses the historical fixed path
}

func (m *mockWorktreeManager) Create(_ context.Context, opts gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
	m.created = append(m.created, opts)
	path := m.worktreePath
	if path == "" {
		path = "/tmp/worktrees/test-repo/test-session"
	}
	return &gitpkg.CreateResult{
		WorktreePath: path,
		BranchName:   "test-session",
	}, nil
}

func (m *mockWorktreeManager) Archive(_ context.Context, path string) error {
	m.archived = append(m.archived, path)
	return m.archiveErr
}

func (m *mockWorktreeManager) Resurrect(_ context.Context, opts gitpkg.ResurrectOpts) error {
	m.resurrected = append(m.resurrected, opts)
	return nil
}

func (m *mockWorktreeManager) EmptyCommit(_ context.Context, worktreePath, _ string) error {
	m.emptyCommits = append(m.emptyCommits, worktreePath)
	return nil
}

func (m *mockWorktreeManager) Push(_ context.Context, _ string, branch string) error {
	m.pushed = append(m.pushed, branch)
	return m.pushErr
}

func (m *mockWorktreeManager) Status(_ context.Context, _ string) (string, error) {
	return m.statusOut, m.statusErr
}

func (m *mockWorktreeManager) Clone(_ context.Context, _, _ string) error {
	return nil
}

func (m *mockWorktreeManager) EmptyTrash(_ context.Context, _ string, _ []string) error {
	return nil
}

func (m *mockWorktreeManager) DetectOriginURL(_ context.Context, _ string) (string, error) {
	return m.originURL, nil
}

func (m *mockWorktreeManager) IsGitRepo(_ context.Context, _ string) bool {
	return true
}

func (m *mockWorktreeManager) DetectDefaultBranch(_ context.Context, _ string) (string, error) {
	return "main", nil
}

func (m *mockWorktreeManager) EnsureBaseBranchReadyForSync(_ context.Context, _, _ string) error {
	return nil
}

func (m *mockWorktreeManager) IsAncestor(_ context.Context, _, _, _ string) (bool, error) {
	return true, nil
}

func (m *mockWorktreeManager) MergeLocalBranch(_ context.Context, _, _, _, _ string) error {
	return nil
}

func (m *mockWorktreeManager) FetchBase(_ context.Context, _, _ string) error {
	return nil
}

func (m *mockWorktreeManager) SyncBaseBranch(_ context.Context, _, _ string) error {
	return nil
}

func (m *mockWorktreeManager) CreateFromExistingBranch(_ context.Context, opts gitpkg.CreateFromExistingBranchOpts) (*gitpkg.CreateResult, error) {
	m.createdFromExisting = append(m.createdFromExisting, opts)
	if m.createFromExistingBranchErr != nil {
		return nil, m.createFromExistingBranchErr
	}
	return &gitpkg.CreateResult{
		WorktreePath: "/tmp/worktrees/" + opts.BranchName,
		BranchName:   opts.BranchName,
	}, nil
}

// --- Mock ClaudeRunner ---

type mockClaudeRunner struct {
	started  []mockStartCall
	stopped  []string
	running  map[string]bool
	nextID   string
	startErr error // if set, Start returns this error
}

type mockStartCall struct {
	workDir string
	plan    string
	resume  *string
}

func newMockClaudeRunner() *mockClaudeRunner {
	return &mockClaudeRunner{
		running: make(map[string]bool),
		nextID:  "claude-123",
	}
}

func (m *mockClaudeRunner) Start(_ context.Context, workDir, plan string, resume *string, _ string) (string, error) {
	m.started = append(m.started, mockStartCall{workDir: workDir, plan: plan, resume: resume})
	if m.startErr != nil {
		return "", m.startErr
	}
	id := m.nextID
	m.running[id] = true
	return id, nil
}

func (m *mockClaudeRunner) Stop(sessionID string) error {
	m.stopped = append(m.stopped, sessionID)
	delete(m.running, sessionID)
	return nil
}

func (m *mockClaudeRunner) IsRunning(sessionID string) bool {
	return m.running[sessionID]
}

func (m *mockClaudeRunner) ExitError(_ string) error {
	return nil
}

func (m *mockClaudeRunner) Subscribe(_ context.Context, _ string) (<-chan claude.OutputLine, error) {
	ch := make(chan claude.OutputLine)
	close(ch)
	return ch, nil
}

func (m *mockClaudeRunner) History(_ string) []claude.OutputLine {
	return nil
}

// --- Mock VCS Provider ---

type mockVCSProvider struct {
	createPRCalls      []vcs.CreatePROpts
	markReadyCalls     []int
	mergePRCalls       []int
	nextPRInfo         *vcs.PRInfo
	nextPRStatus       *vcs.PRStatus
	nextCheckResults   []vcs.CheckResult
	nextReviewComments []vcs.ReviewComment
	nextOpenPRs        []vcs.PRSummary
	createPRErr        error
	checkResultsErr    error
	reviewCommentsErr  error
	mergePRErr         error

	getCheckResultsCalls   int
	getReviewCommentsCalls int
}

func newMockVCSProvider() *mockVCSProvider {
	return &mockVCSProvider{
		nextPRInfo:   &vcs.PRInfo{Number: 42, URL: "https://github.com/owner/repo/pull/42"},
		nextPRStatus: &vcs.PRStatus{State: vcs.PRStateOpen},
	}
}

func (m *mockVCSProvider) CreateDraftPR(_ context.Context, opts vcs.CreatePROpts) (*vcs.PRInfo, error) {
	m.createPRCalls = append(m.createPRCalls, opts)
	if m.createPRErr != nil {
		return nil, m.createPRErr
	}
	return m.nextPRInfo, nil
}

func (m *mockVCSProvider) GetPRStatus(_ context.Context, _ string, _ int) (*vcs.PRStatus, error) {
	if m.nextPRStatus != nil {
		return m.nextPRStatus, nil
	}
	return &vcs.PRStatus{State: vcs.PRStateOpen}, nil
}

func (m *mockVCSProvider) GetCheckResults(_ context.Context, _ string, _ int) ([]vcs.CheckResult, error) {
	m.getCheckResultsCalls++
	return m.nextCheckResults, m.checkResultsErr
}

func (m *mockVCSProvider) GetFailedCheckLogs(_ context.Context, _ string, _ string) (string, error) {
	return "", nil
}

func (m *mockVCSProvider) MarkReadyForReview(_ context.Context, _ string, prID int) error {
	m.markReadyCalls = append(m.markReadyCalls, prID)
	return nil
}

func (m *mockVCSProvider) GetReviewComments(_ context.Context, _ string, _ int) ([]vcs.ReviewComment, error) {
	m.getReviewCommentsCalls++
	return m.nextReviewComments, m.reviewCommentsErr
}

func (m *mockVCSProvider) ListOpenPRs(_ context.Context, _ string) ([]vcs.PRSummary, error) {
	return m.nextOpenPRs, nil
}

func (m *mockVCSProvider) ListClosedPRs(_ context.Context, _ string) ([]vcs.PRSummary, error) {
	return nil, nil
}

func (m *mockVCSProvider) UpdatePRTitle(_ context.Context, _ string, _ int, _ string) error {
	return nil
}

func (m *mockVCSProvider) MergePR(_ context.Context, _ string, prID int, _ string) error {
	m.mergePRCalls = append(m.mergePRCalls, prID)
	return m.mergePRErr
}

func (m *mockVCSProvider) GetPRMergeCommit(_ context.Context, _ string, prID int) (string, error) {
	return fmt.Sprintf("mock-merge-%d", prID), nil
}

func (m *mockVCSProvider) GetAllowedMergeStrategies(_ context.Context, _ string) ([]string, error) {
	return []string{"merge", "squash", "rebase"}, nil
}

// --- Tests ---

func TestStartSession(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	logger := zerolog.Nop()

	// Set up test data.
	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Test Session",
		Plan:       "Do something",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	// Verify worktree was created.
	if len(wt.created) != 1 {
		t.Fatalf("expected 1 worktree created, got %d", len(wt.created))
	}
	if wt.created[0].RepoPath != "/tmp/repo" {
		t.Errorf("worktree repo path = %q, want /tmp/repo", wt.created[0].RepoPath)
	}
	if wt.created[0].BaseBranch != "main" {
		t.Errorf("worktree base branch = %q, want main", wt.created[0].BaseBranch)
	}

	// Verify Claude was started.
	if len(cr.started) != 1 {
		t.Fatalf("expected 1 claude start, got %d", len(cr.started))
	}
	if cr.started[0].workDir != "/tmp/worktrees/test-repo/test-session" {
		t.Errorf("claude workDir = %q, want /tmp/worktrees/test-repo/test-session", cr.started[0].workDir)
	}
	if cr.started[0].plan != "Do something" {
		t.Errorf("claude plan = %q, want 'Do something'", cr.started[0].plan)
	}
	if cr.started[0].resume != nil {
		t.Errorf("claude resume = %v, want nil", cr.started[0].resume)
	}

	// Verify session was updated.
	sess := sessions.sessions["sess-1"]
	if sess.State != machine.ImplementingPlan {
		t.Errorf("session state = %v, want ImplementingPlan", sess.State)
	}
	if sess.WorktreePath != "/tmp/worktrees/test-repo/test-session" {
		t.Errorf("worktree path = %q, want /tmp/worktrees/test-repo/test-session", sess.WorktreePath)
	}
	if sess.BranchName != "test-session" {
		t.Errorf("branch name = %q, want test-session", sess.BranchName)
	}
	if sess.ClaudeSessionID == nil || *sess.ClaudeSessionID != "claude-123" {
		t.Errorf("claude session id = %v, want claude-123", sess.ClaudeSessionID)
	}
}

func TestStartSession_ExistingBranchNotOnRemote_FallsBackToCreate(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		createFromExistingBranchErr: fmt.Errorf("fetch branch: git fetch origin dave/fre-1176: exit status 128: fatal: couldn't find remote ref dave/fre-1176"),
	}
	cr := newMockClaudeRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "FRE-1176 Fix login bug",
		Plan:       "Fix the bug",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	// Pass a branch name that doesn't exist on the remote.
	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{ExistingBranch: "dave/fre-1176"}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	// Should have tried CreateFromExistingBranch first.
	if len(wt.createdFromExisting) != 1 {
		t.Fatalf("expected 1 CreateFromExistingBranch call, got %d", len(wt.createdFromExisting))
	}

	// Should have fallen back to Create with the branch name.
	if len(wt.created) != 1 {
		t.Fatalf("expected 1 Create call (fallback), got %d", len(wt.created))
	}
	if wt.created[0].BranchName != "dave/fre-1176" {
		t.Errorf("Create BranchName = %q, want dave/fre-1176", wt.created[0].BranchName)
	}
	if wt.created[0].BaseBranch != "main" {
		t.Errorf("Create BaseBranch = %q, want main", wt.created[0].BaseBranch)
	}
}

func TestStopSession(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	logger := zerolog.Nop()

	claudeID := "claude-123"
	cr.running[claudeID] = true

	sessions.sessions["sess-1"] = &models.Session{
		ID:              "sess-1",
		RepoID:          "repo-1",
		State:           machine.ImplementingPlan,
		ClaudeSessionID: &claudeID,
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	if err := lc.StopSession(ctx, "sess-1"); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	// Verify Claude was stopped.
	if len(cr.stopped) != 1 || cr.stopped[0] != "claude-123" {
		t.Errorf("expected claude-123 stopped, got %v", cr.stopped)
	}

	// Verify state is Closed.
	if sessions.sessions["sess-1"].State != machine.Closed {
		t.Errorf("state = %v, want Closed", sessions.sessions["sess-1"].State)
	}
}

func TestArchiveSession(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo",
	}

	claudeID := "claude-123"
	cr.running[claudeID] = true

	sessions.sessions["sess-1"] = &models.Session{
		ID:              "sess-1",
		RepoID:          "repo-1",
		State:           machine.ImplementingPlan,
		WorktreePath:    "/tmp/worktrees/test-repo/test",
		ClaudeSessionID: &claudeID,
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	if err := lc.ArchiveSession(ctx, "sess-1"); err != nil {
		t.Fatalf("ArchiveSession: %v", err)
	}

	// Verify Claude was stopped.
	if len(cr.stopped) != 1 {
		t.Errorf("expected 1 claude stop, got %d", len(cr.stopped))
	}

	// Verify worktree was archived.
	if len(wt.archived) != 1 || wt.archived[0] != "/tmp/worktrees/test-repo/test" {
		t.Errorf("expected worktree archived at /tmp/worktrees/test-repo/test, got %v", wt.archived)
	}
}

func TestResurrectSession(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo",
	}

	now := func() *models.Session {
		return sessions.sessions["sess-1"]
	}

	archivedAt := func() *models.Session {
		s := &models.Session{
			ID:           "sess-1",
			RepoID:       "repo-1",
			Title:        "Test Session",
			Plan:         "Do something",
			WorktreePath: "/tmp/worktrees/test-repo/test",
			BranchName:   "test",
			State:        machine.ImplementingPlan,
		}
		// Set ArchivedAt to mark as archived.
		t := now // just need a non-nil value
		_ = t
		return s
	}

	// Create an archived session.
	archivedTime := new(struct{}) // placeholder
	_ = archivedTime

	sess := archivedAt()
	// Actually set ArchivedAt.
	archTime := sess.CreatedAt // zero time, but non-nil pointer
	sess.ArchivedAt = &archTime
	sessions.sessions["sess-1"] = sess

	oldClaudeID := "claude-old"
	sess.ClaudeSessionID = &oldClaudeID

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	if err := lc.ResurrectSession(ctx, "sess-1"); err != nil {
		t.Fatalf("ResurrectSession: %v", err)
	}

	// Verify worktree was resurrected.
	if len(wt.resurrected) != 1 {
		t.Fatalf("expected 1 resurrect call, got %d", len(wt.resurrected))
	}
	if wt.resurrected[0].BranchName != "test" {
		t.Errorf("resurrect branch = %q, want test", wt.resurrected[0].BranchName)
	}

	// Verify Claude was started with resume.
	if len(cr.started) != 1 {
		t.Fatalf("expected 1 claude start, got %d", len(cr.started))
	}
	if cr.started[0].resume == nil || *cr.started[0].resume != "claude-old" {
		t.Errorf("expected claude resume with 'claude-old', got %v", cr.started[0].resume)
	}

	// Verify session state is ImplementingPlan.
	if sessions.sessions["sess-1"].State != machine.ImplementingPlan {
		t.Errorf("state = %v, want ImplementingPlan", sessions.sessions["sess-1"].State)
	}
}

func TestResurrectSessionNotArchived(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.ImplementingPlan,
		// ArchivedAt is nil — not archived.
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	err := lc.ResurrectSession(ctx, "sess-1")
	if err == nil {
		t.Fatal("expected error for non-archived session")
	}
	if err.Error() != "session sess-1 is not archived" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestStopSessionNoClaudeProcess(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	logger := zerolog.Nop()

	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.ImplementingPlan,
		// No ClaudeSessionID.
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	if err := lc.StopSession(ctx, "sess-1"); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	// No Claude stop calls.
	if len(cr.stopped) != 0 {
		t.Errorf("expected 0 claude stops, got %d", len(cr.stopped))
	}

	// State should still be Closed.
	if sessions.sessions["sess-1"].State != machine.Closed {
		t.Errorf("state = %v, want Closed", sessions.sessions["sess-1"].State)
	}
}

func TestSubmitPR(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Test Session",
		Plan:         "Do something",
		WorktreePath: "/tmp/worktrees/test-repo/test-session",
		BranchName:   "test-session",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

	if err := lc.SubmitPR(ctx, "sess-1"); err != nil {
		t.Fatalf("SubmitPR: %v", err)
	}

	// Verify branch was pushed.
	if len(wt.pushed) != 1 || wt.pushed[0] != "test-session" {
		t.Errorf("expected push of test-session, got %v", wt.pushed)
	}

	// Verify draft PR was created.
	if len(vp.createPRCalls) != 1 {
		t.Fatalf("expected 1 createPR call, got %d", len(vp.createPRCalls))
	}
	call := vp.createPRCalls[0]
	if call.RepoPath != "owner/repo" {
		t.Errorf("PR repo = %q, want owner/repo", call.RepoPath)
	}
	if call.HeadBranch != "test-session" {
		t.Errorf("PR head = %q, want test-session", call.HeadBranch)
	}
	if call.BaseBranch != "main" {
		t.Errorf("PR base = %q, want main", call.BaseBranch)
	}
	if call.Title != "Test Session" {
		t.Errorf("PR title = %q, want 'Test Session'", call.Title)
	}
	if !call.Draft {
		t.Error("expected draft PR")
	}

	// Verify session was updated with PR info and state.
	sess := sessions.sessions["sess-1"]
	if sess.State != machine.AwaitingChecks {
		t.Errorf("state = %v, want AwaitingChecks", sess.State)
	}
	if sess.PRNumber == nil || *sess.PRNumber != 42 {
		t.Errorf("PR number = %v, want 42", sess.PRNumber)
	}
	if sess.PRURL == nil || *sess.PRURL != "https://github.com/owner/repo/pull/42" {
		t.Errorf("PR URL = %v, want https://github.com/owner/repo/pull/42", sess.PRURL)
	}
}

func TestSubmitPR_ExistingPRStillPushesImplementationCommits(t *testing.T) {
	// When a draft PR was already created up-front (e.g. via createDraftPR
	// during StartSession), SubmitPR must still push so that any commits
	// Claude made on top of the placeholder empty commit reach the remote.
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo",
		OriginURL: "owner/repo",
	}
	existingPR := 7
	existingURL := "https://github.com/owner/repo/pull/7"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Test Session",
		Plan:         "Do something",
		WorktreePath: "/tmp/worktrees/test-repo/test-session",
		BranchName:   "test-session",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		PRNumber:     &existingPR,
		PRURL:        &existingURL,
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

	if err := lc.SubmitPR(ctx, "sess-1"); err != nil {
		t.Fatalf("SubmitPR: %v", err)
	}

	// Verify branch was pushed even though a PR already existed.
	if len(wt.pushed) != 1 || wt.pushed[0] != "test-session" {
		t.Errorf("expected push of test-session, got %v", wt.pushed)
	}

	// Verify no new PR was created.
	if len(vp.createPRCalls) != 0 {
		t.Errorf("expected 0 createPR calls (PR already exists), got %d", len(vp.createPRCalls))
	}

	// Verify session was advanced to AwaitingChecks with PR info preserved.
	sess := sessions.sessions["sess-1"]
	if sess.State != machine.AwaitingChecks {
		t.Errorf("state = %v, want AwaitingChecks", sess.State)
	}
	if sess.PRNumber == nil || *sess.PRNumber != 7 {
		t.Errorf("PR number = %v, want 7", sess.PRNumber)
	}
}

func TestSubmitPRWrongState(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo",
		OriginURL: "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.CreatingWorktree, // wrong state for SubmitPR
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

	err := lc.SubmitPR(ctx, "sess-1")
	if err == nil {
		t.Fatal("expected error for wrong state")
	}
}

func TestStartSession_NoPlan_CreateDraftPRFailsRepoNotReady(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Test Session",
		Plan:       "", // no plan → triggers immediate PR creation
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	// Make CreateDraftPR return ErrRepoNotReady.
	vp.nextPRInfo = nil
	vp.createPRErr = vcs.ErrRepoNotReady

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

	err := lc.StartSession(ctx, "sess-1", StartSessionOpts{})
	if err != nil {
		t.Fatalf("expected session to start successfully despite PR failure, got: %v", err)
	}

	// Session should be in ImplementingPlan state with no PR.
	sess := sessions.sessions["sess-1"]
	if sess.State != machine.ImplementingPlan {
		t.Errorf("expected state ImplementingPlan, got: %v", sess.State)
	}
	if sess.PRNumber != nil {
		t.Errorf("expected PRNumber to be nil, got: %v", *sess.PRNumber)
	}
	if sess.PRURL != nil {
		t.Errorf("expected PRURL to be nil, got: %v", *sess.PRURL)
	}
}

func TestStartSession_SkipSetupScript_NilsSetupScript(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	logger := zerolog.Nop()

	setupCmd := "npm install"
	repos.repos["repo-1"] = &models.Repo{
		ID:              "repo-1",
		LocalPath:       "/tmp/repo",
		WorktreeBaseDir: "/tmp/worktrees",
		DisplayName:     "test-repo",
		SetupScript:     &setupCmd,
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Bump lodash",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	// skipSetupScript = true with an existing branch (dependabot PR path).
	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		ExistingBranch:  "dependabot/npm/lodash-4.17.21",
		SkipSetupScript: true,
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	// Verify CreateFromExistingBranch was called with nil SetupScript.
	if len(wt.createdFromExisting) != 1 {
		t.Fatalf("expected 1 CreateFromExistingBranch call, got %d", len(wt.createdFromExisting))
	}
	if wt.createdFromExisting[0].SetupScript != nil {
		t.Errorf("expected nil SetupScript when skipSetupScript=true, got %q", *wt.createdFromExisting[0].SetupScript)
	}
}

func TestStartSession_SkipSetupScript_NewBranch(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	logger := zerolog.Nop()

	setupCmd := "npm install"
	repos.repos["repo-1"] = &models.Repo{
		ID:              "repo-1",
		LocalPath:       "/tmp/repo",
		WorktreeBaseDir: "/tmp/worktrees",
		DisplayName:     "test-repo",
		SetupScript:     &setupCmd,
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Bump lodash",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	// skipSetupScript = true with no existing branch (new branch path).
	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{SkipSetupScript: true}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	// Verify Create was called with nil SetupScript.
	if len(wt.created) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(wt.created))
	}
	if wt.created[0].SetupScript != nil {
		t.Errorf("expected nil SetupScript when skipSetupScript=true, got %q", *wt.created[0].SetupScript)
	}
}

func TestStartQuickChatSession(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Quick chat",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	if err := lc.StartQuickChatSession(ctx, "sess-1"); err != nil {
		t.Fatalf("StartQuickChatSession: %v", err)
	}

	// Verify NO worktree was created.
	if len(wt.created) != 0 {
		t.Errorf("expected 0 worktrees created, got %d", len(wt.created))
	}
	if len(wt.createdFromExisting) != 0 {
		t.Errorf("expected 0 existing branch worktrees, got %d", len(wt.createdFromExisting))
	}

	// Verify Claude was NOT started — chat launch happens on-demand from
	// the boss CLI's PTY manager, not from StartSession.
	if len(cr.started) != 0 {
		t.Fatalf("expected 0 claude starts, got %d", len(cr.started))
	}

	// Verify session was updated correctly.
	sess := sessions.sessions["sess-1"]
	if sess.State != machine.ImplementingPlan {
		t.Errorf("session state = %v, want ImplementingPlan", sess.State)
	}
	if sess.WorktreePath != "/tmp/repo" {
		t.Errorf("worktree path = %q, want /tmp/repo", sess.WorktreePath)
	}
	if sess.BranchName != "" {
		t.Errorf("branch name = %q, want empty", sess.BranchName)
	}
}

func TestArchiveQuickChatSession(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo",
	}

	claudeID := "claude-123"
	cr.running[claudeID] = true

	sessions.sessions["sess-1"] = &models.Session{
		ID:              "sess-1",
		RepoID:          "repo-1",
		State:           machine.ImplementingPlan,
		WorktreePath:    "/tmp/repo", // same as repo.LocalPath → quick chat
		ClaudeSessionID: &claudeID,
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	if err := lc.ArchiveSession(ctx, "sess-1"); err != nil {
		t.Fatalf("ArchiveSession: %v", err)
	}

	// Verify Claude was stopped.
	if len(cr.stopped) != 1 {
		t.Errorf("expected 1 claude stop, got %d", len(cr.stopped))
	}

	// Verify worktree was NOT archived (would destroy base repo).
	if len(wt.archived) != 0 {
		t.Errorf("expected 0 worktree archives for quick chat, got %d", len(wt.archived))
	}
}

func TestResurrectQuickChatSession(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo",
	}

	archTime := models.Session{}.CreatedAt
	oldClaudeID := "claude-old"
	sessions.sessions["sess-1"] = &models.Session{
		ID:              "sess-1",
		RepoID:          "repo-1",
		Title:           "Quick chat",
		WorktreePath:    "/tmp/repo", // same as repo.LocalPath → quick chat
		BranchName:      "",
		State:           machine.ImplementingPlan,
		ArchivedAt:      &archTime,
		ClaudeSessionID: &oldClaudeID,
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	if err := lc.ResurrectSession(ctx, "sess-1"); err != nil {
		t.Fatalf("ResurrectSession: %v", err)
	}

	// Verify worktree was NOT resurrected (no worktree to recreate).
	if len(wt.resurrected) != 0 {
		t.Errorf("expected 0 resurrect calls for quick chat, got %d", len(wt.resurrected))
	}

	// Verify Claude was started with resume.
	if len(cr.started) != 1 {
		t.Fatalf("expected 1 claude start, got %d", len(cr.started))
	}
	if cr.started[0].resume == nil || *cr.started[0].resume != "claude-old" {
		t.Errorf("expected claude resume with 'claude-old', got %v", cr.started[0].resume)
	}

	// Verify session state is ImplementingPlan.
	if sessions.sessions["sess-1"].State != machine.ImplementingPlan {
		t.Errorf("state = %v, want ImplementingPlan", sessions.sessions["sess-1"].State)
	}
}

func TestResolveOriginURL_AlreadySet(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo",
		OriginURL: "git@github.com:owner/repo.git",
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	url, err := lc.resolveOriginURL(ctx, repos.repos["repo-1"])
	if err != nil {
		t.Fatalf("resolveOriginURL: %v", err)
	}
	if url != "git@github.com:owner/repo.git" {
		t.Errorf("url = %q, want git@github.com:owner/repo.git", url)
	}
}

func TestResolveOriginURL_EmptyReDetected(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{originURL: "git@github.com:owner/repo.git"}
	cr := newMockClaudeRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo",
		OriginURL: "", // empty — needs re-detection
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	url, err := lc.resolveOriginURL(ctx, repos.repos["repo-1"])
	if err != nil {
		t.Fatalf("resolveOriginURL: %v", err)
	}
	if url != "git@github.com:owner/repo.git" {
		t.Errorf("url = %q, want git@github.com:owner/repo.git", url)
	}
	// Verify it was persisted to the repo.
	if repos.repos["repo-1"].OriginURL != "git@github.com:owner/repo.git" {
		t.Errorf("repo.OriginURL = %q, want git@github.com:owner/repo.git", repos.repos["repo-1"].OriginURL)
	}
}

func TestResolveOriginURL_EmptyNoRemote(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{originURL: ""} // no remote configured
	cr := newMockClaudeRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:          "repo-1",
		DisplayName: "test-repo",
		LocalPath:   "/tmp/repo",
		OriginURL:   "",
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	_, err := lc.resolveOriginURL(ctx, repos.repos["repo-1"])
	if err == nil {
		t.Fatal("expected error when no origin remote is configured")
	}
	if !strings.Contains(err.Error(), "no origin remote configured") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestStartSession_NoPlan_EmptyOriginURL_ReDetected(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{originURL: "git@github.com:owner/repo.git"}
	cr := newMockClaudeRunner()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "", // empty — should be re-detected
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Test Session",
		Plan:       "", // no plan → triggers immediate PR creation
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	// Verify origin URL was re-detected and persisted.
	if repos.repos["repo-1"].OriginURL != "git@github.com:owner/repo.git" {
		t.Errorf("repo.OriginURL = %q, want git@github.com:owner/repo.git", repos.repos["repo-1"].OriginURL)
	}

	// Verify PR was created with the re-detected URL.
	if len(vp.createPRCalls) != 1 {
		t.Fatalf("expected 1 createPR call, got %d", len(vp.createPRCalls))
	}
	if vp.createPRCalls[0].RepoPath != "git@github.com:owner/repo.git" {
		t.Errorf("PR repo = %q, want git@github.com:owner/repo.git", vp.createPRCalls[0].RepoPath)
	}
}

func TestStartSession_NoSkipSetupScript_PassesSetupScript(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	logger := zerolog.Nop()

	setupCmd := "npm install"
	repos.repos["repo-1"] = &models.Repo{
		ID:              "repo-1",
		LocalPath:       "/tmp/repo",
		WorktreeBaseDir: "/tmp/worktrees",
		DisplayName:     "test-repo",
		SetupScript:     &setupCmd,
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Bump lodash",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, newMockVCSProvider(), logger)

	// skipSetupScript = false with existing branch.
	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		ExistingBranch: "dependabot/npm/lodash-4.17.21",
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	// Verify CreateFromExistingBranch was called WITH SetupScript.
	if len(wt.createdFromExisting) != 1 {
		t.Fatalf("expected 1 CreateFromExistingBranch call, got %d", len(wt.createdFromExisting))
	}
	if wt.createdFromExisting[0].SetupScript == nil {
		t.Error("expected non-nil SetupScript when skipSetupScript=false")
	} else if *wt.createdFromExisting[0].SetupScript != "npm install" {
		t.Errorf("expected SetupScript 'npm install', got %q", *wt.createdFromExisting[0].SetupScript)
	}
}

// TestStartSession_DeferPRFalse_CreatesDraftPR pins the pre-FL3 behavior: a
// session with no PR and DeferPR=false (the zero value) must still get an
// up-front draft PR during StartSession.
func TestStartSession_DeferPRFalse_CreatesDraftPR(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Test Session",
		Plan:       "Do something",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if len(vp.createPRCalls) != 1 {
		t.Fatalf("expected 1 createPR call with DeferPR=false, got %d", len(vp.createPRCalls))
	}
	sess := sessions.sessions["sess-1"]
	if sess.PRNumber == nil {
		t.Error("expected PRNumber to be populated after up-front draft PR")
	}
}

// TestStartSession_DeferPRTrue_SkipsDraftPR verifies the cron-session path:
// DeferPR=true must suppress the up-front PR creation so the finalize path
// can later call EnsurePR based on the session's outcome.
func TestStartSession_DeferPRTrue_SkipsDraftPR(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Nightly audit",
		Plan:       "Run the audit",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{DeferPR: true}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if len(vp.createPRCalls) != 0 {
		t.Errorf("expected 0 createPR calls with DeferPR=true, got %d", len(vp.createPRCalls))
	}
	if len(wt.emptyCommits) != 0 {
		t.Errorf("expected 0 empty commits with DeferPR=true, got %d", len(wt.emptyCommits))
	}
	sess := sessions.sessions["sess-1"]
	if sess.PRNumber != nil {
		t.Errorf("expected PRNumber to remain nil, got %v", sess.PRNumber)
	}
}

// TestStartSession_CronJobID_Persisted verifies that opts.CronJobID is
// written onto the session row so the finalize path and cron UI can
// identify which cron job produced a given session.
func TestStartSession_CronJobID_Persisted(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockClaudeChatStore{}
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	tx := tmux.NewClient(tmux.WithCommandFactory(newFakeTmux().factory))
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Nightly audit",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := NewLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), logger)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		DeferPR:   true,
		CronJobID: "cron-42",
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	sess := sessions.sessions["sess-1"]
	if sess.CronJobID == nil {
		t.Fatal("expected CronJobID to be persisted, got nil")
	}
	if *sess.CronJobID != "cron-42" {
		t.Errorf("CronJobID = %q, want %q", *sess.CronJobID, "cron-42")
	}
}

// TestStartSession_HookToken_InstallsHookConfig verifies that when
// StartSessionOpts.HookToken is set, StartSession uses the in-memory
// hook port supplied via Lifecycle.SetHookPort and writes a Stop-hook
// config into worktree/.claude/settings.local.json referencing the
// token + port. The hook server runs in the same process as the
// lifecycle, so the port is plumbed via dependency injection — there
// is no port file on disk to read. Non-cron sessions (empty HookToken)
// skip this path entirely.
func TestStartSession_HookToken_InstallsHookConfig(t *testing.T) {
	ctx := context.Background()

	worktreeDir := t.TempDir()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockClaudeChatStore{}
	wt := &mockWorktreeManager{worktreePath: worktreeDir}
	cr := newMockClaudeRunner()
	tx := tmux.NewClient(tmux.WithCommandFactory(newFakeTmux().factory))
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Nightly audit",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := NewLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), logger)
	lc.SetHookPort(45678)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		DeferPR:   true,
		CronJobID: "cron-42",
		HookToken: "secret-token-123",
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	settingsPath := filepath.Join(worktreeDir, ".claude", "settings.local.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	body := string(data)
	for _, want := range []string{"bossd-finalize", "Bearer secret-token-123", "127.0.0.1:45678", "/hooks/finalize/sess-1"} {
		if !strings.Contains(body, want) {
			t.Errorf("settings.local.json missing %q:\n%s", want, body)
		}
	}
}

// TestEnsurePR_Idempotent verifies EnsurePR is a no-op when the session
// already has a PR, and creates one when it doesn't — without an extra
// empty commit (unlike the StartSession up-front path, which needs one to
// make a PR-less branch diverge from base).
func TestEnsurePR_Idempotent(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	t.Run("PRAlreadyExists_NoOp", func(t *testing.T) {
		sessions := newMockSessionStore()
		repos := newMockRepoStore()
		wt := &mockWorktreeManager{}
		cr := newMockClaudeRunner()
		vp := newMockVCSProvider()

		repos.repos["repo-1"] = &models.Repo{
			ID:        "repo-1",
			LocalPath: "/tmp/repo",
			OriginURL: "owner/repo",
		}
		existingPR := 7
		existingURL := "https://github.com/owner/repo/pull/7"
		sessions.sessions["sess-1"] = &models.Session{
			ID:           "sess-1",
			RepoID:       "repo-1",
			Title:        "Has PR",
			WorktreePath: "/tmp/wt",
			BranchName:   "br-1",
			BaseBranch:   "main",
			PRNumber:     &existingPR,
			PRURL:        &existingURL,
		}

		lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

		if err := lc.EnsurePR(ctx, "sess-1"); err != nil {
			t.Fatalf("EnsurePR: %v", err)
		}

		if len(vp.createPRCalls) != 0 {
			t.Errorf("expected 0 createPR calls when PR exists, got %d", len(vp.createPRCalls))
		}
		if len(wt.pushed) != 0 {
			t.Errorf("expected 0 pushes when PR exists, got %d", len(wt.pushed))
		}
	})

	t.Run("NoPR_PushesAndCreates", func(t *testing.T) {
		sessions := newMockSessionStore()
		repos := newMockRepoStore()
		wt := &mockWorktreeManager{}
		cr := newMockClaudeRunner()
		vp := newMockVCSProvider()

		repos.repos["repo-1"] = &models.Repo{
			ID:        "repo-1",
			LocalPath: "/tmp/repo",
			OriginURL: "owner/repo",
		}
		sessions.sessions["sess-1"] = &models.Session{
			ID:           "sess-1",
			RepoID:       "repo-1",
			Title:        "Deferred PR",
			Plan:         "Do thing",
			WorktreePath: "/tmp/wt",
			BranchName:   "br-1",
			BaseBranch:   "main",
		}

		lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

		if err := lc.EnsurePR(ctx, "sess-1"); err != nil {
			t.Fatalf("EnsurePR: %v", err)
		}

		if len(wt.pushed) != 1 || wt.pushed[0] != "br-1" {
			t.Errorf("expected push of br-1, got %v", wt.pushed)
		}
		// EnsurePR must not produce an empty placeholder commit — Claude
		// already made real commits by the time finalize calls in.
		if len(wt.emptyCommits) != 0 {
			t.Errorf("EnsurePR should not make empty commits, got %d", len(wt.emptyCommits))
		}
		if len(vp.createPRCalls) != 1 {
			t.Fatalf("expected 1 createPR call, got %d", len(vp.createPRCalls))
		}
		sess := sessions.sessions["sess-1"]
		if sess.PRNumber == nil || *sess.PRNumber != 42 {
			t.Errorf("expected PRNumber = 42, got %v", sess.PRNumber)
		}

		// Second call must be a no-op (idempotency).
		prevPushes := len(wt.pushed)
		prevPRCalls := len(vp.createPRCalls)
		if err := lc.EnsurePR(ctx, "sess-1"); err != nil {
			t.Fatalf("EnsurePR second call: %v", err)
		}
		if len(wt.pushed) != prevPushes {
			t.Errorf("second EnsurePR call pushed again: got %d pushes, want %d", len(wt.pushed), prevPushes)
		}
		if len(vp.createPRCalls) != prevPRCalls {
			t.Errorf("second EnsurePR call re-created PR: got %d calls, want %d", len(vp.createPRCalls), prevPRCalls)
		}
	})
}

// --- Cron tmux helpers ---

// recordedTmuxCall is one captured tmux invocation (subcommand + args).
type recordedTmuxCall struct {
	subcommand string
	args       []string
}

// fakeTmux drives a *tmux.Client via WithCommandFactory so cron tmux tests
// can assert which subcommands ran and stub their exit status without
// actually invoking tmux. Specific subcommands can be made to fail via
// failSubcommand. capture-pane returns capturePaneOutput so the SendPlan
// ready-marker poll succeeds without sleeping.
type fakeTmux struct {
	mu                sync.Mutex
	calls             []recordedTmuxCall
	failSubcommand    map[string]bool // subcommand → return non-zero
	capturePaneOutput string          // output for capture-pane stdout
	available         bool            // controls whether `tmux -V` succeeds
}

func newFakeTmux() *fakeTmux {
	return &fakeTmux{
		failSubcommand:    map[string]bool{},
		capturePaneOutput: "Welcome to Claude\n❯\n",
		available:         true,
	}
}

// factory implements tmux.CommandFactory. It records every invocation, then
// returns a no-op exec.Cmd whose exit status reflects the configured
// subcommand-level failure flag. capture-pane returns canned stdout so the
// SendPlan ready-marker wait passes immediately.
func (f *fakeTmux) factory(ctx context.Context, name string, args ...string) *exec.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()

	subcommand := ""
	if len(args) > 0 {
		subcommand = args[0]
	}
	// Treat `tmux -V` as the availability probe, not a subcommand.
	if subcommand == "-V" {
		if !f.available {
			return exec.CommandContext(ctx, "false")
		}
		return exec.CommandContext(ctx, "true")
	}

	f.calls = append(f.calls, recordedTmuxCall{subcommand: subcommand, args: append([]string(nil), args[1:]...)})

	if f.failSubcommand[subcommand] {
		return exec.CommandContext(ctx, "false")
	}
	if subcommand == "capture-pane" {
		return exec.CommandContext(ctx, "printf", "%s", f.capturePaneOutput)
	}
	return exec.CommandContext(ctx, "true")
}

func (f *fakeTmux) hasSubcommand(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.subcommand == name {
			return true
		}
	}
	return false
}

// --- Cron tmux tests ---

// TestStartSession_CronJobID_TmuxAvailable_HappyPath verifies the cron
// branch of StartSession: when CronJobID is set and tmux is available,
// it spawns claude inside a tmux session, persists a claude_chats row,
// invokes SendPlan, and writes the new claude session ID onto the
// session row.
func TestStartSession_CronJobID_TmuxAvailable_HappyPath(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockClaudeChatStore{}
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	fake := newFakeTmux()
	tx := tmux.NewClient(tmux.WithCommandFactory(fake.factory))
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Nightly audit",
		Plan:       "Run the audit",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := NewLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), logger)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		DeferPR:   true,
		CronJobID: "cron-42",
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	// The headless claude path must NOT have run.
	if len(cr.started) != 0 {
		t.Errorf("expected 0 headless claude.Start calls on cron path, got %d", len(cr.started))
	}

	// tmux new-session must have been issued with claude --session-id ...
	var newSessArgs []string
	for _, c := range fake.calls {
		if c.subcommand == "new-session" {
			newSessArgs = c.args
			break
		}
	}
	if newSessArgs == nil {
		t.Fatal("expected tmux new-session call, none recorded")
	}
	joined := strings.Join(newSessArgs, " ")
	if !strings.Contains(joined, "claude --session-id ") {
		t.Errorf("expected new-session args to contain `claude --session-id ...`, got: %s", joined)
	}

	// claude_chats.Create must have been called once with a matching ClaudeID
	// and the cron-style title.
	if len(chats.createCalls) != 1 {
		t.Fatalf("expected 1 claudeChats.Create call, got %d", len(chats.createCalls))
	}
	createdClaudeID := chats.createCalls[0].ClaudeID
	if createdClaudeID == "" {
		t.Error("expected non-empty ClaudeID on claudeChats.Create")
	}
	if chats.createCalls[0].Title != `Run "Nightly audit"` {
		t.Errorf(`Title = %q, want %q`, chats.createCalls[0].Title, `Run "Nightly audit"`)
	}
	if chats.createCalls[0].SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", chats.createCalls[0].SessionID)
	}

	// UpdateTmuxSessionName persisted the resolved tmux name onto the chat row.
	if len(chats.tmuxNameUpdates) != 1 {
		t.Fatalf("expected 1 UpdateTmuxSessionName call, got %d", len(chats.tmuxNameUpdates))
	}
	if chats.tmuxNameUpdates[0].claudeID != createdClaudeID {
		t.Errorf("UpdateTmuxSessionName claudeID = %q, want %q", chats.tmuxNameUpdates[0].claudeID, createdClaudeID)
	}
	if chats.tmuxNameUpdates[0].name == nil || *chats.tmuxNameUpdates[0].name == "" {
		t.Error("expected non-nil/non-empty tmux name persisted on chat row")
	}

	// SendPlan must have run: load-buffer + paste-buffer + send-keys.
	for _, sub := range []string{"load-buffer", "paste-buffer", "send-keys"} {
		if !fake.hasSubcommand(sub) {
			t.Errorf("expected tmux %s call from SendPlan, none recorded", sub)
		}
	}

	// The new claude session UUID was persisted on the session row.
	sess := sessions.sessions["sess-1"]
	if sess.ClaudeSessionID == nil || *sess.ClaudeSessionID != createdClaudeID {
		t.Errorf("session.ClaudeSessionID = %v, want %q", sess.ClaudeSessionID, createdClaudeID)
	}
	if sess.State != machine.ImplementingPlan {
		t.Errorf("session.State = %v, want ImplementingPlan", sess.State)
	}
}

// TestStartSession_CronJobID_TmuxUnavailable_Errors verifies that when
// tmux is not available, the cron branch returns an error before any
// tmux session is created or any claude_chats row is written. The
// scheduler turns this into a fire_failed cron outcome.
func TestStartSession_CronJobID_TmuxUnavailable_Errors(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockClaudeChatStore{}
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	fake := newFakeTmux()
	fake.available = false
	tx := tmux.NewClient(tmux.WithCommandFactory(fake.factory))
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Nightly audit",
		Plan:       "Run the audit",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := NewLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), logger)

	err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		DeferPR:   true,
		CronJobID: "cron-42",
	})
	if err == nil {
		t.Fatal("expected error when tmux is unavailable on cron path")
	}
	if !strings.Contains(err.Error(), "tmux unavailable") {
		t.Errorf("expected tmux-unavailable error, got: %v", err)
	}

	// No new-session call should have been issued.
	if fake.hasSubcommand("new-session") {
		t.Error("expected no tmux new-session call when tmux unavailable")
	}
	// No claude_chats row should have been created.
	if len(chats.createCalls) != 0 {
		t.Errorf("expected 0 claudeChats.Create calls when tmux unavailable, got %d", len(chats.createCalls))
	}
}

// TestStartSession_CronJobID_ChatCreateFails_KillsTmux verifies the
// cleanup contract: if claude_chats.Create fails after tmux NewSession
// already succeeded, the tmux session is killed so we don't leave a
// running claude process orphaned from any DB row.
func TestStartSession_CronJobID_ChatCreateFails_KillsTmux(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockClaudeChatStore{
		createErr: fmt.Errorf("simulated DB failure"),
	}
	wt := &mockWorktreeManager{}
	cr := newMockClaudeRunner()
	fake := newFakeTmux()
	tx := tmux.NewClient(tmux.WithCommandFactory(fake.factory))
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Nightly audit",
		Plan:       "Run the audit",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := NewLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), logger)

	err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		DeferPR:   true,
		CronJobID: "cron-42",
	})
	if err == nil {
		t.Fatal("expected error when claude_chats.Create fails")
	}

	if !fake.hasSubcommand("new-session") {
		t.Error("expected tmux new-session call before chat create failure")
	}
	if len(chats.createCalls) != 1 {
		t.Errorf("expected 1 claudeChats.Create attempt, got %d", len(chats.createCalls))
	}
	if !fake.hasSubcommand("kill-session") {
		t.Error("expected tmux kill-session call to clean up after chat create failure")
	}
}

// TestFinalizeNoChanges_KillsChatTmuxSessionsBeforeDelete verifies the
// cleanup ordering: tmux must be torn down BEFORE the session row is
// deleted, because claude_chats.session_id is ON DELETE CASCADE — once
// the row is gone the tmux_session_name is unrecoverable.
func TestFinalizeNoChanges_KillsChatTmuxSessionsBeforeDelete(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{statusOut: ""} // empty = no changes
	cr := newMockClaudeRunner()
	vp := newMockVCSProvider()

	tmuxName := "boss-repo1234-claude01"
	chats := &mockClaudeChatStore{
		chatsBySession: map[string][]*models.ClaudeChat{
			"sess-1": {{
				ID:              "chat-claude-01",
				SessionID:       "sess-1",
				ClaudeID:        "claude-01",
				TmuxSessionName: &tmuxName,
			}},
		},
	}

	// Track the order of operations: kill-session must happen before sessions.Delete.
	var (
		op             atomic.Int32
		killOpIdx      atomic.Int32
		deleteOpIdx    atomic.Int32
		fakeTmuxClient = &fakeTmux{
			failSubcommand:    map[string]bool{},
			available:         true,
			capturePaneOutput: "",
		}
	)
	killOpIdx.Store(-1)
	deleteOpIdx.Store(-1)

	tx := tmux.NewClient(tmux.WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := fakeTmuxClient.factory(ctx, name, args...)
		if len(args) > 0 && args[0] == "kill-session" {
			killOpIdx.CompareAndSwap(-1, op.Add(1))
		}
		return cmd
	}))

	// Wrap sessions to record when Delete is called relative to kill-session.
	wrappedSessions := &orderingSessionStore{
		mockSessionStore: sessions,
		onDelete: func(_ string) {
			deleteOpIdx.CompareAndSwap(-1, op.Add(1))
		},
	}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
	}
	cronJobID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
	}

	lc := NewLifecycle(wrappedSessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, vp, logger)
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}
	if res.Outcome != models.CronJobOutcomeDeletedNoChanges {
		t.Fatalf("outcome = %q, want %q", res.Outcome, models.CronJobOutcomeDeletedNoChanges)
	}

	// kill-session must have run, and must have run before sessions.Delete.
	if killOpIdx.Load() < 0 {
		t.Fatal("expected tmux kill-session to be invoked for chat with TmuxSessionName")
	}
	if deleteOpIdx.Load() < 0 {
		t.Fatal("expected sessions.Delete to be invoked")
	}
	if killOpIdx.Load() >= deleteOpIdx.Load() {
		t.Errorf("expected kill-session (op %d) to run before sessions.Delete (op %d)",
			killOpIdx.Load(), deleteOpIdx.Load())
	}
}

// orderingSessionStore wraps mockSessionStore to invoke a callback when
// Delete fires, so the finalize-cleanup-order test can record sequencing.
type orderingSessionStore struct {
	*mockSessionStore
	onDelete func(id string)
}

func (o *orderingSessionStore) Delete(ctx context.Context, id string) error {
	if o.onDelete != nil {
		o.onDelete(id)
	}
	return o.mockSessionStore.Delete(ctx, id)
}
