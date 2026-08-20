package taskorchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/session"
)

// --- mock types ---

type mockTaskSourceProvider struct {
	sources []plugin.TaskSource
}

func (m *mockTaskSourceProvider) GetTaskSources() []plugin.TaskSource {
	return m.sources
}

type mockTaskSource struct {
	name          string
	userInitiated bool
	pollFn        func(ctx context.Context, repoOriginURL string) ([]*bossanovav1.TaskItem, error)
}

func (m *mockTaskSource) GetInfo(_ context.Context) (*bossanovav1.PluginInfo, error) {
	name := m.name
	if name == "" {
		name = "test-plugin"
	}
	return &bossanovav1.PluginInfo{Name: name, UserInitiated: m.userInitiated}, nil
}

func (m *mockTaskSource) PollTasks(ctx context.Context, repoOriginURL string) ([]*bossanovav1.TaskItem, error) {
	return m.pollFn(ctx, repoOriginURL)
}

func (m *mockTaskSource) UpdateTaskStatus(_ context.Context, _ string, _ bossanovav1.TaskItemStatus, _ string) error {
	return nil
}

func (m *mockTaskSource) ListAvailableIssues(_ context.Context, _ string, _ string, _ map[string]string) ([]*bossanovav1.TrackerIssue, error) {
	return nil, nil
}

type mockRepoStore struct {
	repos []*models.Repo
}

func (m *mockRepoStore) Create(_ context.Context, _ db.CreateRepoParams) (*models.Repo, error) {
	return nil, nil
}

func (m *mockRepoStore) Get(_ context.Context, id string) (*models.Repo, error) {
	for _, r := range m.repos {
		if r.ID == id {
			return r, nil
		}
	}
	return nil, nil
}

func (m *mockRepoStore) GetByPath(_ context.Context, _ string) (*models.Repo, error) {
	return nil, nil
}

func (m *mockRepoStore) GetByOrigin(_ context.Context, originURL string) (*models.Repo, error) {
	for _, r := range m.repos {
		if r.OriginURL == originURL {
			return r, nil
		}
	}
	return nil, nil
}

func (m *mockRepoStore) List(_ context.Context) ([]*models.Repo, error) {
	return m.repos, nil
}

func (m *mockRepoStore) Update(_ context.Context, _ string, _ db.UpdateRepoParams) (*models.Repo, error) {
	return nil, nil
}

func (m *mockRepoStore) Delete(_ context.Context, _ string) error {
	return nil
}

type mockTaskMappingStore struct {
	mu             sync.Mutex
	mappings       map[string]*models.TaskMapping // keyed by external_id
	bySession      map[string]*models.TaskMapping // keyed by session_id
	byID           map[string]*models.TaskMapping // keyed by mapping ID
	createFn       func(ctx context.Context, params db.CreateTaskMappingParams) (*models.TaskMapping, error)
	updateFn       func(ctx context.Context, id string, params db.UpdateTaskMappingParams) (*models.TaskMapping, error)
	deleteFn       func(ctx context.Context, id string) error
	listPendingFn  func(ctx context.Context) ([]*models.TaskMapping, error)
	listFailuresFn func(ctx context.Context, limit int) ([]*models.TaskMapping, error)
	getFn          func(ctx context.Context, id string) (*models.TaskMapping, error)
	failOrphanedFn func(ctx context.Context) (int64, error)
	nextID         int
}

func (m *mockTaskMappingStore) Create(ctx context.Context, params db.CreateTaskMappingParams) (*models.TaskMapping, error) {
	if m.createFn != nil {
		return m.createFn(ctx, params)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	tm := &models.TaskMapping{
		ID:         "tm-" + params.ExternalID,
		ExternalID: params.ExternalID,
		PluginName: params.PluginName,
		RepoID:     params.RepoID,
		Status:     models.TaskMappingStatusPending,
		RetryCount: params.RetryCount,
	}
	if m.mappings != nil {
		m.mappings[params.ExternalID] = tm
	}
	return tm, nil
}

func (m *mockTaskMappingStore) Get(ctx context.Context, id string) (*models.TaskMapping, error) {
	if m.getFn != nil {
		return m.getFn(ctx, id)
	}
	if m.byID != nil {
		if tm, ok := m.byID[id]; ok {
			return tm, nil
		}
	}
	return nil, fmt.Errorf("not found")
}

func (m *mockTaskMappingStore) FailOrphanedMappings(ctx context.Context) (int64, error) {
	if m.failOrphanedFn != nil {
		return m.failOrphanedFn(ctx)
	}
	return 0, nil
}

func (m *mockTaskMappingStore) GetByExternalID(_ context.Context, externalID string) (*models.TaskMapping, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if tm, ok := m.mappings[externalID]; ok {
		return tm, nil
	}
	return nil, nil
}

func (m *mockTaskMappingStore) GetBySessionID(_ context.Context, sessionID string) (*models.TaskMapping, error) {
	if m.bySession != nil {
		if tm, ok := m.bySession[sessionID]; ok {
			return tm, nil
		}
	}
	return nil, fmt.Errorf("not found")
}

func (m *mockTaskMappingStore) Update(ctx context.Context, id string, params db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
	if m.updateFn != nil {
		return m.updateFn(ctx, id, params)
	}
	result := &models.TaskMapping{ID: id}
	if params.RetryCount != nil {
		result.RetryCount = *params.RetryCount
	}
	if params.Status != nil {
		result.Status = *params.Status
	}
	return result, nil
}

func (m *mockTaskMappingStore) Delete(ctx context.Context, id string) error {
	if m.deleteFn != nil {
		return m.deleteFn(ctx, id)
	}
	for k, tm := range m.mappings {
		if tm.ID == id {
			delete(m.mappings, k)
			return nil
		}
	}
	return nil
}

func (m *mockTaskMappingStore) ListPending(ctx context.Context) ([]*models.TaskMapping, error) {
	if m.listPendingFn != nil {
		return m.listPendingFn(ctx)
	}
	return nil, nil
}

func (m *mockTaskMappingStore) ListRecentFailures(ctx context.Context, limit int) ([]*models.TaskMapping, error) {
	if m.listFailuresFn != nil {
		return m.listFailuresFn(ctx, limit)
	}
	return nil, nil
}

type mockSessionCreatorOrch struct {
	createFn func(ctx context.Context, opts CreateSessionOpts) (*models.Session, error)
}

func (m *mockSessionCreatorOrch) CreateSession(ctx context.Context, opts CreateSessionOpts) (*models.Session, error) {
	if m.createFn != nil {
		return m.createFn(ctx, opts)
	}
	return &models.Session{ID: "test-session"}, nil
}

type mockProvider struct {
	mergeFn             func(ctx context.Context, repoPath string, prID int) error
	mergeCommitFn       func(prID int) (string, error)
	allowedStrategiesFn func() ([]string, error)
}

func (m *mockProvider) CreateDraftPR(_ context.Context, _ vcs.CreatePROpts) (*vcs.PRInfo, error) {
	return nil, nil
}

func (m *mockProvider) GetPRStatus(_ context.Context, _ string, _ int) (*vcs.PRStatus, error) {
	return nil, nil
}

func (m *mockProvider) GetCheckResults(_ context.Context, _ string, _ int) ([]vcs.CheckResult, error) {
	return nil, nil
}

func (m *mockProvider) GetFailedCheckLogs(_ context.Context, _ string, _ string) (string, error) {
	return "", nil
}

func (m *mockProvider) MarkReadyForReview(_ context.Context, _ string, _ int) error {
	return nil
}

func (m *mockProvider) GetReviewComments(_ context.Context, _ string, _ int) ([]vcs.ReviewComment, error) {
	return nil, nil
}

func (m *mockProvider) ListOpenPRs(_ context.Context, _ string) ([]vcs.PRSummary, error) {
	return nil, nil
}

func (m *mockProvider) ListClosedPRs(_ context.Context, _ string) ([]vcs.PRSummary, error) {
	return nil, nil
}
func (m *mockProvider) SearchPRsByTitleTag(_ context.Context, _, _ string) ([]vcs.PRSummary, error) {
	return nil, nil
}

func (m *mockProvider) MergePR(ctx context.Context, repoPath string, prID int, strategy string) error {
	if m.mergeFn != nil {
		return m.mergeFn(ctx, repoPath, prID)
	}
	return nil
}

func (m *mockProvider) UpdatePRTitle(_ context.Context, _ string, _ int, _ string) error {
	return nil
}

func (m *mockProvider) GetPRMergeCommit(_ context.Context, _ string, prID int) (string, error) {
	if m.mergeCommitFn != nil {
		return m.mergeCommitFn(prID)
	}
	return "mock-merge-commit", nil
}

func (m *mockProvider) GetAllowedMergeStrategies(_ context.Context, _ string) ([]string, error) {
	if m.allowedStrategiesFn != nil {
		return m.allowedStrategiesFn()
	}
	return []string{"merge", "squash", "rebase"}, nil
}

// mockLivenessChecker implements SessionLivenessChecker for tests.
type mockLivenessChecker struct {
	livenessFn func(ctx context.Context, sessionID string) session.Liveness
}

func (m *mockLivenessChecker) SessionLiveness(ctx context.Context, sessionID string) session.Liveness {
	if m.livenessFn != nil {
		return m.livenessFn(ctx, sessionID)
	}
	return session.LivenessAlive
}

// helper to create an orchestrator with defaults
func newTestOrchestrator(opts ...func(*Orchestrator)) *Orchestrator {
	o := New(
		&mockTaskSourceProvider{sources: nil},
		&mockRepoStore{},
		&mockTaskMappingStore{mappings: map[string]*models.TaskMapping{}},
		&mockSessionCreatorOrch{},
		&mockProvider{},
		nil, // no base branch syncer by default
		nil, // no liveness checker by default
		time.Second,
		zerolog.Nop(),
	)
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// --- poll loop tests ---

func TestPoll_OnlyEligibleRepos(t *testing.T) {
	var polledURLs []string

	src := &mockTaskSource{
		pollFn: func(_ context.Context, repoOriginURL string) ([]*bossanovav1.TaskItem, error) {
			polledURLs = append(polledURLs, repoOriginURL)
			return nil, nil
		},
	}

	repos := &mockRepoStore{
		repos: []*models.Repo{
			{ID: "r1", OriginURL: "https://github.com/org/repo1", CanAutoMergeDependabot: true},
			{ID: "r2", OriginURL: "https://github.com/org/repo2", CanAutoMergeDependabot: false},
			{ID: "r3", OriginURL: "https://github.com/org/repo3", CanAutoMergeDependabot: true},
		},
	}

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sources = &mockTaskSourceProvider{sources: []plugin.TaskSource{src}}
		o.repos = repos
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orch.poll(ctx)

	if len(polledURLs) != 2 {
		t.Fatalf("expected 2 polls, got %d: %v", len(polledURLs), polledURLs)
	}
	if polledURLs[0] != "https://github.com/org/repo1" {
		t.Errorf("expected repo1, got %s", polledURLs[0])
	}
	if polledURLs[1] != "https://github.com/org/repo3" {
		t.Errorf("expected repo3, got %s", polledURLs[1])
	}
}

func TestPoll_NoSources(t *testing.T) {
	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.repos = &mockRepoStore{repos: []*models.Repo{
			{ID: "r1", OriginURL: "url", CanAutoMergeDependabot: true},
		}}
	})
	orch.poll(context.Background())
}

func TestPoll_NoEligibleRepos(t *testing.T) {
	pollCalled := false
	src := &mockTaskSource{
		pollFn: func(_ context.Context, _ string) ([]*bossanovav1.TaskItem, error) {
			pollCalled = true
			return nil, nil
		},
	}

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sources = &mockTaskSourceProvider{sources: []plugin.TaskSource{src}}
		o.repos = &mockRepoStore{repos: []*models.Repo{
			{ID: "r1", OriginURL: "url", CanAutoMergeDependabot: false},
		}}
	})
	orch.poll(context.Background())

	if pollCalled {
		t.Error("PollTasks should not be called when no repos are eligible")
	}
}

func TestPoll_MultipleSources(t *testing.T) {
	var polls []string
	makeSrc := func(name string) plugin.TaskSource {
		return &mockTaskSource{
			pollFn: func(_ context.Context, repoURL string) ([]*bossanovav1.TaskItem, error) {
				polls = append(polls, name+":"+repoURL)
				return nil, nil
			},
		}
	}

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sources = &mockTaskSourceProvider{sources: []plugin.TaskSource{
			makeSrc("src1"), makeSrc("src2"),
		}}
		o.repos = &mockRepoStore{repos: []*models.Repo{
			{ID: "r1", OriginURL: "repo1", CanAutoMergeDependabot: true},
		}}
	})
	orch.poll(context.Background())

	if len(polls) != 2 {
		t.Fatalf("expected 2 polls, got %d: %v", len(polls), polls)
	}
	if polls[0] != "src1:repo1" || polls[1] != "src2:repo1" {
		t.Errorf("unexpected polls: %v", polls)
	}
}

