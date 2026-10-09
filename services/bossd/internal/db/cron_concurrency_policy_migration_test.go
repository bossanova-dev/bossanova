package db

import (
	"context"
	"os"
	"testing"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/dbtest"
)

// cronConcurrencyPolicyVersion is the goose timestamp of the BOS-1441 migration
// that adds cron_jobs.concurrency_policy.
const cronConcurrencyPolicyVersion int64 = 20261007000000

// preCronConcurrencyPolicyVersion is the migration immediately before BOS-1441;
// rolling down to it exercises the concurrency_policy Down step.
const preCronConcurrencyPolicyVersion int64 = 20260912000000

// TestCronConcurrencyPolicyMigrationSchema asserts the BOS-1441 migration is
// applied at its expected goose version and adds concurrency_policy as a TEXT
// column with the NOT NULL DEFAULT 'skip' that keeps existing jobs unaffected.
func TestCronConcurrencyPolicyMigrationSchema(t *testing.T) {
	db := setupTestDB(t)

	var applied int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM goose_db_version WHERE version_id = ?", cronConcurrencyPolicyVersion,
	).Scan(&applied); err != nil {
		t.Fatalf("query goose version: %v", err)
	}
	if applied == 0 {
		t.Fatalf("migration version %d not applied", cronConcurrencyPolicyVersion)
	}

	cols := tableColumns(t, db, "cron_jobs")
	got, ok := cols["concurrency_policy"]
	if !ok {
		t.Fatalf("cron_jobs.concurrency_policy missing after migration (columns: %v)", cols)
	}
	if got != "TEXT" {
		t.Errorf("cron_jobs.concurrency_policy type = %q, want %q", got, "TEXT")
	}

	var notnull int
	var dflt *string
	row := db.QueryRow(`SELECT "notnull", dflt_value FROM pragma_table_info('cron_jobs') WHERE name = 'concurrency_policy'`)
	if err := row.Scan(&notnull, &dflt); err != nil {
		t.Fatalf("pragma_table_info(cron_jobs): %v", err)
	}
	if notnull != 1 {
		t.Errorf("concurrency_policy notnull = %d, want 1", notnull)
	}
	if dflt == nil || *dflt != "'skip'" {
		t.Errorf("concurrency_policy default = %v, want \"'skip'\"", dflt)
	}
}

