package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/recurser/bossalib/migrate"
)

// migrationsDir returns the absolute path to the bosso migrations directory.
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

// createTestUser creates a user for testing and returns it.
func createTestUser(t *testing.T, store *SQLiteUserStore) *User {
	t.Helper()
	user, err := store.Create(context.Background(), CreateUserParams{
		Sub:   "auth0|test123",
		Email: "test@example.com",
		Name:  "Test User",
	})
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}
	return user
}

// createTestDaemon creates a daemon for testing and returns it.
func createTestDaemon(t *testing.T, dStore *SQLiteDaemonStore, userID string) *Daemon {
	t.Helper()
	daemon, err := dStore.Create(context.Background(), CreateDaemonParams{
		ID:           newID(),
		UserID:       userID,
		Hostname:     "macbook-pro.local",
		SessionToken: newID() + newID(),
		RepoIDs:      []string{"repo-1", "repo-2"},
	})
	if err != nil {
		t.Fatalf("create test daemon: %v", err)
	}
	return daemon
}

func TestMigrationRunner(t *testing.T) {
	db := setupTestDB(t)

	for _, table := range []string{"users", "daemons", "daemon_repos", "sessions_registry", "audit_log"} {
		var count int
		err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count)
		if err != nil {
			t.Errorf("table %s should exist: %v", table, err)
		}
	}
}

