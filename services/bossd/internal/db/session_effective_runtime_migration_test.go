package db

import (
	"os"
	"testing"

	"github.com/recurser/bossd/internal/dbtest"
)

const sessionEffectiveRuntimeVersion int64 = 20260825000002

const preSessionEffectiveRuntimeVersion int64 = 20260825000001

func TestSessionEffectiveRuntimeMigrationSchema(t *testing.T) {
	db := setupTestDB(t)

	var applied int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM goose_db_version WHERE version_id = ?", sessionEffectiveRuntimeVersion,
	).Scan(&applied); err != nil {
		t.Fatalf("query goose version: %v", err)
	}
	if applied == 0 {
		t.Fatalf("migration version %d not applied", sessionEffectiveRuntimeVersion)
	}

	for _, name := range []string{"effective_model", "effective_effort"} {
		got, ok := tableColumns(t, db, "sessions")[name]
		if !ok {
			t.Fatalf("sessions.%s missing after migration", name)
		}
		if got != "TEXT" {
			t.Errorf("sessions.%s type = %q, want TEXT", name, got)
		}
		var notnull int
		var dflt *string
		if err := db.QueryRow(`SELECT "notnull", dflt_value FROM pragma_table_info('sessions') WHERE name = ?`, name).Scan(&notnull, &dflt); err != nil {
			t.Fatalf("pragma_table_info(sessions.%s): %v", name, err)
		}
		if notnull != 1 {
			t.Errorf("sessions.%s notnull = %d, want 1", name, notnull)
		}
		if dflt == nil || *dflt != "''" {
			t.Errorf("sessions.%s default = %v, want ''", name, dflt)
		}
	}
}

func TestSessionEffectiveRuntimeMigrationDown(t *testing.T) {
	migrations := os.DirFS(migrationsDir())
	db, err := OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := dbtest.RunUpTo(db, migrations, sessionEffectiveRuntimeVersion); err != nil {
		t.Fatalf("run up to %d: %v", sessionEffectiveRuntimeVersion, err)
	}
	if _, ok := tableColumns(t, db, "sessions")["effective_effort"]; !ok {
		t.Fatal("effective_effort missing before down")
	}

	if err := dbtest.RunDownTo(db, migrations, preSessionEffectiveRuntimeVersion); err != nil {
		t.Fatalf("run down: %v", err)
	}
	cols := tableColumns(t, db, "sessions")
	if _, ok := cols["effective_model"]; ok {
		t.Fatal("effective_model still present after down")
	}
	if _, ok := cols["effective_effort"]; ok {
		t.Fatal("effective_effort still present after down")
	}

	if err := dbtest.RunUpTo(db, migrations, sessionEffectiveRuntimeVersion); err != nil {
		t.Fatalf("re-run up after down: %v", err)
	}
}

func TestSessionEffectiveRuntimeMigrationLeavesExistingRowsUnknown(t *testing.T) {
	migrations := os.DirFS(migrationsDir())
	db, err := OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := dbtest.RunUpTo(db, migrations, preSessionEffectiveRuntimeVersion); err != nil {
		t.Fatalf("run up to %d: %v", preSessionEffectiveRuntimeVersion, err)
	}
	if _, err := db.Exec(`INSERT INTO repos (id, display_name, local_path, origin_url, worktree_base_dir) VALUES ('r1', 'repo', '/tmp/repo', 'https://example.test/repo.git', '/tmp/worktrees')`); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (id, repo_id, title, worktree_path, branch_name, base_branch) VALUES ('s1', 'r1', 'legacy', '/tmp/worktrees/s1', 'branch', 'main')`); err != nil {
		t.Fatalf("insert legacy session: %v", err)
	}

	if err := dbtest.RunUpTo(db, migrations, sessionEffectiveRuntimeVersion); err != nil {
		t.Fatalf("run up to %d: %v", sessionEffectiveRuntimeVersion, err)
	}
	var model, effort string
	if err := db.QueryRow(`SELECT effective_model, effective_effort FROM sessions WHERE id = 's1'`).Scan(&model, &effort); err != nil {
		t.Fatalf("read migrated session: %v", err)
	}
	if model != "" || effort != "" {
		t.Fatalf("legacy effective runtime = model %q effort %q, want unknown empty strings", model, effort)
	}
}
