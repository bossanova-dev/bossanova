package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/tmux"
)

// Compile-time interface assertions for test mocks.
var (
	_ db.SessionStore        = (*mockSessionStore)(nil)
	_ db.RepoStore           = (*mockRepoStore)(nil)
	_ db.AgentChatStore      = (*mockAgentChatStore)(nil)
	_ gitpkg.WorktreeManager = (*mockWorktreeManager)(nil)
	_ agent.AgentRunner      = (*mockAgentRunner)(nil)
	_ agent.AgentDispatcher  = (*mockAgentRunner)(nil)
	_ vcs.Provider           = (*mockVCSProvider)(nil)
)

// --- Mock SessionStore ---

type mockSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*models.Session

	updateHook func(id string, params db.UpdateSessionParams) error
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

func (m *mockSessionStore) List(_ context.Context, repoID string) ([]*models.Session, error) {
	var result []*models.Session
	for _, s := range m.sessions {
		if repoID != "" && s.RepoID != repoID {
			continue
		}
		result = append(result, s)
	}
	return result, nil
}

func (m *mockSessionStore) ListActive(_ context.Context, repoID string) ([]*models.Session, error) {
	return m.List(context.Background(), repoID)
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

func (m *mockSessionStore) ListByRepoAndPR(_ context.Context, repoID string, prNumber int) ([]*db.SessionWithRepo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*db.SessionWithRepo
	for _, s := range m.sessions {
		if s.RepoID == repoID && s.PRNumber != nil && *s.PRNumber == prNumber && s.ArchivedAt == nil {
			result = append(result, &db.SessionWithRepo{Session: s})
		}
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
	if m.updateHook != nil {
		if err := m.updateHook(id, params); err != nil {
			return nil, err
		}
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
	if params.AgentSessionID != nil {
		s.AgentSessionID = *params.AgentSessionID
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
	if params.LastObservedReviewState != nil {
		s.LastObservedReviewState = *params.LastObservedReviewState
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

func (m *mockSessionStore) UpdateRepairDiagnostics(_ context.Context, params db.UpdateRepairDiagnosticsParams) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[params.SessionID]
	if !ok {
		return nil
	}
	s.LastRepairStartedAt = &params.StartedAt
	s.LastRepairRunnerError = params.RunnerError
	s.LastRepairExitError = params.ExitError
	s.LastRepairAttemptCount++
	return nil
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

func (m *mockRepoStore) GetByOrigin(_ context.Context, originURL string) (*models.Repo, error) {
	_, targetWebURL, targetOK := vcs.RepoWebLink(originURL)
	for _, r := range m.repos {
		if r.OriginURL == originURL {
			return r, nil
		}
		if targetOK {
			_, repoWebURL, repoOK := vcs.RepoWebLink(r.OriginURL)
			if repoOK && repoWebURL == targetWebURL {
				return r, nil
			}
		}
	}
	return nil, fmt.Errorf("repo origin %s not found", originURL)
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

// --- Mock AgentChatStore ---

// mockAgentChatStore satisfies db.AgentChatStore for lifecycle tests. By
// default Create / UpdateTmuxSessionName / DeleteByAgentSessionID succeed and
// record their parameters so tests can assert on them. Setting createErr,
// updateTmuxNameErr, etc. forces the corresponding method to return that
// error instead — used by failure-mode tests for the cron tmux path.
type mockAgentChatStore struct {
	mu                     sync.Mutex
	createCalls            []db.CreateAgentChatParams
	tmuxNameUpdates        []tmuxNameUpdate
	deletedAgentSessionIDs []string
	markStartFailedCalls   []markStartFailedCall
	chatsBySession         map[string][]*models.AgentChat // returned by ListBySession when set
	chatsWithTmux          []*models.AgentChat            // returned by ListWithTmuxSession when set
	createErr              error
	updateTmuxNameErr      error
	deleteErr              error
}

type markStartFailedCall struct {
	agentSessionID string
	reason         string
}

type tmuxNameUpdate struct {
	agentSessionID string
	name           *string
}

func (m *mockAgentChatStore) Create(_ context.Context, params db.CreateAgentChatParams) (*models.AgentChat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCalls = append(m.createCalls, params)
	if m.createErr != nil {
		return nil, m.createErr
	}
	return &models.AgentChat{
		ID:                "chat-" + params.AgentSessionID,
		SessionID:         params.SessionID,
		AgentSessionID:    params.AgentSessionID,
		ProviderSessionID: params.ProviderSessionID,
		Title:             params.Title,
	}, nil
}

func (m *mockAgentChatStore) GetByAgentSessionID(_ context.Context, agentSessionID string) (*models.AgentChat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, chats := range m.chatsBySession {
		for _, chat := range chats {
			if chat.AgentSessionID == agentSessionID {
				return chat, nil
			}
		}
	}
	for _, chat := range m.chatsWithTmux {
		if chat.AgentSessionID == agentSessionID {
			return chat, nil
		}
	}
	return nil, db.ErrAgentChatNotFound
}

func (m *mockAgentChatStore) ListBySession(_ context.Context, sessionID string) ([]*models.AgentChat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.chatsBySession == nil {
		return nil, nil
	}
	return m.chatsBySession[sessionID], nil
}

func (m *mockAgentChatStore) ListBySessions(_ context.Context, sessionIDs []string) (map[string][]*models.AgentChat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	chatsBySession := make(map[string][]*models.AgentChat, len(sessionIDs))
	if m.chatsBySession == nil {
		return chatsBySession, nil
	}
	for _, id := range sessionIDs {
		chatsBySession[id] = m.chatsBySession[id]
	}
	return chatsBySession, nil
}

func (m *mockAgentChatStore) UpdateTitle(_ context.Context, _, _ string) error {
	return nil
}

func (m *mockAgentChatStore) UpdateTitleByAgentSessionID(_ context.Context, _, _ string) error {
	return nil
}

func (m *mockAgentChatStore) UpdateTmuxSessionName(_ context.Context, agentSessionID string, name *string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tmuxNameUpdates = append(m.tmuxNameUpdates, tmuxNameUpdate{agentSessionID: agentSessionID, name: name})
	for _, chats := range m.chatsBySession {
		for _, chat := range chats {
			if chat.AgentSessionID == agentSessionID {
				chat.TmuxSessionName = name
			}
		}
	}
	for _, chat := range m.chatsWithTmux {
		if chat.AgentSessionID == agentSessionID {
			chat.TmuxSessionName = name
		}
	}
	return m.updateTmuxNameErr
}

func (m *mockAgentChatStore) UpdateProviderSessionID(_ context.Context, _ string, _ *string) error {
	return nil
}

func (m *mockAgentChatStore) MarkStartFailed(_ context.Context, agentSessionID, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.markStartFailedCalls = append(m.markStartFailedCalls, markStartFailedCall{agentSessionID: agentSessionID, reason: reason})
	for _, chats := range m.chatsBySession {
		for _, chat := range chats {
			if chat.AgentSessionID == agentSessionID {
				chat.TmuxSessionName = nil
				chat.StartError = &reason
			}
		}
	}
	for _, chat := range m.chatsWithTmux {
		if chat.AgentSessionID == agentSessionID {
			chat.TmuxSessionName = nil
			chat.StartError = &reason
		}
	}
	return nil
}

func (m *mockAgentChatStore) DeleteByAgentSessionID(_ context.Context, agentSessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedAgentSessionIDs = append(m.deletedAgentSessionIDs, agentSessionID)
	if m.deleteErr != nil {
		return m.deleteErr
	}
	return nil
}

func (m *mockAgentChatStore) ListWithTmuxSession(_ context.Context) ([]*models.AgentChat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chatsWithTmux, nil
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
	emptyCommitCalls            int
	verifyCurrentBranchErr      error
	verifiedBranches            []string
	verifyPushedErr             error
	verifyPushedResult          *gitpkg.BranchVerification
	verifyPushedCalls           []verifyPushedCall
	latestCommitSubject         string
	latestCommitSubjectErr      error
	branchDebugSnapshot         *gitpkg.BranchDebugSnapshot
	branchDebugSnapshotErr      error
	branchDebugSnapshotCalls    []branchDebugSnapshotCall
	originURL                   string // returned by DetectOriginURL
	statusOut                   string // returned by Status
	statusErr                   error  // if set, Status returns this error
	worktreePath                string // override for Create's returned WorktreePath; empty uses the historical fixed path
	fetchedBases                []string
	fetchBaseErr                error
	isAncestorFn                func(localPath, ref, target string) (bool, error)
}

type branchDebugSnapshotCall struct {
	worktreePath string
	branch       string
	baseBranch   string
}

type verifyPushedCall struct {
	worktreePath string
	branch       string
	baseBranch   string
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

func (m *mockWorktreeManager) PurgeWorktree(_ context.Context, _, _, _, _ string) {}

func (m *mockWorktreeManager) Resurrect(_ context.Context, opts gitpkg.ResurrectOpts) error {
	m.resurrected = append(m.resurrected, opts)
	return nil
}

func (m *mockWorktreeManager) EmptyCommit(_ context.Context, worktreePath, _ string) error {
	m.emptyCommitCalls++
	m.emptyCommits = append(m.emptyCommits, worktreePath)
	return nil
}

func (m *mockWorktreeManager) VerifyCurrentBranch(_ context.Context, _ string, branch string) error {
	m.verifiedBranches = append(m.verifiedBranches, branch)
	return m.verifyCurrentBranchErr
}

func (m *mockWorktreeManager) Push(_ context.Context, _ string, branch string) error {
	m.pushed = append(m.pushed, branch)
	return m.pushErr
}

func (m *mockWorktreeManager) PushWithLease(_ context.Context, _ string, branch, expectedRemoteSHA string) (string, error) {
	if expectedRemoteSHA == "" {
		return "", errors.New("expected remote SHA is required")
	}
	if m.pushErr != nil {
		return "", m.pushErr
	}
	m.pushed = append(m.pushed, branch)
	return "pushed-head-sha", nil
}

func (m *mockWorktreeManager) VerifyPushedBranchAheadOfBase(_ context.Context, worktreePath, branch, baseBranch string) (*gitpkg.BranchVerification, error) {
	m.verifyPushedCalls = append(m.verifyPushedCalls, verifyPushedCall{
		worktreePath: worktreePath,
		branch:       branch,
		baseBranch:   baseBranch,
	})
	if m.verifyPushedErr != nil {
		return nil, m.verifyPushedErr
	}
	if m.verifyPushedResult != nil {
		return m.verifyPushedResult, nil
	}
	return &gitpkg.BranchVerification{
		HeadSHA:       "head-sha",
		BaseSHA:       "base-sha",
		RemoteHeadSHA: "head-sha",
		AheadCount:    1,
	}, nil
}

func (m *mockWorktreeManager) Status(_ context.Context, _ string) (string, error) {
	return m.statusOut, m.statusErr
}

func (m *mockWorktreeManager) LatestCommitSubject(_ context.Context, _ string) (string, error) {
	return m.latestCommitSubject, m.latestCommitSubjectErr
}

func (m *mockWorktreeManager) BranchDebugSnapshot(_ context.Context, worktreePath, branch, baseBranch string) (*gitpkg.BranchDebugSnapshot, error) {
	m.branchDebugSnapshotCalls = append(m.branchDebugSnapshotCalls, branchDebugSnapshotCall{
		worktreePath: worktreePath,
		branch:       branch,
		baseBranch:   baseBranch,
	})
	if m.branchDebugSnapshotErr != nil {
		return nil, m.branchDebugSnapshotErr
	}
	if m.branchDebugSnapshot != nil {
		return m.branchDebugSnapshot, nil
	}
	return &gitpkg.BranchDebugSnapshot{}, nil
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

func (m *mockWorktreeManager) IsAncestor(_ context.Context, localPath, ref, target string) (bool, error) {
	if m.isAncestorFn != nil {
		return m.isAncestorFn(localPath, ref, target)
	}
	return true, nil
}

func (m *mockWorktreeManager) MergeLocalBranch(_ context.Context, _, _, _, _ string) error {
	return nil
}

func (m *mockWorktreeManager) FetchBase(_ context.Context, _, base string) error {
	m.fetchedBases = append(m.fetchedBases, base)
	return m.fetchBaseErr
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

// --- Mock AgentRunner ---

type mockAgentRunner struct {
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

func newMockAgentRunner() *mockAgentRunner {
	return &mockAgentRunner{
		running: make(map[string]bool),
		nextID:  "claude-123",
	}
}

func (m *mockAgentRunner) Start(_ context.Context, workDir, plan string, resume *string, _ string) (string, error) {
	m.started = append(m.started, mockStartCall{workDir: workDir, plan: plan, resume: resume})
	if m.startErr != nil {
		return "", m.startErr
	}
	id := m.nextID
	m.running[id] = true
	return id, nil
}

func (m *mockAgentRunner) Stop(sessionID string) error {
	m.stopped = append(m.stopped, sessionID)
	delete(m.running, sessionID)
	return nil
}

func (m *mockAgentRunner) IsRunning(sessionID string) bool {
	return m.running[sessionID]
}

func (m *mockAgentRunner) ExitError(_ string) error {
	return nil
}

func (m *mockAgentRunner) Subscribe(_ context.Context, _ string) (<-chan agent.OutputLine, error) {
	ch := make(chan agent.OutputLine)
	close(ch)
	return ch, nil
}

func (m *mockAgentRunner) History(_ string) []agent.OutputLine {
	return nil
}

// StartByAgent forwards to Start so existing test assertions still fire.
// The test fakes don't need to inspect the agent name — by-agent routing
// is exercised by the dispatcher tests in services/bossd/internal/agent.
func (m *mockAgentRunner) StartByAgent(ctx context.Context, _, workDir, plan string, resume *string, agentSessionID string) (string, error) {
	return m.Start(ctx, workDir, plan, resume, agentSessionID)
}

// StopByAgent forwards to Stop, ignoring the agent name (see StartByAgent).
func (m *mockAgentRunner) StopByAgent(_, agentSessionID string) error {
	return m.Stop(agentSessionID)
}

// IsRunningByAgent forwards to IsRunning, ignoring the agent name (see StartByAgent).
func (m *mockAgentRunner) IsRunningByAgent(_, agentSessionID string) bool {
	return m.IsRunning(agentSessionID)
}

// --- Mock VCS Provider ---

type mockVCSProvider struct {
	createPRCalls      []vcs.CreatePROpts
	listOpenPRCalls    []string
	updatePRTitleCalls []updatePRTitleCall
	markReadyCalls     []int
	mergePRCalls       []int
	nextPRInfo         *vcs.PRInfo
	nextPRStatus       *vcs.PRStatus
	nextCheckResults   []vcs.CheckResult
	nextReviewComments []vcs.ReviewComment
	nextOpenPRs        []vcs.PRSummary
	allowedStrategies  []string
	createPRErr        error
	listOpenPRErr      error
	updatePRTitleErr   error
	checkResultsErr    error
	reviewCommentsErr  error
	mergePRErr         error

	getCheckResultsCalls   int
	getReviewCommentsCalls int
	getPRStatusPRNumbers   []int
}

type updatePRTitleCall struct {
	repoPath string
	prID     int
	title    string
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

func (m *mockVCSProvider) GetPRStatus(_ context.Context, _ string, prNumber int) (*vcs.PRStatus, error) {
	m.getPRStatusPRNumbers = append(m.getPRStatusPRNumbers, prNumber)
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

func (m *mockVCSProvider) ListOpenPRs(_ context.Context, repoPath string) ([]vcs.PRSummary, error) {
	m.listOpenPRCalls = append(m.listOpenPRCalls, repoPath)
	if m.listOpenPRErr != nil {
		return nil, m.listOpenPRErr
	}
	return m.nextOpenPRs, nil
}

func (m *mockVCSProvider) ListClosedPRs(_ context.Context, _ string) ([]vcs.PRSummary, error) {
	return nil, nil
}

func (m *mockVCSProvider) UpdatePRTitle(_ context.Context, repoPath string, prID int, title string) error {
	m.updatePRTitleCalls = append(m.updatePRTitleCalls, updatePRTitleCall{
		repoPath: repoPath,
		prID:     prID,
		title:    title,
	})
	return m.updatePRTitleErr
}

func (m *mockVCSProvider) MergePR(_ context.Context, _ string, prID int, _ string) error {
	m.mergePRCalls = append(m.mergePRCalls, prID)
	return m.mergePRErr
}

func (m *mockVCSProvider) GetPRMergeCommit(_ context.Context, _ string, prID int) (string, error) {
	return fmt.Sprintf("mock-merge-%d", prID), nil
}

func (m *mockVCSProvider) GetAllowedMergeStrategies(_ context.Context, _ string) ([]string, error) {
	if m.allowedStrategies != nil {
		return m.allowedStrategies, nil
	}
	return []string{"merge", "squash", "rebase"}, nil
}

// --- Tests ---

func TestStartSession(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
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
	if sess.AgentSessionID == nil || *sess.AgentSessionID != "claude-123" {
		t.Errorf("claude session id = %v, want claude-123", sess.AgentSessionID)
	}
}

func TestStartSession_DraftPRNotAttemptedWhenBranchHasNoCommitsOverBase(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		verifyPushedErr: fmt.Errorf("head branch test-session has no commits over base origin/main (base SHA base123, head SHA head123)"),
	}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		OriginURL:         "owner/repo",
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

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if len(vp.createPRCalls) != 0 {
		t.Fatalf("CreateDraftPR calls = %d, want 0", len(vp.createPRCalls))
	}
	if len(wt.verifyPushedCalls) != 1 {
		t.Fatalf("VerifyPushedBranchAheadOfBase calls = %d, want 1", len(wt.verifyPushedCalls))
	}

	reason := sessions.sessions["sess-1"].BlockedReason
	if reason == nil {
		t.Fatal("BlockedReason = nil, want branch invariant reason")
	}
	for _, want := range []string{
		"head branch test-session has no commits over base origin/main",
		"base SHA base123",
		"head SHA head123",
	} {
		if !strings.Contains(*reason, want) {
			t.Fatalf("BlockedReason = %q, want %q", *reason, want)
		}
	}
}

func TestStartSession_NoCommitsBetweenProviderErrorIncludesBranchSHAs(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		verifyPushedResult: &gitpkg.BranchVerification{
			HeadSHA:       "head456",
			BaseSHA:       "base123",
			RemoteHeadSHA: "head456",
			AheadCount:    1,
		},
	}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	vp.createPRErr = fmt.Errorf("gh pr create: GraphQL: No commits between main and test-session (createPullRequest)")
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		OriginURL:         "owner/repo",
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

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if len(vp.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(vp.createPRCalls))
	}
	reason := sessions.sessions["sess-1"].BlockedReason
	if reason == nil {
		t.Fatal("BlockedReason = nil, want No commits between reason")
	}
	for _, want := range []string{
		"head branch test-session has no commits over base origin/main",
		"base SHA base123",
		"head SHA head456",
		"No commits between main and test-session",
	} {
		if !strings.Contains(*reason, want) {
			t.Fatalf("BlockedReason = %q, want %q", *reason, want)
		}
	}
}

func TestStartSession_DuplicatePRErrorAttachesExistingPRAndClearsBlockedReason(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		OriginURL:         "git@github.com:owner/repo.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	reason := "draft PR creation failed: create draft PR: " + vcs.ErrPRAlreadyExists.Error()
	sessions.sessions["sess-1"] = &models.Session{
		ID:            "sess-1",
		RepoID:        "repo-1",
		Title:         "Test Session",
		Plan:          "Do something",
		BaseBranch:    "main",
		State:         machine.CreatingWorktree,
		BlockedReason: &reason,
	}
	vp.createPRErr = vcs.ErrPRAlreadyExists
	vp.nextOpenPRs = []vcs.PRSummary{
		{
			Number:     77,
			Title:      "Test Session",
			HeadBranch: "test-session",
			State:      vcs.PRStateOpen,
		},
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	sess := sessions.sessions["sess-1"]
	if sess.PRNumber == nil || *sess.PRNumber != 77 {
		t.Fatalf("PRNumber = %v, want 77", sess.PRNumber)
	}
	if sess.PRURL == nil || *sess.PRURL != "https://github.com/owner/repo/pull/77" {
		t.Fatalf("PRURL = %v, want https://github.com/owner/repo/pull/77", sess.PRURL)
	}
	if sess.BlockedReason != nil {
		t.Fatalf("BlockedReason = %q, want nil", *sess.BlockedReason)
	}
	if len(vp.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(vp.createPRCalls))
	}
	if len(vp.listOpenPRCalls) != 1 || vp.listOpenPRCalls[0] != "git@github.com:owner/repo.git" {
		t.Fatalf("ListOpenPRs calls = %v, want [git@github.com:owner/repo.git]", vp.listOpenPRCalls)
	}
}

func TestStartSession_CronPRTitleNormalizesSessionTitleWhenLatestCommitSubjectFails(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := new(mockWorktreeManager)
	wt.latestCommitSubjectErr = errors.New("git log failed")
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	tx := tmux.NewClient(tmux.WithCommandFactory(newFakeTmux().factory))

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		OriginURL:         "git@github.com:owner/repo.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "test(githubapp): [#493] cover malformed Link header without brackets",
		Plan:       "Do something",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "claude",
	}

	lc := NewLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, vp, logger)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": newFakeAgent()})
	lc.SetAgentLogsDir(t.TempDir())

	cronJobID := "cron-42"
	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{CronJobID: cronJobID}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if len(vp.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(vp.createPRCalls))
	}
	if got, want := vp.createPRCalls[0].Title, "Cover malformed Link header without brackets"; got != want {
		t.Fatalf("PR title = %q, want %q", got, want)
	}
}

func TestStartSession_ExistingBranchNotOnRemote_FallsBackToCreate(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		createFromExistingBranchErr: fmt.Errorf("fetch branch: git fetch origin dave/fre-1176: exit status 128: fatal: couldn't find remote ref dave/fre-1176"),
	}
	cr := newMockAgentRunner()
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
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	agentSessionID := "claude-123"
	cr.running[agentSessionID] = true

	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		RepoID:         "repo-1",
		State:          machine.ImplementingPlan,
		AgentSessionID: &agentSessionID,
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
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo",
	}

	agentSessionID := "claude-123"
	cr.running[agentSessionID] = true

	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		RepoID:         "repo-1",
		State:          machine.ImplementingPlan,
		WorktreePath:   "/tmp/worktrees/test-repo/test",
		AgentSessionID: &agentSessionID,
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
	cr := newMockAgentRunner()
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
	sess.AgentSessionID = &oldClaudeID

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
	cr := newMockAgentRunner()
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
	cr := newMockAgentRunner()
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
	cr := newMockAgentRunner()
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

func TestSubmitPR_ClearsOnlyDraftPRBlockedReasonAfterCreate(t *testing.T) {
	t.Run("draft PR blocked reason is cleared", func(t *testing.T) {
		ctx := context.Background()
		sessions := newMockSessionStore()
		repos := newMockRepoStore()
		wt := &mockWorktreeManager{}
		cr := newMockAgentRunner()
		vp := newMockVCSProvider()
		logger := zerolog.Nop()

		repos.repos["repo-1"] = &models.Repo{
			ID:        "repo-1",
			LocalPath: "/tmp/repo",
			OriginURL: "owner/repo",
		}
		reason := "draft PR creation failed: create draft PR: GraphQL: Head sha can't be blank"
		sessions.sessions["sess-1"] = &models.Session{
			ID:            "sess-1",
			RepoID:        "repo-1",
			Title:         "Test Session",
			Plan:          "Do something",
			WorktreePath:  "/tmp/worktrees/test-repo/test-session",
			BranchName:    "test-session",
			BaseBranch:    "main",
			State:         machine.ImplementingPlan,
			BlockedReason: &reason,
		}

		lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

		if err := lc.SubmitPR(ctx, "sess-1"); err != nil {
			t.Fatalf("SubmitPR: %v", err)
		}

		sess := sessions.sessions["sess-1"]
		if sess.PRNumber == nil || *sess.PRNumber != 42 {
			t.Fatalf("PRNumber = %v, want 42", sess.PRNumber)
		}
		if sess.PRURL == nil || *sess.PRURL != "https://github.com/owner/repo/pull/42" {
			t.Fatalf("PRURL = %v, want https://github.com/owner/repo/pull/42", sess.PRURL)
		}
		if sess.BlockedReason != nil {
			t.Fatalf("BlockedReason = %q, want nil", *sess.BlockedReason)
		}
	})

	t.Run("non-draft blocked reason is preserved", func(t *testing.T) {
		ctx := context.Background()
		sessions := newMockSessionStore()
		repos := newMockRepoStore()
		wt := &mockWorktreeManager{}
		cr := newMockAgentRunner()
		vp := newMockVCSProvider()
		logger := zerolog.Nop()

		repos.repos["repo-1"] = &models.Repo{
			ID:        "repo-1",
			LocalPath: "/tmp/repo",
			OriginURL: "owner/repo",
		}
		reason := "manual hold"
		sessions.sessions["sess-1"] = &models.Session{
			ID:            "sess-1",
			RepoID:        "repo-1",
			Title:         "Test Session",
			Plan:          "Do something",
			WorktreePath:  "/tmp/worktrees/test-repo/test-session",
			BranchName:    "test-session",
			BaseBranch:    "main",
			State:         machine.ImplementingPlan,
			BlockedReason: &reason,
		}

		lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)

		if err := lc.SubmitPR(ctx, "sess-1"); err != nil {
			t.Fatalf("SubmitPR: %v", err)
		}

		sess := sessions.sessions["sess-1"]
		if sess.BlockedReason == nil || *sess.BlockedReason != reason {
			t.Fatalf("BlockedReason = %v, want %q", sess.BlockedReason, reason)
		}
	})
}

func TestSubmitPR_ExistingPRStillPushesImplementationCommits(t *testing.T) {
	// When a draft PR was already created up-front (e.g. via createDraftPR
	// during StartSession), SubmitPR must still push so that any commits
	// Claude made on top of the placeholder empty commit reach the remote.
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
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
	cr := newMockAgentRunner()
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
	cr := newMockAgentRunner()
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
	cr := newMockAgentRunner()
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
	cr := newMockAgentRunner()
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
	cr := newMockAgentRunner()
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
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo",
	}

	agentSessionID := "claude-123"
	cr.running[agentSessionID] = true

	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		RepoID:         "repo-1",
		State:          machine.ImplementingPlan,
		WorktreePath:   "/tmp/repo", // same as repo.LocalPath → quick chat
		AgentSessionID: &agentSessionID,
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
	cr := newMockAgentRunner()
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo",
	}

	archTime := models.Session{}.CreatedAt
	oldClaudeID := "claude-old"
	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		RepoID:         "repo-1",
		Title:          "Quick chat",
		WorktreePath:   "/tmp/repo", // same as repo.LocalPath → quick chat
		BranchName:     "",
		State:          machine.ImplementingPlan,
		ArchivedAt:     &archTime,
		AgentSessionID: &oldClaudeID,
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
	cr := newMockAgentRunner()
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
	cr := newMockAgentRunner()
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
	cr := newMockAgentRunner()
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
	cr := newMockAgentRunner()
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
	cr := newMockAgentRunner()
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
	cr := newMockAgentRunner()
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

func TestStartSession_CreateDraftPRFailureStoresBlockedReason(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	agentChats := &mockAgentChatStore{}
	worktrees := &mockWorktreeManager{}
	agentRunner := newMockAgentRunner()
	provider := newMockVCSProvider()
	lifecycle := NewLifecycle(sessions, repos, agentChats, nil, worktrees, agentRunner, nil, provider, zerolog.Nop())

	repo := &models.Repo{
		ID:              "repo-1",
		DisplayName:     "repo",
		LocalPath:       "/tmp/repo",
		OriginURL:       "git@github.com:owner/repo.git",
		WorktreeBaseDir: "/tmp/worktrees",
	}
	repos.repos[repo.ID] = repo

	session := &models.Session{
		ID:         "sess-1",
		RepoID:     repo.ID,
		Title:      "Open a PR",
		Plan:       "Do useful work",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}
	sessions.sessions[session.ID] = session

	provider.createPRErr = errors.New("gh pr create: GraphQL: Head sha can't be blank")

	if err := lifecycle.StartSession(ctx, session.ID, StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession returned error: %v", err)
	}

	updated := sessions.sessions[session.ID]
	if updated.PRNumber != nil {
		t.Fatalf("PRNumber = %v, want nil", *updated.PRNumber)
	}
	if updated.PRURL != nil {
		t.Fatalf("PRURL = %v, want nil", *updated.PRURL)
	}
	if updated.BlockedReason == nil {
		t.Fatal("BlockedReason = nil, want draft PR failure")
	}
	wantReason := "draft PR creation failed: create draft PR: gh pr create: GraphQL: Head sha can't be blank"
	if !strings.Contains(*updated.BlockedReason, wantReason) {
		t.Fatalf("BlockedReason = %q, want to contain %q", *updated.BlockedReason, wantReason)
	}
	if updated.State != machine.ImplementingPlan {
		t.Fatalf("State = %v, want ImplementingPlan", updated.State)
	}
}

func TestStartSession_CreateDraftPRFailureLogsBranchDiagnostics(t *testing.T) {
	const prFailure = "gh pr create: GraphQL: Head sha can't be blank"

	findLogEvent := func(t *testing.T, logs string, message string) map[string]any {
		t.Helper()
		for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				t.Fatalf("decode log line %q: %v", line, err)
			}
			if event["message"] == message {
				return event
			}
		}
		t.Fatalf("log event %q not found in:\n%s", message, logs)
		return nil
	}

	startWithDraftPRError := func(t *testing.T, worktrees *mockWorktreeManager, logger zerolog.Logger) (*mockSessionStore, error) {
		t.Helper()
		ctx := context.Background()
		sessions := newMockSessionStore()
		repos := newMockRepoStore()
		agentChats := &mockAgentChatStore{}
		agentRunner := newMockAgentRunner()
		provider := newMockVCSProvider()
		lifecycle := NewLifecycle(sessions, repos, agentChats, nil, worktrees, agentRunner, nil, provider, logger)

		repo := &models.Repo{
			ID:              "repo-1",
			DisplayName:     "repo",
			LocalPath:       "/tmp/repo",
			OriginURL:       "git@github.com:owner/repo.git",
			WorktreeBaseDir: "/tmp/worktrees",
		}
		repos.repos[repo.ID] = repo
		sessions.sessions["sess-1"] = &models.Session{
			ID:         "sess-1",
			RepoID:     repo.ID,
			Title:      "Open a PR",
			Plan:       "Do useful work",
			BaseBranch: "main",
			State:      machine.CreatingWorktree,
		}
		provider.createPRErr = errors.New(prFailure)

		err := lifecycle.StartSession(ctx, "sess-1", StartSessionOpts{})
		return sessions, err
	}

	assertBlockedReason := func(t *testing.T, sessions *mockSessionStore) {
		t.Helper()
		updated := sessions.sessions["sess-1"]
		if updated.BlockedReason == nil {
			t.Fatal("BlockedReason = nil, want draft PR failure")
		}
		wantReason := "draft PR creation failed: create draft PR: " + prFailure
		if !strings.Contains(*updated.BlockedReason, wantReason) {
			t.Fatalf("BlockedReason = %q, want to contain %q", *updated.BlockedReason, wantReason)
		}
	}

	assertSnapshotCall := func(t *testing.T, worktrees *mockWorktreeManager, wantPath, wantBranch, wantBase string) {
		t.Helper()
		if len(worktrees.branchDebugSnapshotCalls) != 1 {
			t.Fatalf("BranchDebugSnapshot calls = %d, want 1", len(worktrees.branchDebugSnapshotCalls))
		}
		call := worktrees.branchDebugSnapshotCalls[0]
		if call.worktreePath != wantPath || call.branch != wantBranch || call.baseBranch != wantBase {
			t.Fatalf("BranchDebugSnapshot call = %+v, want path=%q branch=%q base=%q", call, wantPath, wantBranch, wantBase)
		}
	}

	t.Run("logs snapshot fields", func(t *testing.T) {
		var logs bytes.Buffer
		worktrees := &mockWorktreeManager{
			worktreePath: "/tmp/worktrees/repo/test-session",
			branchDebugSnapshot: &gitpkg.BranchDebugSnapshot{
				CurrentBranch: "test-session",
				HeadSHA:       "abc123",
				RemoteHeadSHA: "def456",
				AheadBehind:   "0\t2",
			},
		}

		sessions, err := startWithDraftPRError(t, worktrees, zerolog.New(&logs))
		if err != nil {
			t.Fatalf("StartSession returned error: %v", err)
		}
		assertBlockedReason(t, sessions)
		assertSnapshotCall(t, worktrees, "/tmp/worktrees/repo/test-session", "test-session", "main")

		event := findLogEvent(t, logs.String(), "draft PR creation failed with branch debug snapshot")
		for field, want := range map[string]string{
			"session":         "sess-1",
			"branch":          "test-session",
			"base":            "main",
			"current_branch":  "test-session",
			"head_sha":        "abc123",
			"remote_head_sha": "def456",
			"ahead_behind":    "0\t2",
		} {
			if got := event[field]; got != want {
				t.Fatalf("log field %s = %v, want %q", field, got, want)
			}
		}
		if got := fmt.Sprint(event["error"]); !strings.Contains(got, "create draft PR: "+prFailure) {
			t.Fatalf("log error = %q, want original draft PR error", got)
		}
	})

	t.Run("logs original draft PR error when snapshot collection fails", func(t *testing.T) {
		var logs bytes.Buffer
		worktrees := &mockWorktreeManager{
			worktreePath:           "/tmp/worktrees/repo/test-session",
			branchDebugSnapshotErr: errors.New("snapshot failed"),
		}

		sessions, err := startWithDraftPRError(t, worktrees, zerolog.New(&logs))
		if err != nil {
			t.Fatalf("StartSession returned error: %v", err)
		}
		assertBlockedReason(t, sessions)
		assertSnapshotCall(t, worktrees, "/tmp/worktrees/repo/test-session", "test-session", "main")

		event := findLogEvent(t, logs.String(), "failed to collect branch debug snapshot after draft PR failure")
		for field, want := range map[string]string{
			"session":        "sess-1",
			"branch":         "test-session",
			"draft_pr_error": "create draft PR: " + prFailure,
		} {
			if got := event[field]; got != want {
				t.Fatalf("log field %s = %v, want %q", field, got, want)
			}
		}
		if got := fmt.Sprint(event["error"]); got != "snapshot failed" {
			t.Fatalf("log error = %q, want snapshot failure", got)
		}
	})
}

func TestStartSession_BlocksDraftPRWhenWorktreeBranchMismatches(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		verifyCurrentBranchErr: errors.New(`worktree is on branch "production", expected "fix-camera-crash"`),
	}
	cr := newMockAgentRunner()
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
		Title:      "Fix camera crash",
		BranchName: "fix-camera-crash",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, logger)
	err := lc.StartSession(ctx, "sess-1", StartSessionOpts{})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if wt.emptyCommitCalls != 0 {
		t.Fatalf("EmptyCommit calls = %d, want 0", wt.emptyCommitCalls)
	}
	if len(vp.createPRCalls) != 0 {
		t.Fatalf("CreateDraftPR calls = %d, want 0", len(vp.createPRCalls))
	}
	sess := sessions.sessions["sess-1"]
	if sess.BlockedReason == nil {
		t.Fatal("BlockedReason = nil, want branch mismatch reason")
	}
	if !strings.Contains(*sess.BlockedReason, `worktree is on branch "production", expected "fix-camera-crash"`) {
		t.Fatalf("BlockedReason = %q, want branch mismatch", *sess.BlockedReason)
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
	cr := newMockAgentRunner()
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
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
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
		AgentName:  "claude",
	}

	lc := NewLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), logger)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": newFakeAgent()})
	lc.SetAgentLogsDir(t.TempDir())

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

