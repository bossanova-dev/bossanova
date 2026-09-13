package db

import (
	"database/sql"
	"os"
	"testing"

	"github.com/recurser/bossd/internal/dbtest"
)

const sessionListRankVersion int64 = 20260912000000

const preSessionListRankVersion int64 = 20260904000001

// TestSessionListRankMigrationSchema pins the column's shape: NULLABLE with no
// default is what makes "an untouched database does not reorder" true by
// construction — a NOT NULL DEFAULT 0 would rank every existing row identically
// and collapse the whole natural block into a tie.
func TestSessionListRankMigrationSchema(t *testing.T) {
	db := setupTestDB(t)

	var applied int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM goose_db_version WHERE version_id = ?", sessionListRankVersion,
	).Scan(&applied); err != nil {
		t.Fatalf("query goose version: %v", err)
	}
	if applied == 0 {
		t.Fatalf("migration version %d not applied", sessionListRankVersion)
	}

	got, ok := tableColumns(t, db, "sessions")["list_rank"]
	if !ok {
		t.Fatal("sessions.list_rank missing after migration")
	}
	if got != "INTEGER" {
		t.Errorf("sessions.list_rank type = %q, want INTEGER", got)
	}
	var notnull int
	var dflt *string
	if err := db.QueryRow(
		`SELECT "notnull", dflt_value FROM pragma_table_info('sessions') WHERE name = 'list_rank'`,
	).Scan(&notnull, &dflt); err != nil {
		t.Fatalf("pragma_table_info(sessions.list_rank): %v", err)
	}
	if notnull != 0 {
		t.Errorf("sessions.list_rank notnull = %d, want 0 (nullable)", notnull)
	}
	if dflt != nil {
		t.Errorf("sessions.list_rank default = %q, want none", *dflt)
	}
}

// TestSessionListRankMigrationIsReversible applies the migration up, then down,
// and asserts the session rows survive BOTH directions — a down leg that lost
// rows would make a rollback destructive rather than merely forgetful.
func TestSessionListRankMigrationIsReversible(t *testing.T) {
	migrations := os.DirFS(migrationsDir())
	db, err := OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := dbtest.RunUpTo(db, migrations, preSessionListRankVersion); err != nil {
		t.Fatalf("run up to %d: %v", preSessionListRankVersion, err)
	}
	if _, err := db.Exec(`INSERT INTO repos (id, display_name, local_path, origin_url, worktree_base_dir)
	                      VALUES ('r1', 'repo', '/tmp/repo', 'https://example.test/repo.git', '/tmp/worktrees')`); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	for _, id := range []string{"s1", "s2"} {
		if _, err := db.Exec(`INSERT INTO sessions (id, repo_id, title, worktree_path, branch_name, base_branch)
		                      VALUES (?, 'r1', 'legacy', '/tmp/worktrees/'||?, 'branch-'||?, 'main')`, id, id, id); err != nil {
			t.Fatalf("insert legacy session %s: %v", id, err)
		}
	}

	if err := dbtest.RunUpTo(db, migrations, sessionListRankVersion); err != nil {
		t.Fatalf("run up to %d: %v", sessionListRankVersion, err)
	}
	// Every pre-existing row reads back unranked (R2 by construction).
	var ranked int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE list_rank IS NOT NULL`).Scan(&ranked); err != nil {
		t.Fatalf("count ranked rows: %v", err)
	}
	if ranked != 0 {
		t.Fatalf("%d pre-existing rows came back ranked, want 0", ranked)
	}
	if _, err := db.Exec(`UPDATE sessions SET list_rank = 4294967296 WHERE id = 's1'`); err != nil {
		t.Fatalf("rank s1: %v", err)
	}

	if err := dbtest.RunDownTo(db, migrations, preSessionListRankVersion); err != nil {
		t.Fatalf("run down: %v", err)
	}
	if _, ok := tableColumns(t, db, "sessions")["list_rank"]; ok {
		t.Fatal("list_rank still present after down")
	}
	assertSessionsSurvive(t, db)

	if err := dbtest.RunUpTo(db, migrations, sessionListRankVersion); err != nil {
		t.Fatalf("re-run up after down: %v", err)
	}
	if _, ok := tableColumns(t, db, "sessions")["list_rank"]; !ok {
		t.Fatal("list_rank missing after re-running up")
	}
	assertSessionsSurvive(t, db)
}

// assertSessionsSurvive pins that both seeded session rows are still present.
// The reversibility claim is about the ROWS, not just the column: SQLite's
// DROP COLUMN is in-place here, and a migration that reached for the heavier
// table-rebuild shape instead could lose rows under foreign_keys=ON.
func assertSessionsSurvive(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM sessions ORDER BY id`)
	if err != nil {
		t.Fatalf("read sessions: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan session id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if !equalIDs(ids, []string{"s1", "s2"}) {
		t.Fatalf("sessions after migration = %v, want both rows [s1 s2] to survive", ids)
	}
}
