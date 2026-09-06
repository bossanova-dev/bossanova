package db

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/recurser/bossd/internal/dbtest"
)

// uniqueAgentSessionVersion is the goose timestamp of the BOS-1143 migration
// that makes agent_chats.agent_session_id UNIQUE.
const uniqueAgentSessionVersion int64 = 20260904000000

// preUniqueAgentSessionVersion is the migration immediately before it; rolling
// down to it exercises the Down step.
const preUniqueAgentSessionVersion int64 = 20260827000000

// agentSessionIndexName is the index both the original non-unique migration and
// the BOS-1143 replacement declare. The name is reused deliberately: the
// migration replaces the index in place rather than leaving two indexes over
// the same column.
const agentSessionIndexName = "idx_agent_chats_agent_session_id"

// agentSessionIDIndexIsUnique reports whether the named index over agent_chats
// exists and whether SQLite considers it unique.
//
// This reads the live schema through PRAGMA index_list rather than matching the
// migration's SQL text. The uniqueness is what the resume path's in-place
// UPDATE ... WHERE agent_session_id = ? depends on being true of the database;
// a text assertion would pass on SQL that never applied, and would fail on a
// later rewrite that kept the same guarantee.
func agentSessionIDIndexIsUnique(t *testing.T, db *sql.DB) (exists, unique bool) {
	t.Helper()
	rows, err := db.Query("PRAGMA index_list(agent_chats)")
	if err != nil {
		t.Fatalf("pragma index_list(agent_chats): %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var seq int
		var name string
		var isUnique int
		var origin, partial any
		if err := rows.Scan(&seq, &name, &isUnique, &origin, &partial); err != nil {
			t.Fatalf("scan index_list: %v", err)
		}
		if name == agentSessionIndexName {
			exists, unique = true, isUnique == 1
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return exists, unique
}

// seedChatSession creates the repo + session an agent_chats row needs, and
// returns the session id. agent_chats declares a foreign key onto sessions, so
// the raw inserts below cannot use a made-up session id.
func seedChatSession(t *testing.T, db *sql.DB) string {
	t.Helper()
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(db))
	sess, err := NewSessionStore(db).Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "unique agent_session_id migration",
		WorktreePath: "/tmp/wt/unique-agent-session",
		BranchName:   "feat/unique-agent-session",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return sess.ID
}

// TestAgentChatsUniqueAgentSessionMigrationApplied asserts the migration is
// applied at its expected goose version and that the index over
// agent_session_id is present and UNIQUE afterwards.
func TestAgentChatsUniqueAgentSessionMigrationApplied(t *testing.T) {
	db := setupTestDB(t)

	var applied int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM goose_db_version WHERE version_id = ?", uniqueAgentSessionVersion,
	).Scan(&applied); err != nil {
		t.Fatalf("query goose version: %v", err)
	}
	if applied == 0 {
		t.Fatalf("migration version %d not applied", uniqueAgentSessionVersion)
	}

	exists, unique := agentSessionIDIndexIsUnique(t, db)
	if !exists {
		t.Fatalf("index %s missing after migration", agentSessionIndexName)
	}
	if !unique {
		t.Errorf("index %s is not UNIQUE; the resume path's in-place update assumes exactly one row per agent_session_id (BOS-1143)", agentSessionIndexName)
	}
}

// TestAgentChatsUniqueAgentSessionMigrationRejectsDuplicates asserts the
// constraint behaviourally: a second row carrying an agent_session_id that is
// already taken is refused by the database. That refusal is the whole point --
// it is what lets resume update a row in place instead of deleting it.
func TestAgentChatsUniqueAgentSessionMigrationRejectsDuplicates(t *testing.T) {
	db := setupTestDB(t)
	sessionID := seedChatSession(t, db)

	const insert = `INSERT INTO agent_chats (id, session_id, agent_session_id, agent_name, created_at)
		VALUES (?, ?, ?, 'codex', datetime('now'))`
	if _, err := db.Exec(insert, "chat-1", sessionID, "agent-dup"); err != nil {
		t.Fatalf("insert first chat: %v", err)
	}
	if _, err := db.Exec(insert, "chat-2", sessionID, "agent-dup"); err == nil {
		t.Fatal("second row with a duplicate agent_session_id was accepted; the UNIQUE index is not in force")
	}

	// A different id still inserts, so the constraint is on the column and not
	// an accidental blanket rejection.
	if _, err := db.Exec(insert, "chat-3", sessionID, "agent-other"); err != nil {
		t.Fatalf("insert distinct agent_session_id: %v", err)
	}
}

// TestAgentChatsUniqueAgentSessionMigrationRefusesPreexistingDuplicates is the
// fail-loud half of the contract, and it is the half the other tests cannot
// reach: they insert duplicates into an already-migrated database and watch the
// INSERT fail, which only proves the index is in force once it exists. This one
// puts the duplicates in FIRST, on the pre-migration schema, and asserts the
// MIGRATION itself refuses to apply.
//
// The migration deliberately performs no dedupe -- it must stop the upgrade and
// hand the reconciliation to an operator rather than silently choosing which
// row to destroy, because the row it would drop is exactly the provider
// identity BOS-1143 exists to preserve. A migration that quietly applied here
// would erase that data on every deployment with pre-existing duplicates, with
// no error to notice.
func TestAgentChatsUniqueAgentSessionMigrationRefusesPreexistingDuplicates(t *testing.T) {
	db := dbtest.NewEmpty(t)
	fsys := os.DirFS(migrationsDir())

	if err := dbtest.RunUpTo(db, fsys, preUniqueAgentSessionVersion); err != nil {
		t.Fatalf("migrate up to the pre-BOS-1143 schema: %v", err)
	}
	if exists, unique := agentSessionIDIndexIsUnique(t, db); !exists || unique {
		t.Fatalf("pre-migration index state = (exists %v, unique %v), want (true, false); "+
			"the duplicates below could not be seeded otherwise", exists, unique)
	}

	sessionID := seedChatSession(t, db)
	const insert = `INSERT INTO agent_chats (id, session_id, agent_session_id, agent_name, created_at)
		VALUES (?, ?, ?, 'codex', datetime('now'))`
	if _, err := db.Exec(insert, "chat-1", sessionID, "agent-dup"); err != nil {
		t.Fatalf("seed first duplicate: %v", err)
	}
	if _, err := db.Exec(insert, "chat-2", sessionID, "agent-dup"); err != nil {
		t.Fatalf("seed second duplicate: %v; the pre-migration schema must accept it", err)
	}

	if err := dbtest.RunUpTo(db, fsys, uniqueAgentSessionVersion); err == nil {
		t.Fatal("migration applied over duplicate agent_session_id rows; it must fail loudly so an operator reconciles them by hand")
	}

	// The refusal must leave the duplicates intact: no dedupe, no partial
	// destruction of the rows the operator is being asked to reconcile.
	var count int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM agent_chats WHERE agent_session_id = ?", "agent-dup",
	).Scan(&count); err != nil {
		t.Fatalf("count duplicate rows after the refusal: %v", err)
	}
	if count != 2 {
		t.Errorf("%d rows carry agent-dup after the refused migration, want 2 (the migration must not delete anything)", count)
	}
	if _, unique := agentSessionIDIndexIsUnique(t, db); unique {
		t.Error("the UNIQUE index is in force after a refused migration")
	}
}

