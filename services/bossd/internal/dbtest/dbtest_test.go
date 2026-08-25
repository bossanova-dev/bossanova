package dbtest

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/recurser/bossd/migrations"
)

// dumpSchema renders everything SQLite knows about a database's structure:
// tables, indexes, views and triggers, including the implicit indexes SQLite
// creates for UNIQUE and PRIMARY KEY constraints (those have a NULL sql, so
// their presence is the assertion). Ordering by type and name makes the dump
// independent of creation order.
func dumpSchema(t testing.TB, db *sql.DB) string {
	t.Helper()

	rows, err := db.Query(`SELECT type, name, tbl_name, IFNULL(sql, '<implicit>') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var b strings.Builder
	for rows.Next() {
		var typ, name, tblName, stmt string
		if err := rows.Scan(&typ, &name, &tblName, &stmt); err != nil {
			t.Fatalf("scan sqlite_master: %v", err)
		}
		fmt.Fprintf(&b, "%s %s (on %s)\n%s\n---\n", typ, name, tblName, stmt)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite_master: %v", err)
	}
	return b.String()
}

// dumpGooseVersions renders goose's bookkeeping rows. tstamp is deliberately
// excluded: it records the wall clock at which a migration was applied, so two
// real replays disagree on it as readily as a replay and a fixture do.
func dumpGooseVersions(t testing.TB, db *sql.DB) string {
	t.Helper()

	rows, err := db.Query(`SELECT version_id, is_applied FROM goose_db_version ORDER BY version_id`)
	if err != nil {
		t.Fatalf("read goose_db_version: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var b strings.Builder
	for rows.Next() {
		var versionID int64
		var isApplied bool
		if err := rows.Scan(&versionID, &isApplied); err != nil {
			t.Fatalf("scan goose_db_version: %v", err)
		}
		fmt.Fprintf(&b, "%d applied=%t\n", versionID, isApplied)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate goose_db_version: %v", err)
	}
	return b.String()
}

// TestNewMatchesRealMigrations is the guard that makes the whole fixture safe.
//
// The captured schema is only a legitimate stand-in for a migration replay for
// as long as it is indistinguishable from one. This test replays the real
// migrations and compares the result against a fixture database, so a
// migration whose effect the capture fails to reproduce fails here rather than
// silently weakening every test that uses New.
func TestNewMatchesRealMigrations(t *testing.T) {
	t.Parallel()

	fixture := New(t)
	real := NewMigrated(t)

	if got, want := dumpSchema(t, fixture), dumpSchema(t, real); got != want {
		t.Errorf("fixture schema differs from a real migration replay\n--- fixture ---\n%s\n--- real ---\n%s", got, want)
	}
	if got, want := dumpGooseVersions(t, fixture), dumpGooseVersions(t, real); got != want {
		t.Errorf("fixture goose_db_version differs from a real migration replay\n--- fixture ---\n%s\n--- real ---\n%s", got, want)
	}
}

// TestNewAppliesEveryMigration checks the fixture records the same number of
// migrations as there are migration files, so a new .sql file that the capture
// somehow misses is caught even if its schema effect happens to be invisible.
func TestNewAppliesEveryMigration(t *testing.T) {
	t.Parallel()

	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	want := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			want++
		}
	}
	if want == 0 {
		t.Fatal("no migration files found; the embed pattern is broken")
	}

	var got int
	if err := New(t).QueryRow(`SELECT COUNT(*) FROM goose_db_version WHERE is_applied = 1 AND version_id > 0`).Scan(&got); err != nil {
		t.Fatalf("count applied migrations: %v", err)
	}
	if got != want {
		t.Errorf("fixture records %d applied migrations, but there are %d migration files", got, want)
	}
}

// TestNewReturnsIndependentDatabases guards the property that makes the shared
// capture safe to use from parallel tests: only the derivation of the schema is
// shared, never the database itself.
func TestNewReturnsIndependentDatabases(t *testing.T) {
	t.Parallel()

	first := New(t)
	second := New(t)

	if _, err := first.Exec(
		`INSERT INTO repos (id, display_name, local_path, origin_url, worktree_base_dir)
		 VALUES ('r1', 'repo', '/p', 'git@example.com:o/r.git', '/wt')`,
	); err != nil {
		t.Fatalf("insert into first database: %v", err)
	}

	var count int
	if err := second.QueryRow(`SELECT COUNT(*) FROM repos`).Scan(&count); err != nil {
		t.Fatalf("count rows in second database: %v", err)
	}
	if count != 0 {
		t.Errorf("second database sees %d rows written to the first; the databases are not independent", count)
	}
}

// TestNewSupportsAutoincrement checks that replaying the captured DDL restores
// SQLite's implicit sqlite_sequence table, which AUTOINCREMENT columns need and
// which the capture query deliberately filters out of the DDL it stores.
func TestNewSupportsAutoincrement(t *testing.T) {
	t.Parallel()

	db := New(t)

	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE name = 'sqlite_sequence'`).Scan(&name)
	if err != nil {
		t.Fatalf("sqlite_sequence missing from fixture database: %v", err)
	}

	// goose_db_version itself is AUTOINCREMENT, so exercise the sequence
	// through it: a fresh insert must not reuse an id the capture replayed.
	var maxID int64
	if err := db.QueryRow(`SELECT MAX(id) FROM goose_db_version`).Scan(&maxID); err != nil {
		t.Fatalf("read max goose id: %v", err)
	}
	res, err := db.Exec(`INSERT INTO goose_db_version (version_id, is_applied) VALUES (99999999999999, 1)`)
	if err != nil {
		t.Fatalf("insert with autoincrement: %v", err)
	}
	newID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("read inserted id: %v", err)
	}
	if newID <= maxID {
		t.Errorf("autoincrement produced id %d, not greater than the replayed max %d", newID, maxID)
	}
}