// TestCronConcurrencyPolicyMigrationRejectsOutOfSetValue proves the column CHECK
// holds: a value outside skip / cancel_in_progress / allow_concurrent cannot be
// stored, whether by insert or update. UNSPECIFIED leaking from the proto zero
// value is the case this guards.
func TestCronConcurrencyPolicyMigrationRejectsOutOfSetValue(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(db))

	for _, bad := range []string{"", "unspecified", "SKIP", "queue"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO cron_jobs (id, repo_id, name, prompt, schedule, is_enabled, concurrency_policy, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"cron-bad-"+bad, repo.ID, "Bad policy "+bad, "noop", "@daily", 1, bad,
			"2026-10-07T00:00:00.000Z", "2026-10-07T00:00:00.000Z",
		); err == nil {
			t.Errorf("insert concurrency_policy=%q succeeded, want CHECK constraint failure", bad)
		}
	}

	job, err := NewCronJobStore(db).Create(ctx, CreateCronJobParams{
		RepoID: repo.ID, Name: "check-update", Prompt: "noop", Schedule: "@daily", IsEnabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE cron_jobs SET concurrency_policy = 'bogus' WHERE id = ?`, job.ID,
	); err == nil {
		t.Error("update concurrency_policy='bogus' succeeded, want CHECK constraint failure")
	}

	for _, good := range []string{"skip", "cancel_in_progress", "allow_concurrent"} {
		if _, err := db.ExecContext(ctx,
			`UPDATE cron_jobs SET concurrency_policy = ? WHERE id = ?`, good, job.ID,
		); err != nil {
			t.Errorf("update concurrency_policy=%q: %v, want success", good, err)
		}
	}
}

// TestCronConcurrencyPolicyMigrationDown asserts the `-- +goose Down` leg really
// drops the column and that the migration re-applies cleanly afterwards. The
// cycle is capped with RunUpTo for the reason TestCronZeroOutputMigrationDown
// documents: an uncapped re-Run drags later migrations through the cycle.
func TestCronConcurrencyPolicyMigrationDown(t *testing.T) {
	migrations := os.DirFS(migrationsDir())
	db, err := OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := dbtest.RunUpTo(db, migrations, cronConcurrencyPolicyVersion); err != nil {
		t.Fatalf("run up to %d: %v", cronConcurrencyPolicyVersion, err)
	}
	if _, ok := tableColumns(t, db, "cron_jobs")["concurrency_policy"]; !ok {
		t.Fatal("concurrency_policy missing before down")
	}

	if err := dbtest.RunDownTo(db, migrations, preCronConcurrencyPolicyVersion); err != nil {
		t.Fatalf("run down: %v", err)
	}
	if _, ok := tableColumns(t, db, "cron_jobs")["concurrency_policy"]; ok {
		t.Fatal("concurrency_policy still present after down")
	}

	if err := dbtest.RunUpTo(db, migrations, cronConcurrencyPolicyVersion); err != nil {
		t.Fatalf("re-run up after down: %v", err)
	}
	if _, ok := tableColumns(t, db, "cron_jobs")["concurrency_policy"]; !ok {
		t.Fatal("concurrency_policy missing after re-applying the migration")
	}
}

// TestCronConcurrencyPolicyMigrationBackfillsExistingRow proves a cron_jobs row
// written while the column did not exist is backfilled to 'skip' by the ALTER
// TABLE and reads back CronJobConcurrencyPolicySkip through the store.
func TestCronConcurrencyPolicyMigrationBackfillsExistingRow(t *testing.T) {
	migrations := os.DirFS(migrationsDir())
	ctx := context.Background()

	db, err := OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := dbtest.RunUpTo(db, migrations, cronConcurrencyPolicyVersion); err != nil {
		t.Fatalf("run up to %d: %v", cronConcurrencyPolicyVersion, err)
	}
	repo := createTestRepo(t, NewRepoStore(db))

	if err := dbtest.RunDownTo(db, migrations, preCronConcurrencyPolicyVersion); err != nil {
		t.Fatalf("run down: %v", err)
	}
	if _, ok := tableColumns(t, db, "cron_jobs")["concurrency_policy"]; ok {
		t.Fatal("concurrency_policy still present after down; the row below would not be pre-migration")
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO cron_jobs (id, repo_id, name, prompt, schedule, is_enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"cron-pre-concurrency", repo.ID, "Pre-migration job", "noop", "@daily", 1,
		"2026-09-12T00:00:00.000Z", "2026-09-12T00:00:00.000Z",
	); err != nil {
		t.Fatalf("insert pre-migration row: %v", err)
	}

	if err := dbtest.RunUpTo(db, migrations, cronConcurrencyPolicyVersion); err != nil {
		t.Fatalf("re-run up after down: %v", err)
	}
	if err := dbtest.Run(db, migrations); err != nil {
		t.Fatalf("run remaining migrations after BOS-1441: %v", err)
	}

	var raw string
	if err := db.QueryRowContext(ctx,
		`SELECT concurrency_policy FROM cron_jobs WHERE id = ?`, "cron-pre-concurrency",
	).Scan(&raw); err != nil {
		t.Fatalf("read raw column: %v", err)
	}
	if raw != "skip" {
		t.Errorf("stored concurrency_policy = %q, want %q (ALTER TABLE ... DEFAULT 'skip' backfill)", raw, "skip")
	}

	got, err := NewCronJobStore(db).Get(ctx, "cron-pre-concurrency")
	if err != nil {
		t.Fatalf("get pre-migration row: %v", err)
	}
	if got.ConcurrencyPolicy != models.CronJobConcurrencyPolicySkip {
		t.Errorf("ConcurrencyPolicy = %q, want %q", got.ConcurrencyPolicy, models.CronJobConcurrencyPolicySkip)
	}
}
