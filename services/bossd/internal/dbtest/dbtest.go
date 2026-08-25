// Package dbtest builds migrated in-memory SQLite databases for bossd tests.
//
// Replaying all of bossd's goose migrations costs roughly a second per database
// under `-race`, because the pure-Go SQLite driver is instrumented by the race
// detector's checkptr passes and several migrations rebuild tables via ALTER
// TABLE ... RENAME, which re-parses the entire accumulated schema each time.
// With hundreds of tests each opening their own database that dominated the
// package wall clock (see docs/plans/BOS-1022-bossd-race-test-tax.md).
//
// So the migrations run exactly once per test binary. The resulting schema is
// captured as a script -- the CREATE statements plus goose's own bookkeeping
// rows -- and every subsequent database is built by executing that script.
// Each caller still gets its own independent database; only the derivation of
// the schema is shared.
//
// Tests that assert on migration behaviour itself must not use the captured
// script, since it is the output of the very thing under test. They use
// NewMigrated, or drive Run/RunUpTo/RunDownTo directly.
//
// This package deliberately does not import internal/db: db's own tests are in
// package db, so importing it here would be an import cycle. It opens
// databases through sqlutil, which is exactly what db.OpenInMemory does.
package dbtest

import (
	"database/sql"
	"fmt"
	"io/fs"
	"sync"
	"testing"

	"github.com/recurser/bossalib/migrate"
	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossd/migrations"

	// sqlutil leaves the driver import to its consumers, so a package that
	// opens databases has to register it.
	_ "modernc.org/sqlite"
)

// migrateMu serialises every goose entry point.
//
// migrate.Run, RunUpTo and RunDownTo all call goose.SetBaseFS and
// goose.SetDialect, which write package-level globals in goose. Two tests
// migrating concurrently therefore race on those globals even though their
// databases are unrelated. Every path in this package that reaches goose holds
// this mutex, so callers get the guarantee by construction rather than by
// remembering to lock at each call site.
var migrateMu sync.Mutex

// Run applies all pending migrations from fsys to db, serialised against every
// other goose call in the test binary.
func Run(db *sql.DB, fsys fs.FS) error {
	migrateMu.Lock()
	defer migrateMu.Unlock()
	return migrate.Run(db, fsys)
}

// RunUpTo applies pending migrations from fsys up to and including version,
// serialised against every other goose call in the test binary.
func RunUpTo(db *sql.DB, fsys fs.FS, version int64) error {
	migrateMu.Lock()
	defer migrateMu.Unlock()
	return migrate.RunUpTo(db, fsys, version)
}

// RunDownTo rolls db back down to (but not including) version, serialised
// against every other goose call in the test binary.
func RunDownTo(db *sql.DB, fsys fs.FS, version int64) error {
	migrateMu.Lock()
	defer migrateMu.Unlock()
	return migrate.RunDownTo(db, fsys, version)
}

// gooseRow is one row of goose's bookkeeping table. Several migration
// behaviour tests query goose_db_version to assert that a given migration was
// applied, so a database built from the captured script has to carry the same
// rows a real replay would leave behind.
type gooseRow struct {
	id        int64
	versionID int64
	isApplied bool
	tstamp    string
}

// schemaScript is everything needed to reconstruct a fully migrated database
// without running the migrations again.
type schemaScript struct {
	// ddl holds the CREATE statements of the final schema, in sqlite_master
	// rowid order. That order is creation order, so a table always precedes
	// the indexes, views and triggers that depend on it.
	ddl []string
	// goose holds the goose_db_version rows, in id order.
	goose []gooseRow
}

// loadSchemaScript captures the schema once per test binary. sync.OnceValues
// both memoises the result and makes concurrent first calls safe, so tests
// calling New from t.Parallel bodies do not each trigger a replay.
var loadSchemaScript = sync.OnceValues(func() (*schemaScript, error) {
	return captureSchemaScript(migrations.FS)
})

