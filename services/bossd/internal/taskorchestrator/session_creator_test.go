package taskorchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

func (m *mockSessionStore) ListTmuxSessionNames(ctx context.Context) ([]string, error) {
	return nil, nil
}

func (m *mockSessionStore) Update(ctx context.Context, id string, params db.UpdateSessionParams) (*models.Session, error) {
	return nil, nil
}

func (m *mockSessionStore) Archive(ctx context.Context, id string) error {
	return nil
}

func (m *mockSessionStore) ResurrectToState(_ context.Context, _ string, _ int) (bool, error) {
	return false, nil
}

func (m *mockSessionStore) RollbackFailedResurrect(_ context.Context, _ string, _ time.Time, _, _ int) (bool, error) {
	return false, nil
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
func (m *mockSessionStore) ListByStates(_ context.Context, _ []int) ([]*models.Session, error) {
	return nil, nil
}
func (m *mockSessionStore) UpdateStateConditionalFrom(_ context.Context, _ string, _ int, _ []int) (bool, error) {
	return false, nil
}

// mockSessionStarter implements SessionStarter for testing.
type mockSessionStarter struct {
	mu             sync.Mutex
	startSessionFn func(ctx context.Context, sessionID string, opts session.StartSessionOpts) error
	// cleanedUp records the sessions whose bootstrap artifacts were reclaimed,
	// and the ORDER matters: the row is the only thing naming the worktree and
	// branch, so cleanup has to happen before the row is deleted.
	cleanedUp []string
}

func (m *mockSessionStarter) StartSession(ctx context.Context, sessionID string, opts session.StartSessionOpts) error {
	return m.startSessionFn(ctx, sessionID, opts)
}

func (m *mockSessionStarter) CleanUpFailedBootstrapArtifacts(_ context.Context, sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanedUp = append(m.cleanedUp, sessionID)
}

func (m *mockSessionStarter) cleanupRecords() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.cleanedUp...)
}

type mockDuplicateLiveness struct {
	alive  map[string]bool
	parked map[string]bool
}