// TestStartSession_HookToken_CallsConfigureFinalizeHook verifies that when
// StartSessionOpts.HookToken is set, StartSession delegates Stop-hook
// configuration to the agent plugin via ConfigureFinalizeHook RPC,
// passing the worktree path, session ID, token, and port. The actual file
// write happens inside the plugin (tested there); lifecycle only verifies
// the RPC contract. Non-cron sessions (empty HookToken) skip this path.
func TestStartSession_HookToken_CallsConfigureFinalizeHook(t *testing.T) {
	ctx := context.Background()

	worktreeDir := t.TempDir()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{worktreePath: worktreeDir}
	cr := newMockAgentRunner()
	tmuxFake := newFakeTmux()
	tx := tmux.NewClient(tmux.WithCommandFactory(tmuxFake.factory))
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
		AgentName:  "claude",
	}

	fa := newFakeAgent()
	lc := NewLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), logger)
	lc.SetHookPort(45678)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": fa})
	lc.SetAgentLogsDir(t.TempDir())

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		DeferPR:   true,
		CronJobID: "cron-42",
		HookToken: "secret-token-123",
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	// Verify ConfigureFinalizeHook RPC was called with the correct args.
	req := fa.LastConfigureHookReq
	if req == nil {
		t.Fatal("ConfigureFinalizeHook was not called")
	}
	if req.WorkDir != worktreeDir {
		t.Errorf("WorkDir = %q, want %q", req.WorkDir, worktreeDir)
	}
	if req.SessionId != "sess-1" {
		t.Errorf("SessionId = %q, want %q", req.SessionId, "sess-1")
	}
	if req.HookToken != "secret-token-123" {
		t.Errorf("HookToken = %q, want %q", req.HookToken, "secret-token-123")
	}
	if req.HookPort != 45678 {
		t.Errorf("HookPort = %d, want %d", req.HookPort, 45678)
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
		cr := newMockAgentRunner()
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
		cr := newMockAgentRunner()
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

func TestEnsurePR_CreateDraftPRFailureLogsBranchDiagnostics(t *testing.T) {
	const prFailure = "gh pr create: GraphQL: No commits between main and br-1"

	ctx := context.Background()
	var logs bytes.Buffer
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		branchDebugSnapshot: &gitpkg.BranchDebugSnapshot{
			CurrentBranch: "br-1",
			HeadSHA:       "head-sha",
			RemoteHeadSHA: "remote-sha",
			AheadBehind:   "0\t1",
		},
	}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	vp.createPRErr = errors.New(prFailure)

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

	lc := NewLifecycle(sessions, repos, nil, nil, wt, cr, nil, vp, zerolog.New(&logs))

	err := lc.EnsurePR(ctx, "sess-1")
	if err == nil {
		t.Fatal("EnsurePR returned nil, want draft PR failure")
	}
	if !strings.Contains(err.Error(), "create draft PR: "+prFailure) {
		t.Fatalf("EnsurePR error = %q, want draft PR failure", err)
	}

	if len(wt.branchDebugSnapshotCalls) != 1 {
		t.Fatalf("BranchDebugSnapshot calls = %d, want 1", len(wt.branchDebugSnapshotCalls))
	}
	call := wt.branchDebugSnapshotCalls[0]
	if call.worktreePath != "/tmp/wt" || call.branch != "br-1" || call.baseBranch != "main" {
		t.Fatalf("BranchDebugSnapshot call = %+v, want path=/tmp/wt branch=br-1 base=main", call)
	}

	updated := sessions.sessions["sess-1"]
	if updated.BlockedReason == nil {
		t.Fatal("BlockedReason = nil, want draft PR failure")
	}
	if !strings.Contains(*updated.BlockedReason, "draft PR creation failed: create draft PR: "+prFailure) {
		t.Fatalf("BlockedReason = %q, want draft PR failure", *updated.BlockedReason)
	}

	var event map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var candidate map[string]any
		if err := json.Unmarshal([]byte(line), &candidate); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if candidate["message"] == "draft PR creation failed with branch debug snapshot" {
			event = candidate
			break
		}
	}
	if event == nil {
		t.Fatalf("branch diagnostic log not found in:\n%s", logs.String())
	}
	for field, want := range map[string]string{
		"session":         "sess-1",
		"branch":          "br-1",
		"base":            "main",
		"current_branch":  "br-1",
		"head_sha":        "head-sha",
		"remote_head_sha": "remote-sha",
		"ahead_behind":    "0\t1",
	} {
		if got := event[field]; got != want {
			t.Fatalf("log field %s = %v, want %q", field, got, want)
		}
	}
	if got := fmt.Sprint(event["error"]); !strings.Contains(got, "create draft PR: "+prFailure) {
		t.Fatalf("log error = %q, want original draft PR error", got)
	}
}

