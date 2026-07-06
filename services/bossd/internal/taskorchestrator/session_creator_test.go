package taskorchestrator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
)

// mockSessionStore implements db.SessionStore for testing.
type mockSessionStore struct {
	createFn             func(ctx context.Context, params db.CreateSessionParams) (*models.Session, error)
	getFn                func(ctx context.Context, id string) (*models.Session, error)
	listActiveWithRepoFn func(ctx context.Context, repoID string) ([]*db.SessionWithRepo, error)
	listByRepoAndPRFn    func(ctx context.Context, repoID string, prNumber int) ([]*db.SessionWithRepo, error)
	deleteFn             func(ctx context.Context, id string) error
	deleted              []string
	deleteErr            error
}

func (m *mockSessionStore) Create(ctx context.Context, params db.CreateSessionParams) (*models.Session, error) {
	return m.createFn(ctx, params)
}

func (m *mockSessionStore) Get(ctx context.Context, id string) (*models.Session, error) {
	return m.getFn(ctx, id)
}

func (m *mockSessionStore) List(ctx context.Context, repoID string) ([]*models.Session, error) {
	return nil, nil
}

func (m *mockSessionStore) ListActive(ctx context.Context, repoID string) ([]*models.Session, error) {
	return nil, nil
}

func (m *mockSessionStore) ListActiveWithRepo(ctx context.Context, repoID string) ([]*db.SessionWithRepo, error) {
	if m.listActiveWithRepoFn != nil {
		return m.listActiveWithRepoFn(ctx, repoID)
	}
	return nil, nil
}

func (m *mockSessionStore) ListWithRepo(ctx context.Context, repoID string) ([]*db.SessionWithRepo, error) {
	return nil, nil
}

func (m *mockSessionStore) ListByRepoAndPR(ctx context.Context, repoID string, prNumber int) ([]*db.SessionWithRepo, error) {
	if m.listByRepoAndPRFn != nil {
		return m.listByRepoAndPRFn(ctx, repoID, prNumber)
	}
	return nil, nil
}

func (m *mockSessionStore) ListArchived(ctx context.Context, repoID string) ([]*models.Session, error) {
	return nil, nil
}

func (m *mockSessionStore) Update(ctx context.Context, id string, params db.UpdateSessionParams) (*models.Session, error) {
	return nil, nil
}

func (m *mockSessionStore) Archive(ctx context.Context, id string) error {
	return nil
}

func (m *mockSessionStore) Resurrect(ctx context.Context, id string) error {
	return nil
}

func (m *mockSessionStore) Delete(ctx context.Context, id string) error {
	if m.deleteFn != nil {
		return m.deleteFn(ctx, id)
	}
	m.deleted = append(m.deleted, id)
	return m.deleteErr
}

func (m *mockSessionStore) AdvanceOrphanedSessions(ctx context.Context) (int64, error) {
	return 0, nil
}

func (m *mockSessionStore) UpdateStateConditional(_ context.Context, _ string, _, _ int) (bool, error) {
	return false, nil
}

func (m *mockSessionStore) UpdateRepairDiagnostics(_ context.Context, _ db.UpdateRepairDiagnosticsParams) error {
	return nil
}

func (m *mockSessionStore) UpdateRepairBlocked(_ context.Context, _ string, _ time.Time, _ string) error {
	return nil
}

func (m *mockSessionStore) ListByState(_ context.Context, _ int) ([]*models.Session, error) {
	return nil, nil
}

// mockSessionStarter implements SessionStarter for testing.
type mockSessionStarter struct {
	startSessionFn func(ctx context.Context, sessionID string, opts session.StartSessionOpts) error
}

func (m *mockSessionStarter) StartSession(ctx context.Context, sessionID string, opts session.StartSessionOpts) error {
	return m.startSessionFn(ctx, sessionID, opts)
}

type mockDuplicateLiveness struct {
	alive map[string]bool
}

func (m *mockDuplicateLiveness) IsSessionAlive(_ context.Context, sessionID string) bool {
	return m.alive[sessionID]
}

// fakeAccountResolver implements DefaultAccountResolver for the Fix #2
// default-account policy tests.
type fakeAccountResolver struct {
	id  string
	err error
}

func (f fakeAccountResolver) DefaultAccountID(_ context.Context, _ string, _ time.Time) (string, error) {
	return f.id, f.err
}

