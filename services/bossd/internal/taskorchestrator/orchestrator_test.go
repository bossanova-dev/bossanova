package taskorchestrator

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/plugin"
)

// --- mock types ---

type mockTaskSourceProvider struct {
	sources []plugin.TaskSource
}

func (m *mockTaskSourceProvider) GetTaskSources() []plugin.TaskSource {
	return m.sources
}

type mockTaskSource struct {
	pollFn func(ctx context.Context, repoOriginURL string) ([]*bossanovav1.TaskItem, error)
}

func (m *mockTaskSource) GetInfo(_ context.Context) (*bossanovav1.PluginInfo, error) {
	return &bossanovav1.PluginInfo{Name: "test-plugin"}, nil
}

func (m *mockTaskSource) PollTasks(ctx context.Context, repoOriginURL string) ([]*bossanovav1.TaskItem, error) {
	return m.pollFn(ctx, repoOriginURL)
}

func (m *mockTaskSource) UpdateTaskStatus(_ context.Context, _ string, _ bossanovav1.TaskItemStatus, _ string) error {
	return nil
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
	mappings      map[string]*models.TaskMapping // keyed by external_id
	bySession     map[string]*models.TaskMapping // keyed by session_id
	createFn      func(ctx context.Context, params db.CreateTaskMappingParams) (*models.TaskMapping, error)
	updateFn      func(ctx context.Context, id string, params db.UpdateTaskMappingParams) (*models.TaskMapping, error)
	listPendingFn func(ctx context.Context) ([]*models.TaskMapping, error)
	nextID        int
}

func (m *mockTaskMappingStore) Create(ctx context.Context, params db.CreateTaskMappingParams) (*models.TaskMapping, error) {
	if m.createFn != nil {
		return m.createFn(ctx, params)
	}
	m.nextID++
	tm := &models.TaskMapping{
		ID:         "tm-" + params.ExternalID,
		ExternalID: params.ExternalID,
		PluginName: params.PluginName,
		RepoID:     params.RepoID,
		Status:     models.TaskMappingStatusPending,
	}
	if m.mappings != nil {
		m.mappings[params.ExternalID] = tm
	}
	return tm, nil
}

func (m *mockTaskMappingStore) GetByExternalID(_ context.Context, externalID string) (*models.TaskMapping, error) {
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
	return &models.TaskMapping{ID: id}, nil
}

func (m *mockTaskMappingStore) ListPending(ctx context.Context) ([]*models.TaskMapping, error) {
	if m.listPendingFn != nil {
		return m.listPendingFn(ctx)
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
	mergeFn func(ctx context.Context, repoPath string, prID int) error
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

func (m *mockProvider) MergePR(ctx context.Context, repoPath string, prID int, strategy string) error {
	if m.mergeFn != nil {
		return m.mergeFn(ctx, repoPath, prID)
	}
	return nil
}

// helper to create an orchestrator with defaults
func newTestOrchestrator(opts ...func(*Orchestrator)) *Orchestrator {
	o := New(
		&mockTaskSourceProvider{sources: nil},
		&mockRepoStore{},
		&mockTaskMappingStore{mappings: map[string]*models.TaskMapping{}},
		&mockSessionCreatorOrch{},
		&mockProvider{},
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
	var processed []string

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, _ string, prID int) error {
				processed = append(processed, fmt.Sprintf("pr-%d", prID))
				return nil
			},
		}
		o.taskMappings = &mockTaskMappingStore{mappings: map[string]*models.TaskMapping{}}
	})

	ctx := context.Background()

	// Manually mark repo as active so second task gets queued.
	orch.mu.Lock()
	orch.active["r1"] = true
	orch.mu.Unlock()

	// This should be queued (repo busy).
	orch.enqueue(ctx, &bossanovav1.TaskItem{
		ExternalId: "dep:pr:repo:2",
		Title:      "Second",
		Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
	}, repoInfo{id: "r1", originURL: "repo"}, "dependabot")

	// Verify it's queued, not processed.
	orch.mu.Lock()
	qLen := len(orch.queues["r1"])
	orch.mu.Unlock()
	if qLen != 1 {
		t.Fatalf("expected 1 queued task, got %d", qLen)
	}
	if len(processed) != 0 {
		t.Fatalf("expected 0 processed, got %d", len(processed))
	}

	// Now dequeue — simulates first task completing.
	orch.dequeueNext(ctx, "r1")

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
	var createdMapping bool

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.taskMappings = &mockTaskMappingStore{
			mappings: map[string]*models.TaskMapping{}, // empty — no existing mapping
			createFn: func(_ context.Context, params db.CreateTaskMappingParams) (*models.TaskMapping, error) {
				createdMapping = true
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
		ExternalId: "dependabot:pr:repo:999",
		Title:      "Bump new-pkg",
		Action:     bossanovav1.TaskAction_TASK_ACTION_AUTO_MERGE,
	}, repoInfo{id: "r1", originURL: "repo"}, "dependabot")

	if !createdMapping {
		t.Error("expected new task to create a mapping (not be deduped)")
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
	var processed []string

	orch := newTestOrchestrator(func(o *Orchestrator) {
		o.provider = &mockProvider{
			mergeFn: func(_ context.Context, repoPath string, prID int) error {
				processed = append(processed, fmt.Sprintf("%s:pr-%d", repoPath, prID))
				return nil
			},
		}
		o.taskMappings = &mockTaskMappingStore{mappings: map[string]*models.TaskMapping{}}
	})

	ctx := context.Background()

	// Enqueue tasks for two different repos — both should process immediately.
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

	if len(processed) != 2 {
		t.Fatalf("expected 2 tasks processed (one per repo), got %d: %v", len(processed), processed)
	}
	if processed[0] != "repo-a:pr-1" {
		t.Errorf("expected repo-a:pr-1, got %s", processed[0])
	}
	if processed[1] != "repo-b:pr-2" {
		t.Errorf("expected repo-b:pr-2, got %s", processed[1])
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
