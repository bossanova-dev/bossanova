// Package db provides SQLite database initialization for the daemon.
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/sqlutil"
	_ "modernc.org/sqlite"
)

// DefaultDBPath returns the default database path for the daemon.
// On macOS: ~/Library/Application Support/bossanova/bossd.db
// On Linux: ~/.config/bossanova/bossd.db
func DefaultDBPath() (string, error) {
	dir, err := config.DefaultAppDataDir()
	if err != nil {
		return "", fmt.Errorf("resolve app data dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}
	return filepath.Join(dir, "bossd.db"), nil
}

// DefaultDBPathForSettings returns the daemon database path for loaded settings.
// app_data_dir moves the database into that profile. When unset, existing
// platform defaults are preserved.
func DefaultDBPathForSettings(settings config.Settings) (string, error) {
	if dir, ok, err := config.ConfiguredAppDataDir(settings); err != nil {
		return "", err
	} else if ok {
		return filepath.Join(dir, "bossd.db"), nil
	}
	return DefaultDBPath()
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