// TestCreateSession_BindsDefaultAccount proves a task-created session binds the
// account the resolver selects, matching the interactive StreamCreateSession
// path (Fix #2).
func TestCreateSession_BindsDefaultAccount(t *testing.T) {
	var capturedParams db.CreateSessionParams
	store := &mockSessionStore{
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			capturedParams = params
			return &models.Session{ID: "sess-acct", RepoID: params.RepoID}, nil
		},
		getFn: func(_ context.Context, id string) (*models.Session, error) {
			return &models.Session{ID: id, RepoID: "repo-1"}, nil
		},
	}
	starter := &mockSessionStarter{startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error { return nil }}

	creator := NewSessionCreatorWithAccountResolver(store, starter, func() string { return "claude" }, nil, nil,
		fakeAccountResolver{id: "acct-42"}, zerolog.Nop())

	if _, err := creator.CreateSession(context.Background(), CreateSessionOpts{RepoID: "repo-1", Title: "t", BaseBranch: "main"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if capturedParams.AccountID == nil || *capturedParams.AccountID != "acct-42" {
		t.Errorf("AccountID = %v, want acct-42", capturedParams.AccountID)
	}
}

// TestCreateSession_ResolverErrorDoesNotFail proves a default-account resolver
// error never fails session creation — the session is created unbound.
func TestCreateSession_ResolverErrorDoesNotFail(t *testing.T) {
	var capturedParams db.CreateSessionParams
	store := &mockSessionStore{
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			capturedParams = params
			return &models.Session{ID: "sess-unbound", RepoID: params.RepoID}, nil
		},
		getFn: func(_ context.Context, id string) (*models.Session, error) {
			return &models.Session{ID: id, RepoID: "repo-1"}, nil
		},
	}
	starter := &mockSessionStarter{startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error { return nil }}

	creator := NewSessionCreatorWithAccountResolver(store, starter, func() string { return "claude" }, nil, nil,
		fakeAccountResolver{err: errors.New("registry down")}, zerolog.Nop())

	if _, err := creator.CreateSession(context.Background(), CreateSessionOpts{RepoID: "repo-1", Title: "t", BaseBranch: "main"}); err != nil {
		t.Fatalf("CreateSession must not fail on resolver error: %v", err)
	}
	if capturedParams.AccountID != nil {
		t.Errorf("AccountID = %v, want nil (unbound) on resolver error", capturedParams.AccountID)
	}
}

