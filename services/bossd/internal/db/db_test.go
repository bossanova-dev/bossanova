package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	t.Cleanup(func() { _ = db.Close() })
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

// TestMissingIndexesUsed is the acceptance test for the
// 20260421170000_add_missing_indexes migration. It asserts that
// SQLite's query planner picks the new indexes instead of falling back to a
// full table scan.
func TestMissingIndexesUsed(t *testing.T) {
	db := setupTestDB(t)

	cases := []struct {
		name  string
		query string
		index string
	}{
		{
			name:  "workflows.repo_id",
			query: "SELECT id FROM workflows WHERE repo_id = ?",
			index: "idx_workflows_repo_id",
		},
		{
			name:  "claude_chats.claude_id",
			query: "SELECT id FROM claude_chats WHERE claude_id = ?",
			index: "idx_claude_chats_claude_id",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := db.Query("EXPLAIN QUERY PLAN "+tc.query, "x")
			if err != nil {
				t.Fatalf("explain: %v", err)
			}
			defer func() { _ = rows.Close() }()

			var plan string
			for rows.Next() {
				var id, parent, notused int
				var detail string
				if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
					t.Fatalf("scan: %v", err)
				}
				plan += detail + "\n"
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
			if !strings.Contains(plan, tc.index) {
				t.Errorf("plan for %q does not use %s:\n%s", tc.query, tc.index, plan)
			}
		})
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

// TestSessionStore_ListActiveWithRepo verifies the join-based list returns
// each session alongside its repo's display name in a single query, rather
// than the old N+1 list-then-loop pattern. The structural guarantee (single
// QueryContext) lives in the implementation; this test pins the behavior.
func TestSessionStore_ListActiveWithRepo(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewSessionStore(db)
	ctx := context.Background()

	// Two repos with distinct display names so we can verify correct pairing.
	repoA, err := repoStore.Create(ctx, CreateRepoParams{
		DisplayName:       "repo-a",
		LocalPath:         "/tmp/a",
		OriginURL:         "https://github.com/test/a.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/wt/a",
	})
	if err != nil {
		t.Fatalf("create repoA: %v", err)
	}
	repoB, err := repoStore.Create(ctx, CreateRepoParams{
		DisplayName:       "repo-b",
		LocalPath:         "/tmp/b",
		OriginURL:         "https://github.com/test/b.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/wt/b",
	})
	if err != nil {
		t.Fatalf("create repoB: %v", err)
	}

	// Three active sessions in repoA, two in repoB, plus one archived that
	// must be excluded.
	for i, rid := range []string{repoA.ID, repoA.ID, repoA.ID, repoB.ID, repoB.ID} {
		if _, err := store.Create(ctx, CreateSessionParams{
			RepoID:       rid,
			Title:        "s",
			WorktreePath: "/tmp/wt/x",
			BranchName:   "feat/x",
			BaseBranch:   "main",
		}); err != nil {
			t.Fatalf("create session %d: %v", i, err)
		}
	}
	archived, _ := store.Create(ctx, CreateSessionParams{
		RepoID: repoA.ID, Title: "arch", WorktreePath: "/tmp/wt/a", BranchName: "a", BaseBranch: "main",
	})
	if err := store.Archive(ctx, archived.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}

	// Unfiltered: all active sessions, each paired with its repo name.
	all, err := store.ListActiveWithRepo(ctx, "")
	if err != nil {
		t.Fatalf("list active with repo: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("len = %d, want 5", len(all))
	}
	for _, r := range all {
		switch r.RepoID {
		case repoA.ID:
			if r.RepoDisplayName != "repo-a" {
				t.Errorf("repoA session got display %q, want repo-a", r.RepoDisplayName)
			}
		case repoB.ID:
			if r.RepoDisplayName != "repo-b" {
				t.Errorf("repoB session got display %q, want repo-b", r.RepoDisplayName)
			}
		default:
			t.Errorf("unexpected repoID %q", r.RepoID)
		}
	}

	// Filtered by repoA: only repoA's three active sessions.
	onlyA, err := store.ListActiveWithRepo(ctx, repoA.ID)
	if err != nil {
		t.Fatalf("list filtered: %v", err)
	}
	if len(onlyA) != 3 {
		t.Fatalf("filtered len = %d, want 3", len(onlyA))
	}
	for _, r := range onlyA {
		if r.RepoID != repoA.ID || r.RepoDisplayName != "repo-a" {
			t.Errorf("filtered row repoID=%q display=%q", r.RepoID, r.RepoDisplayName)
		}
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

func TestRepoStore_SettingsFields(t *testing.T) {
	db := setupTestDB(t)
	store := NewRepoStore(db)
	ctx := context.Background()

	// Create repo — verify defaults.
	repo, err := store.Create(ctx, CreateRepoParams{
		DisplayName:       "settings-test",
		LocalPath:         "/tmp/settings-test",
		OriginURL:         "https://github.com/test/settings.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if repo.CanAutoMerge {
		t.Error("CanAutoMerge should default to false")
	}
	if !repo.CanAutoMergeDependabot {
		t.Error("CanAutoMergeDependabot should default to true")
	}
	if !repo.CanAutoAddressReviews {
		t.Error("CanAutoAddressReviews should default to true")
	}
	if !repo.CanAutoResolveConflicts {
		t.Error("CanAutoResolveConflicts should default to true")
	}

	// Update each field and verify.
	trueVal := true
	falseVal := false

	updated, err := store.Update(ctx, repo.ID, UpdateRepoParams{
		CanAutoMerge: &trueVal,
	})
	if err != nil {
		t.Fatalf("update CanAutoMerge: %v", err)
	}
	if !updated.CanAutoMerge {
		t.Error("CanAutoMerge should be true after update")
	}

	updated, err = store.Update(ctx, repo.ID, UpdateRepoParams{
		CanAutoMergeDependabot: &falseVal,
	})
	if err != nil {
		t.Fatalf("update CanAutoMergeDependabot: %v", err)
	}
	if updated.CanAutoMergeDependabot {
		t.Error("CanAutoMergeDependabot should be false after update")
	}

	updated, err = store.Update(ctx, repo.ID, UpdateRepoParams{
		CanAutoAddressReviews: &falseVal,
	})
	if err != nil {
		t.Fatalf("update CanAutoAddressReviews: %v", err)
	}
	if updated.CanAutoAddressReviews {
		t.Error("CanAutoAddressReviews should be false after update")
	}

	updated, err = store.Update(ctx, repo.ID, UpdateRepoParams{
		CanAutoResolveConflicts: &falseVal,
	})
	if err != nil {
		t.Fatalf("update CanAutoResolveConflicts: %v", err)
	}
	if updated.CanAutoResolveConflicts {
		t.Error("CanAutoResolveConflicts should be false after update")
	}

	// Verify persistence by re-fetching.
	got, err := store.Get(ctx, repo.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.CanAutoMerge {
		t.Error("CanAutoMerge should persist as true")
	}
	if got.CanAutoMergeDependabot {
		t.Error("CanAutoMergeDependabot should persist as false")
	}
	if got.CanAutoAddressReviews {
		t.Error("CanAutoAddressReviews should persist as false")
	}
	if got.CanAutoResolveConflicts {
		t.Error("CanAutoResolveConflicts should persist as false")
	}
}

func TestRepoStore_UpdateOriginURL(t *testing.T) {
	db := setupTestDB(t)
	store := NewRepoStore(db)
	ctx := context.Background()

	// Create repo with empty origin URL.
	repo, err := store.Create(ctx, CreateRepoParams{
		DisplayName:       "no-origin",
		LocalPath:         "/tmp/no-origin",
		OriginURL:         "",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if repo.OriginURL != "" {
		t.Errorf("origin_url = %q, want empty", repo.OriginURL)
	}

	// Update origin URL.
	newURL := "git@github.com:owner/repo.git"
	updated, err := store.Update(ctx, repo.ID, UpdateRepoParams{
		OriginURL: &newURL,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.OriginURL != newURL {
		t.Errorf("origin_url = %q, want %q", updated.OriginURL, newURL)
	}

	// Verify persistence.
	got, err := store.Get(ctx, repo.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.OriginURL != newURL {
		t.Errorf("origin_url = %q, want %q", got.OriginURL, newURL)
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
	if _, err := attemptStore.Create(ctx, CreateAttemptParams{
		SessionID: sess.ID,
		Trigger:   int(models.AttemptTriggerConflict),
	}); err != nil {
		t.Fatalf("create attempt: %v", err)
	}

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
	if _, err := attemptStore.Create(ctx, CreateAttemptParams{
		SessionID: sess.ID,
		Trigger:   int(models.AttemptTriggerCheckFailure),
	}); err != nil {
		t.Fatalf("create attempt: %v", err)
	}

	// Delete session should cascade to attempts
	if err := sessionStore.Delete(ctx, sess.ID); err != nil {
		t.Fatalf("delete session: %v", err)
	}

	attempts, _ := attemptStore.ListBySession(ctx, sess.ID)
	if len(attempts) != 0 {
		t.Errorf("attempts should be deleted by cascade: got %d", len(attempts))
	}
}

func TestSessionStore_AdvanceOrphanedSessions(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	workflowStore := NewWorkflowStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)

	// Create a session in ImplementingPlan with a running workflow.
	sessActive, _ := sessionStore.Create(ctx, CreateSessionParams{
		RepoID: repo.ID, Title: "Active autopilot",
		WorktreePath: "/tmp/wt/active", BranchName: "feat/active", BaseBranch: "main",
	})
	implState := int(machine.ImplementingPlan)
	if _, err := sessionStore.Update(ctx, sessActive.ID, UpdateSessionParams{State: &implState}); err != nil {
		t.Fatalf("update active to implementing: %v", err)
	}
	wf, _ := workflowStore.Create(ctx, CreateWorkflowParams{
		SessionID: sessActive.ID, RepoID: repo.ID, PlanPath: "plan.md", MaxLegs: 1,
	})
	running := string(models.WorkflowStatusRunning)
	if _, err := workflowStore.Update(ctx, wf.ID, UpdateWorkflowParams{Status: &running}); err != nil {
		t.Fatalf("update workflow to running: %v", err)
	}

	// Create a session in ImplementingPlan with NO running workflow (orphaned).
	sessOrphan, _ := sessionStore.Create(ctx, CreateSessionParams{
		RepoID: repo.ID, Title: "Orphaned autopilot",
		WorktreePath: "/tmp/wt/orphan", BranchName: "feat/orphan", BaseBranch: "main",
	})
	if _, err := sessionStore.Update(ctx, sessOrphan.ID, UpdateSessionParams{State: &implState}); err != nil {
		t.Fatalf("update orphan to implementing: %v", err)
	}

	// Create a session in AwaitingChecks (should not be affected).
	sessAwaiting, _ := sessionStore.Create(ctx, CreateSessionParams{
		RepoID: repo.ID, Title: "Awaiting checks",
		WorktreePath: "/tmp/wt/awaiting", BranchName: "feat/awaiting", BaseBranch: "main",
	})
	awaitState := int(machine.AwaitingChecks)
	if _, err := sessionStore.Update(ctx, sessAwaiting.ID, UpdateSessionParams{State: &awaitState}); err != nil {
		t.Fatalf("update awaiting to awaiting_checks: %v", err)
	}

	// Advance orphaned sessions.
	n, err := sessionStore.AdvanceOrphanedSessions(ctx)
	if err != nil {
		t.Fatalf("AdvanceOrphanedSessions: %v", err)
	}
	if n != 1 {
		t.Errorf("AdvanceOrphanedSessions affected %d rows, want 1", n)
	}

	// Orphaned session should now be AwaitingChecks.
	got, _ := sessionStore.Get(ctx, sessOrphan.ID)
	if got.State != machine.AwaitingChecks {
		t.Errorf("orphaned session state = %v, want AwaitingChecks", got.State)
	}

	// Active session should still be ImplementingPlan (has running workflow).
	got, _ = sessionStore.Get(ctx, sessActive.ID)
	if got.State != machine.ImplementingPlan {
		t.Errorf("active session state = %v, want ImplementingPlan", got.State)
	}

	// AwaitingChecks session should be unchanged.
	got, _ = sessionStore.Get(ctx, sessAwaiting.ID)
	if got.State != machine.AwaitingChecks {
		t.Errorf("awaiting session state = %v, want AwaitingChecks", got.State)
	}
}
