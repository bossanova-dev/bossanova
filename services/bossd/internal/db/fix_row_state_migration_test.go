package db

import (
	"context"
	"os"
	"testing"

	"github.com/recurser/bossd/internal/dbtest"
)

// fixRowStateVersion is the goose timestamp of the migration that adds the
// session_resume_attempts ledger and the sessions.dismissed_alert_* columns.
const fixRowStateVersion int64 = 20261013000000

// TestFixRowStateMigration applies the fix-row-state migration over a database
// that already holds a session, asserts the new table, index and columns (and
// the columns' backfill), then rolls it back and asserts they are gone while
// the session stays readable.
func TestFixRowStateMigration(t *testing.T) {
	migrations := os.DirFS(migrationsDir())
	ctx := context.Background()

	db, err := OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := dbtest.RunUpTo(db, migrations, cronManagedByVersion); err != nil {
		t.Fatalf("run up to %d: %v", cronManagedByVersion, err)
	}
	if _, ok := tableColumns(t, db, "sessions")["dismissed_alert_key"]; ok {
		t.Fatal("dismissed_alert_key present before its migration; the row below would not be pre-migration")
	}
	repo := insertPreMigrationRepo(t, db)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO sessions (id, repo_id, title, worktree_path, branch_name, base_branch)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"sess-pre-fix-row", repo.ID, "Pre-migration session", "/tmp/wt/pre", "feat/pre", "main",
	); err != nil {
		t.Fatalf("insert pre-migration session: %v", err)
	}

	if err := dbtest.RunUpTo(db, migrations, fixRowStateVersion); err != nil {
		t.Fatalf("run up to %d: %v", fixRowStateVersion, err)
	}

	cols := tableColumns(t, db, "session_resume_attempts")
	for name, typ := range map[string]string{
		"id": "TEXT", "session_id": "TEXT", "run_key": "TEXT", "attempt_number": "INTEGER",
		"attempted_at": "TEXT", "failure_class": "TEXT", "failure_detail": "TEXT", "created_at": "TEXT",
	} {
		if got, ok := cols[name]; !ok || got != typ {
			t.Errorf("session_resume_attempts.%s = %q (present %v), want %q", name, got, ok, typ)
		}
	}
	var indexed int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_session_resume_attempts_session_id_run_key_attempt_number'`,
	).Scan(&indexed); err != nil {
		t.Fatalf("look up index: %v", err)
	}
	if indexed != 1 {
		t.Error("idx_session_resume_attempts_session_id_run_key_attempt_number missing after migration")
	}

	sessCols := tableColumns(t, db, "sessions")
	if sessCols["dismissed_alert_key"] != "TEXT" || sessCols["dismissed_alert_at"] != "TEXT" {
		t.Fatalf("sessions dismissal columns = %q/%q, want TEXT/TEXT",
			sessCols["dismissed_alert_key"], sessCols["dismissed_alert_at"])
	}
	var key string
	var dismissedAt *string
	if err := db.QueryRowContext(ctx,
		`SELECT dismissed_alert_key, dismissed_alert_at FROM sessions WHERE id = ?`, "sess-pre-fix-row",
	).Scan(&key, &dismissedAt); err != nil {
		t.Fatalf("read backfilled dismissal: %v", err)
	}
	if key != "" || dismissedAt != nil {
		t.Errorf("backfilled dismissal = %q/%v, want empty/NULL", key, dismissedAt)
	}

	// The CHECK constraint rejects a class outside the policy's set.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO session_resume_attempts (id, session_id, run_key, attempt_number, attempted_at, failure_class)
		 VALUES ('a1', 'sess-pre-fix-row', 'run', 1, '2026-10-13T00:00:00.000Z', 'bogus')`,
	); err == nil {
		t.Error("insert with an unknown failure_class succeeded, want CHECK failure")
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO session_resume_attempts (id, session_id, run_key, attempt_number, attempted_at)
		 VALUES ('a2', 'sess-pre-fix-row', 'run', 1, '2026-10-13T00:00:00.000Z')`,
	); err != nil {
		t.Fatalf("insert with default class: %v", err)
	}
	// The unique index rejects a second row claiming the same attempt number
	// in the same run, while the same number in another run is allowed.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO session_resume_attempts (id, session_id, run_key, attempt_number, attempted_at)
		 VALUES ('a3', 'sess-pre-fix-row', 'run', 1, '2026-10-13T00:00:01.000Z')`,
	); err == nil {
		t.Error("duplicate (session_id, run_key, attempt_number) insert succeeded, want UNIQUE failure")
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO session_resume_attempts (id, session_id, run_key, attempt_number, attempted_at)
		 VALUES ('a4', 'sess-pre-fix-row', 'other-run', 1, '2026-10-13T00:00:01.000Z')`,
	); err != nil {
		t.Fatalf("insert attempt 1 of another run: %v", err)
	}

	if err := dbtest.RunDownTo(db, migrations, cronManagedByVersion); err != nil {
		t.Fatalf("run down to %d: %v", cronManagedByVersion, err)
	}
	if got := tableColumns(t, db, "session_resume_attempts"); len(got) != 0 {
		t.Fatalf("session_resume_attempts still present after down: %v", got)
	}
	sessCols = tableColumns(t, db, "sessions")
	if _, ok := sessCols["dismissed_alert_key"]; ok {
		t.Fatal("dismissed_alert_key still present after down")
	}
	if _, ok := sessCols["dismissed_alert_at"]; ok {
		t.Fatal("dismissed_alert_at still present after down")
	}
	var title string
	if err := db.QueryRowContext(ctx,
		`SELECT title FROM sessions WHERE id = ?`, "sess-pre-fix-row",
	).Scan(&title); err != nil {
		t.Fatalf("session unreadable after down: %v", err)
	}
	if title != "Pre-migration session" {
		t.Errorf("title after down = %q, want %q", title, "Pre-migration session")
	}
}