func TestPoll_SkipsUserInitiatedSources(t *testing.T) {
	userInit := &mockTaskSource{
		name:          "linear",
		userInitiated: true,
		pollFn: func(_ context.Context, _ string) ([]*bossanovav1.TaskItem, error) {
			t.Fatal("PollTasks must not be called on a user-initiated source")
			return nil, nil
		},
	}
	polledNormal := false
	normal := &mockTaskSource{
		name: "dependabot",
		pollFn: func(_ context.Context, _ string) ([]*bossanovav1.TaskItem, error) {
			polledNormal = true
			return nil, nil
		},
	}

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sources = &mockTaskSourceProvider{sources: []plugin.TaskSource{userInit, normal}}
		o.repos = &mockRepoStore{repos: []*models.Repo{
			{ID: "r1", OriginURL: "repo1", CanAutoMergeDependabot: true},
		}}
	})
	orch.poll(context.Background())

	if !polledNormal {
		t.Fatal("normal source PollTasks was not called")
	}
}

func TestStart_StopsOnContextCancel(t *testing.T) {
	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.interval = 50 * time.Millisecond
	})

	ctx, cancel := context.WithCancel(context.Background())
	orch.Start(ctx)
	time.Sleep(100 * time.Millisecond)
	cancel()
	time.Sleep(100 * time.Millisecond)
}

// --- dedup tests ---

func TestProcessTask_DedupSkipsExisting(t *testing.T) {
	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{
				"dependabot:pr:repo:123": {
					ID:         "tm-1",
					ExternalID: "dependabot:pr:repo:123",
					Status:     models.TaskMappingStatusInProgress,
				},
			},
		}
	})

	orch.processTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "dependabot:pr:repo:123",
		Title:      "Bump lodash",
		Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
	}, repoInfo{id: "r1", originURL: "https://github.com/org/repo"}, "dependabot")
}

// --- routing tests ---

func TestRouteTask_AutoMerge(t *testing.T) {
	var mergedRepo string
	var mergedPR int

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, repoPath string, prID int) error {
				mergedRepo = repoPath
				mergedPR = prID
				return nil
			},
		}
		o.taskMappings = &mockTaskMappingStore{mappings: map[string]*models.TaskMapping{}}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "dependabot:pr:https://github.com/org/repo:42",
		Title:      "Bump lodash",
		Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
	}, repoInfo{id: "r1", originURL: "https://github.com/org/repo"}, "dependabot")

	if mergedRepo != "https://github.com/org/repo" {
		t.Errorf("expected repo URL, got %q", mergedRepo)
	}
	if mergedPR != 42 {
		t.Errorf("expected PR 42, got %d", mergedPR)
	}
}

func TestRouteTask_AutoMerge_MergeError(t *testing.T) {
	var updatedStatus models.TaskMappingStatus

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, _ int) error {
				return errors.New("merge conflict")
			},
		}
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			updateFn: func(_ context.Context, _ string, params db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
				if params.Status != nil {
					updatedStatus = *params.Status
				}
				return &models.TaskMapping{}, nil
			},
		}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "dependabot:pr:repo:99",
		Title:      "Bump express",
		Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
	}, repoInfo{id: "r1", originURL: "repo"}, "dependabot")

	if updatedStatus != models.TaskMappingStatusFailed {
		t.Errorf("expected status Failed, got %d", updatedStatus)
	}
}

func TestRouteTask_AutoMerge_StoresActionableFailureDetail(t *testing.T) {
	actionableErr := &vcs.ActionableError{
		Code:    vcs.ErrorCodeGitHubWorkflowScopeRequired,
		Summary: "Auto-merge blocked: GitHub token lacks workflow permission",
		Detail:  "PR #4 in freshclaim/marketing changes a file under .github/workflows.",
		Command: "gh auth refresh -h github.com -s workflow",
		Err:     errors.New("github token missing workflow scope"),
	}

	var finalStatus models.TaskMappingStatus
	var persistedLastError *string

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, _ int) error {
				return actionableErr
			},
		}
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			updateFn: func(_ context.Context, _ string, params db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
				if params.Status != nil {
					finalStatus = *params.Status
				}
				if params.LastError != nil {
					persistedLastError = *params.LastError
				}
				return &models.TaskMapping{}, nil
			},
		}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "dependabot:pr:https://github.com/freshclaim/marketing:4",
		Title:      "Bump workflow action",
		Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
	}, repoInfo{
		id:            "r1",
		originURL:     "https://github.com/freshclaim/marketing",
		mergeStrategy: "rebase",
	}, "dependabot")

	if finalStatus != models.TaskMappingStatusFailed {
		t.Errorf("expected status Failed, got %d", finalStatus)
	}
	if persistedLastError == nil {
		t.Fatal("expected LastError to be persisted")
	}
	for _, want := range []string{
		"Auto-merge blocked: GitHub token lacks workflow permission",
		"PR #4 in freshclaim/marketing changes a file under .github/workflows.",
		"Fix: gh auth refresh -h github.com -s workflow",
	} {
		if !strings.Contains(*persistedLastError, want) {
			t.Errorf("LastError missing %q: %q", want, *persistedLastError)
		}
	}
}

func TestRouteTask_AutoMerge_RebaseFailureCreatesRepairSession(t *testing.T) {
	var capturedOpts CreateSessionOpts
	var updatedStatus models.TaskMappingStatus

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, _ int) error {
				return errors.New("merge PR: gh pr merge 426 --repo recurser/bossanova --rebase --delete-branch: exit status 1: GraphQL: This branch can't be rebased (mergePullRequest)")
			},
		}
		o.sessionCreator = &mockSessionCreatorOrch{
			createFn: func(_ context.Context, opts CreateSessionOpts) (*models.Session, error) {
				capturedOpts = opts
				return &models.Session{ID: "repair-session"}, nil
			},
		}
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			updateFn: func(_ context.Context, _ string, params db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
				if params.Status != nil {
					updatedStatus = *params.Status
				}
				return &models.TaskMapping{}, nil
			},
		}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId:     "dependabot:pr:git@github.com:recurser/bossanova.git:426",
		Title:          "chore(web)(deps-dev): bump @types/react",
		Plan:           "Original dependabot plan.",
		Action:         bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
		ExistingBranch: "dependabot/npm_and_yarn/services/web/types/react-19.2.15",
		Labels:         []string{"dependabot"},
	}, repoInfo{
		id:            "r1",
		originURL:     "git@github.com:recurser/bossanova.git",
		baseBranch:    "main",
		mergeStrategy: "rebase",
	}, "dependabot")

	if capturedOpts.HeadBranch != "dependabot/npm_and_yarn/services/web/types/react-19.2.15" {
		t.Fatalf("HeadBranch = %q, want existing Dependabot branch", capturedOpts.HeadBranch)
	}
	if !strings.Contains(capturedOpts.Plan, "rebase") {
		t.Fatalf("repair plan should explicitly mention rebase, got %q", capturedOpts.Plan)
	}
	if !strings.Contains(capturedOpts.Plan, "gh pr merge") {
		t.Fatalf("repair plan should include failed merge command context, got %q", capturedOpts.Plan)
	}
	if updatedStatus != models.TaskMappingStatusInProgress {
		t.Fatalf("status = %d, want InProgress", updatedStatus)
	}
}

func TestHandleCreateSession_DependabotTaskLeavesAgentResolutionToSessionCreator(t *testing.T) {
	var capturedOpts CreateSessionOpts
	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sessionCreator = &mockSessionCreatorOrch{
			createFn: func(_ context.Context, opts CreateSessionOpts) (*models.Session, error) {
				capturedOpts = opts
				return &models.Session{ID: "session-1"}, nil
			},
		}
	})

	orch.handleCreateSession(context.Background(), &bossanovav1.TaskItem{
		ExternalId:     "dependabot:pr:git@github.com:recurser/bossanova.git:426",
		Title:          "chore(web)(deps-dev): bump @apollo/client",
		Plan:           "Repair failing Dependabot check.",
		Action:         bossanovav1.TaskAction_TASK_ACTION_CREATE_SESSION,
		ExistingBranch: "dependabot/npm_and_yarn/apollo-client-4.2.2",
		Labels:         []string{"dependabot"},
	}, repoInfo{
		id:         "r1",
		originURL:  "git@github.com:recurser/bossanova.git",
		baseBranch: "main",
	}, &models.TaskMapping{ID: "tm-1"}, false)

	if got := capturedOpts.AgentName; got != "" {
		t.Fatalf("AgentName = %q, want empty so SessionCreator applies daemon default", got)
	}
	if got := capturedOpts.SkipSetupScript; !got {
		t.Fatalf("SkipSetupScript = false, want true for dependabot task")
	}
	if got := capturedOpts.HeadBranch; got != "dependabot/npm_and_yarn/apollo-client-4.2.2" {
		t.Fatalf("HeadBranch = %q, want dependabot branch", got)
	}
	if got := capturedOpts.BaseBranch; got != "main" {
		t.Fatalf("BaseBranch = %q, want repo default main", got)
	}
}

func TestUpdateMappingStatus_ClearsLastErrorForNonFailedStatus(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Orchestrator)
	}{
		{
			name: "wrapper",
			run: func(orch *Orchestrator) {
				orch.updateMappingStatus(context.Background(), "tm-1", models.TaskMappingStatusCompleted)
			},
		},
		{
			name: "with details",
			run: func(orch *Orchestrator) {
				orch.updateMappingStatusWithDetails(context.Background(), "tm-1", models.TaskMappingStatusSkipped, "stale failure")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var captured db.UpdateTaskMappingParams

			orch := newTestOrchestrator(func(o *Orchestrator) {
				o.taskMappings = &mockTaskMappingStore{
					updateFn: func(_ context.Context, _ string, params db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
						captured = params
						return &models.TaskMapping{}, nil
					},
				}
			})

			tt.run(orch)

			if captured.Status == nil {
				t.Fatal("expected status update")
			}
			if *captured.Status == models.TaskMappingStatusFailed {
				t.Fatalf("test setup must use non-failed status, got %d", *captured.Status)
			}
			if captured.LastError == nil {
				t.Fatal("expected LastError clear sentinel to be sent")
			}
			if *captured.LastError != nil {
				t.Fatalf("expected LastError clear sentinel, got %q", **captured.LastError)
			}
		})
	}
}

// mockBaseSyncer implements BaseBranchSyncer for orchestrator tests that
// need to drive the new pre-merge check + post-merge verification paths.
type mockBaseSyncer struct {
	fetchErr      error
	ancestorFn    func(ref, target string) (bool, error)
	syncErr       error
	mergeCommits  int
	countErr      error
	syncCalls     int
	fetchCalls    int
	ancestorCalls int
	countCalls    int
}

func (m *mockBaseSyncer) SyncBaseBranch(_ context.Context, _, _ string) error {
	m.syncCalls++
	return m.syncErr
}
func (m *mockBaseSyncer) FetchBase(_ context.Context, _, _ string) error {
	m.fetchCalls++
	return m.fetchErr
}
func (m *mockBaseSyncer) IsAncestor(_ context.Context, _, ref, target string) (bool, error) {
	m.ancestorCalls++
	if m.ancestorFn != nil {
		return m.ancestorFn(ref, target)
	}
	return true, nil
}
func (m *mockBaseSyncer) CountMergeCommits(_ context.Context, _, _, _ string) (int, error) {
	m.countCalls++
	return m.mergeCommits, m.countErr
}

func TestRouteTask_AutoMerge_VerifiesCommitOnBase(t *testing.T) {
	// Regression test for the madverts-core incident: gh reported the PR
	// merged, but the merge commit is not an ancestor of origin/<base>.
	// The orchestrator must mark the task FAILED in that case rather than
	// silently marking it Completed.
	var mergeCalled bool
	var finalStatus models.TaskMappingStatus

	syncer := &mockBaseSyncer{
		ancestorFn: func(_, _ string) (bool, error) { return false, nil }, // merge commit NOT on base
	}
	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, _ int) error {
				mergeCalled = true
				return nil
			},
			mergeCommitFn: func(_ int) (string, error) { return "76b35392", nil },
		}
		o.baseSyncer = syncer
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			updateFn: func(_ context.Context, _ string, params db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
				if params.Status != nil {
					finalStatus = *params.Status
				}
				return &models.TaskMapping{}, nil
			},
		}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "dependabot:pr:https://github.com/org/repo:2222",
		Title:      "Bump foo",
		Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
	}, repoInfo{
		id:         "r1",
		originURL:  "https://github.com/org/repo",
		localPath:  "/tmp/repo",
		baseBranch: "main",
	}, "dependabot")

	if !mergeCalled {
		t.Error("MergePR should have been called")
	}
	if finalStatus != models.TaskMappingStatusFailed {
		t.Errorf("want Failed (verification caught orphaned merge), got status=%d", finalStatus)
	}
	if syncer.fetchCalls == 0 {
		t.Error("FetchBase should have been called as part of verification")
	}
	if syncer.ancestorCalls == 0 {
		t.Error("IsAncestor should have been called as part of verification")
	}
	// Sync should NOT have been attempted after verification failure —
	// there's nothing good to fast-forward to.
	if syncer.syncCalls != 0 {
		t.Errorf("SyncBaseBranch should not run after verification failure; got %d calls", syncer.syncCalls)
	}
}