func TestEnsurePR_RetriesDraftPRWithoutEmptyCommitAndClearsBlockedReason(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	agentChats := &mockAgentChatStore{}
	worktrees := &mockWorktreeManager{}
	agentRunner := newMockAgentRunner()
	provider := newMockVCSProvider()
	lifecycle := NewLifecycle(sessions, repos, agentChats, nil, worktrees, agentRunner, nil, provider, zerolog.Nop())

	repo := &models.Repo{
		ID:              "repo-1",
		DisplayName:     "repo",
		LocalPath:       "/tmp/repo",
		OriginURL:       "git@github.com:owner/repo.git",
		WorktreeBaseDir: "/tmp/worktrees",
	}
	repos.repos[repo.ID] = repo

	reason := "draft PR creation failed: create PR: GraphQL: Head sha can't be blank"
	session := &models.Session{
		ID:            "sess-1",
		RepoID:        repo.ID,
		Title:         "Open a PR",
		Plan:          "Do useful work",
		WorktreePath:  "/tmp/worktrees/repo/open-a-pr",
		BranchName:    "open-a-pr",
		BaseBranch:    "main",
		State:         machine.ImplementingPlan,
		BlockedReason: &reason,
	}
	sessions.sessions[session.ID] = session

	provider.nextPRInfo = &vcs.PRInfo{
		Number: 77,
		URL:    "https://github.com/owner/repo/pull/77",
	}

	if err := lifecycle.EnsurePR(ctx, session.ID); err != nil {
		t.Fatalf("EnsurePR returned error: %v", err)
	}

	updated := sessions.sessions[session.ID]
	if updated.PRNumber == nil || *updated.PRNumber != 77 {
		t.Fatalf("PRNumber = %v, want 77", updated.PRNumber)
	}
	if updated.PRURL == nil || *updated.PRURL != "https://github.com/owner/repo/pull/77" {
		t.Fatalf("PRURL = %v, want expected PR URL", updated.PRURL)
	}
	if updated.BlockedReason != nil {
		t.Fatalf("BlockedReason = %q, want nil", *updated.BlockedReason)
	}
	if worktrees.emptyCommitCalls != 0 {
		t.Fatalf("emptyCommitCalls = %d, want 0", worktrees.emptyCommitCalls)
	}
	if len(provider.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(provider.createPRCalls))
	}
}