// TestAgentChatsUniqueAgentSessionMigrationDown asserts the Down step restores
// the original non-unique index rather than dropping the index outright: the
// pre-BOS-1143 schema still needs the lookup path this index serves.
func TestAgentChatsUniqueAgentSessionMigrationDown(t *testing.T) {
	db := dbtest.NewMigrated(t)
	if err := dbtest.RunDownTo(db, os.DirFS(migrationsDir()), preUniqueAgentSessionVersion); err != nil {
		t.Fatalf("run down: %v", err)
	}

	exists, unique := agentSessionIDIndexIsUnique(t, db)
	if !exists {
		t.Fatalf("index %s missing after down; the non-unique lookup index must be restored", agentSessionIndexName)
	}
	if unique {
		t.Errorf("index %s still UNIQUE after down; Down must restore the non-unique index", agentSessionIndexName)
	}

	// Behavioural counterpart: duplicates are accepted again once rolled back.
	sessionID := seedChatSession(t, db)
	const insert = `INSERT INTO agent_chats (id, session_id, agent_session_id, agent_name, created_at)
		VALUES (?, ?, ?, 'codex', datetime('now'))`
	if _, err := db.Exec(insert, "chat-1", sessionID, "agent-dup"); err != nil {
		t.Fatalf("insert first chat after down: %v", err)
	}
	if _, err := db.Exec(insert, "chat-2", sessionID, "agent-dup"); err != nil {
		t.Errorf("duplicate rejected after down: %v; the rollback did not restore the pre-migration shape", err)
	}
}