// captureSchemaScript replays the migrations in fsys against a throwaway
// database and reads the resulting schema back out of it. It takes the
// filesystem rather than reaching for migrations.FS directly so that its
// refusals are reachable from a test.
func captureSchemaScript(fsys fs.FS) (*schemaScript, error) {
	db, err := sqlutil.OpenInMemory()
	if err != nil {
		return nil, fmt.Errorf("open capture database: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := Run(db, fsys); err != nil {
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	script := &schemaScript{}

	// sql IS NOT NULL skips the implicit indexes SQLite creates for UNIQUE and
	// PRIMARY KEY constraints; those come back automatically when the table
	// DDL is replayed. The sqlite_% filter skips SQLite's own internal tables,
	// notably sqlite_sequence, which AUTOINCREMENT table DDL recreates.
	//
	// Ordering matters because the replay is executed in order and an index,
	// view or trigger cannot be created before the table it names. rowid alone
	// is creation order, which is only equivalent to dependency order while no
	// migration ever drops and recreates a table — the day one does, its new
	// table DDL lands after an index that references it. Sorting tables first
	// and keeping rowid as the tiebreaker inside each group survives that.
	rows, err := db.Query(`SELECT sql FROM sqlite_master WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' ORDER BY CASE type WHEN 'table' THEN 0 ELSE 1 END, rowid`)
	if err != nil {
		return nil, fmt.Errorf("read schema: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var stmt string
		if err := rows.Scan(&stmt); err != nil {
			return nil, fmt.Errorf("scan schema statement: %w", err)
		}
		script.ddl = append(script.ddl, stmt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema: %w", err)
	}
	// goose creates goose_db_version itself, so a non-empty ddl slice is not
	// evidence that any migration ran. Require an application table.
	var appTables int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name != 'goose_db_version'`,
	).Scan(&appTables); err != nil {
		return nil, fmt.Errorf("count application tables: %w", err)
	}
	if appTables == 0 {
		return nil, fmt.Errorf("captured schema has no application tables: the migrations filesystem applied nothing")
	}

	gooseRows, err := db.Query(`SELECT id, version_id, is_applied, tstamp FROM goose_db_version ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read goose_db_version: %w", err)
	}
	defer func() { _ = gooseRows.Close() }()
	for gooseRows.Next() {
		var row gooseRow
		if err := gooseRows.Scan(&row.id, &row.versionID, &row.isApplied, &row.tstamp); err != nil {
			return nil, fmt.Errorf("scan goose_db_version row: %w", err)
		}
		script.goose = append(script.goose, row)
	}
	if err := gooseRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate goose_db_version: %w", err)
	}
	if len(script.goose) == 0 {
		return nil, fmt.Errorf("captured schema has no goose_db_version rows")
	}

	return script, nil
}

// apply executes the captured script against a fresh database.
func (s *schemaScript) apply(db *sql.DB) error {
	for i, stmt := range s.ddl {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("apply schema statement %d (%q): %w", i, stmt, err)
		}
	}
	for _, row := range s.goose {
		if _, err := db.Exec(
			`INSERT INTO goose_db_version (id, version_id, is_applied, tstamp) VALUES (?, ?, ?, ?)`,
			row.id, row.versionID, row.isApplied, row.tstamp,
		); err != nil {
			return fmt.Errorf("apply goose_db_version row %d: %w", row.versionID, err)
		}
	}
	return nil
}

// New returns a migrated in-memory database, built from the schema captured
// once per test binary. The database is independent of every other database
// New has returned, and is closed when the test finishes.
//
// Use this everywhere a test just needs bossd's schema. Tests asserting on the
// migrations themselves want NewMigrated.
func New(t testing.TB) *sql.DB {
	t.Helper()

	script, err := loadSchemaScript()
	if err != nil {
		t.Fatalf("capture schema: %v", err)
	}

	db, err := sqlutil.OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := script.apply(db); err != nil {
		t.Fatalf("build schema: %v", err)
	}
	return db
}

// Apply builds bossd's schema in db, which the caller owns.
//
// New covers the common case of wanting a database; this is for tests that
// need one New cannot provide -- a file-backed database exercising WAL or
// cross-connection behaviour, say -- but that still only want the schema, not
// a migration replay.
func Apply(t testing.TB, db *sql.DB) {
	t.Helper()

	script, err := loadSchemaScript()
	if err != nil {
		t.Fatalf("capture schema: %v", err)
	}
	if err := script.apply(db); err != nil {
		t.Fatalf("build schema: %v", err)
	}
}

// NewMigrated returns an in-memory database with the real migrations replayed
// against it, bypassing the captured script entirely.
//
// This is the slow path -- around a second per call under -race -- so it is
// for tests whose subject is the migrations. Everything else wants New.
func NewMigrated(t testing.TB) *sql.DB {
	t.Helper()

	db, err := sqlutil.OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := Run(db, migrations.FS); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return db
}

// NewEmpty returns an unmigrated in-memory database, closed when the test
// finishes. Tests that drive RunUpTo/RunDownTo themselves start here.
func NewEmpty(t testing.TB) *sql.DB {
	t.Helper()

	db, err := sqlutil.OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