func TestEnsurePR_ClearBlockedReasonFailureKeepsAttachedPR(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	agentChats := &mockAgentChatStore{}
	worktrees := &mockWorktreeManager{}
	agentRunner := newMockAgentRunner()
	provider := newMockVCSProvider()
	lifecycle := NewLifecycle(sessions, repos, agentChats, nil, worktrees, agentRunner, nil, provider, zerolog.Nop())

	repo := &models.Repo{
		ID:              "repo-1",
		DisplayName:     "repo",
		LocalPath:       "/tmp/repo",
		OriginURL:       "git@github.com:owner/repo.git",
		WorktreeBaseDir: "/tmp/worktrees",
	}
	repos.repos[repo.ID] = repo

	reason := "draft PR creation failed: create PR: GraphQL: Head sha can't be blank"
	session := &models.Session{
		ID:            "sess-1",
		RepoID:        repo.ID,
		Title:         "Open a PR",
		Plan:          "Do useful work",
		WorktreePath:  "/tmp/worktrees/repo/open-a-pr",
		BranchName:    "open-a-pr",
		BaseBranch:    "main",
		State:         machine.ImplementingPlan,
		BlockedReason: &reason,
	}
	sessions.sessions[session.ID] = session

	provider.nextPRInfo = &vcs.PRInfo{
		Number: 77,
		URL:    "https://github.com/owner/repo/pull/77",
	}

	clearAttempts := 0
	sessions.updateHook = func(_ string, params db.UpdateSessionParams) error {
		if params.BlockedReason != nil && *params.BlockedReason == nil {
			clearAttempts++
			return errors.New("clear failed")
		}
		return nil
	}

	if err := lifecycle.EnsurePR(ctx, session.ID); err != nil {
		t.Fatalf("EnsurePR returned error: %v", err)
	}

	updated := sessions.sessions[session.ID]
	if updated.PRNumber == nil || *updated.PRNumber != 77 {
		t.Fatalf("PRNumber = %v, want 77", updated.PRNumber)
	}
	if updated.PRURL == nil || *updated.PRURL != "https://github.com/owner/repo/pull/77" {
		t.Fatalf("PRURL = %v, want expected PR URL", updated.PRURL)
	}
	if updated.BlockedReason == nil || *updated.BlockedReason != reason {
		t.Fatalf("BlockedReason = %v, want original reason", updated.BlockedReason)
	}
	if clearAttempts != 1 {
		t.Fatalf("clear attempts = %d, want 1", clearAttempts)
	}
	if len(provider.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(provider.createPRCalls))
	}
}

