package db

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/recurser/bossd/internal/domain"

	"github.com/recurser/bossd/internal/dbtest"
)

// agentAccountsRenameVersion is the goose timestamp of the migration that
// renames the accounts table to agent_accounts (BOS-1784).
const agentAccountsRenameVersion int64 = 20261020120000

// preAgentAccountsRenameVersion is the migration immediately before it.
const preAgentAccountsRenameVersion int64 = 20261018000000

// agentAccountRow is the subset of agent account columns the rename must carry
// across unchanged.
type agentAccountRow struct {
	provider, label, status, health, tier, allowedModels, cooldownUntil string
	priority                                                            int
}

const agentAccountRowColumns = `provider, label, status, health, tier, allowed_models, cooldown_until, priority`

// TestAgentAccountsRenameMigration applies the rename over a database that
// already holds an agent account, asserts the row survives byte-for-byte under
// the new table name with the renamed index in place and the old table gone,
// reads it back through the store, then rolls back and asserts the reverse.
func TestAgentAccountsRenameMigration(t *testing.T) {
	migrations := os.DirFS(migrationsDir())
	ctx := context.Background()

	db, err := OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := dbtest.RunUpTo(db, migrations, preAgentAccountsRenameVersion); err != nil {
		t.Fatalf("run up to %d: %v", preAgentAccountsRenameVersion, err)
	}
	assertSchemaObject(t, db, "table", "accounts", true)
	assertSchemaObject(t, db, "table", "agent_accounts", false)

	const id = "acct-pre-rename"
	want := agentAccountRow{
		provider:      string(domain.AgentAccountProviderCodex),
		label:         "work",
		status:        string(domain.AgentAccountStatusDisabled),
		health:        string(domain.AgentAccountHealthFailed),
		tier:          "pro",
		allowedModels: `["gpt-5"]`,
		cooldownUntil: "2026-10-20T12:00:00Z",
		priority:      7,
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO accounts (id, `+agentAccountRowColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, want.provider, want.label, want.status, want.health, want.tier, want.allowedModels, want.cooldownUntil, want.priority,
	); err != nil {
		t.Fatalf("insert pre-rename account: %v", err)
	}

	if err := dbtest.RunUpTo(db, migrations, agentAccountsRenameVersion); err != nil {
		t.Fatalf("run up to %d: %v", agentAccountsRenameVersion, err)
	}
	assertSchemaObject(t, db, "table", "accounts", false)
	assertSchemaObject(t, db, "table", "agent_accounts", true)
	assertSchemaObject(t, db, "index", "idx_accounts_provider_priority", false)
	assertIndexOn(t, db, "idx_agent_accounts_provider_priority", "agent_accounts")
	if got := readAgentAccountRow(t, db, "agent_accounts", id); got != want {
		t.Errorf("row after rename = %+v, want %+v", got, want)
	}

	// The store reads the current agent account columns, so finish the chain
	// before reading through it.
	if err := dbtest.Run(db, migrations); err != nil {
		t.Fatalf("run remaining migrations: %v", err)
	}
	acct, err := NewAgentAccountStore(db).Get(ctx, id)
	if err != nil {
		t.Fatalf("store get after rename: %v", err)
	}
	if acct.Label != want.label || acct.Priority != want.priority || string(acct.Status) != want.status {
		t.Errorf("store read = label %q priority %d status %q, want %q %d %q",
			acct.Label, acct.Priority, acct.Status, want.label, want.priority, want.status)
	}

	if err := dbtest.RunDownTo(db, migrations, preAgentAccountsRenameVersion); err != nil {
		t.Fatalf("run down to %d: %v", preAgentAccountsRenameVersion, err)
	}
	assertSchemaObject(t, db, "table", "agent_accounts", false)
	assertSchemaObject(t, db, "table", "accounts", true)
	assertSchemaObject(t, db, "index", "idx_agent_accounts_provider_priority", false)
	assertIndexOn(t, db, "idx_accounts_provider_priority", "accounts")
	if got := readAgentAccountRow(t, db, "accounts", id); got != want {
		t.Errorf("row after rollback = %+v, want %+v", got, want)
	}
}

// assertSchemaObject asserts whether sqlite_master holds an object of the given
// type and name.
func assertSchemaObject(t *testing.T, db *sql.DB, kind, name string, wantPresent bool) {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND name = ?`, kind, name).Scan(&n); err != nil {
		t.Fatalf("sqlite_master %s %s: %v", kind, name, err)
	}
	if got := n == 1; got != wantPresent {
		t.Errorf("%s %s present = %v, want %v", kind, name, got, wantPresent)
	}
}

// assertIndexOn asserts the named index exists and covers (provider, priority)
// on table.
func assertIndexOn(t *testing.T, db *sql.DB, index, table string) {
	t.Helper()
	var onTable string
	if err := db.QueryRow(`SELECT tbl_name FROM sqlite_master WHERE type = 'index' AND name = ?`, index).Scan(&onTable); err != nil {
		t.Fatalf("index %s: %v", index, err)
	}
	if onTable != table {
		t.Errorf("index %s is on %q, want %q", index, onTable, table)
	}
	rows, err := db.Query(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, index)
	if err != nil {
		t.Fatalf("pragma_index_info(%s): %v", index, err)
	}
	defer func() { _ = rows.Close() }()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan index column: %v", err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("index columns: %v", err)
	}
	if len(cols) != 2 || cols[0] != "provider" || cols[1] != "priority" {
		t.Errorf("index %s columns = %v, want [provider priority]", index, cols)
	}
}

// readAgentAccountRow reads one account row's carried columns from table.
func readAgentAccountRow(t *testing.T, db *sql.DB, table, id string) agentAccountRow {
	t.Helper()
	var r agentAccountRow
	if err := db.QueryRow(`SELECT `+agentAccountRowColumns+` FROM `+table+` WHERE id = ?`, id).Scan(
		&r.provider, &r.label, &r.status, &r.health, &r.tier, &r.allowedModels, &r.cooldownUntil, &r.priority,
	); err != nil {
		t.Fatalf("read %s row %s: %v", table, id, err)
	}
	return r
}
