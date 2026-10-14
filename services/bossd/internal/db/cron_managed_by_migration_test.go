package db

import (
	"context"
	"os"
	"testing"

	"github.com/recurser/bossd/internal/dbtest"
)

// cronManagedByVersion is the goose timestamp of the migration that adds
// cron_jobs.managed_by, the config-ownership marker.
const cronManagedByVersion int64 = 20261010000000

// preCronManagedByVersion is the migration immediately before it; rolling down
// to it exercises the managed_by Down step.
const preCronManagedByVersion int64 = 20261008000200

// TestCronJobsManagedByMigration applies the managed_by migration over a
// database that already holds a cron row, asserts the column shape and the
// backfill, then rolls it back and asserts the column is gone while the row
// stays readable.
func TestCronJobsManagedByMigration(t *testing.T) {
	migrations := os.DirFS(migrationsDir())
	ctx := context.Background()

	db, err := OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := dbtest.RunUpTo(db, migrations, preCronManagedByVersion); err != nil {
		t.Fatalf("run up to %d: %v", preCronManagedByVersion, err)
	}
	if _, ok := tableColumns(t, db, "cron_jobs")["managed_by"]; ok {
		t.Fatal("managed_by present before its migration; the row below would not be pre-migration")
	}
	repo := insertPreMigrationRepo(t, db)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO cron_jobs (id, repo_id, name, prompt, schedule, is_enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"cron-pre-managed-by", repo.ID, "Pre-migration job", "noop", "@daily", 1,
		"2026-10-08T00:00:00.000Z", "2026-10-08T00:00:00.000Z",
	); err != nil {
		t.Fatalf("insert pre-migration row: %v", err)
	}

	if err := dbtest.RunUpTo(db, migrations, cronManagedByVersion); err != nil {
		t.Fatalf("run up to %d: %v", cronManagedByVersion, err)
	}

	cols := tableColumns(t, db, "cron_jobs")
	if got, ok := cols["managed_by"]; !ok {
		t.Fatalf("cron_jobs.managed_by missing after migration (columns: %v)", cols)
	} else if got != "TEXT" {
		t.Errorf("cron_jobs.managed_by type = %q, want %q", got, "TEXT")
	}

	var notnull int
	var dflt *string
	if err := db.QueryRow(
		`SELECT "notnull", dflt_value FROM pragma_table_info('cron_jobs') WHERE name = 'managed_by'`,
	).Scan(&notnull, &dflt); err != nil {
		t.Fatalf("pragma_table_info(cron_jobs): %v", err)
	}
	if notnull != 1 {
		t.Errorf("managed_by notnull = %d, want 1", notnull)
	}
	if dflt == nil || *dflt != "''" {
		t.Errorf("managed_by default = %v, want \"''\"", dflt)
	}

	var raw string
	if err := db.QueryRowContext(ctx,
		`SELECT managed_by FROM cron_jobs WHERE id = ?`, "cron-pre-managed-by",
	).Scan(&raw); err != nil {
		t.Fatalf("read backfilled managed_by: %v", err)
	}
	if raw != "" {
		t.Errorf("backfilled managed_by = %q, want empty", raw)
	}

	if err := dbtest.RunDownTo(db, migrations, preCronManagedByVersion); err != nil {
		t.Fatalf("run down to %d: %v", preCronManagedByVersion, err)
	}
	if _, ok := tableColumns(t, db, "cron_jobs")["managed_by"]; ok {
		t.Fatal("managed_by still present after down")
	}
	var name string
	if err := db.QueryRowContext(ctx,
		`SELECT name FROM cron_jobs WHERE id = ?`, "cron-pre-managed-by",
	).Scan(&name); err != nil {
		t.Fatalf("row unreadable after down: %v", err)
	}
	if name != "Pre-migration job" {
		t.Errorf("name after down = %q, want %q", name, "Pre-migration job")
	}
}