func TestEnsurePR_AttachesExistingPROnDuplicateError(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	agentChats := &mockAgentChatStore{}
	worktrees := &mockWorktreeManager{}
	agentRunner := newMockAgentRunner()
	provider := newMockVCSProvider()
	lifecycle := NewLifecycle(sessions, repos, agentChats, nil, worktrees, agentRunner, nil, provider, zerolog.Nop())

	repo := &models.Repo{
		ID:              "repo-1",
		DisplayName:     "repo",
		LocalPath:       "/tmp/repo",
		OriginURL:       "git@github.com:owner/repo.git",
		WorktreeBaseDir: "/tmp/worktrees",
	}
	repos.repos[repo.ID] = repo

	reason := "draft PR creation failed: create PR: GraphQL: Head sha can't be blank"
	session := &models.Session{
		ID:            "sess-1",
		RepoID:        repo.ID,
		Title:         "Open a PR",
		Plan:          "Do useful work",
		WorktreePath:  "/tmp/worktrees/repo/open-a-pr",
		BranchName:    "open-a-pr",
		BaseBranch:    "main",
		State:         machine.ImplementingPlan,
		BlockedReason: &reason,
	}
	sessions.sessions[session.ID] = session

	provider.createPRErr = vcs.ErrPRAlreadyExists
	provider.nextOpenPRs = []vcs.PRSummary{
		{
			Number:     42,
			Title:      "Open a PR",
			HeadBranch: "open-a-pr",
			State:      vcs.PRStateOpen,
		},
	}

	if err := lifecycle.EnsurePR(ctx, session.ID); err != nil {
		t.Fatalf("EnsurePR returned error: %v", err)
	}

	updated := sessions.sessions[session.ID]
	if updated.PRNumber == nil || *updated.PRNumber != 42 {
		t.Fatalf("PRNumber = %v, want 42", updated.PRNumber)
	}
	if updated.PRURL == nil || *updated.PRURL != "https://github.com/owner/repo/pull/42" {
		t.Fatalf("PRURL = %v, want https://github.com/owner/repo/pull/42", updated.PRURL)
	}
	if updated.BlockedReason != nil {
		t.Fatalf("BlockedReason = %q, want nil", *updated.BlockedReason)
	}
	if len(provider.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(provider.createPRCalls))
	}
}

func TestEnsurePR_AttachesExistingCronPRAndUpdatesMessyTitle(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	agentChats := &mockAgentChatStore{}
	worktrees := &mockWorktreeManager{
		latestCommitSubject: "test(githubapp): [#493] cover malformed Link header without brackets",
	}
	agentRunner := newMockAgentRunner()
	provider := newMockVCSProvider()
	lifecycle := NewLifecycle(sessions, repos, agentChats, nil, worktrees, agentRunner, nil, provider, zerolog.Nop())

	repo := &models.Repo{
		ID:              "repo-1",
		DisplayName:     "repo",
		LocalPath:       "/tmp/repo",
		OriginURL:       "git@github.com:owner/repo.git",
		WorktreeBaseDir: "/tmp/worktrees",
	}
	repos.repos[repo.ID] = repo

	cronJobID := "cron-1"
	session := &models.Session{
		ID:           "sess-1",
		RepoID:       repo.ID,
		Title:        "test(githubapp): [#493] cover malformed Link header without brackets",
		Plan:         "Do useful work",
		WorktreePath: "/tmp/worktrees/repo/open-a-pr",
		BranchName:   "cron-link-header-123",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
	}
	sessions.sessions[session.ID] = session

	provider.createPRErr = vcs.ErrPRAlreadyExists
	provider.nextOpenPRs = []vcs.PRSummary{
		{
			Number:     493,
			Title:      "test(githubapp): [#493] cover malformed Link header without brackets",
			HeadBranch: "cron-link-header-123",
			State:      vcs.PRStateOpen,
		},
	}

	if err := lifecycle.EnsurePR(ctx, session.ID); err != nil {
		t.Fatalf("EnsurePR returned error: %v", err)
	}

	if len(provider.updatePRTitleCalls) != 1 {
		t.Fatalf("UpdatePRTitle calls = %d, want 1", len(provider.updatePRTitleCalls))
	}
	if got, want := provider.updatePRTitleCalls[0].repoPath, "git@github.com:owner/repo.git"; got != want {
		t.Fatalf("UpdatePRTitle repoPath = %q, want %q", got, want)
	}
	if got, want := provider.updatePRTitleCalls[0].prID, 493; got != want {
		t.Fatalf("UpdatePRTitle prID = %d, want %d", got, want)
	}
	if got, want := provider.updatePRTitleCalls[0].title, "Cover malformed Link header without brackets"; got != want {
		t.Fatalf("UpdatePRTitle title = %q, want %q", got, want)
	}

	updated := sessions.sessions[session.ID]
	if updated.PRNumber == nil || *updated.PRNumber != 493 {
		t.Fatalf("PRNumber = %v, want 493", updated.PRNumber)
	}
}

func TestEnsurePR_AttachesExistingNonCronPRWithoutUpdatingTitle(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	agentChats := &mockAgentChatStore{}
	worktrees := &mockWorktreeManager{
		latestCommitSubject: "test(githubapp): [#493] cover malformed Link header without brackets",
	}
	agentRunner := newMockAgentRunner()
	provider := newMockVCSProvider()
	lifecycle := NewLifecycle(sessions, repos, agentChats, nil, worktrees, agentRunner, nil, provider, zerolog.Nop())

	repo := &models.Repo{
		ID:              "repo-1",
		DisplayName:     "repo",
		LocalPath:       "/tmp/repo",
		OriginURL:       "git@github.com:owner/repo.git",
		WorktreeBaseDir: "/tmp/worktrees",
	}
	repos.repos[repo.ID] = repo

	session := &models.Session{
		ID:           "sess-1",
		RepoID:       repo.ID,
		Title:        "test(githubapp): [#493] cover malformed Link header without brackets",
		Plan:         "Do useful work",
		WorktreePath: "/tmp/worktrees/repo/open-a-pr",
		BranchName:   "cron-link-header-123",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
	}
	sessions.sessions[session.ID] = session

	provider.createPRErr = vcs.ErrPRAlreadyExists
	provider.nextOpenPRs = []vcs.PRSummary{
		{
			Number:     493,
			Title:      "test(githubapp): [#493] cover malformed Link header without brackets",
			HeadBranch: "cron-link-header-123",
			State:      vcs.PRStateOpen,
		},
	}

	if err := lifecycle.EnsurePR(ctx, session.ID); err != nil {
		t.Fatalf("EnsurePR returned error: %v", err)
	}

	if len(provider.updatePRTitleCalls) != 0 {
		t.Fatalf("UpdatePRTitle calls = %d, want 0", len(provider.updatePRTitleCalls))
	}
}