func TestRouteTask_AutoMerge_NoStrategyAvailable(t *testing.T) {
	// When the remote has every merge strategy disabled, resolveMergeStrategy
	// returns an error. The orchestrator must mark the task failed BEFORE
	// attempting gh pr merge — gh would just reject with a less clear message.
	var mergeCalled bool
	var finalStatus models.TaskMappingStatus

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, _ int) error {
				mergeCalled = true
				return nil
			},
			allowedStrategiesFn: func() ([]string, error) { return []string{}, nil },
		}
		o.baseSyncer = &mockBaseSyncer{}
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			updateFn: func(_ context.Context, _ string, params db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
				if params.Status != nil {
					finalStatus = *params.Status
				}
				return &models.TaskMapping{}, nil
			},
		}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "dependabot:pr:https://github.com/org/repo:5",
		Title:      "Bump",
		Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
	}, repoInfo{
		id:            "r1",
		originURL:     "https://github.com/org/repo",
		localPath:     "/tmp/repo",
		baseBranch:    "main",
		mergeStrategy: "merge",
	}, "dependabot")

	if mergeCalled {
		t.Error("MergePR must not run when no strategy is enabled on the remote")
	}
	if finalStatus != models.TaskMappingStatusFailed {
		t.Errorf("want Failed, got %d", finalStatus)
	}
}

func TestRouteTask_AutoMerge_FallsBackStrategy(t *testing.T) {
	// Configured = "rebase" but remote only allows {merge, squash}. The
	// orchestrator must substitute "merge" (the first allowed preference)
	// when calling MergePR rather than passing the disabled config through.
	var passedStrategy string

	syncer := &mockBaseSyncer{
		ancestorFn: func(_, _ string) (bool, error) { return true, nil },
	}
	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, _ int) error { return nil },
			// mergeFn doesn't receive strategy; use allowedStrategiesFn to
			// confirm the policy picked something sensible, and intercept
			// MergePR via a custom mockProvider below.
			allowedStrategiesFn: func() ([]string, error) { return []string{"merge", "squash"}, nil },
		}
		// Wrap the provider so we can capture the strategy argument.
		orig := o.provider
		o.provider = &strategyCapturingProvider{Provider: orig, captured: &passedStrategy}
		o.baseSyncer = syncer
		o.taskMappings = &mockTaskMappingStore{mappings: map[string]*models.TaskMapping{}}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "dependabot:pr:https://github.com/org/repo:9",
		Title:      "Bump",
		Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
	}, repoInfo{
		id:            "r1",
		originURL:     "https://github.com/org/repo",
		localPath:     "/tmp/repo",
		baseBranch:    "main",
		mergeStrategy: "rebase",
	}, "dependabot")

	if passedStrategy != "merge" {
		t.Errorf("want MergePR called with strategy=merge (fallback), got %q", passedStrategy)
	}
}

// strategyCapturingProvider wraps any vcs.Provider and records the strategy
// passed to MergePR. Only MergePR is intercepted — other calls pass through.
type strategyCapturingProvider struct {
	vcs.Provider
	captured *string
}

func (p *strategyCapturingProvider) MergePR(ctx context.Context, repoPath string, prID int, strategy string) error {
	*p.captured = strategy
	return p.Provider.MergePR(ctx, repoPath, prID, strategy)
}

func TestRouteTask_AutoMerge_DefersLocalBaseSync(t *testing.T) {
	// BOS-233 (AC4): a not-fast-forwardable local base (operator's checkout is
	// dirty) must NOT abort auto-merge. The GitHub merge still runs, the
	// deferred local sync is non-fatal, and the mapping still completes.
	var mergeCalled bool
	var finalStatus models.TaskMappingStatus

	syncer := &mockBaseSyncer{
		ancestorFn: func(_, _ string) (bool, error) { return true, nil }, // merge landed on base
		syncErr:    gitpkg.ErrLocalSyncDeferred,
	}
	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, _ int) error {
				mergeCalled = true
				return nil
			},
			mergeCommitFn: func(_ int) (string, error) { return "abc123", nil },
		}
		o.baseSyncer = syncer
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			updateFn: func(_ context.Context, _ string, params db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
				if params.Status != nil {
					finalStatus = *params.Status
				}
				return &models.TaskMapping{}, nil
			},
		}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "dependabot:pr:https://github.com/org/repo:7",
		Title:      "Bump x",
		Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
	}, repoInfo{
		id:         "r1",
		originURL:  "https://github.com/org/repo",
		localPath:  "/tmp/repo",
		baseBranch: "main",
	}, "dependabot")

	if !mergeCalled {
		t.Error("MergePR must still run even when the local base sync will defer")
	}
	if syncer.syncCalls == 0 {
		t.Error("SyncBaseBranch should have been attempted post-merge")
	}
	if finalStatus != models.TaskMappingStatusCompleted {
		t.Errorf("want Completed (deferred local sync is non-fatal), got %d", finalStatus)
	}
}

func TestRouteTask_AutoMerge_SubstitutesSquashWhenBranchHasMergeCommits(t *testing.T) {
	// BOS-513: a rebase-configured repo whose Dependabot branch carries a
	// merge commit is one GitHub's rebase-merge structurally refuses. The
	// shared strategy seam must downgrade to squash BEFORE MergePR is called.
	var passedStrategy string
	var finalStatus models.TaskMappingStatus

	syncer := &mockBaseSyncer{
		ancestorFn:   func(_, _ string) (bool, error) { return true, nil },
		mergeCommits: 1,
	}
	orch := newTestOrchestrator(func(o *Orchestrator) {
		base := &mockProvider{
			mergeFn:             func(_ context.Context, _ string, _ int) error { return nil },
			mergeCommitFn:       func(_ int) (string, error) { return "abc123", nil },
			allowedStrategiesFn: func() ([]string, error) { return []string{"squash", "rebase"}, nil },
		}
		o.provider = &strategyCapturingProvider{Provider: base, captured: &passedStrategy}
		o.baseSyncer = syncer
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			updateFn: func(_ context.Context, _ string, params db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
				if params.Status != nil {
					finalStatus = *params.Status
				}
				return &models.TaskMapping{}, nil
			},
		}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId:     "dependabot:pr:https://github.com/org/repo:31",
		Title:          "Bump foo",
		Action:         bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
		ExistingBranch: "dependabot/npm_and_yarn/foo-1.2.3",
	}, repoInfo{
		id:            "r1",
		originURL:     "https://github.com/org/repo",
		localPath:     "/tmp/repo",
		baseBranch:    "main",
		mergeStrategy: "rebase",
	}, "dependabot")

	if syncer.countCalls == 0 {
		t.Error("CountMergeCommits should have been consulted for a rebase repo")
	}
	if passedStrategy != "squash" {
		t.Errorf("want MergePR called with strategy=squash, got %q", passedStrategy)
	}
	if finalStatus != models.TaskMappingStatusCompleted {
		t.Errorf("want Completed, got %d", finalStatus)
	}
}

func TestRouteTask_AutoMerge_IncompatibleStrategyQueuesRepairSession(t *testing.T) {
	// Merge commits present, rebase is the only strategy the remote allows:
	// nothing can succeed, so the pre-check must route to the SAME repair
	// path that today's post-failure rebase-refusal branch uses rather than
	// dead-ending the mapping in Failed.
	var passedStrategy string
	var capturedOpts CreateSessionOpts
	var updatedStatus models.TaskMappingStatus

	syncer := &mockBaseSyncer{mergeCommits: 2}
	orch := newTestOrchestrator(func(o *Orchestrator) {
		base := &mockProvider{
			mergeFn:             func(_ context.Context, _ string, _ int) error { return nil },
			allowedStrategiesFn: func() ([]string, error) { return []string{"rebase"}, nil },
		}
		o.provider = &strategyCapturingProvider{Provider: base, captured: &passedStrategy}
		o.baseSyncer = syncer
		o.sessionCreator = &mockSessionCreatorOrch{
			createFn: func(_ context.Context, opts CreateSessionOpts) (*models.Session, error) {
				capturedOpts = opts
				return &models.Session{ID: "repair-session"}, nil
			},
		}
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			updateFn: func(_ context.Context, _ string, params db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
				if params.Status != nil {
					updatedStatus = *params.Status
				}
				return &models.TaskMapping{}, nil
			},
		}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId:     "dependabot:pr:https://github.com/org/repo:32",
		Title:          "Bump foo",
		Plan:           "Original dependabot plan.",
		Action:         bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
		ExistingBranch: "dependabot/npm_and_yarn/foo-1.2.3",
		Labels:         []string{"dependabot"},
	}, repoInfo{
		id:            "r1",
		originURL:     "https://github.com/org/repo",
		localPath:     "/tmp/repo",
		baseBranch:    "main",
		mergeStrategy: "rebase",
	}, "dependabot")

	if passedStrategy != "" {
		t.Errorf("MergePR must not run for an incompatible strategy; got %q", passedStrategy)
	}
	if capturedOpts.HeadBranch != "dependabot/npm_and_yarn/foo-1.2.3" {
		t.Errorf("HeadBranch = %q, want the existing Dependabot branch", capturedOpts.HeadBranch)
	}
	if !strings.Contains(capturedOpts.Plan, "rebase") {
		t.Errorf("repair plan should mention rebase, got %q", capturedOpts.Plan)
	}
	if updatedStatus != models.TaskMappingStatusInProgress {
		t.Errorf("status = %d, want InProgress (repair session queued)", updatedStatus)
	}
}

func TestRouteTask_AutoMerge_KeepsRebaseWithoutMergeCommits(t *testing.T) {
	// A linear branch is exactly what rebase-merge wants — no substitution.
	var passedStrategy string

	syncer := &mockBaseSyncer{
		ancestorFn:   func(_, _ string) (bool, error) { return true, nil },
		mergeCommits: 0,
	}
	orch := newTestOrchestrator(func(o *Orchestrator) {
		base := &mockProvider{
			mergeFn:             func(_ context.Context, _ string, _ int) error { return nil },
			allowedStrategiesFn: func() ([]string, error) { return []string{"squash", "rebase"}, nil },
		}
		o.provider = &strategyCapturingProvider{Provider: base, captured: &passedStrategy}
		o.baseSyncer = syncer
		o.taskMappings = &mockTaskMappingStore{mappings: map[string]*models.TaskMapping{}}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId:     "dependabot:pr:https://github.com/org/repo:33",
		Title:          "Bump foo",
		Action:         bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
		ExistingBranch: "dependabot/npm_and_yarn/foo-1.2.3",
	}, repoInfo{
		id:            "r1",
		originURL:     "https://github.com/org/repo",
		localPath:     "/tmp/repo",
		baseBranch:    "main",
		mergeStrategy: "rebase",
	}, "dependabot")

	if passedStrategy != "rebase" {
		t.Errorf("want MergePR called with strategy=rebase, got %q", passedStrategy)
	}
}

func TestRouteTask_AutoMerge_NilBaseSyncerFailsOpenToRebase(t *testing.T) {
	// baseSyncer is optional; with no counter the seam must fail open to the
	// resolved strategy rather than panicking or blocking the merge.
	var passedStrategy string

	orch := newTestOrchestrator(func(o *Orchestrator) {
		base := &mockProvider{
			mergeFn:             func(_ context.Context, _ string, _ int) error { return nil },
			allowedStrategiesFn: func() ([]string, error) { return []string{"squash", "rebase"}, nil },
		}
		o.provider = &strategyCapturingProvider{Provider: base, captured: &passedStrategy}
		o.baseSyncer = nil
		o.taskMappings = &mockTaskMappingStore{mappings: map[string]*models.TaskMapping{}}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId:     "dependabot:pr:https://github.com/org/repo:34",
		Title:          "Bump foo",
		Action:         bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
		ExistingBranch: "dependabot/npm_and_yarn/foo-1.2.3",
	}, repoInfo{
		id:            "r1",
		originURL:     "https://github.com/org/repo",
		localPath:     "/tmp/repo",
		baseBranch:    "main",
		mergeStrategy: "rebase",
	}, "dependabot")

	if passedStrategy != "rebase" {
		t.Errorf("want MergePR called with strategy=rebase (fail open), got %q", passedStrategy)
	}
}

func TestRouteTask_CreateSession(t *testing.T) {
	var capturedOpts CreateSessionOpts

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sessionCreator = &mockSessionCreatorOrch{
			createFn: func(_ context.Context, opts CreateSessionOpts) (*models.Session, error) {
				capturedOpts = opts
				return &models.Session{ID: "sess-new"}, nil
			},
		}
		o.taskMappings = &mockTaskMappingStore{mappings: map[string]*models.TaskMapping{}}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId:     "dependabot:pr:repo:55",
		Title:          "Bump react",
		Plan:           "Fix failing tests",
		BaseBranch:     "develop",
		ExistingBranch: "dependabot/npm/react-18.3.0",
		Action:         bossanovav1.TaskAction_TASK_ACTION_CREATE_SESSION,
	}, repoInfo{id: "r1", originURL: "https://github.com/org/repo"}, "dependabot")

	if capturedOpts.RepoID != "r1" {
		t.Errorf("expected repo r1, got %q", capturedOpts.RepoID)
	}
	if capturedOpts.Title != "Bump react" {
		t.Errorf("expected title 'Bump react', got %q", capturedOpts.Title)
	}
	if capturedOpts.Plan != "Fix failing tests" {
		t.Errorf("expected plan, got %q", capturedOpts.Plan)
	}
	if capturedOpts.BaseBranch != "develop" {
		t.Errorf("expected base branch 'develop', got %q", capturedOpts.BaseBranch)
	}
	if capturedOpts.HeadBranch != "dependabot/npm/react-18.3.0" {
		t.Errorf("expected head branch, got %q", capturedOpts.HeadBranch)
	}
}

