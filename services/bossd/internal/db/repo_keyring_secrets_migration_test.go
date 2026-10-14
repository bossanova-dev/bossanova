package db

import (
	"context"
	"os"
	"testing"

	"github.com/recurser/bossd/internal/dbtest"
)

// repoKeyringSecretsVersion is the goose timestamp of the migration that adds
// repos.has_keyring_secrets (BOS-1587).
const repoKeyringSecretsVersion int64 = 20261014100000

// preRepoKeyringSecretsVersion is the run-up/roll-back target before it: the
// latest migration this branch shares with main (fix_row_state). main also
// carries 20261014000000_repos_is_trusted, which sorts between the two, so on
// the merged tree the up step applies it alongside this migration and the down
// step rolls both back; neither touches the other's columns.
const preRepoKeyringSecretsVersion int64 = 20261013000000

// TestRepoKeyringSecretsMigration applies the flag migration over a database
// holding a repo with plaintext keys, asserts the column shape and that the
// existing row backfills to 0 ("not moved yet") with its keys untouched, then
// rolls it back.
func TestRepoKeyringSecretsMigration(t *testing.T) {
	migrations := os.DirFS(migrationsDir())
	ctx := context.Background()

	db, err := OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := dbtest.RunUpTo(db, migrations, preRepoKeyringSecretsVersion); err != nil {
		t.Fatalf("run up to %d: %v", preRepoKeyringSecretsVersion, err)
	}
	if _, ok := tableColumns(t, db, "repos")["has_keyring_secrets"]; ok {
		t.Fatal("has_keyring_secrets present before its migration")
	}
	repoID := insertPreMigrationRepo(t, db).ID
	linear, sentry := markerKeys("mig")
	if _, err := db.ExecContext(ctx,
		`UPDATE repos SET linear_api_key = ?, sentry_api_key = ? WHERE id = ?`, linear, sentry, repoID,
	); err != nil {
		t.Fatalf("seed plaintext keys: %v", err)
	}

	if err := dbtest.RunUpTo(db, migrations, repoKeyringSecretsVersion); err != nil {
		t.Fatalf("run up to %d: %v", repoKeyringSecretsVersion, err)
	}
	cols := tableColumns(t, db, "repos")
	if got, ok := cols["has_keyring_secrets"]; !ok {
		t.Fatalf("repos.has_keyring_secrets missing after migration (columns: %v)", cols)
	} else if got != "INTEGER" {
		t.Errorf("repos.has_keyring_secrets type = %q, want INTEGER", got)
	}
	var notnull int
	var dflt *string
	if err := db.QueryRow(
		`SELECT "notnull", dflt_value FROM pragma_table_info('repos') WHERE name = 'has_keyring_secrets'`,
	).Scan(&notnull, &dflt); err != nil {
		t.Fatalf("pragma_table_info(repos): %v", err)
	}
	if notnull != 1 {
		t.Errorf("has_keyring_secrets notnull = %d, want 1", notnull)
	}
	if dflt == nil || *dflt != "0" {
		t.Errorf("has_keyring_secrets default = %v, want 0", dflt)
	}
	gotLinear, gotSentry, flag := rawRepoSecretColumns(t, db, repoID)
	if flag != 0 {
		t.Errorf("backfilled has_keyring_secrets = %d, want 0", flag)
	}
	if gotLinear != linear || gotSentry != sentry {
		t.Error("migration changed the existing plaintext keys")
	}

	if err := dbtest.RunDownTo(db, migrations, preRepoKeyringSecretsVersion); err != nil {
		t.Fatalf("run down to %d: %v", preRepoKeyringSecretsVersion, err)
	}
	if _, ok := tableColumns(t, db, "repos")["has_keyring_secrets"]; ok {
		t.Fatal("has_keyring_secrets still present after down")
	}
}