// TestNewEnforcesForeignKeys checks the fixture database is opened with the
// same pragmas as a real one -- a fixture that silently dropped foreign key
// enforcement would let broken writes pass in every test that used it.
func TestNewEnforcesForeignKeys(t *testing.T) {
	t.Parallel()

	var enabled int
	if err := New(t).QueryRow(`PRAGMA foreign_keys`).Scan(&enabled); err != nil {
		t.Fatalf("read foreign_keys pragma: %v", err)
	}
	if enabled != 1 {
		t.Error("foreign_keys is disabled in the fixture database")
	}
}

// TestApplyFailsLoudlyOnBrokenStatement checks a corrupted script aborts with a
// diagnostic naming the offending statement, rather than yielding a
// half-built database that fails obscurely much later.
func TestApplyFailsLoudlyOnBrokenStatement(t *testing.T) {
	t.Parallel()

	db := NewEmpty(t)
	script := &schemaScript{ddl: []string{
		`CREATE TABLE ok (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE broken (`,
	}}

	err := script.apply(db)
	if err == nil {
		t.Fatal("apply accepted a malformed statement")
	}
	if !strings.Contains(err.Error(), "CREATE TABLE broken") {
		t.Errorf("error does not name the offending statement: %v", err)
	}
}

// TestApplyFailsLoudlyOnBrokenGooseRow checks the bookkeeping replay is checked
// too, not just the DDL.
func TestApplyFailsLoudlyOnBrokenGooseRow(t *testing.T) {
	t.Parallel()

	db := NewEmpty(t)
	// No goose_db_version table exists, so replaying a row must fail.
	script := &schemaScript{goose: []gooseRow{{id: 1, versionID: 42, isApplied: true}}}

	err := script.apply(db)
	if err == nil {
		t.Fatal("apply accepted a goose row with no table to hold it")
	}
	if !strings.Contains(err.Error(), "42") {
		t.Errorf("error does not name the offending version: %v", err)
	}
}

// TestCaptureRejectsEmptyMigrations checks a migrations filesystem containing
// nothing is refused rather than memoised. goose rejects this one itself; the
// test pins that the refusal reaches the caller instead of being swallowed.
func TestCaptureRejectsEmptyMigrations(t *testing.T) {
	t.Parallel()

	_, err := captureSchemaScript(fstest.MapFS{})
	if err == nil {
		t.Fatal("capture accepted a migrations filesystem containing nothing")
	}
	if !strings.Contains(err.Error(), "run migrations") {
		t.Errorf("error does not explain the refusal: %v", err)
	}
}

// TestCaptureRejectsSchemalessMigrations checks the capture refuses a
// filesystem whose migrations apply cleanly but create no tables.
//
// This is the case goose cannot catch: it reports success, and the database it
// leaves behind still holds goose_db_version, so the captured DDL is non-empty.
// Without the application-table guard that degenerate schema would be memoised
// for the lifetime of the test binary and every test built on it would fail
// somewhere far from the cause.
func TestCaptureRejectsSchemalessMigrations(t *testing.T) {
	t.Parallel()

	noop := fstest.MapFS{
		"20260101000000_noop.sql": &fstest.MapFile{Data: []byte(
			"-- +goose Up\nSELECT 1;\n\n-- +goose Down\nSELECT 1;\n",
		)},
	}

	_, err := captureSchemaScript(noop)
	if err == nil {
		t.Fatal("capture accepted migrations that created no tables")
	}
	if !strings.Contains(err.Error(), "no application tables") {
		t.Errorf("error does not explain the refusal: %v", err)
	}
}

// TestRunUpToStopsAtVersion checks the mutex-guarded wrappers still behave like
// the migrate functions they delegate to.
func TestRunUpToStopsAtVersion(t *testing.T) {
	t.Parallel()

	// The third migration by timestamp. Stopping here proves RunUpTo applied a
	// prefix rather than everything.
	const stopVersion = 20260318170000

	db := NewEmpty(t)
	if err := RunUpTo(db, migrations.FS, stopVersion); err != nil {
		t.Fatalf("run up to %d: %v", stopVersion, err)
	}

	var maxVersion int64
	if err := db.QueryRow(`SELECT MAX(version_id) FROM goose_db_version`).Scan(&maxVersion); err != nil {
		t.Fatalf("read max version: %v", err)
	}
	if maxVersion != stopVersion {
		t.Errorf("RunUpTo stopped at version %d, want %d", maxVersion, stopVersion)
	}

	// And that the rest really are absent, not merely unrecorded.
	var applied int
	if err := db.QueryRow(`SELECT COUNT(*) FROM goose_db_version WHERE version_id > 0`).Scan(&applied); err != nil {
		t.Fatalf("count applied migrations: %v", err)
	}
	if applied != 3 {
		t.Errorf("RunUpTo applied %d migrations, want 3", applied)
	}
}
