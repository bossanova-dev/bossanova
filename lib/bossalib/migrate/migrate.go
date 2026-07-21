// Package migrate provides a shared migration runner using goose.
// Both the daemon (bossd) and orchestrator (bosso) use this package
// with their own embed.FS instances containing SQL migration files.
package migrate

import (
	"database/sql"
	"io/fs"

	"github.com/pressly/goose/v3"
)

// Run executes all pending migrations from the given filesystem against the database.
// The migrations FS should contain .sql files at the root level (not nested).
// Uses goose in timestamp mode (YYYYMMDDHHMMSS_description.sql).
func Run(db *sql.DB, migrations fs.FS) error {
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}
	return goose.Up(db, ".")
}

// RunUpTo applies pending migrations up to and including the given version
// (a YYYYMMDDHHMMSS timestamp). Primarily useful in tests that need to seed
// pre-migration-shaped data before applying a specific migration.
func RunUpTo(db *sql.DB, migrations fs.FS, version int64) error {
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}
	return goose.UpTo(db, ".", version)
}

// RunDownTo rolls migrations back down to (but not including) the given version.
// Primarily useful in tests exercising a migration's Down block.
func RunDownTo(db *sql.DB, migrations fs.FS, version int64) error {
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}
	return goose.DownTo(db, ".", version)
}