func TestEnsurePR_AttachExistingCronPRTitleUpdateFailureReturnsError(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	agentChats := &mockAgentChatStore{}
	worktrees := &mockWorktreeManager{
		latestCommitSubject: "test(githubapp): [#493] cover malformed Link header without brackets",
	}
	agentRunner := newMockAgentRunner()
	provider := newMockVCSProvider()
	provider.updatePRTitleErr = errors.New("gh pr edit failed")
	lifecycle := NewLifecycle(sessions, repos, agentChats, nil, worktrees, agentRunner, nil, provider, zerolog.Nop())

	repo := &models.Repo{
		ID:              "repo-1",
		DisplayName:     "repo",
		LocalPath:       "/tmp/repo",
		OriginURL:       "git@github.com:owner/repo.git",
		WorktreeBaseDir: "/tmp/worktrees",
	}
	repos.repos[repo.ID] = repo

	cronJobID := "cron-1"
	session := &models.Session{
		ID:           "sess-1",
		RepoID:       repo.ID,
		Title:        "test(githubapp): [#493] cover malformed Link header without brackets",
		Plan:         "Do useful work",
		WorktreePath: "/tmp/worktrees/repo/open-a-pr",
		BranchName:   "cron-link-header-123",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		CronJobID:    &cronJobID,
	}
	sessions.sessions[session.ID] = session

	provider.createPRErr = vcs.ErrPRAlreadyExists
	provider.nextOpenPRs = []vcs.PRSummary{
		{
			Number:     493,
			Title:      "test(githubapp): [#493] cover malformed Link header without brackets",
			HeadBranch: "cron-link-header-123",
			State:      vcs.PRStateOpen,
		},
	}

	err := lifecycle.EnsurePR(ctx, session.ID)
	if err == nil || !strings.Contains(err.Error(), "update attached PR title") {
		t.Fatalf("EnsurePR error = %v, want update attached PR title failure", err)
	}

	updated := sessions.sessions[session.ID]
	if updated.PRNumber != nil {
		t.Fatalf("PRNumber = %v, want nil", updated.PRNumber)
	}
}

func TestEnsurePR_AttachExistingPRReturnsClearBlockedReasonFailure(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	agentChats := &mockAgentChatStore{}
	worktrees := &mockWorktreeManager{}
	agentRunner := newMockAgentRunner()
	provider := newMockVCSProvider()
	lifecycle := NewLifecycle(sessions, repos, agentChats, nil, worktrees, agentRunner, nil, provider, zerolog.Nop())

	repo := &models.Repo{
		ID:              "repo-1",
		DisplayName:     "repo",
		LocalPath:       "/tmp/repo",
		OriginURL:       "git@github.com:owner/repo.git",
		WorktreeBaseDir: "/tmp/worktrees",
	}
	repos.repos[repo.ID] = repo

	reason := "draft PR creation failed: create PR: GraphQL: Head sha can't be blank"
	session := &models.Session{
		ID:            "sess-1",
		RepoID:        repo.ID,
		Title:         "Open a PR",
		Plan:          "Do useful work",
		WorktreePath:  "/tmp/worktrees/repo/open-a-pr",
		BranchName:    "open-a-pr",
		BaseBranch:    "main",
		State:         machine.ImplementingPlan,
		BlockedReason: &reason,
	}
	sessions.sessions[session.ID] = session

	provider.createPRErr = vcs.ErrPRAlreadyExists
	provider.nextOpenPRs = []vcs.PRSummary{
		{
			Number:     42,
			Title:      "Open a PR",
			HeadBranch: "open-a-pr",
			State:      vcs.PRStateOpen,
		},
	}

	clearErr := errors.New("clear failed")
	clearAttempts := 0
	sessions.updateHook = func(_ string, params db.UpdateSessionParams) error {
		if params.BlockedReason != nil && *params.BlockedReason == nil {
			clearAttempts++
			return clearErr
		}
		return nil
	}

	err := lifecycle.EnsurePR(ctx, session.ID)
	if !errors.Is(err, clearErr) {
		t.Fatalf("EnsurePR error = %v, want clear failure", err)
	}

	updated := sessions.sessions[session.ID]
	if updated.PRNumber == nil || *updated.PRNumber != 42 {
		t.Fatalf("PRNumber = %v, want 42", updated.PRNumber)
	}
	if updated.PRURL == nil || *updated.PRURL != "https://github.com/owner/repo/pull/42" {
		t.Fatalf("PRURL = %v, want https://github.com/owner/repo/pull/42", updated.PRURL)
	}
	if clearAttempts != 1 {
		t.Fatalf("clear attempts = %d, want 1", clearAttempts)
	}
	if len(provider.createPRCalls) != 1 {
		t.Fatalf("CreateDraftPR calls = %d, want 1", len(provider.createPRCalls))
	}
}

func TestNormalizeCronPRTitle_StripsConventionalCommitAndPRNumberPrefixes(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		want    string
	}{
		{
			name:    "conventional commit with scope and PR number",
			subject: "test(githubapp): [#493] cover malformed Link header without brackets",
			want:    "Cover malformed Link header without brackets",
		},
		{
			name:    "conventional commit without PR number",
			subject: "fix: bump lodash",
			want:    "Bump lodash",
		},
		{
			name:    "PR number without conventional commit prefix",
			subject: "[#493] cover malformed Link header without brackets",
			want:    "Cover malformed Link header without brackets",
		},
		{
			name:    "already natural language",
			subject: "Cover malformed Link header without brackets",
			want:    "Cover malformed Link header without brackets",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeCronPRTitle(tt.subject); got != tt.want {
				t.Fatalf("normalizeCronPRTitle(%q) = %q, want %q", tt.subject, got, tt.want)
			}
		})
	}
}

func TestLinkPR_NumberUpdatesSessionAndCronLastRun(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	blockedReason := "draft PR creation failed"
	sessions.sessions["sess-1"] = &models.Session{
		ID:            "sess-1",
		RepoID:        "repo-1",
		BranchName:    "cron-br-1",
		State:         machine.Blocked,
		CronJobID:     &cronJobID,
		BlockedReason: &blockedReason,
	}

	lifecycle := NewLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	updated, err := lifecycle.LinkPR(ctx, "sess-1", "42")
	if err != nil {
		t.Fatalf("LinkPR: %v", err)
	}

	if updated.PRNumber == nil || *updated.PRNumber != 42 {
		t.Fatalf("PRNumber = %v, want 42", updated.PRNumber)
	}
	if updated.PRURL == nil || *updated.PRURL != "https://github.com/owner/repo/pull/42" {
		t.Fatalf("PRURL = %v, want generated URL", updated.PRURL)
	}
	if updated.State != machine.AwaitingChecks {
		t.Fatalf("state = %s, want awaiting_checks", updated.State)
	}
	if updated.BlockedReason != nil {
		t.Fatalf("BlockedReason = %q, want nil", *updated.BlockedReason)
	}
	if len(vp.getPRStatusPRNumbers) != 1 || vp.getPRStatusPRNumbers[0] != 42 {
		t.Fatalf("GetPRStatus calls = %v, want [42]", vp.getPRStatusPRNumbers)
	}
	if len(cron.lastRunCalls) != 1 {
		t.Fatalf("UpdateLastRun calls = %d, want 1", len(cron.lastRunCalls))
	}
	if cron.lastRunCalls[0].sessionID == nil || *cron.lastRunCalls[0].sessionID != "sess-1" {
		t.Fatalf("last run session ID = %v, want sess-1", cron.lastRunCalls[0].sessionID)
	}
	if cron.lastRunCalls[0].outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("last run outcome = %q, want pr_created", cron.lastRunCalls[0].outcome)
	}
}

func TestLinkPR_FinalizingSessionMovesToAwaitingChecks(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	cronJobID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		BranchName: "cron-br-1",
		State:      machine.Finalizing,
		CronJobID:  &cronJobID,
	}

	lifecycle := NewLifecycle(sessions, repos, nil, cron, wt, cr, nil, vp, logger)
	updated, err := lifecycle.LinkPR(ctx, "sess-1", "42")
	if err != nil {
		t.Fatalf("LinkPR: %v", err)
	}

	if updated.State != machine.AwaitingChecks {
		t.Fatalf("state = %s, want awaiting_checks", updated.State)
	}
	if updated.PRNumber == nil || *updated.PRNumber != 42 {
		t.Fatalf("PRNumber = %v, want 42", updated.PRNumber)
	}
	if len(cron.lastRunCalls) != 1 {
		t.Fatalf("UpdateLastRun calls = %d, want 1", len(cron.lastRunCalls))
	}
	if cron.lastRunCalls[0].outcome != models.CronJobOutcomePRCreated {
		t.Fatalf("last run outcome = %q, want pr_created", cron.lastRunCalls[0].outcome)
	}
}

func TestLinkPR_URLRejectsWrongRepo(t *testing.T) {
	ctx := context.Background()
	logger := zerolog.Nop()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()

	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:     "sess-1",
		RepoID: "repo-1",
		State:  machine.Blocked,
	}

	lifecycle := NewLifecycle(sessions, repos, nil, &stubCronJobStore{}, wt, cr, nil, vp, logger)
	_, err := lifecycle.LinkPR(ctx, "sess-1", "https://github.com/other/repo/pull/42")
	if err == nil {
		t.Fatal("expected wrong-repo PR URL to be rejected")
	}
	if sessions.sessions["sess-1"].PRNumber != nil {
		t.Fatalf("PRNumber = %v, want nil after rejection", sessions.sessions["sess-1"].PRNumber)
	}
	if len(vp.getPRStatusPRNumbers) != 0 {
		t.Fatalf("GetPRStatus should not be called before repo validation, got %v", vp.getPRStatusPRNumbers)
	}
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
	failStderr        map[string]string
	capturePaneOutput string // output for capture-pane stdout
	available         bool   // controls whether `tmux -V` succeeds
}

