package plugin

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/recurser/bossalib/migrate"
	"github.com/recurser/bossd/internal/db"
)

// openTestDB returns a freshly-migrated in-memory SQLite database for tests
// in package plugin. Tests in package plugin_test should call
// pluginharness.OpenTestDB instead; this duplicate exists because package
// plugin can't import pluginharness without an import cycle.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := db.OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := migrate.Run(sqlDB, os.DirFS(migrationsDir())); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return sqlDB
}

// migrationsDir resolves services/bossd/migrations relative to this file.
// This file lives at services/bossd/internal/plugin/, so the migrations
// directory is three levels up. Mirrors pluginharness.MigrationsDir, which
// package plugin can't call directly (import cycle).
func migrationsDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
}
