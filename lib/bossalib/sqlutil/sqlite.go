// Package sqlutil provides shared SQLite database utilities used by both
// the daemon (bossd) and orchestrator (bosso). Each service still registers
// its own driver import (modernc.org/sqlite) and defines its own
// DefaultDBPath function.
package sqlutil

import (
	"database/sql"
	"fmt"
)

// Open opens (or creates) a SQLite database at the given path with
// WAL mode and foreign keys enabled.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Enable WAL mode for better concurrent read performance.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enable WAL mode: %w", err)
	}

	// Enable foreign key enforcement.
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	// Single connection for SQLite — avoids locking issues.
	db.SetMaxOpenConns(1)

	return db, nil
}

// OpenInMemory opens an in-memory SQLite database with WAL mode and
// foreign keys enabled. Useful for testing.
func OpenInMemory() (*sql.DB, error) {
	return Open(":memory:")
}