func TestRouteTask_CreateSession_DefaultBaseBranch(t *testing.T) {
	var capturedOpts CreateSessionOpts

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sessionCreator = &mockSessionCreatorOrch{
			createFn: func(_ context.Context, opts CreateSessionOpts) (*models.Session, error) {
				capturedOpts = opts
				return &models.Session{ID: "sess-new"}, nil
			},
		}
		o.taskMappings = &mockTaskMappingStore{mappings: map[string]*models.TaskMapping{}}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "task:1",
		Title:      "Fix bug",
		Action:     bossanovav1.TaskAction_TASK_ACTION_CREATE_SESSION,
		// BaseBranch intentionally empty
	}, repoInfo{id: "r1", originURL: "repo"}, "dependabot")

	if capturedOpts.BaseBranch != "main" {
		t.Errorf("expected default base branch 'main', got %q", capturedOpts.BaseBranch)
	}
}

func TestRouteTask_NotifyUser(t *testing.T) {
	var updatedStatus models.TaskMappingStatus

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			updateFn: func(_ context.Context, _ string, params db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
				if params.Status != nil {
					updatedStatus = *params.Status
				}
				return &models.TaskMapping{}, nil
			},
		}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "task:notify:1",
		Title:      "Previously rejected library",
		Action:     bossanovav1.TaskAction_TASK_ACTION_NOTIFY_USER,
	}, repoInfo{id: "r1", originURL: "repo"}, "dependabot")

	if updatedStatus != models.TaskMappingStatusSkipped {
		t.Errorf("expected status Skipped, got %d", updatedStatus)
	}
}

func TestRouteTask_UnspecifiedDefaultsToCreateSession(t *testing.T) {
	sessionCreated := false

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sessionCreator = &mockSessionCreatorOrch{
			createFn: func(_ context.Context, _ CreateSessionOpts) (*models.Session, error) {
				sessionCreated = true
				return &models.Session{ID: "sess-new"}, nil
			},
		}
		o.taskMappings = &mockTaskMappingStore{mappings: map[string]*models.TaskMapping{}}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "task:unspec:1",
		Title:      "Unspecified action",
		Action:     bossanovav1.TaskAction_TASK_ACTION_UNSPECIFIED,
	}, repoInfo{id: "r1", originURL: "repo"}, "dependabot")

	if !sessionCreated {
		t.Error("UNSPECIFIED action should default to CREATE_SESSION")
	}
}

// --- queue tests ---

func TestQueue_TasksProcessedInOrder(t *testing.T) {
	var (
		mu        sync.Mutex
		processed []string
	)

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, prID int) error {
				mu.Lock()
				processed = append(processed, fmt.Sprintf("pr-%d", prID))
				mu.Unlock()
				return nil
			},
		}
		o.taskMappings = &mockTaskMappingStore{mappings: map[string]*models.TaskMapping{}}
	})

	ctx := context.Background()

	// Pre-seed the per-repo slot and queue, then call dequeueNext directly
	// to exercise FIFO ordering without going through enqueue. AUTO_MERGE
	// is used in the queued task because dequeueNext routes through
	// handleAutoMerge, which only needs the provider mock above (no
	// session creator needed).
	orch.mu.Lock()
	orch.active["r1"] = true
	orch.queues["r1"] = []queuedTask{
		{
			task: &bossanovav1.TaskItem{
				ExternalId: "dep:pr:repo:2",
				Title:      "Second",
				Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
			},
			repo:       repoInfo{id: "r1", originURL: "repo"},
			pluginName: "dependabot",
		},
	}
	qLen := len(orch.queues["r1"])
	orch.mu.Unlock()

	if qLen != 1 {
		t.Fatalf("expected 1 queued task, got %d", qLen)
	}
	mu.Lock()
	if len(processed) != 0 {
		mu.Unlock()
		t.Fatalf("expected 0 processed, got %d", len(processed))
	}
	mu.Unlock()

	// Now dequeue — simulates first task completing.
	orch.dequeueNext(ctx, "r1")

	mu.Lock()
	defer mu.Unlock()
	if len(processed) != 1 {
		t.Fatalf("expected 1 processed after dequeue, got %d", len(processed))
	}
	if processed[0] != "pr-2" {
		t.Errorf("expected pr-2, got %s", processed[0])
	}
}

func TestQueue_DequeueEmptyMarksInactive(t *testing.T) {
	orch := newTestOrchestrator()

	orch.mu.Lock()
	orch.active["r1"] = true
	orch.mu.Unlock()

	orch.dequeueNext(context.Background(), "r1")

	orch.mu.Lock()
	active := orch.active["r1"]
	orch.mu.Unlock()

	if active {
		t.Error("expected repo to be inactive after empty dequeue")
	}
}

// --- completion callback tests ---

func TestHandleSessionCompleted_UpdatesPlugin(t *testing.T) {
	var updatedExternalID string
	var updatedStatus bossanovav1.TaskItemStatus

	src := &mockTaskSource{
		pollFn: func(_ context.Context, _ string) ([]*bossanovav1.TaskItem, error) {
			return nil, nil
		},
	}
	captureSrc := &updatingMockTaskSource{
		mockTaskSource: *src,
		updateFn: func(_ context.Context, externalID string, status bossanovav1.TaskItemStatus, _ string) error {
			updatedExternalID = externalID
			updatedStatus = status
			return nil
		},
	}

	sessionID := "sess-abc"
	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sources = &mockTaskSourceProvider{sources: []plugin.TaskSource{captureSrc}}
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			bySession: map[string]*models.TaskMapping{
				sessionID: {
					ID:         "tm-1",
					ExternalID: "dep:pr:repo:10",
					RepoID:     "r1",
					Status:     models.TaskMappingStatusInProgress,
				},
			},
		}
	})

	orch.HandleSessionCompleted(context.Background(), sessionID, models.TaskMappingStatusCompleted)

	if updatedExternalID != "dep:pr:repo:10" {
		t.Errorf("expected external ID 'dep:pr:repo:10', got %q", updatedExternalID)
	}
	if updatedStatus != bossanovav1.TaskItemStatus_TASK_ITEM_STATUS_COMPLETED {
		t.Errorf("expected COMPLETED status, got %v", updatedStatus)
	}
}

func TestHandleSessionCompleted_NoMapping(t *testing.T) {
	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
		}
	})

	// Should not panic for sessions without a task mapping.
	orch.HandleSessionCompleted(context.Background(), "unknown-session", models.TaskMappingStatusCompleted)
}

func TestHandleSessionCompleted_PluginError_StoresPending(t *testing.T) {
	var storedPending bool

	sessionID := "sess-fail"
	captureSrc := &updatingMockTaskSource{
		mockTaskSource: mockTaskSource{
			pollFn: func(_ context.Context, _ string) ([]*bossanovav1.TaskItem, error) {
				return nil, nil
			},
		},
		updateFn: func(_ context.Context, _ string, _ bossanovav1.TaskItemStatus, _ string) error {
			return errors.New("plugin crashed")
		},
	}

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sources = &mockTaskSourceProvider{sources: []plugin.TaskSource{captureSrc}}
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			bySession: map[string]*models.TaskMapping{
				sessionID: {
					ID:         "tm-2",
					ExternalID: "dep:pr:repo:20",
					RepoID:     "r1",
					Status:     models.TaskMappingStatusInProgress,
				},
			},
			updateFn: func(_ context.Context, _ string, params db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
				if params.PendingUpdateStatus != nil {
					storedPending = true
				}
				return &models.TaskMapping{}, nil
			},
		}
	})

	orch.HandleSessionCompleted(context.Background(), sessionID, models.TaskMappingStatusCompleted)

	if !storedPending {
		t.Error("expected pending update to be stored when plugin fails")
	}
}

func TestHandleSessionCompleted_AlreadyTerminal_Skips(t *testing.T) {
	// If a mapping is already in a terminal state (e.g. Completed from a prior
	// PR merge notification), a duplicate call (e.g. from RemoveSession) must
	// be a no-op — no status overwrite, no plugin notification, no dequeue.
	var pluginCalled bool

	sessionID := "sess-dup"
	captureSrc := &updatingMockTaskSource{
		mockTaskSource: mockTaskSource{
			pollFn: func(_ context.Context, _ string) ([]*bossanovav1.TaskItem, error) {
				return nil, nil
			},
		},
		updateFn: func(_ context.Context, _ string, _ bossanovav1.TaskItemStatus, _ string) error {
			pluginCalled = true
			return nil
		},
	}

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sources = &mockTaskSourceProvider{sources: []plugin.TaskSource{captureSrc}}
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			bySession: map[string]*models.TaskMapping{
				sessionID: {
					ID:         "tm-dup",
					ExternalID: "dep:pr:repo:30",
					RepoID:     "r1",
					Status:     models.TaskMappingStatusCompleted, // already terminal
				},
			},
		}
	})

	// Second call with Failed should be silently ignored.
	orch.HandleSessionCompleted(context.Background(), sessionID, models.TaskMappingStatusFailed)

	if pluginCalled {
		t.Error("plugin should NOT be notified when mapping is already terminal")
	}
}

func TestHandleSessionCompleted_DoubleCall_SecondIsNoop(t *testing.T) {
	// Simulate a PR merge (dispatcher) followed by RemoveSession (server).
	// Only the first call should update the plugin; the second should be a no-op.
	var pluginUpdateCount int

	sessionID := "sess-double"
	mapping := &models.TaskMapping{
		ID:         "tm-double",
		ExternalID: "dep:pr:repo:40",
		RepoID:     "r1",
		Status:     models.TaskMappingStatusInProgress,
	}

	captureSrc := &updatingMockTaskSource{
		mockTaskSource: mockTaskSource{
			pollFn: func(_ context.Context, _ string) ([]*bossanovav1.TaskItem, error) {
				return nil, nil
			},
		},
		updateFn: func(_ context.Context, _ string, _ bossanovav1.TaskItemStatus, _ string) error {
			pluginUpdateCount++
			return nil
		},
	}

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sources = &mockTaskSourceProvider{sources: []plugin.TaskSource{captureSrc}}
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			bySession: map[string]*models.TaskMapping{
				sessionID: mapping,
			},
			updateFn: func(_ context.Context, _ string, params db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
				if params.Status != nil {
					// Simulate the DB update so the second lookup sees the new status.
					mapping.Status = *params.Status
				}
				return mapping, nil
			},
		}
	})

	// First call: Completed from dispatcher (PR merge).
	orch.HandleSessionCompleted(context.Background(), sessionID, models.TaskMappingStatusCompleted)

	// Second call: Failed from server (RemoveSession).
	orch.HandleSessionCompleted(context.Background(), sessionID, models.TaskMappingStatusFailed)

	if pluginUpdateCount != 1 {
		t.Errorf("expected plugin to be notified exactly once, got %d", pluginUpdateCount)
	}
	// Verify the status wasn't overwritten: mapping should still be Completed.
	if mapping.Status != models.TaskMappingStatusCompleted {
		t.Errorf("expected mapping status to remain Completed, got %v", mapping.Status)
	}
}

func TestHandleSessionCompleted_ConcurrentCalls_OnlyOneProceeds(t *testing.T) {
	// Two goroutines call HandleSessionCompleted at the same time for the
	// same session. The in-memory guard must ensure only one proceeds.
	var pluginUpdateCount atomic.Int32

	sessionID := "sess-race"
	captureSrc := &updatingMockTaskSource{
		mockTaskSource: mockTaskSource{
			pollFn: func(_ context.Context, _ string) ([]*bossanovav1.TaskItem, error) {
				return nil, nil
			},
		},
		updateFn: func(_ context.Context, _ string, _ bossanovav1.TaskItemStatus, _ string) error {
			pluginUpdateCount.Add(1)
			return nil
		},
	}

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sources = &mockTaskSourceProvider{sources: []plugin.TaskSource{captureSrc}}
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			bySession: map[string]*models.TaskMapping{
				sessionID: {
					ID:         "tm-race",
					ExternalID: "dep:pr:repo:50",
					RepoID:     "r1",
					Status:     models.TaskMappingStatusInProgress,
				},
			},
		}
	})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		orch.HandleSessionCompleted(context.Background(), sessionID, models.TaskMappingStatusCompleted)
	}()
	go func() {
		defer wg.Done()
		orch.HandleSessionCompleted(context.Background(), sessionID, models.TaskMappingStatusFailed)
	}()
	wg.Wait()

	if count := pluginUpdateCount.Load(); count != 1 {
		t.Errorf("expected plugin to be notified exactly once, got %d", count)
	}
}

