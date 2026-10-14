package db

import (
	"context"
	"os"
	"testing"

	"github.com/recurser/bossd/internal/dbtest"
)

// repoIsTrustedVersion is the goose timestamp of the migration that adds
// repos.is_trusted (BOS-1562).
const repoIsTrustedVersion int64 = 20261014000000

// preRepoIsTrustedVersion is the migration immediately before it.
const preRepoIsTrustedVersion int64 = 20261013000000

// TestReposIsTrustedMigration applies the is_trusted migration over a database
// that already holds a repo, asserts the column shape, that the pre-existing
// row is backfilled trusted while a row inserted afterwards defaults
// untrusted, then rolls it back and asserts the column is gone while the row
// stays readable.
func TestReposIsTrustedMigration(t *testing.T) {
	migrations := os.DirFS(migrationsDir())
	ctx := context.Background()

	db, err := OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := dbtest.RunUpTo(db, migrations, preRepoIsTrustedVersion); err != nil {
		t.Fatalf("run up to %d: %v", preRepoIsTrustedVersion, err)
	}
	if _, ok := tableColumns(t, db, "repos")["is_trusted"]; ok {
		t.Fatal("is_trusted present before its migration; the row below would not be pre-migration")
	}
	existing := insertPreMigrationRepo(t, db)

	if err := dbtest.RunUpTo(db, migrations, repoIsTrustedVersion); err != nil {
		t.Fatalf("run up to %d: %v", repoIsTrustedVersion, err)
	}

	cols := tableColumns(t, db, "repos")
	if got, ok := cols["is_trusted"]; !ok {
		t.Fatalf("repos.is_trusted missing after migration (columns: %v)", cols)
	} else if got != "INTEGER" {
		t.Errorf("repos.is_trusted type = %q, want INTEGER", got)
	}
	var notnull int
	var dflt *string
	if err := db.QueryRow(
		`SELECT "notnull", dflt_value FROM pragma_table_info('repos') WHERE name = 'is_trusted'`,
	).Scan(&notnull, &dflt); err != nil {
		t.Fatalf("pragma_table_info(repos): %v", err)
	}
	if notnull != 1 {
		t.Errorf("is_trusted notnull = %d, want 1", notnull)
	}
	if dflt == nil || *dflt != "0" {
		t.Errorf("is_trusted default = %v, want \"0\"", dflt)
	}

	// The repo store reads the current repos columns (later migrations add
	// has_keyring_secrets), so finish the chain before reading through it.
	if err := dbtest.Run(db, migrations); err != nil {
		t.Fatalf("run remaining migrations: %v", err)
	}
	store := NewRepoStore(db)
	got, err := store.Get(ctx, existing.ID)
	if err != nil {
		t.Fatalf("get pre-existing repo: %v", err)
	}
	if !got.IsTrusted {
		t.Error("pre-existing repo read back untrusted, want backfilled trusted")
	}
	fresh, err := store.Create(ctx, CreateRepoParams{
		DisplayName:       "new-repo",
		LocalPath:         "/tmp/new-repo",
		OriginURL:         "https://github.com/test/new.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	})
	if err != nil {
		t.Fatalf("create post-migration repo: %v", err)
	}
	if fresh.IsTrusted {
		t.Error("repo registered after the migration is trusted, want untrusted")
	}

	if err := dbtest.RunDownTo(db, migrations, preRepoIsTrustedVersion); err != nil {
		t.Fatalf("run down to %d: %v", preRepoIsTrustedVersion, err)
	}
	if _, ok := tableColumns(t, db, "repos")["is_trusted"]; ok {
		t.Fatal("is_trusted still present after down")
	}
	var name string
	if err := db.QueryRowContext(ctx, `SELECT display_name FROM repos WHERE id = ?`, existing.ID).Scan(&name); err != nil {
		t.Fatalf("row unreadable after down: %v", err)
	}
	if name != "test-repo" {
		t.Errorf("display_name after down = %q, want test-repo", name)
	}
}