func (m *mockDuplicateLiveness) SessionLiveness(_ context.Context, sessionID string) session.Liveness {
	switch {
	case m.alive[sessionID]:
		return session.LivenessAlive
	case m.parked[sessionID]:
		return session.LivenessParked
	default:
		return session.LivenessDead
	}
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

// TestCreateSession_BindsDefaultAccountRegardlessOfRotationFlag proves
// creation-time binding is decoupled from the rotation kill-switch (BOS-305):
// the task-orchestrator path binds the resolved account even though the caller
// no longer supplies (and the creator no longer consults) any rotation config.
func TestCreateSession_BindsDefaultAccountRegardlessOfRotationFlag(t *testing.T) {
	var capturedParams db.CreateSessionParams
	store := &mockSessionStore{
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			capturedParams = params
			return &models.Session{ID: "sess-bound", RepoID: params.RepoID}, nil
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
		t.Errorf("AccountID = %v, want acct-42 bound regardless of rotation state", capturedParams.AccountID)
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

// TestCreateSession_StartFailureReclaimsBootstrapArtifacts is AC 2 for the
// task-orchestrator create path (cron, dependabot, /boss-epic).
//
// This path only ever deleted the row. That was survivable while a wedged
// bootstrap hung forever and never reached the failure branch at all; the
// BOS-717 bootstrap deadline makes it routine, and by then `git worktree add`
// has usually run — the setup script, the slow step, comes after it. The row is
// the only thing that names the worktree and branch, so the cleanup has to
// happen BEFORE the delete or both are orphaned permanently.
func TestCreateSession_StartFailureReclaimsBootstrapArtifacts(t *testing.T) {
	starter := &mockSessionStarter{
		startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error {
			return context.DeadlineExceeded
		},
	}
	deletes := 0
	store := &mockSessionStore{
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			return &models.Session{ID: "sess-x", RepoID: params.RepoID, Title: params.Title}, nil
		},
		getFn: func(_ context.Context, id string) (*models.Session, error) {
			return &models.Session{ID: id}, nil
		},
		deleteFn: func(_ context.Context, _ string) error {
			// Ordering is the whole point: once the row is gone, nothing names
			// the worktree or branch to clean up.
			if got := starter.cleanupRecords(); len(got) == 0 {
				t.Error("session row deleted before its bootstrap artifacts were reclaimed")
			}
			deletes++
			return nil
		},
	}
	creator := NewSessionCreator(store, starter, "claude", zerolog.Nop())

	if _, err := creator.CreateSession(context.Background(), CreateSessionOpts{
		RepoID: "repo-1", Title: "t", BaseBranch: "main",
	}); err == nil {
		t.Fatal("expected error from the bootstrap deadline")
	}

	cleaned := starter.cleanupRecords()
	if len(cleaned) != 1 || cleaned[0] != "sess-x" {
		t.Fatalf("bootstrap artifacts reclaimed for %v, want [sess-x] — the worktree and branch are orphaned", cleaned)
	}
	if deletes != 1 {
		t.Fatalf("row deleted %d times, want 1", deletes)
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
	var postRow *SessionPostRowError
	if errors.As(err, &postRow) {
		t.Fatal("StartSession failure deleted its row; must not expose a post-row session id")
	}

	// The half-started session row must be cleaned up: leaving it behind
	// produces phantom entries in the home view (a session with no chat,
	// no PR, stuck in CreatingWorktree) that the user can't recover.
	if len(store.deleted) != 1 || store.deleted[0] != "sess-789" {
		t.Errorf("expected Delete(\"sess-789\") to be called once, got %v", store.deleted)
	}
}

func TestCreateSession_RefetchErrorCarriesSurvivingSessionID(t *testing.T) {
	store := &mockSessionStore{
		createFn: func(_ context.Context, _ db.CreateSessionParams) (*models.Session, error) {
			return &models.Session{ID: "sess-789"}, nil
		},
		getFn: func(_ context.Context, _ string) (*models.Session, error) {
			return nil, errors.New("db read failed")
		},
	}
	starter := &mockSessionStarter{
		startSessionFn: func(_ context.Context, _ string, _ session.StartSessionOpts) error {
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
	var postRow *SessionPostRowError
	if !errors.As(err, &postRow) {
		t.Fatalf("error type = %T, want SessionPostRowError", err)
	}
	if postRow.SessionID != "sess-789" {
		t.Fatalf("SessionID = %q, want sess-789", postRow.SessionID)
	}
	if len(store.deleted) != 0 {
		t.Fatalf("re-fetch failure must leave row for recovery, deleted %v", store.deleted)
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

// logSink is a concurrency-safe io.Writer for capturing zerolog output while a
// create runs on another goroutine.
type logSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestCreateSessionLogsLockWaitsOnTheOrchestratorPath pins AC 3 on the
// unattended path. Cron, dependabot and /boss-epic creates contend for the SAME
// two gates as the interactive RPC, but nobody is watching a stream here — so a
// create wedged waiting for a lock has to be visible in the daemon log, and it
// has to be visible BEFORE the wait ends rather than only once it does.
func TestCreateSessionLogsLockWaitsOnTheOrchestratorPath(t *testing.T) {
	// Not parallel: it lowers the package-level warn threshold.
	originalThreshold := session.SlowStartLockWaitThreshold
	session.SlowStartLockWaitThreshold = 10 * time.Millisecond
	t.Cleanup(func() { session.SlowStartLockWaitThreshold = originalThreshold })

	const (
		repoID = "repo-lockwait"
		branch = "bos-717-orchestrator-contended"
	)

	store := &mockSessionStore{
		createFn: func(_ context.Context, params db.CreateSessionParams) (*models.Session, error) {
			return &models.Session{ID: "sess-lockwait", RepoID: params.RepoID, Title: params.Title}, nil
		},
		getFn: func(_ context.Context, id string) (*models.Session, error) {
			return &models.Session{ID: id, RepoID: repoID, BranchName: branch}, nil
		},
	}
	starter := &mockSessionStarter{
		startSessionFn: func(context.Context, string, session.StartSessionOpts) error { return nil },
	}

	sink := &logSink{}
	creator := NewSessionCreator(store, starter, "claude", zerolog.New(sink))

	// Hold the target lock so the create below is genuinely blocked.
	release, _, err := session.AcquireTargetStart(context.Background(), repoID, branch, nil)
	if err != nil {
		t.Fatalf("pre-acquire target lock: %v", err)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()

	done := make(chan error, 1)
	go func() {
		_, createErr := creator.CreateSession(context.Background(), CreateSessionOpts{
			RepoID:     repoID,
			Title:      "contended cron fire",
			BaseBranch: "main",
			BranchName: branch,
		})
		done <- createErr
	}()

	// The before-acquire line must appear while the create is STILL blocked —
	// a line emitted only after the lock is taken tells a responder nothing
	// about a wait that never ends.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(sink.String(), "acquiring session start locks") {
		select {
		case createErr := <-done:
			t.Fatalf("create returned (%v) before the pre-acquire line was logged; log:\n%s", createErr, sink.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pre-acquire line logged while the create was blocked; log:\n%s", sink.String())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Still blocked, so the line really did precede acquisition.
	select {
	case createErr := <-done:
		t.Fatalf("create completed while the target lock was held: %v", createErr)
	case <-time.After(50 * time.Millisecond):
	}

	release()
	released = true
	if createErr := <-done; createErr != nil {
		t.Fatalf("create after lock release = %v, want nil", createErr)
	}

	if !strings.Contains(sink.String(), "session start locks were contended") {
		t.Fatalf("no contention warning after a blocked acquire; log:\n%s", sink.String())
	}
	if !strings.Contains(sink.String(), "target_lock_wait") {
		t.Fatalf("contention warning did not record the wait; log:\n%s", sink.String())
	}
}

// TestCreateSessionLogsAFailedLockAcquireOnTheOrchestratorPath pins the third
// rung: an acquire that gives up must say so at Error, naming the lock. The
// caller's error is returned to a scheduler that logs it as a fire failure, so
// without this the daemon log never names the lock that refused.
func TestCreateSessionLogsAFailedLockAcquireOnTheOrchestratorPath(t *testing.T) {
	t.Parallel()

	sink := &logSink{}
	creator := NewSessionCreator(&mockSessionStore{}, &mockSessionStarter{}, "claude", zerolog.New(sink))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := creator.CreateSession(ctx, CreateSessionOpts{
		RepoID:     "repo-cancelled",
		Title:      "cancelled cron fire",
		BranchName: "bos-717-cancelled",
	}); err == nil {
		t.Fatal("CreateSession on a cancelled context returned nil error")
	}
	if !strings.Contains(sink.String(), "giving up waiting for the target lock") {
		t.Fatalf("a refused acquire logged nothing; log:\n%s", sink.String())
	}
}