func TestHandleSessionCompleted_DoesNotDeleteNewerMapping(t *testing.T) {
	// Regression test: if a new task has already replaced the activeMapping
	// for a repo, the old completion must not delete the newer entry.
	sessionID := "sess-old"
	src := &updatingMockTaskSource{
		mockTaskSource: mockTaskSource{
			pollFn: func(_ context.Context, _ string) ([]*bossanovav1.TaskItem, error) {
				return nil, nil
			},
		},
		updateFn: func(_ context.Context, _ string, _ bossanovav1.TaskItemStatus, _ string) error {
			return nil
		},
	}

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sources = &mockTaskSourceProvider{sources: []plugin.TaskSource{src}}
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			bySession: map[string]*models.TaskMapping{
				sessionID: {
					ID:         "tm-old",
					ExternalID: "dep:pr:repo:60",
					RepoID:     "r1",
					Status:     models.TaskMappingStatusInProgress,
				},
			},
		}
		// Simulate a newer task already owning the activeMapping slot.
		o.activeMapping["r1"] = "tm-new"
	})

	orch.HandleSessionCompleted(context.Background(), sessionID, models.TaskMappingStatusCompleted)

	// The newer mapping must survive.
	orch.mu.Lock()
	got, ok := orch.activeMapping["r1"]
	orch.mu.Unlock()

	if !ok || got != "tm-new" {
		t.Errorf("expected activeMapping[r1] = 'tm-new', got %q (exists=%v)", got, ok)
	}
}

// --- SessionArchiver seam tests ---

func TestSetSessionArchiver_StoresArchiver(t *testing.T) {
	o := newTestOrchestrator()
	ctx := context.Background()

	called := false
	stub := SessionArchiverFunc(func(_ context.Context, _ string) error {
		called = true
		return nil
	})

	o.SetSessionArchiver(stub, nil)
	if err := o.archiver.ArchiveSession(ctx, "s"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !called {
		t.Error("expected SessionArchiverFunc to be called via o.archiver")
	}
}

func TestHandleSessionCompleted_ArchivesDependabotOnMerge(t *testing.T) {
	done := make(chan struct{}, 1)
	sessionID := "sess-dep-merge"

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			bySession: map[string]*models.TaskMapping{
				sessionID: {
					ID:         "tm-dep",
					ExternalID: "dep:pr:repo:10",
					RepoID:     "r1",
					Status:     models.TaskMappingStatusInProgress,
					PluginName: "dependabot",
					SessionID:  &sessionID,
				},
			},
		}
		o.archiver = SessionArchiverFunc(func(_ context.Context, id string) error {
			if id == sessionID {
				select {
				case done <- struct{}{}:
				default:
				}
			}
			return nil
		})
	})

	orch.HandleSessionCompleted(context.Background(), sessionID, models.TaskMappingStatusCompleted)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for archiver to be called")
	}
}

// TestHandleSessionCompleted_TracksArchiveWorkerHandle pins the BOS-923 fix at
// the fourth archive launch point. The dependabot auto-archive runs on a
// detached context.Background(), so discarding safego.Go's completion channel
// left it outside shutdownWG exactly like the three session-package sites. The
// handle must reach the tracker, stay open while the archive is in flight, and
// close when it finishes.
func TestHandleSessionCompleted_TracksArchiveWorkerHandle(t *testing.T) {
	sessionID := "sess-dep-tracked"
	entered := make(chan struct{})
	release := make(chan struct{})
	handles := make(chan (<-chan struct{}), 4)

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			bySession: map[string]*models.TaskMapping{
				sessionID: {
					ID:         "tm-dep",
					ExternalID: "dep:pr:repo:10",
					RepoID:     "r1",
					Status:     models.TaskMappingStatusInProgress,
					PluginName: "dependabot",
					SessionID:  &sessionID,
				},
			},
		}
		o.SetSessionArchiver(SessionArchiverFunc(func(_ context.Context, _ string) error {
			close(entered)
			<-release
			return nil
		}), func(id string, done <-chan struct{}) {
			if id != sessionID {
				t.Errorf("tracker got session %q, want %q", id, sessionID)
			}
			handles <- done
		})
	})

	if !orch.HasArchiveTracker() {
		t.Fatal("HasArchiveTracker() = false after SetSessionArchiver with a tracker")
	}

	orch.HandleSessionCompleted(context.Background(), sessionID, models.TaskMappingStatusCompleted)

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("archiver was never entered")
	}

	var done <-chan struct{}
	select {
	case done = <-handles:
	case <-time.After(2 * time.Second):
		t.Fatal("archive worker handle was never handed to the tracker")
	}

	// Open while the archive is blocked: this is what proves the handle
	// represents the archive rather than the launch.
	select {
	case <-done:
		t.Fatal("tracked handle closed while the archive was still blocked")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tracked handle never closed after the archive completed")
	}
}

func TestHandleSessionCompleted_DoesNotArchiveNonDependabot(t *testing.T) {
	sessionID := "sess-linear"
	called := make(chan struct{}, 1)

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			bySession: map[string]*models.TaskMapping{
				sessionID: {
					ID:         "tm-linear",
					ExternalID: "linear:pr:repo:20",
					RepoID:     "r1",
					Status:     models.TaskMappingStatusInProgress,
					PluginName: "linear",
					SessionID:  &sessionID,
				},
			},
		}
		o.archiver = SessionArchiverFunc(func(_ context.Context, _ string) error {
			select {
			case called <- struct{}{}:
			default:
			}
			return nil
		})
	})

	orch.HandleSessionCompleted(context.Background(), sessionID, models.TaskMappingStatusCompleted)

	select {
	case <-called:
		t.Error("archiver must NOT be called for non-dependabot sessions")
	case <-time.After(300 * time.Millisecond):
		// expected — archiver was not called
	}
}

func TestHandleSessionCompleted_DoesNotArchiveOnFailedOutcome(t *testing.T) {
	sessionID := "sess-dep-failed"
	called := make(chan struct{}, 1)

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			bySession: map[string]*models.TaskMapping{
				sessionID: {
					ID:         "tm-dep-failed",
					ExternalID: "dep:pr:repo:30",
					RepoID:     "r1",
					Status:     models.TaskMappingStatusInProgress,
					PluginName: "dependabot",
					SessionID:  &sessionID,
				},
			},
		}
		o.archiver = SessionArchiverFunc(func(_ context.Context, _ string) error {
			select {
			case called <- struct{}{}:
			default:
			}
			return nil
		})
	})

	orch.HandleSessionCompleted(context.Background(), sessionID, models.TaskMappingStatusFailed)

	select {
	case <-called:
		t.Error("archiver must NOT be called on Failed outcome")
	case <-time.After(300 * time.Millisecond):
		// expected — archiver was not called
	}
}

func TestHandleSessionCompleted_NilArchiverDoesNotPanic(t *testing.T) {
	sessionID := "sess-dep-noarchiver"

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			bySession: map[string]*models.TaskMapping{
				sessionID: {
					ID:         "tm-dep-noarch",
					ExternalID: "dep:pr:repo:40",
					RepoID:     "r1",
					Status:     models.TaskMappingStatusInProgress,
					PluginName: "dependabot",
					SessionID:  &sessionID,
				},
			},
		}
		// o.archiver is nil — the gate must handle this gracefully
	})

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("HandleSessionCompleted panicked: %v", r)
		}
	}()

	orch.HandleSessionCompleted(context.Background(), sessionID, models.TaskMappingStatusCompleted)
}

// updatingMockTaskSource wraps mockTaskSource with a custom UpdateTaskStatus.
type updatingMockTaskSource struct {
	mockTaskSource
	updateFn func(ctx context.Context, externalID string, status bossanovav1.TaskItemStatus, details string) error
}

func (m *updatingMockTaskSource) UpdateTaskStatus(ctx context.Context, externalID string, status bossanovav1.TaskItemStatus, details string) error {
	return m.updateFn(ctx, externalID, status, details)
}

// --- dedup tests (additional) ---

func TestProcessTask_NewTaskPassesThrough(t *testing.T) {
	var createdMapping atomic.Bool
	created := make(chan struct{}, 1)

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{}, // empty — no existing mapping
			createFn: func(_ context.Context, params db.CreateTaskMappingParams) (*models.TaskMapping, error) {
				if createdMapping.CompareAndSwap(false, true) {
					created <- struct{}{}
				}
				return &models.TaskMapping{
					ID:         "tm-new",
					ExternalID: params.ExternalID,
					PluginName: params.PluginName,
					RepoID:     params.RepoID,
					Status:     models.TaskMappingStatusPending,
				}, nil
			},
		}
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, _ int) error { return nil },
		}
	})

	// AUTO_MERGE runs routeTask in a goroutine (see enqueue), so wait on
	// a sync channel before asserting state set inside that goroutine.
	orch.processTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "dependabot:pr:repo:999",
		Title:      "Bump new-pkg",
		Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
	}, repoInfo{id: "r1", originURL: "repo"}, "dependabot")

	select {
	case <-created:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for task mapping create")
	}

	if !createdMapping.Load() {
		t.Error("expected new task to create a mapping (not be deduped)")
	}
}

// TestProcessTask_OrphanedMappingIsReprocessed proves that mappings left
// in the Orphaned state by FailOrphanedMappings (e.g. after a daemon
// restart) are picked up again on the next poll instead of being silently
// dropped by the dedup. The stale row is deleted so routeTask's Create()
// doesn't trip the external_id UNIQUE constraint.
func TestProcessTask_OrphanedMappingIsReprocessed(t *testing.T) {
	var (
		mu             sync.Mutex
		deletedID      string
		createdMapping bool
	)
	created := make(chan struct{}, 1)

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{
				"dependabot:pr:repo:777": {
					ID:         "tm-orphan",
					ExternalID: "dependabot:pr:repo:777",
					Status:     models.TaskMappingStatusOrphaned,
				},
			},
			deleteFn: func(_ context.Context, id string) error {
				mu.Lock()
				deletedID = id
				mu.Unlock()
				return nil
			},
			createFn: func(_ context.Context, params db.CreateTaskMappingParams) (*models.TaskMapping, error) {
				mu.Lock()
				createdMapping = true
				mu.Unlock()
				select {
				case created <- struct{}{}:
				default:
				}
				return &models.TaskMapping{
					ID:         "tm-new",
					ExternalID: params.ExternalID,
					PluginName: params.PluginName,
					RepoID:     params.RepoID,
					Status:     models.TaskMappingStatusPending,
				}, nil
			},
		}
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, _ int) error { return nil },
		}
	})

	// AUTO_MERGE runs routeTask in a goroutine (see enqueue) — wait on a
	// sync channel before asserting state set inside that goroutine.
	orch.processTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "dependabot:pr:repo:777",
		Title:      "Bump foo",
		Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
	}, repoInfo{id: "r1", originURL: "repo"}, "dependabot")

	select {
	case <-created:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for task mapping create")
	}

	mu.Lock()
	defer mu.Unlock()
	if deletedID != "tm-orphan" {
		t.Errorf("expected stale Orphaned mapping to be deleted, got deletedID=%q", deletedID)
	}
	if !createdMapping {
		t.Error("expected new mapping to be created after Orphaned re-process")
	}
}

func TestProcessTask_FailedAutoMergeMappingRetried(t *testing.T) {
	var (
		mu             sync.Mutex
		deletedID      string
		createdMapping bool
	)
	created := make(chan struct{}, 1)

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{
				"dependabot:pr:repo:888": {
					ID:         "tm-failed",
					ExternalID: "dependabot:pr:repo:888",
					Status:     models.TaskMappingStatusFailed,
				},
			},
			deleteFn: func(_ context.Context, id string) error {
				mu.Lock()
				deletedID = id
				mu.Unlock()
				return nil
			},
			createFn: func(_ context.Context, params db.CreateTaskMappingParams) (*models.TaskMapping, error) {
				mu.Lock()
				createdMapping = true
				mu.Unlock()
				select {
				case created <- struct{}{}:
				default:
				}
				return &models.TaskMapping{
					ID:         "tm-new",
					ExternalID: params.ExternalID,
					PluginName: params.PluginName,
					RepoID:     params.RepoID,
					Status:     models.TaskMappingStatusPending,
				}, nil
			},
		}
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, _ int) error { return nil },
		}
	})

	orch.processTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "dependabot:pr:repo:888",
		Title:      "Bump bar",
		Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
	}, repoInfo{id: "r1", originURL: "repo"}, "dependabot")

	select {
	case <-created:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for task mapping create")
	}

	mu.Lock()
	defer mu.Unlock()
	if deletedID != "tm-failed" {
		t.Errorf("expected stale failed AUTO_MERGE mapping to be deleted, got deletedID=%q", deletedID)
	}
	if !createdMapping {
		t.Error("expected new mapping to be created after failed AUTO_MERGE retry")
	}
}