func newFakeTmux() *fakeTmux {
	return &fakeTmux{
		failSubcommand:    map[string]bool{},
		failStderr:        map[string]string{},
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
		if stderr := f.failStderr[subcommand]; stderr != "" {
			return exec.CommandContext(ctx, "sh", "-c", "printf '%s' \"$1\" >&2; exit 1", "sh", stderr)
		}
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
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
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
		AgentName:  "claude",
	}

	lc := NewLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), logger)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": newFakeAgent()})
	lc.SetAgentLogsDir(t.TempDir())

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
		t.Fatalf("expected 1 agentChats.Create call, got %d", len(chats.createCalls))
	}
	createdAgentSessionID := chats.createCalls[0].AgentSessionID
	if createdAgentSessionID == "" {
		t.Error("expected non-empty ClaudeID on agentChats.Create")
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
	if chats.tmuxNameUpdates[0].agentSessionID != createdAgentSessionID {
		t.Errorf("UpdateTmuxSessionName agentSessionID = %q, want %q", chats.tmuxNameUpdates[0].agentSessionID, createdAgentSessionID)
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
	if sess.AgentSessionID == nil || *sess.AgentSessionID != createdAgentSessionID {
		t.Errorf("session.AgentSessionID = %v, want %q", sess.AgentSessionID, createdAgentSessionID)
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
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
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
		AgentName:  "claude",
	}

	lc := NewLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), logger)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": newFakeAgent()})
	lc.SetAgentLogsDir(t.TempDir())

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
		t.Errorf("expected 0 agentChats.Create calls when tmux unavailable, got %d", len(chats.createCalls))
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
	chats := &mockAgentChatStore{
		createErr: fmt.Errorf("simulated DB failure"),
	}
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
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
		AgentName:  "claude",
	}

	lc := NewLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), logger)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": newFakeAgent()})
	lc.SetAgentLogsDir(t.TempDir())

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
		t.Errorf("expected 1 agentChats.Create attempt, got %d", len(chats.createCalls))
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
	cr := newMockAgentRunner()
	vp := newMockVCSProvider()

	tmuxName := "boss-repo1234-claude01"
	chats := &mockAgentChatStore{
		chatsBySession: map[string][]*models.AgentChat{
			"sess-1": {{
				ID:              "chat-claude-01",
				SessionID:       "sess-1",
				AgentSessionID:  "claude-01",
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

// TestSetAgents_RoutesByAgentName verifies that a Lifecycle wired with
// multiple agent clients routes ConfigureFinalizeHook to the client whose
// name matches the session's AgentName — the production multi-agent path
// when more than one bossd-plugin-<agent> is installed.
func TestSetAgents_RoutesByAgentName(t *testing.T) {
	ctx := context.Background()
	worktreeDir := t.TempDir()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{worktreePath: worktreeDir}
	cr := newMockAgentRunner()
	tmuxFake := newFakeTmux()
	tx := tmux.NewClient(tmux.WithCommandFactory(tmuxFake.factory))
	logger := zerolog.Nop()

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		OriginURL:         "owner/repo",
	}
	sessions.sessions["sess-opencode"] = &models.Session{
		ID:         "sess-opencode",
		RepoID:     "repo-1",
		Title:      "opencode session",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "opencode",
	}

	claudeAgent := newFakeAgent()
	openCodeAgent := newFakeAgent()

	lc := NewLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), logger)
	lc.SetHookPort(45678)
	lc.SetAgents(map[string]agent.AgentRunnerClient{
		"claude":   claudeAgent,
		"opencode": openCodeAgent,
	})
	lc.SetAgentLogsDir(t.TempDir())

	if err := lc.StartSession(ctx, "sess-opencode", StartSessionOpts{
		DeferPR:   true,
		CronJobID: "cron-1",
		HookToken: "tok",
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if openCodeAgent.LastConfigureHookReq == nil {
		t.Fatal("expected ConfigureFinalizeHook on opencode agent")
	}
	if claudeAgent.LastConfigureHookReq != nil {
		t.Fatal("did not expect ConfigureFinalizeHook on claude agent")
	}
}

// TestSetAgents_UnknownAgentErrors verifies that StartSession surfaces a
// clear error when the session's AgentName has no matching client in the
// registry — defense in depth in case CreateSession ever persists a name
// for which no plugin is loaded.
func TestSetAgents_UnknownAgentErrors(t *testing.T) {
	ctx := context.Background()
	worktreeDir := t.TempDir()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{worktreePath: worktreeDir}
	cr := newMockAgentRunner()
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
		Title:      "Unknown agent",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "ghost",
	}

	claudeAgent := newFakeAgent()
	lc := NewLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), logger)
	lc.SetHookPort(45678)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": claudeAgent})

	err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		DeferPR:   true,
		CronJobID: "cron-1",
		HookToken: "tok",
	})
	if err == nil {
		t.Fatal("expected error when AgentName has no registered client")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error %q should mention the missing agent name", err)
	}
}

// TestStartSessionDoesNotArmPollFallbackWhenHookUnsupportedWithoutCronJobID
// verifies that hookless non-cron sessions do not arm daemon-side poll
// fallback.
func TestStartSessionDoesNotArmPollFallbackWhenHookUnsupportedWithoutCronJobID(t *testing.T) {
	ctx := context.Background()
	worktreeDir := t.TempDir()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{worktreePath: worktreeDir}
	cr := newMockAgentRunner()
	tx := tmux.NewClient(tmux.WithCommandFactory(newFakeTmux().factory))

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
		Title:      "Hookless audit",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "codex",
	}

	fa := newFakeAgent()
	fa.IsSupported = false // hookless agent (e.g. codex)

	armer := &fakePollArmer{}
	lc := NewLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), zerolog.Nop())
	lc.SetHookPort(45678)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"codex": fa})
	lc.SetAgentLogsDir(t.TempDir())
	lc.SetPollArmer(armer)
	lc.SetDaemonCtx(ctx)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		DeferPR:   true,
		HookToken: "tok-1",
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if armer.armCalled {
		t.Errorf("poll fallback armed for non-cron session id %q", armer.armedID)
	}
}

// TestBootstrapDoesNotPollHooklessTmuxRuns verifies that on daemon restart,
// Lifecycle.Bootstrap does not poll plugin ExitStatus for tmux-hosted rows
// whose agent reports IsSupported=false.
func TestBootstrapDoesNotPollHooklessTmuxRuns(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()
	tmuxFake := newFakeTmux()
	tx := tmux.NewClient(tmux.WithCommandFactory(tmuxFake.factory))

	tok := "tok-3"
	cronID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Some session",
		WorktreePath: "/tmp/wt",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		AgentName:    "codex",
		HookToken:    &tok,
		CronJobID:    &cronID,
	}

	tmuxName := "tmux-x"
	chats := &mockAgentChatStore{
		chatsWithTmux: []*models.AgentChat{
			{
				ID:              "chat-1",
				SessionID:       "sess-1",
				AgentSessionID:  "run-1",
				AgentName:       "codex",
				TmuxSessionName: &tmuxName,
			},
		},
	}

	fa := newFakeAgent()
	fa.IsSupported = false

	armer := &fakePollArmer{}
	notifier := &recordingCronCompletionNotifier{}
	lc := NewLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), zerolog.Nop())
	lc.SetHookPort(45678)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"codex": fa})
	lc.SetAgentLogsDir(t.TempDir())
	lc.SetPollArmer(armer)
	lc.SetCronCompletionNotifier(notifier)
	lc.SetDaemonCtx(ctx)
	lc.tmuxCompletionPollInterval = time.Millisecond

	lc.Bootstrap(ctx)

	if armer.armCalled {
		t.Errorf("Bootstrap armed plugin poll fallback for tmux-hosted run %q", armer.armedID)
	}
	tmuxFake.mu.Lock()
	tmuxFake.failSubcommand["has-session"] = true
	tmuxFake.failStderr["has-session"] = "can't find session"
	tmuxFake.mu.Unlock()

	waitForCount(t, "NotifyCronAgentStopped", notifier.count)
	calls := notifier.callsCopy()
	if calls[0] != "sess-1" {
		t.Fatalf("NotifyCronAgentStopped called with %q, want sess-1", calls[0])
	}
}

// TestBootstrapDoesNotArmForHookedAgents verifies the cache prevents
// arming when the agent reports IsSupported=true (claude).
func TestBootstrapDoesNotArmForHookedAgents(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()

	tok := "tok-3"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		Title:        "Some session",
		WorktreePath: "/tmp/wt",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		AgentName:    "claude",
		HookToken:    &tok,
	}

	tmuxName := "tmux-c"
	chats := &mockAgentChatStore{
		chatsWithTmux: []*models.AgentChat{
			{
				ID:              "chat-1",
				SessionID:       "sess-1",
				AgentSessionID:  "run-c",
				AgentName:       "claude",
				TmuxSessionName: &tmuxName,
			},
		},
	}

	fa := newFakeAgent() // IsSupported defaults to true
	armer := &fakePollArmer{}
	lc := NewLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetHookPort(45678)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": fa})
	lc.SetAgentLogsDir(t.TempDir())
	lc.SetPollArmer(armer)
	lc.SetDaemonCtx(ctx)

	lc.Bootstrap(ctx)

	if armer.armCalled {
		t.Error("Bootstrap should NOT arm for hooked agents (claude)")
	}
	if fa.LastConfigureHookReq == nil {
		t.Fatal("Bootstrap should probe ConfigureFinalizeHook")
	}
	if fa.LastConfigureHookReq.GetAgentSessionId() != "" {
		t.Errorf("bootstrap hook probe AgentSessionId = %q, want empty", fa.LastConfigureHookReq.GetAgentSessionId())
	}
}

