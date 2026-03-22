package taskorchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// mockSessionStore implements db.SessionStore for testing.
type mockSessionStore struct {
	createFn func(ctx context.Context, params db.CreateSessionParams) (*models.Session, error)
	getFn    func(ctx context.Context, id string) (*models.Session, error)
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
	return nil
}

// mockSessionStarter implements SessionStarter for testing.
type mockSessionStarter struct {
	startSessionFn func(ctx context.Context, sessionID string, existingBranch string, forceBranch bool) error
}

func (m *mockSessionStarter) StartSession(ctx context.Context, sessionID string, existingBranch string, forceBranch bool) error {
	return m.startSessionFn(ctx, sessionID, existingBranch, forceBranch)
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
		startSessionFn: func(_ context.Context, sessionID string, existingBranch string, _ bool) error {
			capturedSessionID = sessionID
			capturedBranch = existingBranch
			return nil
		},
	}

	creator := NewSessionCreator(store, starter, zerolog.Nop())

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
	if capturedSessionID != "sess-123" {
		t.Errorf("StartSession called with ID %q, want %q", capturedSessionID, "sess-123")
	}
	if capturedBranch != "dependabot/npm/lodash-4.17.21" {
		t.Errorf("StartSession called with branch %q, want %q", capturedBranch, "dependabot/npm/lodash-4.17.21")
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
		startSessionFn: func(_ context.Context, _ string, existingBranch string, _ bool) error {
			capturedBranch = existingBranch
			return nil
		},
	}

	creator := NewSessionCreator(store, starter, zerolog.Nop())

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

func TestCreateSession_CreateError(t *testing.T) {
	store := &mockSessionStore{
		createFn: func(_ context.Context, _ db.CreateSessionParams) (*models.Session, error) {
			return nil, errors.New("db write failed")
		},
	}

	starter := &mockSessionStarter{
		startSessionFn: func(_ context.Context, _ string, _ string, _ bool) error {
			t.Fatal("StartSession should not be called when Create fails")
			return nil
		},
	}

	creator := NewSessionCreator(store, starter, zerolog.Nop())

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
		startSessionFn: func(_ context.Context, _ string, _ string, _ bool) error {
			return errors.New("worktree conflict")
		},
	}

	creator := NewSessionCreator(store, starter, zerolog.Nop())

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
}
