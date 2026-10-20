package db

import (
	"context"
	"os"
	"testing"

	"github.com/recurser/bossd/internal/dbtest"
)

const fixAttemptRenameVersion int64 = 20261020130000
const beforeFixAttemptRenameVersion int64 = 20261020120000

func TestFixAttemptRenameMigration(t *testing.T) {
	migrations := os.DirFS(migrationsDir())
	ctx := context.Background()
	database, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := dbtest.RunUpTo(database, migrations, beforeFixAttemptRenameVersion); err != nil {
		t.Fatal(err)
	}
	repo := insertPreMigrationRepo(t, database)
	if _, err := database.ExecContext(ctx, `INSERT INTO sessions (id, repo_id, title, worktree_path, branch_name, base_branch) VALUES ('fix-session', ?, 'Fix migration', '/tmp/fix', 'fix', 'main')`, repo.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO attempts (id, session_id, trigger, result, error, created_at, updated_at) VALUES ('fix-attempt', 'fix-session', 1, 2, 'check failed', '2026-10-20T00:00:00.000Z', '2026-10-20T00:01:00.000Z')`); err != nil {
		t.Fatal(err)
	}
	assertState := func(table, index, absentTable, absentIndex string) {
		t.Helper()
		var sessionID, failure, createdAt, updatedAt string
		var trigger, result int
		if err := database.QueryRowContext(ctx, `SELECT session_id, trigger, result, error, created_at, updated_at FROM `+table+` WHERE id = 'fix-attempt'`).Scan(&sessionID, &trigger, &result, &failure, &createdAt, &updatedAt); err != nil {
			t.Fatal(err)
		}
		if sessionID != "fix-session" || trigger != 1 || result != 2 || failure != "check failed" || createdAt != "2026-10-20T00:00:00.000Z" || updatedAt != "2026-10-20T00:01:00.000Z" {
			t.Fatalf("row changed: %q %d %d %q %q %q", sessionID, trigger, result, failure, createdAt, updatedAt)
		}
		for name, want := range map[string]int{index: 1, absentTable: 0, absentIndex: 0} {
			var count int
			if err := database.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != want {
				t.Errorf("schema object %s count = %d, want %d", name, count, want)
			}
		}
		var indexedColumn string
		if err := database.QueryRowContext(ctx, `SELECT name FROM pragma_index_info(?)`, index).Scan(&indexedColumn); err != nil {
			t.Fatal(err)
		}
		if indexedColumn != "session_id" {
			t.Errorf("index column = %s, want session_id", indexedColumn)
		}
	}
	if err := dbtest.RunUpTo(database, migrations, fixAttemptRenameVersion); err != nil {
		t.Fatal(err)
	}
	assertState("fix_attempts", "idx_fix_attempts_session_id", "attempts", "idx_attempts_session_id")
	if err := dbtest.RunDownTo(database, migrations, beforeFixAttemptRenameVersion); err != nil {
		t.Fatal(err)
	}
	assertState("attempts", "idx_attempts_session_id", "fix_attempts", "idx_fix_attempts_session_id")
}