// TestCreateSession_NilResolverLeavesUnbound proves the degrade-safe default: a
// nil DefaultAccountResolver (e.g. the legacy constructors) leaves AccountID nil.
func TestCreateSession_NilResolverLeavesUnbound(t *testing.T) {
	var capturedParams db.CreateSessionParams
	store := &mockSessionStore{
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			capturedParams = params
			return &models.Session{ID: "sess-legacy", RepoID: params.RepoID}, nil
		},
		getFn: func(_ context.Context, id string) (*models.Session, error) {
			return &models.Session{ID: id, RepoID: "repo-1"}, nil
		},
	}
	starter := &mockSessionStarter{startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error { return nil }}

	creator := NewSessionCreator(store, starter, "claude", zerolog.Nop())
	if _, err := creator.CreateSession(context.Background(), CreateSessionOpts{RepoID: "repo-1", Title: "t", BaseBranch: "main"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if capturedParams.AccountID != nil {
		t.Errorf("AccountID = %v, want nil with no resolver", capturedParams.AccountID)
	}
}

func TestCreateSession_Success(t *testing.T) {
	var capturedParams db.CreateSessionParams
	var capturedSessionID, capturedBranch string

	store := &mockSessionStore{
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			capturedParams = params
			return &models.Session{ID: "sess-123", RepoID: params.RepoID, Title: params.Title}, nil
		},
		getFn: func(_ context.Context, id string) (*models.Session, error) {
			return &models.Session{
				ID:           id,
				RepoID:       "repo-1",
				Title:        "Bump lodash",
				WorktreePath: "/tmp/wt",
				BranchName:   "dependabot/npm/lodash-4.17.21",
			}, nil
		},
	}

	starter := &mockSessionStarter{
		startSessionFn: func(_ context.Context, sessionID string, opts session.StartSessionOpts) error {
			capturedSessionID = sessionID
			capturedBranch = opts.ExistingBranch
			return nil
		},
	}

	creator := NewSessionCreator(store, starter, "claude", zerolog.Nop())

	sess, err := creator.CreateSession(context.Background(), CreateSessionOpts{
		RepoID:     "repo-1",
		Title:      "Bump lodash",
		Plan:       "Fix the tests",
		BaseBranch: "main",
		HeadBranch: "dependabot/npm/lodash-4.17.21",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sess.ID != "sess-123" {
		t.Errorf("got session ID %q, want %q", sess.ID, "sess-123")
	}
	if capturedParams.RepoID != "repo-1" {
		t.Errorf("got RepoID %q, want %q", capturedParams.RepoID, "repo-1")
	}
	if capturedParams.Plan != "Fix the tests" {
		t.Errorf("got Plan %q, want %q", capturedParams.Plan, "Fix the tests")
	}
	if capturedParams.BranchName != "dependabot/npm/lodash-4.17.21" {
		t.Errorf("got BranchName %q, want %q", capturedParams.BranchName, "dependabot/npm/lodash-4.17.21")
	}
	if capturedSessionID != "sess-123" {
		t.Errorf("StartSession called with ID %q, want %q", capturedSessionID, "sess-123")
	}
	if capturedBranch != "dependabot/npm/lodash-4.17.21" {
		t.Errorf("StartSession called with branch %q, want %q", capturedBranch, "dependabot/npm/lodash-4.17.21")
	}
}

func TestCreateSession_StartFailurePublishesDelete(t *testing.T) {
	var notified []string
	store := &mockSessionStore{
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			return &models.Session{ID: "sess-x", RepoID: params.RepoID, Title: params.Title}, nil
		},
		getFn: func(_ context.Context, id string) (*models.Session, error) {
			return &models.Session{ID: id}, nil
		},
	}
	starter := &mockSessionStarter{
		startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error {
			return errors.New("worktree create failed")
		},
	}
	creator := NewSessionCreatorWithNotifier(store, starter, func() string { return "claude" }, nil,
		func(_ context.Context, id string) { notified = append(notified, id) }, zerolog.Nop())

	if _, err := creator.CreateSession(context.Background(), CreateSessionOpts{
		RepoID: "repo-1", Title: "t", BaseBranch: "main",
	}); err == nil {
		t.Fatal("expected error from StartSession failure")
	}

	// The half-started row is deleted AND the deletion is published so it
	// doesn't linger as a phantom in the web read model until reconnect.
	if len(store.deleted) != 1 || store.deleted[0] != "sess-x" {
		t.Fatalf("expected session sess-x deleted, got %v", store.deleted)
	}
	if len(notified) != 1 || notified[0] != "sess-x" {
		t.Fatalf("expected delete notification for sess-x, got %v", notified)
	}
}

func TestCreateSession_SuccessDoesNotPublishDelete(t *testing.T) {
	var notified []string
	store := &mockSessionStore{
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			return &models.Session{ID: "sess-ok", RepoID: params.RepoID, Title: params.Title}, nil
		},
		getFn: func(_ context.Context, id string) (*models.Session, error) {
			return &models.Session{ID: id, BranchName: "b"}, nil
		},
	}
	starter := &mockSessionStarter{
		startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error { return nil },
	}
	creator := NewSessionCreatorWithNotifier(store, starter, func() string { return "claude" }, nil,
		func(_ context.Context, id string) { notified = append(notified, id) }, zerolog.Nop())

	if _, err := creator.CreateSession(context.Background(), CreateSessionOpts{
		RepoID: "repo-1", Title: "t", BaseBranch: "main",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(notified) != 0 {
		t.Fatalf("no delete notification expected on success, got %v", notified)
	}
}

func TestSessionCreatorPreservesExplicitAgent(t *testing.T) {
	var capturedParams db.CreateSessionParams

	store := &mockSessionStore{
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			capturedParams = params
			return &models.Session{ID: "sess-123", RepoID: params.RepoID, AgentName: params.AgentName}, nil
		},
		getFn: func(_ context.Context, id string) (*models.Session, error) {
			return &models.Session{ID: id, RepoID: "repo-1", AgentName: capturedParams.AgentName}, nil
		},
	}
	starter := &mockSessionStarter{
		startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error {
			return nil
		},
	}

	creator := NewSessionCreator(store, starter, "claude", zerolog.Nop())
	sess, err := creator.CreateSession(context.Background(), CreateSessionOpts{
		RepoID:     "repo-1",
		Title:      "Cron job",
		Plan:       "Run scheduled task",
		BaseBranch: "main",
		AgentName:  "codex",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if capturedParams.AgentName != "codex" {
		t.Errorf("Create params AgentName = %q, want 'codex'", capturedParams.AgentName)
	}
	if sess.AgentName != "codex" {
		t.Errorf("session AgentName = %q, want 'codex'", sess.AgentName)
	}
}

func TestSessionCreatorUsesLatestDefaultAgentFromProvider(t *testing.T) {
	var capturedParams db.CreateSessionParams

	store := &mockSessionStore{
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			capturedParams = params
			return &models.Session{ID: "sess-123", RepoID: params.RepoID, AgentName: params.AgentName}, nil
		},
		getFn: func(_ context.Context, id string) (*models.Session, error) {
			return &models.Session{ID: id, RepoID: "repo-1", AgentName: capturedParams.AgentName}, nil
		},
	}
	starter := &mockSessionStarter{
		startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error {
			return nil
		},
	}

	defaultAgent := "claude"
	creator := NewSessionCreatorWithDefaultAgentProvider(store, starter, func() string {
		return defaultAgent
	}, zerolog.Nop())
	defaultAgent = "codex"

	sess, err := creator.CreateSession(context.Background(), CreateSessionOpts{
		RepoID:     "repo-1",
		Title:      "Cron job",
		Plan:       "Run scheduled task",
		BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if capturedParams.AgentName != "codex" {
		t.Errorf("Create params AgentName = %q, want latest default 'codex'", capturedParams.AgentName)
	}
	if sess.AgentName != "codex" {
		t.Errorf("session AgentName = %q, want latest default 'codex'", sess.AgentName)
	}
}

func TestSessionCreatorUsesConfiguredDefaultAgentWhenTaskAgentEmpty(t *testing.T) {
	var capturedParams db.CreateSessionParams

	store := &mockSessionStore{
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			capturedParams = params
			return &models.Session{ID: "sess-123", RepoID: params.RepoID, AgentName: params.AgentName}, nil
		},
		getFn: func(_ context.Context, id string) (*models.Session, error) {
			return &models.Session{ID: id, RepoID: "repo-1", AgentName: capturedParams.AgentName}, nil
		},
	}
	starter := &mockSessionStarter{
		startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error {
			return nil
		},
	}

	creator := NewSessionCreator(store, starter, "codex", zerolog.Nop())
	sess, err := creator.CreateSession(context.Background(), CreateSessionOpts{
		RepoID:     "repo-1",
		Title:      "Cron job",
		Plan:       "Run scheduled task",
		BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if capturedParams.AgentName != "codex" {
		t.Errorf("Create params AgentName = %q, want configured default 'codex'", capturedParams.AgentName)
	}
	if sess.AgentName != "codex" {
		t.Errorf("session AgentName = %q, want configured default 'codex'", sess.AgentName)
	}
}

func TestSessionCreatorFallsBackToClaudeWhenConfiguredDefaultEmpty(t *testing.T) {
	var capturedParams db.CreateSessionParams

	store := &mockSessionStore{
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			capturedParams = params
			return &models.Session{ID: "sess-123", RepoID: params.RepoID, AgentName: params.AgentName}, nil
		},
		getFn: func(_ context.Context, id string) (*models.Session, error) {
			return &models.Session{ID: id, RepoID: "repo-1", AgentName: capturedParams.AgentName}, nil
		},
	}
	starter := &mockSessionStarter{
		startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error {
			return nil
		},
	}

	creator := NewSessionCreator(store, starter, "", zerolog.Nop())
	sess, err := creator.CreateSession(context.Background(), CreateSessionOpts{
		RepoID:     "repo-1",
		Title:      "Cron job",
		Plan:       "Run scheduled task",
		BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if capturedParams.AgentName != "claude" {
		t.Errorf("Create params AgentName = %q, want fallback 'claude'", capturedParams.AgentName)
	}
	if sess.AgentName != "claude" {
		t.Errorf("session AgentName = %q, want fallback 'claude'", sess.AgentName)
	}
}

func TestCreateSession_NoHeadBranch(t *testing.T) {
	var capturedBranch string

	store := &mockSessionStore{
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			return &models.Session{ID: "sess-456", RepoID: params.RepoID}, nil
		},
		getFn: func(_ context.Context, id string) (*models.Session, error) {
			return &models.Session{ID: id, RepoID: "repo-1"}, nil
		},
	}

	starter := &mockSessionStarter{
		startSessionFn: func(_ context.Context, _ string, opts session.StartSessionOpts) error {
			capturedBranch = opts.ExistingBranch
			return nil
		},
	}

	creator := NewSessionCreator(store, starter, "claude", zerolog.Nop())

	_, err := creator.CreateSession(context.Background(), CreateSessionOpts{
		RepoID:     "repo-1",
		Title:      "New task",
		BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedBranch != "" {
		t.Errorf("expected empty branch for new session, got %q", capturedBranch)
	}
}

func TestCreateSession_DuplicateActivePRReturnsTypedErrorBeforeCreate(t *testing.T) {
	prNumber := 42
	store := &mockSessionStore{
		listByRepoAndPRFn: func(_ context.Context, repoID string, prNumber int) ([]*db.SessionWithRepo, error) {
			return []*db.SessionWithRepo{{Session: &models.Session{ID: "sess-existing", RepoID: repoID, PRNumber: &prNumber}}}, nil
		},
		createFn: func(_ context.Context, _ db.CreateSessionParams) (*models.Session, error) {
			t.Fatal("Create should not be called for duplicate PR")
			return nil, nil
		},
	}
	starter := &mockSessionStarter{
		startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error {
			t.Fatal("StartSession should not be called for duplicate PR")
			return nil
		},
	}

	creator := NewSessionCreator(store, starter, "", zerolog.Nop())
	_, err := creator.CreateSession(context.Background(), CreateSessionOpts{
		RepoID:                        "repo-1",
		Title:                         "Repair PR",
		BaseBranch:                    "main",
		PRNumber:                      &prNumber,
		PreventDuplicateActiveSession: true,
	})
	if err == nil {
		t.Fatal("expected duplicate error, got nil")
	}
	var duplicate *session.DuplicateActivePRSessionError
	if !errors.As(err, &duplicate) {
		t.Fatalf("error %T = %v, want DuplicateActivePRSessionError", err, err)
	}
	if duplicate.ExistingSessionID != "sess-existing" || duplicate.RepoID != "repo-1" || duplicate.PRNumber != 42 {
		t.Fatalf("duplicate = %+v, want existing sess-existing repo-1 PR 42", duplicate)
	}
}

func TestCreateSession_AllowsSamePRNumberInDifferentRepo(t *testing.T) {
	prNumber := 42
	created := false
	store := &mockSessionStore{
		listByRepoAndPRFn: func(_ context.Context, repoID string, _ int) ([]*db.SessionWithRepo, error) {
			if repoID == "repo-2" {
				return nil, nil
			}
			return []*db.SessionWithRepo{{Session: &models.Session{ID: "sess-existing", RepoID: "repo-1", PRNumber: &prNumber}}}, nil
		},
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			created = true
			return &models.Session{ID: "sess-new", RepoID: params.RepoID}, nil
		},
		getFn: func(_ context.Context, id string) (*models.Session, error) {
			return &models.Session{ID: id, RepoID: "repo-2"}, nil
		},
	}
	starter := &mockSessionStarter{startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error { return nil }}

	creator := NewSessionCreator(store, starter, "", zerolog.Nop())
	if _, err := creator.CreateSession(context.Background(), CreateSessionOpts{RepoID: "repo-2", Title: "Repair PR", BaseBranch: "main", PRNumber: &prNumber, PreventDuplicateActiveSession: true}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !created {
		t.Fatal("Create was not called")
	}
}

func TestCreateSession_AllowsArchivedPriorPRSession(t *testing.T) {
	prNumber := 42
	archivedAt := time.Now()
	created := false
	store := &mockSessionStore{
		listByRepoAndPRFn: func(_ context.Context, repoID string, prNumber int) ([]*db.SessionWithRepo, error) {
			return []*db.SessionWithRepo{{Session: &models.Session{ID: "sess-archived", RepoID: repoID, PRNumber: &prNumber, ArchivedAt: &archivedAt}}}, nil
		},
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			created = true
			return &models.Session{ID: "sess-new", RepoID: params.RepoID}, nil
		},
		getFn: func(_ context.Context, id string) (*models.Session, error) {
			return &models.Session{ID: id, RepoID: "repo-1"}, nil
		},
	}
	starter := &mockSessionStarter{startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error { return nil }}

	creator := NewSessionCreator(store, starter, "", zerolog.Nop())
	if _, err := creator.CreateSession(context.Background(), CreateSessionOpts{RepoID: "repo-1", Title: "Repair PR", BaseBranch: "main", PRNumber: &prNumber, PreventDuplicateActiveSession: true}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !created {
		t.Fatal("Create was not called")
	}
}

func TestCreateSession_AllowsBlockedPriorPRSession(t *testing.T) {
	prNumber := 42
	created := false
	store := &mockSessionStore{
		listByRepoAndPRFn: func(_ context.Context, repoID string, prNumber int) ([]*db.SessionWithRepo, error) {
			return []*db.SessionWithRepo{{Session: &models.Session{ID: "sess-blocked", RepoID: repoID, PRNumber: &prNumber, State: machine.Blocked}}}, nil
		},
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			created = true
			return &models.Session{ID: "sess-new", RepoID: params.RepoID}, nil
		},
		getFn: func(_ context.Context, id string) (*models.Session, error) {
			return &models.Session{ID: id, RepoID: "repo-1"}, nil
		},
	}
	starter := &mockSessionStarter{startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error { return nil }}

	creator := NewSessionCreator(store, starter, "", zerolog.Nop())
	if _, err := creator.CreateSession(context.Background(), CreateSessionOpts{RepoID: "repo-1", Title: "Repair PR", BaseBranch: "main", PRNumber: &prNumber, PreventDuplicateActiveSession: true}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !created {
		t.Fatal("Create was not called")
	}
}

func TestCreateSession_NilPRNumberDoesNotCheckDuplicates(t *testing.T) {
	store := &mockSessionStore{
		listByRepoAndPRFn: func(_ context.Context, _ string, _ int) ([]*db.SessionWithRepo, error) {
			t.Fatal("ListByRepoAndPR should not be called without PRNumber")
			return nil, nil
		},
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			return &models.Session{ID: "sess-new", RepoID: params.RepoID}, nil
		},
		getFn: func(_ context.Context, id string) (*models.Session, error) {
			return &models.Session{ID: id, RepoID: "repo-1"}, nil
		},
	}
	starter := &mockSessionStarter{startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error { return nil }}

	creator := NewSessionCreator(store, starter, "", zerolog.Nop())
	if _, err := creator.CreateSession(context.Background(), CreateSessionOpts{RepoID: "repo-1", Title: "Quick task", BaseBranch: "main"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
}

func TestCreateSession_DoesNotCheckDuplicatePRWithoutOptIn(t *testing.T) {
	prNumber := 42
	created := false
	store := &mockSessionStore{
		listByRepoAndPRFn: func(_ context.Context, _ string, _ int) ([]*db.SessionWithRepo, error) {
			t.Fatal("ListByRepoAndPR should not be called without duplicate prevention opt-in")
			return nil, nil
		},
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			created = true
			return &models.Session{ID: "sess-new", RepoID: params.RepoID}, nil
		},
		getFn: func(_ context.Context, id string) (*models.Session, error) {
			return &models.Session{ID: id, RepoID: "repo-1"}, nil
		},
	}
	starter := &mockSessionStarter{startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error { return nil }}

	creator := NewSessionCreator(store, starter, "", zerolog.Nop())
	if _, err := creator.CreateSession(context.Background(), CreateSessionOpts{RepoID: "repo-1", Title: "Normal PR", BaseBranch: "main", PRNumber: &prNumber}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !created {
		t.Fatal("Create was not called")
	}
}

func TestCreateSession_DuplicateActiveBranchReturnsTypedErrorBeforeCreate(t *testing.T) {
	store := &mockSessionStore{
		listActiveWithRepoFn: func(_ context.Context, repoID string) ([]*db.SessionWithRepo, error) {
			return []*db.SessionWithRepo{{Session: &models.Session{ID: "sess-existing", RepoID: repoID, BranchName: "dependabot/npm/lodash-4.17.21"}}}, nil
		},
		createFn: func(_ context.Context, _ db.CreateSessionParams) (*models.Session, error) {
			t.Fatal("Create should not be called for duplicate branch")
			return nil, nil
		},
	}
	starter := &mockSessionStarter{
		startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error {
			t.Fatal("StartSession should not be called for duplicate branch")
			return nil
		},
	}

	creator := NewSessionCreator(store, starter, "", zerolog.Nop())
	_, err := creator.CreateSession(context.Background(), CreateSessionOpts{
		RepoID:                        "repo-1",
		Title:                         "Repair branch",
		BaseBranch:                    "main",
		HeadBranch:                    "dependabot/npm/lodash-4.17.21",
		PreventDuplicateActiveSession: true,
	})
	if err == nil {
		t.Fatal("expected duplicate error, got nil")
	}
	var duplicate *session.DuplicateActiveBranchSessionError
	if !errors.As(err, &duplicate) {
		t.Fatalf("error %T = %v, want DuplicateActiveBranchSessionError", err, err)
	}
	if duplicate.ExistingSessionID != "sess-existing" || duplicate.RepoID != "repo-1" || duplicate.BranchName != "dependabot/npm/lodash-4.17.21" {
		t.Fatalf("duplicate = %+v, want existing sess-existing repo-1 branch", duplicate)
	}
}

func TestCreateSession_AllowsBlockedPriorBranchSession(t *testing.T) {
	created := false
	store := &mockSessionStore{
		listActiveWithRepoFn: func(_ context.Context, repoID string) ([]*db.SessionWithRepo, error) {
			return []*db.SessionWithRepo{{Session: &models.Session{ID: "sess-blocked", RepoID: repoID, BranchName: "dependabot/npm/lodash-4.17.21", State: machine.Blocked}}}, nil
		},
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			created = true
			return &models.Session{ID: "sess-new", RepoID: params.RepoID}, nil
		},
		getFn: func(_ context.Context, id string) (*models.Session, error) {
			return &models.Session{ID: id, RepoID: "repo-1"}, nil
		},
	}
	starter := &mockSessionStarter{startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error { return nil }}

	creator := NewSessionCreator(store, starter, "", zerolog.Nop())
	if _, err := creator.CreateSession(context.Background(), CreateSessionOpts{
		RepoID:                        "repo-1",
		Title:                         "Repair branch",
		BaseBranch:                    "main",
		HeadBranch:                    "dependabot/npm/lodash-4.17.21",
		PreventDuplicateActiveSession: true,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !created {
		t.Fatal("Create was not called")
	}
}

func TestCreateSession_ConcurrentSameRepoPRCreatesOneSession(t *testing.T) {
	prNumber := 42
	type concurrentStore struct {
		*mockSessionStore
		mu       sync.Mutex
		next     int
		sessions map[string]*models.Session
	}
	store := &concurrentStore{sessions: map[string]*models.Session{}}
	store.mockSessionStore = &mockSessionStore{}
	store.listByRepoAndPRFn = func(_ context.Context, repoID string, prNumber int) ([]*db.SessionWithRepo, error) {
		store.mu.Lock()
		defer store.mu.Unlock()
		var rows []*db.SessionWithRepo
		for _, sess := range store.sessions {
			if sess.RepoID == repoID && sess.PRNumber != nil && *sess.PRNumber == prNumber && sess.ArchivedAt == nil {
				rows = append(rows, &db.SessionWithRepo{Session: sess})
			}
		}
		return rows, nil
	}
	store.createFn = func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
		store.mu.Lock()
		defer store.mu.Unlock()
		store.next++
		id := fmt.Sprintf("sess-%d", store.next)
		sess := &models.Session{ID: id, RepoID: params.RepoID, PRNumber: params.PRNumber}
		store.sessions[id] = sess
		return sess, nil
	}
	store.getFn = func(_ context.Context, id string) (*models.Session, error) {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.sessions[id], nil
	}
	starter := &mockSessionStarter{startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error { return nil }}
	creator := NewSessionCreator(store, starter, "", zerolog.Nop())

	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := creator.CreateSession(context.Background(), CreateSessionOpts{
				RepoID:                        "repo-1",
				Title:                         "Repair PR",
				BaseBranch:                    "main",
				PRNumber:                      &prNumber,
				PreventDuplicateActiveSession: true,
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	var success, duplicate int
	for err := range errs {
		if err == nil {
			success++
			continue
		}
		var dup *session.DuplicateActivePRSessionError
		if errors.As(err, &dup) {
			duplicate++
			continue
		}
		t.Fatalf("unexpected error: %v", err)
	}
	if success != 1 || duplicate != 1 {
		t.Fatalf("success=%d duplicate=%d, want 1 each", success, duplicate)
	}
	if store.next != 1 {
		t.Fatalf("created sessions = %d, want 1", store.next)
	}
}

func TestCreateSession_CreateError(t *testing.T) {
	store := &mockSessionStore{
		createFn: func(_ context.Context, _ db.CreateSessionParams) (*models.Session, error) {
			return nil, errors.New("db write failed")
		},
	}

	starter := &mockSessionStarter{
		startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error {
			t.Fatal("StartSession should not be called when Create fails")
			return nil
		},
	}

	creator := NewSessionCreator(store, starter, "claude", zerolog.Nop())

	_, err := creator.CreateSession(context.Background(), CreateSessionOpts{
		RepoID:     "repo-1",
		Title:      "Test",
		BaseBranch: "main",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if want := "create session: db write failed"; err.Error() != want {
		t.Errorf("got error %q, want %q", err.Error(), want)
	}
}

func TestCreateSession_StartSessionError(t *testing.T) {
	store := &mockSessionStore{
		createFn: func(_ context.Context, _ db.CreateSessionParams) (*models.Session, error) {
			return &models.Session{ID: "sess-789"}, nil
		},
		getFn: func(_ context.Context, _ string) (*models.Session, error) {
			t.Fatal("Get should not be called when StartSession fails")
			return nil, nil
		},
	}

	starter := &mockSessionStarter{
		startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error {
			return errors.New("worktree conflict")
		},
	}

	creator := NewSessionCreator(store, starter, "claude", zerolog.Nop())

	_, err := creator.CreateSession(context.Background(), CreateSessionOpts{
		RepoID:     "repo-1",
		Title:      "Test",
		BaseBranch: "main",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if want := "start session sess-789: worktree conflict"; err.Error() != want {
		t.Errorf("got error %q, want %q", err.Error(), want)
	}

	// The half-started session row must be cleaned up: leaving it behind
	// produces phantom entries in the home view (a session with no chat,
	// no PR, stuck in CreatingWorktree) that the user can't recover.
	if len(store.deleted) != 1 || store.deleted[0] != "sess-789" {
		t.Errorf("expected Delete(\"sess-789\") to be called once, got %v", store.deleted)
	}
}

func TestCreateSession_ConcurrentDuplicateWaitsForFailedStartCleanup(t *testing.T) {
	prNumber := 42
	firstStartEntered := make(chan struct{})

	var (
		mu          sync.Mutex
		next        int
		sessions    = map[string]*models.Session{}
		deleted     []string
		startErrors []error
	)
	store := &mockSessionStore{
		listByRepoAndPRFn: func(_ context.Context, repoID string, prNumber int) ([]*db.SessionWithRepo, error) {
			mu.Lock()
			defer mu.Unlock()
			var rows []*db.SessionWithRepo
			for _, sess := range sessions {
				if sess.RepoID == repoID && sess.PRNumber != nil && *sess.PRNumber == prNumber && sess.ArchivedAt == nil {
					rows = append(rows, &db.SessionWithRepo{Session: sess})
				}
			}
			return rows, nil
		},
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			mu.Lock()
			defer mu.Unlock()
			next++
			id := fmt.Sprintf("sess-%d", next)
			sess := &models.Session{ID: id, RepoID: params.RepoID, PRNumber: params.PRNumber}
			sessions[id] = sess
			return sess, nil
		},
		deleteFn: func(_ context.Context, id string) error {
			mu.Lock()
			defer mu.Unlock()
			delete(sessions, id)
			deleted = append(deleted, id)
			return nil
		},
		getFn: func(_ context.Context, id string) (*models.Session, error) {
			mu.Lock()
			defer mu.Unlock()
			return sessions[id], nil
		},
	}
	starter := &mockSessionStarter{
		startSessionFn: func(_ context.Context, sessionID string, _ session.StartSessionOpts) error {
			if sessionID == "sess-1" {
				close(firstStartEntered)
				time.Sleep(100 * time.Millisecond)
				return errors.New("worktree conflict")
			}
			return nil
		},
	}
	creator := NewSessionCreator(store, starter, "", zerolog.Nop())

	firstErr := make(chan error, 1)
	go func() {
		_, err := creator.CreateSession(context.Background(), CreateSessionOpts{
			RepoID:                        "repo-1",
			Title:                         "Repair PR",
			BaseBranch:                    "main",
			PRNumber:                      &prNumber,
			PreventDuplicateActiveSession: true,
		})
		firstErr <- err
	}()

	select {
	case <-firstStartEntered:
	case <-time.After(time.Second):
		t.Fatal("first session did not enter StartSession")
	}

	_, secondErr := creator.CreateSession(context.Background(), CreateSessionOpts{
		RepoID:                        "repo-1",
		Title:                         "Repair PR",
		BaseBranch:                    "main",
		PRNumber:                      &prNumber,
		PreventDuplicateActiveSession: true,
	})
	startErrors = append(startErrors, <-firstErr, secondErr)

	var duplicates int
	for _, err := range startErrors {
		if session.IsDuplicateActiveSessionError(err) {
			duplicates++
		}
	}
	if duplicates != 0 {
		t.Fatalf("expected overlapping duplicate to wait for cleanup, got %d duplicate errors: %v", duplicates, startErrors)
	}
	if startErrors[0] == nil || startErrors[1] != nil {
		t.Fatalf("expected first start to fail and second create to succeed, got %v", startErrors)
	}
	if next != 2 {
		t.Fatalf("expected second create after first cleanup, created %d sessions", next)
	}
	if len(deleted) != 1 || deleted[0] != "sess-1" {
		t.Fatalf("expected first half-started session to be deleted, got %v", deleted)
	}
}

func TestCreateSession_DuplicatePreventionIgnoresDeadEarlySession(t *testing.T) {
	prNumber := 42
	agentSessionID := "agent-dead"
	existing := &models.Session{
		ID:             "sess-dead",
		RepoID:         "repo-1",
		BranchName:     "dependabot/npm/lodash-4.17.21",
		State:          machine.ImplementingPlan,
		AgentSessionID: &agentSessionID,
		PRNumber:       &prNumber,
	}
	created := false
	store := &mockSessionStore{
		listByRepoAndPRFn: func(_ context.Context, _ string, _ int) ([]*db.SessionWithRepo, error) {
			return []*db.SessionWithRepo{{Session: existing}}, nil
		},
		listActiveWithRepoFn: func(_ context.Context, _ string) ([]*db.SessionWithRepo, error) {
			return []*db.SessionWithRepo{{Session: existing}}, nil
		},
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			created = true
			return &models.Session{ID: "sess-new", RepoID: params.RepoID}, nil
		},
		getFn: func(_ context.Context, id string) (*models.Session, error) {
			return &models.Session{ID: id, RepoID: "repo-1"}, nil
		},
	}
	starter := &mockSessionStarter{startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error {
		return nil
	}}
	creator := NewSessionCreatorWithDefaultAgentProviderAndLiveness(
		store, starter, func() string { return "claude" },
		&mockDuplicateLiveness{alive: map[string]bool{"sess-dead": false}},
		zerolog.Nop(),
	)

	_, err := creator.CreateSession(context.Background(), CreateSessionOpts{
		RepoID:                        "repo-1",
		Title:                         "Repair PR",
		BaseBranch:                    "main",
		HeadBranch:                    "dependabot/npm/lodash-4.17.21",
		PRNumber:                      &prNumber,
		PreventDuplicateActiveSession: true,
	})

	if err != nil {
		t.Fatalf("CreateSession returned error for dead duplicate: %v", err)
	}
	if !created {
		t.Fatal("expected new session to be created after ignoring dead duplicate")
	}
}

func TestCreateSession_DuplicatePreventionIgnoresIdentifierlessStartupSession(t *testing.T) {
	prNumber := 42
	existing := &models.Session{
		ID:         "sess-starting",
		RepoID:     "repo-1",
		BranchName: "dependabot/npm/lodash-4.17.21",
		State:      machine.StartingAgent,
		PRNumber:   &prNumber,
	}
	created := false
	store := &mockSessionStore{
		listByRepoAndPRFn: func(_ context.Context, _ string, _ int) ([]*db.SessionWithRepo, error) {
			return []*db.SessionWithRepo{{Session: existing}}, nil
		},
		listActiveWithRepoFn: func(_ context.Context, _ string) ([]*db.SessionWithRepo, error) {
			return []*db.SessionWithRepo{{Session: existing}}, nil
		},
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			created = true
			return &models.Session{ID: "sess-new", RepoID: params.RepoID}, nil
		},
		getFn: func(_ context.Context, id string) (*models.Session, error) {
			return &models.Session{ID: id, RepoID: "repo-1"}, nil
		},
	}
	starter := &mockSessionStarter{startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error {
		return nil
	}}
	creator := NewSessionCreatorWithDefaultAgentProviderAndLiveness(
		store, starter, func() string { return "claude" },
		&mockDuplicateLiveness{alive: map[string]bool{"sess-starting": true}},
		zerolog.Nop(),
	)

	_, err := creator.CreateSession(context.Background(), CreateSessionOpts{
		RepoID:                        "repo-1",
		Title:                         "Repair PR",
		BaseBranch:                    "main",
		HeadBranch:                    "dependabot/npm/lodash-4.17.21",
		PRNumber:                      &prNumber,
		PreventDuplicateActiveSession: true,
	})

	if err != nil {
		t.Fatalf("CreateSession returned error for identifierless startup duplicate: %v", err)
	}
	if !created {
		t.Fatal("expected new session to be created after ignoring identifierless startup duplicate")
	}
}

func TestCreateSession_DuplicatePreventionKeepsLiveEarlySessionActive(t *testing.T) {
	prNumber := 42
	agentSessionID := "agent-live"
	existing := &models.Session{
		ID:             "sess-live",
		RepoID:         "repo-1",
		State:          machine.ImplementingPlan,
		AgentSessionID: &agentSessionID,
		PRNumber:       &prNumber,
	}
	store := &mockSessionStore{
		listByRepoAndPRFn: func(_ context.Context, _ string, _ int) ([]*db.SessionWithRepo, error) {
			return []*db.SessionWithRepo{{Session: existing}}, nil
		},
		createFn: func(_ context.Context, _ db.CreateSessionParams) (*models.Session, error) {
			t.Fatal("Create should not be called when duplicate session is live")
			return nil, nil
		},
	}
	starter := &mockSessionStarter{startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error {
		t.Fatal("StartSession should not be called when duplicate session is live")
		return nil
	}}
	creator := NewSessionCreatorWithDefaultAgentProviderAndLiveness(
		store, starter, func() string { return "claude" },
		&mockDuplicateLiveness{alive: map[string]bool{"sess-live": true}},
		zerolog.Nop(),
	)

	_, err := creator.CreateSession(context.Background(), CreateSessionOpts{
		RepoID:                        "repo-1",
		Title:                         "Repair PR",
		BaseBranch:                    "main",
		PRNumber:                      &prNumber,
		PreventDuplicateActiveSession: true,
	})

	if !session.IsDuplicateActiveSessionError(err) {
		t.Fatalf("expected live duplicate error, got %v", err)
	}
}