// TestBootstrapReConfiguresHookForEveryWorktree pins the invariant that when
// multiple hook-supporting (claude) cron chats survive a daemon restart,
// Bootstrap calls ConfigureFinalizeHook for EACH one. The hook config is
// written per-worktree and carries this daemon's restarted port; caching the
// per-agent support result must not skip the rewrite for chats after the
// first, or those worktrees keep the previous daemon's dead port and their
// Stop hook never reaches the restarted daemon.
func TestBootstrapReConfiguresHookForEveryWorktree(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	cr := newMockAgentRunner()

	tok1 := "tok-1"
	tok2 := "tok-2"
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-1",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		AgentName:    "claude",
		HookToken:    &tok1,
	}
	sessions.sessions["sess-2"] = &models.Session{
		ID:           "sess-2",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-2",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
		AgentName:    "claude",
		HookToken:    &tok2,
	}

	tmux1 := "tmux-1"
	tmux2 := "tmux-2"
	chats := &mockAgentChatStore{
		chatsWithTmux: []*models.AgentChat{
			{ID: "chat-1", SessionID: "sess-1", AgentSessionID: "run-1", AgentName: "claude", TmuxSessionName: &tmux1},
			{ID: "chat-2", SessionID: "sess-2", AgentSessionID: "run-2", AgentName: "claude", TmuxSessionName: &tmux2},
		},
	}

	fa := newFakeAgent() // IsSupported defaults to true
	lc := NewLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetHookPort(45678)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": fa})
	lc.SetAgentLogsDir(t.TempDir())
	lc.SetDaemonCtx(ctx)

	lc.Bootstrap(ctx)

	if len(fa.ConfigureHookReqs) != 2 {
		t.Fatalf("ConfigureFinalizeHook calls = %d, want 2 (one per surviving worktree)", len(fa.ConfigureHookReqs))
	}
	gotWorkDirs := map[string]bool{}
	for _, req := range fa.ConfigureHookReqs {
		gotWorkDirs[req.GetWorkDir()] = true
		if req.GetHookPort() != 45678 {
			t.Errorf("ConfigureFinalizeHook HookPort = %d, want 45678", req.GetHookPort())
		}
	}
	if !gotWorkDirs["/tmp/wt-1"] || !gotWorkDirs["/tmp/wt-2"] {
		t.Errorf("ConfigureFinalizeHook worktrees = %v, want both /tmp/wt-1 and /tmp/wt-2", gotWorkDirs)
	}
}

// TestStartSessionDoesNotArmPollFallbackWhenHookSupported verifies that
// for agents that own a finalize hook (claude), StartSession does NOT arm
// the poll fallback — the Stop hook drives completion directly.
func TestStartSessionDoesNotArmPollFallbackWhenHookSupported(t *testing.T) {
	ctx := context.Background()
	worktreeDir := t.TempDir()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{worktreePath: worktreeDir}
	cr := newMockAgentRunner()
	tx := tmux.NewClient(tmux.WithCommandFactory(newFakeTmux().factory))

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
		Title:      "Hooked agent",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "claude",
	}

	fa := newFakeAgent() // IsSupported defaults to true
	armer := &fakePollArmer{}
	lc := NewLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), zerolog.Nop())
	lc.SetHookPort(45678)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": fa})
	lc.SetAgentLogsDir(t.TempDir())
	lc.SetPollArmer(armer)
	lc.SetDaemonCtx(ctx)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		DeferPR:   true,
		HookToken: "tok-1",
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if armer.armCalled {
		t.Error("poll fallback should NOT be armed when hook is supported")
	}
}

func TestStartSession_DeferPRTrue_HookSupportedDoesNotArmPollFallback(t *testing.T) {
	ctx := context.Background()
	worktreeDir := t.TempDir()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{worktreePath: worktreeDir}
	cr := newMockAgentRunner()
	tx := tmux.NewClient(tmux.WithCommandFactory(newFakeTmux().factory))

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
		Title:      "Hooked cron agent",
		Plan:       "do work",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "claude",
	}

	client := newFakeAgent()
	pollArmer := &fakePollArmer{}
	lc := NewLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), zerolog.Nop())
	lc.SetHookPort(45678)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"claude": client})
	lc.SetAgentLogsDir(t.TempDir())
	lc.SetPollArmer(pollArmer)
	lc.SetDaemonCtx(ctx)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{DeferPR: true, CronJobID: "cron-1", HookToken: "tok-1"}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if pollArmer.armCalled {
		t.Fatal("hook-supported cron agents should use hook notification, not poll fallback")
	}
}

func TestStartSession_DeferPRTrue_HooklessAgentDoesNotArmPollFallback(t *testing.T) {
	ctx := context.Background()
	worktreeDir := t.TempDir()

	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	chats := &mockAgentChatStore{}
	wt := &mockWorktreeManager{worktreePath: worktreeDir}
	cr := newMockAgentRunner()
	tmuxFake := newFakeTmux()
	tx := tmux.NewClient(tmux.WithCommandFactory(tmuxFake.factory))

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
		Title:      "Hookless cron agent",
		Plan:       "do work",
		BaseBranch: "main",
		State:      machine.CreatingWorktree,
		AgentName:  "codex",
	}

	client := newFakeAgent()
	client.IsSupported = false

	pollArmer := &fakePollArmer{}
	notifier := &recordingCronCompletionNotifier{}
	lc := NewLifecycle(sessions, repos, chats, &stubCronJobStore{}, wt, cr, tx, newMockVCSProvider(), zerolog.Nop())
	lc.newTmuxChatAgentSessionID = func() string { return "agent-1" }
	lc.tmuxCompletionPollInterval = time.Millisecond
	lc.SetHookPort(45678)
	lc.SetAgents(map[string]agent.AgentRunnerClient{"codex": client})
	lc.SetAgentLogsDir(t.TempDir())
	lc.SetPollArmer(pollArmer)
	lc.SetCronCompletionNotifier(notifier)
	lc.SetDaemonCtx(ctx)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{DeferPR: true, CronJobID: "cron-1", HookToken: "tok-1"}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	agentSessionID := sessions.sessions["sess-1"].AgentSessionID
	if agentSessionID == nil {
		t.Fatal("primary cron agent session id missing")
	}
	if pollArmer.armCalled {
		t.Fatalf("poll fallback armed for tmux-hosted hookless cron run %q", *agentSessionID)
	}
	tmuxFake.mu.Lock()
	tmuxFake.failSubcommand["has-session"] = true
	tmuxFake.failStderr["has-session"] = "can't find session"
	tmuxFake.mu.Unlock()

	waitForCount(t, "NotifyCronAgentStopped", notifier.count)
	calls := notifier.callsCopy()
	if calls[0] != "sess-1" {
		t.Fatalf("NotifyCronAgentStopped called with %q, want sess-1", calls[0])
	}
}

// labeledRunner records the "<name>:<agentSessionID>" tag on each Start so
// lifecycle tests can assert which underlying runner the dispatcher routed
// to. This mirrors the helper in services/bossd/internal/agent/dispatcher_test.go
// but is local to the session package because Go does not export test helpers
// across packages. Other AgentRunner methods are no-op stubs.
type labeledRunner struct {
	name      string
	startSeen atomic.Pointer[string] // "<name>:<agentSessionID>" set on each Start
}

func newLabeledRunner(name string) *labeledRunner {
	return &labeledRunner{name: name}
}

func (r *labeledRunner) Start(_ context.Context, _, _ string, _ *string, agentSessionID string) (string, error) {
	tag := r.name + ":" + agentSessionID
	r.startSeen.Store(&tag)
	if agentSessionID == "" {
		return r.name + "-generated-id", nil
	}
	return agentSessionID, nil
}
func (r *labeledRunner) Stop(_ string) error      { return nil }
func (r *labeledRunner) IsRunning(_ string) bool  { return false }
func (r *labeledRunner) ExitError(_ string) error { return nil }
func (r *labeledRunner) Subscribe(_ context.Context, _ string) (<-chan agent.OutputLine, error) {
	ch := make(chan agent.OutputLine)
	close(ch)
	return ch, nil
}
func (r *labeledRunner) History(_ string) []agent.OutputLine { return nil }

// TestStartSession_RoutesToCodexWhenSessionAgentNameIsCodex is the regression
// test for the agent-selection bug: a session whose AgentName="codex" must
// have its headless run dispatched to the codex plugin runner, not the
// default claude runner. Before the fix in commits 171246a0 / a8656b9e /
// f0bf5858 the lifecycle called Start(..., "") on the dispatcher's plain
// Start path, which fell back to defaultAgent="claude" and silently routed
// every codex session to claude.
//
// This test wires a *real* agent.Dispatcher (not the package-local
// mockAgentRunner forwarder, which discards agentName) into the Lifecycle so
// we exercise both the lifecycle-side StartByAgent call site AND the
// dispatcher-side routing in one go.
func TestStartSession_RoutesToCodexWhenSessionAgentNameIsCodex(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{}
	logger := zerolog.Nop()

	// Build a real Dispatcher with two named runners. The lookup func is
	// only consulted by the legacy Start path; the lifecycle uses
	// StartByAgent which bypasses lookup entirely.
	claudeRunner := newLabeledRunner("claude")
	codexRunner := newLabeledRunner("codex")
	registry := map[string]agent.AgentRunner{
		"claude": claudeRunner,
		"codex":  codexRunner,
	}
	dispatcher := agent.NewDispatcher(registry, func(string) (string, error) {
		t.Fatalf("lookup must not be consulted on the StartByAgent path")
		return "", nil
	}, "claude", logger)

	repos.repos["repo-1"] = &models.Repo{
		ID:                "repo-1",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:         "sess-1",
		RepoID:     "repo-1",
		Title:      "Codex Session",
		Plan:       "Build something with codex",
		BaseBranch: "main",
		AgentName:  "codex",
		State:      machine.CreatingWorktree,
	}

	lc := NewLifecycle(sessions, repos, nil, nil, wt, dispatcher, nil, newMockVCSProvider(), logger)

	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	// Codex must have seen Start; claude must not. The empty agentSessionID
	// is what the lifecycle passes for fresh runs (the runner generates one
	// on its own).
	if seen := codexRunner.startSeen.Load(); seen == nil {
		t.Errorf("codex runner did not see Start")
	} else if *seen != "codex:" {
		t.Errorf("codex runner saw Start with unexpected tag %q, want %q", *seen, "codex:")
	}
	if seen := claudeRunner.startSeen.Load(); seen != nil {
		t.Errorf("claude runner unexpectedly saw Start: %q (routing regression: codex session leaked to claude)", *seen)
	}

	// Sanity-check that the session was advanced and the codex runner's
	// generated ID is what got persisted — guards against a future
	// refactor that "fixes" routing but loses the returned session ID.
	sess := sessions.sessions["sess-1"]
	if sess.State != machine.ImplementingPlan {
		t.Errorf("session state = %v, want ImplementingPlan", sess.State)
	}
	if sess.AgentSessionID == nil || *sess.AgentSessionID != "codex-generated-id" {
		t.Errorf("session.AgentSessionID = %v, want codex-generated-id", sess.AgentSessionID)
	}
}
