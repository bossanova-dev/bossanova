package db

import (
	"context"
	"os"
	"testing"

	"github.com/recurser/bossalib/migrate"
)

// cronZeroOutputVersion is the goose timestamp of the BOS-563 migration that
// adds cron_jobs.is_zero_output.
const cronZeroOutputVersion int64 = 20260726000000

// preCronZeroOutputVersion is the migration immediately before BOS-563; rolling
// down to it exercises the is_zero_output Down step.
const preCronZeroOutputVersion int64 = 20260725000001

// TestCronZeroOutputMigrationSchema asserts the BOS-563 migration is applied at
// its expected goose version and adds is_zero_output as an INTEGER column with
// the NOT NULL DEFAULT 0 that makes it opt-in.
func TestCronZeroOutputMigrationSchema(t *testing.T) {
	db := setupTestDB(t)

	var applied int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM goose_db_version WHERE version_id = ?", cronZeroOutputVersion,
	).Scan(&applied); err != nil {
		t.Fatalf("query goose version: %v", err)
	}
	if applied == 0 {
		t.Fatalf("migration version %d not applied", cronZeroOutputVersion)
	}

	cols := tableColumns(t, db, "cron_jobs")
	got, ok := cols["is_zero_output"]
	if !ok {
		t.Fatalf("cron_jobs.is_zero_output missing after migration (columns: %v)", cols)
	}
	if got != "INTEGER" {
		t.Errorf("cron_jobs.is_zero_output type = %q, want %q", got, "INTEGER")
	}

	// PRAGMA table_info carries notnull + dflt_value, which is what makes the
	// column safe to add to an existing table. tableColumns drops them, so read
	// them directly here.
	var notnull int
	var dflt *string
	row := db.QueryRow(`SELECT "notnull", dflt_value FROM pragma_table_info('cron_jobs') WHERE name = 'is_zero_output'`)
	if err := row.Scan(&notnull, &dflt); err != nil {
		t.Fatalf("pragma_table_info(cron_jobs): %v", err)
	}
	if notnull != 1 {
		t.Errorf("is_zero_output notnull = %d, want 1", notnull)
	}
	if dflt == nil || *dflt != "0" {
		t.Errorf("is_zero_output default = %v, want \"0\"", dflt)
	}
}

// TestCronZeroOutputMigrationDown asserts the `-- +goose Down` leg really drops
// the column and that the migration re-applies cleanly afterwards, so the leg is
// mechanically exercised rather than only asserted by inspection.
//
// The down/up cycle is capped with RunUpTo(cronZeroOutputVersion) rather than
// RunDownTo + Run: DownTo rolls back everything above its target, so a bare
// re-Run would drag whatever migrations land after BOS-563 through the cycle
// too. Several existing migrations deliberately have an empty Down (SQLite <
// 3.35 had no DROP COLUMN), and goose still clears their goose_db_version row —
// so an uncapped cycle would eventually re-run one of those Ups against state it
// never removed and go red in an unrelated PR. Capping the ceiling keeps this
// test about BOS-563 only.
func TestCronZeroOutputMigrationDown(t *testing.T) {
	migrations := os.DirFS(migrationsDir())
	db, err := OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := migrate.RunUpTo(db, migrations, cronZeroOutputVersion); err != nil {
		t.Fatalf("run up to %d: %v", cronZeroOutputVersion, err)
	}
	if _, ok := tableColumns(t, db, "cron_jobs")["is_zero_output"]; !ok {
		t.Fatal("is_zero_output missing before down")
	}

	if err := migrate.RunDownTo(db, migrations, preCronZeroOutputVersion); err != nil {
		t.Fatalf("run down: %v", err)
	}
	if _, ok := tableColumns(t, db, "cron_jobs")["is_zero_output"]; ok {
		t.Fatal("is_zero_output still present after down")
	}

	// Re-applying must succeed — a Down that leaves the table in a shape the Up
	// cannot re-enter is not actually reversible.
	if err := migrate.RunUpTo(db, migrations, cronZeroOutputVersion); err != nil {
		t.Fatalf("re-run up after down: %v", err)
	}
	if _, ok := tableColumns(t, db, "cron_jobs")["is_zero_output"]; !ok {
		t.Fatal("is_zero_output missing after re-applying the migration")
	}
}

// TestCronZeroOutputMigrationBackfillsExistingRow proves the stronger form of
// the "pre-migration job reads back false" contract: a cron_jobs row written
// while the column did not exist is backfilled to 0 by the ALTER TABLE and reads
// back IsZeroOutput == false through the store.
func TestCronZeroOutputMigrationBackfillsExistingRow(t *testing.T) {
	migrations := os.DirFS(migrationsDir())
	ctx := context.Background()

	// Migrate only up to BOS-563 so the down/up cycle below cannot drag later
	// migrations through it — see TestCronZeroOutputMigrationDown for why.
	db, err := OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := migrate.RunUpTo(db, migrations, cronZeroOutputVersion); err != nil {
		t.Fatalf("run up to %d: %v", cronZeroOutputVersion, err)
	}
	repo := createTestRepo(t, NewRepoStore(db))

	// Roll back to before BOS-563 so the column genuinely does not exist.
	if err := migrate.RunDownTo(db, migrations, preCronZeroOutputVersion); err != nil {
		t.Fatalf("run down: %v", err)
	}
	if _, ok := tableColumns(t, db, "cron_jobs")["is_zero_output"]; ok {
		t.Fatal("is_zero_output still present after down; the row below would not be pre-migration")
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO cron_jobs (id, repo_id, name, prompt, schedule, is_enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"cron-pre-migration", repo.ID, "Pre-migration job", "noop", "@daily", 1,
		"2026-07-25T00:00:00.000Z", "2026-07-25T00:00:00.000Z",
	); err != nil {
		t.Fatalf("insert pre-migration row: %v", err)
	}

	// Now apply BOS-563 on top of the existing row.
	if err := migrate.RunUpTo(db, migrations, cronZeroOutputVersion); err != nil {
		t.Fatalf("re-run up after down: %v", err)
	}

	got, err := NewCronJobStore(db).Get(ctx, "cron-pre-migration")
	if err != nil {
		t.Fatalf("get pre-migration row: %v", err)
	}
	if got.IsZeroOutput {
		t.Errorf("IsZeroOutput = true, want false (ALTER TABLE ... DEFAULT 0 backfill)")
	}
}
