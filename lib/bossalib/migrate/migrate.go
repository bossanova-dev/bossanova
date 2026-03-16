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
