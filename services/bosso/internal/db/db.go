// Package db provides SQLite database initialization for the orchestrator.
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// DefaultDBPath returns the default database path for the orchestrator.
// On macOS: ~/Library/Application Support/bossanova/bosso.db
func DefaultDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	dir := filepath.Join(home, "Library", "Application Support", "bossanova")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}
	return filepath.Join(dir, "bosso.db"), nil
}

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
