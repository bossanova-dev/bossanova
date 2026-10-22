package db

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/recurser/bossd/internal/dbtest"
)

const resumeBudgetKeyVersion int64 = 20261020140000

func TestResumeBudgetKeyMigration(t *testing.T) {
	migrations := os.DirFS(migrationsDir())
	ctx := context.Background()
	database, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := dbtest.RunUpTo(database, migrations, fixAttemptRenameVersion); err != nil {
		t.Fatal(err)
	}
	repo := insertPreMigrationRepo(t, database)
	if _, err := database.ExecContext(ctx, `INSERT INTO sessions (id, repo_id, title, worktree_path, branch_name, base_branch) VALUES ('budget-session', ?, 'Budget migration', '/tmp/budget', 'budget', 'main')`, repo.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO session_resume_attempts (id, session_id, run_key, attempt_number, attempted_at, failure_class, failure_detail, created_at) VALUES
 ('budget-a', 'budget-session', 'original-chat', 1, '2026-10-20T00:00:00.000Z', 'crash', 'first failure', '2026-10-20T00:01:00.000Z'),
 ('budget-b', 'budget-session', 'original-chat', 2, '2026-10-20T00:02:00.000Z', 'auth_expired', 'second failure', '2026-10-20T00:03:00.000Z')`); err != nil {
		t.Fatal(err)
	}
	assertState := func(column, absentColumn string) {
		t.Helper()
		cols := tableColumns(t, database, "session_resume_attempts")
		if cols[column] != "TEXT" {
			t.Errorf("%s type = %q, want TEXT", column, cols[column])
		}
		if _, exists := cols[absentColumn]; exists {
			t.Errorf("old column %s still exists", absentColumn)
		}
		index := "idx_session_resume_attempts_session_id_" + column + "_attempt_number"
		absentIndex := "idx_session_resume_attempts_session_id_" + absentColumn + "_attempt_number"
		for name, want := range map[string]int{index: 1, absentIndex: 0} {
			var count int
			if err := database.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != want {
				t.Errorf("index %s count = %d, want %d", name, count, want)
			}
		}
		var unique int
		if err := database.QueryRowContext(ctx, `SELECT "unique" FROM pragma_index_list('session_resume_attempts') WHERE name = ?`, index).Scan(&unique); err != nil {
			t.Fatal(err)
		}
		if unique != 1 {
			t.Error("budget index is not unique")
		}
		var columns string
		if err := database.QueryRowContext(ctx, `SELECT group_concat(name, ',') FROM (SELECT name FROM pragma_index_info(?) ORDER BY seqno)`, index).Scan(&columns); err != nil {
			t.Fatal(err)
		}
		if want := "session_id," + column + ",attempt_number"; columns != want {
			t.Errorf("indexed columns = %q, want %q", columns, want)
		}
		rows, err := database.QueryContext(ctx, `SELECT id, session_id, `+column+`, attempt_number, attempted_at, failure_class, failure_detail, created_at FROM session_resume_attempts ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var got [][]any
		for rows.Next() {
			var id, sessionID, key, attemptedAt, class, detail, createdAt string
			var number int
			if err := rows.Scan(&id, &sessionID, &key, &number, &attemptedAt, &class, &detail, &createdAt); err != nil {
				t.Fatal(err)
			}
			got = append(got, []any{id, sessionID, key, number, attemptedAt, class, detail, createdAt})
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		want := [][]any{
			{"budget-a", "budget-session", "original-chat", 1, "2026-10-20T00:00:00.000Z", "crash", "first failure", "2026-10-20T00:01:00.000Z"},
			{"budget-b", "budget-session", "original-chat", 2, "2026-10-20T00:02:00.000Z", "auth_expired", "second failure", "2026-10-20T00:03:00.000Z"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("rows = %#v, want %#v", got, want)
		}
		if _, err := database.ExecContext(ctx, `INSERT INTO session_resume_attempts (id, session_id, `+column+`, attempt_number, attempted_at) VALUES ('duplicate', 'budget-session', 'original-chat', 1, '2026-10-20T00:04:00.000Z')`); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed") {
			t.Errorf("duplicate insert error = %v, want UNIQUE constraint failure", err)
		}
	}
	if err := dbtest.RunUpTo(database, migrations, resumeBudgetKeyVersion); err != nil {
		t.Fatal(err)
	}
	assertState("budget_key", "run_key")
	if err := dbtest.RunDownTo(database, migrations, fixAttemptRenameVersion); err != nil {
		t.Fatal(err)
	}
	assertState("run_key", "budget_key")
}