func TestProcessTask_TerminalMappingReRepairsAfterCooldown(t *testing.T) {
	var mappedSessionExternalID string
	var mu sync.Mutex

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{
				"dependabot:pr:repo:888": {
					ID:         "tm-failed",
					ExternalID: "dependabot:pr:repo:888",
					Status:     models.TaskMappingStatusFailed, // UpdatedAt zero → cooldown elapsed
					RetryCount: 0,
				},
			},
		}
		o.sessionCreator = &mockSessionCreatorOrch{
			createFn: func(_ context.Context, opts CreateSessionOpts) (*models.Session, error) {
				mu.Lock()
				mappedSessionExternalID = "dependabot:pr:repo:888"
				mu.Unlock()
				return &models.Session{ID: "s1"}, nil
			},
		}
	})

	orch.processTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "dependabot:pr:repo:888",
		Title:      "Bump bar",
		Action:     bossanovav1.TaskAction_TASK_ACTION_CREATE_SESSION,
	}, repoInfo{id: "r1", originURL: "repo"}, "dependabot")

	mu.Lock()
	defer mu.Unlock()
	if mappedSessionExternalID != "dependabot:pr:repo:888" {
		t.Errorf("expected re-repair session for dependabot:pr:repo:888, got %q", mappedSessionExternalID)
	}
}

func TestProcessTask_ReRepairCapEscalatesToNotify(t *testing.T) {
	var skippedID string
	var sessionCreated bool
	var mu sync.Mutex

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{
				"dependabot:pr:repo:777": {
					ID:         "tm-exhausted",
					ExternalID: "dependabot:pr:repo:777",
					Status:     models.TaskMappingStatusFailed,
					RetryCount: MaxReRepairAttempts,
				},
			},
			updateFn: func(_ context.Context, id string, p db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
				if p.Status != nil && *p.Status == models.TaskMappingStatusSkipped {
					mu.Lock()
					skippedID = id
					mu.Unlock()
				}
				return &models.TaskMapping{ID: id}, nil
			},
		}
		o.sessionCreator = &mockSessionCreatorOrch{
			createFn: func(_ context.Context, _ CreateSessionOpts) (*models.Session, error) {
				mu.Lock()
				sessionCreated = true
				mu.Unlock()
				return &models.Session{ID: "s2"}, nil
			},
		}
	})

	orch.processTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "dependabot:pr:repo:777",
		Action:     bossanovav1.TaskAction_TASK_ACTION_CREATE_SESSION,
	}, repoInfo{id: "r1", originURL: "repo"}, "dependabot")

	mu.Lock()
	defer mu.Unlock()
	if sessionCreated {
		t.Error("expected NO new session once re-repair attempts are exhausted")
	}
	if skippedID != "tm-exhausted" {
		t.Errorf("expected mapping to be marked Skipped, got %q", skippedID)
	}
}

func TestProcessTask_CompletedMappingReprocessedByAutoMerge(t *testing.T) {
	var deletedID string
	var createdMapping bool
	var mu sync.Mutex
	created := make(chan struct{}, 1)

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{
				"dependabot:pr:repo:999": {
					ID:         "tm-completed",
					ExternalID: "dependabot:pr:repo:999",
					Status:     models.TaskMappingStatusCompleted,
				},
			},
			deleteFn: func(_ context.Context, id string) error {
				mu.Lock()
				deletedID = id
				mu.Unlock()
				return nil
			},
			createFn: func(_ context.Context, _ db.CreateTaskMappingParams) (*models.TaskMapping, error) {
				mu.Lock()
				createdMapping = true
				mu.Unlock()
				select {
				case created <- struct{}{}:
				default:
				}
				return &models.TaskMapping{ID: "tm-new"}, nil
			},
		}
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, _ int) error { return nil },
		}
	})

	orch.processTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "dependabot:pr:repo:999",
		Title:      "Bump baz",
		Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
	}, repoInfo{id: "r1", originURL: "repo"}, "dependabot")

	select {
	case <-created:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for task mapping create")
	}

	mu.Lock()
	defer mu.Unlock()
	if deletedID != "tm-completed" {
		t.Errorf("expected stale Completed mapping to be deleted, got %q", deletedID)
	}
	if !createdMapping {
		t.Error("expected a fresh AUTO_MERGE mapping to be created")
	}
}

func TestRouteTask_CreateMappingError(t *testing.T) {
	var mergedCalled bool

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			createFn: func(_ context.Context, _ db.CreateTaskMappingParams) (*models.TaskMapping, error) {
				return nil, errors.New("db constraint violation")
			},
		}
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, _ int) error {
				mergedCalled = true
				return nil
			},
		}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "dep:pr:repo:50",
		Title:      "Should not merge",
		Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
	}, repoInfo{id: "r1", originURL: "repo"}, "dependabot")

	if mergedCalled {
		t.Error("merge should not be called when task mapping creation fails")
	}
}

// --- queue tests (additional) ---

func TestQueue_DifferentReposProcessIndependently(t *testing.T) {
	var (
		mu        sync.Mutex
		processed []string
	)
	done := make(chan struct{}, 2)

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, repoPath string, prID int) error {
				mu.Lock()
				processed = append(processed, fmt.Sprintf("%s:pr-%d", repoPath, prID))
				mu.Unlock()
				done <- struct{}{}
				return nil
			},
		}
		// Custom createFn — avoids concurrent map writes from the two
		// goroutines launched by AUTO_MERGE.
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			createFn: func(_ context.Context, params db.CreateTaskMappingParams) (*models.TaskMapping, error) {
				return &models.TaskMapping{
					ID:         "tm-" + params.ExternalID,
					ExternalID: params.ExternalID,
					PluginName: params.PluginName,
					RepoID:     params.RepoID,
					Status:     models.TaskMappingStatusPending,
				}, nil
			},
		}
	})

	ctx := context.Background()

	// Enqueue tasks for two different repos. AUTO_MERGE bypasses the
	// per-repo lock and runs in goroutines (see enqueue), so wait on a
	// channel before asserting. Order between repos is non-deterministic
	// now — sort before comparing.
	orch.enqueue(ctx, &bossanovav1.TaskItem{
		ExternalId: "dep:pr:repo-a:1",
		Title:      "Repo A task",
		Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
	}, repoInfo{id: "r1", originURL: "repo-a"}, "dependabot")

	orch.enqueue(ctx, &bossanovav1.TaskItem{
		ExternalId: "dep:pr:repo-b:2",
		Title:      "Repo B task",
		Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
	}, repoInfo{id: "r2", originURL: "repo-b"}, "dependabot")

	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for auto-merge goroutines (%d/2 done)", i)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(processed) != 2 {
		t.Fatalf("expected 2 tasks processed (one per repo), got %d: %v", len(processed), processed)
	}
	// Goroutine ordering is non-deterministic; just assert both ran.
	gotA, gotB := false, false
	for _, p := range processed {
		if p == "repo-a:pr-1" {
			gotA = true
		}
		if p == "repo-b:pr-2" {
			gotB = true
		}
	}
	if !gotA {
		t.Errorf("expected repo-a:pr-1 in processed, got %v", processed)
	}
	if !gotB {
		t.Errorf("expected repo-b:pr-2 in processed, got %v", processed)
	}
}

// --- retry pending tests ---

func TestRetryPendingUpdates_SuccessClearsPending(t *testing.T) {
	var clearedPending bool

	pendingStatus := models.TaskMappingStatusCompleted
	captureSrc := &updatingMockTaskSource{
		mockTaskSource: mockTaskSource{
			pollFn: func(_ context.Context, _ string) ([]*bossanovav1.TaskItem, error) {
				return nil, nil
			},
		},
		updateFn: func(_ context.Context, _ string, _ bossanovav1.TaskItemStatus, _ string) error {
			return nil // success
		},
	}

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sources = &mockTaskSourceProvider{sources: []plugin.TaskSource{captureSrc}}
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			listPendingFn: func(_ context.Context) ([]*models.TaskMapping, error) {
				return []*models.TaskMapping{
					{
						ID:                  "tm-pend",
						ExternalID:          "dep:pr:repo:30",
						RepoID:              "r1",
						PendingUpdateStatus: &pendingStatus,
					},
				}, nil
			},
			updateFn: func(_ context.Context, _ string, params db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
				// Check that PendingUpdateStatus is being cleared (set to nil via double pointer).
				if params.PendingUpdateStatus != nil && *params.PendingUpdateStatus == nil {
					clearedPending = true
				}
				return &models.TaskMapping{}, nil
			},
		}
	})

	orch.RetryPendingUpdates(context.Background())

	if !clearedPending {
		t.Error("expected pending update to be cleared after successful retry")
	}
}

func TestRetryPendingUpdates_StillFailingKeepsPending(t *testing.T) {
	var updateCalled bool

	pendingStatus := models.TaskMappingStatusCompleted
	captureSrc := &updatingMockTaskSource{
		mockTaskSource: mockTaskSource{
			pollFn: func(_ context.Context, _ string) ([]*bossanovav1.TaskItem, error) {
				return nil, nil
			},
		},
		updateFn: func(_ context.Context, _ string, _ bossanovav1.TaskItemStatus, _ string) error {
			return errors.New("plugin still down")
		},
	}

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sources = &mockTaskSourceProvider{sources: []plugin.TaskSource{captureSrc}}
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			listPendingFn: func(_ context.Context) ([]*models.TaskMapping, error) {
				return []*models.TaskMapping{
					{
						ID:                  "tm-pend",
						ExternalID:          "dep:pr:repo:30",
						RepoID:              "r1",
						PendingUpdateStatus: &pendingStatus,
					},
				}, nil
			},
			updateFn: func(_ context.Context, _ string, _ db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
				updateCalled = true
				return &models.TaskMapping{}, nil
			},
		}
	})

	orch.RetryPendingUpdates(context.Background())

	if updateCalled {
		t.Error("task mapping should not be updated when retry still fails")
	}
}

// --- error handling tests (additional) ---

func TestRouteTask_CreateSession_Error(t *testing.T) {
	var updatedStatus models.TaskMappingStatus

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sessionCreator = &mockSessionCreatorOrch{
			createFn: func(_ context.Context, _ CreateSessionOpts) (*models.Session, error) {
				return nil, errors.New("lifecycle busy")
			},
		}
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			updateFn: func(_ context.Context, _ string, params db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
				if params.Status != nil {
					updatedStatus = *params.Status
				}
				return &models.TaskMapping{}, nil
			},
		}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "dep:pr:repo:77",
		Title:      "Bump failing-pkg",
		Action:     bossanovav1.TaskAction_TASK_ACTION_CREATE_SESSION,
	}, repoInfo{id: "r1", originURL: "repo"}, "dependabot")

	if updatedStatus != models.TaskMappingStatusFailed {
		t.Errorf("expected status Failed when session creation fails, got %d", updatedStatus)
	}
}

func TestRouteTask_CreateSession_DuplicateActiveSessionDeletesMapping(t *testing.T) {
	var updatedStatus models.TaskMappingStatus
	var statusUpdated bool
	var deletedMappingID string
	prNumber := 77

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sessionCreator = &mockSessionCreatorOrch{
			createFn: func(_ context.Context, _ CreateSessionOpts) (*models.Session, error) {
				return nil, fmt.Errorf("create session: %w", &session.DuplicateActivePRSessionError{
					ExistingSessionID: "sess-existing",
					RepoID:            "r1",
					PRNumber:          prNumber,
				})
			},
		}
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			updateFn: func(_ context.Context, _ string, params db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
				if params.Status != nil {
					updatedStatus = *params.Status
					statusUpdated = true
				}
				return &models.TaskMapping{}, nil
			},
			deleteFn: func(_ context.Context, id string) error {
				deletedMappingID = id
				return nil
			},
		}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "dependabot:pr:repo:77",
		Title:      "Bump failing-pkg",
		Action:     bossanovav1.TaskAction_TASK_ACTION_CREATE_SESSION,
	}, repoInfo{id: "r1", originURL: "repo"}, "dependabot")

	if statusUpdated {
		t.Errorf("expected no terminal status update when duplicate session exists, got %d", updatedStatus)
	}
	if deletedMappingID == "" {
		t.Fatal("expected duplicate task mapping to be deleted")
	}
}

func TestHandleCreateSession_AutoMergeFallbackDuplicatePreservesMappingCooldown(t *testing.T) {
	prNumber := 77
	var updatedStatus models.TaskMappingStatus
	var persistedLastError *string
	var deletedMappingID string

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sessionCreator = &mockSessionCreatorOrch{
			createFn: func(_ context.Context, _ CreateSessionOpts) (*models.Session, error) {
				return nil, fmt.Errorf("create session: %w", &session.DuplicateActivePRSessionError{
					ExistingSessionID: "sess-existing",
					RepoID:            "r1",
					PRNumber:          prNumber,
				})
			},
		}
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			updateFn: func(_ context.Context, _ string, params db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
				if params.Status != nil {
					updatedStatus = *params.Status
				}
				if params.LastError != nil {
					persistedLastError = *params.LastError
				}
				return &models.TaskMapping{}, nil
			},
			deleteFn: func(_ context.Context, id string) error {
				deletedMappingID = id
				return nil
			},
		}
	})

	orch.handleCreateSession(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "dependabot:pr:repo:77",
		Title:      "Repair rebase: Bump failing-pkg",
		Action:     bossanovav1.TaskAction_TASK_ACTION_CREATE_SESSION,
		Labels:     []string{"dependabot"},
	}, repoInfo{id: "r1", originURL: "repo"}, &models.TaskMapping{ID: "tm-auto-merge"}, true)

	if updatedStatus != models.TaskMappingStatusFailed {
		t.Fatalf("status = %d, want Failed to preserve auto-merge retry cooldown", updatedStatus)
	}
	if persistedLastError == nil || !strings.Contains(*persistedLastError, "sess-existing") {
		t.Fatalf("LastError = %v, want duplicate session details", persistedLastError)
	}
	if deletedMappingID != "" {
		t.Fatalf("mapping was deleted: %q", deletedMappingID)
	}
}

