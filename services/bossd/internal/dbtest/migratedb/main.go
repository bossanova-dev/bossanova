// Command migratedb runs sqlc vet against a migrated scratch database.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossd/internal/dbtest"
	"github.com/recurser/bossd/migrations"
	_ "modernc.org/sqlite"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: migratedb -- vet-config")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dir, err := os.MkdirTemp("", "sqlc-bossd-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	uri := "file:" + filepath.Join(dir, "bossd.db")
	database, err := sqlutil.Open(uri)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	if err := dbtest.Run(database, migrations.FS); err != nil {
		return fmt.Errorf("migrate SQLite: %w", err)
	}
	// #nosec G702 -- fixed sqlc vet executable and flags; the operator supplies one config filename argument, without shell interpretation.
	// owner=@recurser review-by=2027-01-18 issue=BOS-1731
	cmd := exec.CommandContext(ctx, "./bin/sqlc", "vet", "-f", args[0])
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = append(os.Environ(), "SQLC_VET_DATABASE_URI="+uri)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("SQL query check: %w", err)
	}
	return nil
}
