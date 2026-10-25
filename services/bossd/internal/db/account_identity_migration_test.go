package db

import (
	"context"
	"os"
	"testing"

	"github.com/recurser/bossd/internal/dbtest"
)

func TestAccountIdentityMigrationDefaultsAndRollback(t *testing.T) {
	database, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	migrations := os.DirFS(migrationsDir())
	const previous int64 = 20261020140000
	if err := dbtest.RunUpTo(database, migrations, previous); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = database.ExecContext(ctx, `INSERT INTO agent_accounts (id,provider,label,status,priority,health,created_at,updated_at) VALUES ('existing','codex','existing','active',0,'ok','2026-10-25T00:00:00Z','2026-10-25T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := dbtest.Run(database, migrations); err != nil {
		t.Fatal(err)
	}
	var kind, digest string
	if err := database.QueryRowContext(ctx, `SELECT identity_kind,identity_digest FROM agent_accounts WHERE id='existing'`).Scan(&kind, &digest); err != nil {
		t.Fatal(err)
	}
	if kind != "" || digest != "" {
		t.Fatalf("defaults = %q, %q", kind, digest)
	}
	if err := dbtest.RunDownTo(database, migrations, previous); err != nil {
		t.Fatal(err)
	}
	var label string
	if err := database.QueryRowContext(ctx, `SELECT label FROM agent_accounts WHERE id='existing'`).Scan(&label); err != nil {
		t.Fatal(err)
	}
	if label != "existing" {
		t.Fatal("rollback lost account")
	}
}