func TestRouteTask_CreateSession_Error_DequeuesNext(t *testing.T) {
	dequeued := false

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sessionCreator = &mockSessionCreatorOrch{
			createFn: func(_ context.Context, _ CreateSessionOpts) (*models.Session, error) {
				return nil, errors.New("lifecycle busy")
			},
		}
		o.taskMappings = &mockTaskMappingStore{mappings: map[string]*models.TaskMapping{}}
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, _ int) error {
				dequeued = true
				return nil
			},
		}
	})

	ctx := context.Background()

	// Mark repo active and queue a second task.
	orch.mu.Lock()
	orch.active["r1"] = true
	orch.queues["r1"] = []queuedTask{{
		task: &bossanovav1.TaskItem{
			ExternalId: "dep:pr:repo:2",
			Title:      "Queued task",
			Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
		},
		repo:       repoInfo{id: "r1", originURL: "repo"},
		pluginName: "dependabot",
	}}
	orch.mu.Unlock()

	// This will fail to create session and should dequeue the next task.
	orch.routeTask(ctx, &bossanovav1.TaskItem{
		ExternalId: "dep:pr:repo:1",
		Title:      "Failing session",
		Action:     bossanovav1.TaskAction_TASK_ACTION_CREATE_SESSION,
	}, repoInfo{id: "r1", originURL: "repo"}, "dependabot")

	if !dequeued {
		t.Error("expected dequeueNext to process queued task after session creation failure")
	}
}

func TestRouteTask_MappingError_DequeuesNext(t *testing.T) {
	dequeued := false
	createCalls := 0

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{},
			createFn: func(_ context.Context, params db.CreateTaskMappingParams) (*models.TaskMapping, error) {
				createCalls++
				if createCalls == 1 {
					return nil, errors.New("db constraint violation")
				}
				return &models.TaskMapping{
					ID:         "tm-" + params.ExternalID,
					ExternalID: params.ExternalID,
					PluginName: params.PluginName,
					RepoID:     params.RepoID,
				}, nil
			},
		}
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, _ int) error {
				dequeued = true
				return nil
			},
		}
	})

	ctx := context.Background()

	// Mark repo active and queue a second task.
	orch.mu.Lock()
	orch.active["r1"] = true
	orch.queues["r1"] = []queuedTask{{
		task: &bossanovav1.TaskItem{
			ExternalId: "dep:pr:repo:2",
			Title:      "Queued task",
			Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
		},
		repo:       repoInfo{id: "r1", originURL: "repo"},
		pluginName: "dependabot",
	}}
	orch.mu.Unlock()

	// This will fail to create mapping and should dequeue the next task.
	orch.routeTask(ctx, &bossanovav1.TaskItem{
		ExternalId: "dep:pr:repo:1",
		Title:      "Mapping fail",
		Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
	}, repoInfo{id: "r1", originURL: "repo"}, "dependabot")

	if !dequeued {
		t.Error("expected dequeueNext to process queued task after mapping creation failure")
	}
}

// --- SkipSetupScript tests ---

func TestRouteTask_CreateSession_DependabotLabel_SetsSkipSetupScript(t *testing.T) {
	var capturedOpts CreateSessionOpts

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sessionCreator = &mockSessionCreatorOrch{
			createFn: func(_ context.Context, opts CreateSessionOpts) (*models.Session, error) {
				capturedOpts = opts
				return &models.Session{ID: "sess-new"}, nil
			},
		}
		o.taskMappings = &mockTaskMappingStore{mappings: map[string]*models.TaskMapping{}}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId:     "dependabot:pr:repo:55",
		Title:          "Bump react",
		Plan:           "Fix failing tests",
		BaseBranch:     "main",
		ExistingBranch: "dependabot/npm/react-18.3.0",
		Action:         bossanovav1.TaskAction_TASK_ACTION_CREATE_SESSION,
		Labels:         []string{"dependabot", "npm"},
	}, repoInfo{id: "r1", originURL: "https://github.com/org/repo"}, "dependabot")

	if !capturedOpts.SkipSetupScript {
		t.Error("expected SkipSetupScript=true for task with dependabot label")
	}
	if !capturedOpts.PreventDuplicateActiveSession {
		t.Error("expected PreventDuplicateActiveSession=true for task with dependabot label")
	}
}

func TestRouteTask_CreateSession_NoDependabotLabel_NoSkipSetupScript(t *testing.T) {
	var capturedOpts CreateSessionOpts

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sessionCreator = &mockSessionCreatorOrch{
			createFn: func(_ context.Context, opts CreateSessionOpts) (*models.Session, error) {
				capturedOpts = opts
				return &models.Session{ID: "sess-new"}, nil
			},
		}
		o.taskMappings = &mockTaskMappingStore{mappings: map[string]*models.TaskMapping{}}
	})

	orch.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "linear:issue:ABC-123:7",
		Title:      "Fix login bug",
		Plan:       "Debug and fix",
		BaseBranch: "main",
		Action:     bossanovav1.TaskAction_TASK_ACTION_CREATE_SESSION,
		Labels:     []string{"bug", "high-priority"},
	}, repoInfo{id: "r1", originURL: "https://github.com/org/repo"}, "linear")

	if capturedOpts.SkipSetupScript {
		t.Error("expected SkipSetupScript=false for task without dependabot label")
	}
	if capturedOpts.PreventDuplicateActiveSession {
		t.Error("expected PreventDuplicateActiveSession=false for task without dependabot label")
	}
}

// --- failed task mapping tests ---

// Note: the former TestProcessTask_FailedCreateSessionMappingIsSkipped was
// removed in BOS-45. It asserted the old tombstone behavior (a Failed +
// CREATE_SESSION mapping is permanently skipped), which the terminal-mapping
// re-repair carve-out reverses. That scenario is now covered by
// TestProcessTask_TerminalMappingReRepairsAfterCooldown (re-repair fires once
// the cooldown elapses) and TestProcessTask_ReRepairCapEscalatesToNotify (the
// attempt cap escalates to NOTIFY_USER).

func TestProcessTask_CompletedMappingStillSkipped(t *testing.T) {
	// A Completed mapping with a recent UpdatedAt (within the cooldown window)
	// must NOT be re-processed even for AUTO_MERGE.
	var mu sync.Mutex
	createCalls := 0

	recentTime := time.Now().Add(-1 * time.Minute) // well within TerminalRetryCooldown
	store := &mockTaskMappingStore{
		mappings: map[string]*models.TaskMapping{
			"dep:pr:repo:88": {
				ID:         "tm-done",
				ExternalID: "dep:pr:repo:88",
				PluginName: "dependabot",
				RepoID:     "r1",
				Status:     models.TaskMappingStatusCompleted,
				UpdatedAt:  recentTime,
			},
		},
		createFn: func(_ context.Context, _ db.CreateTaskMappingParams) (*models.TaskMapping, error) {
			mu.Lock()
			createCalls++
			mu.Unlock()
			return &models.TaskMapping{}, nil
		},
	}

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = store
	})

	orch.processTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "dep:pr:repo:88",
		Title:      "Bump completed-pkg",
		Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
	}, repoInfo{id: "r1", originURL: "repo"}, "dependabot")

	mu.Lock()
	defer mu.Unlock()
	if createCalls != 0 {
		t.Error("expected completed task mapping within cooldown to still be skipped (not retried)")
	}
}

// --- queue deduplication tests ---

func TestQueue_DuplicateExternalIDNotQueued(t *testing.T) {
	orch := newTestOrchestrator()

	ctx := context.Background()

	// Mark repo active so tasks go to the queue rather than being processed.
	orch.mu.Lock()
	orch.active["r1"] = true
	orch.mu.Unlock()

	// Use CREATE_SESSION so the per-repo queue path (and its
	// deduplication) actually runs — AUTO_MERGE / NOTIFY_USER bypass
	// the queue entirely (see enqueue).
	task := &bossanovav1.TaskItem{
		ExternalId: "dep:pr:repo:42",
		Title:      "Bump some-pkg",
		Action:     bossanovav1.TaskAction_TASK_ACTION_CREATE_SESSION,
	}
	repo := repoInfo{id: "r1", displayName: "org/repo", originURL: "repo"}

	// Enqueue the same task twice.
	orch.enqueue(ctx, task, repo, "dependabot")
	orch.enqueue(ctx, task, repo, "dependabot")

	orch.mu.Lock()
	queueLen := len(orch.queues["r1"])
	orch.mu.Unlock()

	if queueLen != 1 {
		t.Errorf("expected queue length 1 after duplicate enqueue, got %d", queueLen)
	}
}

// --- recovery sweep tests ---

func TestRecoverStaleTasks_DeadSession_UnblocksQueue(t *testing.T) {
	sessionID := "sess-dead"
	mappingID := "tm-stuck"
	var completedSessionID string

	mapping := &models.TaskMapping{
		ID:        mappingID,
		RepoID:    "r1",
		Status:    models.TaskMappingStatusInProgress,
		SessionID: &sessionID,
	}

	captureSrc := &updatingMockTaskSource{
		mockTaskSource: mockTaskSource{
			pollFn: func(_ context.Context, _ string) ([]*bossanovav1.TaskItem, error) {
				return nil, nil
			},
		},
		updateFn: func(_ context.Context, _ string, _ bossanovav1.TaskItemStatus, _ string) error {
			return nil
		},
	}

	store := &mockTaskMappingStore{
		mappings: map[string]*models.TaskMapping{},
		byID: map[string]*models.TaskMapping{
			mappingID: mapping,
		},
		bySession: map[string]*models.TaskMapping{
			sessionID: mapping,
		},
		updateFn: func(_ context.Context, id string, params db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
			if params.Status != nil {
				mapping.Status = *params.Status
			}
			return mapping, nil
		},
	}

	checker := &mockLivenessChecker{
		livenessFn: func(_ context.Context, sid string) session.Liveness {
			completedSessionID = sid
			return session.LivenessDead
		},
	}

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.sources = &mockTaskSourceProvider{sources: []plugin.TaskSource{captureSrc}}
		o.taskMappings = store
		o.livenessChecker = checker
	})

	// Set up the active mapping as if a CREATE_SESSION was in progress.
	orch.mu.Lock()
	orch.active["r1"] = true
	orch.activeMapping["r1"] = mappingID
	orch.mu.Unlock()

	orch.recoverStaleTasks(context.Background())

	if completedSessionID != sessionID {
		t.Errorf("expected liveness check for session %q, got %q", sessionID, completedSessionID)
	}

	// After recovery, the active mapping should be cleared.
	orch.mu.Lock()
	_, hasActive := orch.activeMapping["r1"]
	orch.mu.Unlock()
	if hasActive {
		t.Error("expected activeMapping to be cleared after recovery")
	}
}

func TestRecoverStaleTasks_AliveSession_NoOp(t *testing.T) {
	sessionID := "sess-alive"
	mappingID := "tm-alive"

	mapping := &models.TaskMapping{
		ID:        mappingID,
		RepoID:    "r1",
		Status:    models.TaskMappingStatusInProgress,
		SessionID: &sessionID,
	}

	store := &mockTaskMappingStore{
		mappings: map[string]*models.TaskMapping{},
		byID: map[string]*models.TaskMapping{
			mappingID: mapping,
		},
	}

	checker := &mockLivenessChecker{
		livenessFn: func(_ context.Context, _ string) session.Liveness {
			return session.LivenessAlive
		},
	}

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = store
		o.livenessChecker = checker
	})

	orch.mu.Lock()
	orch.active["r1"] = true
	orch.activeMapping["r1"] = mappingID
	orch.mu.Unlock()

	orch.recoverStaleTasks(context.Background())

	// Active mapping should still be there — session is alive.
	orch.mu.Lock()
	_, hasActive := orch.activeMapping["r1"]
	orch.mu.Unlock()
	if !hasActive {
		t.Error("expected activeMapping to remain when session is alive")
	}
}

