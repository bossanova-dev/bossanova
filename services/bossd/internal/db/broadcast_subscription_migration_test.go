package db

import (
	"os"
	"strings"
	"testing"

	"github.com/recurser/bossalib/migrate"
)

// broadcastSubscriptionsVersion is the goose timestamp of the BOS-557 migration.
const broadcastSubscriptionsVersion int64 = 20260726000003

// preBroadcastSubscriptionsVersion is the migration immediately before BOS-557;
// rolling down to it exercises the broadcast_subscriptions Down step.
const preBroadcastSubscriptionsVersion int64 = 20260726000002

// broadcastSubscriptionIndexes are the indexes the BOS-557 migration declares,
// each named for the access path it serves: the evaluator's per-session lookup
// and the reconcile sweep's expiry scan.
var broadcastSubscriptionIndexes = []string{
	"idx_broadcast_subscriptions_owner_state",
	"idx_broadcast_subscriptions_state_expires",
}

// TestBroadcastSubscriptionsMigrationSchema asserts the BOS-557 migration
// creates the table with the expected columns, canonical timestamp types, and
// nullability.
func TestBroadcastSubscriptionsMigrationSchema(t *testing.T) {
	db := setupTestDB(t)

	var applied int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM goose_db_version WHERE version_id = ?", broadcastSubscriptionsVersion,
	).Scan(&applied); err != nil {
		t.Fatalf("query goose version: %v", err)
	}
	if applied == 0 {
		t.Fatalf("migration version %d not applied", broadcastSubscriptionsVersion)
	}

	cols := broadcastTableInfo(t, db, "broadcast_subscriptions")
	if len(cols) == 0 {
		t.Fatal("broadcast_subscriptions table missing after migration")
	}
	assertColumns(t, cols, "broadcast_subscriptions", map[string]broadcastColumn{
		"id":                 {declType: "TEXT", pk: true},
		"owner_session_id":   {declType: "TEXT", notNull: true},
		"origin_chat_id":     {declType: "TEXT"},
		"trigger_event":      {declType: "TEXT", notNull: true},
		"selector":           {declType: "TEXT", notNull: true},
		"message":            {declType: "TEXT", notNull: true},
		"state":              {declType: "TEXT", notNull: true},
		"fired_at":           {declType: "TEXT"},
		"fired_broadcast_id": {declType: "TEXT"},
		"expires_at":         {declType: "TEXT", notNull: true},
		"created_at":         {declType: "TEXT", notNull: true},
		"updated_at":         {declType: "TEXT", notNull: true},
	})
}

// TestBroadcastSubscriptionsMigrationIndexes asserts both declared indexes exist
// after Up.
func TestBroadcastSubscriptionsMigrationIndexes(t *testing.T) {
	db := setupTestDB(t)

	present := indexNames(t, db, "broadcast_subscriptions")
	for _, want := range broadcastSubscriptionIndexes {
		if !present[want] {
			t.Errorf("index %s missing after migration", want)
		}
	}
}

// TestBroadcastSubscriptionsMigrationNoOwnerForeignKey pins the central schema
// decision: owner_session_id carries NO foreign key. A subscription may outlive
// the session whose outcome fires it, and a cascading DELETE would silently
// erase a standing registration (and its firing history) instead of letting
// expiry retire it.
func TestBroadcastSubscriptionsMigrationNoOwnerForeignKey(t *testing.T) {
	db := setupTestDB(t)

	rows, err := db.Query("PRAGMA foreign_key_list(broadcast_subscriptions)")
	if err != nil {
		t.Fatalf("pragma foreign_key_list: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var fks []string
	for rows.Next() {
		var id, seq int
		var table, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			t.Fatalf("scan foreign_key_list: %v", err)
		}
		fks = append(fks, from+" -> "+table+"("+to+")")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(fks) != 0 {
		t.Errorf("broadcast_subscriptions foreign keys = %v, want none", fks)
	}
}

// TestBroadcastSubscriptionsMigrationTimestampDefaults pins the half of the
// timestamp contract PRAGMA-type assertions cannot see: created_at/updated_at
// default via strftime in the canonical sqlutil.TimeLayout shape, while the
// nullable _at columns and the caller-supplied expiry carry no default at all.
func TestBroadcastSubscriptionsMigrationTimestampDefaults(t *testing.T) {
	db := setupTestDB(t)

	defaultOf := func(col string) *string {
		t.Helper()
		var dflt *string
		if err := db.QueryRow(
			`SELECT dflt_value FROM pragma_table_info('broadcast_subscriptions') WHERE name = ?`, col,
		).Scan(&dflt); err != nil {
			t.Fatalf("pragma_table_info.%s: %v", col, err)
		}
		return dflt
	}

	const wantDefault = `strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`
	for _, col := range []string{"created_at", "updated_at"} {
		got := defaultOf(col)
		if got == nil || !strings.Contains(*got, wantDefault) {
			t.Errorf("broadcast_subscriptions.%s default = %v, want it to contain %s", col, got, wantDefault)
		}
	}

	// fired_at must stay bare TEXT: a strftime default would make an unfired
	// subscription read as already fired to any timestamp-based inspection.
	if got := defaultOf("fired_at"); got != nil {
		t.Errorf("broadcast_subscriptions.fired_at default = %q, want none", *got)
	}
	// expires_at is caller-supplied: a strftime('now') default would mean
	// "already expired" on every row that forgot to set it.
	if got := defaultOf("expires_at"); got != nil {
		t.Errorf("broadcast_subscriptions.expires_at default = %q, want none", *got)
	}
}

// TestBroadcastSubscriptionsMigrationDown asserts the Down step removes the
// table (and with it its indexes) so the migration is reversible.
//
// The ceiling is capped with RunUpTo rather than the shared fully-migrated
// helper for the same reason as TestBroadcastsMigrationDown: RunDownTo rolls
// back everything above its target, so an uncapped run would drag every later
// migration through this test's Down leg.
func TestBroadcastSubscriptionsMigrationDown(t *testing.T) {
	migrations := os.DirFS(migrationsDir())
	db, err := OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := migrate.RunUpTo(db, migrations, broadcastSubscriptionsVersion); err != nil {
		t.Fatalf("run up to %d: %v", broadcastSubscriptionsVersion, err)
	}
	if cols := tableColumns(t, db, "broadcast_subscriptions"); len(cols) == 0 {
		t.Fatal("broadcast_subscriptions missing before down")
	}

	if err := migrate.RunDownTo(db, migrations, preBroadcastSubscriptionsVersion); err != nil {
		t.Fatalf("run down: %v", err)
	}
	if cols := tableColumns(t, db, "broadcast_subscriptions"); len(cols) != 0 {
		t.Errorf("broadcast_subscriptions should be gone after down, got columns: %v", cols)
	}

	var remaining int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name LIKE 'idx_broadcast_subscriptions%'`,
	).Scan(&remaining); err != nil {
		t.Fatalf("count remaining indexes: %v", err)
	}
	if remaining != 0 {
		t.Errorf("broadcast subscription indexes remaining after down = %d, want 0", remaining)
	}

	// Re-applying must succeed: a Down that leaves state the Up cannot re-enter
	// is not actually reversible.
	if err := migrate.RunUpTo(db, migrations, broadcastSubscriptionsVersion); err != nil {
		t.Fatalf("re-run up after down: %v", err)
	}
	if cols := tableColumns(t, db, "broadcast_subscriptions"); len(cols) == 0 {
		t.Error("broadcast_subscriptions missing after re-applying the migration")
	}
}
