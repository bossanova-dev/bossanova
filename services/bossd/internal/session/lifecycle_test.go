package session

import (
	"context"
	"fmt"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/claude"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
)

// Compile-time interface assertions for test mocks.
var (
	_ db.SessionStore        = (*mockSessionStore)(nil)
	_ db.RepoStore           = (*mockRepoStore)(nil)
	_ gitpkg.WorktreeManager = (*mockWorktreeManager)(nil)
	_ claude.ClaudeRunner    = (*mockClaudeRunner)(nil)
	_ vcs.Provider           = (*mockVCSProvider)(nil)
)

// --- Mock SessionStore ---

type mockSessionStore struct {
	sessions map[string]*models.Session
}

func newMockSessionStore() *mockSessionStore {
	return &mockSessionStore{sessions: make(map[string]*models.Session)}
}

func (m *mockSessionStore) Create(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
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

func (m *mockSessionStore) ListArchived(_ context.Context, _ string) ([]*models.Session, error) {
	return nil, nil
}

func (m *mockSessionStore) Update(_ context.Context, id string, params db.UpdateSessionParams) (*models.Session, error) {
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
	delete(m.sessions, id)
	return nil
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

func (m *mockRepoStore) Update(_ context.Context, _ string, _ db.UpdateRepoParams) (*models.Repo, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockRepoStore) Delete(_ context.Context, id string) error {
	delete(m.repos, id)
	return nil
}

// --- Mock WorktreeManager ---

type mockWorktreeManager struct {
	created     []gitpkg.CreateOpts
	archived    []string
	resurrected []gitpkg.ResurrectOpts
	pushed      []string
}

func (m *mockWorktreeManager) Create(_ context.Context, opts gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
	m.created = append(m.created, opts)
	return &gitpkg.CreateResult{
		WorktreePath: "/tmp/worktrees/test-repo/test-session",
		BranchName:   "test-session",
	}, nil
}

func (m *mockWorktreeManager) Archive(_ context.Context, path string) error {
	m.archived = append(m.archived, path)
	return nil
}

func (m *mockWorktreeManager) Resurrect(_ context.Context, opts gitpkg.ResurrectOpts) error {
	m.resurrected = append(m.resurrected, opts)
	return nil
}

func (m *mockWorktreeManager) EmptyCommit(_ context.Context, _, _ string) error {
	return nil
}

func (m *mockWorktreeManager) Push(_ context.Context, _ string, branch string) error {
	m.pushed = append(m.pushed, branch)
	return nil
}

func (m *mockWorktreeManager) Clone(_ context.Context, _, _ string) error {
	return nil
}

func (m *mockWorktreeManager) EmptyTrash(_ context.Context, _ string, _ []string) error {
	return nil
}

func (m *mockWorktreeManager) DetectOriginURL(_ context.Context, _ string) (string, error) {
	return "", nil
}

func (m *mockWorktreeManager) IsGitRepo(_ context.Context, _ string) bool {
	return true
}

func (m *mockWorktreeManager) DetectDefaultBranch(_ context.Context, _ string) (string, error) {
	return "main", nil
}

func (m *mockWorktreeManager) CreateFromExistingBranch(_ context.Context, opts gitpkg.CreateFromExistingBranchOpts) (*gitpkg.CreateResult, error) {
	return &gitpkg.CreateResult{
		WorktreePath: "/tmp/worktrees/" + opts.BranchName,
		BranchName:   opts.BranchName,
	}, nil
}

// --- Mock ClaudeRunner ---

type mockClaudeRunner struct {
	started []mockStartCall
	stopped []string
	running map[string]bool
	nextID  string
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

func (m *mockClaudeRunner) Start(_ context.Context, workDir, plan string, resume *string) (string, error) {
	m.started = append(m.started, mockStartCall{workDir: workDir, plan: plan, resume: resume})
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
	nextPRInfo         *vcs.PRInfo
	nextPRStatus       *vcs.PRStatus
	nextCheckResults   []vcs.CheckResult
	nextReviewComments []vcs.ReviewComment
	checkResultsErr    error
	reviewCommentsErr  error
}

func newMockVCSProvider() *mockVCSProvider {
	return &mockVCSProvider{
		nextPRInfo:   &vcs.PRInfo{Number: 42, URL: "https://github.com/owner/repo/pull/42"},
		nextPRStatus: &vcs.PRStatus{State: vcs.PRStateOpen},
	}
}

func (m *mockVCSProvider) CreateDraftPR(_ context.Context, opts vcs.CreatePROpts) (*vcs.PRInfo, error) {
	m.createPRCalls = append(m.createPRCalls, opts)
	return m.nextPRInfo, nil
}

func (m *mockVCSProvider) GetPRStatus(_ context.Context, _ string, _ int) (*vcs.PRStatus, error) {
	if m.nextPRStatus != nil {
		return m.nextPRStatus, nil
	}
	return &vcs.PRStatus{State: vcs.PRStateOpen}, nil
}

func (m *mockVCSProvider) GetCheckResults(_ context.Context, _ string, _ int) ([]vcs.CheckResult, error) {
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
	return m.nextReviewComments, m.reviewCommentsErr
}

func (m *mockVCSProvider) ListOpenPRs(_ context.Context, _ string) ([]vcs.PRSummary, error) {
	return nil, nil
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

	lc := NewLifecycle(sessions, repos, wt, cr, newMockVCSProvider(), logger)

	if err := lc.StartSession(ctx, "sess-1", "", false); err != nil {
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

	lc := NewLifecycle(sessions, repos, wt, cr, newMockVCSProvider(), logger)

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

	claudeID := "claude-123"
	cr.running[claudeID] = true

	sessions.sessions["sess-1"] = &models.Session{
		ID:              "sess-1",
		RepoID:          "repo-1",
		State:           machine.ImplementingPlan,
		WorktreePath:    "/tmp/worktrees/test-repo/test",
		ClaudeSessionID: &claudeID,
	}

	lc := NewLifecycle(sessions, repos, wt, cr, newMockVCSProvider(), logger)

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

	lc := NewLifecycle(sessions, repos, wt, cr, newMockVCSProvider(), logger)

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

	lc := NewLifecycle(sessions, repos, wt, cr, newMockVCSProvider(), logger)

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

	lc := NewLifecycle(sessions, repos, wt, cr, newMockVCSProvider(), logger)

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

	lc := NewLifecycle(sessions, repos, wt, cr, vp, logger)

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

	lc := NewLifecycle(sessions, repos, wt, cr, vp, logger)

	err := lc.SubmitPR(ctx, "sess-1")
	if err == nil {
		t.Fatal("expected error for wrong state")
	}
}