func TestRecoverStaleTasks_MappingNotFound_ClearsActive(t *testing.T) {
	store := &mockTaskMappingStore{
		mappings: map[string]*models.TaskMapping{},
		byID:     map[string]*models.TaskMapping{}, // empty — mapping not found
	}

	dequeued := false
	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = store
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, _ int) error {
				dequeued = true
				return nil
			},
		}
	})

	// Set up active state with a missing mapping.
	orch.mu.Lock()
	orch.active["r1"] = true
	orch.activeMapping["r1"] = "tm-missing"
	orch.queues["r1"] = []queuedTask{{
		task: &bossanovav1.TaskItem{
			ExternalId: "dep:pr:repo:5",
			Title:      "Queued task",
			Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
		},
		repo:       repoInfo{id: "r1", originURL: "repo"},
		pluginName: "dependabot",
	}}
	orch.mu.Unlock()

	orch.recoverStaleTasks(context.Background())

	// Active mapping should be cleared.
	orch.mu.Lock()
	_, hasActive := orch.activeMapping["r1"]
	orch.mu.Unlock()
	if hasActive {
		t.Error("expected activeMapping to be cleared when mapping not found")
	}

	// Queued task should have been dequeued.
	if !dequeued {
		t.Error("expected queued task to be processed after clearing stale state")
	}
}

func TestRecoverStaleTasks_AlreadyCompleted_Dequeues(t *testing.T) {
	mappingID := "tm-done"

	mapping := &models.TaskMapping{
		ID:     mappingID,
		RepoID: "r1",
		Status: models.TaskMappingStatusCompleted, // already terminal
	}

	store := &mockTaskMappingStore{
		mappings: map[string]*models.TaskMapping{},
		byID: map[string]*models.TaskMapping{
			mappingID: mapping,
		},
	}

	dequeued := false
	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = store
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, _ int) error {
				dequeued = true
				return nil
			},
		}
	})

	orch.mu.Lock()
	orch.active["r1"] = true
	orch.activeMapping["r1"] = mappingID
	orch.queues["r1"] = []queuedTask{{
		task: &bossanovav1.TaskItem{
			ExternalId: "dep:pr:repo:6",
			Title:      "Queued task",
			Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
		},
		repo:       repoInfo{id: "r1", originURL: "repo"},
		pluginName: "dependabot",
	}}
	orch.mu.Unlock()

	orch.recoverStaleTasks(context.Background())

	orch.mu.Lock()
	_, hasActive := orch.activeMapping["r1"]
	orch.mu.Unlock()
	if hasActive {
		t.Error("expected activeMapping to be cleared for terminal mapping")
	}

	if !dequeued {
		t.Error("expected queued task to be processed after clearing terminal mapping")
	}
}

func TestRecoverStaleTasks_MappingReplaced_Skips(t *testing.T) {
	// If HandleSessionCompleted runs concurrently and replaces the
	// activeMapping for a repo between the snapshot and the DB lookup,
	// recoverStaleTasks must not clear the new mapping or double-dequeue.
	oldMappingID := "tm-old"
	newMappingID := "tm-new"

	store := &mockTaskMappingStore{
		mappings: map[string]*models.TaskMapping{},
		byID: map[string]*models.TaskMapping{
			oldMappingID: {
				ID:     oldMappingID,
				RepoID: "r1",
				Status: models.TaskMappingStatusCompleted,
			},
		},
	}

	var orch *Orchestrator
	// Simulate a concurrent HandleSessionCompleted replacing the mapping
	// when the DB lookup happens (after the snapshot, during processing).
	store.getFn = func(_ context.Context, id string) (*models.TaskMapping, error) {
		if id == oldMappingID {
			// Before returning, simulate concurrent completion replacing the mapping.
			orch.mu.Lock()
			orch.activeMapping["r1"] = newMappingID
			orch.mu.Unlock()
			return store.byID[id], nil
		}
		return nil, fmt.Errorf("not found")
	}

	orch = newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = store
	})

	// Set up the initial active mapping.
	orch.mu.Lock()
	orch.active["r1"] = true
	orch.activeMapping["r1"] = oldMappingID
	orch.mu.Unlock()

	orch.recoverStaleTasks(context.Background())

	// The new mapping should not have been cleared.
	orch.mu.Lock()
	currentMapping := orch.activeMapping["r1"]
	orch.mu.Unlock()
	if currentMapping != newMappingID {
		t.Errorf("expected activeMapping to remain %q, got %q", newMappingID, currentMapping)
	}
}

// --- parsePRNumberFromExternalID tests ---

func TestParsePRNumberFromExternalID(t *testing.T) {
	tests := []struct {
		input   string
		wantPR  int
		wantErr bool
	}{
		{"dependabot:pr:https://github.com/org/repo:42", 42, false},
		{"dependabot:pr:repo:1", 1, false},
		{"linear:issue:ABC-123:7", 7, false},
		{"notenough", 0, true},
		{"prefix:notanumber", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parsePRNumberFromExternalID(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, tt.wantErr)
			}
			if got != tt.wantPR {
				t.Errorf("got %d, want %d", got, tt.wantPR)
			}
		})
	}
}

// TestEnqueue_AutoMergeBypassesActiveLock verifies that an AUTO_MERGE task
// for a repo with an active CREATE_SESSION-held slot is routed immediately
// rather than queued. Regression test for the madverts-core stuck-queue
// incident (2026-05-08).
func TestEnqueue_AutoMergeBypassesActiveLock(t *testing.T) {
	t.Parallel()

	var mergeCalls atomic.Int32

	o := newTestOrchestrator(func(o *Orchestrator) {
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, _ int) error {
				mergeCalls.Add(1)
				return nil
			},
		}
		o.taskMappings = &mockTaskMappingStore{mappings: map[string]*models.TaskMapping{}}
	})

	repo := repoInfo{id: "repo-1", displayName: "test-repo", originURL: "git@github.com:o/r.git"}

	// Simulate a CREATE_SESSION already holding the lock.
	o.mu.Lock()
	o.active[repo.id] = true
	o.activeMapping[repo.id] = "stuck-mapping-id"
	o.mu.Unlock()

	autoMerge := &bossanovav1.TaskItem{
		ExternalId: "dependabot:pr:git@github.com:o/r.git:42",
		Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
	}

	o.enqueue(context.Background(), autoMerge, repo, "dependabot")

	// Auto-merge should be running in a goroutine — give it up to 2s.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && mergeCalls.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	if mergeCalls.Load() != 1 {
		t.Fatalf("expected MergePR called once (auto-merge bypassed lock); got %d", mergeCalls.Load())
	}

	o.mu.Lock()
	stillActive := o.active[repo.id]
	stillMapping := o.activeMapping[repo.id]
	o.mu.Unlock()
	if !stillActive {
		t.Fatal("CREATE_SESSION slot was released by AUTO_MERGE — this would let two sessions race")
	}
	if stillMapping != "stuck-mapping-id" {
		t.Fatalf("activeMapping changed from stuck-mapping-id to %q", stillMapping)
	}
}

// TestEnqueue_MultipleAutoMergesAreRateLimited verifies that AUTO_MERGE tasks
// still bypass the repo session lock, but the actual GitHub merge calls are
// serialized to avoid secondary rate limits during Dependabot bursts.
func TestEnqueue_MultipleAutoMergesAreRateLimited(t *testing.T) {
	t.Parallel()

	const n = 5
	entered := make(chan struct{}, n)
	release := make(chan struct{}, n)
	var inflight atomic.Int32
	var maxInflight atomic.Int32

	o := newTestOrchestrator(func(o *Orchestrator) {
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, _ int) error {
				cur := inflight.Add(1)
				for {
					m := maxInflight.Load()
					if cur <= m || maxInflight.CompareAndSwap(m, cur) {
						break
					}
				}
				entered <- struct{}{}
				<-release
				inflight.Add(-1)
				return nil
			},
		}
		o.taskMappings = &mockTaskMappingStore{mappings: map[string]*models.TaskMapping{}}
	})

	repo := repoInfo{id: "repo-1", displayName: "test-repo", originURL: "git@github.com:o/r.git"}

	for i := 0; i < n; i++ {
		task := &bossanovav1.TaskItem{
			ExternalId: fmt.Sprintf("dependabot:pr:git@github.com:o/r.git:%d", i+1),
			Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
		}
		o.enqueue(context.Background(), task, repo, "dependabot")
	}

	for i := 0; i < n; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for merge %d to start", i+1)
		}
		if got := maxInflight.Load(); got != 1 {
			t.Fatalf("expected max 1 in-flight merge, got %d", got)
		}
		release <- struct{}{}
	}

	if got := maxInflight.Load(); got != 1 {
		t.Fatalf("expected serialized merges, max observed was %d", got)
	}
}

// TestEnqueue_AutoMergeDoesNotDequeueQueuedSession is the integrity test:
// when a CREATE_SESSION holds the slot and another CREATE_SESSION is queued
// behind it, an AUTO_MERGE arriving in between must NOT cause the queued
// session to start prematurely. The auto-merge must leave both
// o.active[repo.id] and o.queues[repo.id] untouched.
func TestEnqueue_AutoMergeDoesNotDequeueQueuedSession(t *testing.T) {
	t.Parallel()

	mergeDone := make(chan struct{})
	o := newTestOrchestrator(func(o *Orchestrator) {
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, _ int) error {
				defer close(mergeDone)
				return nil
			},
		}
		o.taskMappings = &mockTaskMappingStore{mappings: map[string]*models.TaskMapping{}}
	})

	repo := repoInfo{id: "repo-1", displayName: "test-repo", originURL: "git@github.com:o/r.git"}

	queuedSession := &bossanovav1.TaskItem{
		ExternalId: "linear:issue:LIN-1",
		Action:     bossanovav1.TaskAction_TASK_ACTION_CREATE_SESSION,
	}
	o.mu.Lock()
	o.active[repo.id] = true
	o.activeMapping[repo.id] = "session-in-flight"
	o.queues[repo.id] = []queuedTask{
		{task: queuedSession, repo: repo, pluginName: "linear"},
	}
	o.mu.Unlock()

	autoMerge := &bossanovav1.TaskItem{
		ExternalId: "dependabot:pr:git@github.com:o/r.git:42",
		Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
	}

	o.enqueue(context.Background(), autoMerge, repo, "dependabot")

	select {
	case <-mergeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("auto-merge did not run within 2s")
	}

	// Give any (incorrect) follow-on dequeue a moment to misbehave.
	time.Sleep(50 * time.Millisecond)

	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.active[repo.id] {
		t.Fatal("CREATE_SESSION slot was released after auto-merge — orchestrator may now run two sessions concurrently")
	}
	if o.activeMapping[repo.id] != "session-in-flight" {
		t.Fatalf("activeMapping unexpectedly changed to %q", o.activeMapping[repo.id])
	}
	if len(o.queues[repo.id]) != 1 {
		t.Fatalf("expected queued CREATE_SESSION to remain in queue; got %d queued items", len(o.queues[repo.id]))
	}
	if o.queues[repo.id][0].task.GetExternalId() != "linear:issue:LIN-1" {
		t.Fatalf("queued task changed; got %q", o.queues[repo.id][0].task.GetExternalId())
	}
}

func TestAutoMergeRebaseRepairQueuesBehindActiveSession(t *testing.T) {
	t.Parallel()

	var createCalls atomic.Int32
	o := newTestOrchestrator(func(o *Orchestrator) {
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, _ int) error {
				return errors.New("pull request can't be rebased")
			},
		}
		o.sessionCreator = &mockSessionCreatorOrch{
			createFn: func(_ context.Context, _ CreateSessionOpts) (*models.Session, error) {
				createCalls.Add(1)
				return &models.Session{ID: "repair-session"}, nil
			},
		}
		o.taskMappings = &mockTaskMappingStore{mappings: map[string]*models.TaskMapping{}}
	})

	repo := repoInfo{
		id:            "repo-1",
		displayName:   "test-repo",
		originURL:     "https://github.com/org/repo",
		baseBranch:    "main",
		mergeStrategy: "rebase",
	}

	o.mu.Lock()
	o.active[repo.id] = true
	o.activeMapping[repo.id] = "session-in-flight"
	o.mu.Unlock()

	o.routeTask(context.Background(), &bossanovav1.TaskItem{
		ExternalId: "dependabot:pr:https://github.com/org/repo:2222",
		Title:      "Bump foo",
		Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
	}, repo, "dependabot")

	if createCalls.Load() != 0 {
		t.Fatalf("repair session started while another CREATE_SESSION held the slot; got %d starts", createCalls.Load())
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.active[repo.id] {
		t.Fatal("CREATE_SESSION slot was released by queued rebase repair")
	}
	if o.activeMapping[repo.id] != "session-in-flight" {
		t.Fatalf("activeMapping unexpectedly changed to %q", o.activeMapping[repo.id])
	}
	if len(o.queues[repo.id]) != 1 {
		t.Fatalf("expected one queued rebase repair task; got %d", len(o.queues[repo.id]))
	}

	queued := o.queues[repo.id][0]
	if queued.mapping == nil {
		t.Fatal("queued repair task lost its existing task mapping")
	}
	if queued.mapping.ExternalID != "dependabot:pr:https://github.com/org/repo:2222" {
		t.Fatalf("queued repair mapping external ID = %q", queued.mapping.ExternalID)
	}
	if queued.task.GetAction() != bossanovav1.TaskAction_TASK_ACTION_CREATE_SESSION {
		t.Fatalf("queued repair action = %s", queued.task.GetAction())
	}
	if !strings.HasPrefix(queued.task.GetTitle(), "Repair rebase: ") {
		t.Fatalf("queued repair title = %q", queued.task.GetTitle())
	}
}
