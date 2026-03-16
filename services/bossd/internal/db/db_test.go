package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/migrate"
	"github.com/recurser/bossalib/models"
)

// migrationsDir returns the absolute path to the bossd migrations directory.
func migrationsDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
}

// setupTestDB creates an in-memory SQLite database with migrations applied.
func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	if err := migrate.Run(db, os.DirFS(migrationsDir())); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// createTestRepo creates a repo for testing and returns it.
func createTestRepo(t *testing.T, store *SQLiteRepoStore) *models.Repo {
	t.Helper()
	repo, err := store.Create(context.Background(), CreateRepoParams{
		DisplayName:       "test-repo",
		LocalPath:         "/tmp/test-repo",
		OriginURL:         "https://github.com/test/repo.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	})
	if err != nil {
		t.Fatalf("create test repo: %v", err)
	}
	return repo
}

func TestMigrationRunner(t *testing.T) {
	db := setupTestDB(t)

	// Verify tables exist by querying them.
	for _, table := range []string{"repos", "sessions", "attempts"} {
		var count int
		err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count)
		if err != nil {
			t.Errorf("table %s should exist: %v", table, err)
		}
	}
}

func TestRepoStore_CRUD(t *testing.T) {
	db := setupTestDB(t)
	store := NewRepoStore(db)
	ctx := context.Background()

	// Create
	repo, err := store.Create(ctx, CreateRepoParams{
		DisplayName:       "my-app",
		LocalPath:         "/home/user/my-app",
		OriginURL:         "https://github.com/user/my-app.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/home/user/.worktrees/my-app",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if repo.DisplayName != "my-app" {
		t.Errorf("display_name = %q, want %q", repo.DisplayName, "my-app")
	}
	if repo.ID == "" {
		t.Error("id should not be empty")
	}

	// Get
	got, err := store.Get(ctx, repo.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LocalPath != "/home/user/my-app" {
		t.Errorf("local_path = %q, want %q", got.LocalPath, "/home/user/my-app")
	}

	// GetByPath
	got, err = store.GetByPath(ctx, "/home/user/my-app")
	if err != nil {
		t.Fatalf("get by path: %v", err)
	}
	if got.ID != repo.ID {
		t.Errorf("id = %q, want %q", got.ID, repo.ID)
	}

	// List
	repos, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(repos) != 1 {
		t.Errorf("list len = %d, want 1", len(repos))
	}

	// Update
	newName := "my-updated-app"
	updated, err := store.Update(ctx, repo.ID, UpdateRepoParams{
		DisplayName: &newName,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.DisplayName != "my-updated-app" {
		t.Errorf("display_name = %q, want %q", updated.DisplayName, "my-updated-app")
	}

	// Delete
	if err := store.Delete(ctx, repo.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = store.Get(ctx, repo.ID)
	if err != sql.ErrNoRows {
		t.Errorf("get after delete: got %v, want sql.ErrNoRows", err)
	}
}

func TestRepoStore_UniqueLocalPath(t *testing.T) {
	db := setupTestDB(t)
	store := NewRepoStore(db)
	ctx := context.Background()

	params := CreateRepoParams{
		DisplayName:       "repo1",
		LocalPath:         "/tmp/same-path",
		OriginURL:         "https://github.com/test/repo1.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	}
	if _, err := store.Create(ctx, params); err != nil {
		t.Fatalf("first create: %v", err)
	}

	params.DisplayName = "repo2"
	params.OriginURL = "https://github.com/test/repo2.git"
	_, err := store.Create(ctx, params)
	if err == nil {
		t.Error("expected error for duplicate local_path")
	}
}

func TestSessionStore_CRUD(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewSessionStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)

	// Create
	sess, err := store.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Add login page",
		Plan:         "Create a login form with email/password",
		WorktreePath: "/tmp/worktrees/login-page",
		BranchName:   "feat/login-page",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sess.State != machine.CreatingWorktree {
		t.Errorf("state = %v, want CreatingWorktree", sess.State)
	}
	if !sess.AutomationEnabled {
		t.Error("automation_enabled should default to true")
	}

	// Get
	got, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "Add login page" {
		t.Errorf("title = %q, want %q", got.Title, "Add login page")
	}

	// Update
	newState := int(machine.ImplementingPlan)
	updated, err := store.Update(ctx, sess.ID, UpdateSessionParams{
		State: &newState,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.State != machine.ImplementingPlan {
		t.Errorf("state = %v, want ImplementingPlan", updated.State)
	}

	// List
	sessions, err := store.List(ctx, repo.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sessions) != 1 {
		t.Errorf("list len = %d, want 1", len(sessions))
	}

	// Delete
	if err := store.Delete(ctx, sess.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = store.Get(ctx, sess.ID)
	if err != sql.ErrNoRows {
		t.Errorf("get after delete: got %v, want sql.ErrNoRows", err)
	}
}

func TestSessionStore_ArchiveResurrect(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewSessionStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, err := store.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Archive test",
		WorktreePath: "/tmp/wt/archive",
		BranchName:   "feat/archive",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Archive
	if err := store.Archive(ctx, sess.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	archived, _ := store.Get(ctx, sess.ID)
	if archived.ArchivedAt == nil {
		t.Error("archived_at should be set after archive")
	}

	// ListActive should exclude archived
	active, _ := store.ListActive(ctx, repo.ID)
	if len(active) != 0 {
		t.Errorf("active sessions = %d, want 0", len(active))
	}

	// ListArchived should include it
	archivedList, _ := store.ListArchived(ctx, repo.ID)
	if len(archivedList) != 1 {
		t.Errorf("archived sessions = %d, want 1", len(archivedList))
	}

	// Resurrect
	if err := store.Resurrect(ctx, sess.ID); err != nil {
		t.Fatalf("resurrect: %v", err)
	}
	resurrected, _ := store.Get(ctx, sess.ID)
	if resurrected.ArchivedAt != nil {
		t.Error("archived_at should be nil after resurrect")
	}

	// Double archive is idempotent
	if err := store.Archive(ctx, sess.ID); err != nil {
		t.Fatalf("second archive: %v", err)
	}
	// Double resurrect should work too
	if err := store.Resurrect(ctx, sess.ID); err != nil {
		t.Fatalf("second resurrect: %v", err)
	}
}

func TestAttemptStore_CRUD(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	store := NewAttemptStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, _ := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "Attempt test",
		WorktreePath: "/tmp/wt/attempt",
		BranchName:   "feat/attempt",
		BaseBranch:   "main",
	})

	// Create
	attempt, err := store.Create(ctx, CreateAttemptParams{
		SessionID: sess.ID,
		Trigger:   int(models.AttemptTriggerCheckFailure),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if attempt.Trigger != models.AttemptTriggerCheckFailure {
		t.Errorf("trigger = %v, want CheckFailure", attempt.Trigger)
	}
	if attempt.Result != models.AttemptResultUnspecified {
		t.Errorf("result = %v, want Unspecified", attempt.Result)
	}

	// Update
	resultSuccess := int(models.AttemptResultSuccess)
	updated, err := store.Update(ctx, attempt.ID, UpdateAttemptParams{
		Result: &resultSuccess,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Result != models.AttemptResultSuccess {
		t.Errorf("result = %v, want Success", updated.Result)
	}

	// ListBySession
	attempts, err := store.ListBySession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(attempts) != 1 {
		t.Errorf("list len = %d, want 1", len(attempts))
	}

	// Delete
	if err := store.Delete(ctx, attempt.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = store.Get(ctx, attempt.ID)
	if err != sql.ErrNoRows {
		t.Errorf("get after delete: got %v, want sql.ErrNoRows", err)
	}
}

func TestForeignKeyCascade_DeleteRepo(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	attemptStore := NewAttemptStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, _ := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "FK cascade test",
		WorktreePath: "/tmp/wt/fk",
		BranchName:   "feat/fk",
		BaseBranch:   "main",
	})
	attemptStore.Create(ctx, CreateAttemptParams{
		SessionID: sess.ID,
		Trigger:   int(models.AttemptTriggerConflict),
	})

	// Delete repo should cascade to sessions and attempts
	if err := repoStore.Delete(ctx, repo.ID); err != nil {
		t.Fatalf("delete repo: %v", err)
	}

	_, err := sessionStore.Get(ctx, sess.ID)
	if err != sql.ErrNoRows {
		t.Errorf("session should be deleted by cascade: got %v", err)
	}

	attempts, _ := attemptStore.ListBySession(ctx, sess.ID)
	if len(attempts) != 0 {
		t.Errorf("attempts should be deleted by cascade: got %d", len(attempts))
	}
}

func TestForeignKeyCascade_DeleteSession(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	attemptStore := NewAttemptStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	sess, _ := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "FK cascade session",
		WorktreePath: "/tmp/wt/fk2",
		BranchName:   "feat/fk2",
		BaseBranch:   "main",
	})
	attemptStore.Create(ctx, CreateAttemptParams{
		SessionID: sess.ID,
		Trigger:   int(models.AttemptTriggerCheckFailure),
	})

	// Delete session should cascade to attempts
	if err := sessionStore.Delete(ctx, sess.ID); err != nil {
		t.Fatalf("delete session: %v", err)
	}

	attempts, _ := attemptStore.ListBySession(ctx, sess.ID)
	if len(attempts) != 0 {
		t.Errorf("attempts should be deleted by cascade: got %d", len(attempts))
	}
}