func TestUserStore_CRUD(t *testing.T) {
	db := setupTestDB(t)
	store := NewUserStore(db)
	ctx := context.Background()

	// Create
	user, err := store.Create(ctx, CreateUserParams{
		Sub:   "auth0|abc123",
		Email: "alice@example.com",
		Name:  "Alice",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if user.Sub != "auth0|abc123" {
		t.Errorf("sub = %q, want %q", user.Sub, "auth0|abc123")
	}
	if user.ID == "" {
		t.Error("id should not be empty")
	}

	// Get
	got, err := store.Get(ctx, user.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Email != "alice@example.com" {
		t.Errorf("email = %q, want %q", got.Email, "alice@example.com")
	}

	// GetBySub
	got, err = store.GetBySub(ctx, "auth0|abc123")
	if err != nil {
		t.Fatalf("get by sub: %v", err)
	}
	if got.ID != user.ID {
		t.Errorf("id = %q, want %q", got.ID, user.ID)
	}

	// List
	users, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(users) != 1 {
		t.Errorf("list len = %d, want 1", len(users))
	}

	// Update
	newName := "Alice Updated"
	updated, err := store.Update(ctx, user.ID, UpdateUserParams{
		Name: &newName,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "Alice Updated" {
		t.Errorf("name = %q, want %q", updated.Name, "Alice Updated")
	}

	// Delete
	if err := store.Delete(ctx, user.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = store.Get(ctx, user.ID)
	if err != sql.ErrNoRows {
		t.Errorf("get after delete: got %v, want sql.ErrNoRows", err)
	}
}

func TestUserStore_UniqueSub(t *testing.T) {
	db := setupTestDB(t)
	store := NewUserStore(db)
	ctx := context.Background()

	params := CreateUserParams{
		Sub:   "auth0|same-sub",
		Email: "first@example.com",
		Name:  "First",
	}
	if _, err := store.Create(ctx, params); err != nil {
		t.Fatalf("first create: %v", err)
	}

	params.Email = "second@example.com"
	params.Name = "Second"
	_, err := store.Create(ctx, params)
	if err == nil {
		t.Error("expected error for duplicate sub")
	}
}

func TestDaemonStore_CRUD(t *testing.T) {
	db := setupTestDB(t)
	uStore := NewUserStore(db)
	dStore := NewDaemonStore(db)
	ctx := context.Background()

	user := createTestUser(t, uStore)

	// Create
	daemonID := newID()
	token := newID() + newID()
	daemon, err := dStore.Create(ctx, CreateDaemonParams{
		ID:           daemonID,
		UserID:       user.ID,
		Hostname:     "macbook.local",
		SessionToken: token,
		RepoIDs:      []string{"repo-a", "repo-b"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if daemon.Hostname != "macbook.local" {
		t.Errorf("hostname = %q, want %q", daemon.Hostname, "macbook.local")
	}
	if len(daemon.RepoIDs) != 2 {
		t.Errorf("repo_ids len = %d, want 2", len(daemon.RepoIDs))
	}
	if daemon.Online {
		t.Error("online should default to false")
	}

	// Get
	got, err := dStore.Get(ctx, daemonID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SessionToken != token {
		t.Errorf("session_token mismatch")
	}

	// GetByToken
	got, err = dStore.GetByToken(ctx, token)
	if err != nil {
		t.Fatalf("get by token: %v", err)
	}
	if got.ID != daemonID {
		t.Errorf("id = %q, want %q", got.ID, daemonID)
	}

	// ListByUser
	daemons, err := dStore.ListByUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(daemons) != 1 {
		t.Errorf("list len = %d, want 1", len(daemons))
	}

	// Update
	online := true
	activeSessions := 3
	hb := timeNow()
	updated, err := dStore.Update(ctx, daemonID, UpdateDaemonParams{
		Online:         &online,
		ActiveSessions: &activeSessions,
		LastHeartbeat:  &hb,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !updated.Online {
		t.Error("online should be true after update")
	}
	if updated.ActiveSessions != 3 {
		t.Errorf("active_sessions = %d, want 3", updated.ActiveSessions)
	}
	if updated.LastHeartbeat == nil {
		t.Error("last_heartbeat should be set")
	}

	// UpdateRepos
	if err := dStore.UpdateRepos(ctx, daemonID, []string{"repo-x"}); err != nil {
		t.Fatalf("update repos: %v", err)
	}
	got, _ = dStore.Get(ctx, daemonID)
	if len(got.RepoIDs) != 1 || got.RepoIDs[0] != "repo-x" {
		t.Errorf("repo_ids = %v, want [repo-x]", got.RepoIDs)
	}

	// Delete
	if err := dStore.Delete(ctx, daemonID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = dStore.Get(ctx, daemonID)
	if err != sql.ErrNoRows {
		t.Errorf("get after delete: got %v, want sql.ErrNoRows", err)
	}
}

func TestDaemonStore_UniqueToken(t *testing.T) {
	db := setupTestDB(t)
	uStore := NewUserStore(db)
	dStore := NewDaemonStore(db)
	ctx := context.Background()

	user := createTestUser(t, uStore)
	sameToken := "shared-token-value"

	if _, err := dStore.Create(ctx, CreateDaemonParams{
		ID:           newID(),
		UserID:       user.ID,
		Hostname:     "host1",
		SessionToken: sameToken,
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}

	_, err := dStore.Create(ctx, CreateDaemonParams{
		ID:           newID(),
		UserID:       user.ID,
		Hostname:     "host2",
		SessionToken: sameToken,
	})
	if err == nil {
		t.Error("expected error for duplicate session_token")
	}
}

func TestSessionRegistryStore_CRUD(t *testing.T) {
	db := setupTestDB(t)
	uStore := NewUserStore(db)
	dStore := NewDaemonStore(db)
	sStore := NewSessionRegistryStore(db)
	ctx := context.Background()

	user := createTestUser(t, uStore)
	daemon := createTestDaemon(t, dStore, user.ID)

	// Create
	entry, err := sStore.Create(ctx, CreateSessionEntryParams{
		SessionID: "sess-001",
		DaemonID:  daemon.ID,
		Title:     "Fix login bug",
		State:     2,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if entry.Title != "Fix login bug" {
		t.Errorf("title = %q, want %q", entry.Title, "Fix login bug")
	}

	// Get
	got, err := sStore.Get(ctx, "sess-001")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DaemonID != daemon.ID {
		t.Errorf("daemon_id mismatch")
	}

	// ListByDaemon
	entries, err := sStore.ListByDaemon(ctx, daemon.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("list len = %d, want 1", len(entries))
	}

	// Update (transfer to different daemon)
	newDaemon := createTestDaemon(t, dStore, user.ID)
	updated, err := sStore.Update(ctx, "sess-001", UpdateSessionEntryParams{
		DaemonID: &newDaemon.ID,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.DaemonID != newDaemon.ID {
		t.Errorf("daemon_id = %q, want %q", updated.DaemonID, newDaemon.ID)
	}

	// Delete
	if err := sStore.Delete(ctx, "sess-001"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = sStore.Get(ctx, "sess-001")
	if err != sql.ErrNoRows {
		t.Errorf("get after delete: got %v, want sql.ErrNoRows", err)
	}
}

func TestAuditStore_CreateAndList(t *testing.T) {
	db := setupTestDB(t)
	uStore := NewUserStore(db)
	aStore := NewAuditStore(db)
	ctx := context.Background()

	user := createTestUser(t, uStore)

	// Create entries
	detail := "Registered from macbook.local"
	entry, err := aStore.Create(ctx, CreateAuditParams{
		UserID:   &user.ID,
		Action:   "daemon.register",
		Resource: "daemon/abc123",
		Detail:   &detail,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if entry.Action != "daemon.register" {
		t.Errorf("action = %q, want %q", entry.Action, "daemon.register")
	}

	// Create without user (system action)
	if _, err := aStore.Create(ctx, CreateAuditParams{
		Action:   "system.startup",
		Resource: "orchestrator",
	}); err != nil {
		t.Fatalf("create system entry: %v", err)
	}

	// List all
	entries, err := aStore.List(ctx, AuditListOpts{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("list len = %d, want 2", len(entries))
	}

	// List by user
	entries, err = aStore.List(ctx, AuditListOpts{UserID: &user.ID})
	if err != nil {
		t.Fatalf("list by user: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("list by user len = %d, want 1", len(entries))
	}

	// List by action
	action := "system.startup"
	entries, err = aStore.List(ctx, AuditListOpts{Action: &action})
	if err != nil {
		t.Fatalf("list by action: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("list by action len = %d, want 1", len(entries))
	}

	// List with limit
	entries, err = aStore.List(ctx, AuditListOpts{Limit: 1})
	if err != nil {
		t.Fatalf("list with limit: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("list with limit len = %d, want 1", len(entries))
	}
}

func TestForeignKeyCascade_DeleteUser(t *testing.T) {
	db := setupTestDB(t)
	uStore := NewUserStore(db)
	dStore := NewDaemonStore(db)
	sStore := NewSessionRegistryStore(db)
	ctx := context.Background()

	user := createTestUser(t, uStore)
	daemon := createTestDaemon(t, dStore, user.ID)

	if _, err := sStore.Create(ctx, CreateSessionEntryParams{
		SessionID: "sess-cascade",
		DaemonID:  daemon.ID,
		Title:     "Cascade test",
	}); err != nil {
		t.Fatalf("create session entry: %v", err)
	}

	// Delete user should cascade to daemons, daemon_repos, and sessions_registry
	if err := uStore.Delete(ctx, user.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	_, err := dStore.Get(ctx, daemon.ID)
	if err != sql.ErrNoRows {
		t.Errorf("daemon should be deleted by cascade: got %v", err)
	}

	_, err = sStore.Get(ctx, "sess-cascade")
	if err != sql.ErrNoRows {
		t.Errorf("session entry should be deleted by cascade: got %v", err)
	}
}

func TestForeignKeyCascade_DeleteDaemon(t *testing.T) {
	db := setupTestDB(t)
	uStore := NewUserStore(db)
	dStore := NewDaemonStore(db)
	sStore := NewSessionRegistryStore(db)
	ctx := context.Background()

	user := createTestUser(t, uStore)
	daemon := createTestDaemon(t, dStore, user.ID)

	if _, err := sStore.Create(ctx, CreateSessionEntryParams{
		SessionID: "sess-daemon-cascade",
		DaemonID:  daemon.ID,
		Title:     "Daemon cascade",
	}); err != nil {
		t.Fatalf("create session entry: %v", err)
	}

	// Delete daemon should cascade to daemon_repos and sessions_registry
	if err := dStore.Delete(ctx, daemon.ID); err != nil {
		t.Fatalf("delete daemon: %v", err)
	}

	_, err := sStore.Get(ctx, "sess-daemon-cascade")
	if err != sql.ErrNoRows {
		t.Errorf("session entry should be deleted by cascade: got %v", err)
	}
}

func TestAuditLog_UserDeleteSetsNull(t *testing.T) {
	db := setupTestDB(t)
	uStore := NewUserStore(db)
	aStore := NewAuditStore(db)
	ctx := context.Background()

	user := createTestUser(t, uStore)

	entry, err := aStore.Create(ctx, CreateAuditParams{
		UserID:   &user.ID,
		Action:   "test.action",
		Resource: "test/resource",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Delete user — audit_log.user_id should become NULL (ON DELETE SET NULL)
	if err := uStore.Delete(ctx, user.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	// Audit entry should still exist but with nil user_id
	entries, err := aStore.List(ctx, AuditListOpts{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("list len = %d, want 1", len(entries))
	}
	if entries[0].ID != entry.ID {
		t.Errorf("entry id mismatch")
	}
	if entries[0].UserID != nil {
		t.Errorf("user_id should be nil after user delete, got %v", *entries[0].UserID)
	}
}
