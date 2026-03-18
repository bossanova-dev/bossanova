// Package db provides SQLite database initialization for the orchestrator.
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/recurser/bossalib/sqlutil"
	_ "modernc.org/sqlite"
)

// DefaultDBPath returns the default database path for the orchestrator.
// If BOSSO_DB_PATH is set, it uses that path (creating parent dirs as needed).
// Otherwise falls back to ~/Library/Application Support/bossanova/bosso.db.
func DefaultDBPath() (string, error) {
	if p := os.Getenv("BOSSO_DB_PATH"); p != "" {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return "", fmt.Errorf("create data dir: %w", err)
		}
		return p, nil
	}
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
	return sqlutil.Open(path)
}

// OpenInMemory opens an in-memory SQLite database with WAL mode and
// foreign keys enabled. Useful for testing.
func OpenInMemory() (*sql.DB, error) {
	return sqlutil.OpenInMemory()
}
