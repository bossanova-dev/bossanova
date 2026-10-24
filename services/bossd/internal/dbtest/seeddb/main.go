// Command seeddb migrates and seeds an offline bossd database for real-stack tests.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossd/internal/dbtest"
	"github.com/recurser/bossd/migrations"
	_ "modernc.org/sqlite"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("bossd-seeddb", flag.ContinueOnError)
	path := flags.String("db", "", "SQLite database path")
	specPath := flags.String("spec", "", "versioned JSON spec file")
	migrateOnly := flags.Bool("migrate-only", false, "migrate without seeding")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *path == "" || (*migrateOnly == (*specPath != "")) {
		return fmt.Errorf("usage: bossd-seeddb --db <path> (--migrate-only | --spec <json-file>)")
	}
	var spec Spec
	if !*migrateOnly {
		file, err := os.Open(*specPath)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		decoder := json.NewDecoder(file)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&spec); err != nil {
			return fmt.Errorf("decode seed spec: %w", err)
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return fmt.Errorf("seed spec must contain exactly one JSON object")
		}
	}
	database, err := sqlutil.Open(*path)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	if err := dbtest.Run(database, migrations.FS); err != nil {
		return fmt.Errorf("migrate seed database: %w", err)
	}
	if *migrateOnly {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result, err := Seed(ctx, database, spec)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}
